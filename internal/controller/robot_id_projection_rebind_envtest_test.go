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
	"errors"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// The projection rebind (RobotProjectionResetter) with one robot_id on a Robot in
// each of two namespaces (RFC-0001 #discoveredrobot-conflict-handling-duplicate-robot-id,
// Case 3). Every Robot reconcile observes its robot_id binding, and the two Robots
// have different UIDs. If the binding were keyed by robot_id alone, each reconcile of
// one Robot would read as a change of identity, drop both robots' projector state,
// and the next unchanged frame for each would issue an establishing status patch.
//
// Patches are counted as issued, per namespace and per writer. The resourceVersion
// is read only to decide whether a watch event would have fired: the Robot watch has
// no predicate, so any change a status patch makes enqueues that Robot, and an
// identical patch changes nothing and enqueues nothing.

// patchCounter counts status patches issued through a client, by namespace.
type patchCounter struct {
	mu    sync.Mutex
	n     map[string]int
	empty map[string]int // patches whose body is {}: they change nothing
}

func (p *patchCounter) get(ns string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n[ns]
}

func (p *patchCounter) wrap(c client.WithWatch) client.Client {
	p.n, p.empty = map[string]int{}, map[string]int{}
	return interceptor.NewClient(c, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			body, err := patch.Data(obj)
			if err != nil {
				return err
			}
			p.mu.Lock()
			p.n[obj.GetNamespace()]++
			if string(body) == "{}" {
				p.empty[obj.GetNamespace()]++
			}
			p.mu.Unlock()
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
}

// rebindHarness drives the real RobotReconciler, resetter, Router, Projector and
// RobotStatusSink against the envtest API server.
type rebindHarness struct {
	t          *testing.T
	robotID    string
	robots     []types.NamespacedName
	frames     []telemetry.Frame
	sinkWrites *patchCounter
	reconciler *RobotReconciler
	router     *telemetry.Router
	lastRV     map[types.NamespacedName]string
	reconciles int
}

func newRebindHarness(t *testing.T, robotID, nameA, nameB string) *rebindHarness {
	t.Helper()
	nsA, nsB := twoNamespaceRobots(t, robotID, nameA, nameB, "dock", "dock")
	h := &rebindHarness{
		t:          t,
		robotID:    robotID,
		robots:     []types.NamespacedName{{Namespace: nsA, Name: nameA}, {Namespace: nsB, Name: nameB}},
		sinkWrites: &patchCounter{},
		lastRV:     map[types.NamespacedName]string{},
	}
	withWatch, ok := envK8s.(client.WithWatch)
	if !ok {
		// The interceptor needs a WithWatch; wrap the plain client as one that cannot watch.
		withWatch = noWatch{envK8s}
	}
	sinkClient := h.sinkWrites.wrap(withWatch)
	h.router = telemetry.NewRouter(
		func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		&RobotStatusSink{Client: sinkClient},
	)
	resetter := &RobotProjectionResetter{}
	resetter.SetForgetter(h.router)
	h.reconciler = &RobotReconciler{Client: envK8s, Scheme: envScheme, Projection: resetter}
	// Unchanged frames: the same battery and hardware every time. The wire robot_id is
	// bare, so both land on the "" plane, as a reference adapter's frames do.
	h.frames = []telemetry.Frame{
		{RobotID: robotID, Namespace: nsA, BatteryPct: i32(80),
			Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareHealthy}},
		{RobotID: robotID, Namespace: nsB, BatteryPct: i32(20),
			Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareHealthy}},
	}
	return h
}

type noWatch struct{ client.Client }

func (noWatch) Watch(context.Context, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
	return nil, errors.New("watch is not used by this harness")
}

func (h *rebindHarness) reconcile(key types.NamespacedName) {
	h.t.Helper()
	h.reconciles++
	if _, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		h.t.Fatalf("reconcile %s: %v", key, err)
	}
}

