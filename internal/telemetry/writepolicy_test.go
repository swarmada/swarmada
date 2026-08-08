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

package telemetry_test

import (
	"reflect"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

func actionFrame(action string) telemetry.Frame {
	return frame(fleetv1.RobotPhaseIdle, i32(80), action, nil)
}

// The per-minute ceiling holds non-critical writes once a robot hits its budget;
// a held change flushes (never lost) once the trailing-minute window clears.
func TestProject_MaxWritesPerMinuteCeiling(t *testing.T) {
	clk := time.Unix(1000, 0)
	p := telemetry.NewProjectorWithClock(
		telemetry.Config{MaxStatusWritesPerMinute: 2}, // MinInterval 0 → only the ceiling applies
		func() time.Time { return clk },
	)
	p.Prime(actionFrame("")) // establish without a write

	if p.Project(actionFrame("a")) == nil {
		t.Fatal("1st non-critical change should write")
	}
	if p.Project(actionFrame("b")) == nil {
		t.Fatal("2nd non-critical change should write")
	}
	if p.Project(actionFrame("c")) != nil {
		t.Fatal("3rd should be HELD by the per-minute ceiling")
	}

	// The window clears; the still-pending change flushes.
	clk = clk.Add(61 * time.Second)
	if p.Project(actionFrame("c")) == nil {
		t.Fatal("held change should flush after the per-minute window elapses")
	}
}

// A safety-critical change bypasses the ceiling (and does not consume the budget).
func TestProject_CriticalBypassesCeiling(t *testing.T) {
	clk := time.Unix(2000, 0)
	p := telemetry.NewProjectorWithClock(
		telemetry.Config{MaxStatusWritesPerMinute: 1},
		func() time.Time { return clk },
	)
	p.Prime(actionFrame(""))

	if p.Project(actionFrame("a")) == nil {
		t.Fatal("1st change should write")
	}
	if p.Project(actionFrame("b")) != nil {
		t.Fatal("2nd non-critical change should be held at the ceiling of 1")
	}
	// phase → Offline is safety-critical: it MUST write even over the ceiling.
	if p.Project(frame(fleetv1.RobotPhaseOffline, i32(80), "b", nil)) == nil {
		t.Fatal("a safety-critical change must bypass the per-minute ceiling")
	}
}

// ConfigFromTelemetry maps the SwarmadaConfig write-policy onto the projector Config.
func TestConfigFromTelemetry(t *testing.T) {
	c := telemetry.ConfigFromTelemetry(5, 10, []int32{20, 40})
	if c.MinStatusWriteInterval != 5*time.Second {
		t.Errorf("MinStatusWriteInterval = %v, want 5s", c.MinStatusWriteInterval)
	}
	if c.MaxStatusWritesPerMinute != 10 {
		t.Errorf("MaxStatusWritesPerMinute = %d, want 10", c.MaxStatusWritesPerMinute)
	}
	if !reflect.DeepEqual(c.BatteryThresholds, []int32{20, 40}) {
		t.Errorf("BatteryThresholds = %v, want [20 40]", c.BatteryThresholds)
	}
	// Empty thresholds fall back to the default buckets.
	if got := telemetry.ConfigFromTelemetry(0, 0, nil).BatteryThresholds; !reflect.DeepEqual(got, []int32{15, 30}) {
		t.Errorf("default BatteryThresholds = %v, want [15 30]", got)
	}
}
