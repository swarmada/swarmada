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
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// A robot is geofenced under the robot_id its adapter reports positions under, on
// every admission path. The Robot's object name is not that id when an operator
// admits it under another name (`swarmctl admit --name`), and a geofence keyed by the
// object name would never match the robot's frames: its breach would be ignored as
// an unknown robot (RFC-0001 #edgenode).
func TestKubeConfigSource_KeysRobotsByTheirWireRobotID(t *testing.T) {
	renamed := robotInZone("warehouse-a", "picker-a", "zone-a") // admitted with --name
	renamed.Annotations = map[string]string{fleetv1.RobotIDAnnotation: "dr-acme-001"}
	same := robotInZone("warehouse-a", "amr-2", "zone-a") // name == robot_id (defaulter backfill)
	same.Annotations = map[string]string{fleetv1.RobotIDAnnotation: "amr-2"}
	bare := robotInZone("warehouse-a", "amr-3", "zone-a") // no annotation: falls back to the name

	c := fake.NewClientBuilder().WithScheme(edgeScheme(t)).WithObjects(
		fzWithBounds("warehouse-a", "zone-a", 0, squarePoints()), renamed, same, bare).Build()
	cfg, err := NewKubeConfigSource(c, "warehouse-a").Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for id, want := range map[string]string{"dr-acme-001": "zone-a", "amr-2": "zone-a", "amr-3": "zone-a"} {
		if got := cfg.RobotZone[id]; got != want {
			t.Errorf("RobotZone[%q] = %q, want %q (config %+v)", id, got, want, cfg.RobotZone)
		}
	}
	if _, byName := cfg.RobotZone["picker-a"]; byName {
		t.Errorf("RobotZone keyed a renamed robot by its object name: %+v", cfg.RobotZone)
	}

	// End to end: the renamed robot's out-of-zone frame, under its wire robot_id,
	// triggers the edge estop.
	n := New(cfg, audit.New(&audit.MemorySink{}, "v0.1.0"))
	n.deliveryTimeout = 60 * time.Millisecond
	n.confirmTimeout = 60 * time.Millisecond
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()
	fs.in <- posMsg("dr-acme-001", 50, 50, 0)
	select {
	case m := <-fs.out:
		if m.GetEstop().GetEstopId() == "" {
			t.Fatalf("message on breach is not an estop: %+v", m)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no edge estop for a robot admitted under another name: its breach was ignored")
	}
}
