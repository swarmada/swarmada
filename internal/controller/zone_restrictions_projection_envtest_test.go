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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// The Zone Controller's restriction projection (FZ-R3 … FZ-R6).
//
// These run on envtest rather than the fake client because the thing under test is
// a STATUS SUBRESOURCE write and a generation-driven recompute guard. The fake
// client does not bump metadata.generation on a spec write and does not separate
// the status subresource, so FZ-R5 — the rule that the projection is recomputed on
// a spec or topology change and NOT on a telemetry tick — would be untestable
// against it: the memo key it guards is metadata.generation.

// zrEnv is a zone-restriction test fixture: a namespace, a controller wired to the
// envtest API server, and a recorder whose events the test can read.
type zrEnv struct {
	t      *testing.T
	ns     string
	zc     *ZoneController
	events *record.FakeRecorder
}

func newZREnv(t *testing.T) *zrEnv {
	t.Helper()
	requireEnvtest(t)
	ns := envtestNamespace(t)
	rec := record.NewFakeRecorder(64)
	return &zrEnv{
		t:      t,
		ns:     ns,
		zc:     &ZoneController{Client: envK8s, Scheme: envScheme, Recorder: rec},
		events: rec,
	}
}

// zone creates a FleetZone with the given parent and restrictions.
func (e *zrEnv) zone(name, parent string, r *fleetv1.ZoneRestrictions) *fleetv1.FleetZone {
	e.t.Helper()
	fz := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Spec:       fleetv1.FleetZoneSpec{DisplayName: name, ParentZone: parent, Restrictions: r},
	}
	if err := envK8s.Create(context.Background(), fz); err != nil {
		e.t.Fatalf("creating zone %q: %v", name, err)
	}
	return fz
}

func (e *zrEnv) reconcile(name string) {
	e.t.Helper()
	if _, err := e.zc.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: e.ns},
	}); err != nil {
		e.t.Fatalf("reconciling %q: %v", name, err)
	}
}

func (e *zrEnv) get(name string) *fleetv1.FleetZone {
	e.t.Helper()
	var fz fleetv1.FleetZone
	if err := envK8s.Get(context.Background(), types.NamespacedName{Name: name, Namespace: e.ns}, &fz); err != nil {
		e.t.Fatalf("getting %q: %v", name, err)
	}
	return &fz
}

func conditionOf(fz *fleetv1.FleetZone, condType string) *metav1.Condition {
	for i := range fz.Status.Conditions {
		if fz.Status.Conditions[i].Type == condType {
			return &fz.Status.Conditions[i]
		}
	}
	return nil
}

