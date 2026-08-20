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
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
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
