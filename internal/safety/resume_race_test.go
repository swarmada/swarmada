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

package safety

import (
	"context"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// staleReadClient lets the nth Get made on a context carrying holdGetKey read the robot,
// and then holds that read's answer back until release is closed: the caller goes on with
// what it read while the stored state has moved on.
type staleReadClient struct {
	client.Client
	n       int
	seen    int
	read    chan struct{} // closed once the held Get has read
	release chan struct{}
	mu      sync.Mutex
}

func (s *staleReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := s.Client.Get(ctx, key, obj, opts...)
	if held, _ := ctx.Value(holdGetKey{}).(bool); held {
		s.mu.Lock()
		s.seen++
		hold := s.seen == s.n
		s.mu.Unlock()
		if hold {
			close(s.read)
			<-s.release
		}
	}
	return err
}

// gatedAckSender answers the first estop at once with STOPPED and holds the answer to
// every later one until release is closed, as an adapter whose robot takes a moment to
// stop.
type gatedAckSender struct {
	d       *Dispatcher
	mu      sync.Mutex
	sends   int
	sent    chan struct{} // closed when the first held estop has gone out
	release chan struct{}
}

func (s *gatedAckSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	if m.GetEstop() == nil {
		return nil // an EstopClear: the gate counts stops only
	}
	s.mu.Lock()
	s.sends++
	first := s.sends == 1
	s.mu.Unlock()
	id, robot := m.GetEstop().GetEstopId(), m.GetRobotId()
	if first {
		_ = s.d.RouteAck(identity("acme"), s, robot, ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
		return nil
	}
	close(s.sent)
	go func() {
		<-s.release
		_ = s.d.RouteAck(identity("acme"), s, robot, ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED))
	}()
	return nil
}

// A confirmed stop is never discarded (RFC-0001 #safety-estop-state-machine), and a resume
// confirmation completes a clear: it never releases a stop. Here a robot is Resuming and an
// operator confirms its resume while a zone's stop is being delivered to it. The stop was
// issued first, so the confirmation's epoch is the later one; the confirmation reads
// Resuming, and the stop's STOPPED lands before the confirmation writes. The robot must
// end Stopped.
func TestResume_AConfirmationDoesNotReleaseAStopConfirmedAfterItsRead(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = 10 * time.Second // the stop waits on its held ack, not on the window
	d.confirmTimeout = 10 * time.Second
	sender := &gatedAckSender{d: d, sent: make(chan struct{}), release: make(chan struct{})}
	d.RegisterStream(identity("acme"), sender)
	ctx := context.Background()
	clearedAt := time.Now()

	// Stopped, cleared to Resuming, and fresh telemetry: a confirmation would release.
	if res, err := d.TriggerEstop(ctx, ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}
	if state, err := d.ClearEstop(ctx, ns, "amr-1", "admin", Ends{}); err != nil || state != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s (err %v), want Resuming", state, err)
	}
	projected(t, d, c, clearedAt.Add(time.Minute), fleetv1.RobotPhaseIdle, activeCap("lidar"))

	stale := &staleReadClient{Client: c, n: 2, read: make(chan struct{}), release: make(chan struct{})}
	d.APIReader = stale

	// The zone's stop is issued; its STOPPED is held.
	triggered := make(chan fleetv1.RobotEstopState, 1)
	go func() {
		res, err := d.TriggerEstop(ctx, ns, "amr-1", "zone z stopped", "operator", metrics.ScopeZone, Carrier{})
		if err != nil {
			t.Errorf("zone TriggerEstop: %v", err)
		}
		triggered <- res.State
	}()
	<-sender.sent

	// The confirmation will read the robot (its second read: the first is the own-trigger
	// check) and be held with what it read.
	confirmed := make(chan fleetv1.RobotEstopState, 1)
	go func() {
		held := context.WithValue(ctx, holdGetKey{}, true)
		got, why, err := d.ConfirmResume(held, ns, "amr-1", "admin")
		if err != nil {
			t.Errorf("ConfirmResume: %v", err)
		}
		t.Logf("RESULT ConfirmResume answered %s: %s", got, why)
		confirmed <- got
	}()
	<-stale.read

	// The stop is confirmed while the confirmation holds its read of Resuming.
	close(sender.release)
	if got := <-triggered; got != fleetv1.RobotEstopStopped {
		t.Fatalf("the zone's stop resolved %s, want Stopped", got)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s after the zone's stop was confirmed, want Stopped", got)
	}

	close(stale.release)
	<-confirmed
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s: a resume confirmation released a stop confirmed after it read Resuming", got)
	}
}
