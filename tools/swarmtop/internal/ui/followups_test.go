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

func TestListWarningBadge(t *testing.T) {
	// robot-3 has one Warning event in the sample fleet.
	out := plain(newTestModel().View())
	if !strings.Contains(out, "1W") {
		t.Fatalf("list should show a warning badge for robot-3:\n%s", out)
	}
}

func TestListWarningBadge_NoneWhenNoWarnings(t *testing.T) {
	f := sampleFleet()
	f.EventsByRobot = nil // strip events
	out := plain(step(New(k8sclient.NewStaticStore(f)), tea.WindowSizeMsg{Width: 120, Height: 40}).View())
	if strings.Contains(out, "1W") || strings.Contains(out, "2W") {
		t.Fatalf("no warning badge expected when there are no events:\n%s", out)
	}
}

func TestSplitPaneScrolls(t *testing.T) {
	// Short viewport so the right pane overflows and can scroll. Start on
	// robot-1 (index 0) so a later 'down' actually moves the cursor.
	m := step(New(k8sclient.NewStaticStore(sampleFleet())), tea.WindowSizeMsg{Width: 120, Height: 12})
	m = step(m, key("down")) // -> robot-3 (has the long detail)
	m = step(m, key("up"))   // -> robot-1
	m = step(m, key("s"))    // split
	if m.mode != modeSplit {
		t.Fatalf("expected split mode")
	}

	// Select robot-3 so the pane has enough content to scroll.
	m = step(m, key("down"))
	if m.splitScroll != 0 {
		t.Fatalf("moving the cursor should reset split scroll, got %d", m.splitScroll)
	}

	m = step(m, key("pgdown"))
	if m.splitScroll <= 0 {
		t.Fatalf("PgDn should scroll the split detail pane down, got %d", m.splitScroll)
	}
	scrolled := m.splitScroll

	// Arrows still move the list cursor — and reset the pane scroll.
	m = step(m, key("up"))
	if m.tbl.Cursor() != 0 {
		t.Fatalf("up should move the list cursor in split mode, got %d", m.tbl.Cursor())
	}
	if m.splitScroll != 0 {
		t.Fatalf("cursor move should reset split scroll (was %d)", scrolled)
	}
}

func TestSplitScrollClamps(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(sampleFleet())), tea.WindowSizeMsg{Width: 120, Height: 12})
	m = step(m, key("down")) // robot-3
	m = step(m, key("s"))

	for i := 0; i < 50; i++ {
		m = step(m, key("pgdown"))
	}
	atMax := m.splitScroll
	m = step(m, key("pgdown"))
	if m.splitScroll != atMax {
		t.Fatalf("split scroll should clamp at max, moved %d -> %d", atMax, m.splitScroll)
	}
	m = step(m, key("home"))
	if m.splitScroll != 0 {
		t.Fatalf("home should return split pane to top, got %d", m.splitScroll)
	}
}
