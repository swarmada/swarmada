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

package telemetry

import (
	"context"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Hardware arrives as a delta: a component's reading is sent once, when it changes. A
// non-critical reading the rate cap holds (Disabled, or a recovery to Healthy) is
// therefore not repeated by the next frame, and the cap must keep it until the window
// elapses and write it then: delayed, never lost (RFC-0001
// #control-plane-telemetry-pipeline-and-the-two-data-plane-split).
func TestIngest_AHeldHardwareDeltaIsWrittenWhenTheWindowElapses(t *testing.T) {
	for _, held := range []fleetv1.HardwareStatus{fleetv1.HardwareDisabled, fleetv1.HardwareHealthy} {
		t.Run(string(held), func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0)
			sink := &failingSink{}
			ing := NewIngestor(nil, NewProjectorWithClock(Config{MinStatusWriteInterval: 10 * time.Second},
				func() time.Time { return now }), sink)
			ctx := context.Background()
			first := fleetv1.HardwareFailed // critical: written at once, whatever the cap
			if held == fleetv1.HardwareHealthy {
				_ = ing.Ingest(ctx, lidarFrame(fleetv1.HardwareHealthy))
				now = now.Add(time.Second)
			} else {
				first = fleetv1.HardwareHealthy
			}
			_ = ing.Ingest(ctx, lidarFrame(first))
			now = now.Add(time.Second)
			before := len(sink.snapshot())
			_ = ing.Ingest(ctx, lidarFrame(held)) // inside the window: held by the cap
			if got := len(sink.snapshot()); got != before {
				t.Fatalf("lidar %s was written inside the rate-cap window", held)
			}
			for i := 0; i < 3; i++ {
				now = now.Add(20 * time.Second)
				_ = ing.Ingest(ctx, Frame{RobotID: "amr-1", Namespace: "warehouse-a", Phase: fleetv1.RobotPhaseIdle})
			}
			calls := sink.snapshot()
			written := 0
			for _, c := range calls[before:] {
				if c.Hardware["lidar"] == held {
					written++
				}
			}
			if written != 1 {
				t.Fatalf("lidar %s, reported once in a delta frame the rate cap held, was written %d time(s), want 1 (%d writes)",
					held, written, len(calls))
			}
		})
	}
}
