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

// The same operator intake on the OTHER rollout controller. Both kinds are named
// by the item and both had exactly one writer of RolloutPhasePaused, so a fix that
// landed on one would leave the other with the defect intact.
func TestModelRollout_OperatorPauseStopsDispatchAndResumeContinues(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(),
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90),
		targetRobot("r2", fleetv1.RobotPhaseIdle, 90))

	reconcileRollout(t, r)
	if !inModelBatch(t, c, "r1") {
		t.Fatal("r1 did not enter the batch; the fixture never got in flight")
	}
	if inModelBatch(t, c, "r2") {
		t.Fatal("r2 entered too: maxUnavailable is 1, so the test cannot tell a hold from a full batch")
	}

	// ── the operator pauses ──────────────────────────────────────────────────
	setModelDesiredState(t, c, fleetv1.RolloutDesiredStatePaused)
	reconcileRollout(t, r)

	mr := getModelRollout(t, c)
	if mr.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused under an operator hold", mr.Status.Phase)
	}
	if !conditionIsTrue(mr.Status.Conditions, conditionOperatorHold) {
		t.Error("OperatorHold is not True; a Paused rollout cannot say which hold is in force")
	}
	if len(mr.Status.FailedRobots) != 0 {
		t.Errorf("failedRobots = %v, want empty: nothing failed", mr.Status.FailedRobots)
	}
	// r1 was already in the batch and MUST NOT be yanked out: its models are marked
	// Updating, which suspends every capability they grant, and this rollout's status
	// is what un-suspends them.
	if !inModelBatch(t, c, "r1") {
		t.Error("an in-flight robot was pulled out of the batch by a hold, stranding its " +
			"suspended capabilities; a hold stops the NEXT robot, not the current one")
	}

	// ── and resumes ──────────────────────────────────────────────────────────
	setModelDesiredState(t, c, fleetv1.RolloutDesiredStateRunning)
	// Free the slot so r2 can enter, isolating the hold from maxUnavailable.
	completeModel(t, c, "r1")
	reconcileRollout(t, r)

	if !inModelBatch(t, c, "r2") {
		t.Fatal("r2 did not enter the batch after the hold was lifted")
	}
	if got := getModelRollout(t, c); conditionIsTrue(got.Status.Conditions, conditionOperatorHold) {
		t.Error("OperatorHold is still True after writing Running")
	}
}

// The same safety property on the model side: a spec write cannot release the
// pauseOnError halt, whose exit is the rollout-resume verb.
func TestModelRollout_WritingRunningDoesNotClearAPauseOnErrorHalt(t *testing.T) {
	r, c := pausedRollout(t)

	setModelDesiredState(t, c, fleetv1.RolloutDesiredStateRunning)
	for i := 0; i < 3; i++ {
		reconcileRollout(t, r)
	}

	mr := getModelRollout(t, c)
	if mr.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused: a spec field released a safety halt", mr.Status.Phase)
	}
	if inModelBatch(t, c, "r2") {
		t.Fatal("r2 entered the batch while the rollout was halted on error")
	}
	if len(mr.Status.ExcludedRobots) != 0 {
		t.Errorf("excludedRobots = %v: only the resume verb may exclude a robot", mr.Status.ExcludedRobots)
	}
}

// An object stored before the field existed keeps dispatching.
func TestModelRollout_UnsetDesiredStateDispatches(t *testing.T) {
	ro := pickerRollout()
	ro.Spec.DesiredState = ""
	r, c := newRolloutReconciler(t, ro, targetRobot("r1", fleetv1.RobotPhaseIdle, 90))

	reconcileRollout(t, r)
	if !inModelBatch(t, c, "r1") {
		t.Fatal("an unset desiredState withheld dispatch; a stored object must behave as before")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func setModelDesiredState(t *testing.T, c client.Client, ds fleetv1.RolloutDesiredState) {
	t.Helper()
	mr := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, mr); err != nil {
		t.Fatal(err)
	}
	mr.Spec.DesiredState = ds
	if err := c.Update(context.Background(), mr); err != nil {
		t.Fatal(err)
	}
}

// inModelBatch reports whether the robot is in this rollout's batch.
func inModelBatch(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	for _, e := range getModelRollout(t, c).Status.CurrentBatch {
		if e.RobotName == name {
			return true
		}
	}
	return false
}

// completeModel reports the robot as running the target version.
func completeModel(t *testing.T, c client.Client, name string) {
	t.Helper()
	rob := rolloutRobot(t, c, name)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", RunningVersion: "3.2.1", Status: fleetv1.ModelStatusActive},
	}
	if err := c.Status().Update(context.Background(), rob); err != nil {
		t.Fatal(err)
	}
}
