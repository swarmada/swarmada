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

// enterDetail selects robot-3 and drills into the full-screen detail view.
func enterDetail(t *testing.T, height int) Model {
	t.Helper()
	m := step(New(k8sclient.NewStaticStore(sampleFleet())), tea.WindowSizeMsg{Width: 120, Height: height})
	m = step(m, key("down")) // robot-3
	m = step(m, key("enter"))
	if m.mode != modeDetail {
		t.Fatalf("expected detail mode")
	}
	return m
}

func TestDetailShowsAllSections(t *testing.T) {
	// Tall enough that everything is visible without scrolling.
	out := plain(enterDetail(t, 100).View())
	for _, want := range []string{
		"floor=2",            // position
		"cam_front",          // capability
		"camera",             // hardware
		"haul-8846",          // current action
		"Health",             // health section
		"camera faulted",     // health message
		"firmware  2.3.1",    // firmware
		"Conditions",         // conditions section
		"CapabilityDegraded", // condition reason
		"Recent events (2)",  // events header + count
		"CameraFault",        // event reason
		"×2",                 // event count badge
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail view missing %q in:\n%s", want, out)
		}
	}
}

func TestDetailScrolling(t *testing.T) {
	// Short viewport forces scrolling; the top of the body shows first, the
	// events (appended last) are off-screen until we scroll down.
	m := enterDetail(t, 12)
	top := plain(m.View())
	if !strings.Contains(top, "robot-3") {
		t.Fatalf("top of detail should show the header:\n%s", top)
	}
	if strings.Contains(top, "CameraFault") {
		t.Fatalf("events should be below the fold at the top:\n%s", top)
	}

	// End jumps to the bottom, revealing the events.
	m = step(m, key("end"))
	bottom := plain(m.View())
	if !strings.Contains(bottom, "CameraFault") {
		t.Fatalf("End should scroll to reveal events:\n%s", bottom)
	}

	// Home returns to the top.
	m = step(m, key("home"))
	if m.detailScroll != 0 {
		t.Fatalf("Home should reset scroll to 0, got %d", m.detailScroll)
	}

	// Scroll offset never exceeds the max or goes negative.
	for i := 0; i < 100; i++ {
		m = step(m, key("down"))
	}
	if m.detailScroll < 0 {
		t.Fatalf("scroll went negative: %d", m.detailScroll)
	}
	afterDown := m.detailScroll
	m = step(m, key("down"))
	if m.detailScroll != afterDown {
		t.Fatalf("scroll should clamp at max, moved from %d to %d", afterDown, m.detailScroll)
	}
}

func TestDetailScrollResetOnReentry(t *testing.T) {
	m := enterDetail(t, 12)
	m = step(m, key("end"))
	if m.detailScroll == 0 {
		t.Fatalf("precondition: expected non-zero scroll after End")
	}
	m = step(m, key("esc"))   // back to split
	m = step(m, key("enter")) // re-enter detail
	if m.detailScroll != 0 {
		t.Fatalf("scroll should reset to 0 on re-entry, got %d", m.detailScroll)
	}
}

func TestListArrowsStillMoveCursor(t *testing.T) {
	// Regression: scroll handling must not steal arrows from the list view.
	m := step(New(k8sclient.NewStaticStore(sampleFleet())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("down"))
	if m.tbl.Cursor() != 1 {
		t.Fatalf("down in list should still move the cursor, got %d", m.tbl.Cursor())
	}
}
