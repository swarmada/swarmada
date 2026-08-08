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
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// FleetTask is the composite, control-plane-only meta-controller: it owns child FleetActions via
// ownerReferences, reads their status, and reconciles the graph. It NEVER contacts a robot or the
// fleet adapter — every unit of execution is a child FleetAction, reconciled by the FleetAction
// controller exactly as a standalone action. The composite controller is the ONLY writer of the
// children it owns and of FleetTask.status, and it never patches a child's status (RA-1 + the
// RFC-0001 ownership discipline).

// RBAC — standalone marker group (blank lines both sides): controller-gen drops +kubebuilder:rbac
// markers folded into a type's doc comment (see the FleetAction controller for the same note).

// +kubebuilder:rbac:groups=swarmada.io,resources=fleettasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=swarmada.io,resources=fleettasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleettasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions/status,verbs=get;list;watch

// FleetTaskReconciler reconciles a composite FleetTask by generating and aggregating child
// FleetActions.
type FleetTaskReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Kubernetes Events (compensation started, append admitted, etc.).
	// Nil disables event emission.
	Recorder record.EventRecorder
}

// childName is the deterministic name of the FleetAction generated for a member action. Determinism
// makes generation idempotent under controller restart — a re-reconcile Gets the existing child by
// name and adopts it rather than duplicating.
func childName(taskName, actionName string) string {
	return taskName + "-" + actionName
}

const ownedByLabel = "swarmada.io/fleettask"

// ── phase helpers (over the FleetAction/atomic ActionPhase vocabulary) ─────────────

func isTerminal(p fleetv1.ActionPhase) bool {
	return p == fleetv1.ActionPhaseSucceeded || p == fleetv1.ActionPhaseFailed || p == fleetv1.ActionPhaseCancelled
}

// startRank orders the phases that a startCondition may name (Assigned<InProgress<Succeeded); any
// other phase ranks 0 (does not satisfy a dependency).
func startRank(p fleetv1.ActionPhase) int {
	switch p {
	case fleetv1.ActionPhaseAssigned:
		return 1
	case fleetv1.ActionPhaseInProgress:
		return 2
	case fleetv1.ActionPhaseSucceeded:
		return 3
	default:
		return 0
	}
}

// terminallyUnsatisfiable reports a predecessor phase that can never reach a startCondition
// (Failed/Cancelled) — the dependent becomes permanently ineligible, resolved via failurePolicy.
func terminallyUnsatisfiable(p fleetv1.ActionPhase) bool {
	return p == fleetv1.ActionPhaseFailed || p == fleetv1.ActionPhaseCancelled
}

