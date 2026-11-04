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

// The exclusion shape (ADR-0069): an open leaf whose single ring still encloses a
// sibling leaf that carries an entry denial. A ring cannot have the hole cut out of
// it, so both leaves contain every point of the exclusion, and nearest-centroid
// alone answers with the open zone wherever the robot stands nearer its centre.
func exclusionShape() []Candidate {
	return []Candidate{
		{Name: "floor1-open", Polygon: box(0, 0, 10, 10), IsLeaf: true, Depth: 1},
		{Name: "floor1-exclusion-wing", Polygon: box(0, 0, 8, 8), IsLeaf: true, Depth: 1, DeniesEntry: true},
		{Name: "floor1", Polygon: box(0, 0, 10, 10), IsLeaf: false, Depth: 0},
	}
}

func box(x0, y0, x1, y1 float64) []Point {
	return []Point{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}
}

func TestDeriveCurrentZone_NestedEntryDenialWinsInsideALargeExclusion(t *testing.T) {
	cases := []struct {
		name string
		pos  Position
		want string
	}{
		// Nearer the exclusion's centroid (4,4): today's rule already answers this.
		{"near the exclusion centroid", Position{X: 1, Y: 1}, "floor1-exclusion-wing"},
		// Nearer the open zone's centroid (5,5) than the exclusion's, yet inside the
		// exclusion. Distances at (7,7): 2.83 to the open zone, 4.24 to the exclusion.
		{"nearer the enclosing centroid", Position{X: 7, Y: 7}, "floor1-exclusion-wing"},
		{"just inside the exclusion edge", Position{X: 7.9, Y: 7.9}, "floor1-exclusion-wing"},
		{"on the exclusion boundary", Position{X: 8, Y: 5}, "floor1-exclusion-wing"},
		// Outside the exclusion the open zone is the only leaf, and it is the answer.
		{"on the open remainder", Position{X: 9, Y: 9}, "floor1-open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveCurrentZone(c.pos, exclusionShape()); got != c.want {
				t.Errorf("DeriveCurrentZone(%v) = %q, want %q", c.pos, got, c.want)
			}
		})
	}
}

// The preference is narrow: it answers the exclusion case only. Every other overlap
// keeps the nearest-centroid rule, so no deployment without an entry-denying nested
// leaf sees a different currentZone.
func TestDeriveCurrentZone_DenialPreferenceIsNarrow(t *testing.T) {
	pos := Position{X: 7, Y: 7} // nearer the enclosing zone's centroid in every case

	t.Run("nested leaf with no denial keeps nearest centroid", func(t *testing.T) {
		zones := exclusionShape()
		zones[1].DeniesEntry = false
		if got := DeriveCurrentZone(pos, zones); got != "floor1-open" {
			t.Errorf("got %q, want floor1-open", got)
		}
	})

	t.Run("denying leaf that does not nest keeps nearest centroid", func(t *testing.T) {
		// Two leaves that overlap without either enclosing the other.
		zones := []Candidate{
			{Name: "aisle", Polygon: box(4, 4, 10, 10), IsLeaf: true},
			{Name: "dock", Polygon: box(0, 0, 8, 8), IsLeaf: true, DeniesEntry: true},
		}
		if got := DeriveCurrentZone(pos, zones); got != "aisle" {
			t.Errorf("got %q, want aisle", got)
		}
	})

	t.Run("denial on the enclosing leaf does not pull the robot out of the enclosed one", func(t *testing.T) {
		zones := exclusionShape()
		zones[0].DeniesEntry, zones[1].DeniesEntry = true, false
		if got := DeriveCurrentZone(Position{X: 1, Y: 1}, zones); got != "floor1-exclusion-wing" {
			t.Errorf("got %q, want floor1-exclusion-wing (nearest centroid)", got)
		}
	})

	t.Run("two nested denials arbitrate between themselves by centroid", func(t *testing.T) {
		zones := append(exclusionShape(),
			Candidate{Name: "floor1-exclusion-pillar", Polygon: box(6, 6, 8, 8), IsLeaf: true, Depth: 1, DeniesEntry: true})
		if got := DeriveCurrentZone(pos, zones); got != "floor1-exclusion-pillar" {
			t.Errorf("got %q, want floor1-exclusion-pillar", got)
		}
	})

	t.Run("a denying non-leaf does not outrank a leaf", func(t *testing.T) {
		zones := exclusionShape()
		zones[1].DeniesEntry = false
		zones[2].DeniesEntry = true // the container
		if got := DeriveCurrentZone(pos, zones); got != "floor1-open" {
			t.Errorf("got %q, want floor1-open", got)
		}
	})
}
