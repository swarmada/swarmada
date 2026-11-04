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

// This file is the ENTIRE integration of spec.flow into the
// FleetTask controller — everything else in fleettask_controller.go is
// exactly the v0.3 dependsOn/completionPolicy path, untouched. reconcileFlow
// is reached only when task.Spec.Flow != nil (admission/CEL already
// guarantees dependsOn is then empty on every member).
//
// KNOWN SCOPING SIMPLIFICATIONS (documented rather than silently accepted):
// - Retention (RecentIterations trim, child pruning) and status projection
// for a loop are keyed by the loop's ANCHOR member (the first `run:`
// leaf in Body, pre-order — internal/flow.anchorMember's exported
// counterpart here). A compound loop body touching several members
// tracks the anchor's iteration bookkeeping fully; a non-anchor member's
// projected status.actions entry reflects only its non-looped (iteration
// 0) child, if any. Every design "Done when" scenario this task must
// satisfy uses a single-member loop body, where this is not a
// simplification at all.
// - `select.cases[].when` (a CEL clause) remains schema-complete but
// evaluator-inert: no CEL runtime is wired. It is documented in
// internal/flow as REPORTED, not never-matching — such a node fails
// naming the clause rather than falling through to the next case.
// - `until.anyOf[].event` and `select.subject{event, from, field}` read the
// producing member's status.events[], carried into flow.ChildState by
// childStateOf below. At v0.4 nothing fills that list: ComputeEventIngestor
// (compute.v1 StreamStatus) is not started by the manager, so each sees no
// event — a select on an event settles Failed once its producer settles, and
// an event-only loop.until never fires. See FleetActionStatus.Events' godoc,
// which also states the BOUNDED WINDOW a consumer gets once the list is filled.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

// memberLabel records, on a flow-generated child FleetAction, which member
// it is — the label internal/controller lists by to prune old iterations.
const memberLabel = "swarmada.io/fleettask-member"

// defaultRetention is applied when a LoopNode sets no Retention
// (RFC-0001 #fleettask-the-flownode-tree): the bound ships with the loop
// construct, not as an operator afterthought.
var defaultRetention = fleetv1.FlowRetention{KeepIterationRecords: 10, KeepChildren: 20, KeepFailedChildren: 5}

func iterationChildName(taskName, member string, iteration int32) string {
	if iteration == 0 {
		return childName(taskName, member) // byte-identical to the v0.3 name — a non-looped member's child is unaffected by flow existing at all
	}
	return fmt.Sprintf("%s-i%d", childName(taskName, member), iteration)
}