// Reconcile is the level-triggered loop (RFC-0001 §9.1.5).
func (r *FleetTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var task fleetv1.FleetTask
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching fleettask: %w", err)
	}
	// Terminal tasks are done; children are garbage-collected via ownerReferences on delete.
	if isGroupTerminal(task.Status.Phase) {
		return ctrl.Result{}, nil
	}
	original := task.DeepCopy()

	// 1. Validate the dependency graph (belt-and-suspenders to admission).
	if msg, ok := validateGraph(task.Spec.Actions); !ok {
		setCond(&task, "DependencyGraphValid", metav1.ConditionFalse, "InvalidGraph", msg)
		task.Status.Phase = fleetv1.FleetTaskPhaseFailed
		finishIfTerminal(&task)
		return ctrl.Result{}, r.patchStatus(ctx, &task, original)
	}
	setCond(&task, "DependencyGraphValid", metav1.ConditionTrue, "Valid", "dependency graph is acyclic and complete")

	// 2. Load existing children by deterministic name; build the per-action view.
	children := map[string]*fleetv1.FleetAction{}
	for i := range task.Spec.Actions {
		a := &task.Spec.Actions[i]
		var child fleetv1.FleetAction
		err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: childName(task.Name, a.Name)}, &child)
		if err == nil {
			children[a.Name] = child.DeepCopy()
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("getting child %q: %w", a.Name, err)
		}
	}

	// 3. Compute eligibility and (optionally) generate children.
	eligible := map[string]bool{}
	for i := range task.Spec.Actions {
		a := &task.Spec.Actions[i]
		if _, exists := children[a.Name]; exists {
			continue // already generated
		}
		if a.Trigger == fleetv1.TriggerModeOnEvent {
			continue // dormant until activated (append/patch); event surface is a later RFC
		}
		eligible[a.Name] = dependenciesMet(a, children)
	}

	// Create all currently-eligible actions in one pass. RFC-0001 composition schedules each action
	// independently; coordinated multi-robot start — holding eligible children at Assigned until all
	// are Assigned, then releasing together — is specified in RFC-0007, not here.
	toCreate := make([]string, 0, len(eligible))
	for name, ok := range eligible {
		if ok {
			toCreate = append(toCreate, name)
		}
	}
	sort.Strings(toCreate)
	for _, name := range toCreate {
		a := actionByName(task.Spec.Actions, name)
		child := &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      childName(task.Name, name),
				Namespace: task.Namespace,
				Labels:    map[string]string{ownedByLabel: task.Name},
			},
			Spec: a.Action,
		}
		child.Spec.DesiredState = task.Spec.DesiredState // fan-out
		if err := controllerutil.SetControllerReference(&task, child, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting owner ref on %q: %w", name, err)
		}
		if err := r.Create(ctx, child); err != nil {
			if errors.IsAlreadyExists(err) {
				continue // raced with a prior reconcile; adopt on next pass
			}
			return ctrl.Result{}, fmt.Errorf("creating child %q: %w", name, err)
		}
		children[name] = child
	}

	// 4. desiredState fan-out: the composite is authoritative for owned children. While the task
	// is compensating, its non-terminal primary actions are being rolled back, so their effective
	// desired state is Cancelled regardless of spec.desiredState — otherwise the fan-out would
	// undo the cancellation enactFailure issued when it entered Compensating.
	fanoutState := task.Spec.DesiredState
	if task.Status.Phase == fleetv1.FleetTaskPhaseCompensating {
		fanoutState = fleetv1.DesiredStateCancelled
	}
	for name, child := range children {
		if isTerminal(child.Status.Phase) {
			continue
		}
		if child.Spec.DesiredState != fanoutState {
			co := child.DeepCopy()
			child.Spec.DesiredState = fanoutState
			if err := r.Patch(ctx, child, client.MergeFrom(co)); err != nil {
				return ctrl.Result{}, fmt.Errorf("propagating desiredState to %q: %w", name, err)
			}
		}
	}

	// 5. Rebuild the status projection.
	rebuildActionStatus(&task, children)

	// 6. Aggregate phase + apply failure/completion policy.
	r.aggregate(ctx, &task, children)

	// 6b. Drive the compensation saga while the task is Compensating: generate each Succeeded
	// action's compensation child in reverse dependency order, track it, and resolve to
	// Compensated (all undos done) or Failed (a compensation itself failed → fail closed).
	if task.Status.Phase == fleetv1.FleetTaskPhaseCompensating {
		if err := r.driveCompensation(ctx, &task, children); err != nil {
			return ctrl.Result{}, err
		}
	}
	finishIfTerminal(&task)

	return ctrl.Result{}, r.patchStatus(ctx, &task, original)
}

// dependenciesMet reports whether every DependsOn predecessor has reached the action's
// StartCondition. A predecessor that is terminally unsatisfiable leaves the action ineligible
// (resolved later via failurePolicy), so this returns false for it.
func dependenciesMet(a *fleetv1.FleetTaskAction, children map[string]*fleetv1.FleetAction) bool {
	need := startRank(a.StartCondition)
	if need == 0 {
		need = startRank(fleetv1.ActionPhaseSucceeded) // default Succeeded
	}
	for _, dep := range a.DependsOn {
		pred, ok := children[dep]
		if !ok {
			return false // predecessor not generated yet
		}
		if startRank(pred.Status.Phase) < need {
			return false
		}
	}
	return true
}

