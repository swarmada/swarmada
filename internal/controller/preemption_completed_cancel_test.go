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

// A preemption victim whose robot answers the preemption's cancel_action
// CANCEL_DISPOSITION_COMPLETED finished the work. It ends Succeeded and is not requeued:
// requeueing it runs finished work a second time.
//
// Both preemption paths are covered: the type-changing handover, where the victim is still
// InProgress when the cancel is answered, and the release of a Preempted action
// (confirmPreemptedStop), reached by zone-reservation and by robot preemption. The guards
// pin what does not change: a cancel answered stopped-safely, or one that fails, still
// returns the victim to Pending, the failing one only once its lease is provably dead.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/tde"
)

// typeChangeScenario is a Critical PickUp with no Idle robot and a Normal Navigate victim
// InProgress on r1: a type-changing robot preemption, so the victim's stop is confirmed
// before the victim is marked.
func typeChangeScenario(t *testing.T, cmd *fakeCommander) (*FleetActionReconciler, client.Client) {
	t.Helper()
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	crit := criticalPending("crit")
	crit.Spec.Type = fleetv1.ActionTypePickUp
	victim := bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease)
	victim.Spec.Type = fleetv1.ActionTypeNavigate
	r, c := newActionReconciler(t, crit, victim, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"))
	r.Commander = cmd
	return r, c
}

// ageVictimLease moves the victim's lease an hour past its horizon: the lease is a stored
// instant, so this is the state the victim reaches by waiting.
func ageVictimLease(t *testing.T, c client.Client, name string) {
	t.Helper()
	v := getAction(t, c, name)
	base := v.DeepCopy()
	v.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	if err := c.Status().Patch(context.Background(), v, client.MergeFrom(base)); err != nil {
		t.Fatalf("age lease: %v", err)
	}
}

// requireSucceededNotRequeued asserts the victim ended Succeeded and stays there over
// further reconciles: never Pending, retryCount unchanged, and no further cancel sent.
func requireSucceededNotRequeued(t *testing.T, r *FleetActionReconciler, c client.Client, cmd *fakeCommander) {
	t.Helper()
	v := getAction(t, c, "norm")
	if v.Status.Phase != fleetv1.ActionPhaseSucceeded {
		t.Fatalf("victim phase = %s after its robot answered the cancel COMPLETED; want Succeeded "+
			"(requeueing it runs finished work again)", v.Status.Phase)
	}
	if v.Status.CompletionTime == nil || v.Status.CompletedAt == nil {
		t.Errorf("victim completionTime=%v completedAt=%v; a Succeeded action records its completion",
			v.Status.CompletionTime, v.Status.CompletedAt)
	}
	cancels := cmd.cancelsFor("norm", "r1")
	for i := 0; i < 3; i++ {
		reconcileAction(t, r, "norm")
	}
	v = getAction(t, c, "norm")
	if v.Status.Phase != fleetv1.ActionPhaseSucceeded || v.Status.PendingSince != nil || v.Status.RetryCount != 0 {
		t.Fatalf("victim phase=%s pendingSince=%v retryCount=%d after more reconciles; want Succeeded, never requeued",
			v.Status.Phase, v.Status.PendingSince, v.Status.RetryCount)
	}
	if v.Status.LeaseExpiresAt != nil {
		t.Errorf("victim leaseExpiresAt = %v once retired; want cleared", v.Status.LeaseExpiresAt)
	}
	if got := cmd.cancelsFor("norm", "r1"); got != cancels {
		t.Errorf("cancels of the victim went %d → %d once it was Succeeded; want none sent", cancels, got)
	}
}

// Path 1, the type-changing handover: the victim is InProgress when the cancel is answered.
func TestPreempt_TypeChange_CompletedCancelEndsTheVictimSucceeded(t *testing.T) {
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelCompleted}
	r, c := typeChangeScenario(t, cmd)

	reconcileAction(t, r, "crit")

	if got := getAction(t, c, "crit"); got.Status.Phase != fleetv1.ActionPhaseAssigned || got.Status.AssignedRobot != "r1" {
		t.Fatalf("preemptor phase=%s robot=%q; the robot finished the victim, so the preemptor takes it now",
			got.Status.Phase, got.Status.AssignedRobot)
	}
	if n := cmd.cancelsFor("norm", "r1"); n != 1 || cmd.cancels[0].Cause != command.CancelCauseSuperseded {
		t.Fatalf("cancels = %+v to %v; want one SUPERSEDED cancel of the victim to r1", cmd.cancels, cmd.cancelTo)
	}
	requireSucceededNotRequeued(t, r, c, cmd)
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "crit" {
		t.Fatalf("r1 assignedAction = %q; retiring the victim must not unbind the preemptor", rob.Status.AssignedAction)
	}
}

