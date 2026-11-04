/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// One FleetAdapter identity, one live SafetyStream: the ControlStream rule applied to the
// stream estops travel on (RFC-0001 #fleet-adapter-protocol-connection-lifecycle,
// #safety-emergency-stop-protocol).
//
// Estops are pushed down one SafetyStream per identity. With two open under one
// certificate, the one estops go down and the one the adapter listens on can differ, and
// a stream the control plane keeps open is one an estop may never be pushed down. These
// tests run the real controlstream.Server over real TLS 1.3 with the real safety
// Dispatcher, on the same world the ControlStream supersession tests use.

// newSafetyWorld is newSupWorld with the safety dispatcher served.
func newSafetyWorld(t *testing.T) (*supWorld, *safety.Dispatcher) {
	t.Helper()
	var d *safety.Dispatcher
	w := newSupWorld(t, 60, func(w *supWorld) {
		d = safety.New(w.c, nil)
		w.srv.Safety = d
	})
	return w, d
}

// safetyEnd is one adapter process's SafetyStream.
type safetyEnd struct {
	stream fav1.FleetAdapterService_SafetyStreamClient
	cancel context.CancelFunc
	sendMu sync.Mutex

	mu     sync.Mutex
	estops []*fav1.ControlPlaneSafetyMessage

	done chan struct{}
	err  error
}

func (e *safetyEnd) received() []*fav1.ControlPlaneSafetyMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*fav1.ControlPlaneSafetyMessage(nil), e.estops...)
}

func (e *safetyEnd) ack(robotID, estopID string, st fav1.EstopState) error {
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	return e.stream.Send(&fav1.AdapterSafetyMessage{RobotId: robotID, Payload: &fav1.AdapterSafetyMessage_EstopAck{
		EstopAck: &fav1.EstopAck{EstopId: estopID, State: st, ConfirmedAtMs: time.Now().UnixMilli()}}})
}

func (e *safetyEnd) closed(within time.Duration) bool {
	// Checked alone first: with within == 0 both cases below are ready at once, and
	// select would pick between them at random.
	select {
	case <-e.done:
		return true
	default:
	}
	select {
	case <-e.done:
		return true
	case <-time.After(within):
		return false
	}
}

// connectSafety opens a SafetyStream as adapter. When confirm is true it acknowledges
// every estop STOPPED on the same stream, as a conformant adapter does; otherwise it
// records the estop and stays silent.
func (w *supWorld) connectSafety(t *testing.T, adapter string, confirm bool) *safetyEnd {
	t.Helper()
	cert := w.ca.leaf(t, adapter+"."+supNS+".svc.cluster.local", x509.ExtKeyUsageClientAuth)
	conn, err := grpc.NewClient("passthrough:///"+w.addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: w.ca.pool, ServerName: "localhost", MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	})))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := fav1.NewFleetAdapterServiceClient(conn).SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open SafetyStream: %v", err)
	}
	e := &safetyEnd{stream: stream, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(e.done)
		for {
			m, err := stream.Recv()
			if err != nil {
				e.err = err
				return
			}
			if m.GetEstop() == nil {
				continue
			}
			e.mu.Lock()
			e.estops = append(e.estops, m)
			e.mu.Unlock()
			if confirm {
				_ = e.ack(m.GetRobotId(), m.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)
			}
		}
	}()
	// A SafetyStream has no hello; give the server the moment it needs to register it.
	time.Sleep(300 * time.Millisecond)
	return e
}

func triggerEstop(t *testing.T, d *safety.Dispatcher, robotID string, within time.Duration) (safety.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return d.TriggerEstop(ctx, supNS, robotID, "test", "operator", metrics.ScopeRobot, safety.Carrier{})
}

func TestSafetyStream_SecondStreamUnderOneIdentitySupersedesTheFirst(t *testing.T) {
	w, d := newSafetyWorld(t)
	phasesBefore := len(w.phaseLog())

	old := w.connectSafety(t, supAdapter, true)
	fresh := w.connectSafety(t, supAdapter, true)

	// 1. The superseded stream is closed, with the status the ControlStream uses.
	if !old.closed(3 * time.Second) {
		t.Errorf("superseded SafetyStream is still open 3s after a second one under %s/%s was accepted", supNS, supAdapter)
	} else if st, _ := status.FromError(old.err); st.Code() != codes.Aborted {
		t.Errorf("superseded SafetyStream ended with %v, want codes.Aborted", old.err)
	} else {
		t.Logf("superseded SafetyStream ended: %v", old.err)
	}
	oldBefore := len(old.received())

	// 2. An estop goes down the new stream and is confirmed there; nothing more reaches the old one.
	res, err := triggerEstop(t, d, "amr-1", 3*time.Second)
	if err != nil || !res.Confirmed {
		t.Errorf("estop after supersession: confirmed=%v state=%s err=%v, want confirmed on the new stream", res.Confirmed, res.State, err)
	}
	if got := len(fresh.received()); got != 1 {
		t.Errorf("new SafetyStream received %d estops, want 1", got)
	}
	if got := len(old.received()); got != oldBefore {
		t.Errorf("superseded SafetyStream received %d more estop(s)", got-oldBefore)
	}

	// 3. The old process giving up does not take estops away from the new stream.
	old.cancel()
	time.Sleep(300 * time.Millisecond)
	if res, err := triggerEstop(t, d, "amr-2", 3*time.Second); err != nil || !res.Confirmed {
		t.Errorf("estop after the old stream ended: confirmed=%v err=%v, want confirmed on the new stream", res.Confirmed, err)
	}

	// 4. The handover raises no disconnect: a SafetyStream never moves the adapter's phase.
	if after := w.phaseLog()[phasesBefore:]; len(after) != 0 {
		t.Errorf("the SafetyStream handover patched the FleetAdapter phase: %v", after)
	}
	select {
	case <-fresh.done:
		t.Errorf("the new SafetyStream ended: %v", fresh.err)
	default:
	}
}

