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

package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// TestZonesScreenShowsACompositeAndItsMembers pins how a composite FleetZone (one with
// spec.memberZones) reads. The control plane reports it isLeaf, because it has no parentZone
// children, and the zone view said "leaf" with nothing to show it groups other zones. An
// operator can then address work or an estop to a zone no robot is ever placed in.
func TestZonesScreenShowsACompositeAndItsMembers(t *testing.T) {
	f := fleetWithAdapters()
	f.Zones = []k8sclient.ZoneView{
		{Name: "floor", EstopStatus: "Clear", IsLeaf: true, LastEstopUnknown: true,
			MemberZones: []string{"floor-room-a", "floor-room-b"}},
		{Name: "floor-room-a", EstopStatus: "Clear", IsLeaf: true, LastEstopUnknown: true},
	}
	m := step(New(k8sclient.NewStaticStore(f)), tea.WindowSizeMsg{Width: 140, Height: 40})
	m = step(m, key("z"))
	out := plain(m.View())
	var floorRow, roomRow string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "floor-room-a"):
			roomRow = line
		case strings.HasPrefix(strings.TrimSpace(line), "floor "):
			floorRow = line
		}
	}
	if !strings.Contains(floorRow, "composite") || strings.Contains(floorRow, "leaf") {
		t.Fatalf("a zone with memberZones must read composite, not leaf:\n%s", out)
	}
	if !strings.Contains(roomRow, "leaf") {
		t.Fatalf("a plain leaf must still read leaf:\n%s", out)
	}

	m = step(m, key("enter"))
	d := plain(m.View())
	if !strings.Contains(d, "composite") || !strings.Contains(d, "floor-room-a, floor-room-b") {
		t.Fatalf("the detail view must say composite and name its members:\n%s", d)
	}
}
