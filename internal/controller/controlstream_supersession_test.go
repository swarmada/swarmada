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
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/probe"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// One FleetAdapter identity, one live ControlStream (RFC-0001
// #fleet-adapter-protocol-connection-lifecycle, #fleetadapter-status-fields-and-phase-machine).
//
// The mTLS certificate names the adapter, so two processes presenting one certificate are
// one identity to the control plane. Commands can go down only one of their streams, so a
// second accepted stream replaces the first: the first is closed with a status the adapter
// can read, its liveness beats stop with it, and the adapter's phase does not drop to
// Disconnected when it closes, because the adapter is still connected on the new one.
//
// These tests run the real controlstream.Server over a real TLS 1.3 handshake, the real
// command.Dispatcher and the real FleetAdapterReconciler as the presence sink, over a fake
// client that records every phase the reconciler patches.

const (
	supNS      = "warehouse-sup"
	supAdapter = "acme-adapter"
	supOther   = "other-adapter"
)

// identityAuthorizer allows each adapter identity a fixed set of robots.
type identityAuthorizer map[string]map[string]bool

func (a identityAuthorizer) AuthorizeRobot(_ context.Context, id controlstream.TLSIdentity, robotID string) error {
	if id.Verified && a[id.AdapterName][robotID] {
		return nil
	}
	return fmt.Errorf("robot %q is not served by adapter %q", robotID, id.AdapterName)
}

func (a identityAuthorizer) AuthorizeAnnounce(ctx context.Context, id controlstream.TLSIdentity, robotID string) error {
	return a.AuthorizeRobot(ctx, id, robotID)
}

// supWorld is one control plane: a server listening once, and a way to connect adapters
// to it with a certificate for a chosen identity.
type supWorld struct {
	c     client.Client
	d     *command.Dispatcher
	r     *FleetAdapterReconciler
	srv   *controlstream.Server
	audit *lockedAudit
	ca    *stubCA
	addr  string

	mu     sync.Mutex
	phases []fleetv1.FleetAdapterPhase // every phase patched onto supAdapter, in order
	writes int                         // every write the control plane made, of any kind
}

func (w *supWorld) phaseLog() []fleetv1.FleetAdapterPhase {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]fleetv1.FleetAdapterPhase(nil), w.phases...)
}

func (w *supWorld) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

func (w *supWorld) countWrite() {
	w.mu.Lock()
	w.writes++
	w.mu.Unlock()
}

