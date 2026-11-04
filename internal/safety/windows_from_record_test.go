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

// Row S4's windows (RFC-0001 #safety-robot-stop-table): "Each stop's windows are its own. They run
// from its persisted sentAt (S1) and acknowledgedAt (S2) in status.stops, not from a timer in one
// replica. Until the record lands, the sending leader's own timer runs the windows. A leader that
// takes over runs out a window its predecessor recorded, and writes Failed itself." And: "A re-send's
// delivery window runs from that re-send. No re-send restarts the confirmation window."

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// answeringSender records every Estop it carries and answers each with states, after delay.
type answeringSender struct {
	d      *Dispatcher
	delay  time.Duration
	states []fav1.EstopState
	mu     sync.Mutex
	ids    []string
}

func (s *answeringSender) Send(m *fav1.ControlPlaneSafetyMessage) error {
	e := m.GetEstop()
	if e == nil {
		return nil
	}
	s.mu.Lock()
	s.ids = append(s.ids, e.GetEstopId())
	s.mu.Unlock()
	go func() {
		time.Sleep(s.delay)
		for _, st := range s.states {
			_ = s.d.RouteAck(identity("acme"), s, m.GetRobotId(), ackState(e.GetEstopId(), st))
		}
	}()
	return nil
}

func (s *answeringSender) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

// slowConfigs delays every SwarmadaConfig list: the read the delivery window's length comes from.
type slowConfigs struct {
	client.Client
	delay time.Duration
}

func (s *slowConfigs) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*fleetv1.SwarmadaConfigList); ok {
		time.Sleep(s.delay)
	}
	return s.Client.List(ctx, list, opts...)
}

// robotCarrier is the carrier the robot reconciler names for amr-1's own stop.
func robotCarrier() Carrier { return Carrier{Kind: fleetv1.StopCarrierRobot, Name: "amr-1"} }

// withRecordedStop records on amr-1, as a predecessor leader would have, the robot-scope stop pred-1
// sent at sentAt, first acknowledged at acked (nil: never), in estopState state.
func withRecordedStop(t *testing.T, c client.Client, sentAt time.Time, acked *time.Time, state fleetv1.RobotEstopState) {
	t.Helper()
	r := stored(t, c)
	st := fleetv1.RobotStop{ID: "pred-1", Scope: fleetv1.StopScopeRobot, SentAt: metav1.NewTime(sentAt),
		Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierRobot, Name: "amr-1"}}
	if acked != nil {
		a := metav1.NewTime(*acked)
		st.AcknowledgedAt = &a
	}
	r.Status.Stops = []fleetv1.RobotStop{st}
	r.Status.EstopState = state
	if err := c.Status().Update(context.Background(), r); err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func triggerOwn(d *Dispatcher) (Result, time.Duration, error) {
	start := time.Now()
	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "alice", metrics.ScopeRobot, robotCarrier())
	return res, time.Since(start), err
}

// The delivery window of a first send runs from its sentAt, the time the record stores, not from a
// later instant: a slow read of the window's own length does not lengthen it.
func TestWindows_TheDeliveryWindowRunsFromSentAt(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = 300 * time.Millisecond
	d.Client = &slowConfigs{Client: c, delay: 400 * time.Millisecond}
	d.RegisterStream(identity("acme"), &answeringSender{d: d}) // never answers

	res, took, _ := triggerOwn(d)
	if res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("state %s, want Failed: no answer came", res.State)
	}
	if took > 600*time.Millisecond {
		t.Fatalf("Failed after %v: the delivery window ran from after the 400ms read, not from sentAt "+
			"(300ms window)", took)
	}
}

// The confirmation window runs from acknowledgedAt, the first STOPPING's arrival, not from after the
// Stopping write.
func TestWindows_TheConfirmationWindowRunsFromAcknowledgedAt(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.confirmTimeout = 300 * time.Millisecond
	mu, log := &sync.Mutex{}, &[]string{}
	var once sync.Once
	d.Client = &hookClient{Client: c, mu: mu, log: log, before: func(_ int, obj client.Object) error {
		// The Stopping write, the first patch that moves the robot to Stopping, is slow.
		if r, ok := obj.(*fleetv1.Robot); ok && r.Status.EstopState == fleetv1.RobotEstopStopping {
			once.Do(func() { time.Sleep(400 * time.Millisecond) })
		}
		return nil
	}}
	d.RegisterStream(identity("acme"), &answeringSender{d: d, states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPING}})

	res, took, _ := triggerOwn(d)
	if res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("state %s, want Failed: STOPPING and no STOPPED", res.State)
	}
	if took > 600*time.Millisecond {
		t.Fatalf("Failed after %v: the confirmation window ran from after the 400ms Stopping write, not from "+
			"the STOPPING's arrival (300ms window)", took)
	}
}

