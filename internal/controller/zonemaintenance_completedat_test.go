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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// status.completedAt is the window's record of when robots came back into service. It is
// stamped on the transition into Completed and never rewritten (RA-1) — a later reconcile
// moving it would silently shorten the outage it documents.

func TestZM_CompletedAtStampedOnTheCompletionEdge(t *testing.T) {
	now := zmBase
	zm := zoneMaint("timed", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, AutoResumeAfterMinutes: 10,
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-idle", "z1", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "timed")
	// Empty while Active: a window that has not closed has no close time, and a
	// pre-stamped value would read as an outage that already ended.
	if got := getZM(t, c, "timed"); got.Status.CompletedAt != nil {
		t.Fatalf("completedAt set while Active: %v", got.Status.CompletedAt)
	}

	closedAt := zmBase.Add(11 * time.Minute)
	now = closedAt
	reconcileZM(t, r, "timed")

	got := getZM(t, c, "timed")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("precondition: phase = %s, want Completed", got.Status.Phase)
	}
	if got.Status.CompletedAt == nil {
		t.Fatal("completedAt not stamped on the Completed transition")
	}
	// The controller's clock at the transition, not wall time — the value has to be
	// comparable against activatedAt to yield the real window duration.
	if !got.Status.CompletedAt.Time.Equal(closedAt) {
		t.Errorf("completedAt = %v, want the transition instant %v", got.Status.CompletedAt.Time, closedAt)
	}
	if got.Status.ActivatedAt == nil || !got.Status.CompletedAt.After(got.Status.ActivatedAt.Time) {
		t.Errorf("completedAt %v must follow activatedAt %v", got.Status.CompletedAt, got.Status.ActivatedAt)
	}
}

func TestZM_CompletedAtIsNotRewrittenByLaterReconciles(t *testing.T) {
	// RA-1, and the property the field is actually useful for. A Completed window stays
	// readable for its audit-retention period and keeps being reconciled; if each pass
	// re-stamped the close time, the recorded outage would shrink toward zero.
	//
	// The mechanism under test is the terminal guard at the top of Reconcile — a Completed
	// window returns before the stamping branch. Nothing else stops a later pass from
	// rewriting the field, so the sequence below has to be one that WOULD rewrite it if the
	// guard went: re-entry re-activates the window and sets a fresh auto-resume deadline, so
	// a single later reconcile is not enough — it must be followed by one past that new
	// deadline, which is what drives a second completion.
	now := zmBase
	zm := zoneMaint("timed", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, AutoResumeAfterMinutes: 10,
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-idle", "z1", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "timed")
	closedAt := zmBase.Add(11 * time.Minute)
	now = closedAt
	reconcileZM(t, r, "timed")

	first := getZM(t, c, "timed").Status.CompletedAt
	if first == nil {
		t.Fatal("precondition: completedAt not stamped")
	}

	// Time moves on and the object is reconciled repeatedly. The second pass sits beyond the
	// deadline a re-activation would have set (activation + autoResumeAfterMinutes), so an
	// unguarded controller reaches the completion branch again and re-stamps.
	now = zmBase.Add(3 * time.Hour)
	reconcileZM(t, r, "timed")
	now = zmBase.Add(3*time.Hour + 15*time.Minute)
	reconcileZM(t, r, "timed")

	got := getZM(t, c, "timed")
	if got.Status.CompletedAt == nil || !got.Status.CompletedAt.Time.Equal(first.Time) {
		t.Fatalf("completedAt moved: %v, want the original %v", got.Status.CompletedAt, first)
	}
}

func TestZM_CompletedAtStaysUnsetWhileResumeIsHeldByEstop(t *testing.T) {
	// A window held open by a robot whose estop is not Clear has NOT completed — robots
	// are still out of service. Stamping at the point the deadline passes, rather than at
	// the phase flip, would record the outage as over while it was still running.
	now := zmBase
	zm := zoneMaint("gated", fleetv1.ZoneMaintenanceSpec{
		Scope:                         fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
		AutoResumeAfterMinutes:        10,
		RequireEstopClearBeforeResume: zmBoolPtr(true),
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-held", "z1", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "gated")
	// An estop lands on the paused robot during the window.
	setEstop(t, c, "r-held", fleetv1.RobotEstopStopped)

	now = zmBase.Add(11 * time.Minute)
	reconcileZM(t, r, "gated")

	got := getZM(t, c, "gated")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("precondition: phase = %s, want the window held Active", got.Status.Phase)
	}
	if got.Status.CompletedAt != nil {
		t.Errorf("completedAt stamped on a window that is still holding robots: %v", got.Status.CompletedAt)
	}
}
