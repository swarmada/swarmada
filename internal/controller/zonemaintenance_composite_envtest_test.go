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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A MAINTENANCE WINDOW ON A COMPOSITE ZONE HAS TO DRAIN ITS MEMBERS — ALL OF THEM, OR NONE.
//
// A composite FleetZone groups other zones through spec.memberZones; it holds no robot
// itself (FZ-C2). RFC-0001 #fleetzone-composite-zones-and-zone-expansion lets ZoneMaintenance.spec.scope.zoneName name a
// composite and requires the scope to be EXPANDED, and — because a maintenance window is
// an operational hold — requires that expansion to be all-or-nothing: a window whose
// scope does not fully resolve MUST NOT activate (reason CompositionUnresolved) and
// pauses nobody (ADR-0064).
//
// The first test is the safety case: a technician is told the ward is drained, and a
// robot in one of its rooms is still running.

const (
	zmScopeResolvedCond        = "ScopeResolved"
	zmCompositionUnresolvedWhy = "CompositionUnresolved"
)

// zmCompositeFixture creates zones, then robots (Idle) in the named zones.
func zmCompositeFixture(t *testing.T, ns string, zones []*fleetv1.FleetZone, robots map[string]string) {
	t.Helper()
	ctx := context.Background()
	for _, z := range zones {
		z.Namespace = ns
		if err := envK8s.Create(ctx, z); err != nil {
			t.Fatalf("create zone %s: %v", z.Name, err)
		}
	}
	for name, zone := range robots {
		robot := envtestValidRobot(ns, name, zone)
		if err := envK8s.Create(ctx, robot); err != nil {
			t.Fatalf("create robot %s: %v", name, err)
		}
		robot.Status.Phase = fleetv1.RobotPhaseIdle
		if err := envK8s.Status().Update(ctx, robot); err != nil {
			t.Fatalf("seed robot %s status: %v", name, err)
		}
	}
}

func zmZoneScoped(ns, name, zone string) *fleetv1.ZoneMaintenance {
	return &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: zone},
			Mode:  fleetv1.ZoneMaintenanceModeGraceful,
		},
	}
}

func robotPhaseOf(t *testing.T, ns, name string) fleetv1.RobotPhase {
	t.Helper()
	got := &fleetv1.Robot{}
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got.Status.Phase
}

func TestEnvtest_ZoneMaintenanceOnCompositePausesEveryMemberRobot(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	// ward-2 groups a terminal room and a containment parent (whose child holds a robot);
	// corridor is outside the ward and is the control.
	zmCompositeFixture(t, ns, []*fleetv1.FleetZone{
		{ObjectMeta: metav1.ObjectMeta{Name: "room-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "wing-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "room-b1"}, Spec: fleetv1.FleetZoneSpec{ParentZone: "wing-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "corridor"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ward-2"}, Spec: fleetv1.FleetZoneSpec{MemberZones: []string{"room-a", "wing-b"}}},
	}, map[string]string{"r-a": "room-a", "r-b1": "room-b1", "r-out": "corridor"})

	if err := envK8s.Create(ctx, zmZoneScoped(ns, "drain-ward", "ward-2")); err != nil {
		t.Fatalf("create ZM: %v", err)
	}
	r := &ZoneMaintenanceReconciler{Client: envK8s, Scheme: envScheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "drain-ward"}}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // activate + pause

	zm := &fleetv1.ZoneMaintenance{}
	if err := envK8s.Get(ctx, req.NamespacedName, zm); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"r-a", "r-b1"} {
		if got := robotPhaseOf(t, ns, name); got != fleetv1.RobotPhaseMaintenance {
			t.Errorf("robot %s is in a member of composite ward-2 and the window on ward-2 is %s, "+
				"but the robot's phase is %s, not Maintenance: the window reports the ward drained "+
				"and this robot is still in service", name, zm.Status.Phase, got)
		}
	}
	if got := robotPhaseOf(t, ns, "r-out"); got != fleetv1.RobotPhaseIdle {
		t.Errorf("control robot r-out (corridor, outside the ward) was paused: phase %s", got)
	}
	if zm.Status.PausedRobotsCount != 2 {
		t.Errorf("status.pausedRobotsCount = %d, want 2 (r-a, r-b1)", zm.Status.PausedRobotsCount)
	}
}

func TestEnvtest_ZoneMaintenanceOnPartlyUnresolvedCompositeDoesNotActivate(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	// room-gone is declared but does not exist (deleted after the composite was written).
	zmCompositeFixture(t, ns, []*fleetv1.FleetZone{
		{ObjectMeta: metav1.ObjectMeta{Name: "room-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ward-3"}, Spec: fleetv1.FleetZoneSpec{MemberZones: []string{"room-a", "room-gone"}}},
	}, map[string]string{"r-a": "room-a"})

	if err := envK8s.Create(ctx, zmZoneScoped(ns, "drain-ward", "ward-3")); err != nil {
		t.Fatalf("create ZM: %v", err)
	}
	r := &ZoneMaintenanceReconciler{Client: envK8s, Scheme: envScheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "drain-ward"}}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // must refuse to activate

	zm := &fleetv1.ZoneMaintenance{}
	if err := envK8s.Get(ctx, req.NamespacedName, zm); err != nil {
		t.Fatal(err)
	}
	if zm.Status.Phase == fleetv1.ZoneMaintenancePhaseActive {
		t.Errorf("a window whose composite scope does not fully resolve went Active; it MUST NOT "+
			"activate (paused robots: %d)", zm.Status.PausedRobotsCount)
	}
	if got := robotPhaseOf(t, ns, "r-a"); got != fleetv1.RobotPhaseIdle {
		t.Errorf("robot r-a was paused (%s) by a window that must pause nobody", got)
	}
	cond := findCond(zm.Status.Conditions, zmScopeResolvedCond)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != zmCompositionUnresolvedWhy ||
		!strings.Contains(cond.Message, "room-gone") {
		t.Errorf("condition %s = %+v, want False / %s naming room-gone",
			zmScopeResolvedCond, cond, zmCompositionUnresolvedWhy)
	}

	// Once the missing member exists, the same window activates and drains the whole ward.
	zmCompositeFixture(t, ns, []*fleetv1.FleetZone{{ObjectMeta: metav1.ObjectMeta{Name: "room-gone"}}},
		map[string]string{"r-g": "room-gone"})
	mustReconcile(t, r, req)
	if err := envK8s.Get(ctx, req.NamespacedName, zm); err != nil {
		t.Fatal(err)
	}
	if zm.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("phase = %s after the member resolved, want Active", zm.Status.Phase)
	}
	for _, name := range []string{"r-a", "r-g"} {
		if got := robotPhaseOf(t, ns, name); got != fleetv1.RobotPhaseMaintenance {
			t.Errorf("robot %s phase = %s after activation, want Maintenance", name, got)
		}
	}
	if c := findCond(zm.Status.Conditions, zmScopeResolvedCond); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("condition %s = %+v after the member resolved, want True", zmScopeResolvedCond, c)
	}
}
