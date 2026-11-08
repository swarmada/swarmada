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

// An estop awaiting its ack on a SafetyStream that a newer stream replaces is re-sent down
// the newer one (RFC-0001 #safety-emergency-stop-protocol). Three rules, one test each:
// the re-send carries the stop's own ID, the stop ID minted once at issue (RFC-0001
// #safety-robot-stop-table, the term stop ID), now bound to the new stream, so an ack with
// that ID on the replaced stream is refused; it keeps the original window; and only an estop
// still wanted is re-sent.

import (
	"context"
	"errors"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

type triggerOutcome struct {
	res Result
	err error
}

// triggerAsync issues an estop for amr-1 and returns where its outcome will arrive.
func triggerAsync(d *Dispatcher, reason string) <-chan triggerOutcome {
	done := make(chan triggerOutcome, 1)
	go func() {
		res, err := d.TriggerEstop(context.Background(), ns, "amr-1", reason, "operator", metrics.ScopeRobot, Carrier{})
		done <- triggerOutcome{res, err}
	}()
	return done
}

func newRecordingSender() *recordingSender {
	return &recordingSender{sent: make(chan *fav1.ControlPlaneSafetyMessage, 8)}
}

// nextEstop waits for the next estop pushed down s.
func nextEstop(t *testing.T, s *recordingSender, what string) *fav1.ControlPlaneSafetyMessage {
	t.Helper()
	select {
	case m := <-s.sent:
		return m
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no estop was pushed", what)
		return nil
	}
}

// nothingPushed requires that no estop reaches s within the window.
func nothingPushed(t *testing.T, s *recordingSender, what string, within time.Duration) {
	t.Helper()
	select {
	case m := <-s.sent:
		t.Fatalf("%s: estop %q was pushed", what, m.GetEstop().GetEstopId())
	case <-time.After(within):
	}
}

// passGate counts the Robot lists that have returned and, once armed, holds the next until release
// is closed: a SafetyStream's opening pass (resendHeld) reads the adapter's robots once.
type passGate struct {
	client.Client
	armed   atomic.Bool
	release chan struct{}
	lists   atomic.Int32
}

func (g *passGate) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	_, robots := list.(*fleetv1.RobotList)
	if robots && g.armed.Load() {
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := g.Client.List(ctx, list, opts...)
	if robots {
		g.lists.Add(1)
	}
	return err
}

func outcome(t *testing.T, done <-chan triggerOutcome) triggerOutcome {
	t.Helper()
	select {
	case o := <-done:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("the estop never resolved")
		return triggerOutcome{}
	}
}

// stopIDForm is a version 4 (random) UUID in its canonical text form.
var stopIDForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestResend_PendingEstopIsResentDownTheStreamThatReplacedItsOwn(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = 2 * time.Second
	old, fresh := newRecordingSender(), newRecordingSender()
	d.RegisterStream(identity("acme"), old)

	done := triggerAsync(d, "aisle blocked")
	sent := nextEstop(t, old, "the estop on the first stream")

	d.RegisterStream(identity("acme"), fresh) // the takeover
	again := nextEstop(t, fresh, "the re-send on the stream that replaced the first")

	id := sent.GetEstop().GetEstopId()
	if again.GetEstop().GetEstopId() != id {
		t.Fatalf("re-sent under estop_id %q, sent under %q: every re-send carries the stop's ID",
			again.GetEstop().GetEstopId(), id)
	}
	if !stopIDForm.MatchString(id) {
		t.Fatalf("stop ID %q is not a random (version 4) UUID", id)
	}
	if again.GetRobotId() != "amr-1" || again.GetEstop().GetReason() != "aisle blocked" ||
		again.GetEstop().GetIssuedBy() != "operator" ||
		again.GetEstop().GetIssuedAtMs() != sent.GetEstop().GetIssuedAtMs() {
		t.Fatalf("the re-send is not the same estop: sent %v, re-sent %v", sent, again)
	}

	// The stop is bound to the new stream: an ack with its ID on the replaced one confirms
	// nothing.
	stoppedNew := ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)
	if err := d.RouteAck(identity("acme"), old, "amr-1", stoppedNew); !errors.Is(err, controlstream.ErrMisroutedAck) {
		t.Fatalf("an ack with the stop's ID on the replaced stream: err = %v, want ErrMisroutedAck", err)
	}
	select {
	case o := <-done:
		t.Fatalf("the estop resolved on a refused ack: %+v", o.res)
	case <-time.After(100 * time.Millisecond):
	}

	if err := d.RouteAck(identity("acme"), fresh, "amr-1", stoppedNew); err != nil {
		t.Fatalf("the ack for the re-send on the new stream was refused: %v", err)
	}
	o := outcome(t, done)
	if o.err != nil || !o.res.Confirmed || estopState(t, c) != fleetv1.RobotEstopStopped {
		t.Fatalf("res=%+v err=%v state=%s; want confirmed Stopped on the stream that replaced the first",
			o.res, o.err, estopState(t, c))
	}
}

