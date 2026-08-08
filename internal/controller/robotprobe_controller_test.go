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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/probe"
)

// fakeProber returns a canned result (or error) for every robot and records the
// last request so tests can assert target selection / synthetic input.
type fakeProber struct {
	result  probe.Result
	err     error
	calls   int
	lastReq probe.VerifyRequest
}

func (f *fakeProber) Verify(_ context.Context, _, _ string, req probe.VerifyRequest) (probe.Result, error) {
	f.calls++
	f.lastReq = req
	return f.result, f.err
}

func probeCR(expected map[string]string) *fleetv1.RobotProbe {
	return &fleetv1.RobotProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "cam-probe", Namespace: rolloutNS},
		Spec: fleetv1.RobotProbeSpec{
			RobotSelector:   metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			ProbeType:       fleetv1.ProbeTypeHardware,
			TargetComponent: "cam-front",
			ExpectedMetrics: expected,
		},
	}
}

func probeRobotObj(name string) *fleetv1.Robot {
	return &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: rolloutNS, Labels: map[string]string{"fleet": "pickers"},
	}}
}

func newProbeReconciler(t *testing.T, prober probe.Prober, objs ...client.Object) (*RobotProbeReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.RobotProbe{}).Build()
	return &RobotProbeReconciler{Client: c, Scheme: scheme, Prober: prober}, c
}

func reconcileProbe(t *testing.T, r *RobotProbeReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cam-probe", Namespace: rolloutNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func probeResult(t *testing.T, c client.Client) fleetv1.RobotProbeStatus {
	t.Helper()
	rp := &fleetv1.RobotProbe{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cam-probe", Namespace: rolloutNS}, rp); err != nil {
		t.Fatalf("get probe: %v", err)
	}
	return rp.Status
}

// A HEALTHY result with metrics met yields Healthy.
func TestRobotProbe_Healthy(t *testing.T) {
	prober := &fakeProber{result: probe.Result{
		Status: probe.StatusHealthy, ActualMetrics: map[string]float64{"frame_rate_pct": 95},
	}}
	r, c := newProbeReconciler(t, prober, probeCR(map[string]string{"frame_rate_pct": "80"}), probeRobotObj("amr-1"))
	reconcileProbe(t, r)

	st := probeResult(t, c)
	if string(st.LastProbeResult) != string(probe.StatusHealthy) || st.ConsecutiveFailures != 0 {
		t.Fatalf("status = %+v, want Healthy/0", st)
	}
}

// A HEALTHY result whose metrics are below threshold is downgraded to Degraded —
// a component out of spec is not healthy.
func TestRobotProbe_MetricsBelowThresholdDegrades(t *testing.T) {
	prober := &fakeProber{result: probe.Result{
		Status: probe.StatusHealthy, ActualMetrics: map[string]float64{"frame_rate_pct": 60},
	}}
	r, c := newProbeReconciler(t, prober, probeCR(map[string]string{"frame_rate_pct": "80"}), probeRobotObj("amr-1"))
	reconcileProbe(t, r)

	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusDegraded) {
		t.Fatalf("result = %s, want Degraded", st.LastProbeResult)
	}
}

// FAIL-SAFE: an RPC error is Failed, NEVER Healthy — a probe that cannot confirm
// health must not report health.
func TestRobotProbe_RPCErrorIsFailedNeverHealthy(t *testing.T) {
	prober := &fakeProber{err: errors.New("stream unreachable")}
	r, c := newProbeReconciler(t, prober, probeCR(nil), probeRobotObj("amr-1"))
	reconcileProbe(t, r)

	st := probeResult(t, c)
	if string(st.LastProbeResult) == string(probe.StatusHealthy) {
		t.Fatal("SAFETY: an unreachable probe reported Healthy")
	}
	if string(st.LastProbeResult) != string(probe.StatusFailed) || st.ConsecutiveFailures != 1 {
		t.Fatalf("status = %+v, want Failed/1", st)
	}
}

// An adapter that does not implement probes reports Unknown (never Healthy).
func TestRobotProbe_UnsupportedIsUnknown(t *testing.T) {
	prober := &fakeProber{result: probe.Result{Unsupported: true}}
	r, c := newProbeReconciler(t, prober, probeCR(nil), probeRobotObj("amr-1"))
	reconcileProbe(t, r)

	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusUnknown) {
		t.Fatalf("result = %s, want Unknown", st.LastProbeResult)
	}
}

// A nil prober (command-push not wired) reports Unknown, never Healthy.
func TestRobotProbe_NilProberIsUnknown(t *testing.T) {
	r, c := newProbeReconciler(t, nil, probeCR(nil), probeRobotObj("amr-1"))
	reconcileProbe(t, r)

	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusUnknown) {
		t.Fatalf("result = %s, want Unknown (no prober)", st.LastProbeResult)
	}
}

// The cycle result is the worst per-robot status, and the failure streak grows.
func TestRobotProbe_WorstOfAggregation(t *testing.T) {
	// One robot Failed dominates a Healthy one.
	prober := &fakeProber{err: errors.New("down")}
	r, c := newProbeReconciler(t, prober, probeCR(nil), probeRobotObj("amr-1"), probeRobotObj("amr-2"))
	reconcileProbe(t, r)
	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusFailed) {
		t.Fatalf("result = %s, want Failed (worst-of)", st.LastProbeResult)
	}
	// A second failing cycle grows the streak.
	reconcileProbe(t, r)
	if st := probeResult(t, c); st.ConsecutiveFailures != 2 {
		t.Fatalf("consecutiveFailures = %d, want 2", st.ConsecutiveFailures)
	}
}
