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

// An operator closes a maintenance window and it reaches Completed WITHOUT the
// deadline.
//
// This is the defect in one test. autoResumeAfterMinutes defaults to 0, whose own
// documentation reads "0 means the operator must manually resume" — and before
// spec.desiredState there was no manual resume: status.autoResumeAt stayed nil, the
// deadline branch was unreachable, and the window could be activated and never
// closed. Robots stayed in Maintenance until someone deleted the object, which is a
// different transition and destroys the record.
func TestZoneMaintenance_OperatorCloseReachesCompletedWithoutTheDeadline(t *testing.T) {
	now := zmBase
	zm := zoneMaint("early", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-a"},
		// The default, and the whole point: no deadline exists to reach Completed by.
		AutoResumeAfterMinutes: 0,
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("amr-1", "z-a", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "early")
	if got := getZM(t, c, "early").Status.Phase; got != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("phase = %s, want Active", got)
	}
	if got := robotPhase(t, c, "amr-1"); got != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("robot phase = %s, want Maintenance (the window should have paused it)", got)
	}
	// No deadline was ever set, so nothing else could ever close this window.
	if at := getZM(t, c, "early").Status.AutoResumeAt; at != nil {
		t.Fatalf("autoResumeAt = %v, want nil for autoResumeAfterMinutes: 0", at)
	}

	// ── the operator closes it ───────────────────────────────────────────────
	setDesiredState(t, c, "early", fleetv1.ZoneMaintenanceDesiredStateCompleted)
	now = zmBase.Add(3 * time.Minute) // still far short of any deadline
	reconcileZM(t, r, "early")

	got := getZM(t, c, "early")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("phase = %s, want Completed after an operator close", got.Status.Phase)
	}
	if got.Status.CompletedAt == nil {
		t.Error("completedAt was not stamped; the window records no closure time")
	}
	if len(got.Status.PausedRobots) != 0 || got.Status.PausedRobotsCount != 0 {
		t.Errorf("pausedRobots not cleared: %v (%d)", got.Status.PausedRobots, got.Status.PausedRobotsCount)
	}
	// The robot is actually back in service — the property that makes this a close
	// and not merely a phase write.
	if p := robotPhase(t, c, "amr-1"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase = %s, want Idle: a close that leaves a robot in Maintenance "+
			"has stranded it, which is worse than no close at all", p)
	}
}

// INTERACTION: the operator closes while a robot is still draining.
//
// A draining robot is InProgress and was never paused — it is in
// status.windingDownRobots, not status.pausedRobots — so an early close does not
// strand it. It finishes its action under no maintenance hold, which is what "the
// window is over" means. The robots that WERE paused are resumed.
func TestZoneMaintenance_OperatorCloseWhileARobotIsStillDraining(t *testing.T) {
	now := zmBase
	zm := zoneMaint("draining", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-a"},
		Mode:  fleetv1.ZoneMaintenanceModeGraceful,
	})
	r, c := newZMReconciler(t, &now, zm,
		zmRobot("idle-1", "z-a", fleetv1.RobotPhaseIdle, ""),
		zmRobot("busy-1", "z-a", fleetv1.RobotPhaseInProgress, "act-1"))

	driveToActive(t, r, "draining")
	active := getZM(t, c, "draining")
	if len(active.Status.WindingDownRobots) != 1 || active.Status.WindingDownRobots[0].Name != "busy-1" {
		t.Fatalf("windingDownRobots = %v, want [busy-1]", active.Status.WindingDownRobots)
	}
	if p := robotPhase(t, c, "busy-1"); p != fleetv1.RobotPhaseInProgress {
		t.Fatalf("draining robot phase = %s, want InProgress (never paused)", p)
	}

	setDesiredState(t, c, "draining", fleetv1.ZoneMaintenanceDesiredStateCompleted)
	now = zmBase.Add(time.Minute)
	reconcileZM(t, r, "draining")

	got := getZM(t, c, "draining")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("phase = %s, want Completed", got.Status.Phase)
	}
	// The paused robot came back.
	if p := robotPhase(t, c, "idle-1"); p != fleetv1.RobotPhaseIdle {
		t.Errorf("paused robot phase = %s, want Idle", p)
	}
	// The draining robot was NOT touched: it keeps its action and its phase. A close
	// that force-requeued it would be cancelling work the operator never asked to
	// cancel.
	if p := robotPhase(t, c, "busy-1"); p != fleetv1.RobotPhaseInProgress {
		t.Errorf("draining robot phase = %s, want InProgress — an early close must not "+
			"interrupt an action that was already running", p)
	}
	if len(got.Status.WindingDownRobots) != 0 {
		t.Errorf("windingDownRobots not cleared on close: %v", got.Status.WindingDownRobots)
	}
}