// reconcileFlow is spec.flow's entire integration into this reconciler.
func (r *FleetTaskReconciler) reconcileFlow(ctx context.Context, req ctrl.Request, task *fleetv1.FleetTask) (ctrl.Result, error) {
	original := task.DeepCopy()

	names := flow.MemberNames(task.Spec.Actions)
	if msg, ok := flow.ValidateStructure(task.Spec.Flow, names); !ok {
		setCond(task, "FlowValid", metav1.ConditionFalse, "InvalidFlow", msg)
		task.Status.Phase = fleetv1.FleetTaskPhaseFailed
		finishIfTerminal(task)
		return ctrl.Result{}, r.patchStatus(ctx, task, original)
	}
	setCond(task, "FlowValid", metav1.ConditionTrue, "Valid", "flow tree is structurally valid")

	// Entries are addressed by INDEX, never by a cached pointer. append
	// reallocates task.Status.Actions the moment it outgrows its capacity, and
	// a pointer handed out before that append points into the old array — so
	// every write through it is silently dropped. The index stays
	// valid across any number of appends, because nothing here ever removes or
	// reorders an entry.
	indexByMember := map[string]int{}
	for i := range task.Status.Actions {
		indexByMember[task.Status.Actions[i].Name] = i
	}
	ensureStatusIdx := func(member string) int {
		if i, ok := indexByMember[member]; ok {
			return i
		}
		task.Status.Actions = append(task.Status.Actions, fleetv1.FleetTaskActionStatus{Name: member, DependenciesMet: true})
		i := len(task.Status.Actions) - 1
		indexByMember[member] = i
		return i
	}
	// ensureStatus resolves the entry FRESH on every call. Callers must not
	// hold the result across an ensureStatus/append for another member.
	ensureStatus := func(member string) *fleetv1.FleetTaskActionStatus {
		return &task.Status.Actions[ensureStatusIdx(member)]
	}

	type ref struct {
		member    string
		iteration int32
	}
	childCache, conflicts := map[ref]*fleetv1.FleetAction{}, newFlowNameConflicts(task)
	var getErr error
	// childStateOf is one spelling of "what the evaluator sees", so the cached
	// and freshly-read paths cannot drift — they did not, but they were two
	// literals, and status.events[] is the second field to be added to both.
	childStateOf := func(c *fleetv1.FleetAction) flow.ChildState {
		return flow.ChildState{
			Exists:        true,
			Phase:         c.Status.Phase,
			Result:        c.Status.Result,
			Events:        c.Status.Events,
			EventsDropped: c.Status.EventsDropped,
		}
	}
	childFn := func(member string, iteration int32) flow.ChildState {
		key := ref{member, iteration}
		if c, cached := childCache[key]; cached {
			if c == nil {
				return flow.ChildState{}
			}
			return childStateOf(c)
		}
		var child fleetv1.FleetAction
		err := conflicts.owned(r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: iterationChildName(task.Name, member, iteration)}, &child), &child, member, iteration)
		switch {
		case err == nil:
			childCache[key] = &child
			return childStateOf(&child)
		case errors.IsNotFound(err):
			childCache[key] = nil
			return flow.ChildState{}
		default:
			if getErr == nil {
				getErr = err
			}
			return flow.ChildState{}
		}
	}
	loopProgressFn := func(anchor string) flow.LoopProgress {
		i, ok := indexByMember[anchor]
		if !ok {
			return flow.LoopProgress{}
		}
		st := &task.Status.Actions[i]
		var startedAt *time.Time
		if st.LoopStartedAt != nil {
			t := st.LoopStartedAt.Time
			startedAt = &t
		}
		return flow.LoopProgress{Iteration: st.Iteration, StartedAt: startedAt}
	}

	eval := flow.Evaluate(task.Spec.Flow, &flow.Context{Now: time.Now(), Child: childFn, LoopProgress: loopProgressFn})
	if getErr != nil {
		return ctrl.Result{}, fmt.Errorf("loading flow children: %w", getErr)
	}

	// ── Ensure: create children the evaluator asked for ────────────────────
	// A refused create stops further creates on this pass, and the cancels below
	// still run: a refusal of one child must not keep a sibling the evaluator
	// cancelled running. The refusal is returned after them.
	var refused func() error
	for _, e := range eval.Ensure {
		if childCache[ref{e.Member, e.Iteration}] != nil || conflicts.held(e.Member, e.Iteration) {
			continue
		}
		memberSpec := actionByName(task.Spec.Actions, e.Member)
		if memberSpec == nil {
			continue // guarded by flow.ValidateStructure; defensive only
		}
		name := iterationChildName(task.Name, e.Member, e.Iteration)
		child := &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: task.Namespace,
				Labels:    map[string]string{ownedByLabel: task.Name, memberLabel: e.Member},
			},
			// DEEP copy, not the shallow struct assignment this was. The spec
			// carries maps (executor.parameters) and slices, and materialising a
			// resolved fromMember binding below writes into that map — through a
			// shallow copy that would be a write into task.Spec.Actions[i], i.e.
			// the AUTHOR'S spec, mutated in memory and then patched back by the
			// next status update.
			Spec: *memberSpec.Action.DeepCopy(),
		}
		child.Spec.DesiredState = task.Spec.DesiredState

		// fromMember bindings are resolved HERE, against sibling state, and
		// pinned into the child at creation — see fleettask_params.go for why
		// this is the task controller's job and why it happens exactly once.
		resolved := resolveMemberBindings(child.Spec.Executor, e.Iteration, childFn)
		materialiseMemberBindings(child, resolved)

		if err := controllerutil.SetControllerReference(task, child, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting owner ref on %q: %w", name, err)
		}
		if err := r.Create(ctx, child); err != nil {
			if isAdmissionRefusal(err) {
				member, createErr := e.Member, err
				refused = func() error { return r.recordChildRefused(ctx, original, member, child, createErr) }
				break
			}
			if !errors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("creating child %q: %w", name, err)
			}
			continue
		}
		childCache[ref{e.Member, e.Iteration}] = child
		// The provenance record is a SEPARATE status write because status is a
		// subresource: Create never carries it. It is guarded on the list being
		// empty, so a failure here is retried on the next pass rather than lost,
		// and a child that already has one is never patched again.
		if err := r.recordParameterResolution(ctx, child, resolved); err != nil {
			return ctrl.Result{}, err
		}
	}

	if refused == nil && conflicts.none() {
		markChildrenAdmitted(task)
	}

	// ── Cancel: confirmed-cancel via the SAME desiredState fan-out the v0.3
	// path already uses — never a delete, never an abandon (the
	// FleetAction controller's own confirmed-cancel machinery, CancelAction /
	// CancelDisposition, is what actually stops the robot). ──
	for _, c := range eval.Cancel {
		child := childCache[ref{c.Member, c.Iteration}]
		if child == nil || isTerminal(child.Status.Phase) || child.Spec.DesiredState == fleetv1.DesiredStateCancelled {
			continue
		}
		co := child.DeepCopy()
		child.Spec.DesiredState = fleetv1.DesiredStateCancelled
		if err := r.Patch(ctx, child, client.MergeFrom(co)); err != nil {
			return ctrl.Result{}, fmt.Errorf("cancelling %q: %w", child.Name, err)
		}
	}
	// ── The task's own desiredState reaches every child that exists, as on the dependsOn
	// path: a task-level Paused, Returning or Cancelled written after a child was created is
	// enacted on that child by the FleetAction controller through the same confirmed paths
	// (RFC-0001 #fleettask-declarative-desired-state). A child the evaluator cancelled stays
	// cancelled: cancellation is not lifted by the task's own value; see takesWrite. ──
	if want := task.Spec.DesiredState; want != "" {
		for _, child := range childCache {
			if child == nil || !takesWrite(child) || child.Spec.DesiredState == want ||
				child.Spec.DesiredState == fleetv1.DesiredStateCancelled {
				continue
			}
			co := child.DeepCopy()
			child.Spec.DesiredState = want
			if err := r.Patch(ctx, child, client.MergeFrom(co)); err != nil {
				return ctrl.Result{}, fmt.Errorf("propagating desiredState to %q: %w", child.Name, err)
			}
		}
	}
	if refused != nil || !conflicts.none() {
		return ctrl.Result{}, conflicts.report(ctx, r, original, refused)
	}

	// ── StartIteration: persist the advanced counter BEFORE any child for
	// it is created — the next reconcile creates the child once this status
	// write has landed (see internal/flow's evalLoop comment). ──
	for anchor, next := range eval.StartIteration {
		st := ensureStatus(anchor)
		st.Iteration = next
		if st.LoopStartedAt == nil {
			now := metav1.Now()
			st.LoopStartedAt = &now
		}
	}

	// ── Settled: iteration bookkeeping + retention — the bound
	// ships with the loop construct, in the SAME reconcile that advances it. ──
	for _, s := range eval.Settled {
		st := ensureStatus(s.AnchorMember)
		st.IterationsCompleted++
		if s.Outcome == flow.OutcomeFailed {
			st.IterationsFailed++
		}
		rec := fleetv1.IterationRecord{Iteration: s.Iteration, ActionRef: iterationChildName(task.Name, s.AnchorMember, s.Iteration), Phase: outcomePhase(s.Outcome)}
		if c := childCache[ref{s.AnchorMember, s.Iteration}]; c != nil {
			rec.CompletedAt = c.Status.CompletionTime
		}
		st.RecentIterations = append(st.RecentIterations, rec)

		ret := effectiveRetention(findLoopNode(task.Spec.Flow, s.AnchorMember))
		if int32(len(st.RecentIterations)) > ret.KeepIterationRecords {
			st.RecentIterations = append([]fleetv1.IterationRecord{}, st.RecentIterations[int32(len(st.RecentIterations))-ret.KeepIterationRecords:]...)
		}
		if err := r.pruneIterationChildren(ctx, task, s.AnchorMember, ret); err != nil {
			return ctrl.Result{}, fmt.Errorf("pruning iteration children for %q: %w", s.AnchorMember, err)
		}
	}

	// ── Project status.actions for every member (best-effort at iteration 0
	// for a non-anchor member of a compound loop body — see file comment). ──
	for i := range task.Spec.Actions {
		m := task.Spec.Actions[i].Name
		// idx first: ensureStatusIdx may append here, and the child lookup
		// below must read the iteration off the entry that append produced.
		idx := ensureStatusIdx(m)
		iter := task.Status.Actions[idx].Iteration
		if iter < 0 {
			iter = 0
		}
		if c := childCache[ref{m, iter}]; c != nil {
			st := &task.Status.Actions[idx]
			st.ActionRef = c.Name
			st.Phase = c.Status.Phase
			st.AssignedRobot = c.Status.AssignedRobot
		}
	}

	switch eval.Outcome {
	case flow.OutcomeSucceeded:
		task.Status.Phase = fleetv1.FleetTaskPhaseSucceeded
	case flow.OutcomeFailed:
		task.Status.Phase = fleetv1.FleetTaskPhaseFailed
		// A flow-produced failure gets its reason onto the object. A child's
		// failure explains itself on the child; a node that failed because the
		// evaluator could not evaluate it has nowhere else to be read, and
		// "Failed" with no reason on a task the author wrote correctly against
		// the design is the worst outcome of the three.
		// Carried on a CONDITION, not a new status field: FleetTaskStatus has no
		// message, and adding one to say what a condition already says is a
		// second place for the same fact to be read and to go stale.
		// `FlowEvaluable` is deliberately distinct from `FlowValid` above — the
		// tree IS valid, which is why it was admitted; what it is not is
		// runnable by this release.
		if eval.Diagnostic != "" {
			setCond(task, "FlowEvaluable", metav1.ConditionFalse, "NotEvaluableAtThisVersion", eval.Diagnostic)
		}
	case flow.OutcomeCancelled:
		task.Status.Phase = fleetv1.FleetTaskPhaseCancelled
	default:
		task.Status.Phase = fleetv1.FleetTaskPhaseRunning
	}
	if task.Status.Phase == fleetv1.FleetTaskPhaseRunning && task.Status.StartedAt == nil {
		now := metav1.Now()
		task.Status.StartedAt = &now
	}
	finishIfTerminal(task)

	res := ctrl.Result{}
	if !flow.Outcome(task.Status.Phase).Settled() && task.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		// Level-triggered via child-status watches in the common case;
		// requeue as a backstop for the "waiting on a wall-clock bound"
		// cases (maxDurationSeconds) a watch event alone would not wake.
		res = ctrl.Result{RequeueAfter: 5 * time.Second}
	}
	return res, r.patchStatus(ctx, task, original)
}