// newSupWorld builds the control plane. beatSeconds is supAdapter's
// spec.heartbeatIntervalSeconds; the liveness beat is pushed every half of it. Each
// configure func runs on the built world before the server starts listening.
func newSupWorld(t *testing.T, beatSeconds int32, configure ...func(*supWorld)) *supWorld {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	w := &supWorld{audit: &lockedAudit{}, ca: newStubCA(t)}
	robot := func(name, adapter string) *fleetv1.Robot {
		return &fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: supNS},
			Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: adapter}},
		}
	}
	adapter := func(name string) *fleetv1.FleetAdapter {
		return &fleetv1.FleetAdapter{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: supNS},
			Spec:       fleetv1.FleetAdapterSpec{HeartbeatIntervalSeconds: beatSeconds},
		}
	}
	w.c = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(adapter(supAdapter), adapter(supOther),
			robot("amr-1", supAdapter), robot("amr-2", supAdapter), robot("amr-b", supOther)).
		WithStatusSubresource(&fleetv1.FleetAdapter{}, &fleetv1.Robot{}).
		WithIndex(&fleetv1.Robot{}, robotAdapterNameField, func(o client.Object) []string {
			if n := o.(*fleetv1.Robot).Spec.Adapter.Name; n != "" {
				return []string{n}
			}
			return nil
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				w.countWrite()
				return cl.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				w.countWrite()
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				w.countWrite()
				return cl.Patch(ctx, obj, patch, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				w.countWrite()
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				w.mu.Lock()
				w.writes++
				if fa, ok := obj.(*fleetv1.FleetAdapter); ok && fa.Name == supAdapter {
					w.phases = append(w.phases, fa.Status.Phase)
				}
				w.mu.Unlock()
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	w.r = &FleetAdapterReconciler{Client: w.c, Scheme: scheme}
	w.d = command.New(w.c)
	w.srv = &controlstream.Server{
		Authorizer: identityAuthorizer{
			supAdapter: {"amr-1": true, "amr-2": true},
			supOther:   {"amr-b": true},
		},
		Commands: w.d,
		Presence: w.r,
		Audit:    w.audit,
	}
	for _, f := range configure {
		f(w)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	w.addr = lis.Addr().String()
	_ = lis.Close()
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{w.ca.leaf(t, "localhost", x509.ExtKeyUsageServerAuth)},
		ClientCAs:    w.ca.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controlstream.NewGRPCRunnable(w.addr, w.srv, logr.Discard(), serverTLS).Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", w.addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s", w.addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return w
}

// adapterEnd is one adapter process's ControlStream: its own TCP connection, its own
// certificate for the identity it was given.
type adapterEnd struct {
	stream fav1.FleetAdapterService_ControlStreamClient
	cancel context.CancelFunc
	sendMu sync.Mutex
	answer func(*fav1.Command) *fav1.CommandResult

	mu       sync.Mutex
	commands []*fav1.Command
	beats    int

	done chan struct{} // closed when the stream ends, for any reason
	err  error         // why it ended; read only after done
}

func (e *adapterEnd) send(m *fav1.AdapterMessage) error {
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	return e.stream.Send(m)
}

func (e *adapterEnd) counts() (commands, beats int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.commands), e.beats
}

func (e *adapterEnd) closed(within time.Duration) bool {
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

// connect opens a ControlStream as adapter, completes the hello, and serves it the way an
// adapter does: it answers every HeartbeatRequest and, if answer is set, every Command.
func (w *supWorld) connect(t *testing.T, adapter string, answer func(*fav1.Command) *fav1.CommandResult) *adapterEnd {
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
	stream, err := fav1.NewFleetAdapterServiceClient(conn).ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	e := &adapterEnd{stream: stream, cancel: cancel, answer: answer, done: make(chan struct{})}
	if err := e.send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Hello{Hello: &fav1.AdapterHello{
		Vendor: "acme", AdapterVersion: "1.0.0", ProtocolVersion: controlstream.ProtocolVersion, Namespace: supNS,
		ContractVersion: contract.Version,
	}}}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if !ack.GetHelloAck().GetAccepted() {
		t.Fatalf("hello refused: %s", ack.GetHelloAck().GetMessage())
	}
	go func() {
		defer close(e.done)
		for {
			m, err := stream.Recv()
			if err != nil {
				e.err = err
				return
			}
			if hb := m.GetHeartbeat(); hb != nil {
				e.mu.Lock()
				e.beats++
				e.mu.Unlock()
				_ = e.send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Heartbeat{
					Heartbeat: &fav1.HeartbeatResponse{RobotId: hb.GetRobotId()}}})
			}
			if cmd := m.GetCommand(); cmd != nil {
				e.mu.Lock()
				e.commands = append(e.commands, cmd)
				e.mu.Unlock()
				if e.answer != nil {
					_ = e.send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_CommandResult{
						CommandResult: e.answer(cmd)}})
				}
			}
		}
	}()
	return e
}

// answerHealthy is an honest adapter: it answers a verify for the robot it was asked about.
func answerHealthy(cmd *fav1.Command) *fav1.CommandResult {
	return &fav1.CommandResult{
		CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
		Result: &fav1.CommandResult_Verify{Verify: &fav1.VerifyResult{
			Status: fav1.ProbeStatus_PROBE_STATUS_HEALTHY, Message: "ok"}},
	}
}

func verifyRobot(t *testing.T, w *supWorld, robotID string, within time.Duration) (probe.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return w.d.Verify(ctx, supNS, robotID, probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "lidar"})
}

