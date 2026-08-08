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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// RFC-0001 §9.1.6.5 step 2: a SUSTAINED probe failure degrades the component in
// Robot.status.hardware[]; recovery clears it.
//
// The rule under test is the EDGE, not the state. Streaks are clamped at their thresholds, so a
// sustained failure reads `failures == failureT` on every subsequent tick — acting on the state
// would mean a Robot status write per probe cycle, which is exactly the per-tick write RA-1
// forbids. Only the crossing may act, and that same property is what makes a sub-threshold flap
// (fail, fail, recover, with threshold 3) write nothing at all.

func hwProbe(probeType fleetv1.ProbeType, target string) *fleetv1.RobotProbe {
	rp := &fleetv1.RobotProbe{Spec: fleetv1.RobotProbeSpec{ProbeType: probeType}}
	switch probeType {
	case fleetv1.ProbeTypeCapability:
		rp.Spec.TargetCapability = target
	case fleetv1.ProbeTypeModel:
		rp.Spec.TargetModel = target
	default:
		rp.Spec.TargetComponent = target
	}
	return rp
}

func prevResult(failures, successes int32) fleetv1.RobotProbeRobotResult {
	return fleetv1.RobotProbeRobotResult{ConsecutiveFailures: failures, ConsecutiveSuccesses: successes}
}

// The crossing fires exactly once, on the tick that reaches the threshold.
func TestProbeHardwareEdge_FiresOnlyOnTheCrossing(t *testing.T) {
	rp := hwProbe(fleetv1.ProbeTypeHardware, "camera-front")
	const failT, recT int32 = 3, 2

	// 2 -> 3 is the crossing.
	edge, target := probeHardwareEdge(rp, prevResult(2, 0), 3, 0, failT, recT)
	if edge != hwEdgeDegraded || target != "camera-front" {
		t.Fatalf("crossing to failed: edge=%v target=%q, want degraded/camera-front", edge, target)
	}

	// Already at the threshold and staying there — the sustained state must NOT re-fire, or every
	// subsequent probe cycle writes Robot status.
	if edge, _ := probeHardwareEdge(rp, prevResult(3, 0), 3, 0, failT, recT); edge != hwEdgeNone {
		t.Errorf("sustained failure re-fired (edge=%v); a held state must not write every tick", edge)
	}
}

// A flap that never reaches the threshold produces no edge at all.
func TestProbeHardwareEdge_SubThresholdFlapIsSilent(t *testing.T) {
	rp := hwProbe(fleetv1.ProbeTypeHardware, "camera-front")
	const failT, recT int32 = 3, 2

	for _, tc := range []struct {
		name               string
		prevF, prevS, f, s int32
	}{
		{"first failure", 0, 1, 1, 0},
		{"second failure", 1, 0, 2, 0},
		{"recovered before threshold", 2, 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if edge, _ := probeHardwareEdge(rp, prevResult(tc.prevF, tc.prevS), tc.f, tc.s, failT, recT); edge != hwEdgeNone {
				t.Errorf("edge=%v for a sub-threshold tick; want none", edge)
			}
		})
	}
}

// Recovery crosses on reaching recoveryThreshold, and only then.
func TestProbeHardwareEdge_RecoveryCrossing(t *testing.T) {
	rp := hwProbe(fleetv1.ProbeTypeHardware, "camera-front")
	const failT, recT int32 = 3, 2

	if edge, _ := probeHardwareEdge(rp, prevResult(0, 1), 0, 2, failT, recT); edge != hwEdgeHealthy {
		t.Errorf("1 -> 2 successes: edge=%v, want healthy", edge)
	}
	if edge, _ := probeHardwareEdge(rp, prevResult(0, 2), 0, 2, failT, recT); edge != hwEdgeNone {
		t.Errorf("sustained healthy re-fired (edge=%v)", edge)
	}
}

// Capability and model probes never touch hardware — degrading hardware from a capability result
// would invert §6.10's derivation direction and create a feedback loop.
func TestProbeHardwareEdge_OnlyHardwareProbesPropagate(t *testing.T) {
	const failT, recT int32 = 3, 2
	for _, pt := range []fleetv1.ProbeType{fleetv1.ProbeTypeCapability, fleetv1.ProbeTypeModel} {
		t.Run(string(pt), func(t *testing.T) {
			rp := hwProbe(pt, "item-pick.ai-guided")
			if edge, _ := probeHardwareEdge(rp, prevResult(2, 0), 3, 0, failT, recT); edge != hwEdgeNone {
				t.Errorf("%s probe produced edge=%v; only hardware probes may write status.hardware", pt, edge)
			}
		})
	}
}

// A hardware probe with no targetComponent has nothing to address.
func TestProbeHardwareEdge_NoTargetIsNoOp(t *testing.T) {
	rp := hwProbe(fleetv1.ProbeTypeHardware, "")
	if edge, _ := probeHardwareEdge(rp, prevResult(2, 0), 3, 0, 3, 2); edge != hwEdgeNone {
		t.Errorf("edge=%v with an empty targetComponent, want none", edge)
	}
}

// ── the write itself ──────────────────────────────────────────────────────────

func robotWithHW(name string, comps ...fleetv1.HardwareComponentStatus) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "warehouse-a"},
		Status:     fleetv1.RobotStatus{Hardware: comps},
	}
}

