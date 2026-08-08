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

// Package zone implements the pure spatial-containment logic the Zone Controller
// uses to derive a robot's current zone from its position (RFC-0001 §9.1.5.5). It
// carries no Kubernetes dependency so the geometry is deterministic and trivially
// unit-testable; the controller converts FleetZone resources into [Candidate]s
// and feeds position frames in.
package zone

// Point is a 2-D coordinate in the site frame (metres from the site origin).
type Point struct {
	X float64
	Y float64
}

// Position is the robot pose used for containment: floor plus planar coordinates.
type Position struct {
	X     float64
	Y     float64
	Floor int32
}

// Candidate is a zone's derivation-relevant geometry and tree metadata. Depth is
// the zone's depth in the hierarchy (root = 0); IsLeaf is true when the zone has
// no children. Both are computed by the Zone Controller from the FleetZone tree.
type Candidate struct {
	Name    string
	Floor   int32
	Polygon []Point
	IsLeaf  bool
	Depth   int
}

// PointInPolygon reports whether (x, y) lies inside the closed polygon, using the
// even-odd ray-casting rule. A point exactly on an edge counts as inside, so the
// result is deterministic on shared boundaries (overlaps are then resolved by
// [DeriveCurrentZone]). A polygon with fewer than 3 vertices never contains.
func PointInPolygon(x, y float64, poly []Point) bool {
	n := len(poly)
	if n < 3 {
		return false
	}
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		if onSegment(x, y, poly[i], poly[j]) {
			return true // on the boundary → treated as inside (deterministic)
		}
		xi, yi := poly[i].X, poly[i].Y
		xj, yj := poly[j].X, poly[j].Y
		if (yi > y) != (yj > y) {
			xCross := (xj-xi)*(y-yi)/(yj-yi) + xi
			if x < xCross {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

// onSegment reports whether (px, py) lies on the closed segment a–b.
func onSegment(px, py float64, a, b Point) bool {
	// Collinearity via the cross product of (b-a) and (p-a).
	cross := (b.X-a.X)*(py-a.Y) - (b.Y-a.Y)*(px-a.X)
	if cross != 0 {
		return false
	}
	return px >= min(a.X, b.X) && px <= max(a.X, b.X) &&
		py >= min(a.Y, b.Y) && py <= max(a.Y, b.Y)
}

// DeriveCurrentZone implements the §9.1.5.5 containment algorithm. It returns the
// name of the zone containing pos, or "" when none matches. It is deterministic
// and idempotent: the same inputs always yield the same result.
//
// Resolution when several zones match (overlapping polygons or a boundary point):
// prefer leaf zones; among leaves pick the nearest centroid (name breaks ties);
// if no leaf matches, pick the deepest zone (name breaks ties).
func DeriveCurrentZone(pos Position, zones []Candidate) string {
	var matched []Candidate
	for _, z := range zones {
		if len(z.Polygon) < 3 || z.Floor != pos.Floor {
			continue
		}
		if PointInPolygon(pos.X, pos.Y, z.Polygon) {
			matched = append(matched, z)
		}
	}
	switch len(matched) {
	case 0:
		return ""
	case 1:
		return matched[0].Name
	}

	// Multiple matches — prefer the most specific (leaf) zone.
	leaves := matched[:0:0]
	for _, z := range matched {
		if z.IsLeaf {
			leaves = append(leaves, z)
		}
	}
	if len(leaves) > 0 {
		best := leaves[0]
		bestDist := centroidSqDist(best, pos)
		for _, z := range leaves[1:] {
			d := centroidSqDist(z, pos)
			if d < bestDist || (d == bestDist && z.Name < best.Name) {
				best, bestDist = z, d
			}
		}
		return best.Name
	}

	// No leaf matched — return the deepest zone.
	best := matched[0]
	for _, z := range matched[1:] {
		if z.Depth > best.Depth || (z.Depth == best.Depth && z.Name < best.Name) {
			best = z
		}
	}
	return best.Name
}

// centroidSqDist returns the squared distance from the polygon's vertex centroid
// to pos. Squared distance is monotonic in distance, so it orders candidates
// identically while avoiding a sqrt.
func centroidSqDist(z Candidate, pos Position) float64 {
	if len(z.Polygon) == 0 {
		return 0
	}
	var cx, cy float64
	for _, p := range z.Polygon {
		cx += p.X
		cy += p.Y
	}
	n := float64(len(z.Polygon))
	cx /= n
	cy /= n
	dx, dy := cx-pos.X, cy-pos.Y
	return dx*dx + dy*dy
}
