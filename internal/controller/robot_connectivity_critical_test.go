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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// criticalConfig builds the namespace SwarmadaConfig with a connectivity-critical
// threshold (seconds).
func criticalConfig(ns string, criticalSeconds int32) *fleetv1.SwarmadaConfig {
	return configWithSpec(ns, fleetv1.SwarmadaConfigSpec{
		Health: fleetv1.SwarmadaHealthConfig{ConnectivityCriticalThresholdSeconds: criticalSeconds},
	})
}

func conditionStatus(robot *fleetv1.Robot, condType string) (metav1.ConditionStatus, bool) {
	for _, c := range robot.Status.Conditions {
		if c.Type == condType {
			return c.Status, true
		}
	}
	return "", false
}

// TestConnectivityCriticalEscalation exercises ADR-0011: a robot Offline beyond the
// (namespace-configured, else default 120s) threshold gets a ConnectivityCritical
// condition; below it, none.
func TestConnectivityCriticalEscalation(t *testing.T) {
	const ns = "crit-ns"

	tests := []struct {
		name          string
		offlineAgo    time.Duration
		config        *fleetv1.SwarmadaConfig // nil ⇒ default 120s
		wantCondition bool
	}{
		{name: "offline past default threshold → critical", offlineAgo: 200 * time.Second, config: nil, wantCondition: true},
		{name: "offline below default threshold → not critical", offlineAgo: 60 * time.Second, config: nil, wantCondition: false},
		{name: "config raises threshold → not yet critical", offlineAgo: 200 * time.Second, config: criticalConfig(ns, 300), wantCondition: false},
		{name: "config lowers threshold → critical", offlineAgo: 45 * time.Second, config: criticalConfig(ns, 30), wantCondition: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lastSeen := &metav1.Time{Time: time.Now().Add(-tc.offlineAgo)}
			offlineSince := &metav1.Time{Time: time.Now().Add(-tc.offlineAgo)}
			robot := offlineTestRobot(ns, "r1", fleetv1.RobotPhaseOffline, lastSeen, offlineSince)

			objs := []client.Object{robot}
			if tc.config != nil {
				objs = append(objs, tc.config)
			}
			c := reconcileRobotFor(t, objs...)

			got := &fleetv1.Robot{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: ns}, got); err != nil {
				t.Fatalf("get: %v", err)
			}
			status, present := conditionStatus(got, conditionTypeConnectivityCritical)
			if tc.wantCondition {
				if !present || status != metav1.ConditionTrue {
					t.Errorf("ConnectivityCritical = (%v, present=%v), want True", status, present)
				}
			} else if present && status == metav1.ConditionTrue {
				t.Errorf("ConnectivityCritical unexpectedly True")
			}
		})
	}
}

// TestConnectivityCriticalClearsOnReconnect verifies a robot that recovers (no
// longer Offline) has an existing ConnectivityCritical=True condition set back to
// False.
func TestConnectivityCriticalClearsOnReconnect(t *testing.T) {
	const ns = "crit-clear-ns"

	robot := offlineTestRobot(ns, "r1", fleetv1.RobotPhaseIdle,
		&metav1.Time{Time: time.Now()}, nil) // fresh heartbeat, not offline
	robot.Status.Conditions = []metav1.Condition{{
		Type:               conditionTypeConnectivityCritical,
		Status:             metav1.ConditionTrue,
		Reason:             "OfflineThresholdExceeded",
		Message:            "was offline",
		LastTransitionTime: metav1.Now(),
	}}

	c := reconcileRobotFor(t, robot)

	got := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: ns}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	status, present := conditionStatus(got, conditionTypeConnectivityCritical)
	if !present || status != metav1.ConditionFalse {
		t.Errorf("ConnectivityCritical = (%v, present=%v), want False after reconnect", status, present)
	}
}
