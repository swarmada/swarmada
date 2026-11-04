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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/probe"
)

// probeClock is a manual clock for the cycle-pacing gate. Real time is unusable here:
// the property under test is that two reconciles inside one intervalSeconds issue one
// probe RPC, and asserting that against time.Now would need the test to sleep.
type probeClock struct{ t time.Time }

func (p *probeClock) advance(d time.Duration) { p.t = p.t.Add(d) }

func newPacedProbeReconciler(t *testing.T, prober probe.Prober, objs ...client.Object) (*RobotProbeReconciler, client.Client, *probeClock) {
	t.Helper()
	r, c := newProbeReconciler(t, prober, objs...)
	clk := &probeClock{t: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	r.now = func() time.Time { return clk.t }
	return r, c, clk
}

func pacedProbeCR(intervalSeconds int32) *fleetv1.RobotProbe {
	rp := probeCR(nil)
	rp.Spec.IntervalSeconds = intervalSeconds
	return rp
}

func reconcileRaw(t *testing.T, r *RobotProbeReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cam-probe", Namespace: rolloutNS},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// TestRobotProbe_CycleRateIsIntervalSeconds pins ADR-0050's cycle rate.
//
// RFC-0001 §9.1.7 makes spec.intervalSeconds the rate at which the control plane issues
// the probe RPC. The controller watches its own RobotProbe, so a cycle that changes
// status re-enqueues the object at once; before this gate the ramp to failureThreshold
// therefore ran at reconcile rate, and on the unreachable-adapter path (roundTrip returns
// ErrUnreachable without waiting) burned the threshold in milliseconds.
func TestRobotProbe_CycleRateIsIntervalSeconds(t *testing.T) {
	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
	r, _, clk := newPacedProbeReconciler(t, prober, pacedProbeCR(10), probeRobotObj("amr-1"))

	// First reconcile: the cycle is due, one Verify RPC, requeue at one interval.
	if res := reconcileRaw(t, r); res.RequeueAfter != 10*time.Second {
		t.Fatalf("first cycle requeue = %s, want 10s", res.RequeueAfter)
	}
	if prober.calls != 1 {
		t.Fatalf("first cycle: Verify calls = %d, want 1", prober.calls)
	}

	// The status patch the first cycle made re-enqueues the object immediately. That
	// wake must not become a second probe cycle: no RPC, and a requeue for the
	// remainder of the interval.
	clk.advance(50 * time.Millisecond)
	res := reconcileRaw(t, r)
	if prober.calls != 1 {
		t.Fatalf("self-triggered wake issued a probe RPC: calls = %d, want 1", prober.calls)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 10*time.Second {
		t.Fatalf("gated requeue = %s, want the remainder of the 10s interval", res.RequeueAfter)
	}

	// One interval later the next cycle runs.
	clk.advance(10 * time.Second)
	if reconcileRaw(t, r); prober.calls != 2 {
		t.Fatalf("after one interval: Verify calls = %d, want 2", prober.calls)
	}
}

// TestRobotProbe_FailureRampTakesFailureThresholdIntervals is the behavioural half of
// ADR-0050: an adapter that is unreachable — the failure mode most likely to flap, and the
// one that fails its RPC instantly — must still take failureThreshold paced cycles to
// cross, so failureThreshold × intervalSeconds is the time an operator reads it as.
func TestRobotProbe_FailureRampTakesFailureThresholdIntervals(t *testing.T) {
	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
	r, c, clk := newPacedProbeReconciler(t, prober, pacedProbeCR(10), probeRobotObj("amr-1"))

	reconcileRaw(t, r) // cycle 1 at t=0: Healthy establishes the effective baseline.
	clk.advance(10 * time.Second)
	reconcileRaw(t, r) // cycle 2 at t=10s: still Healthy.

	// The adapter goes away immediately after cycle 2. roundTrip's ErrUnreachable is
	// instant, so nothing but the pacing gate stands between the first failure and the
	// threshold. Hammer the reconciler every 500ms for the next 29 seconds, the way the
	// self-retriggering status watch does: two failing cycles fit (t=20s, t=30s), the
	// third does not.
	prober.result = probe.Result{}
	prober.err = errors.New(`unreachable: no ControlStream to adapter "sim-fleet-adapter"`)
	for elapsed := time.Duration(0); elapsed < 29*time.Second; elapsed += 500 * time.Millisecond {
		clk.advance(500 * time.Millisecond)
		reconcileRaw(t, r)
	}
	st := probeResult(t, c)
	if got := st.ConsecutiveFailures; got != 2 {
		t.Fatalf("29s unreachable: consecutiveFailures = %d, want 2 (threshold 3 not crossed)", got)
	}
	if string(st.LastProbeResult) != string(probe.StatusHealthy) {
		t.Fatalf("29s unreachable: lastProbeResult = %s, want Healthy (debounced)", st.LastProbeResult)
	}
	if string(st.RobotResults[0].ProbeStatus) != string(probe.StatusFailed) {
		t.Fatalf("raw per-robot result = %s, want Failed (undebounced, ADR-0012)", st.RobotResults[0].ProbeStatus)
	}

	// Held to failureThreshold × intervalSeconds — 30s — the third failing cycle crosses.
	clk.advance(time.Second)
	reconcileRaw(t, r)
	st = probeResult(t, c)
	if st.ConsecutiveFailures != 3 || string(st.LastProbeResult) != string(probe.StatusFailed) {
		t.Fatalf("30s unreachable: failures = %d, result = %s; want 3 / Failed",
			st.ConsecutiveFailures, st.LastProbeResult)
	}
}
