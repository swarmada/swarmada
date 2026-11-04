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
)

// An operator pauses an in-flight rollout and it stops; resumes and it continues.
//
// Before spec.desiredState, RolloutPhasePaused was written on exactly one path —
// pauseOnError — so an operator watching a rollout they had decided was wrong could
// only delete the object, which does not unwind what the rollout did.
func TestFirmwareRollout_OperatorPauseStopsDispatchAndResumeContinues(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	r, c := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90),
		targetRobot("r2", fleetv1.RobotPhaseIdle, 90))

	// One robot enters the batch (maxUnavailable: 1).
	reconcileFirmware(t, r)
	if !robotPending(t, c, "r1") {
		t.Fatal("r1 was not dispatched to; the fixture never got in flight")
	}
	if robotPending(t, c, "r2") {
		t.Fatal("r2 dispatched too: maxUnavailable is 1, so the test cannot tell a hold from a full batch")
	}

	// r1 finishes, freeing the slot. Without a hold, r2 would go next.
	completeFirmware(t, c, "r1")

	// ── the operator pauses ──────────────────────────────────────────────────
	setFirmwareDesiredState(t, c, fleetv1.RolloutDesiredStatePaused)
	reconcileFirmware(t, r)

	if robotPending(t, c, "r2") {
		t.Fatal("r2 was dispatched to under an operator hold: spec.desiredState: Paused did not stop the rollout")
	}
	fw := getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused", fw.Status.Phase)
	}
	// The operator must be able to tell WHICH hold this is without cross-referencing.
	if !conditionIsTrue(fw.Status.Conditions, conditionOperatorHold) {
		t.Error("OperatorHold is not True: a Paused rollout that cannot say whether it is " +
			"waiting for the operator or halted on error is the ambiguity ADR-0045 had to work around")
	}
	if len(fw.Status.FailedRobots) != 0 {
		t.Errorf("failedRobots = %v, want empty: nothing failed, so this is purely an operator hold",
			fw.Status.FailedRobots)
	}

	// ── and resumes ──────────────────────────────────────────────────────────
	setFirmwareDesiredState(t, c, fleetv1.RolloutDesiredStateRunning)
	reconcileFirmware(t, r)

	if !robotPending(t, c, "r2") {
		t.Fatal("r2 was not dispatched to after the hold was lifted: writing Running did not resume the rollout")
	}
	fw = getFirmwareRollout(t, c)
	if fw.Status.Phase == fleetv1.RolloutPhasePaused {
		t.Errorf("phase = %s, want the rollout running again", fw.Status.Phase)
	}
	if conditionIsTrue(fw.Status.Conditions, conditionOperatorHold) {
		t.Error("OperatorHold is still True after writing Running")
	}
}

// INTERACTION: the controller pauses on error and the operator then resumes —
// does the error condition reassert?
//
// It never lifted. spec.desiredState: Running does not touch status.failedRobots,
// which is what the pauseOnError halt is derived from, so a rollout halted on error
// stays halted under Running. This is the property that makes the operator's field
// safe to be symmetric: it cannot be used to push a rollout past a failure the halt
// exists to surface. The exit is the rollout-resume verb (ADR-0041), which also
// decides what happens to the robots that failed.
func TestFirmwareRollout_WritingRunningDoesNotClearAPauseOnErrorHalt(t *testing.T) {
	r, c, _ := pausedFirmware(t)

	fw := getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused on error", fw.Status.Phase)
	}
	if len(fw.Status.FailedRobots) == 0 {
		t.Fatal("failedRobots is empty; the fixture did not produce a pauseOnError halt")
	}
	// The discriminator says this is NOT the operator's hold.
	if conditionIsTrue(fw.Status.Conditions, conditionOperatorHold) {
		t.Error("OperatorHold is True on a pauseOnError halt")
	}

	// ── the operator tries to resume with the spec field ─────────────────────
	setFirmwareDesiredState(t, c, fleetv1.RolloutDesiredStateRunning)
	for i := 0; i < 3; i++ {
		reconcileFirmware(t, r)
	}

	fw = getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused: writing desiredState: Running released a safety halt "+
			"whose exit is the rollout-resume verb", fw.Status.Phase)
	}
	if robotPending(t, c, "r2") {
		t.Fatal("r2 was dispatched to while the rollout was halted on error — the halt was bypassed " +
			"by a spec field, which is exactly what it must not be")
	}
	if len(fw.Status.FailedRobots) == 0 {
		t.Error("failedRobots was cleared by a spec write; only the resume verb may do that")
	}
}