// Path 2 by zone-reservation preemption: the Preempted victim's robot answers COMPLETED.
func TestPreempted_ZoneReservation_CompletedCancelEndsTheVictimSucceeded(t *testing.T) {
	r, ft, get := zoneVictimScenario(t)
	reconcileAction(t, r, "crit")
	if v := get("norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s, want Preempted by the TDE eviction", v.Status.Phase)
	}
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelCompleted}
	r.Commander = cmd
	releases := ft.releases

	reconcileAction(t, r, "norm")

	if n := cmd.cancelsFor("norm", "r1"); n != 1 {
		t.Fatalf("cancels = %+v to %v; want one cancel of the victim to r1", cmd.cancels, cmd.cancelTo)
	}
	if rob := getRobot(t, r.Client, "r1", actionNS); rob.Status.AssignedAction != "" || rob.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("r1 assignedAction=%q phase=%s; want released to Idle", rob.Status.AssignedAction, rob.Status.Phase)
	}
	requireSucceededNotRequeued(t, r, r.Client, cmd)
	if ft.releases == releases {
		t.Error("the victim's zone slot was not released once it was Succeeded")
	}
}

// Path 2 by robot preemption (same type, handed over in place): the robot records the
// preemptor, and the Preempted victim's cancel is answered COMPLETED.
func TestPreempted_RobotPreemption_CompletedCancelEndsTheVictimSucceeded(t *testing.T) {
	r, c := preemptScenario(t, tde.ReservationResult{Status: tde.Granted})
	reconcileAction(t, r, "crit")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s, want Preempted", v.Status.Phase)
	}
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelCompleted}
	r.Commander = cmd

	reconcileAction(t, r, "norm")

	requireSucceededNotRequeued(t, r, c, cmd)
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "crit" {
		t.Fatalf("r1 assignedAction = %q; the victim's end must not unbind the preemptor", rob.Status.AssignedAction)
	}
}

// Guard, path 1: a cancel answered stopped-safely still displaces the victim and requeues it.
// UNSPECIFIED maps to the same value (command.CancelStoppedSafely).
func TestPreempt_TypeChange_StoppedSafelyCancelStillRequeues(t *testing.T) {
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}
	r, c := typeChangeScenario(t, cmd)

	reconcileAction(t, r, "crit")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s after a stopped-safely answer; want Preempted (A11)", v.Status.Phase)
	}
	reconcileAction(t, r, "norm")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q; a stopped-safely answer requeues it (A12)",
			v.Status.Phase, v.Status.AssignedRobot)
	}
}

// Guard, path 1: a failing cancel holds the handover while the lease lives, and once the
// lease is provably dead the victim is displaced and requeued, never Succeeded.
func TestPreempt_TypeChange_FailingCancelStillRequeuesAtTheHorizon(t *testing.T) {
	cmd := &fakeCommander{cancelErr: command.ErrUnreachable}
	r, c := typeChangeScenario(t, cmd)

	reconcileAction(t, r, "crit")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("victim phase = %s with no answer and a live lease; want InProgress", v.Status.Phase)
	}
	if got := getAction(t, c, "crit"); got.Status.AssignedRobot == "r1" {
		t.Fatal("the type-changing preemptor took r1 with no confirmed stop")
	}

	ageVictimLease(t, c, "norm")
	reconcileAction(t, r, "crit")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s with its lease provably dead; want Preempted", v.Status.Phase)
	}
	reconcileAction(t, r, "norm")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q; a dead lease with no answer requeues it (A12)",
			v.Status.Phase, v.Status.AssignedRobot)
	}
}

// Guard, path 2: a stopped-safely answer releases a Preempted victim to Pending, by either
// preemption.
func TestPreempted_StoppedSafelyCancelStillRequeues(t *testing.T) {
	t.Run("zone-reservation", func(t *testing.T) {
		r, _, get := zoneVictimScenario(t)
		reconcileAction(t, r, "crit")
		r.Commander = &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}
		reconcileAction(t, r, "norm")
		if v := get("norm"); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
			t.Fatalf("victim phase=%s robot=%q; want Pending and unbound", v.Status.Phase, v.Status.AssignedRobot)
		}
	})
	t.Run("robot", func(t *testing.T) {
		r, c := preemptScenario(t, tde.ReservationResult{Status: tde.Granted})
		reconcileAction(t, r, "crit")
		r.Commander = &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}
		reconcileAction(t, r, "norm")
		if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
			t.Fatalf("victim phase=%s robot=%q; want Pending and unbound", v.Status.Phase, v.Status.AssignedRobot)
		}
	})
}

// Guard, path 2: a failing cancel holds a Preempted victim while its lease lives, and
// requeues it once the lease is provably dead.
func TestPreempted_FailingCancelStillRequeuesAtTheHorizon(t *testing.T) {
	r, _, get := zoneVictimScenario(t)
	reconcileAction(t, r, "crit")
	r.Commander = &fakeCommander{cancelErr: command.ErrUnreachable}

	reconcileAction(t, r, "norm")
	if v := get("norm"); v.Status.Phase != fleetv1.ActionPhasePreempted || v.Status.AssignedRobot != "r1" {
		t.Fatalf("victim phase=%s robot=%q with no answer and a live lease; want held Preempted on r1",
			v.Status.Phase, v.Status.AssignedRobot)
	}

	ageVictimLease(t, r.Client, "norm")
	reconcileAction(t, r, "norm")
	if v := get("norm"); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q with its lease an hour dead; want Pending and unbound",
			v.Status.Phase, v.Status.AssignedRobot)
	}
}
