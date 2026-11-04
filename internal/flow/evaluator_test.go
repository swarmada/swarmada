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

// fakeCluster is an in-memory stand-in for the FleetTask controller's view of
// child FleetActions, keyed by (member, iteration).
type fakeCluster struct {
	children map[flow.Ref]flow.ChildState
	progress map[string]flow.LoopProgress
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{children: map[flow.Ref]flow.ChildState{}, progress: map[string]flow.LoopProgress{}}
}

func (f *fakeCluster) ctx() *flow.Context {
	return &flow.Context{
		Now: time.Now(),
		Child: func(member string, iteration int32) flow.ChildState {
			return f.children[flow.Ref{Member: member, Iteration: iteration}]
		},
		LoopProgress: func(anchor string) flow.LoopProgress {
			return f.progress[anchor]
		},
	}
}

func (f *fakeCluster) set(member string, iteration int32, phase fleetv1.ActionPhase) {
	f.children[flow.Ref{Member: member, Iteration: iteration}] = flow.ChildState{Exists: true, Phase: phase}
}

func run(name string) fleetv1.FlowNode { return fleetv1.FlowNode{Run: &name} }

func strptr(s string) *string { return &s }

func mustContainRef(t *testing.T, refs []flow.Ref, member string, iter int32) {
	t.Helper()
	for _, r := range refs {
		if r.Member == member && r.Iteration == iter {
			return
		}
	}
	t.Fatalf("expected %v to contain {%s, %d}", refs, member, iter)
}

// ── sequence ─────────────────────────────────────────────────────────────────

// THE CRITICAL ONE: a sequence starts its first node, and does NOT start the
// second until the first settles Succeeded.
func TestSequence_SecondNodeWaitsForFirst(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{run("a"), run("b")}}

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress", eval.Outcome)
	}
	mustContainRef(t, eval.Ensure, "a", 0)
	if len(eval.Ensure) != 1 {
		t.Fatalf("expected ONLY 'a' to be ensured before it settles, got %v", eval.Ensure)
	}

	// 'a' is still running: 'b' must still not start.
	fc.set("a", 0, fleetv1.ActionPhaseInProgress)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress || len(eval.Ensure) != 0 {
		t.Fatalf("while 'a' runs: outcome=%v ensure=%v, want InProgress and no new ensures", eval.Outcome, eval.Ensure)
	}

	// 'a' succeeds: 'b' starts.
	fc.set("a", 0, fleetv1.ActionPhaseSucceeded)
	eval = flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "b", 0)
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress ('b' not yet settled)", eval.Outcome)
	}

	// 'b' succeeds: the sequence (and the whole tree) succeeds.
	fc.set("b", 0, fleetv1.ActionPhaseSucceeded)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("outcome = %v, want Succeeded", eval.Outcome)
	}
}

// A non-Succeeded settlement short-circuits: later nodes never start.
func TestSequence_FailureShortCircuits(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{run("a"), run("b")}}
	fc.set("a", 0, fleetv1.ActionPhaseFailed)

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed", eval.Outcome)
	}
	for _, r := range eval.Ensure {
		if r.Member == "b" {
			t.Fatalf("'b' must never start after 'a' failed, got ensure %v", eval.Ensure)
		}
	}
}

// Absent flow, behaviour is identical to v0.3 — a single `run:` leaf behaves
// exactly like a one-member task with no dependsOn.
func TestSequence_SingleRunLeaf(t *testing.T) {
	fc := newFakeCluster()
	tree := run("a")
	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "a", 0)
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress", eval.Outcome)
	}
}

// The empty node {} is an explicit no-op that settles Succeeded immediately.
func TestEmptyNode_SettlesSucceeded(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{}
	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("outcome = %v, want Succeeded", eval.Outcome)
	}
	if len(eval.Ensure) != 0 {
		t.Fatalf("the empty node must not ensure anything, got %v", eval.Ensure)
	}
}

// ── parallel + join ──────────────────────────────────────────────────────────

func parallelTree(branches []fleetv1.FlowNode, join *fleetv1.JoinSpec) fleetv1.FlowNode {
	return fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{Branches: branches, Join: join}}
}