// aggregate computes the group phase and enacts the failure policy.
func (r *FleetTaskReconciler) aggregate(ctx context.Context, task *fleetv1.FleetTask, children map[string]*fleetv1.FleetAction) {
	// Once the saga has started, driveCompensation owns the task phase; do not recompute it back
	// toward Running/Pending here (RFC-0001 §9.1.5).
	if task.Status.Phase == fleetv1.FleetTaskPhaseCompensating || task.Status.Phase == fleetv1.FleetTaskPhaseCompensated {
		return
	}
	total := len(task.Spec.Actions)
	succeeded, failedOrBlocked, nonTerminal := 0, 0, 0
	for i := range task.Spec.Actions {
		a := &task.Spec.Actions[i]
		child, exists := children[a.Name]
		switch {
		case exists && child.Status.Phase == fleetv1.ActionPhaseSucceeded:
			succeeded++
		case exists && (child.Status.Phase == fleetv1.ActionPhaseFailed || child.Status.Phase == fleetv1.ActionPhaseCancelled):
			failedOrBlocked++
		case dependencyPermanentlyFailed(a, children):
			failedOrBlocked++ // permanently ineligible
		default:
			nonTerminal++
		}
	}

	task.Status.ActionSummary = fmt.Sprintf("%d/%d Succeeded", succeeded, total)

	completionMet := func() bool {
		switch task.Spec.CompletionPolicy {
		case fleetv1.CompletionPolicyAny:
			return succeeded >= 1
		case fleetv1.CompletionPolicyQuorum:
			q := int32(0)
			if task.Spec.Quorum != nil {
				q = *task.Spec.Quorum
			}
			return int32(succeeded) >= q
		default: // All
			return succeeded == total
		}
	}
	// Can the policy STILL be met given how many are already lost?
	remainingPossible := succeeded + nonTerminal
	stillPossible := func() bool {
		switch task.Spec.CompletionPolicy {
		case fleetv1.CompletionPolicyAny:
			return remainingPossible >= 1
		case fleetv1.CompletionPolicyQuorum:
			q := int32(0)
			if task.Spec.Quorum != nil {
				q = *task.Spec.Quorum
			}
			return int32(remainingPossible) >= q
		default:
			return remainingPossible == total
		}
	}

	if completionMet() {
		task.Status.Phase = fleetv1.FleetTaskPhaseSucceeded
		return
	}
	if nonTerminal == 0 {
		// everything terminal, policy unmet -> failure path
		r.enactFailure(ctx, task, children)
		return
	}
	if !stillPossible() {
		// policy can no longer be met even though some are still running
		switch task.Spec.FailurePolicy {
		case fleetv1.FailurePolicyContinueOthers:
			task.Status.Phase = fleetv1.FleetTaskPhaseRunning // let independents finish; fail when all terminal
		default:
			r.enactFailure(ctx, task, children)
			return
		}
	}
	if succeeded == 0 && nonTerminal == 0 {
		task.Status.Phase = fleetv1.FleetTaskPhasePending
		return
	}
	task.Status.Phase = fleetv1.FleetTaskPhaseRunning
}

