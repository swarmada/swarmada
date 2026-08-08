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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// §9.1.6.5 end to end against a real API server: a sustained probe failure degrades the component,
// and the §6.10 derivation reacts by dropping the capability that requires it — the whole point of
// the propagation. Recovery reverses both.
//
// Run against envtest rather than a fake client because the property is the HANDOFF between two
// controllers through the /status subresource: the probe controller writes status.hardware[], and
// the Robot controller re-derives capabilities from what the API server actually holds.

// probeRobotWithCapability builds a robot whose "inspection.visual" capability is hardware-native on
// camera-front, so degrading that component must drop the capability.
func probeRobotWithCapability(ns, name string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "Acme", Model: "X1",
			Adapter: fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
			Zone:    "z-a",
			Hardware: []fleetv1.HardwareComponent{
				{Name: "camera-front", Type: fleetv1.HardwareTypeCamera},
			},
			Capabilities: []fleetv1.ClassCapability{{
				Name:             "inspection.visual",
				Type:             fleetv1.CapabilityKindHardwareNative,
				RequiredHardware: []string{"camera-front"},
			}},
		},
	}
}

func capState(t *testing.T, r *fleetv1.Robot, name string) fleetv1.CapabilityStatus {
	t.Helper()
	for _, c := range r.Status.Capabilities {
		if c.Name == name {
			return c.Status
		}
	}
	t.Fatalf("capability %q absent from status: %+v", name, r.Status.Capabilities)
	return ""
}

func TestEnvtest_ProbeFailureDegradesHardwareAndDropsCapability(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	probeR := &RobotProbeReconciler{Client: envK8s, Scheme: envScheme}
	robotR := &RobotReconciler{Client: envK8s, Scheme: envScheme}

	robot := probeRobotWithCapability(ns, "amr-1")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{
		{Name: "camera-front", Status: fleetv1.HardwareHealthy},
	}
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed robot status: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "amr-1", Namespace: ns}}
	if _, err := robotR.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile robot: %v", err)
	}
	var got fleetv1.Robot
	if err := envK8s.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if s := capState(t, &got, "inspection.visual"); s != fleetv1.CapabilityStatusActive {
		t.Fatalf("precondition: capability = %s, want Active on healthy hardware", s)
	}

	// The probe crosses failureThreshold → §9.1.6.5 step 2.
	if err := probeR.propagateHardwareStatus(ctx, &got, "camera-front",
		fleetv1.HardwareDegraded, "frame_rate_pct=48 (threshold 80)", nil); err != nil {
		t.Fatalf("propagate degrade: %v", err)
	}

	// Step 3: the derivation reacts to the status watch event.
	if _, err := robotR.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile robot after degrade: %v", err)
	}
	if err := envK8s.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if hw := hwByName(&got)["camera-front"]; hw.Status != fleetv1.HardwareDegraded {
		t.Errorf("camera-front = %s, want Degraded", hw.Status)
	}
	if s := capState(t, &got, "inspection.visual"); s == fleetv1.CapabilityStatusActive {
		t.Errorf("capability stayed %s after its required hardware degraded; the derivation did not react", s)
	}

	// Recovery reverses both.
	if err := probeR.propagateHardwareStatus(ctx, &got, "camera-front",
		fleetv1.HardwareHealthy, "", nil); err != nil {
		t.Fatalf("propagate recovery: %v", err)
	}
	if _, err := robotR.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile robot after recovery: %v", err)
	}
	if err := envK8s.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if hw := hwByName(&got)["camera-front"]; hw.Status != fleetv1.HardwareHealthy {
		t.Errorf("camera-front = %s after recovery, want Healthy", hw.Status)
	}
	if s := capState(t, &got, "inspection.visual"); s != fleetv1.CapabilityStatusActive {
		t.Errorf("capability = %s after recovery, want Active restored", s)
	}
}