// drainEvents returns every event the recorder holds, without blocking.
func (e *zrEnv) drainEvents() []string {
	var out []string
	for {
		select {
		case ev := <-e.events.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// FZ-R4 — the projection, with inheritedFrom, and both conditions.
// ---------------------------------------------------------------------------

func TestEnvtest_ZoneRestrictions_ProjectionCarriesInheritedFrom(t *testing.T) {
	e := newZREnv(t)

	e.zone("site", "", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied, Reason: "patient privacy"},
			{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited, Max: "1.5"},
		},
	})
	e.zone("ward-3", "site", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited, Max: "0.4", Reason: "occupied ward at night"},
		},
	})
	e.zone("bay-2", "ward-3", nil)

	e.reconcile("bay-2")
	got := e.get("bay-2")

	if got.Status.Restrictions == nil {
		t.Fatal("status.restrictions is nil on a zone whose ancestors restrict. FZ-R4: the Zone " +
			"Controller writes the resolved set; consumers read status, never the tree")
	}
	byKey := effBySubject(got.Status.Restrictions.Effective)

	cam, ok := byKey["Operating/camera"]
	if !ok {
		t.Fatal("the ancestor's camera denial did not reach the leaf's projection")
	}
	if cam.Mode != fleetv1.ZoneRestrictionDenied || cam.InheritedFrom != "site" {
		t.Errorf("camera = {mode:%s inheritedFrom:%q}, want {Denied site}", cam.Mode, cam.InheritedFrom)
	}
	if cam.Reason != "patient privacy" {
		t.Errorf("camera reason = %q; the operator's words must travel with the rule", cam.Reason)
	}

	speed := byKey["Operating/speed"]
	if speed.Max != "0.4" || speed.InheritedFrom != "ward-3" {
		t.Errorf("speed = {max:%q inheritedFrom:%q}, want {0.4 ward-3} — the zone named must be "+
			"the one whose bound is actually in force, not the first to mention the subject",
			speed.Max, speed.InheritedFrom)
	}

	resolved := conditionOf(got, fleetv1.ZoneCondRestrictionsResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue {
		t.Errorf("RestrictionsResolved = %v, want True", resolved)
	}
	consistent := conditionOf(got, fleetv1.ZoneCondRestrictionsConsistent)
	if consistent == nil || consistent.Status != metav1.ConditionTrue {
		t.Errorf("RestrictionsConsistent = %v, want True", consistent)
	}

	// A zone with no restrictions anywhere in its chain must still publish
	// RestrictionsResolved — an absent condition is indistinguishable from a
	// controller that has not run, and K7's filters fail closed on that.
	e.zone("elsewhere", "", nil)
	e.reconcile("elsewhere")
	plain := e.get("elsewhere")
	if c := conditionOf(plain, fleetv1.ZoneCondRestrictionsResolved); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("an unrestricted zone published RestrictionsResolved = %v, want True", c)
	}
	if plain.Status.Restrictions != nil && len(plain.Status.Restrictions.Effective) != 0 {
		t.Errorf("an unrestricted zone projected %+v, want nothing", plain.Status.Restrictions.Effective)
	}
}

// ---------------------------------------------------------------------------
// FZ-R3 — contradiction: Denied wins, BOTH zones named, Warning emitted.
// ---------------------------------------------------------------------------

func TestEnvtest_ZoneRestrictions_ContradictionNamesBothZonesAndWarns(t *testing.T) {
	e := newZREnv(t)

	e.zone("hospital", "", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied, Reason: "patient privacy"},
		},
	})
	e.zone("inspection-bay", "hospital", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionRequired, Reason: "weld inspection"},
		},
	})

	e.reconcile("inspection-bay")
	got := e.get("inspection-bay")

	cam := effBySubject(got.Status.Restrictions.Effective)["Operating/camera"]
	if cam.Mode != fleetv1.ZoneRestrictionDenied {
		t.Errorf("camera projected as %s, want Denied — FZ-R3 resolves the contradiction to "+
			"Denied so the camera stays off and the action is declined, rather than switching "+
			"a camera on in a patient room by operation of a merge rule", cam.Mode)
	}

	consistent := conditionOf(got, fleetv1.ZoneCondRestrictionsConsistent)
	if consistent == nil || consistent.Status != metav1.ConditionFalse {
		t.Fatalf("RestrictionsConsistent = %v, want False", consistent)
	}
	// BOTH declaring zones, per FZ-R3.
	for _, want := range []string{"hospital", "inspection-bay", "camera"} {
		if !strings.Contains(consistent.Message, want) {
			t.Errorf("RestrictionsConsistent message %q does not name %q — FZ-R3 requires BOTH "+
				"declaring zones, because an operator told only one of them does not know "+
				"which edit to make", consistent.Message, want)
		}
	}

	// RestrictionsResolved stays True: a contradiction is resolved, not unresolvable.
	if c := conditionOf(got, fleetv1.ZoneCondRestrictionsResolved); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("RestrictionsResolved = %v, want True — a contradiction has an answer "+
			"(Denied); it is not a failure to resolve", c)
	}

	events := e.drainEvents()
	var found string
	for _, ev := range events {
		if strings.Contains(ev, "RestrictionContradiction") {
			found = ev
		}
	}
	if found == "" {
		t.Fatalf("no RestrictionContradiction event emitted; events were %v", events)
	}
	if !strings.Contains(found, corev1.EventTypeWarning) {
		t.Errorf("RestrictionContradiction event was not a Warning: %q", found)
	}
	for _, want := range []string{"hospital", "inspection-bay"} {
		if !strings.Contains(found, want) {
			t.Errorf("event %q does not name %q", found, want)
		}
	}

	// Edge-triggered: a resync must not re-warn, or an operator's event stream fills
	// with a fact that has not changed.
	e.reconcile("inspection-bay")
	for _, ev := range e.drainEvents() {
		if strings.Contains(ev, "RestrictionContradiction") {
			t.Errorf("a second reconcile re-emitted RestrictionContradiction: %q — the event is "+
				"edge-triggered, once per transition into the contradictory state", ev)
		}
	}
}

