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
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// A telemetry frame resolves to a Robot by (namespace, robot_id). The robot-id
// annotation is unique per namespace only, so the same id in two namespaces is two
// robots, and each frame reaches its own.

func TestFindTelemetryRobot_ScopedToTheFrameNamespace(t *testing.T) {
	sink, _ := newSink(t,
		robotWithID("robot-a", "staging", "chassis-7"),
		robotWithID("robot-b", "production", "chassis-7"),
	)
	for ns, want := range map[string]string{"staging": "robot-a", "production": "robot-b"} {
		got, err := findTelemetryRobot(context.Background(), sink.Client, ns, "chassis-7")
		if err != nil {
			t.Fatalf("namespace %q: %v", ns, err)
		}
		if got == nil || got.Name != want || got.Namespace != ns {
			t.Fatalf("namespace %q resolved %v, want %s/%s", ns, got, ns, want)
		}
	}
}

// Inside one namespace a shared robot_id is a genuine violation, and the scoped
// lookup still refuses to guess.
func TestFindTelemetryRobot_DuplicateInsideOneNamespaceStillRefused(t *testing.T) {
	sink, _ := newSink(t,
		robotWithID("robot-a", "staging", "chassis-7"),
		robotWithID("robot-b", "staging", "chassis-7"),
	)
	_, err := findTelemetryRobot(context.Background(), sink.Client, "staging", "chassis-7")
	if err == nil || !strings.Contains(err.Error(), `more than one Robot in namespace "staging"`) {
		t.Fatalf("err = %v, want a refusal naming the namespace", err)
	}
}

// An adapter can open a stream without declaring a namespace. Then there is nothing
// to scope to, and the lookup searches the cluster: one match resolves.
func TestFindTelemetryRobot_NoNamespace_OneMatchResolves(t *testing.T) {
	sink, _ := newSink(t,
		robotWithID("robot-a", "staging", "chassis-7"),
		robotWithID("robot-c", "production", "chassis-9"),
	)
	got, err := findTelemetryRobot(context.Background(), sink.Client, "", "chassis-7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Name != "robot-a" {
		t.Fatalf("resolved %v, want staging/robot-a", got)
	}
}

// With no namespace and two matches, nothing on the frame says which robot it
// describes. The refusal names that reason, not a duplicated id.
func TestFindTelemetryRobot_NoNamespace_TwoMatchesRefusedWithTheReason(t *testing.T) {
	sink, _ := newSink(t,
		robotWithID("robot-a", "staging", "chassis-7"),
		robotWithID("robot-b", "production", "chassis-7"),
	)
	_, err := findTelemetryRobot(context.Background(), sink.Client, "", "chassis-7")
	if err == nil {
		t.Fatal("expected a refusal: two robots share the id and no namespace was declared")
	}
	for _, want := range []string{"declared no namespace", "cannot be identified", "staging", "production"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to contain %q", err, want)
		}
	}
}

// RA-1 with the same robot_id in two namespaces, through the real Ingestor,
// Projector and RobotStatusSink. Each robot gets exactly one establishing write, and
// every later identical frame, interleaved between the two, writes nothing. Counted
// on the status patches the sink issues, not on the values they leave.
func TestLiveFeed_RA1_SameRobotIDInTwoNamespaces_IdenticalFramesWriteNothing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	patches := map[string]*atomic.Int64{"staging": {}, "production": {}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			robotWithID("robot-a", "staging", "chassis-7"),
			robotWithID("robot-b", "production", "chassis-7"),
		).
		WithStatusSubresource(&fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if n := patches[obj.GetNamespace()]; n != nil {
					n.Add(1)
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	ing := telemetry.NewIngestor(nil, telemetry.NewProjector(telemetry.DefaultConfig()), &RobotStatusSink{Client: c})

	frames := []telemetry.Frame{
		{RobotID: "chassis-7", Namespace: "staging", BatteryPct: i32(80)},
		{RobotID: "chassis-7", Namespace: "production", BatteryPct: i32(20)},
	}
	for round := 0; round < 50; round++ {
		for _, f := range frames {
			if err := ing.Ingest(context.Background(), f); err != nil {
				t.Fatalf("round %d, namespace %q: %v", round, f.Namespace, err)
			}
		}
	}
	for ns, n := range patches {
		if got := n.Load(); got != 1 {
			t.Fatalf("namespace %q: %d status patches from 50 identical frames, want 1 (the establishing write)", ns, got)
		}
	}
	if got := getRobot(t, c, "robot-a", "staging").Status.BatteryPercent; got == nil || *got != 80 {
		t.Fatalf("staging robot battery = %v, want 80", got)
	}
	if got := getRobot(t, c, "robot-b", "production").Status.BatteryPercent; got == nil || *got != 20 {
		t.Fatalf("production robot battery = %v, want 20", got)
	}
}

// The zone path's RA-1 memos are per robot, and the same robot_id in two namespaces
// is two robots. Interleaved identical frames must issue no status patch after each
// robot's first placement; a memo shared by the two would read every frame as a
// transition. Counted on patches issued, because an identical patch changes nothing
// a resourceVersion check could see.
func TestZoneObserver_SameRobotIDInTwoNamespaces_IdenticalFramesPatchNothing(t *testing.T) {
	a := robotWithID("robot-a", "staging", "chassis-7")
	b := robotWithID("robot-b", "production", "chassis-7")
	z, c, patches := newCountingZoneController(t, a, b)
	z.setZones("staging", []zone.Candidate{{Name: "bay-a", Polygon: toZonePoints(rectPts), IsLeaf: true}})
	z.setZones("production", []zone.Candidate{{Name: "bay-b", Polygon: toZonePoints(rectPts), IsLeaf: true}})

	ctx := context.Background()
	pos := &telemetry.Position{X: 60, Y: 40}
	for i := 0; i < 20; i++ {
		z.ObservePosition(ctx, telemetry.Frame{RobotID: "chassis-7", Namespace: "staging", Position: pos})
		z.ObservePosition(ctx, telemetry.Frame{RobotID: "chassis-7", Namespace: "production", Position: pos})
	}
	if n := patches.Load(); n != 2 {
		t.Fatalf("%d robot status patches from 20 rounds of identical frames, want 2 (one placement per robot)", n)
	}
	if got := getRobot(t, c, "robot-a", "staging").Status.CurrentZone; got != "bay-a" {
		t.Fatalf("staging robot currentZone = %q, want bay-a", got)
	}
	if got := getRobot(t, c, "robot-b", "production").Status.CurrentZone; got != "bay-b" {
		t.Fatalf("production robot currentZone = %q, want bay-b", got)
	}
}
