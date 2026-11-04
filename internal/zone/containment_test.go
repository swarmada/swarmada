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

package zone

import (
	"reflect"
	"testing"
)

// CONTAINMENT IS A SET; `currentZone` IS A CHOICE FROM IT.
//
// DeriveCurrentZone answers "where should this robot report it is" and arbitrates
// overlapping polygons to one winner by nearest centroid. That tie-break was written
// for position REPORTING and, until now, was also deciding what BINDS the robot —
// permissively, by discarding the loser, where FZ-R2 (denial absorbs) and FZ-R6 (fail
// closed) both say union.
//
// DeriveContainingZones answers the other question and only the other question: every
// zone whose polygon contains the robot. Nothing about DeriveCurrentZone changes.

func square(x, y, side float64) []Point {
	return []Point{{x, y}, {x + side, y}, {x + side, y + side}, {x, y + side}}
}

// ADR-0059's worked case: two overlapping leaves, the robot inside both.
func overlappingPair() []Candidate {
	return []Candidate{
		// ward-a spans (0,0)-(10,10); its centroid is (5,5).
		{Name: "ward-a", Floor: 0, Polygon: square(0, 0, 10), IsLeaf: true, Depth: 1},
		// bay-1 spans (8,8)-(12,12); its centroid is (10,10) — nearer to (9,9).
		{Name: "bay-1", Floor: 0, Polygon: square(8, 8, 4), IsLeaf: true, Depth: 1},
	}
}

func TestDeriveContainingZones_ReturnsEveryZoneWhosePolygonContainsThePoint(t *testing.T) {
	pos := Position{X: 9, Y: 9, Floor: 0}
	got := DeriveContainingZones(pos, overlappingPair())
	want := []string{"bay-1", "ward-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeriveContainingZones = %v, want %v. A robot standing inside two overlapping "+
			"polygons is inside BOTH; discarding one is what lets the loser's restrictions "+
			"escape, and the tie-break that discards it was written to answer where the robot "+
			"should REPORT it is", got, want)
	}
	// The winner of the position question is still exactly what it was. Too much reads
	// currentZone for its cardinality or its derivation to move.
	if one := DeriveCurrentZone(pos, overlappingPair()); one != "bay-1" {
		t.Fatalf("DeriveCurrentZone = %q, want bay-1 unchanged; the nearest-centroid tie-break "+
			"is still the answer to the position question", one)
	}
}

// SORTED, AND THE ORDER IS LOAD-BEARING. The set is compared for equality on every
// telemetry frame to decide whether anything changed (RA-1). Map iteration or
// candidate order would make an unchanged containment look like a transition and
// would emit on every tick.
func TestDeriveContainingZones_IsSortedSoSetEqualityIsCheap(t *testing.T) {
	cands := overlappingPair()
	forward := DeriveContainingZones(Position{X: 9, Y: 9}, cands)
	reversed := DeriveContainingZones(Position{X: 9, Y: 9},
		[]Candidate{cands[1], cands[0]})
	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("candidate order changed the result: %v vs %v. The set is compared for "+
			"equality on every frame; an order that varies with the caller's slice makes an "+
			"unchanged containment look like a transition and emits on every tick",
			forward, reversed)
	}
}

// A ZONE ON ANOTHER FLOOR NEVER CONTAINS, and a degenerate polygon never contains —
// the same two guards DeriveCurrentZone applies, because this answers the same
// containment question over the same candidates.
func TestDeriveContainingZones_SkipsOtherFloorsAndDegeneratePolygons(t *testing.T) {
	cands := []Candidate{
		{Name: "ground", Floor: 0, Polygon: square(0, 0, 10), IsLeaf: true},
		{Name: "first", Floor: 1, Polygon: square(0, 0, 10), IsLeaf: true},
		{Name: "line", Floor: 0, Polygon: []Point{{0, 0}, {10, 10}}, IsLeaf: true},
		{Name: "identity", Floor: 0, IsLeaf: true}, // ZoneGeometryAbsent: no polygon at all
	}
	got := DeriveContainingZones(Position{X: 5, Y: 5, Floor: 0}, cands)
	if !reflect.DeepEqual(got, []string{"ground"}) {
		t.Fatalf("DeriveContainingZones = %v, want [ground]. A zone with no polygon has no "+
			"containment answer — it is not contained and it is not excluded, it simply cannot "+
			"be derived, and it must not silently become a member", got)
	}
}

// A NON-LEAF THAT CONTAINS IS IN THE SET. DeriveCurrentZone prefers leaves because a
// robot reports the most specific place it is; containment has no such preference,
// and a parent that declares a restriction binds a robot standing in its child.
func TestDeriveContainingZones_IncludesAncestorsThatContain(t *testing.T) {
	cands := []Candidate{
		{Name: "site", Floor: 0, Polygon: square(0, 0, 100), IsLeaf: false, Depth: 0},
		{Name: "ward-a", Floor: 0, Polygon: square(0, 0, 10), IsLeaf: true, Depth: 1},
	}
	got := DeriveContainingZones(Position{X: 5, Y: 5}, cands)
	if !reflect.DeepEqual(got, []string{"site", "ward-a"}) {
		t.Fatalf("DeriveContainingZones = %v, want [site ward-a]", got)
	}
}

// NOTHING CONTAINS THE POINT → an EMPTY set, and nil is the empty set. The caller
// distinguishes "no zone contains it" from "containment was never derived" by the
// same means it always has: currentZone is "" and ZonePositionUnmatched fires.
func TestDeriveContainingZones_NoMatchIsEmpty(t *testing.T) {
	if got := DeriveContainingZones(Position{X: 500, Y: 500}, overlappingPair()); len(got) != 0 {
		t.Fatalf("DeriveContainingZones = %v, want empty", got)
	}
}