func (w *supWorld) waitPhase(t *testing.T, want fleetv1.FleetAdapterPhase) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		fa := &fleetv1.FleetAdapter{}
		if err := w.c.Get(context.Background(), client.ObjectKey{Namespace: supNS, Name: supAdapter}, fa); err == nil &&
			fa.Status.Phase == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("FleetAdapter %s never reached phase %s (log %v)", supAdapter, want, w.phaseLog())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestControlStream_SecondStreamUnderOneIdentitySupersedesTheFirst(t *testing.T) {
	// heartbeatIntervalSeconds 2 → a liveness beat every second on each registered stream.
	w := newSupWorld(t, 2)

	old := w.connect(t, supAdapter, answerHealthy)
	w.waitPhase(t, fleetv1.FleetAdapterPhaseConnected)
	fresh := w.connect(t, supAdapter, answerHealthy)
	logFrom := len(w.phaseLog())

	// 1. The superseded stream is closed, with a status the adapter can read.
	if !old.closed(3 * time.Second) {
		t.Errorf("superseded stream is still open 3s after a second stream under %s/%s was accepted", supNS, supAdapter)
	} else if st, _ := status.FromError(old.err); st.Code() != codes.Aborted {
		t.Errorf("superseded stream ended with %v, want codes.Aborted so the adapter can tell supersession from a fault", old.err)
	} else {
		t.Logf("superseded stream ended: %v", old.err)
	}
	oldCmds, oldBeats := old.counts()

	// 2. Commands and liveness beats reach the new stream, and nothing more reaches the old one.
	if res, err := verifyRobot(t, w, "amr-1", 3*time.Second); err != nil || res.Status != probe.StatusHealthy {
		t.Errorf("verify after supersession: status=%q err=%v, want Healthy from the new stream", res.Status, err)
	}
	time.Sleep(2500 * time.Millisecond) // two beat periods
	freshCmds, freshBeats := fresh.counts()
	nowOldCmds, nowOldBeats := old.counts()
	if freshCmds != 1 {
		t.Errorf("new stream received %d commands, want 1", freshCmds)
	}
	if freshBeats < 1 {
		t.Errorf("new stream received no liveness beat in two periods")
	}
	if nowOldCmds != oldCmds || nowOldBeats != oldBeats {
		t.Errorf("superseded stream kept receiving traffic: commands %d→%d, beats %d→%d",
			oldCmds, nowOldCmds, oldBeats, nowOldBeats)
	}

	// 3. The old process giving up (a no-op if the control plane already closed it) does not
	// move the adapter off Connected, and commands still reach the new stream.
	old.cancel()
	time.Sleep(300 * time.Millisecond)
	if res, err := verifyRobot(t, w, "amr-1", 3*time.Second); err != nil || res.Status != probe.StatusHealthy {
		t.Errorf("verify after the old stream ended: status=%q err=%v, want Healthy from the new stream", res.Status, err)
	}
	after := w.phaseLog()[logFrom:]
	for _, p := range after {
		if p != fleetv1.FleetAdapterPhaseConnected {
			t.Errorf("FleetAdapter phase left Connected while the new stream was live: patched phases %v", after)
			break
		}
	}
	fa := &fleetv1.FleetAdapter{}
	if err := w.c.Get(context.Background(), client.ObjectKey{Namespace: supNS, Name: supAdapter}, fa); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	if fa.Status.Phase != fleetv1.FleetAdapterPhaseConnected {
		t.Errorf("FleetAdapter phase = %s after supersession, want Connected", fa.Status.Phase)
	}
	select {
	case <-fresh.done:
		t.Errorf("the new stream ended: %v", fresh.err)
	default:
	}
}

// Before replacement, the newer of two streams closing left the key deleted while the older
// was still up, so commands stopped with a live stream in place. The invariant this checks is
// the one that failure broke: a stream the control plane keeps open is one it can command.
func TestControlStream_NoStreamIsLeftOpenThatCommandsCannotReach(t *testing.T) {
	w := newSupWorld(t, 60)

	old := w.connect(t, supAdapter, answerHealthy)
	w.waitPhase(t, fleetv1.FleetAdapterPhaseConnected)
	fresh := w.connect(t, supAdapter, answerHealthy)
	fresh.cancel() // the newer process goes away first
	if !fresh.closed(3 * time.Second) {
		t.Fatal("the newer stream did not end after its client cancelled")
	}
	time.Sleep(300 * time.Millisecond)

	_, err := verifyRobot(t, w, "amr-1", time.Second)
	switch {
	case err == nil:
		t.Logf("verify reached a live stream")
	case old.closed(0):
		t.Logf("no stream is open; verify unreachable as it should be: %v", err)
	default:
		t.Errorf("the older stream under %s/%s is still open, yet commands cannot reach it: %v", supNS, supAdapter, err)
	}
	if err != nil && !errors.Is(err, command.ErrUnreachable) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("verify error = %v, want ErrUnreachable or a deadline", err)
	}
}
