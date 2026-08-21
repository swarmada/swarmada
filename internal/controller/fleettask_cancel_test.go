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
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// An operator cancellation is an intent that was honoured, not a fault.
//
// Before this: the desiredState fan-out cancelled the children, the children
// reached ActionPhaseCancelled, the completion policy was therefore unmet, and
// enactFailure reported the task as Failed. FleetTaskPhaseCancelled was a
// declared enum value that no code path ever wrote — an enum member with no
// inbound transition, the same shape as the Preempted contradiction in the
// FleetAction phase table.
//
// RFC-0001 crds/fleettask.md now states the rule this asserts: a
// desiredState: Cancelled write is the ONLY path into status.phase: Cancelled.
func TestFleetTask_OperatorCancelReportsCancelledNotFailed(t *testing.T) {
	ft := fleetTask("ft-cancel", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateCancelled,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-cancel", "a", fleetv1.ActionPhaseCancelled),
		childAction("ft-cancel", "b", fleetv1.ActionPhaseCancelled),
	)

	reconcileFT(t, r, "ft-cancel")

	if got := getFT(t, c, "ft-cancel").Status.Phase; got != fleetv1.FleetTaskPhaseCancelled {
		t.Fatalf("phase = %q, want Cancelled — an operator cancellation reported as a fault is "+
			"indistinguishable from work that went wrong", got)
	}
}

// A task whose members failed on their own is still Failed. The Cancelled path
// must not swallow real failures just because someone later wrote Cancelled, so
// the discriminator has to be the desiredState, not the children's phases.
func TestFleetTask_GenuineFailureStillReportsFailed(t *testing.T) {
	ft := fleetTask("ft-fail", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-fail", "a", fleetv1.ActionPhaseFailed),
	)

	reconcileFT(t, r, "ft-fail")

	if got := getFT(t, c, "ft-fail").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got)
	}
}

// Compensate is deliberately NOT short-circuited by a cancellation. Cancelling a
// partly-completed saga must still roll back the members that already succeeded —
// that is the entire purpose of the policy, and a cancel that skipped it would
// leave the world in the half-done state Compensate exists to prevent.
func TestFleetTask_CancelUnderCompensateStillCompensates(t *testing.T) {
	ft := fleetTask("ft-comp", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyCompensate,
		fleetv1.DesiredStateCancelled,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-comp", "a", fleetv1.ActionPhaseSucceeded),
		childAction("ft-comp", "b", fleetv1.ActionPhaseCancelled),
	)

	reconcileFT(t, r, "ft-comp")

	got := getFT(t, c, "ft-comp").Status.Phase
	if got == fleetv1.FleetTaskPhaseCancelled {
		t.Fatalf("phase = Cancelled: a cancelled saga skipped compensation, leaving action %q "+
			"succeeded and un-rolled-back", "a")
	}
	if got != fleetv1.FleetTaskPhaseCompensating && got != fleetv1.FleetTaskPhaseCompensated {
		t.Fatalf("phase = %q, want Compensating or Compensated", got)
	}
}

