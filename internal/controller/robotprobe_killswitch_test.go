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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/probe"
	"github.com/swarmada/swarmada/internal/telemetry"
)

func probeConfig(disable bool) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: rolloutNS},
		Spec:       fleetv1.SwarmadaConfigSpec{Health: fleetv1.SwarmadaHealthConfig{DisableAllProbes: &disable}},
	}
}

// With disableAllProbes set, the loop issues NO Verify RPCs and reports every
// matched robot as Unknown/paused. Passive telemetry is untouched (not exercised
// here — the probe loop never drives it).
func TestRobotProbe_DisableAllProbes_PausesWithoutRPC(t *testing.T) {
	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy, ActualMetrics: map[string]float64{"frame_rate_pct": 95}}}
	r, c := newProbeReconciler(t, prober,
		probeConfig(true),
		probeCR(map[string]string{"frame_rate_pct": "80"}),
		probeRobotObj("amr-1"),
	)
	reconcileProbe(t, r)

	if prober.calls != 0 {
		t.Fatalf("prober called %d times while disabled; want 0 (no Verify RPCs)", prober.calls)
	}
	st := probeResult(t, c)
	if string(st.LastProbeResult) != string(probe.StatusUnknown) {
		t.Fatalf("lastProbeResult = %s, want Unknown (paused)", st.LastProbeResult)
	}
	if len(st.RobotResults) != 1 || string(st.RobotResults[0].ProbeStatus) != string(probe.StatusUnknown) {
		t.Fatalf("robot results = %+v, want one Unknown entry", st.RobotResults)
	}
}

// RA-1: while disabled, the paused status is written once, then a second cycle
// produces no write.
func TestRobotProbe_DisableAllProbes_RA1NoChurn(t *testing.T) {
	r, c := newProbeReconciler(t, &fakeProber{}, probeConfig(true), probeCR(nil), probeRobotObj("amr-1"))
	reconcileProbe(t, r)
	rv1 := getProbeRV(t, c)
	reconcileProbe(t, r)
	if rv2 := getProbeRV(t, c); rv2 != rv1 {
		t.Fatalf("paused status churned (rv %s → %s) — RA-1 violated", rv1, rv2)
	}
}

// Flipping the switch back re-enables probing on the next reconcile.
func TestRobotProbe_DisableAllProbes_ResumesOnReenable(t *testing.T) {
	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
	r, c := newProbeReconciler(t, prober, probeConfig(true), probeCR(nil), probeRobotObj("amr-1"))
	reconcileProbe(t, r)
	if prober.calls != 0 {
		t.Fatalf("prober called while disabled: %d", prober.calls)
	}

	// Re-enable: disableAllProbes → false.
	cfg := &fleetv1.SwarmadaConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "swarmada-config", Namespace: rolloutNS}, cfg); err != nil {
		t.Fatal(err)
	}
	off := false
	cfg.Spec.Health.DisableAllProbes = &off
	if err := c.Update(context.Background(), cfg); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	// RPCs resume immediately; the reported status re-confirms through the normal
	// debounce (recoveryThreshold=2 default) rather than trusting one probe after a
	// pause (ADR-0024, fail-safe direction).
	reconcileProbe(t, r)
	if prober.calls == 0 {
		t.Fatal("prober not called after re-enable")
	}
	reconcileProbe(t, r) // second Healthy cycle reaches recoveryThreshold
	if st := probeResult(t, c); string(st.LastProbeResult) != string(probe.StatusHealthy) {
		t.Fatalf("lastProbeResult = %s, want Healthy after recovery threshold", st.LastProbeResult)
	}
}

// The verify target and syntheticInput are selected by probeType.
func TestRobotProbe_TargetSelectionByType(t *testing.T) {
	cases := []struct {
		name       string
		probeType  fleetv1.ProbeType
		wantTarget string
		wantInput  bool
	}{
		{"hardware uses targetComponent", fleetv1.ProbeTypeHardware, "cam-front", false},
		{"capability uses targetCapability", fleetv1.ProbeTypeCapability, "navigation", false},
		{"model uses targetModel + syntheticInput", fleetv1.ProbeTypeModel, "pick-net", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
			rp := probeCR(nil)
			rp.Spec.ProbeType = tc.probeType
			rp.Spec.TargetCapability = "navigation"
			rp.Spec.TargetModel = "pick-net"
			rp.Spec.SyntheticInput = []byte("frame")
			r, _ := newProbeReconciler(t, prober, rp, probeRobotObj("amr-1"))
			reconcileProbe(t, r)

			if prober.lastReq.ProbeType != tc.probeType {
				t.Fatalf("probeType = %s, want %s", prober.lastReq.ProbeType, tc.probeType)
			}
			if prober.lastReq.Target != tc.wantTarget {
				t.Fatalf("target = %q, want %q", prober.lastReq.Target, tc.wantTarget)
			}
			if (len(prober.lastReq.SyntheticInput) > 0) != tc.wantInput {
				t.Fatalf("syntheticInput present = %v, want %v", len(prober.lastReq.SyntheticInput) > 0, tc.wantInput)
			}
		})
	}
}