// join: All (default) — every branch must succeed.
func TestParallel_JoinAll_WaitsForEveryBranch(t *testing.T) {
	fc := newFakeCluster()
	tree := parallelTree([]fleetv1.FlowNode{run("a"), run("b"), run("c")}, nil)

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "a", 0)
	mustContainRef(t, eval.Ensure, "b", 0)
	mustContainRef(t, eval.Ensure, "c", 0)

	fc.set("a", 0, fleetv1.ActionPhaseSucceeded)
	fc.set("b", 0, fleetv1.ActionPhaseSucceeded)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("two of three done: outcome = %v, want InProgress", eval.Outcome)
	}

	fc.set("c", 0, fleetv1.ActionPhaseSucceeded)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("all three done: outcome = %v, want Succeeded", eval.Outcome)
	}
}

// THE ACCEPTANCE SCENARIO: three concurrent steps joined on the
// FIRST SUCCESS, with the remainder CONFIRMED-CANCELLED (not abandoned) —
// and the node itself does not report terminal until every branch, including
// the cancelled ones, has actually settled.
func TestParallel_JoinAny_FirstSuccessCancelsSiblings(t *testing.T) {
	fc := newFakeCluster()
	tree := parallelTree([]fleetv1.FlowNode{run("a"), run("b"), run("c")},
		&fleetv1.JoinSpec{Mode: fleetv1.JoinModeAny})
	fc.set("a", 0, fleetv1.ActionPhaseInProgress)
	fc.set("b", 0, fleetv1.ActionPhaseInProgress)
	fc.set("c", 0, fleetv1.ActionPhaseSucceeded)

	eval := flow.Evaluate(&tree, fc.ctx())
	// Join is satisfied by 'c', but 'a' and 'b' are still non-terminal — the
	// node must NOT report Succeeded yet (RULE: terminal only once every
	// branch has settled).
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress — 'a' and 'b' have not settled yet", eval.Outcome)
	}
	// The still-running siblings must be REQUESTED for confirmed-cancel —
	// never silently abandoned.
	mustContainRef(t, eval.Cancel, "a", 0)
	mustContainRef(t, eval.Cancel, "b", 0)

	// The FleetAction controller's OWN confirmed-cancel path (not this
	// package) eventually lands 'a' and 'b' on Cancelled. Once it does:
	fc.set("a", 0, fleetv1.ActionPhaseCancelled)
	fc.set("b", 0, fleetv1.ActionPhaseCancelled)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("all settled (2 cancelled, 1 succeeded): outcome = %v, want Succeeded", eval.Outcome)
	}
}

// Any fails only once EVERY branch has settled non-Succeeded — a still-live
// branch could still turn the join, so Any must not give up early.
func TestParallel_JoinAny_FailsOnlyWhenAllBranchesAreDone(t *testing.T) {
	fc := newFakeCluster()
	tree := parallelTree([]fleetv1.FlowNode{run("a"), run("b")}, &fleetv1.JoinSpec{Mode: fleetv1.JoinModeAny})
	fc.set("a", 0, fleetv1.ActionPhaseFailed)
	fc.set("b", 0, fleetv1.ActionPhaseInProgress)

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("'b' still running: outcome = %v, want InProgress", eval.Outcome)
	}

	fc.set("b", 0, fleetv1.ActionPhaseFailed)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeFailed {
		t.Fatalf("both failed: outcome = %v, want Failed", eval.Outcome)
	}
}

// First takes WHATEVER settles first, success or not — the distinction from
// Any that the design calls out as easy to get wrong.
func TestParallel_JoinFirst_TakesWhicheverSettlesFirstEvenAFailure(t *testing.T) {
	fc := newFakeCluster()
	tree := parallelTree([]fleetv1.FlowNode{run("a"), run("b")}, &fleetv1.JoinSpec{Mode: fleetv1.JoinModeFirst})
	fc.set("a", 0, fleetv1.ActionPhaseFailed) // settles first, and it's a failure
	fc.set("b", 0, fleetv1.ActionPhaseInProgress)

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("'b' still running: outcome = %v, want InProgress (waiting for 'b' to settle)", eval.Outcome)
	}
	mustContainRef(t, eval.Cancel, "b", 0)

	fc.set("b", 0, fleetv1.ActionPhaseCancelled)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed ('a' settled first, with a failure)", eval.Outcome)
	}
}

