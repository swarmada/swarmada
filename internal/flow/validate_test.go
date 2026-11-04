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

// ValidateStructure is the ONLY check that sees a flow node below depth 1:
// every recursion point generates as x-kubernetes-preserve-unknown-fields, so
// the CRD's enums, minima and defaults stop at the first nesting level.
// These tests hold the line that every constraint the schema
// declares at depth 1 is re-enforced here at every depth — and that the tree
// itself is bounded, which nothing bounded before.

import (
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

func members(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// nestChain returns a tree whose depth is exactly depth and whose node count
// is also exactly depth: a chain of single-entry sequences ending in a leaf.
func nestChain(depth int) *fleetv1.FlowNode {
	n := run("a")
	for i := 1; i < depth; i++ {
		n = fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{n}}
	}
	return &n
}

// wideTree returns a tree of exactly nodes nodes: one root sequence holding
// nodes-1 leaves.
func wideTree(nodes int) *fleetv1.FlowNode {
	seq := make([]fleetv1.FlowNode, 0, nodes-1)
	for i := 0; i < nodes-1; i++ {
		seq = append(seq, run("a"))
	}
	return &fleetv1.FlowNode{Sequence: seq}
}

// THE DEPTH-1 CASE. A nested loop that omits `mode` is invisible to the CRD
// enum (its parent is schemaless), the webhook's Forever check keys on the
// literal "Forever" and does not fire, and the evaluator's mode switch falls
// through to the Forever branch — so it repeats indefinitely, which is exactly
// what the rule exists to prevent.
func TestValidateStructure_NestedLoopWithoutModeRejected(t *testing.T) {
	tree := &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{
		run("a"),
		{Loop: &fleetv1.LoopNode{Body: run("a")}}, // mode omitted, at depth 2
	}}

	msg, ok := flow.ValidateStructure(tree, members("a"))
	if ok {
		t.Fatal("a nested loop with no mode must be rejected: the evaluator runs it as Forever with no bound")
	}
	if !strings.Contains(msg, "loop.mode") {
		t.Fatalf("message must name loop.mode, got: %q", msg)
	}
}

// The same at depth 1, where the CRD enum would also catch it — the webhook
// must not rely on the schema having done so, because the reconcile-time call
// site sees an already-stored object.
func TestValidateStructure_RootLoopWithoutModeRejected(t *testing.T) {
	tree := &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Body: run("a")}}
	if _, ok := flow.ValidateStructure(tree, members("a")); ok {
		t.Fatal("a root loop with no mode must be rejected")
	}
}

// A mode the enum does not list is rejected wherever it appears.
func TestValidateStructure_NestedLoopBogusModeRejected(t *testing.T) {
	tree := &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeOnce,
		Body: fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: "forever", Body: run("a")}},
	}}
	if _, ok := flow.ValidateStructure(tree, members("a")); ok {
		t.Fatal(`loop.mode "forever" is not a member of the enum and must be rejected`)
	}
}

// The Forever bound itself, below depth 1 — the message the API server already
// serves must be true at every depth, not only the first.
func TestValidateStructure_NestedUnboundedForeverRejected(t *testing.T) {
	tree := &fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{Branches: []fleetv1.FlowNode{
		{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeForever, Body: run("a")}},
	}}}
	if _, ok := flow.ValidateStructure(tree, members("a")); ok {
		t.Fatal("a nested unbounded Forever loop must be rejected")
	}
}

// join.mode is defaulted to All by the API server at depth 1 only, so an unset
// value stays legal at every depth (the evaluator substitutes All) while an
// illegal spelling does not.
func TestValidateStructure_NestedJoinMode(t *testing.T) {
	unset := &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeOnce, Body: fleetv1.FlowNode{
		Parallel: &fleetv1.ParallelNode{Branches: []fleetv1.FlowNode{run("a")}, Join: &fleetv1.JoinSpec{}},
	}}}
	if _, ok := flow.ValidateStructure(unset, members("a")); !ok {
		t.Fatal("an unset nested join.mode must stay admissible — it is not defaulted below depth 1, and the evaluator reads it as All")
	}

	bogus := &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeOnce, Body: fleetv1.FlowNode{
		Parallel: &fleetv1.ParallelNode{Branches: []fleetv1.FlowNode{run("a")}, Join: &fleetv1.JoinSpec{Mode: "all"}},
	}}}
	if _, ok := flow.ValidateStructure(bogus, members("a")); ok {
		t.Fatal(`join.mode "all" is not a member of the enum and must be rejected`)
	}
}

