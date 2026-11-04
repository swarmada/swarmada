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

package zonegeometry

import (
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func TestStateOf(t *testing.T) {
	square := []fleetv1.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}
	bounds := func(f *fleetv1.CoordinateFrame) *fleetv1.PhysicalBounds {
		return &fleetv1.PhysicalBounds{Polygon: square, Frame: f}
	}
	cases := []struct {
		name      string
		spec      fleetv1.FleetZoneSpec
		wantState fleetv1.ZoneGeometryState
		wantFrame string
	}{
		{"no bounds", fleetv1.FleetZoneSpec{}, fleetv1.ZoneGeometryAbsent, ""},
		{"empty polygon", fleetv1.FleetZoneSpec{PhysicalBounds: &fleetv1.PhysicalBounds{}}, fleetv1.ZoneGeometryAbsent, ""},
		{"no frame reads site-aligned", fleetv1.FleetZoneSpec{PhysicalBounds: bounds(nil)}, fleetv1.ZoneGeometryContainment, ""},
		{"site-aligned", fleetv1.FleetZoneSpec{PhysicalBounds: bounds(&fleetv1.CoordinateFrame{ID: "site", Alignment: fleetv1.FrameSiteAligned})},
			fleetv1.ZoneGeometryContainment, "site"},
		{"frame-local", fleetv1.FleetZoneSpec{PhysicalBounds: bounds(&fleetv1.CoordinateFrame{ID: "map-3", Alignment: fleetv1.FrameFrameLocal})},
			fleetv1.ZoneGeometryFrameLocal, "map-3"},
		{"composite with a site-aligned polygon", fleetv1.FleetZoneSpec{MemberZones: []string{"room-a"},
			PhysicalBounds: bounds(&fleetv1.CoordinateFrame{ID: "site", Alignment: fleetv1.FrameSiteAligned})},
			fleetv1.ZoneGeometryAbsent, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, frame := StateOf(&tc.spec)
			if state != tc.wantState || frame != tc.wantFrame {
				t.Fatalf("StateOf = (%s, %q); want (%s, %q)", state, frame, tc.wantState, tc.wantFrame)
			}
		})
	}
}
