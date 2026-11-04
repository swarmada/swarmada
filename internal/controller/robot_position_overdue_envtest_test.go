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
	"github.com/swarmada/swarmada/internal/telemetry"
)

// A robot declared into a zone with a site-aligned polygon is placed only by a position
// frame. Until one arrives, candidate filter 2 withholds zone-scoped work from it, and the
// ZonePositionOverdue condition says so: False with AwaitingFirstPosition, then True with
// NoPositionReported once the threshold has passed.

var siteSquare = &fleetv1.PhysicalBounds{
	Floor:   0,
	Polygon: []fleetv1.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}},
	Frame:   &fleetv1.CoordinateFrame{ID: "site", Alignment: fleetv1.FrameSiteAligned},
}

func (e *declaredEnv) robotReconciler(threshold time.Duration) *RobotReconciler {
	return &RobotReconciler{Client: envK8s, Scheme: envScheme, Positions: e.zc, positionOverdueThreshold: threshold}
}

func (e *declaredEnv) reconcileRobot(rr *RobotReconciler, name string) {
	e.t.Helper()
	if _, err := rr.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: e.ns},
	}); err != nil {
		e.t.Fatalf("reconciling robot %q: %v", name, err)
	}
}

func (e *declaredEnv) overdue(name string) *metav1.Condition {
	e.t.Helper()
	return findCondition(e.robot(name), conditionTypeZonePositionOverdue)
}

func (e *declaredEnv) wantOverdue(name string, status metav1.ConditionStatus, reason string) *metav1.Condition {
	e.t.Helper()
	c := e.overdue(name)
	if c == nil {
		e.t.Fatalf("robot %q carries no %s condition, want %s/%s", name, conditionTypeZonePositionOverdue, status, reason)
	}
	if c.Status != status || c.Reason != reason {
		e.t.Fatalf("robot %q %s = %s/%s (%q), want %s/%s", name, conditionTypeZonePositionOverdue,
			c.Status, c.Reason, c.Message, status, reason)
	}
	return c
}

// withheldInContainmentZone creates a zone with a site-aligned polygon, a robot declared
// into it that sends no frame, and a zoned action naming the zone. It asserts the robot
// is not placed and the action is not assigned, and returns the action request.
func (e *declaredEnv) withheldInContainmentZone(zoneName, robotName string) ctrl.Request {
	e.t.Helper()
	f12SeedAdapter(e.t, e.ns)
	if err := envK8s.Create(context.Background(), &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: e.ns},
		Spec:       fleetv1.FleetZoneSpec{PhysicalBounds: siteSquare.DeepCopy()},
	}); err != nil {
		e.t.Fatalf("create zone %q: %v", zoneName, err)
	}
	e.reconcileZone(zoneName)
	e.seedRobotWithNoPosition(robotName, zoneName)
	e.reconcileZone(zoneName) // the robot's create enqueues this in a running manager

	if got := e.robot(robotName).Status.CurrentZone; got != "" {
		e.t.Fatalf("currentZone = %q, want \"\": a zone with a site-aligned polygon is not placed by declaration", got)
	}
	req := zrSeedAction(e.t, e.ns, "act-1", zoneName)
	if got := e.reconcileAction(req); got.Status.AssignedRobot != "" {
		e.t.Fatalf("action assigned to %q; filter 2 withholds a robot with no currentZone", got.Status.AssignedRobot)
	}
	return req
}

// The deliverable: no currentZone, no zoned action, and the condition published with the
// never-reported reason.
func TestEnvtest_ZonePositionOverdue_RobotThatNeverReportsIsWithheldAndSaysSo(t *testing.T) {
	e := newDeclaredEnv(t)
	const zoneName, robotName = "zpo-bay", "zpo-never-reports-amr"
	req := e.withheldInContainmentZone(zoneName, robotName)

	e.reconcileRobot(e.robotReconciler(time.Hour), robotName)
	e.wantOverdue(robotName, metav1.ConditionFalse, reasonAwaitingFirstPosition)

	e.reconcileRobot(e.robotReconciler(time.Nanosecond), robotName)
	c := e.wantOverdue(robotName, metav1.ConditionTrue, reasonNoPositionReported)
	if !strings.Contains(c.Message, "withheld") {
		t.Fatalf("message %q does not say zone-scoped actions are withheld", c.Message)
	}

	robot := e.robot(robotName)
	if robot.Status.CurrentZone != "" {
		t.Fatalf("currentZone = %q; the condition must not place the robot", robot.Status.CurrentZone)
	}
	if got := e.reconcileAction(req); got.Status.AssignedRobot != "" {
		t.Fatalf("action assigned to %q; the condition must not change dispatch", got.Status.AssignedRobot)
	}
}

