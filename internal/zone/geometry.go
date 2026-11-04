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
// uses to derive a robot's current zone from its position (RFC-0001 #fleetzone-zone-controller-current-zone-derivation). It
// carries no Kubernetes dependency so the geometry is deterministic and trivially
// unit-testable; the controller converts FleetZone resources into [Candidate]s
// and feeds position frames in.
package zone

import "sort"

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
	// DeniesEntry is true when the zone itself declares spec.restrictions.entry: no
	// robot may be here. DeriveCurrentZone reads it only to arbitrate nested leaves.
	DeniesEntry bool
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

// DeriveCurrentZone implements the RFC-0001 #fleetzone-zone-controller-current-zone-derivation containment algorithm. It returns the
// name of the zone containing pos, or "" when none matches. It is deterministic
// and idempotent: the same inputs always yield the same result.
//
// Resolution when several zones match (overlapping polygons or a boundary point):
// prefer leaf zones; among leaves, prefer one that carries an entry denial and lies
// inside another matched leaf (see [preferNestedDenials]); then pick the nearest
// centroid (name breaks ties); if no leaf matches, pick the deepest zone (name
// breaks ties).
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
		leaves = preferNestedDenials(leaves)
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

// preferNestedDenials narrows matched leaves to those that carry an entry denial and
// lie inside another matched leaf, when any do; otherwise it returns leaves unchanged.
//
// The exclusion shape (ADR-0069) is an open leaf whose single polygon ring still
// encloses a sibling that denies entry, because a ring cannot carry a hole. Both
// contain every point of the exclusion, and centroid distance is not a nesting test:
// inside a large exclusion the robot is usually nearer the open zone's centre, so
// nearest-centroid alone reports the permissive zone on the field candidate filter 2
// reads. The preference is deliberately narrow. It fails closed for the exclusion case
// and changes nothing for overlaps that do not nest or carry no denial, so arbitration
// between ordinary overlapping zones stays as it was.
//
// Nesting is tested by vertices: every vertex of the denying leaf lies inside or on the
// other leaf's polygon. That is exact for a convex enclosing zone and for the
// shared-edge case the exclusion pattern produces. The test runs only when a matched
// leaf carries a denial, so the common path does no extra work.
func preferNestedDenials(leaves []Candidate) []Candidate {
	var nested []Candidate
	for i, d := range leaves {
		if !d.DeniesEntry {
			continue
		}
		for j, o := range leaves {
			if i != j && polygonWithin(d.Polygon, o.Polygon) {
				nested = append(nested, d)
				break
			}
		}
	}
	if len(nested) == 0 {
		return leaves
	}
	return nested
}

// polygonWithin reports whether every vertex of inner lies inside or on outer.
func polygonWithin(inner, outer []Point) bool {
	for _, p := range inner {
		if !PointInPolygon(p.X, p.Y, outer) {
			return false
		}
	}
	return len(inner) > 0
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

// DeriveContainingZones returns the names of EVERY zone whose polygon contains pos,
// sorted by name. It is the containment SET, of which [DeriveCurrentZone] returns one
// arbitrated member (ADR-0059).
//
// # Why this exists beside DeriveCurrentZone rather than replacing it
//
// Overlapping polygons are expected and supported — a charging bay inside a warehouse
// floor, a sterile corridor crossing two wards. DeriveCurrentZone answers "where should
// this robot report it is", and its nearest-centroid tie-break was written for exactly
// that question. Until ADR-0059 it was also answering "what binds this robot", and it
// answered PERMISSIVELY: it picked one chain and discarded the other, where FZ-R2
// (denial is absorbing) and FZ-R6 (fail closed) both point at a union. A robot standing
// inside a zone that denies cameras, whose position happened to be nearer a second
// zone's centroid, was bound by neither the denial nor anything else.
//
// So the two questions are separated rather than conflated. Nothing about
// DeriveCurrentZone changes: Robot.status.currentZone stays single-valued because
// candidate filter 2, the reservation lifecycle, TDE admission, SpecZoneMatchesCurrent
// and ZonePositionUnmatched all read it and all want one answer.
//
// # Sorted, and the order is load-bearing
//
// The result is compared for EQUALITY against the last derived set on every position
// frame, to decide whether containment changed at all (RA-1). A result whose order
// varied with the caller's slice, or with map iteration, would make an unchanged
// containment look like a transition and would write status and emit a command on every
// tick — which is the single write this project prohibits outright.
//
// # A zone with no usable polygon is not contained, and is not excluded either
//
// Fewer than three vertices, or a different floor, and the zone is skipped — the same
// two guards DeriveCurrentZone applies, because this answers the same containment
// question over the same candidates. A zone that declares no polygon at all
// (ZoneGeometryAbsent) therefore never appears here, and a robot inside its declared
// region is governed by the assignment path alone. That limit is stated in ADR-0059
// §6 rather than hidden here: containment cannot be derived for a region whose shape
// the plane does not know.
func DeriveContainingZones(pos Position, zones []Candidate) []string {
	var out []string
	for _, z := range zones {
		if len(z.Polygon) < 3 || z.Floor != pos.Floor {
			continue
		}
		if PointInPolygon(pos.X, pos.Y, z.Polygon) {
			out = append(out, z.Name)
		}
	}
	sort.Strings(out)
	return out
}
