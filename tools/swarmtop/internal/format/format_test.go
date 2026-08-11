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

package format

import (
	"testing"
	"time"

	"github.com/swarmada/swarmtop/internal/k8sclient"
)

func p32(v int32) *int32 { return &v }

func TestBattery(t *testing.T) {
	cases := []struct {
		name     string
		in       *int32
		wantText string
		wantLvl  Level
	}{
		{"nil is muted dashes", nil, "--", LevelMuted},
		{"zero is red not muted", p32(0), "0%", LevelBad},
		{"just below bad", p32(19), "19%", LevelBad},
		{"boundary 20 is warn", p32(20), "20%", LevelWarn},
		{"boundary 50 is warn", p32(50), "50%", LevelWarn},
		{"just above warn", p32(51), "51%", LevelGood},
		{"full", p32(100), "100%", LevelGood},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, lvl := Battery(tc.in)
			if text != tc.wantText || lvl != tc.wantLvl {
				t.Fatalf("got %q/%v want %q/%v", text, lvl, tc.wantText, tc.wantLvl)
			}
		})
	}
}

func TestCapSummary(t *testing.T) {
	all := k8sclient.CapSummary{Active: 3, Total: 3}
	if text, lvl := CapSummary(all); text != "3" || lvl != LevelGood {
		t.Fatalf("all-active: got %q/%v", text, lvl)
	}
	degraded := k8sclient.CapSummary{Active: 2, Total: 3, FirstProblem: "cam_front", FirstProblemState: "Degraded"}
	if text, lvl := CapSummary(degraded); text != "2/3 cam_front" || lvl != LevelWarn {
		t.Fatalf("degraded: got %q/%v", text, lvl)
	}
	if text, lvl := CapSummary(k8sclient.CapSummary{}); text != "—" || lvl != LevelMuted {
		t.Fatalf("empty: got %q/%v", text, lvl)
	}
}

func TestEstop(t *testing.T) {
	if text, lvl := Estop(""); text != "Normal" || lvl != LevelGood {
		t.Fatalf("empty estop: got %q/%v", text, lvl)
	}
	if _, lvl := Estop("Stopped"); lvl != LevelBad {
		t.Fatalf("Stopped should be bad, got %v", lvl)
	}
	if _, lvl := Estop("Resuming"); lvl != LevelWarn {
		t.Fatalf("Resuming should be warn, got %v", lvl)
	}
}

func TestAge(t *testing.T) {
	ref := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"seconds", ref.Add(-3 * time.Second), "3s"},
		{"minutes", ref.Add(-4 * time.Minute), "4m"},
		{"hours", ref.Add(-2 * time.Hour), "2h"},
		{"days", ref.Add(-49 * time.Hour), "2d"},
		{"future clamps to 0s", ref.Add(5 * time.Second), "0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Age(tc.t, ref, false); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	if got := Age(time.Time{}, ref, true); got != "—" {
		t.Fatalf("unknown age should be dash, got %q", got)
	}
	// A zero timestamp dashes even when the caller does not flag it unknown. This
	// is what lets a view carry a bare time.Time with no companion Unknown flag —
	// FleetTaskView.CreatedAt relies on it.
	if got := Age(time.Time{}, ref, false); got != "—" {
		t.Fatalf("zero age should be dash without an unknown flag, got %q", got)
	}
}

func TestTelemetryAge_Staleness(t *testing.T) {
	ref := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	stale := 30 * time.Second

	if text, lvl := TelemetryAge(ref.Add(-3*time.Second), ref, false, stale); text != "3s" || lvl != LevelGood {
		t.Fatalf("fresh: got %q/%v", text, lvl)
	}
	if text, lvl := TelemetryAge(ref.Add(-47*time.Second), ref, false, stale); text != "47s" || lvl != LevelBad {
		t.Fatalf("stale: got %q/%v", text, lvl)
	}
	if text, lvl := TelemetryAge(time.Time{}, ref, true, stale); text != "—" || lvl != LevelMuted {
		t.Fatalf("unknown: got %q/%v", text, lvl)
	}
}

func TestDeadline(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	future := now.Add(30 * time.Minute)
	soon := now.Add(2 * time.Minute)
	past := now.Add(-1 * time.Minute)

	if text, lvl := Deadline(nil, now); text != "—" || lvl != LevelMuted {
		t.Fatalf("nil deadline: got %q/%v", text, lvl)
	}
	if text, lvl := Deadline(&future, now); text != "in 30m" || lvl != LevelMuted {
		t.Fatalf("future deadline: got %q/%v", text, lvl)
	}
	if text, lvl := Deadline(&soon, now); text != "in 2m" || lvl != LevelWarn {
		t.Fatalf("soon deadline should warn: got %q/%v", text, lvl)
	}
	if text, lvl := Deadline(&past, now); text != "overdue" || lvl != LevelBad {
		t.Fatalf("past deadline: got %q/%v", text, lvl)
	}
}

func TestRobotPhaseAndHardware(t *testing.T) {
	if RobotPhase("InProgress") != LevelGood || RobotPhase("Offline") != LevelBad || RobotPhase("Maintenance") != LevelWarn {
		t.Fatal("robot phase severity mapping wrong")
	}
	if HardwareStatus("Healthy") != LevelGood || HardwareStatus("Failed") != LevelBad || HardwareStatus("Degraded") != LevelWarn {
		t.Fatal("hardware severity mapping wrong")
	}
}
