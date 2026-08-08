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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func squarePolygon() []fleetv1.Point {
	return []fleetv1.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}
}

// Estop-gated resume against a real API server: a paused robot with an active
// estop is HELD past the auto-resume deadline (maintenance stays Active, condition
// set, count reflects the hold); once the estop clears it resumes and completes.
// The gate is on by the field's CRD default (true). This is an operational hold on
// the phase flip, not a safety stop.
func TestEnvtest_ZMResumeGatedByEstop(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	base := time.Now()
	nowVal := base
	r := &ZoneMaintenanceReconciler{Client: envK8s, Scheme: envScheme, now: func() time.Time { return nowVal }}

	robot := envtestValidRobot(ns, "r1", "z1")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed robot status: %v", err)
	}

	zm := &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: "zm", Namespace: ns},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope:                  fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
			Mode:                   fleetv1.ZoneMaintenanceModeGraceful,
			AutoResumeAfterMinutes: 10,
		},
	}
	if err := envK8s.Create(ctx, zm); err != nil {
		t.Fatalf("create ZM: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "zm", Namespace: ns}}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // activate + pause

	robotKey := types.NamespacedName{Name: "r1", Namespace: ns}
	got := &fleetv1.Robot{}
	if err := envK8s.Get(ctx, robotKey, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("robot phase = %s, want Maintenance after activation", got.Status.Phase)
	}

	// Estop lands on the paused robot; advance past the deadline → held.
	got.Status.EstopState = fleetv1.RobotEstopStopped
	if err := envK8s.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	nowVal = base.Add(11 * time.Minute)
	mustReconcile(t, r, req)

	if err := envK8s.Get(ctx, robotKey, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != fleetv1.RobotPhaseMaintenance {
		t.Fatalf("robot phase = %s, want Maintenance (resume held by estop)", got.Status.Phase)
	}
	zmGot := &fleetv1.ZoneMaintenance{}
	if err := envK8s.Get(ctx, req.NamespacedName, zmGot); err != nil {
		t.Fatal(err)
	}
	if zmGot.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("ZM phase = %s, want Active while held", zmGot.Status.Phase)
	}
	if cond := findCond(zmGot.Status.Conditions, zmCondResumeBlockedByEstop); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ResumeBlockedByEstop = %+v, want True (must round-trip through the real CRD)", cond)
	}
	if zmGot.Status.PausedRobotsCount != 1 {
		t.Fatalf("PausedRobotsCount = %d, want 1", zmGot.Status.PausedRobotsCount)
	}

	// Clear the estop → resumes and completes.
	if err := envK8s.Get(ctx, robotKey, got); err != nil {
		t.Fatal(err)
	}
	got.Status.EstopState = fleetv1.RobotEstopNormal
	if err := envK8s.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, req)

	if err := envK8s.Get(ctx, robotKey, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase = %s, want Idle after estop cleared", got.Status.Phase)
	}
	if err := envK8s.Get(ctx, req.NamespacedName, zmGot); err != nil {
		t.Fatal(err)
	}
	if zmGot.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("ZM phase = %s, want Completed", zmGot.Status.Phase)
	}
}

