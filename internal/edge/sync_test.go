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

package edge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/zone"
)

func square() []zone.Point {
	return []zone.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}
}

// fakeSource returns whatever Load is told to, so tests can drive success/error/empty.
type fakeSource struct {
	cfg  Config
	err  error
	call atomic.Int32
}

func (f *fakeSource) Load(context.Context) (Config, error) {
	f.call.Add(1)
	return f.cfg, f.err
}

func oneZone(ns string) Config {
	return Config{
		Namespace: ns,
		Zones:     []ZonePolygon{{Name: "z", Floor: 0, Polygon: square()}},
		RobotZone: map[string]string{"amr-1": "z"},
	}
}

func newSyncNode() *Node {
	return New(oneZone(edgeNS), audit.New(&audit.MemorySink{}, "t"))
}

func TestSyncer_AppliesConfigOnSuccess(t *testing.T) {
	n := New(Config{Namespace: edgeNS}, audit.New(&audit.MemorySink{}, "t")) // starts empty
	src := &fakeSource{cfg: oneZone(edgeNS)}
	s := NewSyncer(n, src, time.Second, logr.Discard())
	s.syncOnce(context.Background())

	if got := len(n.view().cfg.Zones); got != 1 {
		t.Fatalf("zones after sync = %d, want 1", got)
	}
	if n.view().cfg.RobotZone["amr-1"] != "z" {
		t.Fatalf("robot assignment not applied: %+v", n.view().cfg.RobotZone)
	}
}

func TestSyncer_RetainsLastGoodOnError(t *testing.T) {
	n := newSyncNode() // starts with zone z + amr-1
	src := &fakeSource{err: errors.New("control plane unreachable")}
	s := NewSyncer(n, src, time.Second, logr.Discard())
	s.syncOnce(context.Background())

	// FAIL SAFE: the outage must not drop the guard.
	if got := len(n.view().cfg.Zones); got != 1 {
		t.Fatalf("SAFETY: zones dropped on sync error, got %d want 1", got)
	}
	if n.view().cfg.RobotZone["amr-1"] != "z" {
		t.Fatal("SAFETY: robot assignment dropped on sync error")
	}
	if s.applied != 0 {
		t.Errorf("applied = %d, want 0 (nothing applied on error)", s.applied)
	}
}

func TestSyncer_RefusesEmptyOverNonEmpty(t *testing.T) {
	n := newSyncNode()
	src := &fakeSource{cfg: Config{Namespace: edgeNS}} // successful but zero zones
	s := NewSyncer(n, src, time.Second, logr.Discard())
	s.syncOnce(context.Background())

	// FAIL SAFE: a transient empty read must not blank out active guards.
	if got := len(n.view().cfg.Zones); got != 1 {
		t.Fatalf("SAFETY: active zones blanked by an empty read, got %d want 1", got)
	}
}

func TestSyncer_RunDoesInitialSyncThenStops(t *testing.T) {
	n := New(Config{Namespace: edgeNS}, audit.New(&audit.MemorySink{}, "t"))
	src := &fakeSource{cfg: oneZone(edgeNS)}
	s := NewSyncer(n, src, time.Hour, logr.Discard()) // long interval: only the initial sync fires

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	// The initial sync should apply promptly.
	deadline := time.After(time.Second)
	for len(n.view().cfg.Zones) == 0 {
		select {
		case <-deadline:
			t.Fatal("initial sync did not apply")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}
}

// A config hot-swap ADDS a guard: a robot unknown before the swap is estopped after.
func TestSetConfig_HotSwapAddsGuard(t *testing.T) {
	n := New(Config{Namespace: edgeNS, Zones: []ZonePolygon{{Name: "z", Floor: 0, Polygon: square()}}},
		audit.New(&audit.MemorySink{}, "t")) // no robots assigned yet
	n.deliveryTimeout = 30 * time.Millisecond
	n.confirmTimeout = 30 * time.Millisecond
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()

	fs.in <- posMsg("amr-9", 50, 50, 0) // out of zone but unknown → ignored
	select {
	case <-fs.out:
		t.Fatal("estop for a robot not yet in the synced config")
	case <-time.After(60 * time.Millisecond):
	}

	// Sync now assigns amr-9 to zone z.
	n.SetConfig(Config{Namespace: edgeNS, Zones: []ZonePolygon{{Name: "z", Floor: 0, Polygon: square()}},
		RobotZone: map[string]string{"amr-9": "z"}})

	fs.in <- posMsg("amr-9", 50, 50, 0) // now guarded → breach
	select {
	case est := <-fs.out:
		if est.GetEstop() == nil {
			t.Fatalf("expected an Estop after the guard was synced, got %T", est.GetMsg())
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no estop after the robot's zone was synced in")
	}
}

// SetConfig is safe under -race against concurrent boundary evaluations.
func TestSetConfig_ConcurrentWithReads(t *testing.T) {
	n := newSyncNode()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			n.SetConfig(Config{
				Namespace: edgeNS,
				Zones:     []ZonePolygon{{Name: "z", Floor: int32(i % 3), Polygon: square()}},
				RobotZone: map[string]string{"amr-1": "z"},
			})
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			v := n.view()
			_ = v.cfg.RobotZone["amr-1"]
			_ = v.zoneByName["z"]
			_ = n.Namespace()
		}
	}
}
