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
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"

	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// failingTSDB is a TSDBWriter whose writes always fail, standing in for a degraded
// remote-write sink.
type failingTSDB struct{}

func (failingTSDB) WriteSamples(context.Context, []telemetry.Sample) error {
	return errors.New("tsdb sink unavailable")
}

// A frame whose TSDB write fails increments dropped_frames{namespace, adapter}. A
// Drop/unset sink (NoopWriter) never errors, so it never increments — asserted too.
func TestIngest_DroppedFrameCountedOnTSDBError(t *testing.T) {
	// namespaceOf(testRobot) == "warehouse-east".
	dropped := metrics.TelemetryDroppedFramesTotal.WithLabelValues("warehouse-east", "adapter-x")

	// Failing sink → dropped_frames increments with the adapter label.
	failing := telemetry.NewIngestor(failingTSDB{}, telemetry.NewProjector(telemetry.DefaultConfig()), nil)
	f := frame(fleetv1.RobotPhaseIdle, i32(80), "", nil)
	f.Adapter = "adapter-x"
	before := testutil.ToFloat64(dropped)
	_ = failing.Ingest(context.Background(), f) // returns the sink error (logged by the caller)
	if got := testutil.ToFloat64(dropped) - before; got != 1 {
		t.Errorf("dropped_frames delta = %v, want 1 on a TSDB write error", got)
	}

	// NoopWriter (Drop/unset) never errors → no increment.
	drop := telemetry.NewIngestor(telemetry.NoopWriter{}, telemetry.NewProjector(telemetry.DefaultConfig()), nil)
	f2 := frame(fleetv1.RobotPhaseIdle, i32(80), "", nil)
	f2.Adapter = "adapter-x"
	before = testutil.ToFloat64(dropped)
	_ = drop.Ingest(context.Background(), f2)
	if got := testutil.ToFloat64(dropped) - before; got != 0 {
		t.Errorf("dropped_frames delta = %v, want 0 for a Drop sink (NoopWriter never errors)", got)
	}
}

// Project classifies the material write by its primary (highest-severity) reason.
func TestProject_TransitionTypeClassification(t *testing.T) {
	cases := []struct {
		name string
		next telemetry.Frame
		want string
	}{
		{"phase change (non-critical)", frame(fleetv1.RobotPhaseInProgress, i32(80), "", nil), telemetry.TransitionPhaseChange},
		{"phase -> Offline is safety_critical", frame(fleetv1.RobotPhaseOffline, i32(80), "", nil), telemetry.TransitionSafetyCritical},
		{"battery bucket cross", frame(fleetv1.RobotPhaseIdle, i32(25), "", nil), telemetry.TransitionBatteryThreshold},
		{"assigned task change", frame(fleetv1.RobotPhaseIdle, i32(80), "task-1", nil), telemetry.TransitionAssignedAction},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := telemetry.NewProjector(telemetry.DefaultConfig())
			p.Prime(frame(fleetv1.RobotPhaseIdle, i32(80), "", nil)) // established baseline, no write
			upd := p.Project(tc.next)
			if upd == nil {
				t.Fatal("expected a material update")
			}
			if upd.TransitionType != tc.want {
				t.Errorf("TransitionType = %q, want %q", upd.TransitionType, tc.want)
			}
		})
	}
}

type countingSink struct{ n int }

func (c *countingSink) ApplyMaterialUpdate(context.Context, telemetry.MaterialUpdate) error {
	c.n++
	return nil
}

// The status-write counter increments ONLY on a material transition — never per
// telemetry tick. This is the observable proof of the RA-1 material-transition
// gate: an unchanging stream produces exactly one write (the initial establish),
// not one per frame.
func TestIngest_StatusWriteCounterProvesRA1Gate(t *testing.T) {
	sink := &countingSink{}
	ing := telemetry.NewIngestor(nil, telemetry.NewProjector(telemetry.DefaultConfig()), sink)

	// namespaceOf(testRobot) == "warehouse-east".
	establish := metrics.TelemetryStatusWritesTotal.WithLabelValues("warehouse-east", telemetry.TransitionPhaseChange)
	before := testutil.ToFloat64(establish)

	base := frame(fleetv1.RobotPhaseIdle, i32(80), "", nil)
	for i := 0; i < 20; i++ {
		if err := ing.Ingest(context.Background(), base); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}

	// Exactly one material write (the initial establish), so exactly one counted.
	if sink.n != 1 {
		t.Fatalf("sink writes = %d, want 1 (RA-1: no per-tick writes)", sink.n)
	}
	if got := testutil.ToFloat64(establish) - before; got != 1 {
		t.Errorf("status_writes{phase_change} delta = %v, want 1", got)
	}
}