// The confirmation window carries over as well: STOPPING on the first stream, the
// takeover, then STOPPED on the second.
func TestResend_AnEstopInItsConfirmWindowIsResentAndConfirmed(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = 2 * time.Second
	d.confirmTimeout = 2 * time.Second
	old, fresh := newRecordingSender(), newRecordingSender()
	d.RegisterStream(identity("acme"), old)

	done := triggerAsync(d, "test")
	sent := nextEstop(t, old, "the estop on the first stream")
	if err := d.RouteAck(identity("acme"), old, "amr-1",
		ackState(sent.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPING)); err != nil {
		t.Fatalf("STOPPING on the first stream: %v", err)
	}
	waitState(t, c, fleetv1.RobotEstopStopping)

	d.RegisterStream(identity("acme"), fresh)
	again := nextEstop(t, fresh, "the re-send of an estop awaiting confirmation")
	if err := d.RouteAck(identity("acme"), fresh, "amr-1",
		ackState(again.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatalf("STOPPED on the new stream: %v", err)
	}
	if o := outcome(t, done); !o.res.Confirmed || estopState(t, c) != fleetv1.RobotEstopStopped {
		t.Fatalf("res=%+v state=%s; want confirmed Stopped", o.res, estopState(t, c))
	}
}

// Rule 2: the re-send keeps the original window. Taken over after the window ended, the
// estop is not re-sent by the takeover; the new stream carries the stop the robot's Failed
// holds once, under its recorded ID (Sending a stop again); re-sent inside it but answered
// after it, it resolves Failed.
func TestResend_TheOriginalDeadlineStands(t *testing.T) {
	t.Run("taken over after the window ended", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.deliveryTimeout = 100 * time.Millisecond
		old, fresh := newRecordingSender(), newRecordingSender()
		d.RegisterStream(identity("acme"), old)
		done := triggerAsync(d, "test")
		sent := nextEstop(t, old, "the estop on the first stream")
		o := outcome(t, done)
		if o.res.State != fleetv1.RobotEstopFailed {
			t.Fatalf("state = %s, want Failed at the end of the delivery window", o.res.State)
		}
		d.RegisterStream(identity("acme"), fresh)
		if held := heldStopResent(t, fresh); held.GetEstop().GetEstopId() != sent.GetEstop().GetEstopId() {
			t.Fatalf("the held stop went out under %q; want its recorded ID %q", held.GetEstop().GetEstopId(),
				sent.GetEstop().GetEstopId())
		}
		nothingPushed(t, fresh, "an estop whose window had ended", 200*time.Millisecond)
		if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
			t.Fatalf("state = %s, want Failed", got)
		}
	})

	t.Run("re-sent inside the window, answered after it", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.deliveryTimeout = 300 * time.Millisecond
		old, fresh := newRecordingSender(), newRecordingSender()
		d.RegisterStream(identity("acme"), old)
		start := time.Now()
		done := triggerAsync(d, "test")
		nextEstop(t, old, "the estop on the first stream")
		time.Sleep(100 * time.Millisecond)
		d.RegisterStream(identity("acme"), fresh)
		again := nextEstop(t, fresh, "the re-send inside the window")

		// No reset: the estop resolves Failed at the ORIGINAL deadline, not a new one.
		o := outcome(t, done)
		elapsed := time.Since(start)
		if o.res.State != fleetv1.RobotEstopFailed || !errors.Is(o.err, ErrUndelivered) {
			t.Fatalf("res=%+v err=%v; want Failed, undelivered", o.res, o.err)
		}
		if elapsed > 300*time.Millisecond+250*time.Millisecond {
			t.Fatalf("resolved after %s: the re-send extended the 300ms window", elapsed)
		}
		if err := d.RouteAck(identity("acme"), fresh, "amr-1",
			ackState(again.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
			t.Fatalf("a late ack is dropped, not refused: %v", err)
		}
		if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
			t.Fatalf("state = %s after a late ack, want Failed (never Stopped late)", got)
		}
	})
}