// ZoneReady/CapacityAvailable persist through the real FleetZone CRD (validating
// the conditions listType=map), and CapacityAvailable transitions True→False as
// the zone fills.
func TestEnvtest_ZoneConditionsPersist(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	z := &ZoneController{Client: envK8s, Scheme: envScheme}

	fz := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "leaf", Namespace: ns},
		Spec: fleetv1.FleetZoneSpec{
			MaxConcurrentRobots: 1,
			PhysicalBounds:      &fleetv1.PhysicalBounds{Floor: 0, Polygon: squarePolygon()},
		},
	}
	if err := envK8s.Create(ctx, fz); err != nil {
		t.Fatalf("create zone: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "leaf", Namespace: ns}}
	if _, err := z.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &fleetv1.FleetZone{}
	if err := envK8s.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if c := findCond(got.Status.Conditions, "ZoneReady"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("ZoneReady = %+v, want True", c)
	}
	if c := findCond(got.Status.Conditions, "CapacityAvailable"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("CapacityAvailable = %+v, want True (empty zone)", c)
	}

	// Fill the zone → CapacityAvailable flips False.
	got.Status.CurrentConcurrentRobots = 1
	if err := envK8s.Status().Update(ctx, got); err != nil {
		t.Fatalf("bump occupancy: %v", err)
	}
	if _, err := z.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := envK8s.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if c := findCond(got.Status.Conditions, "CapacityAvailable"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "AtCapacity" {
		t.Fatalf("CapacityAvailable after fill = %+v, want False/AtCapacity", c)
	}
}

// capabilitiesSuspendedAt is stamped into ModelRollout.status.currentBatch through
// the real controller path and persists on the real CRD status subresource.
func TestEnvtest_ModelRolloutStampsSuspension(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &ModelRolloutReconciler{Client: envK8s, Scheme: envScheme}

	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns, Labels: map[string]string{"fleet": "pickers"}},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "Acme", Model: "X1", Zone: "z1",
			Adapter:  fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
			Hardware: []fleetv1.HardwareComponent{{Name: "cam", Type: fleetv1.HardwareTypeCamera}},
		},
	}
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.BatteryPercent = i32(90)
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{{Name: "cam", Status: fleetv1.HardwareHealthy}}
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed robot status: %v", err)
	}

	ro := &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "roll", Namespace: ns},
		Spec: fleetv1.ModelRolloutSpec{
			TargetSelector:    metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			ModelName:         "item-recognition",
			NewVersion:        "3.2.1",
			ModelURI:          "oci://registry/models/item-recognition:3.2.1",
			ModelChecksum:     "sha256:" + strings.Repeat("a", 64),
			Strategy:          fleetv1.RolloutStrategy{RollingUpdate: &fleetv1.RollingUpdateStrategy{MaxUnavailable: "1"}},
			SafetyConstraints: fleetv1.RolloutSafetyConstraints{MinBatteryPct: 30, RequireIdleState: true},
		},
	}
	if err := envK8s.Create(ctx, ro); err != nil {
		t.Fatalf("create rollout: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "roll", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &fleetv1.ModelRollout{}
	if err := envK8s.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	var entry *fleetv1.RolloutBatchRobot
	for i := range got.Status.CurrentBatch {
		if got.Status.CurrentBatch[i].RobotName == "r1" {
			entry = &got.Status.CurrentBatch[i]
		}
	}
	if entry == nil {
		t.Fatalf("r1 not in currentBatch: %+v", got.Status.CurrentBatch)
	}
	if entry.CapabilitiesSuspendedAt == nil {
		t.Fatal("capabilitiesSuspendedAt not stamped/persisted on suspend")
	}
}

// Both convenience counts are populated and persist: an idle robot is paused and a
// busy robot winds down.
func TestEnvtest_ZMCounts(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	nowVal := time.Now()
	r := &ZoneMaintenanceReconciler{Client: envK8s, Scheme: envScheme, now: func() time.Time { return nowVal }}

	idle := envtestValidRobot(ns, "idle-1", "z1")
	busy := envtestValidRobot(ns, "busy-1", "z1")
	for _, rob := range []*fleetv1.Robot{idle, busy} {
		if err := envK8s.Create(ctx, rob); err != nil {
			t.Fatalf("create robot %s: %v", rob.Name, err)
		}
	}
	idle.Status.Phase = fleetv1.RobotPhaseIdle
	if err := envK8s.Status().Update(ctx, idle); err != nil {
		t.Fatal(err)
	}
	busy.Status.Phase = fleetv1.RobotPhaseInProgress
	busy.Status.AssignedAction = "task-1"
	if err := envK8s.Status().Update(ctx, busy); err != nil {
		t.Fatal(err)
	}

	zm := &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: "counts", Namespace: ns},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace},
			Mode:  fleetv1.ZoneMaintenanceModeGraceful,
		},
	}
	if err := envK8s.Create(ctx, zm); err != nil {
		t.Fatalf("create ZM: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "counts", Namespace: ns}}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // activate

	got := &fleetv1.ZoneMaintenance{}
	if err := envK8s.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.PausedRobotsCount != int32(len(got.Status.PausedRobots)) || got.Status.PausedRobotsCount != 1 {
		t.Fatalf("PausedRobotsCount = %d (len %d), want 1", got.Status.PausedRobotsCount, len(got.Status.PausedRobots))
	}
	if got.Status.WindingDownRobotsCount != int32(len(got.Status.WindingDownRobots)) || got.Status.WindingDownRobotsCount != 1 {
		t.Fatalf("WindingDownRobotsCount = %d (len %d), want 1", got.Status.WindingDownRobotsCount, len(got.Status.WindingDownRobots))
	}
}

func mustReconcile(t *testing.T, r *ZoneMaintenanceReconciler, req ctrl.Request) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile %s: %v", req.Name, err)
	}
}
