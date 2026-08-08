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

package controlstream

import (
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func f64(v float64) *float64 { return &v }
func i32(v int32) *int32     { return &v }

func TestTelemetryFrame_BatteryPresencePreserved(t *testing.T) {
	// A real 0% reading (critical) must survive as *0, distinct from "absent".
	got := TelemetryFrame(&fav1.TelemetryPayload{
		RobotId:     "warehouse-east/acme-042",
		TimestampMs: 1_700_000_000_000,
		Battery:     &fav1.BatteryStatus{Percent: i32(0)},
	})
	if got.BatteryPct == nil {
		t.Fatal("battery 0%% collapsed to absent; explicit presence lost")
	}
	if *got.BatteryPct != 0 {
		t.Fatalf("battery = %d, want 0", *got.BatteryPct)
	}
	if got.RobotID != "warehouse-east/acme-042" {
		t.Fatalf("robot id = %q", got.RobotID)
	}

	// Battery message present but percent absent → nil.
	if f := TelemetryFrame(&fav1.TelemetryPayload{Battery: &fav1.BatteryStatus{}}); f.BatteryPct != nil {
		t.Fatalf("absent percent mapped to %v, want nil", *f.BatteryPct)
	}
	// No battery message at all → nil.
	if f := TelemetryFrame(&fav1.TelemetryPayload{}); f.BatteryPct != nil {
		t.Fatalf("missing battery mapped to %v, want nil", *f.BatteryPct)
	}
}

func TestTelemetryFrame_PositionGatedOnXY(t *testing.T) {
	// x present but y absent must NOT fabricate a phantom origin (RA-3b).
	if f := TelemetryFrame(&fav1.TelemetryPayload{
		Position: &fav1.RobotPosition{X: f64(3.0)},
	}); f.Position != nil {
		t.Fatalf("position emitted with y absent: %+v", *f.Position)
	}

	// Both x and y present (both 0.0, a valid coordinate) → emitted.
	f := TelemetryFrame(&fav1.TelemetryPayload{
		Position: &fav1.RobotPosition{X: f64(0), Y: f64(0), Yaw: f64(1.5), Floor: i32(2)},
	})
	if f.Position == nil {
		t.Fatal("position dropped when x and y are both present at 0.0")
	}
	if f.Position.X != 0 || f.Position.Y != 0 || f.Position.Yaw != 1.5 || f.Position.Floor != 2 {
		t.Fatalf("position = %+v", *f.Position)
	}

	// No position message → nil.
	if f := TelemetryFrame(&fav1.TelemetryPayload{}); f.Position != nil {
		t.Fatalf("missing position mapped to %+v, want nil", *f.Position)
	}
}

func TestMapRobotPhase(t *testing.T) {
	cases := map[fav1.RobotPhase]fleetv1.RobotPhase{
		fav1.RobotPhase_ROBOT_PHASE_IDLE:        fleetv1.RobotPhaseIdle,
		fav1.RobotPhase_ROBOT_PHASE_ASSIGNED:    fleetv1.RobotPhaseAssigned,
		fav1.RobotPhase_ROBOT_PHASE_IN_PROGRESS: fleetv1.RobotPhaseInProgress,
		fav1.RobotPhase_ROBOT_PHASE_CHARGING:    fleetv1.RobotPhaseCharging,
		fav1.RobotPhase_ROBOT_PHASE_ERROR:       fleetv1.RobotPhaseError,
		fav1.RobotPhase_ROBOT_PHASE_OFFLINE:     fleetv1.RobotPhaseOffline,
		fav1.RobotPhase_ROBOT_PHASE_MAINTENANCE: fleetv1.RobotPhaseMaintenance,
		fav1.RobotPhase_ROBOT_PHASE_DISCOVERED:  fleetv1.RobotPhaseDiscovered,
		// UNSPECIFIED → "" (projector leaves the recorded phase unchanged).
		fav1.RobotPhase_ROBOT_PHASE_UNSPECIFIED: "",
	}
	for in, want := range cases {
		if got := mapRobotPhase(in); got != want {
			t.Errorf("mapRobotPhase(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestMapHardware_SkipsUnknownAndEmptyNames(t *testing.T) {
	f := TelemetryFrame(&fav1.TelemetryPayload{
		Hardware: []*fav1.HardwareStatusUpdate{
			{ComponentName: "lidar-front", Status: fav1.HardwareStatus_HARDWARE_STATUS_DEGRADED},
			{ComponentName: "", Status: fav1.HardwareStatus_HARDWARE_STATUS_FAILED},                // no name → skip
			{ComponentName: "camera-top", Status: fav1.HardwareStatus_HARDWARE_STATUS_UNSPECIFIED}, // unknown → skip
			{ComponentName: "gripper", Status: fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY},
			{ComponentName: "mic", Status: fav1.HardwareStatus_HARDWARE_STATUS_DISABLED}, // intentionally off (ADR-0031)
		},
	})
	if len(f.Hardware) != 3 {
		t.Fatalf("hardware map size = %d, want 3 (%v)", len(f.Hardware), f.Hardware)
	}
	if f.Hardware["lidar-front"] != fleetv1.HardwareDegraded {
		t.Errorf("lidar-front = %q, want Degraded", f.Hardware["lidar-front"])
	}
	if f.Hardware["gripper"] != fleetv1.HardwareHealthy {
		t.Errorf("gripper = %q, want Healthy", f.Hardware["gripper"])
	}
	if f.Hardware["mic"] != fleetv1.HardwareDisabled {
		t.Errorf("mic = %q, want Disabled", f.Hardware["mic"])
	}

	// An all-unknown delta produces no hardware change.
	f2 := TelemetryFrame(&fav1.TelemetryPayload{
		Hardware: []*fav1.HardwareStatusUpdate{
			{ComponentName: "x", Status: fav1.HardwareStatus_HARDWARE_STATUS_UNSPECIFIED},
		},
	})
	if f2.Hardware != nil {
		t.Fatalf("all-unknown delta mapped to %v, want nil", f2.Hardware)
	}
}
