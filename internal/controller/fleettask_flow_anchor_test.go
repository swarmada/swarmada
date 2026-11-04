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

// ─────────────────────────────────────────────────────────────────────────────
// THE THREE DECISIONS THE LOOP BOOKKEEPING RESTS ON.
//
// fleettask_flow.go keys a loop's retention, child pruning and status projection
// to the loop's ANCHOR member, resolves the LoopNode that anchor belongs to, and
// resolves that loop's retention window. Each of the three is a small pure
// function, and each has a stated contract with a consequence attached:
//
//   anchorOf            — "the first `run:` leaf in Body, pre-order" (the file's own
//                         KNOWN SCOPING SIMPLIFICATIONS note, which is also what makes
//                         the note TRUE: pick a different leaf and a compound body's
//                         iteration bookkeeping lands on a member the evaluator is not
//                         counting).
//   findLoopNode        — "the LoopNode whose body's anchor member is anchor". Get the
//                         wrong node and a nested loop is pruned to its PARENT's window.
//   effectiveRetention  — LoopNode.Retention's godoc: "Unset applies the built-in
//                         defaults (keepIterationRecords: 10, keepChildren: 20,
//                         keepFailedChildren: 5)". Every FlowRetention field also
//                         carries Minimum=1 and a schema default, so a zero can only
//                         reach here unset-and-undefaulted — and a zero taken at face
//                         value means "keep nothing", which would delete the failed
//                         children the field exists to preserve.
//
// These are unit tests on purpose: none of the three reads the cluster, and the
// question each answers is decided entirely by the tree it is handed.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

// seqNode / parNode / selNode build the composite nodes anchorOf and findLoopNode
// must walk. runNode is fleettask_flow_retention_test.go's.
func seqNode(children ...fleetv1.FlowNode) fleetv1.FlowNode {
	return fleetv1.FlowNode{Sequence: children}
}

func parNode(branches ...fleetv1.FlowNode) fleetv1.FlowNode {
	return fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{Branches: branches}}
}

func selNode(def fleetv1.FlowNode, cases ...fleetv1.FlowNode) fleetv1.FlowNode {
	sel := &fleetv1.SelectNode{Default: def}
	for _, c := range cases {
		sel.Cases = append(sel.Cases, fleetv1.SelectCase{Then: c})
	}
	return fleetv1.FlowNode{Select: sel}
}

func loopNode(body fleetv1.FlowNode) fleetv1.FlowNode {
	return fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeForever, Body: body}}
}