// ---------------------------------------------------------------------------
// FZ-R6 — resolution fails closed.
// ---------------------------------------------------------------------------

func TestEnvtest_ZoneRestrictions_UnresolvableChainIsNotDispatchable(t *testing.T) {
	e := newZREnv(t)

	// A zone naming a parent that does not exist. The webhook rejects this on a
	// normal write, but FZ-R9 is explicit that the resolver must hold on objects
	// admission never saw — restored from a backup, or written under
	// failurePolicy: Ignore. envtest runs no webhook, which is exactly that case.
	e.zone("orphan", "no-such-zone", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied},
		},
	})

	e.reconcile("orphan")
	got := e.get("orphan")

	resolved := conditionOf(got, fleetv1.ZoneCondRestrictionsResolved)
	if resolved == nil || resolved.Status != metav1.ConditionFalse {
		t.Fatalf("RestrictionsResolved = %v, want False. FZ-R6: a zone that names a parentZone "+
			"which does not resolve publishes False and is treated as NOT DISPATCHABLE. An "+
			"unresolvable restriction is not an absent restriction", resolved)
	}
	if !strings.Contains(resolved.Message, "no-such-zone") {
		t.Errorf("RestrictionsResolved message %q does not name the unresolvable parent", resolved.Message)
	}

	// The zone's own rules must still be projected — failing closed means reporting
	// the chain is untrustworthy, not discarding what IS known. Discarding would make
	// the zone look unrestricted, which is the permissive direction.
	if got.Status.Restrictions == nil || len(got.Status.Restrictions.Effective) == 0 {
		t.Error("an unresolvable zone projected NO restrictions. Its own declared denial is " +
			"still known and must still be published; emptying the set makes a denied zone " +
			"read as unrestricted, which is the one direction that must never happen")
	}
}

// ---------------------------------------------------------------------------
// FZ-R5 — a telemetry tick does not recompute the projection. This is RA-1.
// ---------------------------------------------------------------------------

