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
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// FZ-G5: membership in a zone whose geometry state is not Containment is declared,
// not derived. These tests cover the decision itself, the RA-1 discipline at both
// write sites, and the part of the unmatched path FZ-G5 leaves in place.

// declaredRobot is a robot declared into specZone, reporting currentZone.
func declaredRobot(name, robotID, specZone, currentZone string) *fleetv1.Robot {
	r := robotAt(name, robotID, currentZone)
	r.Spec.Zone = specZone
	return r
}

// newCountingZoneController is newZoneController with a counter on Robot status
// patches, so RA-1 is asserted on the number of writes rather than on the value
// they leave behind: an unchanged value is also what a redundant write leaves.
func newCountingZoneController(t *testing.T, objs ...client.Object) (*ZoneController, client.Client, *atomic.Int64) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	var robotPatches atomic.Int64
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if _, ok := obj.(*fleetv1.Robot); ok {
					robotPatches.Add(1)
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	return &ZoneController{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(64)}, c, &robotPatches
}

// unmatchedEvents drains the recorder and returns the ZonePositionUnmatched events.
func unmatchedEvents(z *ZoneController) []string {
	rec := z.Recorder.(*record.FakeRecorder)
	var out []string
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "ZonePositionUnmatched") {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// The declared-membership decision, over every geometry state of the robot's
// declared zone and every kind of position input. "other" is a site-aligned zone
// elsewhere on the floor, so "inside other" separates a declaration from a
// derivation: only a derivation can answer "other".
func TestPlaceRobot_DeclaredMembershipDecision(t *testing.T) {
	other := leafZoneWithBounds("other")
	other.Spec.PhysicalBounds.Polygon = []fleetv1.Point{{X: 100, Y: 100}, {X: 110, Y: 100}, {X: 110, Y: 110}, {X: 100, Y: 110}}

	declaredAs := map[fleetv1.ZoneGeometryState]*fleetv1.FleetZone{
		fleetv1.ZoneGeometryAbsent:      zoneObj("declared", ""),
		fleetv1.ZoneGeometryFrameLocal:  framedZone("declared", "amr-07/map", fleetv1.FrameFrameLocal),
		fleetv1.ZoneGeometryContainment: framedZone("declared", "site", fleetv1.FrameSiteAligned),
	}

	positions := []struct {
		name string
		pos  *zone.Position
	}{
		{"no position", nil},
		{"inside the declared zone's vertices", &zone.Position{X: 5, Y: 5}},
		{"inside another site-aligned zone", &zone.Position{X: 105, Y: 105}},
		{"inside no polygon", &zone.Position{X: 500, Y: 500}},
	}

	// want[state][position] = (currentZone, declared)
	type outcome struct {
		zone     string
		declared bool
	}
	want := map[fleetv1.ZoneGeometryState][]outcome{
		// No polygon: the declaration is the only authority, whatever the input.
		fleetv1.ZoneGeometryAbsent: {{"declared", true}, {"declared", true}, {"declared", true}, {"declared", true}},
		// A frame-local polygon is never compared with a position (FZ-G3), even one
		// that falls inside its vertices numerically.
		fleetv1.ZoneGeometryFrameLocal: {{"declared", true}, {"declared", true}, {"declared", true}, {"declared", true}},
		// Usable geometry: derivation governs, and an unmatched position stays "".
		fleetv1.ZoneGeometryContainment: {{"", false}, {"declared", false}, {"other", false}, {"", false}},
	}

	for state, declaredZone := range declaredAs {
		zones := []fleetv1.FleetZone{*declaredZone, *other}
		childrenOf, depthOf := buildTree(zones)
		cands := candidatesFrom(zones, childrenOf, depthOf)
		declaredOnly := declaredOnlyFrom(zones)

		for i, p := range positions {
			t.Run(string(state)+"/"+p.name, func(t *testing.T) {
				got, declared := placeRobot("declared", declaredOnly, p.pos, cands)
				if w := want[state][i]; got != w.zone || declared != w.declared {
					t.Errorf("placeRobot = (%q, declared=%v), want (%q, declared=%v)", got, declared, w.zone, w.declared)
				}
			})
		}
	}

	// A robot with no declared zone, or one naming a zone that does not exist, is
	// not covered by FZ-G5 and is derived like any other.
	zones := []fleetv1.FleetZone{*zoneObj("identity-only", ""), *other}
	childrenOf, depthOf := buildTree(zones)
	cands := candidatesFrom(zones, childrenOf, depthOf)
	declaredOnly := declaredOnlyFrom(zones)
	for _, spec := range []string{"", "no-such-zone"} {
		if got, declared := placeRobot(spec, declaredOnly, &zone.Position{X: 500, Y: 500}, cands); got != "" || declared {
			t.Errorf("spec.zone %q: placeRobot = (%q, declared=%v), want (\"\", false)", spec, got, declared)
		}
	}
}

// RA-1 at both write sites. The reconcile writes the declared placement once; a
// second reconcile writes nothing; two position frames from the placed robot write
// nothing and emit no ZonePositionUnmatched.
func TestZoneDeclaredMembership_WritesOnceAtEachSite(t *testing.T) {
	z, c, patches := newCountingZoneController(t,
		zoneObj("dock", ""),
		declaredRobot("r1", "rid1", "dock", ""),
		declaredRobot("r2", "rid2", "elsewhere", ""),
	)

	reconcileZone(t, z, "dock")
	if n := patches.Load(); n != 1 {
		t.Fatalf("first reconcile made %d robot status patches, want 1 (r1 only)", n)
	}
	r1 := getRobot(t, c, "r1", actionNS)
	if r1.Status.CurrentZone != "dock" || r1.Status.SpecZoneMatchesCurrent == nil || !*r1.Status.SpecZoneMatchesCurrent {
		t.Fatalf("r1 status = currentZone %q, specZoneMatchesCurrent %v; want dock, true",
			r1.Status.CurrentZone, r1.Status.SpecZoneMatchesCurrent)
	}
	if got := getRobot(t, c, "r2", actionNS).Status.CurrentZone; got != "" {
		t.Fatalf("r2 declares another zone and was placed in %q", got)
	}

	reconcileZone(t, z, "dock")
	if n := patches.Load(); n != 1 {
		t.Fatalf("second reconcile of an unchanged namespace patched robot status (%d patches total, want 1)", n)
	}

	// A frame inside no polygon, twice. Neither may move the declared value.
	frame := telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 500, Y: 500}}
	z.ObservePosition(context.Background(), frame)
	z.ObservePosition(context.Background(), frame)
	if n := patches.Load(); n != 1 {
		t.Fatalf("position frames from a declared robot patched robot status (%d patches total, want 1)", n)
	}
	if ev := unmatchedEvents(z); len(ev) != 0 {
		t.Fatalf("declared robot got ZonePositionUnmatched: %v", ev)
	}
}

