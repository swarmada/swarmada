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

package controlstream

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/swarmada/swarmada/internal/leadership"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A control-plane process sends on an adapter's streams only while it holds leadership, and
// closes them when it loses it (RFC-0001 #safety-robot-stop-table). These rows run the real
// leadership.State against the stream server.

func leading(t *testing.T, term uint64) *leadership.State {
	t.Helper()
	s := &leadership.State{}
	if !s.Acquire(term) {
		t.Fatalf("Acquire(%d) refused", term)
	}
	return s
}

// wantUnavailable fails unless err is Unavailable and its message carries want.
func wantUnavailable(t *testing.T, what string, err error, want string) {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable || !strings.Contains(st.Message(), want) {
		t.Fatalf("%s: got %v (%v), want Unavailable containing %q", what, st.Code(), err, want)
	}
}

func TestControlStream_ANonLeaderRefusesTheStreamBeforeItsHandshake(t *testing.T) {
	reg := &fakeRegistrar{}
	client, cleanup := newTestServer(t, &Server{Registrar: reg, Leadership: &leadership.State{}})
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = stream.Send(hello())
	_, err = stream.Recv()
	wantUnavailable(t, "a ControlStream opened on a process that does not lead", err, "not the leader")
}

func TestSafetyStream_ANonLeaderRefusesTheStreamAndRegistersNothingForEstops(t *testing.T) {
	disp := &registeringSafetyDispatcher{}
	client, cleanup := newTestServer(t, &Server{Safety: disp, Leadership: &leadership.State{}})
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = stream.Recv()
	wantUnavailable(t, "a SafetyStream opened on a process that does not lead", err, "not the leader")
	if n := len(disp.registered()); n != 0 {
		t.Fatalf("%d SafetyStreams registered for estops on a process that does not lead, want 0", n)
	}
}

func TestControlStream_LosingLeadershipClosesAnOpenStream(t *testing.T) {
	state := leading(t, 4)
	reg := &fakeRegistrar{}
	client, cleanup := newTestServer(t, &Server{Registrar: reg, Leadership: state})
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if msg, err := stream.Recv(); err != nil || !msg.GetHelloAck().GetAccepted() {
		t.Fatalf("handshake on the leader: %v %v", msg, err)
	}

	state.Release()
	_, err = stream.Recv()
	wantUnavailable(t, "the open ControlStream after leadership was lost", err, "leadership lost")

	// A new stream to this process is refused until it leads again, under a higher term.
	again, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = again.Send(hello())
	_, err = again.Recv()
	wantUnavailable(t, "a ControlStream opened after leadership was lost", err, "not the leader")

	if !state.Acquire(5) {
		t.Fatal("Acquire(5) refused")
	}
	third, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open under the new term: %v", err)
	}
	if err := third.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if msg, err := third.Recv(); err != nil || !msg.GetHelloAck().GetAccepted() {
		t.Fatalf("handshake after leadership was acquired again: %v %v", msg, err)
	}
}

func TestSafetyStream_LosingLeadershipClosesAnOpenStream(t *testing.T) {
	state := leading(t, 2)
	disp := &registeringSafetyDispatcher{}
	client, cleanup := newTestServer(t, &Server{Safety: disp, Leadership: state})
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(disp.registered()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(disp.registered()) != 1 {
		t.Fatal("the leader did not register the SafetyStream for estops")
	}
	state.Release()
	_, err = stream.Recv()
	wantUnavailable(t, "the open SafetyStream after leadership was lost", err, "leadership lost")
}

// retainedIngestor is an ActionStatusIngestor that also reads retained outcomes. It answers
// ack for each retained report, and records what it was given.
type retainedIngestor struct {
	mu       sync.Mutex
	ack      bool
	retained []*fav1.ActionStatusUpdate
	live     []*fav1.ActionStatusUpdate
}

func (r *retainedIngestor) IngestActionStatus(_ context.Context, _ string, _ RobotAuthorization, u *fav1.ActionStatusUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live = append(r.live, u)
	return nil
}

func (r *retainedIngestor) IngestRetainedOutcome(_ context.Context, _ string, _ RobotAuthorization, u *fav1.ActionStatusUpdate) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retained = append(r.retained, u)
	return r.ack, nil
}

// liveOnlyIngestor reads no retained outcome: it implements ActionStatusIngestor only.
type liveOnlyIngestor struct {
	mu   sync.Mutex
	live []*fav1.ActionStatusUpdate
}

func (l *liveOnlyIngestor) IngestActionStatus(_ context.Context, _ string, _ RobotAuthorization, u *fav1.ActionStatusUpdate) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live = append(l.live, u)
	return nil
}

