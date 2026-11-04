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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/zone"
)

// The exclusion shape ADR-0069 requires: three objects, not two.
//
//	floor1                    container; carries the exclusion children, robots are never here
//	├── floor1-open           the navigable remainder; robots are HERE
//	└── floor1-exclusion-*    entry: Denied
//
// The requirement stands because the leaf-set walk never puts a zone with children in
// its own leaf set: add a denied child directly under the zone robots occupy and that
// zone leaves every leaf set, so filter 2 excludes every robot on it and filter 11
// refuses the floor terminally. These tests pin both halves: the three-object shape
// dispatches, and the two-object shape still does not. If the second half ever
// passes, the walk has changed and ADR-0069's requirement should be revisited, not
// this test relaxed.

func exclusionFloor(withOpen bool) []fleetv1.FleetZone {
	zones := []fleetv1.FleetZone{
		zrZone("floor1", ""),
		zrZone("floor1-exclusion-wing", "floor1", entryDenied("floor1-exclusion-wing", "structural")),
	}
	if withOpen {
		zones = append(zones, zrZone("floor1-open", "floor1"))
	}
	return zones
}

func robotInZone(current string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns"},
		Status:     fleetv1.RobotStatus{CurrentZone: current},
	}
}

func TestExclusionShape_ThreeObjectsDispatchToTheOpenRemainder(t *testing.T) {
	zones := exclusionFloor(true)
	action := zrAction("floor1")

	if got := resolveZoneLeaves("floor1", zones); !got["floor1-open"] || !got["floor1-exclusion-wing"] || got["floor1"] {
		t.Fatalf("resolveZoneLeaves(floor1) = %v, want {floor1-open, floor1-exclusion-wing}", got)
	}

	v := evaluateZoneRestrictions(action, zones)
	if v.refuse {
		t.Fatalf("filter 11 refused the container: reason=%s msg=%s", v.reason, v.message)
	}
	if len(v.leaves) != 1 || !v.leaves["floor1-open"] {
		t.Errorf("filter 11 leaves = %v, want only floor1-open", v.leaves)
	}
	if len(v.narrowedOut) != 1 || v.narrowedOut[0] != "floor1-exclusion-wing" {
		t.Errorf("narrowedOut = %v, want [floor1-exclusion-wing]: an exclusion is removed by name, never silently", v.narrowedOut)
	}

	if ex := excludedByZone(robotInZone("floor1-open"), action, v.leaves); ex != nil {
		t.Errorf("filter 2 excluded a robot on the open remainder: %s", ex.Reason)
	}
	if ex := excludedByZone(robotInZone("floor1-exclusion-wing"), action, v.leaves); ex == nil {
		t.Error("filter 2 admitted a robot reporting the exclusion as its current zone")
	}
}

func TestExclusionShape_TwoObjectsStillRefuseTheFloor(t *testing.T) {
	zones := exclusionFloor(false)
	action := zrAction("floor1")

	if got := resolveZoneLeaves("floor1", zones); got["floor1"] {
		t.Fatalf("resolveZoneLeaves(floor1) = %v now includes floor1: the leaf-set walk changed; revisit ADR-0069's shape requirement", got)
	}
	v := evaluateZoneRestrictions(action, zones)
	if !v.refuse || !v.terminal || v.reason != fleetv1.ActionFailureZoneEntryDenied {
		t.Fatalf("two-object shape: refuse=%v terminal=%v reason=%s, want a terminal ZoneEntryDenied", v.refuse, v.terminal, v.reason)
	}
	if ex := excludedByZone(robotInZone("floor1"), action, resolveZoneLeaves("floor1", zones)); ex == nil {
		t.Error("filter 2 admitted a robot reporting the container; with no restriction at all the walk already excludes it")
	}
}

// The three-object shape leans on currentZone arbitration: floor1-open's single ring
// still encloses the exclusion, so a robot inside the exclusion is contained by both
// leaves. Through the Zone Controller's own candidate builder, it must report the
// exclusion wherever it stands inside it, including nearer the open zone's centroid.
func TestExclusionShape_CurrentZoneInsideTheExclusionIsTheExclusion(t *testing.T) {
	ring := func(x0, y0, x1, y1 float64) *fleetv1.PhysicalBounds {
		return &fleetv1.PhysicalBounds{Polygon: []fleetv1.Point{{X: x0, Y: y0}, {X: x1, Y: y0}, {X: x1, Y: y1}, {X: x0, Y: y1}}}
	}
	zones := exclusionFloor(true)
	for i := range zones {
		switch zones[i].Name {
		case "floor1", "floor1-open":
			zones[i].Spec.PhysicalBounds = ring(0, 0, 10, 10)
		case "floor1-exclusion-wing":
			zones[i].Spec.PhysicalBounds = ring(0, 0, 8, 8)
		}
	}
	childrenOf, depthOf := buildTree(zones)
	cands := candidatesFrom(zones, childrenOf, depthOf)

	for _, c := range []struct {
		x, y float64
		want string
	}{
		{1, 1, "floor1-exclusion-wing"},
		{7, 7, "floor1-exclusion-wing"}, // nearer floor1-open's centroid
		{7.9, 7.9, "floor1-exclusion-wing"},
		{9, 9, "floor1-open"},
	} {
		if got := zone.DeriveCurrentZone(zone.Position{X: c.x, Y: c.y}, cands); got != c.want {
			t.Errorf("currentZone at (%v,%v) = %q, want %q", c.x, c.y, got, c.want)
		}
	}
}
