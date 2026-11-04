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

package flow_test

import (
	"strings"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

// The two flow-side readers of FleetAction.status.events[]: the select subject's
// event arm and loop.until's event arm. Both were schema-complete and inert
// until the events channel existed; every test here reads a REAL event through
// the same ChildState the controller fills, not a stub of the decision.

// setEvents places a member's child with a populated status.events[] window.
func (f *fakeCluster) setEvents(member string, iteration int32, phase fleetv1.ActionPhase, dropped int64, evs ...fleetv1.ActionEvent) {
	f.children[flow.Ref{Member: member, Iteration: iteration}] = flow.ChildState{
		Exists: true, Phase: phase, Events: evs, EventsDropped: dropped,
	}
}

func ev(seq int64, evType string, payload map[string]string) fleetv1.ActionEvent {
	return fleetv1.ActionEvent{Type: evType, Seq: seq, Payload: payload}
}

// ── select.subject{event, from, field} ───────────────────────────────────────

// THE ONE THIS EXISTS FOR: a resident watcher raises an event, and the select
// branches on a field of that event's payload while the watcher is STILL
// RUNNING. Waiting for a Service producer to settle would mean waiting for the
// thing that raises the event to stop existing.
func TestSelectEvent_BranchesOnAPayloadFieldWhileTheProducerRuns(t *testing.T) {
	fc := newFakeCluster()
	evType := "IntruderDetected"
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
		Cases: []fleetv1.SelectCase{
			{Equals: strptr("dock"), Then: run("alarm-dock")},
			{Equals: strptr("aisle"), Then: run("alarm-aisle")},
		},
		Default: run("log-only"),
	}}

	fc.setEvents("watch", 0, fleetv1.ActionPhaseInProgress, 0,
		ev(7, "IntruderDetected", map[string]string{"zone": "aisle", "confidence": "0.91"}))

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress (the chosen branch has not run yet)", eval.Outcome)
	}
	mustContainRef(t, eval.Ensure, "alarm-aisle", 0)
	for _, r := range eval.Ensure {
		if r.Member == "alarm-dock" || r.Member == "log-only" {
			t.Fatalf("wrong branch started: %q — the event payload said zone=aisle", r.Member)
		}
	}
	if eval.Diagnostic != "" {
		t.Fatalf("an ordinary, readable outcome must carry no diagnostic; got %q", eval.Diagnostic)
	}
}

// The HIGHEST seq of the named type wins, not the first retained. A recognition
// service raises the same type repeatedly; "what was recognised" means the most
// recent recognition.
func TestSelectEvent_ReadsTheHighestSeqOfThatType(t *testing.T) {
	fc := newFakeCluster()
	evType := "IntruderDetected"
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
		Cases: []fleetv1.SelectCase{
			{Equals: strptr("dock"), Then: run("alarm-dock")},
			{Equals: strptr("aisle"), Then: run("alarm-aisle")},
		},
		Default: run("log-only"),
	}}

	// Deliberately NOT in seq order in the slice, and with an interleaved event
	// of another type that must be ignored entirely.
	fc.setEvents("watch", 0, fleetv1.ActionPhaseInProgress, 0,
		ev(4, "IntruderDetected", map[string]string{"zone": "dock"}),
		ev(9, "IntruderDetected", map[string]string{"zone": "aisle"}),
		ev(11, "HeartbeatSeen", map[string]string{"zone": "dock"}),
		ev(5, "IntruderDetected", map[string]string{"zone": "dock"}),
	)

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "alarm-aisle", 0)
}

// A select whose subject event exists but whose cases do not match DOES take
// `default` — and that is correct here, because a value WAS read. The contrast
// with the absent-event case below is the whole point: "none of your cases
// matched" is a claim about a value, and it is now a true one.
func TestSelectEvent_TakesDefaultWhenTheEventWasReadAndNoCaseMatched(t *testing.T) {
	fc := newFakeCluster()
	evType := "IntruderDetected"
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
		Cases:   []fleetv1.SelectCase{{Equals: strptr("dock"), Then: run("alarm-dock")}},
		Default: run("log-only"),
	}}
	fc.setEvents("watch", 0, fleetv1.ActionPhaseInProgress, 0,
		ev(3, "IntruderDetected", map[string]string{"zone": "loading-bay"}))

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "log-only", 0)
	if eval.Diagnostic != "" {
		t.Fatalf("a read value that matched no case is an ordinary outcome; got diagnostic %q", eval.Diagnostic)
	}
}

