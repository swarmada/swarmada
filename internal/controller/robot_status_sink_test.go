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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

func i32(v int32) *int32 { return &v }

func robotWithID(name, ns, robotID string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: map[string]string{RobotIDAnnotation: robotID},
		},
	}
}

func newSink(t *testing.T, objs ...client.Object) (*RobotStatusSink, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}).
		Build()
	return &RobotStatusSink{Client: c}, c
}

func getRobot(t *testing.T, c client.Client, name, ns string) *fleetv1.Robot {
	t.Helper()
	r := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, r); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return r
}

func TestApplyMaterialUpdate_ProjectsBattery(t *testing.T) {
	sink, c := newSink(t, robotWithID("sim-robot-001", "warehouse-a", "rid-1"))

	// 0% is a valid critical reading; it must persist as *0.
	if err := sink.ApplyMaterialUpdate(context.Background(), telemetry.MaterialUpdate{
		RobotID: "rid-1", BatteryPct: i32(0),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := getRobot(t, c, "sim-robot-001", "warehouse-a")
	if got.Status.BatteryPercent == nil || *got.Status.BatteryPercent != 0 {
		t.Fatalf("batteryPercent = %v, want 0", got.Status.BatteryPercent)
	}
}

func TestApplyMaterialUpdate_ProjectsHardwarePreservingOtherFields(t *testing.T) {
	robot := robotWithID("sim-robot-001", "warehouse-a", "rid-1")
	healthyAt := metav1.NewTime(time.Unix(1_700_000_000, 0))
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{
		{Name: "lidar", Status: fleetv1.HardwareHealthy, LastHealthyAt: &healthyAt},
	}
	sink, c := newSink(t, robot)

	if err := sink.ApplyMaterialUpdate(context.Background(), telemetry.MaterialUpdate{
		RobotID:  "rid-1",
		Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareDegraded, "camera": fleetv1.HardwareFailed},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := getRobot(t, c, "sim-robot-001", "warehouse-a")
	byName := map[string]fleetv1.HardwareComponentStatus{}
	for _, h := range got.Status.Hardware {
		byName[h.Name] = h
	}
	if byName["lidar"].Status != fleetv1.HardwareDegraded {
		t.Errorf("lidar status = %q, want Degraded", byName["lidar"].Status)
	}
	if byName["lidar"].LastHealthyAt == nil {
		t.Error("lidar LastHealthyAt was clobbered; existing fields must be preserved")
	}
	if byName["camera"].Status != fleetv1.HardwareFailed {
		t.Errorf("camera status = %q, want Failed (newly appended)", byName["camera"].Status)
	}
}

func TestApplyMaterialUpdate_DoesNotWritePhaseOrAction(t *testing.T) {
	robot := robotWithID("sim-robot-001", "warehouse-a", "rid-1")
	robot.Status.Phase = fleetv1.RobotPhaseInProgress
	robot.Status.AssignedAction = "task-42"
	sink, c := newSink(t, robot)

	// A phase-only material transition (no battery / hardware) must not write.
	offline := fleetv1.RobotPhaseOffline
	action := ""
	if err := sink.ApplyMaterialUpdate(context.Background(), telemetry.MaterialUpdate{
		RobotID: "rid-1", Phase: &offline, AssignedAction: &action,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := getRobot(t, c, "sim-robot-001", "warehouse-a")
	if got.Status.Phase != fleetv1.RobotPhaseInProgress {
		t.Errorf("Phase = %q; telemetry sink must not overwrite the scheduler-owned phase", got.Status.Phase)
	}
	if got.Status.AssignedAction != "task-42" {
		t.Errorf("AssignedAction = %q; telemetry sink must not overwrite the single-executor task binding", got.Status.AssignedAction)
	}
}

func TestApplyMaterialUpdate_UnresolvedRobotIsDroppedNotErrored(t *testing.T) {
	sink, _ := newSink(t, robotWithID("sim-robot-001", "warehouse-a", "rid-1"))
	if err := sink.ApplyMaterialUpdate(context.Background(), telemetry.MaterialUpdate{
		RobotID: "ghost", BatteryPct: i32(55),
	}); err != nil {
		t.Fatalf("unresolved robot_id must be dropped, not errored; got %v", err)
	}
}

func TestApplyMaterialUpdate_DuplicateRobotIDErrors(t *testing.T) {
	// Two Robots claiming the same robot_id is a spec violation; the sink must
	// refuse to guess rather than write to an arbitrary one.
	sink, _ := newSink(t,
		robotWithID("robot-a", "warehouse-a", "dup"),
		robotWithID("robot-b", "warehouse-b", "dup"),
	)
	if err := sink.ApplyMaterialUpdate(context.Background(), telemetry.MaterialUpdate{
		RobotID: "dup", BatteryPct: i32(10),
	}); err == nil {
		t.Fatal("expected an error for a non-unique robot_id")
	}
}

// TestLiveFeed_RA1_NoWritePerTick is the guard for the single most important
// invariant: N unchanged telemetry frames must produce zero status writes beyond
// the one establishing write. It exercises the real Ingestor + Projector + a
// counting sink, mirroring the production wiring.
func TestLiveFeed_RA1_NoWritePerTick(t *testing.T) {
	proj := telemetry.NewProjector(telemetry.DefaultConfig())
	cs := &countingSink{}
	ing := telemetry.NewIngestor(nil, proj, cs)

	for i := 0; i < 100; i++ {
		if err := ing.Ingest(context.Background(), telemetry.Frame{
			RobotID:    "rid-1",
			Phase:      fleetv1.RobotPhaseIdle,
			BatteryPct: i32(50),
		}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	if cs.n != 1 {
		t.Fatalf("RA-1 violated: 100 identical frames produced %d status writes, want 1 (establishing only)", cs.n)
	}
}

type countingSink struct{ n int }

func (c *countingSink) ApplyMaterialUpdate(_ context.Context, _ telemetry.MaterialUpdate) error {
	c.n++
	return nil
}