// The frame site places a declared robot on its own, for a frame that arrives
// before the zone reconcile has written the placement.
func TestZoneDeclaredMembership_FramePlacesDeclaredRobot(t *testing.T) {
	z, c, patches := newCountingZoneController(t, declaredRobot("r1", "rid1", "dock", ""))
	zones := []fleetv1.FleetZone{*zoneObj("dock", "")}
	childrenOf, depthOf := buildTree(zones)
	z.setZoneIndex(actionNS, candidatesFrom(zones, childrenOf, depthOf), declaredOnlyFrom(zones))

	z.ObservePosition(context.Background(), telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 500, Y: 500}})

	got := getRobot(t, c, "r1", actionNS)
	if got.Status.CurrentZone != "dock" || got.Status.SpecZoneMatchesCurrent == nil || !*got.Status.SpecZoneMatchesCurrent {
		t.Fatalf("status = currentZone %q, specZoneMatchesCurrent %v; want dock, true",
			got.Status.CurrentZone, got.Status.SpecZoneMatchesCurrent)
	}
	if n := patches.Load(); n != 1 {
		t.Fatalf("%d robot status patches, want 1", n)
	}
	if ev := unmatchedEvents(z); len(ev) != 0 {
		t.Fatalf("declared robot got ZonePositionUnmatched: %v", ev)
	}
}

