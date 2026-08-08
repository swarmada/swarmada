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

// ADR-0029: Robot.status.phase is a derived state machine, not a sticky Discovered label.
// The Robot controller owns only the liveness-class phases: it advances a live robot off
// Discovered/Offline/"" to its steady summary (Idle, or InProgress when it holds an assigned
// action), marks Offline on a heartbeat lapse, and never clobbers scheduler-/maintenance-owned
// phases. (robotBound / livenessScheme / getRobot live in the sibling *_test.go files.)

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func TestRobotPhase_StateMachine(t *testing.T) {
	scheme := livenessScheme(t)
	reconcile := func(t *testing.T, robot *fleetv1.Robot) *fleetv1.Robot {
		t.Helper()
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
			WithStatusSubresource(&fleetv1.Robot{}).Build()
		rr := &RobotReconciler{Client: c, Scheme: scheme}
		if _, err := rr.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: robot.Name, Namespace: robot.Namespace},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return getRobot(t, c, robot.Name, robot.Namespace)
	}

	// Admission + first liveness: a Discovered robot that is now live advances to Idle
	// ("Ready") — it must NOT stay Discovered once the Ready condition is True.
	t.Run("Discovered + live -> Idle (Ready)", func(t *testing.T) {
		now := metav1.Now()
		robot := robotBound("p1", faNS, "acme", &now)
		robot.Status.Phase = fleetv1.RobotPhaseDiscovered
		got := reconcile(t, robot)
		if got.Status.Phase != fleetv1.RobotPhaseIdle {
			t.Errorf("phase = %s, want Idle (a live admitted robot must leave Discovered)", got.Status.Phase)
		}
		cond := findCondition(got, conditionTypeReady)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("Ready = %+v, want True", cond)
		}
	})

	// Holding an assigned action, a live robot summarises as InProgress ("Working").
	t.Run("Discovered + live + assigned action -> InProgress (Working)", func(t *testing.T) {
		now := metav1.Now()
		robot := robotBound("p2", faNS, "acme", &now)
		robot.Status.Phase = fleetv1.RobotPhaseDiscovered
		robot.Status.AssignedAction = "chat-action-001"
		got := reconcile(t, robot)
		if got.Status.Phase != fleetv1.RobotPhaseInProgress {
			t.Errorf("phase = %s, want InProgress (a live robot holding an action is Working)", got.Status.Phase)
		}
	})

	// A heartbeat older than the offline threshold drives Offline.
	t.Run("stale heartbeat -> Offline", func(t *testing.T) {
		old := metav1.NewTime(time.Now().Add(-time.Hour))
		robot := robotBound("p3", faNS, "acme", &old)
		robot.Status.Phase = fleetv1.RobotPhaseIdle
		got := reconcile(t, robot)
		if got.Status.Phase != fleetv1.RobotPhaseOffline {
			t.Errorf("phase = %s, want Offline after a heartbeat lapse", got.Status.Phase)
		}
		cond := findCondition(got, conditionTypeReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Errorf("Ready = %+v, want False", cond)
		}
	})

	// The controller owns only the liveness-class phases: an already-Idle live robot is left
	// for the scheduler (it must not be re-derived, e.g. clobbered while an assign is in flight).
	t.Run("live Idle is left to the scheduler (not clobbered)", func(t *testing.T) {
		now := metav1.Now()
		robot := robotBound("p4", faNS, "acme", &now)
		robot.Status.Phase = fleetv1.RobotPhaseInProgress
		robot.Status.AssignedAction = "chat-action-002"
		got := reconcile(t, robot)
		if got.Status.Phase != fleetv1.RobotPhaseInProgress {
			t.Errorf("phase = %s, want InProgress unchanged (scheduler-owned, not re-derived)", got.Status.Phase)
		}
	})
}
