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
	"context"
	"sync"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// recordingTSDB counts the frames whose samples reach it.
type recordingTSDB struct {
	mu sync.Mutex
	n  int
}

func (r *recordingTSDB) WriteSamples(_ context.Context, _ []telemetry.Sample) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return nil
}

func (r *recordingTSDB) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func robotFrame(robotID string, phase fleetv1.RobotPhase) telemetry.Frame {
	return telemetry.Frame{RobotID: robotID, Timestamp: time.Now(), Phase: phase, BatteryPct: i32(80)}
}

// A frame is routed to the TSDB sink configured for ITS namespace: two namespaces
// with different sink types land in different sinks.
func TestRouter_RoutesPerNamespaceSink(t *testing.T) {
	sinkA, sinkB := &recordingTSDB{}, &recordingTSDB{}
	factory := func(sinkType, _ string) telemetry.TSDBWriter {
		switch sinkType {
		case "A":
			return sinkA
		case "B":
			return sinkB
		default:
			return telemetry.NoopWriter{}
		}
	}
	resolver := func(ns string) telemetry.PlaneConfig {
		switch ns {
		case "ns-a":
			return telemetry.PlaneConfig{SinkType: "A"}
		case "ns-b":
			return telemetry.PlaneConfig{SinkType: "B"}
		default:
			return telemetry.PlaneConfig{}
		}
	}
	r := telemetry.NewRouter(resolver, &countingSink{}, telemetry.WithSinkFactory(factory))

	if err := r.Ingest(context.Background(), robotFrame("ns-a/r1", fleetv1.RobotPhaseIdle)); err != nil {
		t.Fatalf("ingest ns-a: %v", err)
	}
	if err := r.Ingest(context.Background(), robotFrame("ns-b/r1", fleetv1.RobotPhaseIdle)); err != nil {
		t.Fatalf("ingest ns-b: %v", err)
	}
	if sinkA.count() != 1 || sinkB.count() != 1 {
		t.Fatalf("per-namespace routing wrong: sinkA=%d sinkB=%d, want 1 and 1", sinkA.count(), sinkB.count())
	}
}

// A namespace with no telemetry config (resolver returns the zero PlaneConfig)
// fails safe to a Drop sink (NoopWriter): the high-cadence samples are dropped,
// never forced onto etcd, and Ingest returns no error. The low-cadence status
// plane is independent of the sink, so the material establish write still lands.
func TestRouter_FailSafeDropsUnconfiguredNamespace(t *testing.T) {
	sink := &countingSink{}
	// Default sink factory (real NewSink) → NoopWriter for the empty sink type.
	r := telemetry.NewRouter(
		func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		sink,
	)
	if err := r.Ingest(context.Background(), robotFrame("ns-x/r1", fleetv1.RobotPhaseIdle)); err != nil {
		t.Fatalf("unconfigured namespace must not error (drop): %v", err)
	}
	if sink.n != 1 {
		t.Fatalf("status establish writes = %d, want 1 (status plane independent of the dropped TSDB sink)", sink.n)
	}
}

// The resolver is memoized within the refresh window: many frames for one
// namespace resolve its config exactly once, not per frame (the hot path cannot
// hit the cached client per tick). Past the window it re-resolves.
func TestRouter_MemoizesResolverWithinRefreshWindow(t *testing.T) {
	var calls int
	clk := time.Unix(1000, 0)
	r := telemetry.NewRouter(
		func(string) telemetry.PlaneConfig { calls++; return telemetry.PlaneConfig{} },
		&countingSink{},
		telemetry.WithClock(func() time.Time { return clk }),
		telemetry.WithRefreshInterval(15*time.Second),
	)
	for i := 0; i < 10; i++ {
		_ = r.Ingest(context.Background(), robotFrame("ns-a/r1", fleetv1.RobotPhaseIdle))
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1 (memoized within the refresh window)", calls)
	}
	clk = clk.Add(16 * time.Second) // past the window
	_ = r.Ingest(context.Background(), robotFrame("ns-a/r1", fleetv1.RobotPhaseIdle))
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (re-resolve after the refresh window)", calls)
	}
}

// When a namespace's sink changes, subsequent frames route to the NEW sink — and
// the per-robot Projector state is preserved across the reconfiguration, so an
// unchanged robot does NOT produce a second (spurious) establish status write.
func TestRouter_ReconfiguresSinkPreservingProjectorState(t *testing.T) {
	sinkA, sinkB := &recordingTSDB{}, &recordingTSDB{}
	factory := func(sinkType, _ string) telemetry.TSDBWriter {
		if sinkType == "B" {
			return sinkB
		}
		return sinkA
	}
	current := "A"
	clk := time.Unix(2000, 0)
	status := &countingSink{}
	r := telemetry.NewRouter(
		func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{SinkType: current} },
		status,
		telemetry.WithSinkFactory(factory),
		telemetry.WithClock(func() time.Time { return clk }),
		telemetry.WithRefreshInterval(15*time.Second),
	)

	// Establish the robot on sink A (one status write).
	_ = r.Ingest(context.Background(), robotFrame("ns-a/r1", fleetv1.RobotPhaseIdle))
	if sinkA.count() != 1 || status.n != 1 {
		t.Fatalf("after establish: sinkA=%d status=%d, want 1 and 1", sinkA.count(), status.n)
	}

	// Flip the sink and cross the refresh window; the same unchanged frame now
	// routes to sink B.
	current = "B"
	clk = clk.Add(16 * time.Second)
	_ = r.Ingest(context.Background(), robotFrame("ns-a/r1", fleetv1.RobotPhaseIdle))
	if sinkB.count() != 1 {
		t.Fatalf("after sink change: sinkB=%d, want 1 (routed to the new sink)", sinkB.count())
	}
	// State preserved: no re-establish, so the status sink is untouched by the
	// unchanged frame.
	if status.n != 1 {
		t.Fatalf("status writes = %d, want 1 (Projector state preserved across reconfig — no spurious re-establish)", status.n)
	}
}

// Invalidate forces an immediate re-resolve, without waiting out the refresh
// window, so a SwarmadaConfig watch can propagate a change at once.
func TestRouter_InvalidateForcesImmediateReresolve(t *testing.T) {
	sinkA, sinkB := &recordingTSDB{}, &recordingTSDB{}
	factory := func(sinkType, _ string) telemetry.TSDBWriter {
		if sinkType == "B" {
			return sinkB
		}
		return sinkA
	}
	current := "A"
	clk := time.Unix(3000, 0) // clock never advances
	r := telemetry.NewRouter(
		func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{SinkType: current} },
		&countingSink{},
		telemetry.WithSinkFactory(factory),
		telemetry.WithClock(func() time.Time { return clk }),
		telemetry.WithRefreshInterval(15*time.Second),
	)

	_ = r.Ingest(context.Background(), robotFrame("ns-a/r1", fleetv1.RobotPhaseIdle))
	current = "B"
	// Without Invalidate the window has not elapsed, so B would not be seen yet.
	r.Invalidate("ns-a")
	_ = r.Ingest(context.Background(), robotFrame("ns-a/r1", fleetv1.RobotPhaseInProgress))
	if sinkB.count() != 1 {
		t.Fatalf("sinkB=%d, want 1 (Invalidate forced an immediate re-resolve to B)", sinkB.count())
	}
}