// Quorum: 2 of 3 — succeeds once two branches succeed, without waiting for
// the third; the third is cancelled (CancelSiblings default).
func TestParallel_JoinQuorum(t *testing.T) {
	fc := newFakeCluster()
	two := int32(2)
	tree := parallelTree([]fleetv1.FlowNode{run("a"), run("b"), run("c")},
		&fleetv1.JoinSpec{Mode: fleetv1.JoinModeQuorum, Quorum: &two})
	fc.set("a", 0, fleetv1.ActionPhaseSucceeded)
	fc.set("b", 0, fleetv1.ActionPhaseSucceeded)
	fc.set("c", 0, fleetv1.ActionPhaseInProgress)

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress ('c' has not settled)", eval.Outcome)
	}
	mustContainRef(t, eval.Cancel, "c", 0)

	fc.set("c", 0, fleetv1.ActionPhaseCancelled)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("outcome = %v, want Succeeded", eval.Outcome)
	}
}

// ── loop ─────────────────────────────────────────────────────────────────────

func (f *fakeCluster) advance(anchor string, iteration int32, startedAt *time.Time) {
	f.progress[anchor] = flow.LoopProgress{Iteration: iteration, StartedAt: startedAt}
}

// THE ACCEPTANCE SCENARIO: twelve laps of a route.
func TestLoop_TimesRunsExactlyNIterations(t *testing.T) {
	fc := newFakeCluster()
	times := int32(12)
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeTimes, Times: &times, Body: run("patrol")}}

	// Not started: the loop requests iteration 1 and does nothing else this pass.
	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress || eval.StartIteration["patrol"] != 1 {
		t.Fatalf("first pass: outcome=%v startIteration=%v, want InProgress and StartIteration[patrol]=1", eval.Outcome, eval.StartIteration)
	}
	if len(eval.Ensure) != 0 {
		t.Fatalf("first pass must not create a child before the iteration count is committed, got %v", eval.Ensure)
	}

	start := time.Now()
	fc.advance("patrol", 1, &start)

	for i := int32(1); i <= times; i++ {
		eval = flow.Evaluate(&tree, fc.ctx())
		mustContainRef(t, eval.Ensure, "patrol", i)
		fc.set("patrol", i, fleetv1.ActionPhaseSucceeded)

		eval = flow.Evaluate(&tree, fc.ctx())
		if len(eval.Settled) != 1 || eval.Settled[0].Iteration != i {
			t.Fatalf("iteration %d: expected exactly one settlement for iteration %d, got %v", i, i, eval.Settled)
		}
		if i < times {
			if eval.Outcome != flow.OutcomeInProgress || eval.StartIteration["patrol"] != i+1 {
				t.Fatalf("iteration %d/%d: outcome=%v startIteration=%v, want InProgress and next=%d", i, times, eval.Outcome, eval.StartIteration, i+1)
			}
			fc.advance("patrol", i+1, &start)
		} else {
			if eval.Outcome != flow.OutcomeSucceeded {
				t.Fatalf("iteration %d/%d (last): outcome = %v, want Succeeded", i, times, eval.Outcome)
			}
		}
	}
}

// A Forever loop exits via `until` (result-based) — the "patrol until told to
// stop" acceptance shape.
func TestLoop_ForeverExitsOnUntilResult(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeForever,
		Body: run("watch"),
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{
			{Result: &fleetv1.UntilResultCondition{Member: "watch", Field: "done", Equals: "true"}},
		}},
	}}
	start := time.Now()
	fc.advance("watch", 1, &start)

	fc.set("watch", 1, fleetv1.ActionPhaseSucceeded)
	fc.children[flow.Ref{Member: "watch", Iteration: 1}] = flow.ChildState{
		Exists: true, Phase: fleetv1.ActionPhaseSucceeded,
		Result: &fleetv1.ActionResult{Values: map[string]string{"done": "false"}},
	}
	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress || eval.StartIteration["watch"] != 2 {
		t.Fatalf("done=false: outcome=%v startIteration=%v, want InProgress, next=2", eval.Outcome, eval.StartIteration)
	}

	fc.advance("watch", 2, &start)
	fc.children[flow.Ref{Member: "watch", Iteration: 2}] = flow.ChildState{
		Exists: true, Phase: fleetv1.ActionPhaseSucceeded,
		Result: &fleetv1.ActionResult{Values: map[string]string{"done": "true"}},
	}
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("done=true: outcome = %v, want Succeeded", eval.Outcome)
	}
}

