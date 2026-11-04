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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"

	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// writingSink reports every material update as written.
type writingSink struct{}

func (writingSink) ApplyMaterialUpdate(context.Context, telemetry.MaterialUpdate) (bool, error) {
	return true, nil
}

// A robot_id on the wire carries no namespace ("acme-bot-042"); the ControlStream server
// sets Frame.Namespace from the adapter's verified identity. Both counters take their
// namespace label from there (RFC-0001 #control-plane-observability-prometheus-metrics-contract),
// not from the robot id, or every real deployment reports namespace="".
func TestIngest_CountersLabelTheFrameNamespaceNotTheBareRobotID(t *testing.T) {
	const ns, robotID = "warehouse-a", "acme-bot-042"

	dropped := metrics.TelemetryDroppedFramesTotal.WithLabelValues(ns, "adapter-ns")
	f := telemetry.Frame{RobotID: robotID, Namespace: ns, Adapter: "adapter-ns",
		Timestamp: time.Now(), Phase: fleetv1.RobotPhaseIdle, BatteryPct: i32(80)}
	before := testutil.ToFloat64(dropped)
	_ = telemetry.NewIngestor(failingTSDB{}, telemetry.NewProjector(telemetry.DefaultConfig()), nil).
		Ingest(context.Background(), f)
	if got := testutil.ToFloat64(dropped) - before; got != 1 {
		t.Errorf("dropped_frames{namespace=%q} delta = %v, want 1", ns, got)
	}

	proj := telemetry.NewProjector(telemetry.DefaultConfig())
	proj.Prime(f)
	ing := telemetry.NewIngestor(nil, proj, writingSink{})
	next := f
	next.Phase = fleetv1.RobotPhaseOffline
	writes := metrics.TelemetryStatusWritesTotal.WithLabelValues(ns, telemetry.TransitionSafetyCritical)
	before = testutil.ToFloat64(writes)
	if err := ing.Ingest(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(writes) - before; got != 1 {
		t.Errorf("status_writes{namespace=%q, transition_type=%q} delta = %v, want 1",
			ns, telemetry.TransitionSafetyCritical, got)
	}
}
