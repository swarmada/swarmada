// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sclient

import (
	"testing"

	swarmadav1 "github.com/swarmada/swarmada/api/v1"
)

// TestMapFleetZoneCarriesMemberZones pins that a composite zone's members reach the view. The
// control plane reports a composite isLeaf (it has no parentZone children), so without
// spec.memberZones nothing in the view can tell a grouping from a room.
func TestMapFleetZoneCarriesMemberZones(t *testing.T) {
	z := &swarmadav1.FleetZone{}
	z.Name = "floor"
	z.Spec.MemberZones = []string{"floor-room-a", "floor-room-b"}
	z.Status.IsLeaf = true

	v := mapFleetZone(z)
	if len(v.MemberZones) != 2 || v.MemberZones[0] != "floor-room-a" || v.MemberZones[1] != "floor-room-b" {
		t.Fatalf("memberZones not carried into the view: %+v", v.MemberZones)
	}
	z.Spec.MemberZones[0] = "changed"
	if v.MemberZones[0] != "floor-room-a" {
		t.Fatalf("the view aliases the object's slice")
	}
}
