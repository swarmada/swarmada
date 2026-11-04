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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Auto rollback settles on the REVERT, not on the push acknowledgement.
//
// The controller marked a robot rolled back the instant the adapter acknowledged the rollback
// model_update, and never looked again. The model entry was left Updating, so the capabilities
// that model grants stayed Inactive — and because rolled-back robots count as settled, the
// rollout could report Succeeded with reverted robots still carrying suspended capabilities.
// An acknowledgement is a promise to revert; only the robot reporting the previous version
// Active is evidence that it did.

// revertingRollout drives one Auto rollback to the acknowledged-but-unobserved state.
func revertingRollout(t *testing.T) (*ModelRolloutReconciler, *fakePusher, *fleetv1.ModelRollout) {
	t.Helper()
	pusher := &fakePusher{ack: true}
	r, c := newRolloutReconciler(t,
		autoRollbackRollout(map[string]string{"r1": "3.2.0"}),
		failedModelRobot("r1", "inference health check failed"))
	r.Pusher = pusher
	reconcileRollout(t, r)
	roll := getRollout(t, c, "roll")
	// The reconciler and the client share state; hand back the client-bound rollout.
	t.Cleanup(func() { _ = c })
	return r, pusher, roll
}

// THE BUG. The push is acknowledged; the robot has not reported the previous version yet.
// The rollout must not be Succeeded, because the revert has not been observed.
func TestModelRollback_AckAloneDoesNotSettle(t *testing.T) {
	_, pusher, roll := revertingRollout(t)

	if len(pusher.pushes) != 1 || !pusher.pushes[0].Rollback {
		t.Fatalf("expected exactly one rollback push, got %+v", pusher.pushes)
	}
	if roll.Status.Phase == fleetv1.RolloutPhaseSucceeded {
		t.Fatalf("phase = Succeeded on the rollback ACK alone; the robot has not reported %s Active yet (%+v)",
			"3.2.0", roll.Status)
	}
	if roll.Status.RobotsRolledBack != 0 {
		t.Errorf("robotsRolledBack = %d, want 0 until the revert is observed", roll.Status.RobotsRolledBack)
	}
	// It IS on the reverted list — the rollback was pushed and must never be re-updated —
	// and its capabilities are suspended, which is the harm this item is about.
	if len(roll.Status.RolledBackRobots) != 1 || roll.Status.RolledBackRobots[0] != "r1" {
		t.Errorf("rolledBackRobots = %v, want [r1]: a pushed rollback must not be forgotten", roll.Status.RolledBackRobots)
	}
	if roll.Status.CapabilitiesSuspendedOn != 1 {
		t.Errorf("capabilitiesSuspendedOn = %d, want 1: the reverting robot's model is Updating",
			roll.Status.CapabilitiesSuspendedOn)
	}
	// The revert target has to survive, or nothing can recognise the revert when it lands.
	if roll.Status.RollbackVersions["r1"] != "3.2.0" {
		t.Errorf("rollbackVersions = %v, want r1 -> 3.2.0 retained while the revert is outstanding",
			roll.Status.RollbackVersions)
	}
}

// The robot reports the previous version Active: now it has settled.
func TestModelRollback_SettlesOnObservedRevert(t *testing.T) {
	pusher := &fakePusher{ack: true}
	r, c := newRolloutReconciler(t,
		autoRollbackRollout(map[string]string{"r1": "3.2.0"}),
		failedModelRobot("r1", "inference health check failed"))
	r.Pusher = pusher
	reconcileRollout(t, r)

	rob := rolloutRobot(t, c, "r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.2.0"},
	}
	if err := c.Status().Update(context.Background(), rob); err != nil {
		t.Fatalf("reporting the revert: %v", err)
	}
	reconcileRollout(t, r)

	roll := getRollout(t, c, "roll")
	if roll.Status.RobotsRolledBack != 1 {
		t.Errorf("robotsRolledBack = %d, want 1 once the revert is observed", roll.Status.RobotsRolledBack)
	}
	if roll.Status.Phase != fleetv1.RolloutPhaseSucceeded {
		t.Errorf("phase = %s, want Succeeded once every robot has settled (%+v)", roll.Status.Phase, roll.Status)
	}
	if roll.Status.CapabilitiesSuspendedOn != 0 {
		t.Errorf("capabilitiesSuspendedOn = %d, want 0: the model is Active again", roll.Status.CapabilitiesSuspendedOn)
	}
	if _, ok := roll.Status.RollbackVersions["r1"]; ok {
		t.Errorf("rollbackVersions still holds r1 after the revert landed: %v", roll.Status.RollbackVersions)
	}
}

// A revert that itself fails must not leave the rollout reverting for ever: the robot is
// counted failed, which is the state that pauses (or, with pauseOnError off, terminates).
func TestModelRollback_FailedRevertIsCountedFailed(t *testing.T) {
	pusher := &fakePusher{ack: true}
	r, c := newRolloutReconciler(t,
		autoRollbackRollout(map[string]string{"r1": "3.2.0"}),
		failedModelRobot("r1", "inference health check failed"))
	r.Pusher = pusher
	reconcileRollout(t, r)

	rob := rolloutRobot(t, c, "r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusFailed, FailureReason: "retained model would not reactivate"},
	}
	if err := c.Status().Update(context.Background(), rob); err != nil {
		t.Fatalf("reporting the failed revert: %v", err)
	}
	reconcileRollout(t, r)

	roll := getRollout(t, c, "roll")
	if roll.Status.RobotsFailed != 1 {
		t.Errorf("robotsFailed = %d, want 1: the revert itself failed (%+v)", roll.Status.RobotsFailed, roll.Status)
	}
	if roll.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Errorf("phase = %s, want Paused: a failed revert must reach an operator", roll.Status.Phase)
	}
	if len(pusher.pushes) != 1 {
		t.Errorf("a failed revert must not be pushed again, got %+v", pusher.pushes)
	}
}
