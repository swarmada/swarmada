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

package components

import (
	"regexp"
	"testing"
	"time"

	"github.com/swarmada/swarmtop/internal/format"
	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// These tests exercise the view-model directly — no tea.Program, no terminal —
// which is the whole point of the components layer.

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plain(s string) string { return ansi.ReplaceAllString(s, "") }

func i32(v int32) *int32 { return &v }

func sampleFleet(now time.Time) k8sclient.Fleet {
	return k8sclient.Fleet{
		SnapshotAt: now,
		Robots: []k8sclient.RobotView{
			{
				Name: "robot-1", Phase: "Idle", BatteryPercent: i32(87), CurrentZone: "A",
				Caps:        k8sclient.CapSummary{Active: 3, Total: 3},
				AdapterName: "vda5050-a",
			},
			{
				Name: "robot-3", Phase: "InProgress", BatteryPercent: i32(23), CurrentZone: "B",
				ZoneDrift:      true,
				AssignedAction: "haul-8846",
				Caps:           k8sclient.CapSummary{Active: 2, Total: 3, FirstProblem: "camera_front", FirstProblemState: "Degraded"},
				AdapterName:    "sim-fleet-adapter",
			},
		},
		EventsByRobot: map[string][]k8sclient.EventView{
			"robot-3": {{Type: "Warning", Reason: "CameraFault"}},
		},
	}
}

func TestRobotColumns(t *testing.T) {
	cols := RobotColumns()
	want := []string{"NAME", "PHASE", "ESTOP", "BATT", "ZONE", "CAPABILITIES", "ADAPTER", "EVENTS", "ACTION"}
	if len(cols) != len(want) {
		t.Fatalf("got %d columns, want %d", len(cols), len(want))
	}
	for i, w := range want {
		if cols[i].Title != w {
			t.Fatalf("column %d: got %q want %q", i, cols[i].Title, w)
		}
	}
}

func TestRobotRows_Cells(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	f := sampleFleet(now)
	rows := RobotRows(f, now)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	r1 := rows[0]
	if r1[ColName] != "robot-1" || plain(r1[ColBattery]) != "87%" || plain(r1[ColCaps]) != "3" {
		t.Fatalf("robot-1 cells: name=%q batt=%q caps=%q", r1[ColName], plain(r1[ColBattery]), plain(r1[ColCaps]))
	}
	if plain(r1[ColAdapter]) != "vda5050-a" {
		t.Fatalf("robot-1 adapter name: got %q want vda5050-a", plain(r1[ColAdapter]))
	}
	if r1[ColEvents] != "" {
		t.Fatalf("robot-1 should have no event badge, got %q", plain(r1[ColEvents]))
	}

	r3 := rows[1]
	if plain(r3[ColCaps]) != "2/3 camera_front" {
		t.Fatalf("robot-3 caps summary: got %q", plain(r3[ColCaps]))
	}
	if plain(r3[ColZone]) != "B*" {
		t.Fatalf("robot-3 zone should mark drift: got %q", plain(r3[ColZone]))
	}
	if plain(r3[ColEvents]) != "1W" {
		t.Fatalf("robot-3 event badge: got %q", plain(r3[ColEvents]))
	}
	if plain(r3[ColAction]) != "haul-8846" {
		t.Fatalf("robot-3 action: got %q", plain(r3[ColAction]))
	}
}

func TestRobotRows_BatteryColorThresholds(t *testing.T) {
	now := time.Now()
	// green >50, yellow 20–50, red <20 — assert via the ANSI code family the
	// cell is wrapped in (green 42/28, yellow 214/130, red 203/160).
	cases := []struct {
		pct     int32
		wantLvl format.Level
	}{
		{87, format.LevelGood},
		{50, format.LevelWarn},
		{20, format.LevelWarn},
		{19, format.LevelBad},
	}
	for _, tc := range cases {
		f := k8sclient.Fleet{Robots: []k8sclient.RobotView{{Name: "r", BatteryPercent: i32(tc.pct)}}}
		cell := RobotRows(f, now)[0][ColBattery]
		want := Colorize(plain(cell), tc.wantLvl)
		if cell != want {
			t.Fatalf("battery %d%%: color mismatch\n got %q\nwant %q", tc.pct, cell, want)
		}
	}
}

func TestRobotRow_UnreportedBatteryAndAdapter(t *testing.T) {
	now := time.Now()
	f := k8sclient.Fleet{Robots: []k8sclient.RobotView{{Name: "off", Phase: "Offline"}}}
	row := RobotRow(f, f.Robots[0], now)
	if plain(row[ColBattery]) != "--" {
		t.Fatalf("unreported battery should be --, got %q", plain(row[ColBattery]))
	}
	if plain(row[ColAdapter]) != "—" {
		t.Fatalf("an unset adapter should be —, got %q", plain(row[ColAdapter]))
	}
}
