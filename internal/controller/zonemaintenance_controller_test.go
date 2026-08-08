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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const zmNS = "warehouse-a"

var zmBase = time.Date(2026, 7, 9, 8, 0, 0, 0, time.UTC)

func newZMReconciler(t *testing.T, nowVal *time.Time, objs ...client.Object) (*ZoneMaintenanceReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.ZoneMaintenance{}, &fleetv1.Robot{}).
		Build()
	r := &ZoneMaintenanceReconciler{Client: c, Scheme: scheme, now: func() time.Time { return *nowVal }}
	return r, c
}

func zoneMaint(name string, spec fleetv1.ZoneMaintenanceSpec) *fleetv1.ZoneMaintenance {
	return &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Namespace: zmNS, Name: name},
		Spec:       spec,
	}
}

func zmRobot(name, zone string, phase fleetv1.RobotPhase, assignedAction string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: zmNS, Name: name},
		Spec:       fleetv1.RobotSpec{Zone: zone},
		Status:     fleetv1.RobotStatus{Phase: phase, AssignedAction: assignedAction},
	}
}

func reconcileZM(t *testing.T, r *ZoneMaintenanceReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: zmNS, Name: name}})
	if err != nil {
		t.Fatalf("reconcile %q: %v", name, err)
	}
	return res
}

func getZM(t *testing.T, c client.Client, name string) *fleetv1.ZoneMaintenance {
	t.Helper()
	zm := &fleetv1.ZoneMaintenance{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: name}, zm); err != nil {
		t.Fatalf("get ZoneMaintenance %q: %v", name, err)
	}
	return zm
}

func robotPhase(t *testing.T, c client.Client, name string) fleetv1.RobotPhase {
	t.Helper()
	robot := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: name}, robot); err != nil {
		t.Fatalf("get robot %q: %v", name, err)
	}
	return robot.Status.Phase
}

// driveToActive reconciles twice (add finalizer, then activate).
func driveToActive(t *testing.T, r *ZoneMaintenanceReconciler, name string) {
	t.Helper()
	reconcileZM(t, r, name) // adds finalizer, returns
	reconcileZM(t, r, name) // activates + pauses
}

func TestZM_ActivatesAndPausesIdleRobot(t *testing.T) {
	now := zmBase
	zm := zoneMaint("db-maint", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, Mode: fleetv1.ZoneMaintenanceModeGraceful,
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-idle", "z1", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "db-maint")

	if p := robotPhase(t, c, "r-idle"); p != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("idle robot phase = %s, want Maintenance", p)
	}
	got := getZM(t, c, "db-maint")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Errorf("ZM phase = %s, want Active", got.Status.Phase)
	}
	if got.Status.ActivatedAt == nil {
		t.Error("ActivatedAt not set")
	}
	if len(got.Status.PausedRobots) != 1 || got.Status.PausedRobots[0].Name != "r-idle" {
		t.Errorf("PausedRobots = %+v", got.Status.PausedRobots)
	}
}

func TestZM_InProgressRobotWindsDownNotPaused(t *testing.T) {
	now := zmBase
	zm := zoneMaint("db-maint", fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-busy", "z1", fleetv1.RobotPhaseInProgress, "task-1"))

	driveToActive(t, r, "db-maint")

	if p := robotPhase(t, c, "r-busy"); p != fleetv1.RobotPhaseInProgress {
		t.Fatalf("busy robot phase = %s, want InProgress (never yanked mid-task)", p)
	}
	got := getZM(t, c, "db-maint")
	if len(got.Status.WindingDownRobots) != 1 || got.Status.WindingDownRobots[0].AssignedAction != "task-1" {
		t.Errorf("WindingDownRobots = %+v", got.Status.WindingDownRobots)
	}
	if len(got.Status.PausedRobots) != 0 {
		t.Errorf("busy robot was paused: %+v", got.Status.PausedRobots)
	}
}

func TestZM_ScheduledInFutureDoesNotPause(t *testing.T) {
	now := zmBase
	start := metav1.Time{Time: zmBase.Add(time.Hour)}
	zm := zoneMaint("later", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, ScheduledStart: &start,
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-idle", "z1", fleetv1.RobotPhaseIdle, ""))

	reconcileZM(t, r, "later") // finalizer
	res := reconcileZM(t, r, "later")

	if p := robotPhase(t, c, "r-idle"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot paused before scheduled start: phase = %s", p)
	}
	if getZM(t, c, "later").Status.Phase != fleetv1.ZoneMaintenancePhaseScheduled {
		t.Errorf("ZM phase = %s, want Scheduled", getZM(t, c, "later").Status.Phase)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected a requeue toward the scheduled start, got %v", res.RequeueAfter)
	}
}

func TestZM_AutoResumeRestoresAndCompletes(t *testing.T) {
	now := zmBase
	zm := zoneMaint("timed", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, AutoResumeAfterMinutes: 10,
	})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-idle", "z1", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "timed")
	if robotPhase(t, c, "r-idle") != fleetv1.RobotPhaseMaintenance {
		t.Fatal("robot not paused on activation")
	}

	// Advance past the auto-resume deadline.
	now = zmBase.Add(11 * time.Minute)
	reconcileZM(t, r, "timed")

	if p := robotPhase(t, c, "r-idle"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase after auto-resume = %s, want Idle", p)
	}
	got := getZM(t, c, "timed")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Errorf("ZM phase = %s, want Completed", got.Status.Phase)
	}
	if len(got.Status.PausedRobots) != 0 {
		t.Errorf("PausedRobots not cleared on completion: %+v", got.Status.PausedRobots)
	}
}