// What FZ-G5 must NOT change. A robot whose declared zone HAS usable geometry and
// whose position matches no polygon is still cleared, still warned about, and still
// withheld by candidate filter 2. A short-circuit on spec.zone that ignored the
// geometry state would place this robot in "bay" and fail all three assertions.
func TestZoneDeclaredMembership_ZoneWithGeometryStillUnmatches(t *testing.T) {
	z, c, patches := newCountingZoneController(t,
		leafZoneWithBounds("bay"),
		declaredRobot("r1", "rid1", "bay", "bay"),
	)
	reconcileZone(t, z, "bay")
	if n := patches.Load(); n != 0 {
		t.Fatalf("reconcile of a zone with usable geometry patched robot status %d times, want 0", n)
	}

	z.ObservePosition(context.Background(), telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 500, Y: 500}})

	robot := getRobot(t, c, "r1", actionNS)
	if robot.Status.CurrentZone != "" {
		t.Fatalf("currentZone = %q, want \"\": the declared zone has a polygon and the position is outside it",
			robot.Status.CurrentZone)
	}
	if ev := unmatchedEvents(z); len(ev) != 1 {
		t.Fatalf("got %d ZonePositionUnmatched events, want 1", len(ev))
	}
	action := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{Zone: "bay"}}
	if ex := excludedByZone(robot, action, map[string]bool{"bay": true}); ex == nil {
		t.Fatal("candidate filter 2 admitted an unmatched robot; it must withhold it")
	}
}

// A robot re-homed out of a zone with no usable geometry is withdrawn from it by
// that zone's reconcile, with no position frame. Nothing else could move it: the zone
// has no polygon to derive against, and the declaration now names somewhere else.
// Without the withdrawal, candidate filter 2 would keep giving it the old zone's work.
func TestZoneDeclaredMembership_RehomedRobotIsWithdrawn(t *testing.T) {
	z, c, patches := newCountingZoneController(t,
		zoneObj("dock", ""),
		zoneObj("dock-2", ""),
		leafZoneWithBounds("bay"),
		// Re-homed to a zone with a polygon: only a position frame can place it now.
		declaredRobot("to-bay", "rid1", "bay", "dock"),
		// Re-homed to another identity-only zone: its new declaration places it.
		declaredRobot("to-dock-2", "rid2", "dock-2", "dock"),
		// Still declared here and already placed: untouched.
		declaredRobot("stays", "rid3", "dock", "dock"),
	)
	stays := getRobot(t, c, "stays", actionNS)
	matched := true
	stays.Status.SpecZoneMatchesCurrent = &matched
	if err := c.Status().Update(context.Background(), stays); err != nil {
		t.Fatalf("seed stays: %v", err)
	}

	reconcileZone(t, z, "dock")
	if n := patches.Load(); n != 2 {
		t.Fatalf("reconcile made %d robot status patches, want 2 (the two re-homed robots)", n)
	}
	for _, tc := range []struct {
		robot     string
		wantZone  string
		wantMatch bool
	}{
		{"to-bay", "", false},
		{"to-dock-2", "dock-2", true},
		{"stays", "dock", true},
	} {
		r := getRobot(t, c, tc.robot, actionNS)
		m := r.Status.SpecZoneMatchesCurrent
		if r.Status.CurrentZone != tc.wantZone || m == nil || *m != tc.wantMatch {
			t.Errorf("%s: currentZone %q, specZoneMatchesCurrent %v; want %q, %v",
				tc.robot, r.Status.CurrentZone, m, tc.wantZone, tc.wantMatch)
		}
	}
	action := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{Zone: "dock"}}
	if ex := excludedByZone(getRobot(t, c, "to-bay", actionNS), action, map[string]bool{"dock": true}); ex == nil {
		t.Error("candidate filter 2 admitted a robot re-homed out of the action's zone")
	}

	reconcileZone(t, z, "dock")
	reconcileZone(t, z, "dock-2")
	if n := patches.Load(); n != 2 {
		t.Fatalf("follow-up reconciles patched robot status (%d patches total, want 2)", n)
	}
}

// The Robot watch admits a spec.zone change, so re-homing a robot into a zone with
// no usable geometry enqueues the zone reconcile that places it.
func TestRobotZoneChangedPredicate(t *testing.T) {
	base := declaredRobot("r1", "rid1", "dock", "dock")
	for _, tc := range []struct {
		name   string
		mutate func(*fleetv1.Robot)
		want   bool
	}{
		{"spec.zone changed", func(r *fleetv1.Robot) { r.Spec.Zone = "dock-2" }, true},
		{"status.currentZone changed", func(r *fleetv1.Robot) { r.Status.CurrentZone = "" }, true},
		{"unrelated change", func(r *fleetv1.Robot) { r.Labels = map[string]string{"a": "b"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := base.DeepCopy()
			tc.mutate(updated)
			got := robotZoneChanged.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: updated})
			if got != tc.want {
				t.Errorf("robotZoneChanged.Update = %v, want %v", got, tc.want)
			}
		})
	}
}