// enactFailure applies FailFast / ContinueOthers / Compensate once the policy is unmeetable.
func (r *FleetTaskReconciler) enactFailure(ctx context.Context, task *fleetv1.FleetTask, children map[string]*fleetv1.FleetAction) {
	switch task.Spec.FailurePolicy {
	case fleetv1.FailurePolicyCompensate:
		// Stop any non-terminal primary work, then enter Compensating. The saga itself —
		// generating each Succeeded action's compensation child in reverse dependency order,
		// tracking it, and the Compensated / fail-closed decision — is driven by
		// driveCompensation after aggregation (RFC-0001 §9.1.5).
		for _, child := range children {
			if !isTerminal(child.Status.Phase) && child.Spec.DesiredState != fleetv1.DesiredStateCancelled {
				co := child.DeepCopy()
				child.Spec.DesiredState = fleetv1.DesiredStateCancelled
				_ = r.Patch(ctx, child, client.MergeFrom(co))
			}
		}
		if task.Status.Phase != fleetv1.FleetTaskPhaseCompensating && r.Recorder != nil {
			r.Recorder.Event(task, "Warning", "Compensating", "completionPolicy unmet; compensating succeeded actions in reverse dependency order")
		}
		task.Status.Phase = fleetv1.FleetTaskPhaseCompensating
	default: // FailFast (and ContinueOthers once nothing is left running)
		for name, child := range children {
			if !isTerminal(child.Status.Phase) && child.Spec.DesiredState != fleetv1.DesiredStateCancelled {
				co := child.DeepCopy()
				child.Spec.DesiredState = fleetv1.DesiredStateCancelled
				_ = r.Patch(ctx, child, client.MergeFrom(co))
				_ = name
			}
		}
		task.Status.Phase = fleetv1.FleetTaskPhaseFailed
	}
}

// compensationComplete reports whether every compensation the saga still owes has reached
// Succeeded. A member owes a compensation only if it both declared one and itself Succeeded; a
// member that declared a compensation but did not succeed has nothing to undo. The per-member
// CompensationPhase is read from the status projection driveCompensation wrote this pass, so a
// still-Pending or InProgress (or Failed) undo keeps the task out of Compensated.
func (r *FleetTaskReconciler) compensationComplete(task *fleetv1.FleetTask) bool {
	for i := range task.Status.Actions {
		switch task.Status.Actions[i].CompensationPhase {
		case compPhasePending, compPhaseInProgress, compPhaseFailed:
			return false
		}
	}
	return true
}

// ── compensation saga (failurePolicy: Compensate) ─────────────────────────────

// CompensationPhase values (mirror the CRD enum None;Pending;InProgress;Succeeded;Failed; a member
// with nothing to undo keeps the empty/omitted value).
const (
	compPhasePending    = "Pending"
	compPhaseInProgress = "InProgress"
	compPhaseSucceeded  = "Succeeded"
	compPhaseFailed     = "Failed"
)

// compensationLabel records, on a compensation child, the member action it undoes.
const compensationLabel = "swarmada.io/compensates"

// compChildName is the deterministic name of the compensation child generated to undo a member
// action. Determinism keeps generation idempotent under controller restart, exactly like childName.
func compChildName(taskName, actionName string) string {
	return taskName + "-" + actionName + "-comp"
}

// needsCompensation reports whether a member both reached Succeeded and declared a compensation —
// the only members the saga must undo.
func needsCompensation(a *fleetv1.FleetTaskAction, children map[string]*fleetv1.FleetAction) bool {
	if a.Compensation == nil {
		return false
	}
	child, ok := children[a.Name]
	return ok && child.Status.Phase == fleetv1.ActionPhaseSucceeded
}

// compensationEligible reports whether member a's compensation may start now. Reverse dependency
// order: a's undo waits until every action that depended on a (its successors) is terminal and, if
// that successor itself owed a compensation, that compensation has Succeeded — so an undo never
// races the work it reverses (RFC-0001 §9.1.5).
func compensationEligible(task *fleetv1.FleetTask, aName string, children map[string]*fleetv1.FleetAction, compPhase map[string]string) bool {
	for i := range task.Spec.Actions {
		b := &task.Spec.Actions[i]
		successor := false
		for _, dep := range b.DependsOn {
			if dep == aName {
				successor = true
				break
			}
		}
		if !successor {
			continue
		}
		bChild, ok := children[b.Name]
		if !ok || !isTerminal(bChild.Status.Phase) {
			return false // successor still running; undoing a now could race it
		}
		if needsCompensation(b, children) && compPhase[b.Name] != compPhaseSucceeded {
			return false // the successor's own undo must finish first
		}
	}
	return true
}