// The fail-safe binding holds for EVERY probe type: unsupported → Unknown,
// RPC error/timeout → Failed, and only a confirmed HEALTHY (with metrics met) →
// Healthy. Exercised for capability and model, not just hardware.
func TestRobotProbe_FailSafeByType(t *testing.T) {
	cases := []struct {
		name      string
		probeType fleetv1.ProbeType
		result    probe.Result
		err       error
		want      probe.Status
	}{
		{"capability confirmed healthy", fleetv1.ProbeTypeCapability, probe.Result{Status: probe.StatusHealthy}, nil, probe.StatusHealthy},
		{"capability unsupported → Unknown", fleetv1.ProbeTypeCapability, probe.Result{Unsupported: true}, nil, probe.StatusUnknown},
		{"capability unreachable → Failed", fleetv1.ProbeTypeCapability, probe.Result{}, errors.New("stream unreachable"), probe.StatusFailed},
		{"model confirmed healthy", fleetv1.ProbeTypeModel, probe.Result{Status: probe.StatusHealthy}, nil, probe.StatusHealthy},
		{"model unsupported → Unknown", fleetv1.ProbeTypeModel, probe.Result{Unsupported: true}, nil, probe.StatusUnknown},
		{"model timeout → Failed", fleetv1.ProbeTypeModel, probe.Result{}, errors.New("no CommandResult within 5s"), probe.StatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp := probeCR(nil)
			rp.Spec.ProbeType = tc.probeType
			rp.Spec.TargetCapability = "navigation"
			rp.Spec.TargetModel = "pick-net"
			r, c := newProbeReconciler(t, &fakeProber{result: tc.result, err: tc.err}, rp, probeRobotObj("amr-1"))
			reconcileProbe(t, r)

			st := probeResult(t, c)
			if string(st.LastProbeResult) != string(tc.want) {
				t.Fatalf("lastProbeResult = %s, want %s", st.LastProbeResult, tc.want)
			}
			if tc.want != probe.StatusHealthy && string(st.LastProbeResult) == string(probe.StatusHealthy) {
				t.Fatal("SAFETY: a non-healthy outcome reported Healthy")
			}
		})
	}
}

// With probes disabled, active verification is suspended but PASSIVE telemetry
// still flows: a MaterialUpdate from the telemetry sink updates Robot.status
// regardless of the kill-switch (the telemetry path never reads disableAllProbes).
func TestRobotProbe_PassiveTelemetryFlowsWhileProbesDisabled(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	robot := probeRobotObj("amr-1")
	robot.Annotations = map[string]string{RobotIDAnnotation: "rid-1"}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(probeConfig(true), probeCR(nil), robot).
		WithStatusSubresource(&fleetv1.RobotProbe{}, &fleetv1.Robot{}).Build()

	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
	pr := &RobotProbeReconciler{Client: c, Scheme: scheme, Prober: prober}
	reconcileProbe(t, pr)
	if prober.calls != 0 {
		t.Fatalf("active probe RPC issued while disabled: %d", prober.calls)
	}

	// Passive telemetry (battery projection) via the real status sink still writes.
	sink := &RobotStatusSink{Client: c}
	if err := sink.ApplyMaterialUpdate(context.Background(), telemetry.MaterialUpdate{RobotID: "rid-1", BatteryPct: i32(42)}); err != nil {
		t.Fatalf("apply telemetry: %v", err)
	}
	if got := getRobot(t, c, "amr-1", rolloutNS); got.Status.BatteryPercent == nil || *got.Status.BatteryPercent != 42 {
		t.Fatalf("batteryPercent = %v, want 42 (passive telemetry must flow while probes are disabled)", got.Status.BatteryPercent)
	}
}

func getProbeRV(t *testing.T, c client.Client) string {
	t.Helper()
	rp := &fleetv1.RobotProbe{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cam-probe", Namespace: rolloutNS}, rp); err != nil {
		t.Fatalf("get probe: %v", err)
	}
	return rp.ResourceVersion
}
