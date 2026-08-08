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

func TestSelfIntersects(t *testing.T) {
	tests := []struct {
		name string
		poly []Point
		want bool
	}{
		{"triangle", []Point{{0, 0}, {4, 0}, {2, 3}}, false},
		{"square", []Point{{0, 0}, {4, 0}, {4, 4}, {0, 4}}, false},
		{"convex-pentagon", []Point{{0, 0}, {4, 0}, {5, 3}, {2, 5}, {-1, 3}}, false},
		{"concave-L", []Point{{0, 0}, {4, 0}, {4, 2}, {2, 2}, {2, 4}, {0, 4}}, false},
		{"bowtie", []Point{{0, 0}, {4, 4}, {4, 0}, {0, 4}}, true},        // classic figure-eight
		{"crossing-quad", []Point{{0, 0}, {4, 0}, {0, 4}, {4, 4}}, true}, // edges cross
		{"too-few-vertices", []Point{{0, 0}, {1, 0}, {0, 1}}, false},     // < 4 → cannot self-intersect
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SelfIntersects(tt.poly); got != tt.want {
				t.Errorf("SelfIntersects(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// A vertex touched by a non-adjacent edge (a pinch) is not simple.
func TestSelfIntersects_Pinch(t *testing.T) {
	// Two triangles meeting at the shared point (2,2): non-adjacent edges touch there.
	pinch := []Point{{0, 0}, {2, 2}, {4, 0}, {4, 4}, {2, 2}, {0, 4}}
	if !SelfIntersects(pinch) {
		t.Error("a polygon that pinches at a repeated vertex must be reported self-intersecting")
	}
}
