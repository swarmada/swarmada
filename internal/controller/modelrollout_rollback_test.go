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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// autoRollbackRollout is pickerRollout with rollbackPolicy=Auto and an optional
// pre-seeded per-robot revert target in status.
func autoRollbackRollout(rollbackVersions map[string]string) *fleetv1.ModelRollout {
	roll := pickerRollout()
	roll.Spec.RollbackPolicy = fleetv1.ModelRollbackAuto
	roll.Status.RollbackVersions = rollbackVersions
	return roll
}

func failedModelRobot(name, failureReason string) *fleetv1.Robot {
	rob := targetRobot(name, fleetv1.RobotPhaseIdle, 90)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusFailed, FailureReason: failureReason},
	}
	return rob
}

func getRollout(t *testing.T, c client.Client, _ string) *fleetv1.ModelRollout {
	t.Helper()
	roll := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, roll); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	return roll
}

// Under rollbackPolicy=Auto, a failed robot with a known previous version is
// reverted (rollback model_update pushed, adapter reactivates the retained model) and
// tracked in status. Under pauseOnError (on here) the revert pauses the rollout, as a
// failure does: the model failed on this robot (modelrollout_revert_pause_test.go).
//
// Settlement is a separate question from the push, and this test used to conflate them: it
// asserted Succeeded on the acknowledgement alone, which is the state where the robot's model
// entry is still Updating and the capabilities it grants are still Inactive. The rollout
// completes here only once the robot reports the previous version Active.
func TestModelRollout_AutoRollbackRevertsFailedRobot(t *testing.T) {
	pusher := &fakePusher{ack: true}
	r, c := newRolloutReconciler(t,
		autoRollbackRollout(map[string]string{"r1": "3.2.0"}),
		failedModelRobot("r1", "inference health check failed"))
	r.Pusher = pusher
	reconcileRollout(t, r)

	if len(pusher.pushes) != 1 {
		t.Fatalf("expected exactly one rollback push, got %+v", pusher.pushes)
	}
	got := pusher.pushes[0]
	if !got.Rollback || got.NewVersion != "3.2.0" || got.OldVersion != "3.2.1" || got.ModelURI != "" {
		t.Errorf("rollback payload = %+v (want Rollback=true, New=3.2.0, Old=3.2.1, empty URI)", got)
	}
	roll := getRollout(t, c, "roll")
	if len(roll.Status.RolledBackRobots) != 1 || roll.Status.RolledBackRobots[0] != "r1" {
		t.Errorf("rollback not surfaced in status: names=%v", roll.Status.RolledBackRobots)
	}
	if roll.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Errorf("phase = %s, want Paused: a revert is an error for pauseOnError", roll.Status.Phase)
	}
	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusUpdating {
		t.Error("reverting robot should be marked Updating while the adapter reinstalls the previous model")
	}

	// The adapter finishes reactivating the retained model and the robot reports it.
	rob := rolloutRobot(t, c, "r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.2.0"},
	}
	if err := c.Status().Update(context.Background(), rob); err != nil {
		t.Fatalf("reporting the revert: %v", err)
	}
	reconcileRollout(t, r)

	roll = getRollout(t, c, "roll")
	if roll.Status.RobotsRolledBack != 1 {
		t.Errorf("robotsRolledBack = %d, want 1 once the revert is observed", roll.Status.RobotsRolledBack)
	}
	// total=1 and it settled back on the previous version → Succeeded, with the version
	// fragmentation surfaced by the non-zero rolledBack count.
	if roll.Status.Phase != fleetv1.RolloutPhaseSucceeded {
		t.Errorf("phase = %s, want Succeeded with rollback surfaced", roll.Status.Phase)
	}
	if len(pusher.pushes) != 1 {
		t.Errorf("a settled robot must not be pushed again, got %+v", pusher.pushes)
	}
}