// A stream the control plane keeps open is one it can push an estop down. Before, the
// newer of two streams closing left no registration while the older stayed up, so an
// estop found "no SafetyStream" with an open one in place.
func TestSafetyStream_NoStreamIsLeftOpenThatEstopsCannotReach(t *testing.T) {
	w, d := newSafetyWorld(t)

	old := w.connectSafety(t, supAdapter, true)
	fresh := w.connectSafety(t, supAdapter, true)
	fresh.cancel() // the newer process goes away first
	if !fresh.closed(3 * time.Second) {
		t.Fatal("the newer SafetyStream did not end after its client cancelled")
	}
	time.Sleep(300 * time.Millisecond)

	res, err := triggerEstop(t, d, "amr-1", 3*time.Second)
	switch {
	case err == nil && res.Confirmed:
		t.Logf("the estop reached a live stream and was confirmed")
	case old.closed(0):
		t.Logf("no SafetyStream is open; the estop is Failed (escalate), as it must be: state=%s err=%v", res.State, err)
	default:
		t.Errorf("the older SafetyStream under %s/%s is still open, yet the estop could not be pushed down it: "+
			"state=%s err=%v", supNS, supAdapter, res.State, err)
	}
	if res.Confirmed && err != nil {
		t.Errorf("confirmed with an error: %v", err)
	}
}

// An estop is confirmed only by a stream it went down. Two rows:
//   - same identity: a second process under the same certificate replaces the first stream, and the
//     pending stop is re-sent down it under its own stop ID, so an acknowledgement there names a stop
//     that stream carries; one naming a stop ID never sent down it confirms nothing;
//   - another robot's stream: an adapter that serves another robot, and never carried this stop,
//     names the stop's real ID for this robot; it is refused, sealed as a denial, and confirms
//     nothing.
func TestSafetyStream_AckFromAnotherStreamDoesNotConfirmTheEstop(t *testing.T) {
	for _, row := range []string{"same-identity-unknown-stop-id", "another-robots-stream-same-stop-id"} {
		t.Run(row, func(t *testing.T) {
			w, d := newSafetyWorld(t)

			sentOn := w.connectSafety(t, supAdapter, false) // receives the estop, stays silent

			type outcome struct {
				res safety.Result
				err error
			}
			got := make(chan outcome, 1)
			go func() {
				res, err := triggerEstop(t, d, "amr-1", 4*time.Second)
				got <- outcome{res, err}
			}()
			var estopID string
			deadline := time.Now().Add(3 * time.Second)
			for estopID == "" {
				if msgs := sentOn.received(); len(msgs) > 0 {
					estopID = msgs[0].GetEstop().GetEstopId()
				} else if time.Now().After(deadline) {
					t.Fatal("the estop never reached the stream it was pushed down")
				}
				time.Sleep(10 * time.Millisecond)
			}

			if row == "same-identity-unknown-stop-id" {
				other := w.connectSafety(t, supAdapter, false)
				// A stop ID this stream was never sent (the original stream's is re-sent to it under its own).
				if err := other.ack("amr-1", safety.NewStopID(), fav1.EstopState_ESTOP_STATE_STOPPED); err != nil {
					t.Fatalf("sending the ack on the other stream: %v", err)
				}
			} else {
				other := w.connectSafety(t, supOther, false)
				if err := other.ack("amr-1", estopID, fav1.EstopState_ESTOP_STATE_STOPPED); err != nil {
					t.Fatalf("sending the ack on another robot's stream: %v", err)
				}
			}

			o := <-got
			if o.res.Confirmed || o.res.State == fleetv1.RobotEstopStopped {
				t.Fatalf("SAFETY: an EstopAck on a stream that did not carry the stop confirmed it: "+
					"confirmed=%v state=%s", o.res.Confirmed, o.res.State)
			}
			t.Logf("the estop resolved without the other stream's ack: state=%s err=%v", o.res.State, o.err)
			robot := &fleetv1.Robot{}
			if err := w.c.Get(context.Background(), client.ObjectKey{Namespace: supNS, Name: "amr-1"}, robot); err != nil {
				t.Fatal(err)
			}
			if robot.Status.EstopState == fleetv1.RobotEstopStopped {
				t.Fatalf("robot estopState = Stopped on an ack from a stream that did not carry the stop")
			}
			if row == "another-robots-stream-same-stop-id" && !w.audit.denied(audit.EventRobotAuthzDenied, "amr-1") {
				t.Errorf("the refused ack was not sealed as %s", audit.EventRobotAuthzDenied)
			}
		})
	}
}

// A superseded SafetyStream is reported to the operator as one Warning Event on the
// FleetAdapter, as a superseded ControlStream is.
func TestSafetyStream_SupersessionEmitsOneWarningEvent(t *testing.T) {
	w, _ := newSafetyWorld(t)
	rec := record.NewFakeRecorder(8)
	w.r.Recorder = rec

	old := w.connectSafety(t, supAdapter, false)
	w.connectSafety(t, supAdapter, false)
	select {
	case ev := <-rec.Events:
		if !strings.HasPrefix(ev, "Warning SafetyStreamSuperseded ") {
			t.Fatalf("event = %q, want a Warning SafetyStreamSuperseded", ev)
		}
		t.Logf("event: %s", ev)
	case <-time.After(3 * time.Second):
		t.Fatal("no Event for the superseded SafetyStream")
	}
	if !old.closed(3 * time.Second) {
		t.Fatal("superseded SafetyStream still open")
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case ev := <-rec.Events:
		t.Fatalf("a second Event for one supersession: %s", ev)
	default:
	}
}