// driveCompensation advances the saga one level-triggered step while the task is Compensating. It
// reconstructs each owed member's CompensationPhase from its compensation child, generates the next
// eligible compensation children (reverse dependency order), projects the phases onto the status,
// and resolves the task to Compensated (all undos done) or Failed (a compensation itself failed —
// the task fails closed and awaits an operator, it never loops or issues further compensations).
func (r *FleetTaskReconciler) driveCompensation(ctx context.Context, task *fleetv1.FleetTask, children map[string]*fleetv1.FleetAction) error {
	compPhase := map[string]string{}

	// Reconstruct phases from any compensation children that already exist.
	for i := range task.Spec.Actions {
		a := &task.Spec.Actions[i]
		if !needsCompensation(a, children) {
			continue
		}
		var cc fleetv1.FleetAction
		err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: compChildName(task.Name, a.Name)}, &cc)
		switch {
		case err == nil:
			switch cc.Status.Phase {
			case fleetv1.ActionPhaseSucceeded:
				compPhase[a.Name] = compPhaseSucceeded
			case fleetv1.ActionPhaseFailed, fleetv1.ActionPhaseCancelled:
				compPhase[a.Name] = compPhaseFailed
			default:
				compPhase[a.Name] = compPhaseInProgress
			}
		case errors.IsNotFound(err):
			compPhase[a.Name] = compPhasePending
		default:
			return fmt.Errorf("getting compensation child for %q: %w", a.Name, err)
		}
	}

	// Fail closed: a failed compensation halts the saga (no further compensations issued).
	failedClosed := false
	for _, p := range compPhase {
		if p == compPhaseFailed {
			failedClosed = true
			break
		}
	}

	// Generate the next eligible compensation children in reverse dependency order.
	if !failedClosed {
		names := make([]string, 0, len(task.Spec.Actions))
		for i := range task.Spec.Actions {
			names = append(names, task.Spec.Actions[i].Name)
		}
		sort.Strings(names)
		for _, name := range names {
			if compPhase[name] != compPhasePending {
				continue // not owed, already running, or already done
			}
			if !compensationEligible(task, name, children, compPhase) {
				continue // wait for successors' undo
			}
			a := actionByName(task.Spec.Actions, name)
			comp := &fleetv1.FleetAction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      compChildName(task.Name, name),
					Namespace: task.Namespace,
					Labels:    map[string]string{ownedByLabel: task.Name, compensationLabel: name},
				},
				Spec: *a.Compensation,
			}
			comp.Spec.DesiredState = fleetv1.DesiredStateRunning // a compensation always runs to completion
			if err := controllerutil.SetControllerReference(task, comp, r.Scheme); err != nil {
				return fmt.Errorf("setting owner ref on compensation %q: %w", name, err)
			}
			if err := r.Create(ctx, comp); err != nil {
				if !errors.IsAlreadyExists(err) {
					return fmt.Errorf("creating compensation child %q: %w", name, err)
				}
			}
			compPhase[name] = compPhaseInProgress
		}
	}

	// Project phases back onto the status.
	for i := range task.Status.Actions {
		if p, ok := compPhase[task.Status.Actions[i].Name]; ok {
			task.Status.Actions[i].CompensationPhase = p
		}
	}

	// Resolve the terminal transition.
	switch {
	case failedClosed:
		setCond(task, "CompensationComplete", metav1.ConditionFalse, "CompensationFailed",
			"a compensation action failed; the task fails closed and requires operator intervention")
		if r.Recorder != nil {
			r.Recorder.Event(task, "Warning", "CompensationFailed", "a compensation failed; task fails closed")
		}
		task.Status.Phase = fleetv1.FleetTaskPhaseFailed
	case r.compensationComplete(task):
		setCond(task, "CompensationComplete", metav1.ConditionTrue, "Compensated",
			"all required compensations succeeded")
		task.Status.Phase = fleetv1.FleetTaskPhaseCompensated
	default:
		task.Status.Phase = fleetv1.FleetTaskPhaseCompensating
	}
	return nil
}

