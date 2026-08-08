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

package controller

// Phase-3(c) guard for ADR-0028: a telemetry HardwareStatusUpdate for a given
// robot_id must reach Robot.status.hardware through the real ingest → project →
// sink path, joined to the Robot solely by the swarmada.io/robot-id annotation
// (the identity that auto-admit stamps and the defaulter backfills). This is the
// end-to-end complement to the direct-sink assertions in robot_status_sink_test.go.

import (
	"context"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// A hardware frame keyed by robot_id lands in status.hardware on the Robot that
// carries RobotIDAnnotation == that robot_id — even though the Robot's object name
// ("sim-robot-001") differs from the wire robot_id ("rid-hw"), proving the join is
// on the annotation, not on metadata.name.
func TestLiveFeed_HardwareStatusUpdateLandsInStatus(t *testing.T) {
	sink, c := newSink(t, robotWithID("sim-robot-001", "warehouse-a", "rid-hw"))

	proj := telemetry.NewProjector(telemetry.DefaultConfig())
	ing := telemetry.NewIngestor(nil, proj, sink)

	if err := ing.Ingest(context.Background(), telemetry.Frame{
		RobotID: "rid-hw",
		Hardware: map[string]fleetv1.HardwareStatus{
			"camera": fleetv1.HardwareHealthy,
			"imu":    fleetv1.HardwareFailed,
		},
	}); err != nil {
		t.Fatalf("ingest hardware frame: %v", err)
	}

	got := getRobot(t, c, "sim-robot-001", "warehouse-a")
	if len(got.Status.Hardware) == 0 {
		t.Fatal("status.hardware is empty: the hardware telemetry did not reach the Robot via the robot-id annotation join")
	}
	byName := map[string]fleetv1.HardwareStatus{}
	for _, h := range got.Status.Hardware {
		byName[h.Name] = h.Status
	}
	if byName["camera"] != fleetv1.HardwareHealthy {
		t.Errorf("camera status = %q, want %q", byName["camera"], fleetv1.HardwareHealthy)
	}
	if byName["imu"] != fleetv1.HardwareFailed {
		t.Errorf("imu status = %q, want %q", byName["imu"], fleetv1.HardwareFailed)
	}
}

// A hardware frame whose robot_id maps to no Robot (annotation absent / not admitted)
// is dropped, not errored, and writes nothing — the unmapped-robot case that was the
// original defect before ADR-0028 stamped the annotation at admission.
func TestLiveFeed_HardwareForUnmappedRobotIDIsDropped(t *testing.T) {
	// Present Robot carries a different robot_id, so "ghost-rid" resolves to nil.
	sink, c := newSink(t, robotWithID("sim-robot-001", "warehouse-a", "rid-hw"))

	proj := telemetry.NewProjector(telemetry.DefaultConfig())
	ing := telemetry.NewIngestor(nil, proj, sink)

	if err := ing.Ingest(context.Background(), telemetry.Frame{
		RobotID:  "ghost-rid",
		Hardware: map[string]fleetv1.HardwareStatus{"camera": fleetv1.HardwareHealthy},
	}); err != nil {
		t.Fatalf("unmapped robot_id must be dropped, not errored; got %v", err)
	}
	got := getRobot(t, c, "sim-robot-001", "warehouse-a")
	if len(got.Status.Hardware) != 0 {
		t.Errorf("status.hardware = %v; a frame for an unmapped robot_id must not write to any Robot", got.Status.Hardware)
	}
}