// Both holds at once: the verb clears the error halt, the rollout STAYS paused on
// the operator's own hold, and lifting that releases it. Composition, asserted.
func TestFirmwareRollout_BothHoldsComposeAndBothMustLift(t *testing.T) {
	r, c, _ := pausedFirmware(t)
	setFirmwareDesiredState(t, c, fleetv1.RolloutDesiredStatePaused)
	reconcileFirmware(t, r)

	if got := getFirmwareRollout(t, c).Status.Phase; got != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused", got)
	}

	// The verb clears the error halt only.
	requestFirmwareResume(t, c, "operator decided to drop r1")
	reconcileFirmware(t, r)

	fw := getFirmwareRollout(t, c)
	if len(fw.Status.ExcludedRobots) == 0 {
		t.Fatal("the resume verb did not exclude the failed robot")
	}
	if fw.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want still Paused: the operator's own hold is still in force", fw.Status.Phase)
	}
	if robotPending(t, c, "r2") {
		t.Fatal("r2 dispatched while the operator hold was still set")
	}

	// Lifting the operator hold releases it.
	setFirmwareDesiredState(t, c, fleetv1.RolloutDesiredStateRunning)
	reconcileFirmware(t, r)
	if !robotPending(t, c, "r2") {
		t.Fatal("r2 was not dispatched to once BOTH holds were lifted")
	}
}

// The default keeps an existing rollout dispatching exactly as before. The Go zero
// value ("") is what a stored object decoded without the field carries in a unit
// test, and it must not read as a hold.
func TestFirmwareRollout_UnsetDesiredStateDispatches(t *testing.T) {
	secret, sigRef := signingFixture(t, fwChecksum)
	rollout := fwRollout(sigRef)
	rollout.Spec.DesiredState = "" // as an object stored before the field existed
	r, c := newFirmwareReconciler(t, rollout, signingConfig(true), secret,
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)
	if !robotPending(t, c, "r1") {
		t.Fatal("an unset desiredState withheld dispatch; a stored object must behave as it did before")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func setFirmwareDesiredState(t *testing.T, c client.Client, ds fleetv1.RolloutDesiredState) {
	t.Helper()
	fw := &fleetv1.FirmwareRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "fw", Namespace: rolloutNS}, fw); err != nil {
		t.Fatal(err)
	}
	fw.Spec.DesiredState = ds
	if err := c.Update(context.Background(), fw); err != nil {
		t.Fatal(err)
	}
}

func requestFirmwareResume(t *testing.T, c client.Client, reason string) {
	t.Helper()
	fw := &fleetv1.FirmwareRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "fw", Namespace: rolloutNS}, fw); err != nil {
		t.Fatal(err)
	}
	if fw.Annotations == nil {
		fw.Annotations = map[string]string{}
	}
	fw.Annotations[rolloutResumeAnnotation] = reason
	if err := c.Update(context.Background(), fw); err != nil {
		t.Fatal(err)
	}
}

// completeFirmware marks a robot as having installed the target version.
func completeFirmware(t *testing.T, c client.Client, name string) {
	t.Helper()
	rob := getRobot(t, c, name, rolloutNS)
	delete(rob.Annotations, annPendingFirmwareVersion)
	if err := c.Update(context.Background(), rob); err != nil {
		t.Fatal(err)
	}
	rob = getRobot(t, c, name, rolloutNS)
	rob.Status.FirmwareVersion = "2.5.0"
	if err := c.Status().Update(context.Background(), rob); err != nil {
		t.Fatal(err)
	}
}