// INTERACTION: an operator close is held by exactly what holds the deadline.
//
// A robot whose estop is not Clear cannot be administratively resumed
// (requireEstopClearBeforeResume). The deadline path holds the window Active and
// reports it; an operator close MUST do the same, or an early close would strand a
// robot the deadline would not have. It does, because it is the same branch.
func TestZoneMaintenance_OperatorCloseIsHeldByAnUnclearedEstop(t *testing.T) {
	now := zmBase
	requireClear := true
	zm := zoneMaint("held", fleetv1.ZoneMaintenanceSpec{
		Scope:                         fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-a"},
		RequireEstopClearBeforeResume: &requireClear,
	})
	robot := zmRobot("amr-1", "z-a", fleetv1.RobotPhaseIdle, "")
	r, c := newZMReconciler(t, &now, zm, robot)

	driveToActive(t, r, "held")

	// The robot's estop goes non-Clear while it is held in Maintenance.
	live := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: zmNS, Name: "amr-1"}, live); err != nil {
		t.Fatal(err)
	}
	live.Status.EstopState = fleetv1.RobotEstopStopped
	if err := c.Status().Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	setDesiredState(t, c, "held", fleetv1.ZoneMaintenanceDesiredStateCompleted)
	now = zmBase.Add(time.Minute)
	res := reconcileZM(t, r, "held")

	got := getZM(t, c, "held")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("phase = %s, want Active: a close must not complete while a robot it paused "+
			"cannot be resumed", got.Status.Phase)
	}
	if p := robotPhase(t, c, "amr-1"); p != fleetv1.RobotPhaseMaintenance {
		t.Errorf("robot phase = %s, want Maintenance (still held)", p)
	}
	if !conditionIsTrue(got.Status.Conditions, zmCondResumeBlockedByEstop) {
		t.Error("ResumeBlockedByEstop is not True; the operator is not told why their close did not take")
	}
	if res.RequeueAfter <= 0 {
		t.Error("no requeue: the window would never complete once the estop clears")
	}

	// ── and it completes on its own once the estop clears ────────────────────
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: zmNS, Name: "amr-1"}, live); err != nil {
		t.Fatal(err)
	}
	live.Status.EstopState = fleetv1.RobotEstopNormal
	if err := c.Status().Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	now = zmBase.Add(2 * time.Minute)
	reconcileZM(t, r, "held")

	if got := getZM(t, c, "held").Status.Phase; got != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("phase = %s, want Completed once the estop cleared", got)
	}
	if p := robotPhase(t, c, "amr-1"); p != fleetv1.RobotPhaseIdle {
		t.Errorf("robot phase = %s, want Idle", p)
	}
}

// A window the operator closes before it ever activated is NOT activated in order
// to be closed. Pausing robots so they can be resumed in the same reconcile is a
// real disturbance — in Immediate mode it requeues a live action — in service of a
// window that has been called off.
func TestZoneMaintenance_OperatorCloseOnAScheduledWindowPausesNothing(t *testing.T) {
	now := zmBase
	start := metav1.NewTime(zmBase.Add(time.Hour))
	zm := zoneMaint("calledoff", fleetv1.ZoneMaintenanceSpec{
		Scope:          fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-a"},
		Mode:           fleetv1.ZoneMaintenanceModeImmediate,
		ScheduledStart: &start,
		DesiredState:   fleetv1.ZoneMaintenanceDesiredStateCompleted,
	})
	r, c := newZMReconciler(t, &now, zm,
		zmRobot("amr-1", "z-a", fleetv1.RobotPhaseIdle, ""),
		zmRobot("busy-1", "z-a", fleetv1.RobotPhaseInProgress, "act-1"))

	reconcileZM(t, r, "calledoff") // finalizer
	reconcileZM(t, r, "calledoff") // sees the close

	if got := getZM(t, c, "calledoff").Status.Phase; got != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("phase = %s, want Completed", got)
	}
	if p := robotPhase(t, c, "amr-1"); p != fleetv1.RobotPhaseIdle {
		t.Errorf("robot phase = %s, want Idle — a called-off window must not pause anything", p)
	}
	if p := robotPhase(t, c, "busy-1"); p != fleetv1.RobotPhaseInProgress {
		t.Errorf("busy robot phase = %s, want InProgress — a called-off window must not "+
			"requeue a live action", p)
	}
}

// The default keeps every existing window behaving exactly as before: Active, and
// closing only on its deadline.
func TestZoneMaintenance_DefaultDesiredStateDoesNotClose(t *testing.T) {
	now := zmBase
	zm := zoneMaint("normal", fleetv1.ZoneMaintenanceSpec{
		Scope:                  fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-a"},
		AutoResumeAfterMinutes: 30,
	})
	// The Go zero value is "", which is what a stored object decoded without the
	// field carries in a unit test. It must behave as Active, not as a close.
	r, c := newZMReconciler(t, &now, zm, zmRobot("amr-1", "z-a", fleetv1.RobotPhaseIdle, ""))
	driveToActive(t, r, "normal")

	now = zmBase.Add(5 * time.Minute)
	reconcileZM(t, r, "normal")
	if got := getZM(t, c, "normal").Status.Phase; got != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("phase = %s, want Active well before the deadline", got)
	}
	if p := robotPhase(t, c, "amr-1"); p != fleetv1.RobotPhaseMaintenance {
		t.Errorf("robot phase = %s, want Maintenance", p)
	}
}

func setDesiredState(t *testing.T, c client.Client, name string, ds fleetv1.ZoneMaintenanceDesiredState) {
	t.Helper()
	zm := &fleetv1.ZoneMaintenance{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: zmNS, Name: name}, zm); err != nil {
		t.Fatal(err)
	}
	zm.Spec.DesiredState = ds
	if err := c.Update(context.Background(), zm); err != nil {
		t.Fatal(err)
	}
}