func outcomePhase(o flow.Outcome) fleetv1.ActionPhase {
	switch o {
	case flow.OutcomeSucceeded:
		return fleetv1.ActionPhaseSucceeded
	case flow.OutcomeFailed:
		return fleetv1.ActionPhaseFailed
	case flow.OutcomeCancelled:
		return fleetv1.ActionPhaseCancelled
	default:
		return ""
	}
}

func effectiveRetention(l *fleetv1.LoopNode) fleetv1.FlowRetention {
	if l == nil || l.Retention == nil {
		return defaultRetention
	}
	ret := *l.Retention
	if ret.KeepIterationRecords == 0 {
		ret.KeepIterationRecords = defaultRetention.KeepIterationRecords
	}
	if ret.KeepChildren == 0 {
		ret.KeepChildren = defaultRetention.KeepChildren
	}
	if ret.KeepFailedChildren == 0 {
		ret.KeepFailedChildren = defaultRetention.KeepFailedChildren
	}
	return ret
}

// findLoopNode returns the LoopNode whose body's anchor member is anchor.
func findLoopNode(node *fleetv1.FlowNode, anchor string) *fleetv1.LoopNode {
	if node == nil {
		return nil
	}
	switch {
	case node.Loop != nil:
		if anchorOf(&node.Loop.Body) == anchor {
			return node.Loop
		}
		return findLoopNode(&node.Loop.Body, anchor)
	case len(node.Sequence) > 0:
		for i := range node.Sequence {
			if l := findLoopNode(&node.Sequence[i], anchor); l != nil {
				return l
			}
		}
	case node.Parallel != nil:
		for i := range node.Parallel.Branches {
			if l := findLoopNode(&node.Parallel.Branches[i], anchor); l != nil {
				return l
			}
		}
	case node.Select != nil:
		for i := range node.Select.Cases {
			if l := findLoopNode(&node.Select.Cases[i].Then, anchor); l != nil {
				return l
			}
		}
		return findLoopNode(&node.Select.Default, anchor)
	}
	return nil
}