// The anchor is the first run: leaf in pre-order, whatever composite nodes stand
// between the loop body and it. Each case below names a member that is NOT first
// as well, so "returns some member in the tree" cannot pass.
func TestAnchorOf_IsTheFirstRunLeafInPreOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		node fleetv1.FlowNode
		want string
	}{
		"a bare run leaf is its own anchor": {
			node: runNode("scan"),
			want: "scan",
		},
		"a sequence anchors on its first member, not its last": {
			node: seqNode(runNode("scan"), runNode("pick"), runNode("place")),
			want: "scan",
		},
		"a parallel anchors on its first branch": {
			node: parNode(runNode("scan"), runNode("pick")),
			want: "scan",
		},
		"nesting does not change which leaf is first": {
			node: seqNode(parNode(seqNode(runNode("scan")), runNode("pick")), runNode("place")),
			want: "scan",
		},
		"a loop anchors on its body's anchor": {
			node: loopNode(seqNode(runNode("scan"), runNode("pick"))),
			want: "scan",
		},
		"a select anchors on its first case, ahead of its default": {
			node: selNode(runNode("fallback"), runNode("scan"), runNode("pick")),
			want: "scan",
		},
		"a select with no case falls through to the default": {
			node: selNode(runNode("fallback")),
			want: "fallback",
		},
		"an empty branch is skipped rather than anchoring on nothing": {
			node: seqNode(fleetv1.FlowNode{}, runNode("pick")),
			want: "pick",
		},
		"a tree with no run leaf has no anchor": {
			node: seqNode(fleetv1.FlowNode{}, parNode(fleetv1.FlowNode{})),
			want: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			node := tc.node
			if got := anchorOf(&node); got != tc.want {
				t.Errorf("anchorOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// A nil node is not a tree with no anchor by accident — the walk is recursive and
// hands itself the address of an absent Default, so nil has to be answerable.
func TestAnchorOf_NilNodeHasNoAnchor(t *testing.T) {
	if got := anchorOf(nil); got != "" {
		t.Errorf("anchorOf(nil) = %q, want the empty string", got)
	}
}

// findLoopNode must return the loop the anchor actually belongs to. The tree here
// holds two loops with DIFFERENT retention windows, so returning the wrong one is
// visible as the wrong window rather than as a nil.
func TestFindLoopNode_ResolvesTheLoopTheAnchorBelongsTo(t *testing.T) {
	outer := &fleetv1.LoopNode{
		Mode:      fleetv1.LoopModeForever,
		Body:      seqNode(runNode("patrol"), runNode("report")),
		Retention: &fleetv1.FlowRetention{KeepIterationRecords: 3, KeepChildren: 4, KeepFailedChildren: 2},
	}
	inner := &fleetv1.LoopNode{
		Mode:      fleetv1.LoopModeForever,
		Body:      runNode("inspect"),
		Retention: &fleetv1.FlowRetention{KeepIterationRecords: 7, KeepChildren: 8, KeepFailedChildren: 6},
	}
	tree := seqNode(
		fleetv1.FlowNode{Loop: outer},
		parNode(selNode(fleetv1.FlowNode{}, fleetv1.FlowNode{Loop: inner})),
	)

	for name, tc := range map[string]struct {
		anchor string
		want   *fleetv1.LoopNode
	}{
		"the outer loop's anchor resolves to the outer loop":  {anchor: "patrol", want: outer},
		"the inner loop's anchor resolves to the inner loop":  {anchor: "inspect", want: inner},
		"a non-anchor member of a loop body resolves to none": {anchor: "report", want: nil},
		"a member outside every loop resolves to none":        {anchor: "charge", want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			node := tree
			got := findLoopNode(&node, tc.anchor)
			if got != tc.want {
				t.Fatalf("findLoopNode(%q) resolved to %+v, want %+v", tc.anchor, got, tc.want)
			}
		})
	}
}

func TestFindLoopNode_NilNodeResolvesToNoLoop(t *testing.T) {
	if got := findLoopNode(nil, "patrol"); got != nil {
		t.Errorf("findLoopNode(nil, …) = %+v, want nil", got)
	}
}

// A loop declaring no retention is bounded anyway, at the documented defaults —
// "the bound ships with the loop construct itself, not as an operator afterthought".
// The partial case is the one that matters most: FlowRetention's fields all carry
// Minimum=1, so a zero is never something an author wrote, and treating it as
// "keep nothing" would prune away exactly the failed children KeepFailedChildren
// exists to hold on to.
func TestEffectiveRetention_UnsetAndPartialFieldsFallBackToTheDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		loop *fleetv1.LoopNode
		want fleetv1.FlowRetention
	}{
		"no loop at all": {
			loop: nil,
			want: fleetv1.FlowRetention{KeepIterationRecords: 10, KeepChildren: 20, KeepFailedChildren: 5},
		},
		"a loop declaring no retention": {
			loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeForever},
			want: fleetv1.FlowRetention{KeepIterationRecords: 10, KeepChildren: 20, KeepFailedChildren: 5},
		},
		"only keepChildren set": {
			loop: &fleetv1.LoopNode{Retention: &fleetv1.FlowRetention{KeepChildren: 2}},
			want: fleetv1.FlowRetention{KeepIterationRecords: 10, KeepChildren: 2, KeepFailedChildren: 5},
		},
		"only keepIterationRecords set": {
			loop: &fleetv1.LoopNode{Retention: &fleetv1.FlowRetention{KeepIterationRecords: 1}},
			want: fleetv1.FlowRetention{KeepIterationRecords: 1, KeepChildren: 20, KeepFailedChildren: 5},
		},
		"only keepFailedChildren set": {
			loop: &fleetv1.LoopNode{Retention: &fleetv1.FlowRetention{KeepFailedChildren: 99}},
			want: fleetv1.FlowRetention{KeepIterationRecords: 10, KeepChildren: 20, KeepFailedChildren: 99},
		},
		"every field set is left alone": {
			loop: &fleetv1.LoopNode{Retention: &fleetv1.FlowRetention{
				KeepIterationRecords: 1, KeepChildren: 2, KeepFailedChildren: 3,
			}},
			want: fleetv1.FlowRetention{KeepIterationRecords: 1, KeepChildren: 2, KeepFailedChildren: 3},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := effectiveRetention(tc.loop); got != tc.want {
				t.Errorf("effectiveRetention = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// effectiveRetention must not write through the author's pointer: the LoopNode
// belongs to task.Spec, and back-filling a default onto it would turn a read into
// a spec mutation the controller has no business making.
func TestEffectiveRetention_DoesNotBackfillTheAuthorsSpec(t *testing.T) {
	declared := &fleetv1.FlowRetention{KeepChildren: 2}
	loop := &fleetv1.LoopNode{Retention: declared}

	if got := effectiveRetention(loop); got.KeepFailedChildren != 5 {
		t.Fatalf("keepFailedChildren = %d, want the default 5", got.KeepFailedChildren)
	}
	if declared.KeepFailedChildren != 0 || declared.KeepIterationRecords != 0 {
		t.Errorf("the spec's own retention was rewritten to %+v; it must stay as authored", *declared)
	}
}

// outcomePhase projects the evaluator's verdict onto the phase an operator reads.
// The default arm is the one with teeth: an InProgress node must project NOTHING,
// because writing any ActionPhase there would report a running member as settled.
func TestOutcomePhase_ProjectsOnlySettledOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome flow.Outcome
		want    fleetv1.ActionPhase
	}{
		"succeeded":                            {outcome: flow.OutcomeSucceeded, want: fleetv1.ActionPhaseSucceeded},
		"failed":                               {outcome: flow.OutcomeFailed, want: fleetv1.ActionPhaseFailed},
		"cancelled":                            {outcome: flow.OutcomeCancelled, want: fleetv1.ActionPhaseCancelled},
		"in progress projects no phase at all": {outcome: flow.OutcomeInProgress, want: ""},
		"an outcome this build does not know projects no phase": {outcome: flow.Outcome("Pending"), want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := outcomePhase(tc.outcome); got != tc.want {
				t.Errorf("outcomePhase(%s) = %q, want %q", tc.outcome, got, tc.want)
			}
		})
	}
}
