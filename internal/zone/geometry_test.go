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

import "testing"

// A 120×80 rectangle at the origin (matches the spec's worked example).
var rect = []Point{{0, 0}, {120, 0}, {120, 80}, {0, 80}}

func TestPointInPolygon(t *testing.T) {
	cases := []struct {
		name string
		x, y float64
		want bool
	}{
		{"interior", 60, 40, true},
		{"far outside", 200, 200, false},
		{"outside left", -1, 40, false},
		{"on bottom edge", 60, 0, true}, // boundary counts as inside
		{"on corner", 0, 0, true},       // vertex counts as inside
		{"just inside", 0.001, 0.001, true},
		{"just outside", -0.001, 40, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PointInPolygon(tc.x, tc.y, rect); got != tc.want {
				t.Fatalf("PointInPolygon(%v,%v) = %v, want %v", tc.x, tc.y, got, tc.want)
			}
		})
	}
}

func TestPointInPolygon_DegeneratePolygon(t *testing.T) {
	if PointInPolygon(0, 0, []Point{{0, 0}, {1, 1}}) {
		t.Fatal("a <3-vertex polygon must never contain")
	}
}

func TestDeriveCurrentZone_SingleMatch(t *testing.T) {
	zones := []Candidate{{Name: "z1", Floor: 2, Polygon: rect, IsLeaf: true}}
	if got := DeriveCurrentZone(Position{X: 60, Y: 40, Floor: 2}, zones); got != "z1" {
		t.Fatalf("got %q, want z1", got)
	}
}

func TestDeriveCurrentZone_FloorMismatch(t *testing.T) {
	zones := []Candidate{{Name: "z1", Floor: 2, Polygon: rect, IsLeaf: true}}
	if got := DeriveCurrentZone(Position{X: 60, Y: 40, Floor: 1}, zones); got != "" {
		t.Fatalf("got %q, want empty (wrong floor)", got)
	}
}

func TestDeriveCurrentZone_NoMatch(t *testing.T) {
	zones := []Candidate{{Name: "z1", Floor: 2, Polygon: rect, IsLeaf: true}}
	if got := DeriveCurrentZone(Position{X: 999, Y: 999, Floor: 2}, zones); got != "" {
		t.Fatalf("got %q, want empty (no containment)", got)
	}
}

// A root polygon covering a leaf polygon: the leaf (most specific) wins.
func TestDeriveCurrentZone_PrefersLeafOverParent(t *testing.T) {
	zones := []Candidate{
		{Name: "root", Floor: 2, Polygon: rect, IsLeaf: false, Depth: 0},
		{Name: "leaf", Floor: 2, Polygon: []Point{{0, 0}, {60, 0}, {60, 80}, {0, 80}}, IsLeaf: true, Depth: 1},
	}
	if got := DeriveCurrentZone(Position{X: 30, Y: 40, Floor: 2}, zones); got != "leaf" {
		t.Fatalf("got %q, want leaf (most specific)", got)
	}
	// A point only in the root (right half) resolves to root.
	if got := DeriveCurrentZone(Position{X: 90, Y: 40, Floor: 2}, zones); got != "root" {
		t.Fatalf("got %q, want root", got)
	}
}

// Two overlapping leaves: nearest centroid wins; deterministic.
func TestDeriveCurrentZone_OverlappingLeavesByCentroid(t *testing.T) {
	left := []Point{{0, 0}, {70, 0}, {70, 80}, {0, 80}}      // centroid x=35
	right := []Point{{50, 0}, {120, 0}, {120, 80}, {50, 80}} // centroid x=85
	zones := []Candidate{
		{Name: "left", Floor: 2, Polygon: left, IsLeaf: true, Depth: 1},
		{Name: "right", Floor: 2, Polygon: right, IsLeaf: true, Depth: 1},
	}
	// x=55 is in both; closer to left's centroid (35) than right's (85).
	if got := DeriveCurrentZone(Position{X: 55, Y: 40, Floor: 2}, zones); got != "left" {
		t.Fatalf("got %q, want left (nearest centroid)", got)
	}
	// x=65 is in both; closer to right's centroid.
	if got := DeriveCurrentZone(Position{X: 65, Y: 40, Floor: 2}, zones); got != "right" {
		t.Fatalf("got %q, want right (nearest centroid)", got)
	}
}

// Idempotence: the same input twice yields the same result.
func TestDeriveCurrentZone_Idempotent(t *testing.T) {
	zones := []Candidate{
		{Name: "a", Floor: 0, Polygon: rect, IsLeaf: true, Depth: 1},
		{Name: "b", Floor: 0, Polygon: rect, IsLeaf: true, Depth: 1},
	}
	pos := Position{X: 60, Y: 40, Floor: 0}
	first := DeriveCurrentZone(pos, zones)
	for i := 0; i < 5; i++ {
		if got := DeriveCurrentZone(pos, zones); got != first {
			t.Fatalf("non-deterministic: %q vs %q", got, first)
		}
	}
	if first != "a" { // name tie-break (equal centroid + depth)
		t.Fatalf("tie-break not deterministic by name: got %q, want a", first)
	}
}
