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
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A trigger writes its outcome as soon as the adapter answers, and records its stop in a
// goroutine of its own (row S1), so a robot can be read in a stop state before the record of the
// stop that put it there has landed. A SafetyStream that opens then must send that stop again under
// its own ID (RFC-0001 #safety-robot-stop-table, *Sending a stop again*), not mint a second stop of
// the robot's own: the stop is in this process's state before its record lands.
//
// Every step here is ordered by the test, not by the scheduler: the record write is held at the
// client until the test lets it go, and the sends are read off a channel.

// estopTap is a SafetyStream that confirms every Estop STOPPED and hands its ID to the test.
type estopTap struct {
	d   *Dispatcher
	ids chan string
}

func (s *estopTap) Send(m *fav1.ControlPlaneSafetyMessage) error {
	if e := m.GetEstop(); e != nil {
		s.ids <- e.GetEstopId()
		_ = s.d.RouteAck(identity("acme"), s, m.GetRobotId(), ackState(e.GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED))
	}
	return nil
}

// nextEstop is the ID of the next Estop the tap carries. The bound only keeps a regression from
// hanging the run; nothing waits on it to order a step.
func (s *estopTap) nextEstop(t *testing.T) string {
	t.Helper()
	select {
	case id := <-s.ids:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("no Estop on the new SafetyStream")
		return ""
	}
}

// heldRecordRig is a dispatcher whose robot is Failed by a robot-scope trigger sent with no
// SafetyStream open, and whose stop's record is held at the client: a status patch that carries a
// stop waits until release is called. onList, when set, runs after each Robot list returns and
// before the caller sees it. It returns the trigger's stop ID.
func heldRecordRig(t *testing.T) (d *Dispatcher, c client.Client, h *hookClient, release func(), triggerID string) {
	t.Helper()
	d, c, _ = newDispatcher(t, "acme")
	gate := make(chan struct{})
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	h = &hookClient{Client: c, mu: &sync.Mutex{}, log: &[]string{}, before: func(_ int, obj client.Object) error {
		if r, ok := obj.(*fleetv1.Robot); ok && len(r.Status.Stops) > 0 {
			<-gate
		}
		return nil
	}}
	d.Client = h

	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "alice", metrics.ScopeRobot, Carrier{})
	if !errors.Is(err, ErrUndelivered) || res.State != fleetv1.RobotEstopFailed {
		t.Fatalf("setup: TriggerEstop = %+v, %v; want Failed, undelivered", res, err)
	}
	if r := stored(t, c); r.Status.EstopState != fleetv1.RobotEstopFailed || len(r.Status.Stops) != 0 {
		t.Fatalf("setup: estopState %s with %d records; want Failed and the record held", r.Status.EstopState, len(r.Status.Stops))
	}
	recs := d.inflightOf(robotEstopKey(ns, "amr-1"))
	if len(recs) != 1 {
		t.Fatalf("setup: %d stops in flight; want the trigger's", len(recs))
	}
	return d, c, h, release, recs[0].entry.ID
}

// listHook runs after each Robot list returns, before its caller sees it.
type listHook struct {
	*hookClient
	after func()
}

func (l *listHook) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := l.hookClient.List(ctx, list, opts...)
	if _, ok := list.(*fleetv1.RobotList); ok && l.after != nil {
		l.after()
	}
	return err
}

// passSignal closes listed once a Robot list has returned through it.
type passSignal struct {
	client.Client
	listed chan struct{}
	once   sync.Once
}

func (p *passSignal) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := p.Client.List(ctx, list, opts...)
	if _, ok := list.(*fleetv1.RobotList); ok {
		p.once.Do(func() { close(p.listed) })
	}
	return err
}

// registerAfterPass registers sender for "acme" and returns once the stream's opening pass
// (resendHeld) has listed the adapter's robots. A trigger issued after it returns is not read by
// that pass. One issued before could be: the pass would then send its stop again, from a goroutine
// of its own, beside the trigger. The client stays wrapped; the wrapper only signals.
func registerAfterPass(t *testing.T, d *Dispatcher, sender controlstream.SafetySender) {
	t.Helper()
	p := &passSignal{Client: d.Client, listed: make(chan struct{})}
	d.Client = p
	d.RegisterStream(identity("acme"), sender)
	select {
	case <-p.listed:
	case <-time.After(5 * time.Second):
		t.Fatal("the SafetyStream's opening pass never listed the robots")
	}
}

// The stream opens while the trigger's record is still in flight: the trigger's stop goes again
// under its own ID, and no second stop is minted.
func TestHeldStop_AStopInFlightGoesUnderItsOwnID(t *testing.T) {
	d, _, _, _, triggerID := heldRecordRig(t)
	tap := &estopTap{d: d, ids: make(chan string, 4)}
	d.RegisterStream(identity("acme"), tap)

	if got := tap.nextEstop(t); got != triggerID {
		t.Fatalf("the new SafetyStream carried stop %q; want the trigger's own stop %q, not a second one", got, triggerID)
	}
	if recs := d.inflightOf(robotEstopKey(ns, "amr-1")); len(recs) != 1 || recs[0].entry.ID != triggerID {
		ids := make([]string, 0, len(recs))
		for _, r := range recs {
			ids = append(ids, r.entry.ID)
		}
		t.Fatalf("stops in flight %v; want only the trigger's %q", ids, triggerID)
	}
}

// The record lands, and leaves this process's state, after the stream's opening pass has listed
// the robot and before it looks at the robot's stops in flight. The pass reads the robot again and
// finds the record: the trigger's stop goes again under its own ID.
func TestHeldStop_ARecordThatLandsAfterTheListGoesUnderItsOwnID(t *testing.T) {
	d, c, h, release, triggerID := heldRecordRig(t)
	key := robotEstopKey(ns, "amr-1")
	d.Client = &listHook{hookClient: h, after: func() {
		release()
		// The record's goroutine untracks the stop once its write has landed; wait for that.
		for deadline := time.Now().Add(5 * time.Second); len(d.inflightOf(key)) != 0; runtime.Gosched() {
			if time.Now().After(deadline) {
				t.Error("the trigger's record never left this process's state")
				return
			}
		}
	}}
	tap := &estopTap{d: d, ids: make(chan string, 4)}
	d.RegisterStream(identity("acme"), tap)

	if got := tap.nextEstop(t); got != triggerID {
		t.Fatalf("the new SafetyStream carried stop %q; want the trigger's own stop %q, not a second one", got, triggerID)
	}
	if stops := stored(t, c).Status.Stops; len(stops) != 1 || stops[0].ID != triggerID {
		t.Fatalf("recorded %+v; want only the trigger's stop %q", stops, triggerID)
	}
	if recs := d.inflightOf(key); len(recs) != 0 {
		t.Fatalf("%d stops in flight; want none: no second stop is minted", len(recs))
	}
}
