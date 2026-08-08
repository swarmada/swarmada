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

// SelfIntersects reports whether the closed polygon (the last vertex is implicitly
// joined back to the first) is NOT simple — i.e. some pair of non-adjacent edges
// crosses or touches. A FleetZone boundary must be a simple polygon so that
// point-in-polygon containment (PointInPolygon / DeriveCurrentZone) is well defined.
//
// Adjacent edges that merely share a vertex are not counted. A polygon with fewer
// than 4 vertices cannot self-intersect, so this returns false.
func SelfIntersects(poly []Point) bool {
	n := len(poly)
	if n < 4 {
		return false
	}
	for i := 0; i < n; i++ {
		a1, a2 := poly[i], poly[(i+1)%n]
		for j := i + 1; j < n; j++ {
			if edgesAdjacent(i, j, n) {
				continue
			}
			b1, b2 := poly[j], poly[(j+1)%n]
			if segmentsIntersect(a1, a2, b1, b2) {
				return true
			}
		}
	}
	return false
}

// edgesAdjacent reports whether edges i and j share a vertex (including the
// wrap-around pair, where edge n-1 shares vertex 0 with edge 0).
func edgesAdjacent(i, j, n int) bool {
	return j == i+1 || (i == 0 && j == n-1)
}

// segmentsIntersect reports whether segments a1a2 and b1b2 intersect — a proper
// crossing, or a collinear/endpoint touch. For non-adjacent polygon edges, even a
// touch means the polygon pinches, which is not simple.
func segmentsIntersect(a1, a2, b1, b2 Point) bool {
	d1 := orientation(b1, b2, a1)
	d2 := orientation(b1, b2, a2)
	d3 := orientation(a1, a2, b1)
	d4 := orientation(a1, a2, b2)

	if ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) &&
		((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0)) {
		return true // proper crossing
	}
	// Collinear / touching endpoints.
	if d1 == 0 && onSegment(a1.X, a1.Y, b1, b2) {
		return true
	}
	if d2 == 0 && onSegment(a2.X, a2.Y, b1, b2) {
		return true
	}
	if d3 == 0 && onSegment(b1.X, b1.Y, a1, a2) {
		return true
	}
	if d4 == 0 && onSegment(b2.X, b2.Y, a1, a2) {
		return true
	}
	return false
}

// orientation returns the sign of the cross product (q-p)×(r-p): >0 counter-clockwise
// turn, <0 clockwise, 0 collinear.
func orientation(p, q, r Point) float64 {
	return (q.X-p.X)*(r.Y-p.Y) - (q.Y-p.Y)*(r.X-p.X)
}
