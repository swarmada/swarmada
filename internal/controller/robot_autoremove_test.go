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

// ADR-0030: opt-in auto-removal of Offline auto-admitted Robots. The Robot controller removes a
// Robot only when ALL hold: the namespace opted in (autoRemoveOfflineRobots), the Robot is
// auto-admitted (swarmada.io/auto-admitted), it is Offline (adapter presence gone), the offline
// dwell has elapsed, and any assigned action's lease is provably dead (§9.6.3.5; nil/transient ⇒
// not dead). Operator-created robots (no marker) are never removed — the warehouse gate.
// (robotBound / livenessScheme / configWithSpec / faNS live in the sibling *_test.go files.)

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// offlineRobot builds an Offline robot with a stale heartbeat (so it stays Offline through the
// reconcile) and a preset offlineSince, optionally auto-admitted and optionally holding an action.
func offlineRobot(name string, autoAdmitted bool, offlineSince time.Time, assignedAction string) *fleetv1.Robot {
	stale := metav1.NewTime(time.Now().Add(-time.Hour))
	r := robotBound(name, faNS, "acme", &stale)
	r.Status.Phase = fleetv1.RobotPhaseOffline
	os := metav1.NewTime(offlineSince)
	r.Status.OfflineSince = &os
	r.Status.AssignedAction = assignedAction
	if autoAdmitted {
		r.Annotations = map[string]string{fleetv1.AutoAdmittedAnnotation: "true"}
	}
	return r
}

func removalConfig(enabled bool) *fleetv1.SwarmadaConfig {
	return configWithSpec(faNS, fleetv1.SwarmadaConfigSpec{
		Provisioning: fleetv1.SwarmadaProvisioningConfig{
			AutoRemoveOfflineRobots:       enabled,
			AutoRemoveOfflineGraceSeconds: 60, // 1-minute dwell
		},
	})
}

func actionWithLease(name string, expires time.Time) *fleetv1.FleetAction {
	t := metav1.NewTime(expires)
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: faNS},
		Status:     fleetv1.FleetActionStatus{LeaseExpiresAt: &t},
	}
}

func TestRobotAutoRemove(t *testing.T) {
	scheme := livenessScheme(t)
	past := time.Now().Add(-10 * time.Minute) // well beyond the 60s grace
	// reconcile the named robot against the given objects; report whether the Robot still exists.
	stillExists := func(t *testing.T, robot *fleetv1.Robot, objs ...client.Object) bool {
		t.Helper()
		all := append([]client.Object{robot}, objs...)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).
			WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.FleetAction{}).Build()
		rr := &RobotReconciler{Client: c, Scheme: scheme}
		if _, err := rr.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKey{Name: robot.Name, Namespace: robot.Namespace},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		err := c.Get(context.Background(), client.ObjectKey{Name: robot.Name, Namespace: robot.Namespace}, &fleetv1.Robot{})
		if errors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Fatalf("get robot: %v", err)
		}
		return true
	}

	// Removed: auto-admitted + offline + dwell elapsed + no assigned action (no lease at all).
	t.Run("removed: offline, dwell elapsed, no action", func(t *testing.T) {
		robot := offlineRobot("robot-1", true, past, "")
		if stillExists(t, robot, removalConfig(true)) {
			t.Error("expected the offline auto-admitted robot to be removed")
		}
	})

	// Removed: the assigned action's lease is provably dead (expired beyond the clock skew).
	t.Run("removed: assigned action lease provably dead", func(t *testing.T) {
		robot := offlineRobot("robot-2", true, past, "act-dead")
		deadLease := actionWithLease("act-dead", time.Now().Add(-time.Minute))
		if stillExists(t, robot, removalConfig(true), deadLease) {
			t.Error("expected removal when the assigned action's lease is provably dead")
		}
	})

	// Removed: the assigned action is gone entirely (NotFound ⇒ no live lease).
	t.Run("removed: assigned action no longer exists", func(t *testing.T) {
		robot := offlineRobot("robot-3", true, past, "act-ghost")
		if stillExists(t, robot, removalConfig(true)) {
			t.Error("expected removal when the assigned action does not exist")
		}
	})

	// Kept: operator-created robot (no auto-admitted marker) is never removed — the warehouse gate.
	t.Run("kept: not auto-admitted (warehouse robot)", func(t *testing.T) {
		robot := offlineRobot("amr-7", false, past, "")
		if !stillExists(t, robot, removalConfig(true)) {
			t.Error("an operator-created robot must never be auto-removed")
		}
	})

	// Kept: policy disabled — default behaviour is unchanged.
	t.Run("kept: policy off", func(t *testing.T) {
		robot := offlineRobot("robot-4", true, past, "")
		if !stillExists(t, robot, removalConfig(false)) {
			t.Error("no removal when autoRemoveOfflineRobots is false")
		}
	})

	// Kept: no SwarmadaConfig at all ⇒ policy resolves disabled (fail safe).
	t.Run("kept: no config", func(t *testing.T) {
		robot := offlineRobot("robot-5", true, past, "")
		if !stillExists(t, robot) {
			t.Error("no removal when the namespace has no SwarmadaConfig")
		}
	})

	// Kept: the assigned action's lease is still alive (future horizon) — fail closed.
	t.Run("kept: lease still alive", func(t *testing.T) {
		robot := offlineRobot("robot-6", true, past, "act-live")
		liveLease := actionWithLease("act-live", time.Now().Add(5*time.Minute))
		if !stillExists(t, robot, removalConfig(true), liveLease) {
			t.Error("must not remove a robot whose assigned action lease is not provably dead")
		}
	})

	// Kept: the assigned action has a nil lease horizon — nil is NOT proof of death.
	t.Run("kept: nil lease horizon is not death", func(t *testing.T) {
		robot := offlineRobot("robot-7", true, past, "act-nolease")
		noLease := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "act-nolease", Namespace: faNS}}
		if !stillExists(t, robot, removalConfig(true), noLease) {
			t.Error("a nil lease horizon must not be treated as provably dead")
		}
	})

	// Kept: still within the offline grace dwell.
	t.Run("kept: within grace dwell", func(t *testing.T) {
		robot := offlineRobot("robot-8", true, time.Now().Add(-10*time.Second), "") // < 60s grace
		if !stillExists(t, robot, removalConfig(true)) {
			t.Error("must not remove before the offline grace dwell has elapsed")
		}
	})
}
