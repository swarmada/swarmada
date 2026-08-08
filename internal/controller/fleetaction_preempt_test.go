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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func criticalPending(name string) *fleetv1.FleetAction {
	return pendingInBand(name, fleetv1.ActionPriorityCritical)
}

func pendingInBand(name string, prio fleetv1.ActionPriority) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Priority: prio},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
}

func bandAction(name, robot string, phase fleetv1.ActionPhase, prio fleetv1.ActionPriority, gen int64, lease *metav1.Time) *fleetv1.FleetAction {
	ft := assignedAction(name, robot, phase, gen, lease)
	ft.Spec.Priority = prio
	return ft
}

// Core §9.1.4.3: a Critical action with no Idle robot preempts a Normal InProgress
// action on an eligible robot — victim → Preempted, Critical → Assigned to it.
func TestPreempt_CriticalDisplacesNormal(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		criticalPending("crit"),
		bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"),
	)

	reconcileAction(t, r, "crit")

	// Victim displaced.
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s, want Preempted", v.Status.Phase)
	}
	// Critical action took the robot with a fresh lease.
	crit := getAction(t, c, "crit")
	if crit.Status.Phase != fleetv1.ActionPhaseAssigned || crit.Status.AssignedRobot != "r1" {
		t.Fatalf("critical not assigned to freed robot: phase=%s robot=%q", crit.Status.Phase, crit.Status.AssignedRobot)
	}
	if crit.Status.AssignmentGeneration != 1 || crit.Status.LeaseExpiresAt == nil {
		t.Fatalf("critical lease not minted: gen=%d lease=%v", crit.Status.AssignmentGeneration, crit.Status.LeaseExpiresAt)
	}
	// Robot now bound to the Critical action.
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "crit" {
		t.Fatalf("robot not switched to critical: assignedAction=%q", rob.Status.AssignedAction)
	}
}

// Victim eviction: once the robot holds the Critical action, the Preempted victim
// requeues to Pending (generation preserved, retryCount is not tracked).
func TestPreempt_VictimRequeuesOnceRobotSwitches(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		// Robot already switched to the critical action; victim left in Preempted.
		bandAction("norm", "r1", fleetv1.ActionPhasePreempted, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "crit"),
	)

	reconcileAction(t, r, "norm")

	v := getAction(t, c, "norm")
	if v.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("victim phase = %s, want Pending once robot switched off it", v.Status.Phase)
	}
	if v.Status.AssignedRobot != "" || v.Status.LeaseExpiresAt != nil {
		t.Fatalf("victim binding not cleared: robot=%q lease=%v", v.Status.AssignedRobot, v.Status.LeaseExpiresAt)
	}
	if v.Status.AssignmentGeneration != 2 {
		t.Fatalf("victim generation reset on preemption: %d, want 2 preserved", v.Status.AssignmentGeneration)
	}
}

// Critical never preempts another Critical (FIFO within the band).
func TestPreempt_NeverPreemptsCritical(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		criticalPending("crit2"),
		bandAction("crit1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityCritical, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "crit1"),
	)

	reconcileAction(t, r, "crit2")

	if getAction(t, c, "crit1").Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatal("a Critical task was preempted by another Critical — FIFO violated")
	}
	if getAction(t, c, "crit2").Status.Phase != fleetv1.ActionPhasePending {
		t.Fatal("preempting Critical should have stayed Pending (no eligible victim)")
	}
}

// A preemptor band never preempts High: High is not a preemptible victim band
// (only Normal/Low are), so a Critical action leaves a High action running.
func TestPreempt_DoesNotPreemptHigh(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		criticalPending("crit"),
		bandAction("high", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityHigh, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "high"),
	)

	reconcileAction(t, r, "crit")

	if getAction(t, c, "high").Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatal("a High task was preempted (High is never a victim band)")
	}
}

// §9.1.4.3 High-band preemption: a High action with no Idle robot preempts a Normal
// InProgress action on an eligible robot — same displacement path as Critical.
func TestPreempt_HighDisplacesNormal(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		pendingInBand("high", fleetv1.ActionPriorityHigh),
		bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"),
	)

	reconcileAction(t, r, "high")

	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s, want Preempted (High preempts Normal)", v.Status.Phase)
	}
	high := getAction(t, c, "high")
	if high.Status.Phase != fleetv1.ActionPhaseAssigned || high.Status.AssignedRobot != "r1" {
		t.Fatalf("High not assigned to freed robot: phase=%s robot=%q", high.Status.Phase, high.Status.AssignedRobot)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "high" {
		t.Fatalf("robot not switched to High task: assignedAction=%q", rob.Status.AssignedAction)
	}
}