// Row 26: a leader that takes over a recorded stop, acknowledged but not confirmed, runs out the
// confirmation window its predecessor recorded. The re-send goes under the recorded ID, and its
// STOPPING does not restart the window.
func TestTakeover_RunsOutTheConfirmationWindowItsPredecessorRecorded(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.confirmTimeout = 2500 * time.Millisecond
	// Whole seconds: the record stores a time to the second.
	acked := time.Now().Truncate(time.Second).Add(-time.Second)
	withRecordedStop(t, c, acked.Add(-time.Second), &acked, fleetv1.RobotEstopStopping)
	s := &answeringSender{d: d, states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPING}}
	d.RegisterStream(identity("acme"), s)

	res, took, _ := triggerOwn(d)
	if res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("state %s, want Failed", res.State)
	}
	if took > 2*time.Second {
		t.Fatalf("Failed after %v: the window ends 2.5s after the recorded acknowledgedAt (1-2s ago), so a "+
			"re-send restarted it", took)
	}
	if ids := s.sent(); len(ids) == 0 || ids[0] != "pred-1" {
		t.Fatalf("Estops sent %v, want the recorded stop pred-1 sent again under its ID", ids)
	}
}

// Row 27: with the recorded window already run out, the new leader writes Failed at once; with
// sentAt only, the re-send's delivery window runs from the re-send; and the trigger sends the
// recorded stop again rather than issuing a fresh one with fresh windows.
func TestTakeover_TheRecordDecides(t *testing.T) {
	t.Run("window run out: Failed at once, then sent again", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.confirmTimeout = 2 * time.Second
		d.deliveryTimeout = 2 * time.Second
		acked := time.Now().Add(-5 * time.Second)
		withRecordedStop(t, c, acked.Add(-time.Second), &acked, fleetv1.RobotEstopStopping)
		s := &answeringSender{d: d, delay: 400 * time.Millisecond,
			states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPED}}
		d.RegisterStream(identity("acme"), s)

		done := make(chan Result, 1)
		go func() { res, _, _ := triggerOwn(d); done <- res }()
		time.Sleep(200 * time.Millisecond)
		if got := estopState(t, c); got != fleetv1.RobotEstopFailed {
			t.Fatalf("estopState %s before the re-send's answer, want Failed written at once: the window its "+
				"predecessor recorded had run out", got)
		}
		if res := <-done; res.State != fleetv1.RobotEstopStopped || estopState(t, c) != fleetv1.RobotEstopStopped {
			t.Fatalf("result %s, stored %s; want Stopped: the STOPPED answering the re-send confirms rest (S3)",
				res.State, estopState(t, c))
		}
		if ids := s.sent(); len(ids) == 0 || ids[0] != "pred-1" {
			t.Fatalf("Estops sent %v, want pred-1 sent again", ids)
		}
	})
	t.Run("sentAt only: delivery runs from the re-send", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.deliveryTimeout = 300 * time.Millisecond
		withRecordedStop(t, c, time.Now().Add(-time.Hour), nil, fleetv1.RobotEstopNormal)
		s := &answeringSender{d: d, delay: 100 * time.Millisecond,
			states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPED}}
		d.RegisterStream(identity("acme"), s)

		res, _, err := triggerOwn(d)
		if err != nil || res.State != fleetv1.RobotEstopStopped {
			t.Fatalf("%s, %v; want Stopped: the answer came within the re-send's window", res.State, err)
		}
		if ids := s.sent(); len(ids) != 1 || ids[0] != "pred-1" {
			t.Fatalf("Estops sent %v, want the recorded stop pred-1 once", ids)
		}
	})
	t.Run("no fresh stop is recorded beside it", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		withRecordedStop(t, c, time.Now().Add(-time.Hour), nil, fleetv1.RobotEstopNormal)
		d.RegisterStream(identity("acme"), &answeringSender{d: d,
			states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPED}})
		if res, _, err := triggerOwn(d); err != nil || res.State != fleetv1.RobotEstopStopped {
			t.Fatalf("%s, %v", res.State, err)
		}
		time.Sleep(100 * time.Millisecond) // a fresh stop's record would land by now
		if st := stored(t, c).Status.Stops; len(st) != 1 || st[0].ID != "pred-1" {
			t.Fatalf("status.stops %v, want only pred-1", st)
		}
	})
	t.Run("a Stopped robot is not failed at once", func(t *testing.T) {
		d, c, _ := newDispatcher(t, "acme")
		d.confirmTimeout = 2 * time.Second
		d.deliveryTimeout = 2 * time.Second
		acked := time.Now().Add(-5 * time.Second)
		withRecordedStop(t, c, acked.Add(-time.Second), &acked, fleetv1.RobotEstopStopped)
		s := &answeringSender{d: d, delay: 400 * time.Millisecond,
			states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPED}}
		d.RegisterStream(identity("acme"), s)
		done := make(chan Result, 1)
		go func() { res, _, _ := triggerOwn(d); done <- res }()
		time.Sleep(200 * time.Millisecond)
		if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
			t.Fatalf("estopState %s while the re-send's answer is awaited, want Stopped kept: this leader holds "+
				"no record of which stop's Stopped is stored, so the answer decides", got)
		}
		if res := <-done; res.State != fleetv1.RobotEstopStopped {
			t.Fatalf("%s; want Stopped: the answer to the re-send confirms it", res.State)
		}
		if ids := s.sent(); len(ids) == 0 || ids[0] != "pred-1" {
			t.Fatalf("Estops sent %v, want pred-1 sent again", ids)
		}
	})
}