// A Forever loop with no bound at all must be rejected by admission before it
// ever reaches this evaluator — this test documents
// that assumption by giving the evaluator a bound anyway (maxIterations) and
// confirming it, rather than silently spinning, exits there.
func TestLoop_ForeverExitsOnMaxIterations(t *testing.T) {
	fc := newFakeCluster()
	max := int32(3)
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeForever, MaxIterations: &max, Body: run("scan")}}
	start := time.Now()

	for i := int32(1); i <= max; i++ {
		fc.advance("scan", i, &start)
		fc.set("scan", i, fleetv1.ActionPhaseSucceeded)
	}
	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("outcome = %v, want Succeeded (maxIterations reached)", eval.Outcome)
	}
}

// interruptRunning: an in-flight child is confirmed-cancelled (never
// abandoned) the moment `until` matches, rather than waiting for it to settle.
func TestLoop_InterruptRunningCancelsInFlightChildOnUntilMatch(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeForever, InterruptRunning: true, Body: run("wander"),
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{
			{Result: &fleetv1.UntilResultCondition{Member: "exit-watch", Field: "seen", Equals: "true"}},
		}},
	}}
	start := time.Now()
	fc.advance("wander", 1, &start)
	fc.set("wander", 1, fleetv1.ActionPhaseInProgress) // still running mid-iteration

	// A sibling (non-looped) member outside the loop reports the exit sign.
	fc.children[flow.Ref{Member: "exit-watch", Iteration: 0}] = flow.ChildState{
		Exists: true, Phase: fleetv1.ActionPhaseSucceeded,
		Result: &fleetv1.ActionResult{Values: map[string]string{"seen": "true"}},
	}

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress (the interrupted child has not settled yet)", eval.Outcome)
	}
	mustContainRef(t, eval.Cancel, "wander", 1)

	// Once confirmed-cancelled, the loop ends Succeeded — the exit condition
	// was met, which is an intended stop, not a failure.
	fc.set("wander", 1, fleetv1.ActionPhaseCancelled)
	eval = flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("outcome = %v, want Succeeded (interrupt-exit is an intended stop)", eval.Outcome)
	}
}

// A Failed iteration fails the loop outright — it does not retry silently.
func TestLoop_FailedIterationFailsTheLoop(t *testing.T) {
	fc := newFakeCluster()
	times := int32(5)
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeTimes, Times: &times, Body: run("step")}}
	start := time.Now()
	fc.advance("step", 1, &start)
	fc.set("step", 1, fleetv1.ActionPhaseFailed)

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed", eval.Outcome)
	}
	if v, ok := eval.StartIteration["step"]; ok {
		t.Fatalf("a failed iteration must not start another, got StartIteration[step]=%d", v)
	}
}

// attempt (on the FleetAction, retries within one execution) and iteration
// (executions) are independent counters — this evaluator only ever advances
// iteration; retry/attempt bookkeeping belongs entirely to the FleetAction
// controller's existing RetryPolicy path and this package never touches it.
// Documented here because it is a rule this task is explicit and
// non-negotiable about, even though there is nothing to assert against this
// package's API (it has no attempt concept at all — the absence IS the point).

// ── 500-ITERATION MEASUREMENT ───────────────────────────────────
//
// This is the evaluator-level half of the retention measurement: it proves
// the evaluator's OWN in-memory state (the fakeCluster's maps) never becomes
// the bottleneck and that Settled/RecentIterations bookkeeping is correct at
// scale. The FleetTask.status OBJECT SIZE measurement — the actual "Done
// when" the status bound asks for — is a separate, controller-level test
// (internal/controller), because object size is a property of the CRD status
// struct the controller writes, not of this package's pure Eval result.
func TestLoop_FiveHundredIterations_EvaluatorCorrectness(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeForever, MaxIterations: ptrInt32(500), Body: run("patrol")}}
	start := time.Now()

	settledCount := 0
	for i := int32(1); i <= 500; i++ {
		fc.advance("patrol", i, &start)
		eval := flow.Evaluate(&tree, fc.ctx())
		mustContainRef(t, eval.Ensure, "patrol", i)
		fc.set("patrol", i, fleetv1.ActionPhaseSucceeded)
		eval = flow.Evaluate(&tree, fc.ctx())
		settledCount += len(eval.Settled)
		if i < 500 && eval.Outcome != flow.OutcomeInProgress {
			t.Fatalf("iteration %d/500: outcome = %v, want InProgress", i, eval.Outcome)
		}
		if i == 500 && eval.Outcome != flow.OutcomeSucceeded {
			t.Fatalf("iteration 500/500: outcome = %v, want Succeeded", eval.Outcome)
		}
	}
	if settledCount != 500 {
		t.Fatalf("settled %d iterations, want exactly 500 (no double-counting, none skipped)", settledCount)
	}
}