// Rule 3: only an estop still wanted is re-sent.
func TestResend_OnlyAnEstopStillWantedIsResent(t *testing.T) {
	t.Run("cleared between the push and the takeover", func(t *testing.T) {
		d, _, _ := newDispatcher(t, "acme")
		d.deliveryTimeout = 500 * time.Millisecond
		old, fresh := newRecordingSender(), newRecordingSender()
		d.RegisterStream(identity("acme"), old)
		done := triggerAsync(d, "test")
		nextEstop(t, old, "the estop on the first stream")

		if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "operator", Ends{}); err != nil {
			t.Fatalf("clear: %v", err)
		}
		d.RegisterStream(identity("acme"), fresh)
		nothingPushed(t, fresh, "an estop cleared before the takeover", 200*time.Millisecond)
		if o := outcome(t, done); o.res.Confirmed {
			t.Fatalf("a cleared estop was confirmed: %+v", o.res)
		}
	})

	// A confirmed estop is not moved to the new stream: it awaits nothing. Its stop still holds the
	// robot, so the new stream's opening pass sends it again, once, under its ID (RFC-0001
	// #safety-robot-stop-table, Sending a stop again).
	t.Run("resolved before the takeover", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.deliveryTimeout = 2 * time.Second
		g := &passGate{Client: d.Client, release: make(chan struct{})}
		d.Client = g
		old, fresh := newRecordingSender(), newRecordingSender()
		d.RegisterStream(identity("acme"), old)
		// The first stream's opening pass reads the robot in a goroutine of its own. Read after the
		// stop below, it would send the stop down the first stream; it reads first, as clearRigAt's.
		for end := time.Now().Add(2 * time.Second); g.lists.Load() < 1; time.Sleep(time.Millisecond) {
			if time.Now().After(end) {
				t.Fatal("setup: the first stream's opening pass did not read the robots")
			}
		}
		done := triggerAsync(d, "test")
		sent := nextEstop(t, old, "the estop on the first stream")
		id := sent.GetEstop().GetEstopId()
		if err := d.RouteAck(identity("acme"), old, "amr-1",
			ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
			t.Fatal(err)
		}
		// Taken over before the waiter has necessarily run: the resolving ack alone settles the
		// estop, so it is not moved. A move is pushed before RegisterStream returns; the new
		// stream's opening pass is held until after that, so what is pushed by then is a move.
		g.armed.Store(true)
		d.RegisterStream(identity("acme"), fresh)
		select {
		case m := <-fresh.sent:
			t.Fatalf("the confirmed estop %q was moved to the new stream", m.GetEstop().GetEstopId())
		default:
		}
		// The pass reads the robot once Stopped and the stop's record have landed. Read before the
		// record, the robot would be a stopped robot that records no stop, and the pass would send it
		// a stop of its own (resendHeld).
		for end := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
			r := stored(t, c)
			if r.Status.EstopState == fleetv1.RobotEstopStopped && len(r.Status.Stops) == 1 && r.Status.Stops[0].ID == id {
				break
			}
			if time.Now().After(end) {
				t.Fatalf("setup: stored %s %+v; want Stopped and the stop %s recorded", r.Status.EstopState, r.Status.Stops, id)
			}
		}
		close(g.release)
		again := nextEstop(t, fresh, "the stop sent again on the new stream")
		if got := again.GetEstop().GetEstopId(); got != id {
			t.Fatalf("sent again under estop_id %q, want the stop's own ID %q", got, id)
		}
		nothingPushed(t, fresh, "a second send of the stop", 200*time.Millisecond)
		if o := outcome(t, done); !o.res.Confirmed || estopState(t, c) != fleetv1.RobotEstopStopped {
			t.Fatalf("res=%+v state=%s; want confirmed Stopped on the first stream", o.res, estopState(t, c))
		}
		if stops := waitRecorded(t, c, 1); stops[0].ID != id {
			t.Fatalf("status.stops = %+v; want only the stop %s", stops, id)
		}
	})

	t.Run("superseded by a newer trigger for the same robot", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.deliveryTimeout = 2 * time.Second
		old, fresh := newRecordingSender(), newRecordingSender()
		d.RegisterStream(identity("acme"), old)
		first := triggerAsync(d, "first")
		nextEstop(t, old, "the first trigger")
		second := triggerAsync(d, "second")
		nextEstop(t, old, "the second trigger")

		d.RegisterStream(identity("acme"), fresh)
		again := nextEstop(t, fresh, "the re-send of the newer trigger")
		if r := again.GetEstop().GetReason(); r != "second" {
			t.Fatalf("re-sent the %q trigger; only the newer one is still wanted", r)
		}
		nothingPushed(t, fresh, "the superseded first trigger", 200*time.Millisecond)

		if err := d.RouteAck(identity("acme"), fresh, "amr-1",
			ackState(again.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
			t.Fatal(err)
		}
		if o := outcome(t, second); !o.res.Confirmed || estopState(t, c) != fleetv1.RobotEstopStopped {
			t.Fatalf("the newer trigger: %+v state=%s; want confirmed Stopped", o.res, estopState(t, c))
		}
		// The first is not re-sent, so it resolves at the end of its own window. It is not
		// confirmed by the newer trigger's ack.
		if o := outcome(t, first); o.res.Confirmed {
			t.Fatalf("the superseded trigger was confirmed: %+v", o.res)
		}
		// Its Failed does not overwrite the newer trigger's confirmed Stopped.
		if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
			t.Fatalf("estopState = %s after the superseded trigger resolved; the newer trigger's confirmed Stopped must stand", got)
		}
	})
}

