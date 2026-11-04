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
	"reflect"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// FA-R7 on the telemetry path (ADR-0059).
//
// RA-1 IS THE PROPERTY UNDER TEST: containment is derived from position telemetry,
// which is exactly where a per-tick status write is prohibited. The write is gated as
// ObservePosition gates currentZone — a compare-and-set against both the last write and
// the persisted status — and the same gate covers the containment set and the push.

type fakeConstraintPush struct {
	mu    sync.Mutex
	calls []command.ZoneConstraints
	to    []string
}

func (f *fakeConstraintPush) NotifyZoneConstraints(_ context.Context, _, robotID string, c command.ZoneConstraints) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.to = append(f.to, robotID)
}

func (f *fakeConstraintPush) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeConstraintPush) last() command.ZoneConstraints {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return command.ZoneConstraints{}
	}
	return f.calls[len(f.calls)-1]
}

// restrictedZone builds a zone with a declared operating rule and the matching resolved
// projection, as the Zone Controller leaves it.
func restrictedZone(name string, pts []fleetv1.Point, rules ...fleetv1.ZoneOperatingRestriction) *fleetv1.FleetZone {
	fz := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetZoneSpec{PhysicalBounds: &fleetv1.PhysicalBounds{Floor: 0, Polygon: pts}},
	}
	effective := make([]fleetv1.EffectiveZoneRestriction, 0, len(rules))
	for _, r := range rules {
		effective = append(effective, fleetv1.EffectiveZoneRestriction{
			Axis: fleetv1.ZoneAxisOperating, Subject: r.Subject, Mode: r.Mode,
			Max: r.Max, Allowed: r.Allowed, Reason: r.Reason, InheritedFrom: name,
		})
	}
	if len(rules) > 0 {
		fz.Spec.Restrictions = &fleetv1.ZoneRestrictions{Operating: rules}
	}
	fz.Status.Restrictions = &fleetv1.ZoneRestrictionsStatus{Effective: effective}
	fz.Status.Conditions = []metav1.Condition{{
		Type: fleetv1.ZoneCondRestrictionsResolved, Status: metav1.ConditionTrue,
		Reason: "Resolved", LastTransitionTime: metav1.Now(),
	}}
	return fz
}

func denies(subject, reason string) fleetv1.ZoneOperatingRestriction {
	return fleetv1.ZoneOperatingRestriction{
		Subject: subject, Mode: fleetv1.ZoneRestrictionDenied, Reason: reason,
	}
}

// overlapping rectangles: wardPts spans (0,0)-(120,80); bayPts spans (100,60)-(160,120).
var (
	wardPts = []fleetv1.Point{{X: 0, Y: 0}, {X: 120, Y: 0}, {X: 120, Y: 80}, {X: 0, Y: 80}}
	bayPts  = []fleetv1.Point{{X: 100, Y: 60}, {X: 160, Y: 60}, {X: 160, Y: 120}, {X: 100, Y: 120}}
)

func containmentFixture(t *testing.T, push *fakeConstraintPush, zones ...*fleetv1.FleetZone) (*ZoneController, *fakeConstraintPush) {
	t.Helper()
	objs := []client.Object{robotAt("amr-1", "rid1", "")}
	for _, fz := range zones {
		objs = append(objs, fz)
	}
	z, _ := newZoneController(t, objs...)
	if push != nil {
		z.Constraints = push
	}
	cands := make([]zone.Candidate, 0, len(zones))
	for _, fz := range zones {
		cands = append(cands, zone.Candidate{
			Name: fz.Name, Floor: 0, Polygon: toZonePoints(fz.Spec.PhysicalBounds.Polygon), IsLeaf: true, Depth: 1,
		})
	}
	z.setZones(actionNS, cands)
	return z, push
}