func ptrInt32(v int32) *int32 { return &v }

// ── select ───────────────────────────────────────────────────────────────────

func withResult(fc *fakeCluster, member string, iter int32, phase fleetv1.ActionPhase, values map[string]string) {
	fc.children[flow.Ref{Member: member, Iteration: iter}] = flow.ChildState{
		Exists: true, Phase: phase, Result: &fleetv1.ActionResult{Values: values},
	}
}

// THE ACCEPTANCE SCENARIO: a branch on a classification value.
func TestSelect_EqualsBranchesOnClassificationValue(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "classify", Field: "label"},
		Cases: []fleetv1.SelectCase{
			{Equals: strptr("chair"), Then: run("say-chair")},
			{Equals: strptr("door"), Then: run("say-door")},
		},
		Default: run("say-other"),
	}}

	// Producer not yet settled: select waits, does not guess.
	fc.set("classify", 0, fleetv1.ActionPhaseInProgress)
	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress (producer not settled)", eval.Outcome)
	}

	withResult(fc, "classify", 0, fleetv1.ActionPhaseSucceeded, map[string]string{"label": "chair"})
	eval = flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "say-chair", 0)
	for _, r := range eval.Ensure {
		if r.Member == "say-door" || r.Member == "say-other" {
			t.Fatalf("only the matching case's branch may start, got %v", eval.Ensure)
		}
	}
}

// An unmatched value falls through to Default — select is TOTAL, never a hang.
func TestSelect_UnmatchedValueFallsThroughToDefault(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "classify", Field: "label"},
		Cases:   []fleetv1.SelectCase{{Equals: strptr("chair"), Then: run("say-chair")}},
		Default: run("say-other"),
	}}
	withResult(fc, "classify", 0, fleetv1.ActionPhaseSucceeded, map[string]string{"label": "a model said 'drive to the loading dock'"})

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "say-other", 0)
	for _, r := range eval.Ensure {
		if r.Member == "say-chair" {
			t.Fatalf("an unrecognised value must fall through to default, not match an unrelated case, got %v", eval.Ensure)
		}
	}
}

// `present: false` distinguishes "settled Succeeded but produced nothing"
// from an equals/in comparison on an absent value — and case ORDER decides
// which wins when both could apply, exactly as the select rule specifies.
func TestSelect_PresentFalse_DistinguishesNoValueFromAValue(t *testing.T) {
	fc := newFakeCluster()
	no := false
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "plate-read", Field: "plate"},
		Cases:   []fleetv1.SelectCase{{Present: &no, Then: run("log-unread")}},
		Default: run("noop"),
	}}
	// Succeeded, but the plate field was never populated ("worked, found nothing").
	withResult(fc, "plate-read", 0, fleetv1.ActionPhaseSucceeded, map[string]string{"confidence": "0.6"})

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "log-unread", 0)
}

// `in` matches a set of literal values.
func TestSelect_InMatchesASet(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "classify", Field: "label"},
		Cases:   []fleetv1.SelectCase{{In: []string{"door", "gate"}, Then: run("log-and-continue")}},
		Default: run("noop"),
	}}
	withResult(fc, "classify", 0, fleetv1.ActionPhaseSucceeded, map[string]string{"label": "gate"})

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "log-and-continue", 0)
}

// `range` compares a numeric field (optionally a DIFFERENT field than the
// subject — the select rule's own example compares `confidence` while the
// subject is something else).
func TestSelect_RangeComparesADifferentFieldThanTheSubject(t *testing.T) {
	fc := newFakeCluster()
	lt := 0.5
	tree := fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "classify", Field: "label"},
		Cases:   []fleetv1.SelectCase{{Range: &fleetv1.SelectRange{Field: "confidence", Lt: &lt}, Then: run("re-observe")}},
		Default: run("accept"),
	}}
	withResult(fc, "classify", 0, fleetv1.ActionPhaseSucceeded, map[string]string{"label": "chair", "confidence": "0.31"})

	eval := flow.Evaluate(&tree, fc.ctx())
	mustContainRef(t, eval.Ensure, "re-observe", 0)
}