// A High action never preempts another High (FIFO within the preemptor bands) nor a
// Critical — only Normal/Low are preemptible victims.
func TestPreempt_HighNeverPreemptsHighOrCritical(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		pendingInBand("high2", fleetv1.ActionPriorityHigh),
		bandAction("high1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityHigh, 2, lease),
		bandAction("crit", "r2", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityCritical, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "high1"),
		robotInPhase("r2", fleetv1.RobotPhaseInProgress, "crit"),
	)

	reconcileAction(t, r, "high2")

	if getAction(t, c, "high1").Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatal("a High task preempted another High — FIFO within the preemptor bands violated")
	}
	if getAction(t, c, "crit").Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatal("a High task preempted a Critical — victim rule violated")
	}
	if getAction(t, c, "high2").Status.Phase != fleetv1.ActionPhasePending {
		t.Fatal("preempting High should have stayed Pending (no eligible victim)")
	}
}

// Lowest-priority victim first: with both a Normal and a Low candidate, Low is
// preempted.
func TestPreempt_PrefersLowestBandVictim(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		criticalPending("crit"),
		bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
		bandAction("low", "r2", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityLow, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"),
		robotInPhase("r2", fleetv1.RobotPhaseInProgress, "low"),
	)

	reconcileAction(t, r, "crit")

	if getAction(t, c, "low").Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatal("lowest-band (Low) victim should have been preempted first")
	}
	if getAction(t, c, "norm").Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatal("Normal task should have been left running when a Low victim existed")
	}
}

// ── The three MANDATORY review cases for preemption ────────────────────────────

// Case 1 — Connectivity loss: a Preempted victim whose robot goes Offline must NOT
// requeue while its (frozen) lease is alive — the robot might still be executing.
func TestPreempt_ConnectivityLoss_HoldsOnFrozenLease(t *testing.T) {
	alive := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		bandAction("norm", "r1", fleetv1.ActionPhasePreempted, fleetv1.ActionPriorityNormal, 2, alive),
		robotInPhase("r1", fleetv1.RobotPhaseOffline, "norm"), // still claims the victim, now offline
	)

	reconcileAction(t, r, "norm")

	v := getAction(t, c, "norm")
	if v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("phase = %s — a Preempted victim must not requeue on connectivity loss before lease death", v.Status.Phase)
	}
	if v.Status.AssignedRobot != "r1" {
		t.Fatalf("victim released before provable lease death (%q) — double-execution hazard", v.Status.AssignedRobot)
	}
}

// Case 2 — Control-plane restart/failover: a cold reconcile of a persisted
// Preempted victim re-evaluates purely; if the robot has switched, it requeues,
// preserving generation. No in-memory state needed.
func TestPreempt_RestartFailover_PureReevaluation(t *testing.T) {
	alive := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		bandAction("norm", "r1", fleetv1.ActionPhasePreempted, fleetv1.ActionPriorityNormal, 6, alive),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "crit"), // robot already switched
	)

	reconcileAction(t, r, "norm")

	v := getAction(t, c, "norm")
	if v.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s — cold reconcile should requeue a victim whose robot switched", v.Status.Phase)
	}
	if v.Status.AssignmentGeneration != 6 {
		t.Fatalf("generation reset across restart: %d, want 6 preserved", v.Status.AssignmentGeneration)
	}
}

// Case 3 — Delayed/duplicate: an already-Assigned Critical action does NOT re-preempt
// on a duplicate reconcile, and duplicate Preempted reconciles are idempotent
// while the robot still claims the victim.
func TestPreempt_DelayedDuplicate_NoDoublePreempt(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		// Critical already Assigned to r1; a Normal action sits InProgress on r2.
		bandAction("crit", "r1", fleetv1.ActionPhaseAssigned, fleetv1.ActionPriorityCritical, 1, lease),
		bandAction("norm", "r2", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "crit"),
		robotInPhase("r2", fleetv1.RobotPhaseInProgress, "norm"),
	)

	// Duplicate reconcile of the already-Assigned Critical must not preempt anything.
	reconcileAction(t, r, "crit")
	if getAction(t, c, "norm").Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatal("an already-Assigned Critical task re-preempted a victim on a duplicate reconcile")
	}

	// Duplicate reconciles of a Preempted victim whose robot still claims it are
	// idempotent (stable Preempted, no premature requeue).
	r2, c2 := newActionReconciler(t,
		bandAction("norm", "r2", fleetv1.ActionPhasePreempted, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r2", fleetv1.RobotPhaseInProgress, "norm"), // robot still claims the victim
	)
	for i := 0; i < 3; i++ {
		reconcileAction(t, r2, "norm")
		if v := getAction(t, c2, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
			t.Fatalf("iter %d: victim requeued while robot still claims it (phase=%s)", i, v.Status.Phase)
		}
	}
}