var _ ActionStatusIngestor = (*liveOnlyIngestor)(nil)

// handshaken opens a ControlStream on srv and completes its handshake.
func handshaken(t *testing.T, srv *Server) (fav1.FleetAdapterService_ControlStreamClient, func()) {
	t.Helper()
	client, cleanup := newTestServer(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if msg, err := stream.Recv(); err != nil || !msg.GetHelloAck().GetAccepted() {
		t.Fatalf("handshake: %v %v", msg, err)
	}
	return stream, func() { cancel(); cleanup() }
}

func retainedReport(action string, token uint64, retained bool) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_ActionStatus{ActionStatus: &fav1.ActionStatusUpdate{
		ActionId:     action,
		State:        fav1.ActionState_ACTION_STATE_SUCCEEDED,
		FencingToken: proto.Uint64(token),
		Retained:     retained,
	}}}
}

// nextAfter sends a register for robot r1 and returns every message received before its
// RegisterAck: the stream is answered in order, so what a report produced arrives first.
func nextAfter(t *testing.T, stream fav1.FleetAdapterService_ControlStreamClient) []*fav1.ControlPlaneMessage {
	t.Helper()
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Register{
		Register: &fav1.RegisterRobot{RobotId: "r1"}}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	var before []*fav1.ControlPlaneMessage
	for {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if msg.GetRegisterAck() != nil {
			return before
		}
		before = append(before, msg)
	}
}

func TestControlStream_ARetainedOutcomeTheIngestorRecordedIsAcknowledgedOnce(t *testing.T) {
	ing := &retainedIngestor{ack: true}
	stream, done := handshaken(t, &Server{Registrar: &fakeRegistrar{}, ActionStatus: ing})
	defer done()

	if err := stream.Send(retainedReport("a1", 7, true)); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := nextAfter(t, stream)
	if len(got) != 1 || got[0].GetOutcomeAck() == nil {
		t.Fatalf("messages before the RegisterAck = %v; want exactly one ActionOutcomeAck", got)
	}
	if a := got[0].GetOutcomeAck(); a.GetActionId() != "a1" || a.GetFencingToken() != 7 {
		t.Fatalf("ack names %q/%d, want a1/7 (the action and the outcome's report token)", a.GetActionId(), a.GetFencingToken())
	}
	ing.mu.Lock()
	defer ing.mu.Unlock()
	if len(ing.retained) != 1 || len(ing.live) != 0 {
		t.Fatalf("retained report read %d times on the retained path and %d on the live path; want 1 and 0",
			len(ing.retained), len(ing.live))
	}
}

func TestControlStream_ARetainedOutcomeLeftUndecidedIsNotAcknowledged(t *testing.T) {
	ing := &retainedIngestor{ack: false}
	stream, done := handshaken(t, &Server{Registrar: &fakeRegistrar{}, ActionStatus: ing})
	defer done()
	if err := stream.Send(retainedReport("a1", 3, true)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := nextAfter(t, stream); len(got) != 0 {
		t.Fatalf("messages before the RegisterAck = %v; an undecided retained outcome must not be acknowledged", got)
	}
}

func TestControlStream_ALiveReportIsNeverAcknowledged(t *testing.T) {
	ing := &retainedIngestor{ack: true}
	stream, done := handshaken(t, &Server{Registrar: &fakeRegistrar{}, ActionStatus: ing})
	defer done()
	if err := stream.Send(retainedReport("a1", 7, false)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := nextAfter(t, stream); len(got) != 0 {
		t.Fatalf("messages before the RegisterAck = %v; only a retained outcome is acknowledged", got)
	}
	ing.mu.Lock()
	defer ing.mu.Unlock()
	if len(ing.live) != 1 || len(ing.retained) != 0 {
		t.Fatalf("live report read %d times live and %d retained; want 1 and 0", len(ing.live), len(ing.retained))
	}
}

func TestControlStream_AnIngestorThatReadsNoRetainedOutcomeLeavesItUnacknowledged(t *testing.T) {
	ing := &liveOnlyIngestor{}
	stream, done := handshaken(t, &Server{Registrar: &fakeRegistrar{}, ActionStatus: ing})
	defer done()
	if err := stream.Send(retainedReport("a1", 7, true)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := nextAfter(t, stream); len(got) != 0 {
		t.Fatalf("messages before the RegisterAck = %v; want none", got)
	}
	ing.mu.Lock()
	defer ing.mu.Unlock()
	if len(ing.live) != 0 {
		t.Fatal("a retained outcome went down the live path, where it could move the action as a fresh report")
	}
}
