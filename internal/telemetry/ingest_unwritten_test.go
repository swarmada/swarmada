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

// Hardware arrives as a DELTA: a component's reading is reported once, when it
// changes. A write of that reading that fails must not lose it, even when no later
// frame mentions the component again.
func TestIngest_AFailedWriteOfADeltaIsWrittenByAFrameThatDoesNotRepeatIt(t *testing.T) {
	sink := &failingSink{fail: map[int]bool{2: true}}
	ing := NewIngestor(nil, NewProjector(Config{}), sink)
	ctx := context.Background()

	_ = ing.Ingest(ctx, lidarFrame(fleetv1.HardwareHealthy))
	if err := ing.Ingest(ctx, lidarFrame(fleetv1.HardwareFailed)); err == nil {
		t.Fatal("the failed write was not reported")
	}
	if !ing.Projector.Unwritten("warehouse-a", "amr-1") {
		t.Fatal("Unwritten = false after a failed write, want true")
	}
	quiet := Frame{RobotID: "amr-1", Namespace: "warehouse-a", Phase: fleetv1.RobotPhaseIdle}
	if err := ing.Ingest(ctx, quiet); err != nil {
		t.Fatalf("the frame after the failure: %v", err)
	}
	calls := sink.snapshot()
	if len(calls) != 3 || calls[2].Hardware["lidar"] != fleetv1.HardwareFailed {
		t.Fatalf("writes = %+v, want the lidar Failed written by the frame after the failure", calls)
	}
	if ing.Projector.Unwritten("warehouse-a", "amr-1") {
		t.Fatal("Unwritten = true once the reading is written, want false")
	}

	// RA-1: written once. Unchanged frames write nothing more.
	for i := 0; i < 3; i++ {
		_ = ing.Ingest(ctx, quiet)
	}
	if n := len(sink.snapshot()); n != 3 {
		t.Errorf("%d status writes after unchanged frames, want 3", n)
	}
}

// A change the rate cap holds is unwritten until it is written; a frame that reports
// the written reading again leaves nothing unwritten.
func TestProjector_AHeldChangeIsUnwritten(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := NewProjectorWithClock(Config{MinStatusWriteInterval: 10 * time.Second}, func() time.Time { return now })
	idle := Frame{RobotID: "amr-1", Namespace: "warehouse-a", Phase: fleetv1.RobotPhaseIdle}
	busy := Frame{RobotID: "amr-1", Namespace: "warehouse-a", Phase: fleetv1.RobotPhaseInProgress}

	if !p.Unwritten("warehouse-a", "amr-1") {
		t.Fatal("Unwritten = false for a robot the projector has never seen, want true")
	}
	if p.Project(idle) == nil {
		t.Fatal("the establishing frame emitted nothing")
	}
	if p.Unwritten("warehouse-a", "amr-1") {
		t.Fatal("Unwritten = true after the establishing write, want false")
	}
	now = now.Add(time.Second)
	if p.Project(busy) != nil {
		t.Fatal("a non-critical change inside the interval was not held")
	}
	if !p.Unwritten("warehouse-a", "amr-1") {
		t.Fatal("Unwritten = false while the rate cap holds a change, want true")
	}
	if p.Project(idle) != nil || p.Unwritten("warehouse-a", "amr-1") {
		t.Fatal("a frame reporting the written reading again left the robot unwritten")
	}
}