// Manual (the default) reverts nothing: the failed robot stays failed and the
// rollout pauses — the operator must decide.
func TestModelRollout_ManualDoesNotRollBack(t *testing.T) {
	pusher := &fakePusher{ack: true}
	roll := pickerRollout() // RollbackPolicy defaults to Manual
	roll.Status.RollbackVersions = map[string]string{"r1": "3.2.0"}
	r, c := newRolloutReconciler(t, roll, failedModelRobot("r1", "health check failed"))
	r.Pusher = pusher
	reconcileRollout(t, r)

	if len(pusher.pushes) != 0 {
		t.Errorf("Manual policy must push no rollback, got %+v", pusher.pushes)
	}
	got := getRollout(t, c, "roll")
	if got.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Errorf("phase = %s, want Paused under Manual", got.Status.Phase)
	}
	if got.Status.RobotsRolledBack != 0 || len(got.Status.RolledBackRobots) != 0 {
		t.Error("Manual policy must not roll any robot back")
	}
}

// Auto with no known previous version cannot safely revert → the robot stays failed
// and the rollout pauses (fail-safe, surfaced), exactly like Manual.
func TestModelRollout_AutoRollbackUnknownPreviousStaysFailed(t *testing.T) {
	pusher := &fakePusher{ack: true}
	r, c := newRolloutReconciler(t,
		autoRollbackRollout(nil), // no revert target recorded
		failedModelRobot("r1", "health check failed"))
	r.Pusher = pusher
	reconcileRollout(t, r)

	if len(pusher.pushes) != 0 {
		t.Errorf("no known previous version → no rollback push, got %+v", pusher.pushes)
	}
	got := getRollout(t, c, "roll")
	if got.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Errorf("phase = %s, want Paused (fail-safe) when the revert target is unknown", got.Status.Phase)
	}
	if got.Status.RobotsRolledBack != 0 {
		t.Error("a robot with no revert target must not be counted as rolled back")
	}
}

// An undeliverable rollback push leaves the robot failed (not rolled back) and
// requeues — the revert is retried, never silently dropped.
func TestModelRollout_AutoRollbackUndeliverableRetries(t *testing.T) {
	pusher := &fakePusher{err: command.ErrUnreachable}
	r, c := newRolloutReconciler(t,
		autoRollbackRollout(map[string]string{"r1": "3.2.0"}),
		failedModelRobot("r1", "health check failed"))
	r.Pusher = pusher
	reconcileRollout(t, r)

	if len(pusher.pushes) != 1 {
		t.Fatalf("expected one (failed) rollback attempt, got %+v", pusher.pushes)
	}
	got := getRollout(t, c, "roll")
	if got.Status.RobotsRolledBack != 0 || len(got.Status.RolledBackRobots) != 0 {
		t.Error("an undelivered rollback must NOT mark the robot rolled back")
	}
	if got.Status.RobotsFailed != 1 {
		t.Errorf("robotsFailed = %d, want the robot still counted failed pending retry", got.Status.RobotsFailed)
	}
}

// A robot already recorded as rolled back is never pushed again — it is excluded
// from re-update even though it now reports the (old) model Active.
func TestModelRollout_RolledBackRobotNotReUpdated(t *testing.T) {
	pusher := &fakePusher{ack: true}
	roll := autoRollbackRollout(nil)
	roll.Status.RolledBackRobots = []string{"r1"}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.2.0"}, // reverted, on old version
	}
	r, c := newRolloutReconciler(t, roll, rob)
	r.Pusher = pusher
	reconcileRollout(t, r)

	if len(pusher.pushes) != 0 {
		t.Errorf("a rolled-back robot must not be re-updated, got pushes %+v", pusher.pushes)
	}
	got := getRollout(t, c, "roll")
	if got.Status.RobotsRolledBack != 1 || got.Status.RobotsPending != 0 {
		t.Errorf("rolled-back robot mis-counted: rolledBack=%d pending=%d",
			got.Status.RobotsRolledBack, got.Status.RobotsPending)
	}
}

// Entering the batch captures the robot's current version as the revert target, so
// a later failure can be auto-reverted.
func TestModelRollout_CapturesPreviousVersionAtBatchEntry(t *testing.T) {
	pusher := &fakePusher{ack: true}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.2.0"}, // on the OLD version
	}
	r, c := newRolloutReconciler(t, autoRollbackRollout(nil), rob)
	r.Pusher = pusher
	reconcileRollout(t, r)

	got := getRollout(t, c, "roll")
	if got.Status.RollbackVersions["r1"] != "3.2.0" {
		t.Errorf("previous version not captured at batch entry: %v", got.Status.RollbackVersions)
	}
}