// A TICK THAT CHANGES NOTHING WRITES NOTHING AND SENDS NOTHING.
//
// This is the whole of §3, and the number that matters is the SECOND frame's: the first
// establishes containment (a real transition), and every identical frame after it must
// be free. The assertion is on resourceVersion, not on a call count, because a status
// patch that writes identical bytes is still an etcd write and still a watch event for
// every controller in the namespace.
func TestContainmentPush_AnUnchangedTickWritesNothingAndSendsNothing(t *testing.T) {
	push := &fakeConstraintPush{}
	ward := restrictedZone("ward", wardPts, denies("camera", "patient privacy"))
	z, _ := containmentFixture(t, push, ward)
	c := z.Client
	ctx := context.Background()
	inside := &telemetry.Position{X: 60, Y: 40, Floor: 0}

	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: inside})
	first := getRobotRV(t, c, "amr-1")
	if push.count() != 1 {
		t.Fatalf("%d pushes after the entry transition, want 1", push.count())
	}

	for i := 0; i < 5; i++ {
		z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: inside})
	}
	if got := getRobotRV(t, c, "amr-1"); got != first {
		t.Fatalf("resourceVersion moved from %q to %q across five identical frames. Containment "+
			"is derived from POSITION TELEMETRY, which is precisely where a per-tick status "+
			"write is prohibited (RA-1): a tick that changes nothing must write nothing",
			first, got)
	}
	if push.count() != 1 {
		t.Fatalf("%d pushes after five identical frames, want 1. A tick that changes nothing "+
			"must also SEND nothing — a command per frame is the same defect as a write per "+
			"frame, arriving on the wire instead of in etcd", push.count())
	}
}

// The set is published, sorted, and is the SET rather than the arbitrated winner.
func TestContainmentPush_TheSetIsPublishedBesideTheArbitratedWinner(t *testing.T) {
	push := &fakeConstraintPush{}
	z, _ := containmentFixture(t, push,
		restrictedZone("ward", wardPts, denies("camera", "patient privacy")),
		restrictedZone("bay", bayPts))
	c := z.Client
	// (110, 70) is inside both rectangles.
	z.ObservePosition(context.Background(), telemetry.Frame{
		RobotID: "rid1", Position: &telemetry.Position{X: 110, Y: 70, Floor: 0}})

	robot := getRobot(t, c, "amr-1", actionNS)
	if !reflect.DeepEqual(robot.Status.ContainingZones, []string{"bay", "ward"}) {
		t.Fatalf("containingZones = %v, want [bay ward]", robot.Status.ContainingZones)
	}
	if robot.Status.CurrentZone == "" {
		t.Fatal("currentZone was cleared; its derivation and cardinality must not change")
	}
	if len(robot.Status.ContainingZones) < 2 {
		t.Fatal("the fixture is not exercising an overlap")
	}
}

// THE DENIAL FROM THE OVERLAPPING ZONE REACHES THE ADAPTER, end to end: the robot stands
// in both polygons, the position report arbitrates to one, and the rule from the other is
// what the adapter is told about (ADR-0059 §2).
func TestContainmentPush_TheOverlappingZonesDenialIsPushed(t *testing.T) {
	push := &fakeConstraintPush{}
	z, _ := containmentFixture(t, push,
		restrictedZone("ward", wardPts, denies("camera", "patient privacy")),
		restrictedZone("bay", bayPts))
	z.ObservePosition(context.Background(), telemetry.Frame{
		RobotID: "rid1", Position: &telemetry.Position{X: 110, Y: 70, Floor: 0}})

	got := push.last()
	if !reflect.DeepEqual(got.ContainingZones, []string{"bay", "ward"}) {
		t.Fatalf("pushed containingZones = %v, want [bay ward]", got.ContainingZones)
	}
	if len(got.OperatingConstraints) != 1 {
		t.Fatalf("pushed %d constraints, want the camera denial: %+v",
			len(got.OperatingConstraints), got.OperatingConstraints)
	}
	oc := got.OperatingConstraints[0]
	if oc.GetSubject() != "camera" || oc.GetDeclaringZone() != "ward" {
		t.Fatalf("pushed constraint = %+v, want camera declared by ward", oc)
	}
	if oc.GetMode().String() != "OPERATING_CONSTRAINT_MODE_DENIED" {
		t.Errorf("mode = %s, want DENIED", oc.GetMode())
	}
	if oc.GetReason() != "patient privacy" {
		t.Errorf("reason = %q, want the operator's own words", oc.GetReason())
	}
}