// While the producer is still running and has raised nothing, the node WAITS.
// A resident watcher that has not yet recognised anything has not failed to.
func TestSelectEvent_WaitsWhileTheProducerIsStillRunning(t *testing.T) {
	fc := newFakeCluster()
	evType := "IntruderDetected"
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
		Cases:   []fleetv1.SelectCase{{Equals: strptr("dock"), Then: run("alarm-dock")}},
		Default: run("log-only"),
	}}
	fc.setEvents("watch", 0, fleetv1.ActionPhaseInProgress, 0)

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress", eval.Outcome)
	}
	for _, r := range eval.Ensure {
		if r.Member == "alarm-dock" || r.Member == "log-only" {
			t.Fatalf("branch %q started before any event was raised", r.Member)
		}
	}
	if eval.Diagnostic != "" {
		t.Fatalf("waiting is not failing; got diagnostic %q", eval.Diagnostic)
	}
}

// The producer has not been started at all: the select ENSURES it, rather than
// failing on a member that was never asked to run. Without this the event arm
// would deadlock on a watcher nobody created.
func TestSelectEvent_EnsuresTheProducerWhenItDoesNotExist(t *testing.T) {
	fc := newFakeCluster()
	evType := "IntruderDetected"
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
		Cases:   []fleetv1.SelectCase{{Equals: strptr("dock"), Then: run("alarm-dock")}},
		Default: run("log-only"),
	}}

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "watch", 0)
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress", eval.Outcome)
	}
}

// RETENTION IS VISIBLE IN THE DIAGNOSTIC. "Never raised" and "raised, then
// evicted from the bounded window" are different facts, and an author whose task
// failed needs to know which one they are looking at — the bound is the one
// thing in this design that can make a correctly-written task fail with no other
// trace.
func TestSelectEvent_DiagnosticDistinguishesEvictionFromNeverRaised(t *testing.T) {
	evType := "IntruderDetected"
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
		Cases:   []fleetv1.SelectCase{{Equals: strptr("dock"), Then: run("alarm-dock")}},
		Default: run("log-only"),
	}}

	neverRaised := newFakeCluster()
	neverRaised.setEvents("watch", 0, fleetv1.ActionPhaseSucceeded, 0)
	d1 := flow.Evaluate(&tree, neverRaised.ctx()).Diagnostic
	if !strings.Contains(d1, "has ever been raised") {
		t.Fatalf("with nothing evicted the diagnostic must say none was ever raised; got %q", d1)
	}

	evicted := newFakeCluster()
	evicted.setEvents("watch", 0, fleetv1.ActionPhaseSucceeded, 40,
		ev(99, "HeartbeatSeen", nil))
	d2 := flow.Evaluate(&tree, evicted.ctx()).Diagnostic
	if !strings.Contains(d2, "evicted") || !strings.Contains(d2, "40") {
		t.Fatalf("with events evicted the diagnostic must say so, and how many; got %q", d2)
	}
	if d1 == d2 {
		t.Fatal("the two absences produced the same message — which is the confusion this exists to prevent")
	}
}

// `present` reads the event's payload, not the producer's result. A field the
// event does not carry is absent, and an absent field is a fact a case may
// branch on.
func TestSelectEvent_PresentReadsTheEventPayload(t *testing.T) {
	fc := newFakeCluster()
	evType := "PlateRead"
	no := false
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &evType, From: "reader", Field: "plate"},
		Cases:   []fleetv1.SelectCase{{Present: &no, Then: run("escalate")}},
		Default: run("record"),
	}}
	fc.setEvents("reader", 0, fleetv1.ActionPhaseInProgress, 0,
		ev(2, "PlateRead", map[string]string{"confidence": "0.4"}))

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "escalate", 0)
}

// ── loop.until.anyOf[].event ─────────────────────────────────────────────────

