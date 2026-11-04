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

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// swarmada_telemetry_status_writes_total is the observable proof of the RA-1 gate
// (RFC-0001 #control-plane-observability-prometheus-metrics-contract): it counts
// Robot.status writes. A material transition the sink does not write — a reported
// phase other than Error, or a frame for a robot nothing maps to — is not a write,
// so it must not be counted as one. Each case drives the real sink through the
// Ingestor and compares the counter with the status patches the API server saw.
func TestIngest_StatusWriteCounterCountsOnlyWritesTheSinkMade(t *testing.T) {
	const ns = "warehouse-east"
	const robotID = ns + "/acme-bot-042" // namespaceOf(robotID) labels the counter

	cases := []struct {
		name   string
		next   fleetv1.RobotPhase
		action string
		robot  bool
		want   float64
	}{
		// Idle -> InProgress is material to the projector, and the sink leaves phase
		// to the scheduler and the lifecycle controllers: no write.
		{"phase-only report the sink does not write", fleetv1.RobotPhaseInProgress, "", true, 0},
		// The robot-reported assigned action is material to the projector; the
		// scheduler owns status.assignedAction, so the sink writes nothing.
		{"assigned-action report the sink does not write", fleetv1.RobotPhaseIdle, "fa-1", true, 0},
		// Error is the one reported phase the sink writes.
		{"reported Error is written", fleetv1.RobotPhaseError, "", true, 1},
		// No Robot carries the robot-id annotation: the sink drops the frame.
		{"frame for an unmapped robot", fleetv1.RobotPhaseError, "", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.robot {
				rob := robotWithID("acme-bot-042", ns, robotID)
				rob.Status.Phase = fleetv1.RobotPhaseIdle
				rob.Status.BatteryPercent = i32(80)
				objs = append(objs, rob)
			}
			sink, patches := newPatchCountingSink(t, objs...)
			proj := telemetry.NewProjector(telemetry.DefaultConfig())
			base := telemetry.Frame{RobotID: robotID, Namespace: ns, Timestamp: time.Now(),
				Phase: fleetv1.RobotPhaseIdle, BatteryPct: i32(80)}
			proj.Prime(base) // the stored state: nothing to establish
			ing := telemetry.NewIngestor(nil, proj, sink)

			next := base
			next.Phase = tc.next
			next.AssignedAction = tc.action
			next.Timestamp = base.Timestamp.Add(time.Minute) // outside any rate cap
			before := statusWritesCounted(ns)

			if err := ing.Ingest(context.Background(), next); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if got := float64(patches.Load()); got != tc.want {
				t.Fatalf("status patches = %v, want %v", got, tc.want)
			}
			if got := statusWritesCounted(ns) - before; got != tc.want {
				t.Errorf("status_writes_total delta = %v, want %v: the counter must equal the writes made", got, tc.want)
			}
		})
	}
}

// statusWritesCounted sums swarmada_telemetry_status_writes_total over every
// transition type in namespace ns.
func statusWritesCounted(ns string) float64 {
	var sum float64
	for _, tt := range []string{
		telemetry.TransitionSafetyCritical, telemetry.TransitionPhaseChange,
		telemetry.TransitionHardwareHealth, telemetry.TransitionBatteryThreshold,
		telemetry.TransitionAssignedAction,
	} {
		sum += testutil.ToFloat64(metrics.TelemetryStatusWritesTotal.WithLabelValues(ns, tt))
	}
	return sum
}

// newPatchCountingSink is newSink with every Robot status patch counted.
func newPatchCountingSink(t *testing.T, objs ...client.Object) (*RobotStatusSink, *atomic.Int64) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	var patches atomic.Int64
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if _, ok := obj.(*fleetv1.Robot); ok {
					patches.Add(1)
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	return &RobotStatusSink{Client: c}, &patches
}
