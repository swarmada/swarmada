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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/probe"
	"github.com/swarmada/swarmada/internal/telemetry"
)

func envtestProbeRobot(ns, name, robotID string) *fleetv1.Robot {
	robot := envtestValidRobot(ns, name, "z1")
	robot.Labels = map[string]string{"fleet": "pickers"}
	robot.Annotations = map[string]string{RobotIDAnnotation: robotID}
	return robot
}

func envtestProbe(ns string, pt fleetv1.ProbeType) *fleetv1.RobotProbe {
	return &fleetv1.RobotProbe{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: ns},
		Spec: fleetv1.RobotProbeSpec{
			RobotSelector:    metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			ProbeType:        pt,
			TargetComponent:  "cam-front",
			TargetCapability: "navigation",
			TargetModel:      "pick-net",
		},
	}
}

// Real API server: with disableAllProbes set, the loop issues NO Verify RPCs and
// persists Unknown/paused; passive telemetry still writes Robot.status; and
// flipping the switch off resumes RPCs.
func TestEnvtest_DisableAllProbes(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	cfg := &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: ns},
		Spec:       fleetv1.SwarmadaConfigSpec{Health: fleetv1.SwarmadaHealthConfig{DisableAllProbes: zmBoolPtr(true)}},
	}
	if err := envK8s.Create(ctx, cfg); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := envK8s.Create(ctx, envtestProbeRobot(ns, "amr-1", "rid-1")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	if err := envK8s.Create(ctx, envtestProbe(ns, fleetv1.ProbeTypeHardware)); err != nil {
		t.Fatalf("create probe: %v", err)
	}

	prober := &fakeProber{result: probe.Result{Status: probe.StatusHealthy}}
	r := &RobotProbeReconciler{Client: envK8s, Scheme: envScheme, Prober: prober}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "probe", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (disabled): %v", err)
	}
	if prober.calls != 0 {
		t.Fatalf("Verify RPC issued while disabled: %d", prober.calls)
	}
	got := &fleetv1.RobotProbe{}
	if err := envK8s.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if string(got.Status.LastProbeResult) != string(probe.StatusUnknown) {
		t.Fatalf("lastProbeResult = %s, want Unknown/paused (persisted)", got.Status.LastProbeResult)
	}

	// Passive telemetry still flows to Robot.status while probes are disabled.
	sink := &RobotStatusSink{Client: envK8s}
	if err := sink.ApplyMaterialUpdate(ctx, telemetry.MaterialUpdate{RobotID: "rid-1", BatteryPct: i32(55)}); err != nil {
		t.Fatalf("apply telemetry: %v", err)
	}
	gotRobot := &fleetv1.Robot{}
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "amr-1", Namespace: ns}, gotRobot); err != nil {
		t.Fatal(err)
	}
	if gotRobot.Status.BatteryPercent == nil || *gotRobot.Status.BatteryPercent != 55 {
		t.Fatalf("batteryPercent = %v, want 55 (passive telemetry must flow while probes disabled)", gotRobot.Status.BatteryPercent)
	}

	// Re-enable → RPCs resume.
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "swarmada-config", Namespace: ns}, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Spec.Health.DisableAllProbes = zmBoolPtr(false)
	if err := envK8s.Update(ctx, cfg); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (enabled): %v", err)
	}
	if prober.calls == 0 {
		t.Fatal("Verify RPC not issued after re-enable")
	}
}

// Real API server: capability and model probe outcomes bind fail-safe and persist
// through the RobotProbe status subresource.
func TestEnvtest_CapabilityModelProbeOutcomes(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	cases := []struct {
		name      string
		probeType fleetv1.ProbeType
		result    probe.Result
		err       error
		want      probe.Status
	}{
		{"capability healthy", fleetv1.ProbeTypeCapability, probe.Result{Status: probe.StatusHealthy}, nil, probe.StatusHealthy},
		{"capability unsupported", fleetv1.ProbeTypeCapability, probe.Result{Unsupported: true}, nil, probe.StatusUnknown},
		{"capability unreachable", fleetv1.ProbeTypeCapability, probe.Result{}, errors.New("stream unreachable"), probe.StatusFailed},
		{"model healthy", fleetv1.ProbeTypeModel, probe.Result{Status: probe.StatusHealthy}, nil, probe.StatusHealthy},
		{"model unsupported", fleetv1.ProbeTypeModel, probe.Result{Unsupported: true}, nil, probe.StatusUnknown},
		{"model timeout", fleetv1.ProbeTypeModel, probe.Result{}, errors.New("no CommandResult within 5s"), probe.StatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := envtestNamespace(t)
			if err := envK8s.Create(ctx, envtestProbeRobot(ns, "amr-1", "rid-1")); err != nil {
				t.Fatalf("create robot: %v", err)
			}
			if err := envK8s.Create(ctx, envtestProbe(ns, tc.probeType)); err != nil {
				t.Fatalf("create probe: %v", err)
			}
			r := &RobotProbeReconciler{Client: envK8s, Scheme: envScheme, Prober: &fakeProber{result: tc.result, err: tc.err}}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "probe", Namespace: ns}}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := &fleetv1.RobotProbe{}
			if err := envK8s.Get(ctx, req.NamespacedName, got); err != nil {
				t.Fatal(err)
			}
			if string(got.Status.LastProbeResult) != string(tc.want) {
				t.Fatalf("lastProbeResult = %s, want %s", got.Status.LastProbeResult, tc.want)
			}
		})
	}
}