// onJoin: LetRunToCompletion must NOT request cancellation of a still-running
// sibling once the join is satisfied.
func TestParallel_LetRunToCompletion_DoesNotCancel(t *testing.T) {
	fc := newFakeCluster()
	tree := fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{
		Branches: []fleetv1.FlowNode{run("a"), run("b")},
		Join:     &fleetv1.JoinSpec{Mode: fleetv1.JoinModeAny},
		OnJoin:   fleetv1.OnJoinLetRunToCompletion,
	}}
	fc.set("a", 0, fleetv1.ActionPhaseSucceeded)
	fc.set("b", 0, fleetv1.ActionPhaseInProgress)

	eval := flow.Evaluate(&tree, fc.ctx())
	if eval.Outcome != flow.OutcomeInProgress {
		t.Fatalf("outcome = %v, want InProgress ('b' has not settled)", eval.Outcome)
	}
	if len(eval.Cancel) != 0 {
		t.Fatalf("LetRunToCompletion must not cancel anything, got %v", eval.Cancel)
	}
}

// An event-bound select whose producer SETTLED without ever raising the event
// settles FAILED with a diagnostic, and specifically does NOT take `default`.
//
// This is the sharpest of the three places the same honesty is applied. Taking
// `default` would report "the producer ran and none of your cases matched" — a
// claim about a value — when the truth is that no event of that type was ever
// readable and no value could be read at all. An author would then watch their
// task take the fallback branch forever with nothing anywhere saying why, which
// is a confident answer computed from no evidence.
//
// The producer here has SETTLED. While it is still running the node waits
// instead (TestSelect_EventBindingWaitsWhileTheProducerIsStillRunning) — a
// resident producer that has not yet recognised anything has not failed to.
func TestSelect_EventBindingFailsWhenTheProducerSettledWithoutRaisingIt(t *testing.T) {
	ev := "ObjectClassified"
	root := &fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &ev, From: "classify", Field: "label"},
		Cases:   []fleetv1.SelectCase{{Equals: strptr("chair"), Then: fleetv1.FlowNode{Run: strptr("say-chair")}}},
		Default: fleetv1.FlowNode{Run: strptr("say-other")},
	}}

	got := flow.Evaluate(root, &flow.Context{
		Now: time.Now(),
		Child: func(member string, _ int32) flow.ChildState {
			if member == "classify" {
				// Succeeded, and status.events[] empty: the run ended and this
				// type was never raised.
				return flow.ChildState{Exists: true, Phase: fleetv1.ActionPhaseSucceeded}
			}
			return flow.ChildState{}
		},
	})

	if got.Outcome != flow.OutcomeFailed {
		t.Fatalf("outcome = %s, want Failed — an unreadable subject must not settle as a branch decision", got.Outcome)
	}
	// The default branch must not have been started. `say-other` running would
	// mean the fallback was taken, which is the exact confusion this prevents.
	for _, ref := range got.Ensure {
		if ref.Member == "say-other" || ref.Member == "say-chair" {
			t.Fatalf("a branch (%q) was started from an unreadable subject", ref.Member)
		}
	}
	if got.Diagnostic == "" {
		t.Fatal("a flow-produced failure must carry a diagnostic; the child object has none to offer")
	}
	for _, want := range []string{"ObjectClassified", "classify", "status.events"} {
		if !strings.Contains(got.Diagnostic, want) {
			t.Fatalf("diagnostic %q does not mention %q", got.Diagnostic, want)
		}
	}
}

// The complement: a RESULT-bound select still evaluates normally and carries no
// diagnostic. A diagnostic on an ordinary outcome teaches a reader to ignore it.
func TestSelect_ResultBindingCarriesNoDiagnostic(t *testing.T) {
	root := &fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "classify", Field: "label"},
		Cases:   []fleetv1.SelectCase{{Equals: strptr("chair"), Then: fleetv1.FlowNode{Run: strptr("say-chair")}}},
		Default: fleetv1.FlowNode{Run: strptr("say-other")},
	}}

	got := flow.Evaluate(root, &flow.Context{
		Now: time.Now(),
		Child: func(member string, _ int32) flow.ChildState {
			if member == "classify" {
				return flow.ChildState{
					Exists: true, Phase: fleetv1.ActionPhaseSucceeded,
					Result: &fleetv1.ActionResult{Values: map[string]string{"label": "chair"}},
				}
			}
			return flow.ChildState{}
		},
	})

	if got.Diagnostic != "" {
		t.Fatalf("an ordinary evaluation carried a diagnostic: %q", got.Diagnostic)
	}
	found := false
	for _, ref := range got.Ensure {
		if ref.Member == "say-chair" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the matching case's branch was not started; Ensure = %v", got.Ensure)
	}
}