// The fan-out is what makes `swarmctl cancel task` cancel the actions the task is
// running: one write to spec.desiredState, and the controller — which owns its
// children — propagates it. The CLI must never write the children itself, so this
// asserts the controller really does, and that terminal members are left alone.
func TestFleetTask_CancelFansOutToNonTerminalMembersOnly(t *testing.T) {
	ft := fleetTask("ft-fan", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateCancelled,
		ftAction("running", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("done", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-fan", "running", fleetv1.ActionPhaseInProgress),
		childAction("ft-fan", "done", fleetv1.ActionPhaseSucceeded),
	)

	reconcileFT(t, r, "ft-fan")

	running := getActionOrNil(t, c, childName("ft-fan", "running"))
	if running == nil {
		t.Fatal("in-flight member action disappeared")
	}
	if running.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Errorf("in-flight member desiredState = %q, want Cancelled — cancelling a task must "+
			"cancel the action it is running", running.Spec.DesiredState)
	}

	done := getActionOrNil(t, c, childName("ft-fan", "done"))
	if done == nil {
		t.Fatal("terminal member action disappeared")
	}
	if done.Spec.DesiredState == fleetv1.DesiredStateCancelled {
		t.Errorf("terminal member was written: a Succeeded action must not be re-driven by a cancel")
	}
}

// ── The join D3 closes (Round-4) ─────────────────────────────────────────────
//
// The three tests above pre-set their children to ActionPhaseCancelled, so they assert the
// aggregation half in isolation and take it on faith that a child ever ARRIVES there. Before
// D3 it never did: the fan-out wrote spec.desiredState onto the child and no code read that
// field, so a composite could not stop its own children and this whole path was unreachable
// in production while its unit tests passed.
//
// This test runs both controllers over one client to close that gap end to end:
// fan-out -> child enactment -> a terminal phase the composite's aggregation accepts.
func TestFleetTask_CancelFanOutActuallyStopsChildAndAggregates(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	ft := fleetTask("ft-join", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStateCancelled,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
	)
	// A live, NON-terminal child: the case the fan-out has to act on. Unbound, so the
	// confirmed-stop gate is satisfied without an adapter and the cancel finalizes.
	child := childAction("ft-join", "a", fleetv1.ActionPhasePending)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ft, child).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}, &fleetv1.Robot{}).Build()
	ftr := &FleetTaskReconciler{Client: c, Scheme: scheme}
	far := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler()}

	// 1. Composite fans its cancel intent out onto the child's spec.
	reconcileFT(t, ftr, "ft-join")
	kid := getActionOrNil(t, c, childName("ft-join", "a"))
	if kid == nil {
		t.Fatal("child action missing after fan-out")
	}
	if kid.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Fatalf("fan-out did not write the cancel intent: desiredState = %q", kid.Spec.DesiredState)
	}

	// 2. The FleetAction controller ENACTS it. This is the step D3 added; without it the
	//    child stays Pending forever and the composite hangs non-terminal.
	if _, err := far.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: actionNS, Name: childName("ft-join", "a")},
	}); err != nil {
		t.Fatalf("reconcile child: %v", err)
	}
	kid = getActionOrNil(t, c, childName("ft-join", "a"))
	if kid.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("child phase = %q, want Cancelled — the fan-out's intent was never enacted",
			kid.Status.Phase)
	}
	if !isTerminal(kid.Status.Phase) {
		t.Fatalf("child phase %q is not terminal, so aggregation will never settle", kid.Status.Phase)
	}

	// 3. The composite's aggregation accepts that phase and reports the operator's intent.
	reconcileFT(t, ftr, "ft-join")
	if got := getFT(t, c, "ft-join").Status.Phase; got != fleetv1.FleetTaskPhaseCancelled {
		t.Fatalf("task phase = %q, want Cancelled", got)
	}
}