// The retention window is the status bound. Its CRD maximum binds at
// depth 1 only, so a nested loop could ask to keep a million iteration records
// in status — the unbounded-status failure by another route.
func TestValidateStructure_NestedRetentionOutOfRangeRejected(t *testing.T) {
	tree := &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{{
		Loop: &fleetv1.LoopNode{
			Mode:      fleetv1.LoopModeOnce,
			Body:      run("a"),
			Retention: &fleetv1.FlowRetention{KeepIterationRecords: 1000000},
		},
	}}}
	if _, ok := flow.ValidateStructure(tree, members("a")); ok {
		t.Fatal("a nested retention.keepIterationRecords above the CRD maximum must be rejected")
	}
}

// A minimum the CRD declares is re-enforced below depth 1 too.
func TestValidateStructure_NestedTimesBelowMinimumRejected(t *testing.T) {
	zero := int32(0)
	tree := &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{{
		Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeTimes, Times: &zero, Body: run("a")},
	}}}
	if _, ok := flow.ValidateStructure(tree, members("a")); ok {
		t.Fatal("loop.times: 0 is below the CRD minimum and must be rejected at every depth")
	}
}

// ── ceilings ──────────────────────────────────────────────────────────────

func TestValidateStructure_DepthCeiling(t *testing.T) {
	if _, ok := flow.ValidateStructure(nestChain(flow.MaxDepth), members("a")); !ok {
		t.Fatalf("a tree exactly at the depth ceiling (%d) must be admitted", flow.MaxDepth)
	}
	msg, ok := flow.ValidateStructure(nestChain(flow.MaxDepth+1), members("a"))
	if ok {
		t.Fatalf("a tree one level past the depth ceiling (%d) must be rejected", flow.MaxDepth)
	}
	if !strings.Contains(msg, "depth") {
		t.Fatalf("message must name the depth ceiling, got: %q", msg)
	}
}

func TestValidateStructure_NodeCountCeiling(t *testing.T) {
	if _, ok := flow.ValidateStructure(wideTree(flow.MaxNodes), members("a")); !ok {
		t.Fatalf("a tree exactly at the node ceiling (%d) must be admitted", flow.MaxNodes)
	}
	msg, ok := flow.ValidateStructure(wideTree(flow.MaxNodes+1), members("a"))
	if ok {
		t.Fatalf("a tree one node past the ceiling (%d) must be rejected", flow.MaxNodes)
	}
	if !strings.Contains(msg, "node") {
		t.Fatalf("message must name the node ceiling, got: %q", msg)
	}
}

// The node ceiling counts the whole tree, not one branch: breadth is bounded
// transitively, which is why no separate arity limit exists.
func TestValidateStructure_NodeCountCountsEveryBranch(t *testing.T) {
	half := flow.MaxNodes/2 + 1
	tree := &fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{Branches: []fleetv1.FlowNode{
		*wideTree(half), *wideTree(half),
	}}}
	if _, ok := flow.ValidateStructure(tree, members("a")); ok {
		t.Fatal("two branches each under the ceiling must still trip it together")
	}
}

// The select subject's producer is resolved against spec.actions exactly as a
// `run:` leaf is. Until this check existed it was the ONE reference in the flow
// tree that was not resolved, and the consequence was not a harmless typo: the
// evaluator waits on the named producer, so the select never branches, the
// enclosing node never settles, and the task sits InProgress forever with
// nothing anywhere naming the member that does not exist.
func TestValidateStructure_SelectSubjectNamingAnUnknownMemberRejected(t *testing.T) {
	root := &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{{
		Select: &fleetv1.SelectNode{
			Subject: fleetv1.SelectSubject{Member: "no-such-member", Field: "label"},
			Default: fleetv1.FlowNode{},
		},
	}}}
	msg, ok := flow.ValidateStructure(root, map[string]bool{"classify": true})
	if ok {
		t.Fatal("a select subject naming a member not in spec.actions must be rejected")
	}
	if !strings.Contains(msg, "no-such-member") {
		t.Fatalf("message %q does not name the member that does not exist", msg)
	}
}

// The event arm's producer is named in `from`, and it is resolved too — a check
// that only looked at `member` would leave the event form unvalidated, which is
// the arm an author is most likely to get wrong because it is the newer one.
func TestValidateStructure_SelectEventSubjectFromAnUnknownMemberRejected(t *testing.T) {
	ev := "ObjectClassified"
	root := &fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Event: &ev, From: "ghost", Field: "label"},
		Default: fleetv1.FlowNode{},
	}}
	msg, ok := flow.ValidateStructure(root, map[string]bool{"wander": true})
	if ok {
		t.Fatal("an event-bound select naming an unknown producer must be rejected")
	}
	if !strings.Contains(msg, "ghost") {
		t.Fatalf("message %q does not name the member that does not exist", msg)
	}
}