// ── small helpers ────────────────────────────────────────────────────────────

func actionByName(actions []fleetv1.FleetTaskAction, name string) *fleetv1.FleetTaskAction {
	for i := range actions {
		if actions[i].Name == name {
			return &actions[i]
		}
	}
	return nil
}

func dependencyPermanentlyFailed(a *fleetv1.FleetTaskAction, children map[string]*fleetv1.FleetAction) bool {
	for _, dep := range a.DependsOn {
		if pred, ok := children[dep]; ok && terminallyUnsatisfiable(pred.Status.Phase) {
			return true
		}
	}
	return false
}

func rebuildActionStatus(task *fleetv1.FleetTask, children map[string]*fleetv1.FleetAction) {
	out := make([]fleetv1.FleetTaskActionStatus, 0, len(task.Spec.Actions))
	for i := range task.Spec.Actions {
		a := &task.Spec.Actions[i]
		st := fleetv1.FleetTaskActionStatus{Name: a.Name, DependenciesMet: dependenciesMet(a, children)}
		if child, ok := children[a.Name]; ok {
			st.ActionRef = child.Name
			st.Phase = child.Status.Phase
			st.AssignedRobot = child.Status.AssignedRobot
		}
		out = append(out, st)
	}
	task.Status.Actions = out
}

func isGroupTerminal(p fleetv1.FleetTaskPhase) bool {
	return p == fleetv1.FleetTaskPhaseSucceeded || p == fleetv1.FleetTaskPhaseFailed ||
		p == fleetv1.FleetTaskPhaseCompensated || p == fleetv1.FleetTaskPhaseCancelled
}

func finishIfTerminal(task *fleetv1.FleetTask) {
	if isGroupTerminal(task.Status.Phase) && task.Status.CompletionTime == nil {
		now := metav1.Now()
		task.Status.CompletionTime = &now
	}
}

func setCond(task *fleetv1.FleetTask, condType string, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: task.Generation,
	})
}

func (r *FleetTaskReconciler) patchStatus(ctx context.Context, task *fleetv1.FleetTask, original *fleetv1.FleetTask) error {
	task.Status.ObservedGeneration = task.Generation
	if err := r.Status().Patch(ctx, task, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("patching fleettask status: %w", err)
	}
	return nil
}

// validateGraph checks that every dependsOn names an existing action and the graph is acyclic.
func validateGraph(actions []fleetv1.FleetTaskAction) (string, bool) {
	names := map[string]bool{}
	for i := range actions {
		names[actions[i].Name] = true
	}
	adj := map[string][]string{}
	for i := range actions {
		for _, dep := range actions[i].DependsOn {
			if !names[dep] {
				return fmt.Sprintf("action %q dependsOn unknown action %q", actions[i].Name, dep), false
			}
			adj[actions[i].Name] = append(adj[actions[i].Name], dep)
		}
	}
	// DFS cycle detection.
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = gray
		for _, m := range adj[n] {
			if color[m] == gray {
				return true
			}
			if color[m] == white && visit(m) {
				return true
			}
		}
		color[n] = black
		return false
	}
	for i := range actions {
		if color[actions[i].Name] == white && visit(actions[i].Name) {
			return fmt.Sprintf("dependency cycle detected involving %q", actions[i].Name), false
		}
	}
	return "", true
}

// SetupWithManager wires the controller: it owns child FleetActions, so a child status transition
// requeues the parent.
func (r *FleetTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetTask{}).
		Owns(&fleetv1.FleetAction{}).
		Complete(r)
}
