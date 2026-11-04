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
	"errors"
	"sync"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A TRANSITION WHOSE STATUS WRITE FAILED IS STILL TO BE WRITTEN.
//
// The projector decides a frame is material and records it; if the status write then
// fails, the record says the transition reached Robot.status when it did not, and the
// next frame reporting the same reading is "not material" and never written. The status
// would keep the pre-transition reading until the component changes again. RA-1 still
// holds: only transitions are written, and a transition written after a failure is the
// same transition, written once.

// failingSink fails the writes whose 1-based call number is in fail, and records every
// update it was asked to write.
type failingSink struct {
	mu    sync.Mutex
	fail  map[int]bool
	calls []MaterialUpdate
}

func (s *failingSink) ApplyMaterialUpdate(_ context.Context, upd MaterialUpdate) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, upd)
	if s.fail[len(s.calls)] {
		return false, errors.New("apiserver unavailable")
	}
	return true, nil
}

func (s *failingSink) snapshot() []MaterialUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MaterialUpdate(nil), s.calls...)
}

func lidarFrame(status fleetv1.HardwareStatus) Frame {
	return Frame{
		RobotID: "amr-1", Namespace: "warehouse-a", Phase: fleetv1.RobotPhaseIdle,
		Hardware: map[string]fleetv1.HardwareStatus{"lidar": status},
	}
}

func TestIngest_AFailedStatusWriteIsWrittenByTheNextFrame(t *testing.T) {
	sink := &failingSink{fail: map[int]bool{2: true}}
	ing := NewIngestor(nil, NewProjector(Config{}), sink)
	ctx := context.Background()

	if err := ing.Ingest(ctx, lidarFrame(fleetv1.HardwareHealthy)); err != nil {
		t.Fatalf("establishing frame: %v", err)
	}
	if err := ing.Ingest(ctx, lidarFrame(fleetv1.HardwareFailed)); err == nil {
		t.Fatal("the failed write was not reported")
	}
	if err := ing.Ingest(ctx, lidarFrame(fleetv1.HardwareFailed)); err != nil {
		t.Fatalf("the frame after the failure: %v", err)
	}

	calls := sink.snapshot()
	if len(calls) != 3 {
		t.Fatalf("%d status writes, want 3 (establish, the failed transition, the transition again): "+
			"the lidar Failed never reaches Robot.status", len(calls))
	}
	if got := calls[2].Hardware["lidar"]; got != fleetv1.HardwareFailed {
		t.Errorf("the write after the failure carried lidar %q, want Failed", got)
	}

	// RA-1: once written, the reading is not written again.
	for i := 0; i < 5; i++ {
		_ = ing.Ingest(ctx, lidarFrame(fleetv1.HardwareFailed))
	}
	if n := len(sink.snapshot()); n != 3 {
		t.Errorf("%d status writes after five unchanged frames, want 3: an unchanged frame is not a transition", n)
	}
}

// The establishing write is a transition too: a robot whose first write failed is
// established by its next frame.
func TestIngest_AFailedEstablishingWriteIsWrittenByTheNextFrame(t *testing.T) {
	sink := &failingSink{fail: map[int]bool{1: true}}
	ing := NewIngestor(nil, NewProjector(Config{}), sink)
	ctx := context.Background()
	_ = ing.Ingest(ctx, lidarFrame(fleetv1.HardwareHealthy))
	if err := ing.Ingest(ctx, lidarFrame(fleetv1.HardwareHealthy)); err != nil {
		t.Fatalf("the frame after the failure: %v", err)
	}
	calls := sink.snapshot()
	if len(calls) != 2 || calls[1].Hardware["lidar"] != fleetv1.HardwareHealthy {
		t.Fatalf("writes = %+v, want the establishing write repeated once", calls)
	}
}

// A non-critical transition whose write failed is not held by the rate cap on account of
// the write that did not happen: the cap counts writes, and the failed one is not one.
func TestIngest_AFailedWriteDoesNotConsumeTheRateCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	sink := &failingSink{fail: map[int]bool{2: true}}
	cfg := Config{MinStatusWriteInterval: 10 * time.Second}
	ing := NewIngestor(nil, NewProjectorWithClock(cfg, clock), sink)
	ctx := context.Background()

	idle := Frame{RobotID: "amr-1", Namespace: "warehouse-a", Phase: fleetv1.RobotPhaseIdle}
	busy := Frame{RobotID: "amr-1", Namespace: "warehouse-a", Phase: fleetv1.RobotPhaseInProgress}
	_ = ing.Ingest(ctx, idle)
	now = now.Add(11 * time.Second)
	_ = ing.Ingest(ctx, busy) // non-critical, outside the window; the write fails
	now = now.Add(time.Second)
	_ = ing.Ingest(ctx, busy)
	calls := sink.snapshot()
	if len(calls) != 3 || calls[2].Phase == nil || *calls[2].Phase != fleetv1.RobotPhaseInProgress {
		t.Fatalf("writes = %d, want the InProgress transition written by the frame after the failure", len(calls))
	}
}
