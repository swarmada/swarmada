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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// FZ-G5 end to end: a robot in a zone with no polygon, reporting no position at all,
// is placed in its declared zone and receives zone-scoped work. Candidate filter 2
// reads status.currentZone and excludes "", so without declared membership such a
// robot is excluded from every zone-scoped FleetAction.
//
// Robot names are unique across this package because ObservePosition resolves a
// robot_id cluster-wide, and envtest namespaces are never garbage-collected.

type declaredEnv struct {
	t      *testing.T
	ns     string
	zc     *ZoneController
	events *record.FakeRecorder
	ar     *FleetActionReconciler
}

func newDeclaredEnv(t *testing.T) *declaredEnv {
	t.Helper()
	requireEnvtest(t)
	rec := record.NewFakeRecorder(64)
	return &declaredEnv{
		t:      t,
		ns:     envtestNamespace(t),
		zc:     &ZoneController{Client: envK8s, Scheme: envScheme, Recorder: rec},
		events: rec,
		ar: &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
			Scheduler: scheduler.NewDefaultScheduler(), Commander: &fakeCommander{}},
	}
}

func (e *declaredEnv) reconcileZone(name string) {
	e.t.Helper()
	if _, err := e.zc.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: e.ns},
	}); err != nil {
		e.t.Fatalf("reconciling zone %q: %v", name, err)
	}
}

func (e *declaredEnv) reconcileAction(req ctrl.Request) *fleetv1.FleetAction {
	e.t.Helper()
	if _, err := e.ar.Reconcile(context.Background(), req); err != nil {
		e.t.Fatalf("reconciling action: %v", err)
	}
	return zrGetAction(e.t, req)
}

func (e *declaredEnv) robot(name string) *fleetv1.Robot {
	e.t.Helper()
	var r fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: e.ns, Name: name}, &r); err != nil {
		e.t.Fatalf("get robot %q: %v", name, err)
	}
	return &r
}

// seedRobotWithNoPosition creates an Idle, recently-seen robot declared into zoneName.
// It sets no currentZone and no position, and no frame is ever sent for it.
func (e *declaredEnv) seedRobotWithNoPosition(name, zoneName string) {
	e.t.Helper()
	ctx := context.Background()
	robot := envtestValidRobot(e.ns, name, zoneName)
	if err := envK8s.Create(ctx, robot); err != nil {
		e.t.Fatalf("create robot %q: %v", name, err)
	}
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &metav1.Time{Time: time.Now()}}
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		e.t.Fatalf("seed robot status %q: %v", name, err)
	}
}

// placeAndAssign builds the starting state both tests share: an identity-only leaf
// zone, a robot declared into it with no position telemetry, and a zoned action
// naming the zone. It asserts the action is withheld before declared membership is
// written and assigned after, and returns the action request.
func (e *declaredEnv) placeAndAssign(zoneName, robotName string) ctrl.Request {
	e.t.Helper()
	f12SeedAdapter(e.t, e.ns)
	if err := envK8s.Create(context.Background(), &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: e.ns},
	}); err != nil {
		e.t.Fatalf("create zone %q: %v", zoneName, err)
	}
	// Reconcile the zone before the robot exists, so its conditions and restriction
	// projection are published and the only difference between the withheld half and
	// the assigned half below is the robot's currentZone.
	e.reconcileZone(zoneName)
	e.seedRobotWithNoPosition(robotName, zoneName)

	req := zrSeedAction(e.t, e.ns, "act-1", zoneName)
	if got := e.reconcileAction(req); got.Status.AssignedRobot != "" {
		e.t.Fatalf("control failed: action assigned to %q before declared membership was written "+
			"(robot currentZone %q); this fixture does not exercise candidate filter 2",
			got.Status.AssignedRobot, e.robot(robotName).Status.CurrentZone)
	}

	// The robot's create enqueues this reconcile in a running manager.
	e.reconcileZone(zoneName)
	robot := e.robot(robotName)
	if robot.Status.CurrentZone != zoneName {
		e.t.Fatalf("currentZone = %q, want %q: FZ-G5 places a robot in its declared zone when that zone has no polygon",
			robot.Status.CurrentZone, zoneName)
	}
	if m := robot.Status.SpecZoneMatchesCurrent; m == nil || !*m {
		e.t.Fatalf("specZoneMatchesCurrent = %v, want true", m)
	}

	if got := e.reconcileAction(req); got.Status.AssignedRobot != robotName {
		e.t.Fatalf("assignedRobot = %q, want %q (phase %q, message %q)",
			got.Status.AssignedRobot, robotName, got.Status.Phase, got.Status.Message)
	}
	return req
}