// blockingSender holds each Send until released, then returns err.
type blockingSender struct {
	entered chan *fav1.ControlPlaneSafetyMessage
	release chan error
}

func (s *blockingSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	s.entered <- m
	return <-s.release
}

// A send down the first stream that fails because the stream was replaced while it was in
// flight does not resolve the estop Failed when it has already been re-sent.
func TestResend_ASendThatFailsAfterTheResendKeepsWaiting(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = 2 * time.Second
	old := &blockingSender{entered: make(chan *fav1.ControlPlaneSafetyMessage, 1), release: make(chan error, 1)}
	fresh := newRecordingSender()
	d.RegisterStream(identity("acme"), old)
	done := triggerAsync(d, "test")
	<-old.entered // the send down the first stream is in flight

	d.RegisterStream(identity("acme"), fresh)
	again := nextEstop(t, fresh, "the re-send while the first send is in flight")
	old.release <- errors.New("stream closed: superseded")

	select {
	case o := <-done:
		t.Fatalf("the estop resolved on the first stream's send error after it was re-sent: %+v err=%v", o.res, o.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := d.RouteAck(identity("acme"), fresh, "amr-1",
		ackState(again.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, done); !o.res.Confirmed || estopState(t, c) != fleetv1.RobotEstopStopped {
		t.Fatalf("res=%+v state=%s; want confirmed Stopped on the new stream", o.res, estopState(t, c))
	}
}

func waitState(t *testing.T, c client.Client, want fleetv1.RobotEstopState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if estopState(t, c) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("estopState never became %s (is %s)", want, estopState(t, c))
}