func anchorOf(node *fleetv1.FlowNode) string {
	if node == nil {
		return ""
	}
	switch {
	case node.Run != nil:
		return *node.Run
	case len(node.Sequence) > 0:
		for i := range node.Sequence {
			if m := anchorOf(&node.Sequence[i]); m != "" {
				return m
			}
		}
	case node.Parallel != nil:
		for i := range node.Parallel.Branches {
			if m := anchorOf(&node.Parallel.Branches[i]); m != "" {
				return m
			}
		}
	case node.Loop != nil:
		return anchorOf(&node.Loop.Body)
	case node.Select != nil:
		for i := range node.Select.Cases {
			if m := anchorOf(&node.Select.Cases[i].Then); m != "" {
				return m
			}
		}
		return anchorOf(&node.Select.Default)
	}
	return ""
}

// pruneIterationChildren deletes completed iteration children for member
// beyond the effective retention window — separately for Failed
// (KeepFailedChildren) and everything else terminal (KeepChildren), so a
// failure is not evicted merely because more recent successes exist.
// Owner-referenced children, so this is an ordinary Delete,
// not a race with garbage collection.
func (r *FleetTaskReconciler) pruneIterationChildren(ctx context.Context, task *fleetv1.FleetTask, member string, ret fleetv1.FlowRetention) error {
	var list fleetv1.FleetActionList
	if err := r.List(ctx, &list, client.InNamespace(task.Namespace), client.MatchingLabels{ownedByLabel: task.Name, memberLabel: member}); err != nil {
		return fmt.Errorf("listing iteration children: %w", err)
	}
	var failed, other []fleetv1.FleetAction
	for _, c := range list.Items {
		if !isTerminal(c.Status.Phase) || !ownedChild(&c, task) {
			continue // never prune a non-terminal child, nor an object the task does not control
		}
		if c.Status.Phase == fleetv1.ActionPhaseFailed {
			failed = append(failed, c)
		} else {
			other = append(other, c)
		}
	}
	prune := func(items []fleetv1.FleetAction, keep int32) error {
		if int32(len(items)) <= keep {
			return nil
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreationTimestamp.After(items[j].CreationTimestamp.Time) }) // newest first
		for _, extra := range items[keep:] {
			victim := extra
			if err := r.Delete(ctx, &victim); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("deleting old iteration child %q: %w", victim.Name, err)
			}
		}
		return nil
	}
	if err := prune(failed, ret.KeepFailedChildren); err != nil {
		return err
	}
	return prune(other, ret.KeepChildren)
}

// memberNameSet, validateFlowStructure moved to internal/flow (MemberNames,
// ValidateStructure) so the FleetTask validating webhook and this reconciler
// share ONE implementation and can never disagree — see internal/flow's
// validate.go for the full rationale.
