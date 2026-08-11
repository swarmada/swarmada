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
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swarmada/swarmtop/internal/k8sclient"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plain(s string) string { return ansi.ReplaceAllString(s, "") }

func key(s string) tea.KeyMsg {
	special := map[string]tea.KeyType{
		"up":     tea.KeyUp,
		"down":   tea.KeyDown,
		"enter":  tea.KeyEnter,
		"esc":    tea.KeyEsc,
		"pgup":   tea.KeyPgUp,
		"pgdown": tea.KeyPgDown,
		"home":   tea.KeyHome,
		"end":    tea.KeyEnd,
	}
	if t, ok := special[s]; ok {
		return tea.KeyMsg{Type: t}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func step(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	return next.(Model)
}

func b32(v int32) *int32 { return &v }

func sampleFleet() k8sclient.Fleet {
	return k8sclient.Fleet{
		SnapshotAt: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		Robots: []k8sclient.RobotView{
			{Name: "robot-1", Phase: "Idle", Estop: "Normal", BatteryPercent: b32(87), CurrentZone: "A",
				Caps: k8sclient.CapSummary{Active: 3, Total: 3}},
			{Name: "robot-3", Phase: "InProgress", Estop: "Normal", BatteryPercent: b32(23), CurrentZone: "B",
				AssignedAction: "haul-8846",
				Caps:           k8sclient.CapSummary{Active: 2, Total: 3, FirstProblem: "cam_front", FirstProblemState: "Degraded"},
				Capabilities: []k8sclient.CapabilityView{
					{Name: "lift", Status: "Active"},
					{Name: "cam_front", Status: "Degraded", Reason: "hardware fault"},
				},
				Hardware:        []k8sclient.HardwareView{{Name: "camera", Status: "Failed"}},
				HasPosition:     true,
				Position:        k8sclient.PositionView{X: 14.2, Y: 8.7, Floor: b32(2)},
				HealthStatus:    "Degraded",
				HealthMessage:   "camera faulted",
				FirmwareVersion: "2.3.1",
				Conditions: []k8sclient.ConditionView{
					{Type: "Ready", Status: "False", Reason: "CapabilityDegraded", Message: "cam_front degraded"},
				},
			},
		},
		Actions: []k8sclient.FleetActionView{
			{Name: "haul-8846", Phase: "InProgress", AssignedRobot: "robot-3", Priority: "High", ProgressPct: 62},
		},
		EventsByRobot: map[string][]k8sclient.EventView{
			"robot-3": {
				{Time: time.Date(2026, 7, 11, 11, 59, 55, 0, time.UTC), Type: "Warning", Reason: "CameraFault", Message: "depth frame rate below threshold", Count: 2},
				{Time: time.Date(2026, 7, 11, 11, 59, 50, 0, time.UTC), Type: "Normal", Reason: "ActionAssigned", Message: "assigned haul-8846"},
			},
		},
	}
}

func newTestModel() Model {
	m := New(k8sclient.NewStaticStore(sampleFleet()))
	return step(m, tea.WindowSizeMsg{Width: 120, Height: 40})
}

func TestListView_ShowsRobots(t *testing.T) {
	out := plain(newTestModel().View())
	for _, want := range []string{"robot-1", "robot-3", "87%", "23%", "2/3 cam_front"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list view missing %q in:\n%s", want, out)
		}
	}
}

func TestCursorMovesAndClamps(t *testing.T) {
	m := newTestModel()
	if m.tbl.Cursor() != 0 {
		t.Fatalf("cursor should start at 0")
	}
	m = step(m, key("down"))
	if m.tbl.Cursor() != 1 {
		t.Fatalf("down should move to 1, got %d", m.tbl.Cursor())
	}
	m = step(m, key("down")) // already at last row
	if m.tbl.Cursor() != 1 {
		t.Fatalf("cursor must not exceed last row, got %d", m.tbl.Cursor())
	}
	m = step(m, key("up"))
	m = step(m, key("up")) // already at top
	if m.tbl.Cursor() != 0 {
		t.Fatalf("cursor must not go below 0, got %d", m.tbl.Cursor())
	}
}

func TestSplitToggleAndDetailDrill(t *testing.T) {
	m := newTestModel()
	m = step(m, key("down")) // select robot-3

	m = step(m, key("s"))
	if m.mode != modeSplit {
		t.Fatalf("s should enter split mode")
	}
	out := plain(m.View())
	// Split pane shows the selected robot's detail alongside the list.
	if !strings.Contains(out, "Capabilities") || !strings.Contains(out, "cam_front") {
		t.Fatalf("split detail pane missing capability breakdown:\n%s", out)
	}

	m = step(m, key("enter"))
	if m.mode != modeDetail {
		t.Fatalf("enter should drill into detail")
	}
	out = plain(m.View())
	if !strings.Contains(out, "floor=2") || !strings.Contains(out, "haul-8846") {
		t.Fatalf("detail screen missing position/action:\n%s", out)
	}

	m = step(m, key("esc"))
	if m.mode != modeSplit {
		t.Fatalf("esc from detail should return to split, got %v", m.mode)
	}

	m = step(m, key("s"))
	if m.mode != modeList {
		t.Fatalf("s from split should return to list")
	}
}

func TestChangedNudgeRefreshesFleet(t *testing.T) {
	store := k8sclient.NewStaticStore(sampleFleet())
	m := step(New(store), tea.WindowSizeMsg{Width: 120, Height: 40})

	// Simulate a live update: fewer robots. The UI re-snapshots on changedMsg.
	store.Set(k8sclient.Fleet{
		SnapshotAt: time.Now(),
		Robots:     []k8sclient.RobotView{{Name: "robot-1", Phase: "Idle", BatteryPercent: b32(90)}},
	})
	m = step(m, key("down")) // move cursor to index 1 first
	m = step(m, changedMsg{})
	// changedMsg returns a snapshot command; apply it.
	m = step(m, fleetMsg{store.Snapshot()})

	if got := len(m.fleet.Robots); got != 1 {
		t.Fatalf("expected refreshed fleet of 1 robot, got %d", got)
	}
	if m.tbl.Cursor() != 0 {
		t.Fatalf("cursor should clamp to 0 after fleet shrank, got %d", m.tbl.Cursor())
	}
}

func TestEmptyFleetMessage(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(k8sclient.Fleet{})), tea.WindowSizeMsg{Width: 80, Height: 24})
	if !strings.Contains(plain(m.View()), "no robots") {
		t.Fatalf("empty fleet should show a hint:\n%s", plain(m.View()))
	}
}

var _ tea.Model = Model{}
