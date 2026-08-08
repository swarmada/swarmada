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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// healthConfig builds the namespace SwarmadaConfig singleton carrying the given
// connectivityOfflineThresholdSeconds.
func healthConfig(ns string, offlineSeconds int32) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: ns},
		Spec: fleetv1.SwarmadaConfigSpec{
			Health: fleetv1.SwarmadaHealthConfig{
				ConnectivityOfflineThresholdSeconds: offlineSeconds,
			},
		},
	}
}

// TestRobotHeartbeatTimeoutFromConfig verifies the offline threshold is sourced
// from spec.health.connectivityOfflineThresholdSeconds and fails safe to the
// 30s default when the config is absent or carries a non-positive value. Each case
// picks a heartbeat age that straddles the expected threshold so the resulting
// phase reveals which timeout was applied.
func TestRobotHeartbeatTimeoutFromConfig(t *testing.T) {
	const ns = "hb-ns"

	tests := []struct {
		name       string
		config     *fleetv1.SwarmadaConfig // nil ⇒ no config in the namespace
		ageSeconds int
		wantPhase  fleetv1.RobotPhase
	}{
		{name: "config 90s: 60s age stays online", config: healthConfig(ns, 90), ageSeconds: 60, wantPhase: fleetv1.RobotPhaseIdle},
		{name: "config 90s: 100s age goes offline", config: healthConfig(ns, 90), ageSeconds: 100, wantPhase: fleetv1.RobotPhaseOffline},
		{name: "config 10s: 20s age goes offline", config: healthConfig(ns, 10), ageSeconds: 20, wantPhase: fleetv1.RobotPhaseOffline},
		{name: "no config: 20s age stays online (default 30s)", config: nil, ageSeconds: 20, wantPhase: fleetv1.RobotPhaseIdle},
		{name: "no config: 40s age goes offline (default 30s)", config: nil, ageSeconds: 40, wantPhase: fleetv1.RobotPhaseOffline},
		{name: "zero threshold falls back to default 30s", config: healthConfig(ns, 0), ageSeconds: 20, wantPhase: fleetv1.RobotPhaseIdle},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := fleetv1.AddToScheme(scheme); err != nil {
				t.Fatalf("scheme: %v", err)
			}
			lastSeen := &metav1.Time{Time: time.Now().Add(-time.Duration(tc.ageSeconds) * time.Second)}
			objs := []client.Object{offlineTestRobot(ns, "r1", fleetv1.RobotPhaseIdle, lastSeen, nil)}
			if tc.config != nil {
				objs = append(objs, tc.config)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
				WithStatusSubresource(&fleetv1.Robot{}).Build()

			r := &RobotReconciler{Client: c, Scheme: scheme}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "r1", Namespace: ns},
			}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			got := &fleetv1.Robot{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: ns}, got); err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %s, want %s", got.Status.Phase, tc.wantPhase)
			}
		})
	}
}