// REQUIRED TRAVELS. Entering a zone can oblige a subject to be ON, and a push that only
// ever forbade would implement half the enum.
func TestContainmentPush_RequiredIsCarriedNotOnlyDenied(t *testing.T) {
	push := &fakeConstraintPush{}
	z, _ := containmentFixture(t, push, restrictedZone("terminal-clean", wardPts,
		fleetv1.ZoneOperatingRestriction{
			Subject: "uv", Mode: fleetv1.ZoneRestrictionRequired,
			Reason: "terminal clean; the lamp must be running",
		}))
	z.ObservePosition(context.Background(), telemetry.Frame{
		RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40, Floor: 0}})

	got := push.last()
	if len(got.OperatingConstraints) != 1 {
		t.Fatalf("pushed %d constraints, want the uv requirement", len(got.OperatingConstraints))
	}
	if m := got.OperatingConstraints[0].GetMode().String(); m != "OPERATING_CONSTRAINT_MODE_REQUIRED" {
		t.Fatalf("mode = %s, want REQUIRED. The maintainer's phrasing was \"what capabilities "+
			"can be active\": Required is the inspection-zone mirror case, and a push that "+
			"only ever forbids implements half the enum", m)
	}
}

// LEAVING A RESTRICTED REGION SENDS AN EMPTY SET, which is PERMISSION. A robot that
// walked out of a ward must be told the rule no longer applies; leaving it holding the
// last set it received would make the ward's boundary one-way.
func TestContainmentPush_LeavingSendsAnEmptySetWhichIsPermission(t *testing.T) {
	push := &fakeConstraintPush{}
	z, _ := containmentFixture(t, push, restrictedZone("ward", wardPts, denies("camera", "privacy")))
	ctx := context.Background()

	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40}})
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 9000, Y: 9000}})

	if push.count() != 2 {
		t.Fatalf("%d pushes, want 2 (enter, then leave)", push.count())
	}
	got := push.last()
	if len(got.OperatingConstraints) != 0 || len(got.ContainingZones) != 0 {
		t.Fatalf("the exit push carried %+v; leaving a restricted region sends an EMPTY set, "+
			"which is permission — not a lockout, and not silence", got)
	}
}

// A CONTAINMENT CHANGE THAT CHANGES NO RULE SENDS NOTHING. The status write still
// happens, because containingZones must report what is true; the command does not,
// because nothing an adapter has to do has changed. Two gates, not one.
func TestContainmentPush_CrossingBetweenUnrestrictedZonesSendsNothing(t *testing.T) {
	push := &fakeConstraintPush{}
	z, _ := containmentFixture(t, push,
		restrictedZone("open-a", wardPts),
		restrictedZone("open-b", bayPts))
	c := z.Client
	ctx := context.Background()

	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 10, Y: 10}})
	rv1 := getRobotRV(t, c, "amr-1")
	sent := push.count()

	// Cross into open-b only: containment moves, the rule set does not.
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 150, Y: 110}})
	if got := getRobotRV(t, c, "amr-1"); got == rv1 {
		t.Fatal("containment moved and status did not: containingZones must report what is true")
	}
	if push.count() != sent {
		t.Fatalf("%d pushes after crossing between two unrestricted zones, want %d. The rule "+
			"set did not move, and re-sending an identical one would be a command per boundary "+
			"crossing on a fleet where a robot may cross several in a minute", push.count(), sent)
	}
}