func TestEnvtest_ZoneRestrictions_TelemetryTickDoesNotRecompute(t *testing.T) {
	e := newZREnv(t)

	e.zone("site", "", &fleetv1.ZoneRestrictions{
		Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied, Reason: "privacy"},
		},
	})
	// A leaf with a polygon, so a position frame actually derives a currentZone and
	// the telemetry path does real work rather than bailing early.
	leaf := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: e.ns},
		Spec: fleetv1.FleetZoneSpec{
			DisplayName: "bay", ParentZone: "site",
			PhysicalBounds: &fleetv1.PhysicalBounds{
				Floor: 0,
				Polygon: []fleetv1.Point{
					{X: -10, Y: -10}, {X: 10, Y: -10}, {X: 10, Y: 10}, {X: -10, Y: 10},
				},
			},
		},
	}
	if err := envK8s.Create(context.Background(), leaf); err != nil {
		t.Fatalf("creating leaf: %v", err)
	}

	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "amr-1", Namespace: e.ns,
			Annotations: map[string]string{RobotIDAnnotation: "amr-1-wire"},
		},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "Acme", Model: "X1", Zone: "bay",
			Adapter: fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
		},
	}
	if err := envK8s.Create(context.Background(), robot); err != nil {
		t.Fatalf("creating robot: %v", err)
	}

	// First reconcile: the projection is computed once.
	e.reconcile("bay")
	afterFirst := e.zc.RestrictionRecomputes()
	if afterFirst == 0 {
		t.Fatal("the first reconcile did not compute the projection at all")
	}
	before := e.get("bay")
	if before.Status.Restrictions == nil || len(before.Status.Restrictions.Effective) == 0 {
		t.Fatal("the projection was not written on the first reconcile")
	}

	// Now force telemetry. ObservePosition is the position path; it derives a zone
	// and writes Robot.status.currentZone on a TRANSITION, which is what enqueues a
	// FleetZone reconcile through the robotToZones watch. Twenty frames, one of them
	// a transition — this is the tick FZ-R5 names.
	for i := 0; i < 20; i++ {
		e.zc.ObservePosition(context.Background(), telemetry.Frame{
			RobotID:  "amr-1-wire",
			Position: &telemetry.Position{X: float64(i%3) - 1, Y: 0, Floor: 0},
		})
	}
	// And the reconciles those frames cause, exactly as the watch would deliver them.
	for i := 0; i < 5; i++ {
		e.reconcile("bay")
	}

	afterTicks := e.zc.RestrictionRecomputes()

	// RA-1's other half: restriction handling must contribute NO write.
	//
	// resourceVersion is deliberately NOT the assertion. It moves here for a reason
	// that predates this work and is correct: the robot entered the zone, so
	// status.robotCount went 0 -> 1, which is a material-transition projection the
	// Zone Controller has always written. Asserting on resourceVersion would fail on
	// that and would be measuring someone else's field.
	//
	// What FZ-R5 owns is that the restriction projection and its two conditions are
	// byte-identical across the telemetry path — including LastTransitionTime, which
	// would move if a condition had been re-upserted with a flipped status.
	afterTick := e.get("bay")
	if !reflect.DeepEqual(before.Status.Restrictions, afterTick.Status.Restrictions) {
		t.Errorf("status.restrictions changed across telemetry-driven reconciles:\n before %+v\n after  %+v\n"+
			"FZ-R5: restriction handling adds NO new per-tick status write anywhere",
			before.Status.Restrictions, afterTick.Status.Restrictions)
	}
	for _, ct := range []string{fleetv1.ZoneCondRestrictionsResolved, fleetv1.ZoneCondRestrictionsConsistent} {
		b, a := conditionOf(before, ct), conditionOf(afterTick, ct)
		if b == nil || a == nil || !reflect.DeepEqual(*b, *a) {
			t.Errorf("condition %s changed across telemetry-driven reconciles:\n before %+v\n after  %+v", ct, b, a)
		}
	}

	if afterTicks != afterFirst {
		t.Fatalf("the projection was recomputed %d extra time(s) across 20 position frames and "+
			"5 telemetry-driven reconciles (%d -> %d).\n"+
			"FZ-R5: status.restrictions is a projection of spec and the zone tree; it MUST be "+
			"recomputed on a spec or topology change and MUST NOT be recomputed on a telemetry "+
			"tick. This is RA-1, the most-enforced rule in this project",
			afterTicks-afterFirst, afterFirst, afterTicks)
	}

	// The guard must not be a permanent latch: a real spec change MUST recompute, or
	// the projection silently goes stale and FZ-R5's first clause is broken instead.
	cur := e.get("site")
	cur.Spec.Restrictions.Operating = append(cur.Spec.Restrictions.Operating,
		fleetv1.ZoneOperatingRestriction{Subject: "microphone", Mode: fleetv1.ZoneRestrictionDenied})
	if err := envK8s.Update(context.Background(), cur); err != nil {
		t.Fatalf("updating site restrictions: %v", err)
	}
	e.reconcile("bay")
	if e.zc.RestrictionRecomputes() <= afterTicks {
		t.Error("an ancestor's spec change did NOT recompute the projection. FZ-R5 requires " +
			"recomputation on a spec or topology change; a memo that never invalidates is a " +
			"stale projection, which fails in the permissive direction")
	}
	after := e.get("bay")
	if _, ok := effBySubject(after.Status.Restrictions.Effective)["Operating/microphone"]; !ok {
		t.Error("the ancestor's new microphone denial did not reach the descendant's projection")
	}
}
