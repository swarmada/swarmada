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
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// wideFleet mirrors the field widths the capture stage actually shows: a 13-character
// robot name, a 17-character adapter name and a 22-character action. sampleFleet's
// "robot-1" / "haul-8846" are short enough to fit an 82-column pane even with the bug
// present, so a regression test built on it would have passed throughout.
func wideFleet() k8sclient.Fleet {
	return k8sclient.Fleet{
		SnapshotAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
		Robots: []k8sclient.RobotView{
			{Name: "sim-robot-001", Phase: "InProgress", Estop: "Stopped", BatteryPercent: b32(99),
				CurrentZone: "warehouse-a", AdapterName: "sim-fleet-adapter",
				AssignedAction: "inspect-receiving-dock",
				Caps:           k8sclient.CapSummary{Active: 1, Total: 2, FirstProblem: "cam_front", FirstProblemState: "Degraded"}},
			{Name: "sim-robot-002", Phase: "InProgress", Estop: "Normal", BatteryPercent: b32(85),
				CurrentZone: "warehouse-a", AdapterName: "sim-fleet-adapter",
				AssignedAction: "deliver-pallet-001",
				Caps:           k8sclient.CapSummary{Active: 2, Total: 2}},
		},
		Actions: []k8sclient.FleetActionView{
			{Name: "inspect-receiving-dock", Phase: "Paused", AssignedRobot: "sim-robot-001", Priority: "Normal"},
			{Name: "deliver-pallet-001", Phase: "InProgress", AssignedRobot: "sim-robot-002", Priority: "High", ProgressPct: 40},
		},
	}
}

// No rendered row may be wider than the terminal it is rendered for.
//
// This is not a cosmetic rule. A row wider than the terminal wraps onto the next line, the
// next repaint only covers the rows the renderer believes it own, and the wrapped tail is
// stranded on screen until something else happens to overwrite it. In a recorded terminal
// session nothing ever does, so the fragments accumulate for the length of the take.
//
// Two separate defects produced exactly that, and neither was covered:
//
//   - The help footers were fixed strings rendered with a style that had no width -- 94
//     columns for robots, 95 for tasks and zones, 110 for the split detail view. Anything
//     narrower than about 105 columns wrapped them.
//   - robotColWidths shrank CAPS/ADAPTER/NAME/ZONE toward their minimums and then returned
//     whatever it had, even if that still did not fit. ACTION was exempt from shrinking
//     while being eligible for growth, so with a long adapter name and a long action the
//     row overshot an 82-column pane by a few columns.
//
// 82 is the width that matters in practice: the capture stage puts three of these panes
// side by side in a 250-column window. The narrow end of the range is deliberately absurd
// to pin the hard floor.
func TestNoRenderedRowExceedsTerminalWidth(t *testing.T) {
	widths := []int{40, 60, 66, 70, 82, 83, 100, 105, 120, 200}
	screens := []struct {
		name string
		keys []string
	}{
		{"robots", nil},
		{"tasks", []string{"t"}},
		{"adapters", []string{"a"}},
		{"zones", []string{"z"}},
		{"split", []string{"s"}},
		{"detail", []string{"enter"}},
	}

	for _, w := range widths {
		for _, sc := range screens {
			m := New(k8sclient.NewStaticStore(wideFleet()))
			m = step(m, tea.WindowSizeMsg{Width: w, Height: 40})
			for _, k := range sc.keys {
				m = step(m, key(k))
			}
			for i, line := range strings.Split(plain(m.View()), "\n") {
				if got := len([]rune(line)); got > w {
					t.Errorf("width=%d screen=%s row %d is %d columns (max %d):\n%q",
						w, sc.name, i, got, w, line)
				}
			}
		}
	}
}