// The first position frame that places the robot clears the condition, and the action
// withheld until then is assigned.
func TestEnvtest_ZonePositionOverdue_FirstPositionFramePlacesAndClears(t *testing.T) {
	e := newDeclaredEnv(t)
	const zoneName, robotName = "zpo-clear-bay", "zpo-first-frame-amr"
	req := e.withheldInContainmentZone(zoneName, robotName)

	e.reconcileRobot(e.robotReconciler(time.Hour), robotName)
	e.reconcileRobot(e.robotReconciler(time.Nanosecond), robotName)
	e.wantOverdue(robotName, metav1.ConditionTrue, reasonNoPositionReported)

	e.zc.ObservePosition(context.Background(), telemetry.Frame{
		RobotID: robotName, Namespace: e.ns, Position: &telemetry.Position{X: 5, Y: 5},
	})
	if got := e.robot(robotName).Status.CurrentZone; got != zoneName {
		t.Fatalf("currentZone = %q after a frame inside the polygon, want %q", got, zoneName)
	}
	// The currentZone write enqueues this reconcile in a running manager.
	e.reconcileRobot(e.robotReconciler(time.Nanosecond), robotName)
	e.wantOverdue(robotName, metav1.ConditionFalse, reasonPositionReported)

	if got := e.reconcileAction(req); got.Status.AssignedRobot != robotName {
		t.Fatalf("assignedRobot = %q, want %q once a frame has placed the robot (phase %q, message %q)",
			got.Status.AssignedRobot, robotName, got.Status.Phase, got.Status.Message)
	}
}

// FZ-G8 widening without telemetry: a robot placed by declaration keeps currentZone after
// its zone gains a polygon. The condition reaches it, and the action already assigned to
// it is not disturbed.
func TestEnvtest_ZonePositionOverdue_WideningWithoutTelemetry(t *testing.T) {
	e := newDeclaredEnv(t)
	const zoneName, robotName = "zpo-widen-dock", "zpo-widened-amr"
	req := e.placeAndAssign(zoneName, robotName)
	before := zrGetAction(t, req)

	e.reconcileRobot(e.robotReconciler(time.Nanosecond), robotName)
	if c := e.overdue(robotName); c != nil {
		t.Fatalf("robot in an identity-only zone carries %s %s/%s; FZ-G5 places it by declaration",
			conditionTypeZonePositionOverdue, c.Status, c.Reason)
	}

	var fz fleetv1.FleetZone
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: e.ns, Name: zoneName}, &fz); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	fz.Spec.PhysicalBounds = siteSquare.DeepCopy()
	if err := envK8s.Update(context.Background(), &fz); err != nil {
		t.Fatalf("add polygon: %v", err)
	}
	rv := e.robot(robotName).ResourceVersion
	e.reconcileZone(zoneName)
	if got := e.robot(robotName).ResourceVersion; got != rv {
		t.Fatalf("widening reconcile wrote robot status (rv %s -> %s)", rv, got)
	}

	e.reconcileRobot(e.robotReconciler(time.Hour), robotName)
	e.wantOverdue(robotName, metav1.ConditionFalse, reasonAwaitingFirstPosition)
	e.reconcileRobot(e.robotReconciler(time.Nanosecond), robotName)
	c := e.wantOverdue(robotName, metav1.ConditionTrue, reasonNoPositionReported)
	if !strings.Contains(c.Message, `still reads "`+zoneName+`"`) {
		t.Fatalf("message %q does not name the unconfirmed currentZone", c.Message)
	}
	if got := e.robot(robotName).Status.CurrentZone; got != zoneName {
		t.Fatalf("currentZone = %q, want %q: the condition does not move the robot", got, zoneName)
	}

	after := e.reconcileAction(req)
	if after.Status.AssignedRobot != before.Status.AssignedRobot || after.Status.Phase != before.Status.Phase {
		t.Fatalf("in-flight action disturbed: assignedRobot %q -> %q, phase %q -> %q",
			before.Status.AssignedRobot, after.Status.AssignedRobot, before.Status.Phase, after.Status.Phase)
	}
}

// FZ-G5 is unchanged: a robot in a zone with no usable geometry is placed by declaration
// and never acquires the condition, however long it sends nothing.
func TestEnvtest_ZonePositionOverdue_IdentityOnlyZoneNeverCarriesIt(t *testing.T) {
	e := newDeclaredEnv(t)
	const zoneName, robotName = "zpo-identity-dock", "zpo-identity-amr"
	e.placeAndAssign(zoneName, robotName)
	rr := e.robotReconciler(time.Nanosecond)
	for i := 0; i < 3; i++ {
		e.reconcileRobot(rr, robotName)
	}
	if c := e.overdue(robotName); c != nil {
		t.Fatalf("robot in an identity-only zone carries %s %s/%s", conditionTypeZonePositionOverdue, c.Status, c.Reason)
	}
	if got := e.robot(robotName).Status.CurrentZone; got != zoneName {
		t.Fatalf("currentZone = %q, want %q", got, zoneName)
	}
}