// A subject with neither arm set names no producer at all. The CRD's CEL rule
// catches this at depth 1; below it the schema sees nothing, which is why the
// same rule is repeated here (the whole reason this file exists).
func TestValidateStructure_SelectSubjectWithNoProducerRejected(t *testing.T) {
	root := &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeOnce,
		Body: fleetv1.FlowNode{Select: &fleetv1.SelectNode{
			Subject: fleetv1.SelectSubject{Field: "label"},
			Default: fleetv1.FlowNode{},
		}},
	}}
	msg, ok := flow.ValidateStructure(root, map[string]bool{"classify": true})
	if ok {
		t.Fatal("a select subject naming no producer must be rejected")
	}
	if !strings.Contains(msg, "member") || !strings.Contains(msg, "event") {
		t.Fatalf("message %q should tell the author both arms and which contract each is for", msg)
	}
}

// The valid case, so the three rejections above cannot be satisfied by a check
// that refuses every select.
func TestValidateStructure_SelectSubjectNamingARealMemberAccepted(t *testing.T) {
	root := &fleetv1.FlowNode{Select: &fleetv1.SelectNode{
		Subject: fleetv1.SelectSubject{Member: "classify", Field: "label"},
		Cases:   []fleetv1.SelectCase{{Equals: strptr("chair"), Then: fleetv1.FlowNode{Run: strptr("say-chair")}}},
		Default: fleetv1.FlowNode{Run: strptr("say-other")},
	}}
	if msg, ok := flow.ValidateStructure(root, map[string]bool{
		"classify": true, "say-chair": true, "say-other": true,
	}); !ok {
		t.Fatalf("a well-formed select was rejected: %s", msg)
	}
}

// An `until` condition names a member too, and it dangled for exactly the reason
// the select subject did: only `run:` leaves were ever resolved. This failure is
// QUIETER and therefore worse to diagnose — an unresolvable condition simply
// never matches, so the loop silently falls back to its maxIterations /
// maxDurationSeconds bound and an author sees a patrol that runs to the wall
// clock instead of to the thing it was told to wait for.
func TestValidateStructure_UntilResultNamingAnUnknownMemberRejected(t *testing.T) {
	root := &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeForever,
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{{
			Result: &fleetv1.UntilResultCondition{Member: "ghost", Field: "done", Equals: "true"},
		}}},
		Body: fleetv1.FlowNode{Run: strptr("patrol")},
	}}
	msg, ok := flow.ValidateStructure(root, map[string]bool{"patrol": true})
	if ok {
		t.Fatal("an until.result naming a member not in spec.actions must be rejected")
	}
	if !strings.Contains(msg, "ghost") {
		t.Fatalf("message %q does not name the member that does not exist", msg)
	}
}

func TestValidateStructure_UntilEventFromAnUnknownMemberRejected(t *testing.T) {
	root := &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode:          fleetv1.LoopModeForever,
		MaxIterations: ptrInt32(10),
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{{
			Event: &fleetv1.UntilEventCondition{Type: "ExitSignSeen", From: "ghost"},
		}}},
		Body: fleetv1.FlowNode{Run: strptr("patrol")},
	}}
	msg, ok := flow.ValidateStructure(root, map[string]bool{"patrol": true})
	if ok {
		t.Fatal("an until.event naming a member not in spec.actions must be rejected")
	}
	if !strings.Contains(msg, "ghost") {
		t.Fatalf("message %q does not name the member that does not exist", msg)
	}
}

// The valid case, so the two rejections cannot be satisfied by a check that
// refuses every until.
func TestValidateStructure_UntilNamingRealMembersAccepted(t *testing.T) {
	root := &fleetv1.FlowNode{Loop: &fleetv1.LoopNode{
		Mode: fleetv1.LoopModeForever,
		Until: &fleetv1.UntilSpec{AnyOf: []fleetv1.UntilCondition{
			{Result: &fleetv1.UntilResultCondition{Member: "patrol", Field: "done", Equals: "true"}},
			{Event: &fleetv1.UntilEventCondition{Type: "ExitSignSeen", From: "watch"}},
		}},
		Body: fleetv1.FlowNode{Run: strptr("patrol")},
	}}
	if msg, ok := flow.ValidateStructure(root, map[string]bool{"patrol": true, "watch": true}); !ok {
		t.Fatalf("a well-formed until was rejected: %s", msg)
	}
}