func TestZM_DeleteResumesViaFinalizer(t *testing.T) {
	now := zmBase
	zm := zoneMaint("del", fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-idle", "z1", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "del")
	if robotPhase(t, c, "r-idle") != fleetv1.RobotPhaseMaintenance {
		t.Fatal("robot not paused")
	}

	// Delete: the finalizer keeps the object until the controller resumes robots.
	if err := c.Delete(context.Background(), getZM(t, c, "del")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reconcileZM(t, r, "del")

	if p := robotPhase(t, c, "r-idle"); p != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot not resumed on delete: phase = %s", p)
	}
	// Finalizer gone → object actually removed.
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "del"}, &fleetv1.ZoneMaintenance{}); err == nil {
		t.Error("ZoneMaintenance still exists after finalizer resume")
	}
}

func TestZM_ResumeSkipsRobotCoveredByOtherActive(t *testing.T) {
	now := zmBase
	zmA := zoneMaint("a", fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}})
	zmB := zoneMaint("b", fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}})
	r, c := newZMReconciler(t, &now, zmA, zmB, zmRobot("r-idle", "z1", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "a")
	driveToActive(t, r, "b") // both Active, both cover the robot (namespace scope)
	if robotPhase(t, c, "r-idle") != fleetv1.RobotPhaseMaintenance {
		t.Fatal("robot not paused")
	}

	// Delete A: B still covers the robot, so it must stay paused.
	if err := c.Delete(context.Background(), getZM(t, c, "a")); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	reconcileZM(t, r, "a")

	if p := robotPhase(t, c, "r-idle"); p != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("robot un-paused while another maintenance still active: phase = %s", p)
	}
}

func TestZM_ZoneScopeCoversDescendantZone(t *testing.T) {
	now := zmBase
	// child (z-child) → parent (z-parent). Maintenance targets the parent.
	parent := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: zmNS, Name: "z-parent"}}
	child := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: zmNS, Name: "z-child"},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: "z-parent"},
	}
	zm := zoneMaint("parent-maint", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z-parent"},
	})
	r, c := newZMReconciler(t, &now, zm, parent, child,
		zmRobot("r-in-child", "z-child", fleetv1.RobotPhaseIdle, ""),
		zmRobot("r-elsewhere", "z-other", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "parent-maint")

	if p := robotPhase(t, c, "r-in-child"); p != fleetv1.RobotPhaseMaintenance {
		t.Errorf("descendant-zone robot phase = %s, want Maintenance", p)
	}
	if p := robotPhase(t, c, "r-elsewhere"); p != fleetv1.RobotPhaseIdle {
		t.Errorf("out-of-scope robot phase = %s, want Idle (untouched)", p)
	}
}

func TestZM_PausedAtPreservedAcrossReconciles(t *testing.T) {
	now := zmBase
	zm := zoneMaint("db-maint", fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}})
	r, c := newZMReconciler(t, &now, zm, zmRobot("r-idle", "z1", fleetv1.RobotPhaseIdle, ""))

	driveToActive(t, r, "db-maint")
	firstPausedAt := getZM(t, c, "db-maint").Status.PausedRobots[0].PausedAt

	// A later reconcile at a different time must not reset the recorded pausedAt.
	now = zmBase.Add(5 * time.Minute)
	reconcileZM(t, r, "db-maint")
	if got := getZM(t, c, "db-maint").Status.PausedRobots[0].PausedAt; !got.Equal(&firstPausedAt) {
		t.Errorf("pausedAt changed on re-reconcile: %v → %v", firstPausedAt, got)
	}
}
