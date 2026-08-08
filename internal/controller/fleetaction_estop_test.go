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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// robotEstopped builds a reachable robot under an active estop, still holding T.
func robotEstopped(name string, estop fleetv1.RobotEstopState, assignedAction string) *fleetv1.Robot {
	r := robotInPhase(name, fleetv1.RobotPhaseInProgress, assignedAction)
	r.Status.EstopState = estop
	return r
}

// Core §9.6.2.4: InProgress + estop → Paused, robot KEPT bound, lease retained.
func TestEstop_InProgressPausesAndKeepsBinding(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease),
		robotEstopped("r1", fleetv1.RobotEstopStopped, "t1"),
	)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s, want Paused", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Fatalf("robot released on InProgress estop (%q) — must stay bound (§9.6.2.4)", ft.Status.AssignedRobot)
	}
	if ft.Status.AssignmentGeneration != 4 || ft.Status.LeaseExpiresAt == nil {
		t.Fatalf("lease not retained: gen=%d lease=%v", ft.Status.AssignmentGeneration, ft.Status.LeaseExpiresAt)
	}
}

// Core §9.6.2.4: Assigned + estop → Paused, robot RELEASED (table row Assigned).
func TestEstop_AssignedPausesAndReleasesRobot(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	robot := robotEstopped("r1", fleetv1.RobotEstopStopped, "t1")
	robot.Status.Phase = fleetv1.RobotPhaseAssigned
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseAssigned, 4, lease),
		robot,
	)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s, want Paused", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "" || ft.Status.LeaseExpiresAt != nil {
		t.Fatalf("Assigned+estop must release binding+lease: robot=%q lease=%v", ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt)
	}
	// The robot's action pointer must be cleared so it is free for other work.
	got := getRobot(t, c, "r1", actionNS)
	if got.Status.AssignedAction != "" {
		t.Fatalf("robot still bound to task: %q", got.Status.AssignedAction)
	}
}

// Stopping is also an active estop (not only Stopped).
func TestEstop_StoppingAlsoPauses(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 1, lease),
		robotEstopped("r1", fleetv1.RobotEstopStopping, "t1"),
	)
	reconcileAction(t, r, "t1")
	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatal("Stopping estop must pause the task")
	}
}

// ── The three MANDATORY review cases for estop-pause ───────────────────────────

// Case 1 — Connectivity loss: a Paused (estop) action whose robot then goes Offline
// must STAY Paused and never be reassigned. Operator-gated; single-executor holds.
func TestEstop_ConnectivityLoss_StaysPausedNeverReassigns(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(-1 * time.Hour)} // even a long-dead lease
	offline := robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1")
	offline.Status.EstopState = fleetv1.RobotEstopStopped
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 4, lease),
		offline,
	)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s — a Paused task must NOT be auto-transitioned on connectivity loss", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Fatalf("Paused task reassigned/released on connectivity loss (%q) — operator-gating violated", ft.Status.AssignedRobot)
	}
}

// Case 2 — Control-plane restart/failover: a fresh reconcile of a persisted Paused
// action must NOT auto-resume; generation and binding are preserved across restarts.
func TestEstop_RestartFailover_NoAutoResume(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 9, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"), // estop already cleared (Normal)
	)

	// Several independent reconciles (as after restarts) never resume the action.
	for i := 0; i < 3; i++ {
		reconcileAction(t, r, "t1")
		ft := getAction(t, c, "t1")
		if ft.Status.Phase != fleetv1.ActionPhasePaused {
			t.Fatalf("iter %d: phase = %s — Paused must not auto-resume even after estop clears", i, ft.Status.Phase)
		}
		if ft.Status.AssignmentGeneration != 9 || ft.Status.AssignedRobot != "r1" {
			t.Fatalf("iter %d: binding/generation drifted: robot=%q gen=%d", i, ft.Status.AssignedRobot, ft.Status.AssignmentGeneration)
		}
	}
}

// Case 3 — Delayed/duplicate: duplicate estop reconciles are idempotent, and a
// delayed "estop cleared" (robot → Normal) must NOT auto-resume the Paused action.
func TestEstop_DelayedDuplicate_Idempotent(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	robot := robotEstopped("r1", fleetv1.RobotEstopStopped, "t1")
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, lease),
		robot,
	)

	// First estop reconcile → Paused.
	reconcileAction(t, r, "t1")
	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatal("first estop reconcile should Pause")
	}
	// Duplicate estop reconcile → still Paused, stable.
	reconcileAction(t, r, "t1")
	if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhasePaused || ft.Status.AssignmentGeneration != 5 {
		t.Fatalf("duplicate estop not idempotent: phase=%s gen=%d", ft.Status.Phase, ft.Status.AssignmentGeneration)
	}

	// Delayed estop CLEAR: robot returns to Normal. Must NOT auto-resume.
	cleared := getRobot(t, c, "r1", actionNS)
	ro := cleared.DeepCopy()
	cleared.Status.EstopState = fleetv1.RobotEstopNormal
	if err := c.Status().Patch(context.Background(), cleared, client.MergeFrom(ro)); err != nil {
		t.Fatalf("clear estop: %v", err)
	}
	reconcileAction(t, r, "t1")
	if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s — Paused must remain until an OPERATOR resumes (no auto-resume on estop clear)", ft.Status.Phase)
	}
}