// A RULE WRITTEN AFTER THE ROBOT ENTERED REACHES IT — the temporal half. Filters 11 and
// 12 gate dispatch only, so without the zone reconcile's re-push a camera denial written
// at T1 would reach a robot admitted at T0 through no path at all.
func TestContainmentPush_ARuleWrittenAfterEntryReachesTheRobot(t *testing.T) {
	push := &fakeConstraintPush{}
	z, _ := containmentFixture(t, push, restrictedZone("ward", wardPts))
	c := z.Client
	ctx := context.Background()

	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40}})
	if got := push.last(); len(got.OperatingConstraints) != 0 {
		t.Fatalf("the zone declares nothing yet, and the push carried %+v", got.OperatingConstraints)
	}
	before := push.count()

	// The operator writes the rule while the robot is standing there.
	var fz fleetv1.FleetZone
	if err := c.Get(ctx, types.NamespacedName{Name: "ward", Namespace: actionNS}, &fz); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	fz.Spec.Restrictions = &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{denies("camera", "patient privacy")},
	}
	fz.Generation++ // a spec write advances it; the fake client does not
	if err := c.Update(ctx, &fz); err != nil {
		t.Fatalf("update zone: %v", err)
	}
	reconcileZone(t, z, "ward")

	if push.count() <= before {
		t.Fatalf("%d pushes, want more than %d. A restriction written after a robot entered "+
			"reached it through no path whatsoever: filters 11 and 12 gate DISPATCH, and "+
			"nothing re-evaluated a robot once it was moving", push.count(), before)
	}
	got := push.last()
	if len(got.OperatingConstraints) != 1 || got.OperatingConstraints[0].GetSubject() != "camera" {
		t.Fatalf("the re-push carried %+v, want the camera denial", got.OperatingConstraints)
	}
}

// A ZONE RECONCILE THAT CHANGES NO RULE RE-PUSHES NOTHING. The re-push is gated on the
// resolved set, so a resync, an unrelated spec edit, or a status write sends nobody
// anything.
func TestContainmentPush_AZoneResyncRePushesNothing(t *testing.T) {
	push := &fakeConstraintPush{}
	z, _ := containmentFixture(t, push, restrictedZone("ward", wardPts, denies("camera", "privacy")))
	ctx := context.Background()
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40}})
	before := push.count()

	for i := 0; i < 3; i++ {
		reconcileZone(t, z, "ward")
	}
	if push.count() != before {
		t.Fatalf("%d pushes after three resyncs, want %d. FZ-R5 makes the projection a "+
			"material-transition product; a re-push on every reconcile would put a command "+
			"per resync on the wire", push.count(), before)
	}
}

// A NIL NOTIFIER CHANGES NOTHING ELSE. The set is still derived and written, so the API
// still reports what binds a robot in a deployment with no adapter listening.
func TestContainmentPush_ANilNotifierStillPublishesTheSet(t *testing.T) {
	z, _ := containmentFixture(t, nil, restrictedZone("ward", wardPts, denies("camera", "privacy")))
	z.Constraints = nil
	z.ObservePosition(context.Background(), telemetry.Frame{
		RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40}})
	robot := getRobot(t, z.Client, "amr-1", actionNS)
	if !reflect.DeepEqual(robot.Status.ContainingZones, []string{"ward"}) {
		t.Fatalf("containingZones = %v, want [ward]", robot.Status.ContainingZones)
	}
}

// getRobotRV reads a robot's resourceVersion. The RA-1 assertions are on this and not
// on a call count: a status patch that writes identical bytes is still an etcd write and
// still a watch event for every controller in the namespace.
func getRobotRV(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var r fleetv1.Robot
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: actionNS}, &r); err != nil {
		t.Fatalf("get robot %q: %v", name, err)
	}
	return r.ResourceVersion
}