// deliverWatchEvents reconciles every Robot whose resourceVersion moved since it was
// last seen, repeating until none did — what the unfiltered Robot watch does.
func (h *rebindHarness) deliverWatchEvents() {
	h.t.Helper()
	for pass := 0; pass < 20; pass++ {
		moved := false
		for _, key := range h.robots {
			rv := etRobot(h.t, key.Namespace, key.Name).ResourceVersion
			if rv == h.lastRV[key] {
				continue
			}
			moved = true
			h.reconcile(key)
			h.lastRV[key] = etRobot(h.t, key.Namespace, key.Name).ResourceVersion
		}
		if !moved {
			return
		}
	}
	h.t.Fatal("watch events did not quiesce after 20 passes")
}

func (h *rebindHarness) streamRound() {
	h.t.Helper()
	for _, f := range h.frames {
		if err := h.router.Ingest(context.Background(), f); err != nil {
			h.t.Fatalf("ingest %s/%s: %v", f.Namespace, f.RobotID, err)
		}
	}
}

func (h *rebindHarness) sinkPatches() (a, b int) {
	return h.sinkWrites.get(h.robots[0].Namespace), h.sinkWrites.get(h.robots[1].Namespace)
}

// settle runs the create-time reconciles and the first frames, and returns once
// nothing more is pending: each robot has had its establishing write.
func (h *rebindHarness) settle() {
	h.t.Helper()
	h.deliverWatchEvents()
	for i := 0; i < 3; i++ {
		h.streamRound()
		h.deliverWatchEvents()
	}
}

// Unchanged frames from both robots, with every watch event the status writes
// cause delivered as a reconcile. After each robot's establishing write, no frame
// issues a status patch to either robot.
func TestEnvtest_OneRobotIDInTwoNamespaces_UnchangedFramesIssueNoPatches(t *testing.T) {
	requireEnvtest(t)
	h := newRebindHarness(t, "rebind-stream-chassis", "rebind-stream-a", "rebind-stream-b")
	h.settle()

	a0, b0 := h.sinkPatches()
	t.Logf("settling: %d status patches to %s, %d to %s, %d reconciles",
		a0, h.robots[0].Namespace, b0, h.robots[1].Namespace, h.reconciles)
	r0 := h.reconciles
	const rounds = 50
	for i := 0; i < rounds; i++ {
		h.streamRound()
		h.deliverWatchEvents()
	}
	a1, b1 := h.sinkPatches()
	t.Logf("%d rounds of unchanged frames: %d status patches to %s, %d to %s, %d reconciles",
		rounds, a1-a0, h.robots[0].Namespace, b1-b0, h.robots[1].Namespace, h.reconciles-r0)
	if a1 != a0 || b1 != b0 {
		t.Fatalf("%d rounds of unchanged frames issued %d status patches to %s and %d to %s, want 0 and 0 (RA-1)",
			rounds, a1-a0, h.robots[0].Namespace, b1-b0, h.robots[1].Namespace)
	}
}

// The periodic requeue reconciles each Robot every heartbeatTimeout/2 whether or not
// anything changed. A reconcile of one robot must not make the other robot's next
// unchanged frame, or its own, issue a status patch.
func TestEnvtest_OneRobotIDInTwoNamespaces_ReconcilesCauseNoPatches(t *testing.T) {
	requireEnvtest(t)
	h := newRebindHarness(t, "rebind-requeue-chassis", "rebind-requeue-a", "rebind-requeue-b")
	h.settle()

	a0, b0 := h.sinkPatches()
	e0 := h.sinkWrites.empty[h.robots[0].Namespace] + h.sinkWrites.empty[h.robots[1].Namespace]
	r0 := h.reconciles
	const rounds = 50
	for i := 0; i < rounds; i++ {
		h.reconcile(h.robots[0]) // requeue of the first robot
		h.streamRound()
		h.deliverWatchEvents()
		h.reconcile(h.robots[1]) // requeue of the second robot
		h.streamRound()
		h.deliverWatchEvents()
	}
	a1, b1 := h.sinkPatches()
	e1 := h.sinkWrites.empty[h.robots[0].Namespace] + h.sinkWrites.empty[h.robots[1].Namespace]
	t.Logf("%d requeue rounds (2 reconciles, 2 frame rounds each): %d status patches to %s, %d to %s; "+
		"%d of them with an empty body; %d reconciles, %d of them watch-driven",
		rounds, a1-a0, h.robots[0].Namespace, b1-b0, h.robots[1].Namespace,
		e1-e0, h.reconciles-r0, h.reconciles-r0-2*rounds)
	if a1 != a0 || b1 != b0 {
		t.Fatalf("%d requeue rounds of unchanged frames issued %d status patches to %s and %d to %s, want 0 and 0 (RA-1)",
			rounds, a1-a0, h.robots[0].Namespace, b1-b0, h.robots[1].Namespace)
	}
}