// A Forever loop exits when a watcher OUTSIDE it raises the named event. The
// watcher lives at iteration 0 beside the loop, which is the shape the design's
// own case describes and the one the iteration-0 fallback exists for.
func TestUntilEvent_ForeverLoopExitsOnAnEventFromASiblingWatcher(t *testing.T) {
	fc := newFakeCluster()
	body := run("patrol")
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeForever,
		Body: body,
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{
			{Event: &fleetv1.UntilEventCondition{Type: "IntruderDetected", From: "watch"}},
		}},
		MaxIterations: int32ptr(50),
	}}

	// Iteration 3 of the body has just succeeded; the watcher has raised nothing.
	fc.progress["patrol"] = flow.LoopProgress{Iteration: 3, StartedAt: timeptr(time.Now().Add(-time.Minute))}
	fc.set("patrol", 3, fleetv1.ActionPhaseSucceeded)
	fc.setEvents("watch", 0, fleetv1.ActionPhaseInProgress, 0)

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress — nothing has been recognised yet", eval.Outcome)
	}
	if eval.StartIteration["patrol"] != 4 {
		t.Fatalf("StartIteration = %v, want a 4th iteration", eval.StartIteration)
	}

	// Now the watcher raises it. The loop must stop, and must NOT start a 4th.
	fc.setEvents("watch", 0, fleetv1.ActionPhaseInProgress, 0,
		ev(12, "IntruderDetected", map[string]string{"zone": "dock"}))

	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("outcome = %v, want Succeeded — the until condition is satisfied", eval.Outcome)
	}
	if _, started := eval.StartIteration["patrol"]; started {
		t.Fatalf("a further iteration was started after the exit condition held: %v", eval.StartIteration)
	}
}

// Only the NAMED type from the NAMED member satisfies it. A loop that exited on
// any event from anyone would exit on the first heartbeat.
func TestUntilEvent_IgnoresOtherTypesAndOtherMembers(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeForever,
		Body: run("patrol"),
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{
			{Event: &fleetv1.UntilEventCondition{Type: "IntruderDetected", From: "watch"}},
		}},
		MaxIterations: int32ptr(50),
	}}
	fc.progress["patrol"] = flow.LoopProgress{Iteration: 1, StartedAt: timeptr(time.Now())}
	fc.set("patrol", 1, fleetv1.ActionPhaseSucceeded)
	// Right type, wrong member; and right member, wrong type.
	fc.setEvents("other", 0, fleetv1.ActionPhaseInProgress, 0, ev(1, "IntruderDetected", nil))
	fc.setEvents("watch", 0, fleetv1.ActionPhaseInProgress, 0, ev(2, "HeartbeatSeen", nil))

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress — neither event satisfies the condition", eval.Outcome)
	}
}

// The event arm and the result arm coexist in one anyOf, and EITHER satisfies
// it. Until both arms were readable this could not be tested at all.
func TestUntilEvent_EitherArmOfAnyOfSatisfiesTheLoop(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeForever,
		Body: run("patrol"),
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{
			{Event: &fleetv1.UntilEventCondition{Type: "IntruderDetected", From: "watch"}},
			{Result: &fleetv1.UntilResultCondition{Member: "patrol", Field: "done", Equals: "true"}},
		}},
		MaxIterations: int32ptr(50),
	}}
	fc.progress["patrol"] = flow.LoopProgress{Iteration: 2, StartedAt: timeptr(time.Now())}
	fc.children[flow.Ref{Member: "patrol", Iteration: 2}] = flow.ChildState{
		Exists: true,
		Phase:  fleetv1.ActionPhaseSucceeded,
		Result: &fleetv1.ActionResult{Values: map[string]string{"done": "true"}},
	}
	fc.setEvents("watch", 0, fleetv1.ActionPhaseInProgress, 0)

	if got := flow.Evaluate(&tree, fc.ctx()).Outcome; got != flow.OutcomeSucceeded {
		t.Fatalf("result arm alone: outcome = %v, want Succeeded", got)
	}
}

func int32ptr(v int32) *int32        { return &v }
func timeptr(t time.Time) *time.Time { return &t }