// A guard: a stop this process issued is its own, not a predecessor's. A trigger that fires again
// for the same carrier issues a stop of its own, as before.
func TestTakeover_AStopThisProcessIssuedIsNotTakenOver(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	s := &answeringSender{d: d, states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPED}}
	d.RegisterStream(identity("acme"), s)
	if res, _, err := triggerOwn(d); err != nil || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("first: %s, %v", res.State, err)
	}
	first := waitRecorded(t, c, 1)[0].ID
	if res, _, err := triggerOwn(d); err != nil || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("second: %s, %v", res.State, err)
	}
	if ids := s.sent(); len(ids) != 2 || ids[1] == first {
		t.Fatalf("Estops sent %v; want a second stop of this process's own, not %s again", ids, first)
	}
}

// A guard: a waiter's deregister removes only its own entry. Two waiters for one ID, a takeover's
// and a stream-open re-send's, cannot drop each other's.
func TestTakeover_ADeregisterRemovesOnlyItsOwnWaiter(t *testing.T) {
	d, _, _ := newDispatcher(t, "acme")
	d.RegisterStream(identity("acme"), &answeringSender{d: d})
	_, first, _ := d.register(ns, "amr-1", "amr-1", "acme", "pred-1", 0, &fav1.Estop{})
	_, second, _ := d.register(ns, "amr-1", "amr-1", "acme", "pred-1", 1, &fav1.Estop{})
	d.deregister(first)
	d.mu.Lock()
	got := d.pending[second.key]
	d.mu.Unlock()
	if got != second {
		t.Fatalf("after the first waiter's deregister the ID is awaited by %p, want the second %p", got, second)
	}
}