// Degrading writes the component, the reason, and leaves siblings alone.
func TestPropagateHardwareStatus_DegradesOnlyTheTarget(t *testing.T) {
	r, c := newProbePropagationReconciler(t)
	robot := robotWithHW("amr-1",
		fleetv1.HardwareComponentStatus{Name: "camera-front", Status: fleetv1.HardwareHealthy},
		fleetv1.HardwareComponentStatus{Name: "lidar-top", Status: fleetv1.HardwareHealthy},
	)
	mustCreateRobot(t, c, robot)

	if err := r.propagateHardwareStatus(testCtx(), robot, "camera-front",
		fleetv1.HardwareDegraded, "depth pipeline stalled", nil); err != nil {
		t.Fatalf("propagate: %v", err)
	}

	got := getRobotByName(t, c, "amr-1")
	hw := hwByName(got)
	if hw["camera-front"].Status != fleetv1.HardwareDegraded {
		t.Errorf("camera-front = %s, want Degraded", hw["camera-front"].Status)
	}
	if hw["camera-front"].DegradationReason != "depth pipeline stalled" {
		t.Errorf("degradationReason = %q", hw["camera-front"].DegradationReason)
	}
	if hw["lidar-top"].Status != fleetv1.HardwareHealthy {
		t.Errorf("lidar-top = %s, want an untouched Healthy", hw["lidar-top"].Status)
	}
}

// A component the robot does not inventory is a no-op — a probe must not invent hardware the
// adapter never reported.
func TestPropagateHardwareStatus_UnknownComponentIsNoOp(t *testing.T) {
	r, c := newProbePropagationReconciler(t)
	robot := robotWithHW("amr-1", fleetv1.HardwareComponentStatus{Name: "lidar-top", Status: fleetv1.HardwareHealthy})
	mustCreateRobot(t, c, robot)

	if err := r.propagateHardwareStatus(testCtx(), robot, "camera-front",
		fleetv1.HardwareDegraded, "nope", nil); err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if n := len(getRobotByName(t, c, "amr-1").Status.Hardware); n != 1 {
		t.Errorf("hardware list grew to %d entries; a probe must not append inventory", n)
	}
}

// Already-correct state writes nothing.
func TestPropagateHardwareStatus_NoWriteWhenAlreadyThere(t *testing.T) {
	r, c, writes := newProbePropagationReconcilerCounting(t)
	robot := robotWithHW("amr-1", fleetv1.HardwareComponentStatus{
		Name: "camera-front", Status: fleetv1.HardwareDegraded, DegradationReason: "stalled",
	})
	mustCreateRobot(t, c, robot)

	for i := 0; i < 3; i++ {
		if err := r.propagateHardwareStatus(testCtx(), robot, "camera-front",
			fleetv1.HardwareDegraded, "stalled", nil); err != nil {
			t.Fatalf("propagate: %v", err)
		}
	}
	if *writes != 0 {
		t.Errorf("status writes = %d for an already-correct component, want 0", *writes)
	}
}

// Recovery clears the reason and stamps lastHealthyAt.
func TestPropagateHardwareStatus_RecoveryClearsReason(t *testing.T) {
	r, c := newProbePropagationReconciler(t)
	robot := robotWithHW("amr-1", fleetv1.HardwareComponentStatus{
		Name: "camera-front", Status: fleetv1.HardwareDegraded, DegradationReason: "stalled",
	})
	mustCreateRobot(t, c, robot)

	if err := r.propagateHardwareStatus(testCtx(), robot, "camera-front",
		fleetv1.HardwareHealthy, "", nil); err != nil {
		t.Fatalf("propagate: %v", err)
	}
	hw := hwByName(getRobotByName(t, c, "amr-1"))["camera-front"]
	if hw.Status != fleetv1.HardwareHealthy {
		t.Errorf("status = %s, want Healthy", hw.Status)
	}
	if hw.DegradationReason != "" {
		t.Errorf("degradationReason = %q, want cleared on recovery", hw.DegradationReason)
	}
	if hw.LastHealthyAt == nil {
		t.Error("lastHealthyAt was not stamped on recovery")
	}
}

// ── fixtures ──────────────────────────────────────────────────────────────────

func testCtx() context.Context { return context.Background() }

func probeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

func newProbePropagationReconciler(t *testing.T) (*RobotProbeReconciler, client.Client) {
	t.Helper()
	s := probeScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&fleetv1.Robot{}).Build()
	return &RobotProbeReconciler{Client: c, Scheme: s}, c
}

func newProbePropagationReconcilerCounting(t *testing.T) (*RobotProbeReconciler, client.Client, *int) {
	t.Helper()
	s := probeScheme(t)
	writes := 0
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, isRobot := obj.(*fleetv1.Robot); isRobot {
					writes++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	return &RobotProbeReconciler{Client: c, Scheme: s}, c, &writes
}

func mustCreateRobot(t *testing.T, c client.Client, robot *fleetv1.Robot) {
	t.Helper()
	status := robot.Status
	if err := c.Create(testCtx(), robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	robot.Status = status
	if err := c.Status().Update(testCtx(), robot); err != nil {
		t.Fatalf("seed robot status: %v", err)
	}
}

func getRobotByName(t *testing.T, c client.Client, name string) *fleetv1.Robot {
	t.Helper()
	var r fleetv1.Robot
	if err := c.Get(testCtx(), types.NamespacedName{Name: name, Namespace: "warehouse-a"}, &r); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return &r
}

func hwByName(r *fleetv1.Robot) map[string]fleetv1.HardwareComponentStatus {
	m := make(map[string]fleetv1.HardwareComponentStatus, len(r.Status.Hardware))
	for _, h := range r.Status.Hardware {
		m[h.Name] = h
	}
	return m
}