// ── A composite whose children are held (ADR-0045) ───────────────────────────
//
// The fan-out puts spec.desiredState onto every non-terminal child, so a task set to
// Paused pauses its members. ActionPhasePaused is NOT terminal, which is the correct
// classification — held work is neither done nor abandoned — but it means the composite
// cannot settle while any child is held. These pin what it reports instead, and that the
// wait is a wait rather than a wedge: nothing drives it to Failed, and nothing rewrites it
// on every reconcile.
func TestFleetTask_PausedChildrenHoldTheTaskRunningWithoutWedgingIt(t *testing.T) {
	ft := fleetTask("ft-hold", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStatePaused,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-hold", "a", fleetv1.ActionPhasePaused),
		childAction("ft-hold", "b", fleetv1.ActionPhaseSucceeded),
	)

	reconcileFT(t, r, "ft-hold")
	got := getFT(t, c, "ft-hold")

	// NOT Failed: a held child is still a possible success, so the completion policy is
	// not yet unmeetable. Reporting Failed here would turn an operator's pause into a
	// fault, and FailFast would then cancel the siblings.
	if got.Status.Phase == fleetv1.FleetTaskPhaseFailed {
		t.Fatal("a held child drove the composite to Failed — a pause is not a failure")
	}
	// NOT Cancelled: the Cancelled short-circuit keys on desiredState Cancelled, not on a
	// non-terminal child, so a paused task must not be reported as a cancelled one.
	if got.Status.Phase == fleetv1.FleetTaskPhaseCancelled {
		t.Fatal("a paused composite was reported Cancelled")
	}
	if got.Status.Phase != fleetv1.FleetTaskPhaseRunning {
		t.Errorf("phase = %q, want Running — the composite is still in flight, blocked on "+
			"an operator", got.Status.Phase)
	}
	// The summary still counts honestly, so an operator can see WHICH member is holding.
	if got.Status.ActionSummary != "1/2 Succeeded" {
		t.Errorf("actionSummary = %q, want \"1/2 Succeeded\"", got.Status.ActionSummary)
	}

	// Stability: the verdict does not drift on re-reconcile, and the fan-out has nothing
	// left to patch (the child's desiredState already matches the task's).
	//
	// NOT asserted here: that the re-reconcile performs no status WRITE. It does — but so
	// does every non-terminal composite in this controller, including one whose child is
	// merely InProgress, because patchStatus is unconditional on the non-terminal path.
	// That is a pre-existing write-amplification issue, not one this feature introduces;
	// what the hold changes is how LONG it lasts, since a held composite stays
	// non-terminal until a human acts. Asserting RA-1 here would be asserting someone
	// else's bug in this feature's test.
	reconcileFT(t, r, "ft-hold")
	again := getFT(t, c, "ft-hold")
	if again.Status.Phase != got.Status.Phase || again.Status.ActionSummary != got.Status.ActionSummary {
		t.Errorf("held composite drifted on re-reconcile: %s/%q -> %s/%q",
			got.Status.Phase, got.Status.ActionSummary, again.Status.Phase, again.Status.ActionSummary)
	}
	kid := getActionOrNil(t, c, childName("ft-hold", "a"))
	if kid.Spec.DesiredState != fleetv1.DesiredStatePaused {
		t.Errorf("child desiredState = %q, want Paused (fan-out)", kid.Spec.DesiredState)
	}
}

// The composite settles the moment the operator resolves the held child — the wait has an
// exit, which is what makes it a hold rather than a deadlock.
func TestFleetTask_ResolvingTheHeldChildLetsTheTaskSettle(t *testing.T) {
	ft := fleetTask("ft-settle", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStatePaused,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-settle", "a", fleetv1.ActionPhasePaused),
	)

	reconcileFT(t, r, "ft-settle")
	if got := getFT(t, c, "ft-settle").Status.Phase; got != fleetv1.FleetTaskPhaseRunning {
		t.Fatalf("phase = %q, want Running while held", got)
	}

	// The operator resumes the held child through the verb-gated intake and it completes.
	kid := getActionOrNil(t, c, childName("ft-settle", "a"))
	kid.Status.Phase = fleetv1.ActionPhaseSucceeded
	if err := c.Status().Update(t.Context(), kid); err != nil {
		t.Fatal(err)
	}
	reconcileFT(t, r, "ft-settle")

	if got := getFT(t, c, "ft-settle").Status.Phase; got != fleetv1.FleetTaskPhaseSucceeded {
		t.Errorf("phase = %q after the held child completed, want Succeeded — the "+
			"composite must settle once no child is held", got)
	}
}

// The other way a held child resolves, and the answer is deliberately NOT Cancelled.
//
// FleetTaskPhaseCancelled is reserved for a task the operator cancelled AS A TASK
// (spec.desiredState: Cancelled). Cancelling one held member of a task that is merely
// Paused leaves the completion policy unmeetable, and that is a failure to complete —
// reporting it as an intended cancellation would claim an intent nobody expressed. An
// operator who means to cancel the whole composite writes Cancelled at the task level,
// which is level-triggered in both directions because cancellation is terminal.
func TestFleetTask_CancellingOneHeldChildFailsTheTaskRatherThanCancellingIt(t *testing.T) {
	ft := fleetTask("ft-partial", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast,
		fleetv1.DesiredStatePaused,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, ft,
		childAction("ft-partial", "a", fleetv1.ActionPhaseCancelled),
	)

	reconcileFT(t, r, "ft-partial")

	got := getFT(t, c, "ft-partial").Status.Phase
	if got == fleetv1.FleetTaskPhaseCancelled {
		t.Fatal("cancelling one member of a PAUSED task reported the task as Cancelled; " +
			"that verdict is reserved for spec.desiredState: Cancelled at the task level")
	}
	if got != fleetv1.FleetTaskPhaseFailed {
		t.Errorf("phase = %q, want Failed — the completion policy can no longer be met", got)
	}
}