// A guard: a takeover's waiter and the re-send of a SafetyStream share the stop's ID. The re-send of
// the stream the takeover found registered first; the takeover takes its waiter, so that re-send
// records no unanswered stop. A stream that opens during the takeover leaves the stop to the
// takeover's waiter, which takes its answer.
func TestTakeover_ANewStreamDoesNotTakeTheTakeoversAnswer(t *testing.T) {
	d, c, rec := newDispatcher(t, "acme")
	d.deliveryTimeout = 500 * time.Millisecond
	withRecordedStop(t, c, time.Now().Add(-time.Hour), nil, fleetv1.RobotEstopStopping)
	key := pendingKey{namespace: ns, robot: "amr-1", estopID: "pred-1"}
	awaitedAt := func(epoch func(uint64) bool) bool {
		for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
			d.mu.Lock()
			p := d.pending[key]
			d.mu.Unlock()
			if p != nil && epoch(p.epoch) {
				return true
			}
		}
		return false
	}
	d.RegisterStream(identity("acme"), &answeringSender{d: d}) // silent: the stream the takeover finds
	if !awaitedAt(func(e uint64) bool { return e == 0 }) {
		t.Fatal("setup: the stream's opening pass did not re-send the recorded stop")
	}

	done := make(chan Result, 1)
	go func() { res, _, _ := triggerOwn(d); done <- res }()
	if !awaitedAt(func(e uint64) bool { return e != 0 }) {
		t.Fatal("the takeover did not take the stop's waiter")
	}
	// Its answer comes after its own opening pass has reached the stop.
	d.RegisterStream(identity("acme"), &answeringSender{d: d, delay: 150 * time.Millisecond,
		states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPED}})
	select {
	case res := <-done:
		if res.State != fleetv1.RobotEstopStopped {
			t.Fatalf("state %s, want Stopped: the new stream's STOPPED answers the stop being run out", res.State)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the takeover never resolved")
	}
	// Past the window of the first stream's re-send: it does not say the stop went unanswered.
	time.Sleep(d.deliveryTimeout + d.confirmTimeout + 400*time.Millisecond)
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.Contains(e, "EstopResendUnconfirmed") {
			t.Fatalf("event %q: a re-send whose stop a takeover answered was recorded unanswered", e)
		}
	}
}

// slowRobotLists delays every Robot list: a SafetyStream's opening pass reads the adapter's robots
// with one.
type slowRobotLists struct {
	client.Client
	delay time.Duration
}

func (s *slowRobotLists) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*fleetv1.RobotList); ok {
		time.Sleep(s.delay)
	}
	return s.Client.List(ctx, list, opts...)
}

// A guard: the opening pass of the stream a takeover sends on can reach the stop after the takeover
// has taken it. It leaves the stop to the takeover's waiter, which takes the answer.
func TestTakeover_TheSameStreamsOpeningPassLeavesTheStopToTheTakeover(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = time.Second
	withRecordedStop(t, c, time.Now().Add(-time.Hour), nil, fleetv1.RobotEstopStopping)
	d.Client = &slowRobotLists{Client: c, delay: 200 * time.Millisecond}
	d.RegisterStream(identity("acme"), &answeringSender{d: d, delay: 500 * time.Millisecond,
		states: []fav1.EstopState{fav1.EstopState_ESTOP_STATE_STOPPED}})

	if res, _, _ := triggerOwn(d); res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("state %s, want Stopped: the answer to the takeover's send is the takeover's", res.State)
	}
}

// A guard: a stop in force whose waiter stays on the stream it went down (a newer trigger means no
// takeover moves it) is still sent again on a SafetyStream that opens, with its ID, and its answer
// there goes to that waiter (Sending a stop again: every stop in force is sent again with its ID).
func TestTakeover_AnAwaitedStopNotMovedIsSentOnTheNewStreamToItsWaiter(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.deliveryTimeout = 5 * time.Second // a's re-send is still awaited when b opens
	withRecordedStop(t, c, time.Now().Add(-time.Hour), nil, fleetv1.RobotEstopStopping)
	a := &answeringSender{d: d} // silent
	d.RegisterStream(identity("acme"), a)
	for end := time.Now().Add(time.Second); len(a.sent()) == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond) // a's opening pass re-sends pred-1 and awaits it
	}
	// A newer trigger stands, so the waiter is not one a new stream takes over (rebindLocked).
	o := d.orderFor(robotEstopKey(ns, "amr-1"))
	d.mu.Lock()
	first := d.pending[pendingKey{namespace: ns, robot: "amr-1", estopID: "pred-1"}]
	o.lastStop = 99
	d.mu.Unlock()
	if first == nil {
		t.Fatal("setup: a's opening pass did not await pred-1")
	}
	b := &answeringSender{d: d}
	d.RegisterStream(identity("acme"), b)
	for end := time.Now().Add(time.Second); len(b.sent()) == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	if ids := b.sent(); len(ids) != 1 || ids[0] != "pred-1" {
		t.Fatalf("Estops on the new stream %v, want pred-1 sent again under its ID", ids)
	}
	d.mu.Lock()
	got := d.pending[first.key]
	sender := first.sender
	d.mu.Unlock()
	if got != first || sender != b {
		t.Fatalf("the stop's waiter is %p on %p, want the first waiter %p moved to the new stream %p", got, sender, first, b)
	}
}