// The deliverable: a robot with no position telemetry, in a leaf zone with no
// physicalBounds, is placed in that zone and is assigned a FleetAction naming it.
func TestEnvtest_FZG5_RobotWithNoPositionInIdentityOnlyZoneIsAssigned(t *testing.T) {
	e := newDeclaredEnv(t)
	e.placeAndAssign("fzg5-dock", "fzg5-noposition-amr")
}

// FZ-G8: adding a polygon to the zone is a widening change. Derivation takes over,
// a robot whose position is outside the new polygon becomes unmatched, and the action
// already assigned to it is not disturbed.
func TestEnvtest_FZG8_AddingAPolygonHandsTheZoneToDerivation(t *testing.T) {
	e := newDeclaredEnv(t)
	// The robot name is its robot id, and a position frame resolves the id across namespaces,
	// so it is unique per namespace: a second run in one process (-count=N) finds the first
	// run's robot still there.
	const zoneName = "fzg8-dock"
	robotName := "fzg8-widened-amr-" + e.ns
	req := e.placeAndAssign(zoneName, robotName)
	before := zrGetAction(t, req)

	var fz fleetv1.FleetZone
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: e.ns, Name: zoneName}, &fz); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	fz.Spec.PhysicalBounds = &fleetv1.PhysicalBounds{
		Floor:   0,
		Polygon: []fleetv1.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}},
		Frame:   &fleetv1.CoordinateFrame{ID: "site", Alignment: fleetv1.FrameSiteAligned},
	}
	if err := envK8s.Update(context.Background(), &fz); err != nil {
		t.Fatalf("add polygon: %v", err)
	}
	rv := e.robot(robotName).ResourceVersion
	e.reconcileZone(zoneName)
	if got := e.robot(robotName).ResourceVersion; got != rv {
		t.Fatalf("widening reconcile wrote robot status (rv %s -> %s); only a position frame may move it now", rv, got)
	}

	// Inside the new polygon: derivation agrees with the declaration, nothing moves.
	e.zc.ObservePosition(context.Background(), telemetry.Frame{
		RobotID: robotName, Position: &telemetry.Position{X: 5, Y: 5},
	})
	if got := e.robot(robotName).Status.CurrentZone; got != zoneName {
		t.Fatalf("frame inside the polygon: currentZone = %q, want %q", got, zoneName)
	}

	// Outside it: derivation governs, and the robot is unmatched.
	e.zc.ObservePosition(context.Background(), telemetry.Frame{
		RobotID: robotName, Position: &telemetry.Position{X: 50, Y: 50},
	})
	robot := e.robot(robotName)
	if robot.Status.CurrentZone != "" {
		t.Fatalf("frame outside the polygon: currentZone = %q, want \"\" once the zone has usable geometry",
			robot.Status.CurrentZone)
	}
	var unmatched int
	for done := false; !done; {
		select {
		case ev := <-e.events.Events:
			if contains(ev, "ZonePositionUnmatched") {
				unmatched++
			}
		default:
			done = true
		}
	}
	if unmatched != 1 {
		t.Fatalf("got %d ZonePositionUnmatched events, want 1", unmatched)
	}

	after := e.reconcileAction(req)
	if after.Status.AssignedRobot != before.Status.AssignedRobot || after.Status.Phase != before.Status.Phase {
		t.Fatalf("in-flight action disturbed by the widening: assignedRobot %q -> %q, phase %q -> %q",
			before.Status.AssignedRobot, after.Status.AssignedRobot, before.Status.Phase, after.Status.Phase)
	}
}
