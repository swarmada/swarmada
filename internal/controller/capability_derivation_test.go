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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func hwNative(name string, pauseable bool, req ...string) fleetv1.ClassCapability {
	return fleetv1.ClassCapability{Name: name, Type: fleetv1.CapabilityKindHardwareNative, Pauseable: pauseable, RequiredHardware: req}
}

func modelDriven(name, model string, req ...string) fleetv1.ClassCapability {
	return fleetv1.ClassCapability{Name: name, Type: fleetv1.CapabilityKindModelDriven, Pauseable: true, RequiredHardware: req, ProvidingModel: model}
}

// The §6.10 truth table, exhaustively.
func TestEvaluateCapability_TruthTable(t *testing.T) {
	healthy := map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareHealthy, "cam": fleetv1.HardwareHealthy}
	degraded := map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareDegraded}
	failed := map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareFailed}
	disabled := map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareDisabled}
	modelActive := map[string]fleetv1.ModelStatus{"m": fleetv1.ModelStatusActive}
	modelUpdating := map[string]fleetv1.ModelStatus{"m": fleetv1.ModelStatusUpdating}
	modelFailed := map[string]fleetv1.ModelStatus{"m": fleetv1.ModelStatusFailed}

	cases := []struct {
		name       string
		cap        fleetv1.ClassCapability
		hw         map[string]fleetv1.HardwareStatus
		models     map[string]fleetv1.ModelStatus
		maintain   bool
		wantStatus fleetv1.CapabilityStatus
		wantPaused bool
	}{
		{"hw-native healthy", hwNative("nav", true, "lidar"), healthy, nil, false, fleetv1.CapabilityStatusActive, false},
		{"hw-native degraded", hwNative("nav", true, "lidar"), degraded, nil, false, fleetv1.CapabilityStatusDegraded, false},
		{"hw-native failed", hwNative("nav", true, "lidar"), failed, nil, false, fleetv1.CapabilityStatusInactive, false},
		{"hw-native disabled (benign off)", hwNative("nav", true, "lidar"), disabled, nil, false, fleetv1.CapabilityStatusInactive, false},
		{"hw-native missing", hwNative("nav", true, "lidar"), map[string]fleetv1.HardwareStatus{}, nil, false, fleetv1.CapabilityStatusInactive, false},
		{"model-driven all good", modelDriven("pick", "m", "cam"), healthy, modelActive, false, fleetv1.CapabilityStatusActive, false},
		{"model-driven model updating", modelDriven("pick", "m", "cam"), healthy, modelUpdating, false, fleetv1.CapabilityStatusInactive, false},
		{"model-driven model failed", modelDriven("pick", "m", "cam"), healthy, modelFailed, false, fleetv1.CapabilityStatusInactive, false},
		{"model-driven model absent", modelDriven("pick", "m", "cam"), healthy, nil, false, fleetv1.CapabilityStatusInactive, false},
		{"model-driven hw degraded, model ok", modelDriven("pick", "m", "lidar"), degraded, modelActive, false, fleetv1.CapabilityStatusDegraded, false},
		{"manual is active", fleetv1.ClassCapability{Name: "x", Type: fleetv1.CapabilityKindManual, Pauseable: true}, nil, nil, false, fleetv1.CapabilityStatusActive, false},
		{"pauseable paused under maintenance", hwNative("nav", true, "lidar"), healthy, nil, true, fleetv1.CapabilityStatusPaused, true},
		{"non-pauseable NOT paused under maintenance", hwNative("estop.receive", false, "lidar"), healthy, nil, true, fleetv1.CapabilityStatusActive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := tc.cap
			got, paused, _ := evaluateCapability(&cap, tc.hw, tc.models, tc.maintain)
			if got != tc.wantStatus || paused != tc.wantPaused {
				t.Fatalf("= %s/paused=%v, want %s/paused=%v", got, paused, tc.wantStatus, tc.wantPaused)
			}
		})
	}
}

// ADR-0031: a required component intentionally turned OFF is Inactive with a benign
// "disabled" reason — distinct from the "failed" fault reason — and the Disabled case is
// evaluated before Failed, so an off component never reads as a fault.
func TestEvaluateCapability_DisabledIsBenignInactive(t *testing.T) {
	cap := hwNative("nav", true, "camera")
	got, paused, reason := evaluateCapability(&cap, map[string]fleetv1.HardwareStatus{"camera": fleetv1.HardwareDisabled}, nil, false)
	if got != fleetv1.CapabilityStatusInactive || paused {
		t.Fatalf("= %s/paused=%v, want Inactive/false", got, paused)
	}
	if reason != "disabled: camera" {
		t.Fatalf("reason = %q, want %q (not a failure reason)", reason, "disabled: camera")
	}
}

// Non-pauseable capability lost during maintenance is a genuine fault, not a pause.
func TestEvaluateCapability_NonPauseableFailedIsInactive(t *testing.T) {
	cap := hwNative("estop.receive", false, "safety-bus")
	got, paused, _ := evaluateCapability(&cap, map[string]fleetv1.HardwareStatus{"safety-bus": fleetv1.HardwareFailed}, nil, true)
	if got != fleetv1.CapabilityStatusInactive || paused {
		t.Fatalf("= %s/paused=%v, want Inactive/false", got, paused)
	}
}

// DegradedSince is set once and preserved across evaluations, never churning.
func TestDegradedSince_StableAcrossReconciles(t *testing.T) {
	if ds := degradedSince(fleetv1.CapabilityStatusActive, fleetv1.CapabilityStatusEntry{}); ds != nil {
		t.Fatal("Active must have no DegradedSince")
	}
	set := degradedSince(fleetv1.CapabilityStatusDegraded, fleetv1.CapabilityStatusEntry{})
	if set == nil {
		t.Fatal("first non-Active must set DegradedSince")
	}
	again := degradedSince(fleetv1.CapabilityStatusInactive, fleetv1.CapabilityStatusEntry{DegradedSince: set})
	if again != set {
		t.Fatal("DegradedSince must be preserved, not reset, while non-Active")
	}
}

// Reconciler integration: spec.capabilities[] + hardware status → status.capabilities[].
func TestRobotReconcile_WritesTruthTableCapabilities(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	robot := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "warehouse-a"},
		Spec: fleetv1.RobotSpec{
			Capabilities: []fleetv1.ClassCapability{
				hwNative("nav", true, "lidar"),
				hwNative("inspect", true, "cam"),
			},
		},
		Status: fleetv1.RobotStatus{
			Phase: fleetv1.RobotPhaseIdle,
			Hardware: []fleetv1.HardwareComponentStatus{
				{Name: "lidar", Status: fleetv1.HardwareHealthy},
				{Name: "cam", Status: fleetv1.HardwareDegraded},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	r := &RobotReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "r1", Namespace: "warehouse-a"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: "warehouse-a"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	byName := map[string]fleetv1.CapabilityStatus{}
	for _, cs := range got.Status.Capabilities {
		byName[cs.Name] = cs.Status
	}
	if byName["nav"] != fleetv1.CapabilityStatusActive {
		t.Errorf("nav = %s, want Active", byName["nav"])
	}
	if byName["inspect"] != fleetv1.CapabilityStatusDegraded {
		t.Errorf("inspect = %s, want Degraded (camera degraded)", byName["inspect"])
	}
}