// Frames that are unchanged in the projector's sense but not byte-identical: the
// battery moves by one percent inside its bucket every frame, as a real battery
// reading does. The projector writes nothing for an in-bucket move, so after each
// robot's establishing write no frame issues a status patch to either robot.
func TestEnvtest_OneRobotIDInTwoNamespaces_InBucketDriftIssuesNoPatches(t *testing.T) {
	requireEnvtest(t)
	h := newRebindHarness(t, "rebind-drift-chassis", "rebind-drift-a", "rebind-drift-b")
	h.settle()

	a0, b0 := h.sinkPatches()
	r0 := h.reconciles
	const rounds = 50
	for i := 0; i < rounds; i++ {
		// 80/79 and 20/21 stay inside the default buckets (thresholds 15 and 30).
		h.frames[0].BatteryPct = i32(80 - int32(i%2))
		h.frames[1].BatteryPct = i32(20 + int32(i%2))
		h.streamRound()
		h.deliverWatchEvents()
	}
	a1, b1 := h.sinkPatches()
	t.Logf("%d rounds of in-bucket drift: %d status patches to %s, %d to %s, %d watch-driven reconciles",
		rounds, a1-a0, h.robots[0].Namespace, b1-b0, h.robots[1].Namespace, h.reconciles-r0)
	if a1 != a0 || b1 != b0 {
		t.Fatalf("%d rounds of in-bucket drift issued %d status patches to %s and %d to %s, want 0 and 0 (RA-1)",
			rounds, a1-a0, h.robots[0].Namespace, b1-b0, h.robots[1].Namespace)
	}
}

// One periodic reconcile, then in-bucket drift with every watch event delivered. The
// reconcile is the only thing that could drop projector state; after it, at most one
// establishing patch per robot, and then none.
func TestEnvtest_OneRobotIDInTwoNamespaces_OneReconcileThenDriftStaysFlat(t *testing.T) {
	requireEnvtest(t)
	h := newRebindHarness(t, "rebind-kick-chassis", "rebind-kick-a", "rebind-kick-b")
	h.settle()

	h.reconcile(h.robots[0]) // one requeue of the first robot
	a0, b0 := h.sinkPatches()
	r0 := h.reconciles
	const rounds = 50
	for i := 0; i < rounds; i++ {
		// Odd phase: the first frame after the reconcile differs from the persisted value.
		h.frames[0].BatteryPct = i32(80 - int32((i+1)%2))
		h.frames[1].BatteryPct = i32(20 + int32((i+1)%2))
		h.streamRound()
		h.deliverWatchEvents()
	}
	a1, b1 := h.sinkPatches()
	t.Logf("one reconcile then %d rounds of in-bucket drift: %d status patches to %s, %d to %s, %d watch-driven reconciles",
		rounds, a1-a0, h.robots[0].Namespace, b1-b0, h.robots[1].Namespace, h.reconciles-r0)
	if a1-a0 > 1 || b1-b0 > 1 {
		t.Fatalf("one reconcile then %d rounds of in-bucket drift issued %d status patches to %s and %d to %s, "+
			"want at most 1 each (RA-1: a write per frame is write amplification at telemetry frequency)",
			rounds, a1-a0, h.robots[0].Namespace, b1-b0, h.robots[1].Namespace)
	}
}