// An unevaluable `when` case settles the node FAILED, and specifically does NOT
// fall through to the next case.
//
// This hazard is sharper than the event arm's and the test exists to pin the
// difference. An unevaluable subject produces NO answer, which is at least
// visible as a stuck or failed node. An unevaluable `when` that merely "does not
// match" produces a WRONG answer that looks right: the select falls through to
// the next case, the task runs a real branch the author's first rule was written
// to pre-empt, nothing fails and nothing is logged. The run looks correct.
func TestSelect_UnevaluableWhenFailsRatherThanFallingThroughToTheNextCase(t *testing.T) {
	when := "confidence > 0.9"
	root := &fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "classify", Field: "label"},
		Cases: []fleetv1.SelectCase{
			{When: &when, Then: fleetv1.FlowNode{Run: strptr("high-confidence")}},
			{Equals: strptr("chair"), Then: fleetv1.FlowNode{Run: strptr("say-chair")}},
		},
		Default: fleetv1.FlowNode{Run: strptr("say-other")},
	}}

	got := flow.Evaluate(root, &flow.Context{
		Now: time.Now(),
		Child: func(member string, _ int32) flow.ChildState {
			if member == "classify" {
				return flow.ChildState{
					Exists: true, Phase: fleetv1.ActionPhaseSucceeded,
					Result: &fleetv1.ActionResult{Values: map[string]string{"label": "chair"}},
				}
			}
			return flow.ChildState{}
		},
	})

	if got.Outcome != flow.OutcomeFailed {
		t.Fatalf("outcome = %s, want Failed", got.Outcome)
	}
	// The next case MATCHES on the value ("chair"), which is exactly the trap:
	// a fall-through would look like a correct branch decision.
	for _, ref := range got.Ensure {
		if ref.Member == "say-chair" {
			t.Fatal("the select fell through an unevaluable `when` to a case that matched; " +
				"the task would have run a branch the `when` case was written to pre-empt, " +
				"and nothing would have said so")
		}
		if ref.Member == "say-other" || ref.Member == "high-confidence" {
			t.Fatalf("a branch (%q) was taken from an unevaluated case set", ref.Member)
		}
	}
	if !strings.Contains(got.Diagnostic, when) {
		t.Fatalf("diagnostic %q does not quote the clause that could not be evaluated", got.Diagnostic)
	}
}

// A `when` case AFTER an already-matching case is never reached and must not be
// reported: the author's earlier rule decided the branch, and an unevaluated
// clause below it changed nothing. Reporting it would fail a task that is
// running exactly as written.
func TestSelect_AWhenCaseBelowAMatchingCaseIsNeverReached(t *testing.T) {
	when := "confidence > 0.9"
	root := &fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "classify", Field: "label"},
		Cases: []fleetv1.SelectCase{
			{Equals: strptr("chair"), Then: fleetv1.FlowNode{Run: strptr("say-chair")}},
			{When: &when, Then: fleetv1.FlowNode{Run: strptr("high-confidence")}},
		},
		Default: fleetv1.FlowNode{Run: strptr("say-other")},
	}}

	got := flow.Evaluate(root, &flow.Context{
		Now: time.Now(),
		Child: func(member string, _ int32) flow.ChildState {
			if member == "classify" {
				return flow.ChildState{
					Exists: true, Phase: fleetv1.ActionPhaseSucceeded,
					Result: &fleetv1.ActionResult{Values: map[string]string{"label": "chair"}},
				}
			}
			return flow.ChildState{}
		},
	})

	if got.Diagnostic != "" {
		t.Fatalf("a `when` case below a matching case was reported: %q", got.Diagnostic)
	}
	found := false
	for _, ref := range got.Ensure {
		if ref.Member == "say-chair" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the matching case's branch was not taken; Ensure = %v", got.Ensure)
	}
}
