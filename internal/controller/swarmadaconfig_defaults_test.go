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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func defaultsTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func configWithSpec(ns string, spec fleetv1.SwarmadaConfigSpec) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: ns},
		Spec:       spec,
	}
}

// TestProbeDefaultsFromConfig verifies RobotProbe interval/timeout defaults are
// sourced from spec.health and fail safe to the built-in constants (30s / 5s) when
// the config is absent or carries a non-positive value.
func TestProbeDefaultsFromConfig(t *testing.T) {
	const ns = "probe-ns"

	tests := []struct {
		name         string
		spec         *fleetv1.SwarmadaConfigSpec // nil ⇒ no config
		wantInterval int32
		wantTimeout  int32
	}{
		{name: "no config → constants", spec: nil, wantInterval: 30, wantTimeout: 5},
		{
			name:         "config honored",
			spec:         &fleetv1.SwarmadaConfigSpec{Health: fleetv1.SwarmadaHealthConfig{DefaultHardwareProbeIntervalSeconds: 45, DefaultProbeTimeoutSeconds: 8}},
			wantInterval: 45, wantTimeout: 8,
		},
		{
			name:         "zero values fall back to constants",
			spec:         &fleetv1.SwarmadaConfigSpec{Health: fleetv1.SwarmadaHealthConfig{DefaultHardwareProbeIntervalSeconds: 0, DefaultProbeTimeoutSeconds: 0}},
			wantInterval: 30, wantTimeout: 5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.spec != nil {
				objs = append(objs, configWithSpec(ns, *tc.spec))
			}
			r := &RobotProbeReconciler{Client: defaultsTestClient(t, objs...)}
			gotInterval, gotTimeout := r.probeDefaults(context.Background(), ns)
			if gotInterval != tc.wantInterval || gotTimeout != tc.wantTimeout {
				t.Errorf("probeDefaults = (%d, %d), want (%d, %d)", gotInterval, gotTimeout, tc.wantInterval, tc.wantTimeout)
			}
		})
	}
}

// TestTDERetryBoundsFromConfig verifies the deconfliction backoff bounds are
// sourced from spec.trafficDeconfliction and fail safe to the constants, and that
// an inverted config is corrected so max ≥ min.
func TestTDERetryBoundsFromConfig(t *testing.T) {
	const ns = "tde-ns"

	tests := []struct {
		name    string
		spec    *fleetv1.SwarmadaConfigSpec // nil ⇒ no config
		wantMin time.Duration
		wantMax time.Duration
	}{
		{name: "no config → constants", spec: nil, wantMin: tdeMinRetryAfter, wantMax: tdeMaxRetryAfter},
		{
			name:    "config honored",
			spec:    &fleetv1.SwarmadaConfigSpec{TrafficDeconfliction: fleetv1.SwarmadaTrafficDeconflictionConfig{MinRetryAfterSeconds: 20, MaxRetryAfterSeconds: 200}},
			wantMin: 20 * time.Second, wantMax: 200 * time.Second,
		},
		{
			name:    "zero values fall back to constants",
			spec:    &fleetv1.SwarmadaConfigSpec{TrafficDeconfliction: fleetv1.SwarmadaTrafficDeconflictionConfig{MinRetryAfterSeconds: 0, MaxRetryAfterSeconds: 0}},
			wantMin: tdeMinRetryAfter, wantMax: tdeMaxRetryAfter,
		},
		{
			name:    "inverted config corrected to max = min",
			spec:    &fleetv1.SwarmadaConfigSpec{TrafficDeconfliction: fleetv1.SwarmadaTrafficDeconflictionConfig{MinRetryAfterSeconds: 90, MaxRetryAfterSeconds: 30}},
			wantMin: 90 * time.Second, wantMax: 90 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.spec != nil {
				objs = append(objs, configWithSpec(ns, *tc.spec))
			}
			r := &FleetActionReconciler{Client: defaultsTestClient(t, objs...)}
			gotMin, gotMax := r.tdeRetryBounds(context.Background(), ns)
			if gotMin != tc.wantMin || gotMax != tc.wantMax {
				t.Errorf("tdeRetryBounds = (%v, %v), want (%v, %v)", gotMin, gotMax, tc.wantMin, tc.wantMax)
			}
		})
	}
}
