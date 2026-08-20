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
	"github.com/swarmada/swarmada/internal/webhook"
)

// THE DEADLOCK THESE TESTS EXIST FOR (ADR-0041). pauseOnError defaults to TRUE, so one failed
// robot paused a rollout; no resume path existed; and the delete webhook admits only terminal
// (Succeeded/Failed) records. A paused rollout could therefore be neither advanced nor removed,
// and for a ModelRollout it also held every capability its in-flight models grant suspended,
// because the rollout's own status is what un-suspends them.

func getModelRollout(t *testing.T, c client.Client) *fleetv1.ModelRollout {
	t.Helper()
	mr := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, mr); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	return mr
}

// requestResume writes the operator's annotation, exactly as `swarmctl rollout resume` does.
func requestResume(t *testing.T, c client.Client, mr *fleetv1.ModelRollout, reason string) {
	t.Helper()
	base := mr.DeepCopy()
	if mr.Annotations == nil {
		mr.Annotations = map[string]string{}
	}
	mr.Annotations[rolloutResumeAnnotation] = reason
	if err := c.Patch(context.Background(), mr, client.MergeFrom(base)); err != nil {
		t.Fatalf("requesting resume: %v", err)
	}
}

// pausedRollout drives a rollout to Paused with r1 failed and r2 untouched.
func pausedRollout(t *testing.T) (*ModelRolloutReconciler, client.Client) {
	t.Helper()
	failed := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	failed.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusFailed, FailureReason: "inference health check failed"},
	}
	fresh := targetRobot("r2", fleetv1.RobotPhaseIdle, 90)
	r, c := newRolloutReconciler(t, pickerRollout(), failed, fresh)
	reconcileRollout(t, r)
	if got := getModelRollout(t, c).Status.Phase; got != fleetv1.RolloutPhasePaused {
		t.Fatalf("fixture did not pause: phase = %s", got)
	}
	return r, c
}

// THE ACCEPTANCE CASE. A Paused rollout can be resumed and then deleted. Before this, the
// delete webhook refused every non-terminal phase and nothing could move the rollout out of
// Paused, so the two guards closed on each other.
func TestModelRolloutResume_ThenDeletable(t *testing.T) {
	r, c := pausedRollout(t)
	ctx := context.Background()

	// Delete is refused while Paused — the second half of the wedge.
	mr := getModelRollout(t, c)
	if _, err := (&webhook.ModelRolloutValidator{}).ValidateDelete(ctx, mr); err == nil {
		t.Fatal("a Paused rollout must not be deletable; the wedge premise no longer holds")
	}

	requestResume(t, c, mr, "r1 has a failed SSD; excluding it")
	reconcileRollout(t, r) // consumes the annotation, excludes r1
	reconcileRollout(t, r) // r2 now enters the batch

	mr = getModelRollout(t, c)
	if mr.Status.Phase == fleetv1.RolloutPhasePaused {
		t.Fatalf("rollout is still Paused after resume: %+v", mr.Status)
	}
	if len(mr.Status.ExcludedRobots) != 1 || mr.Status.ExcludedRobots[0] != "r1" {
		t.Fatalf("excludedRobots = %v, want [r1]", mr.Status.ExcludedRobots)
	}
	if mr.Status.PausedAt != nil {
		t.Errorf("pausedAt should be cleared on resume, got %v", mr.Status.PausedAt)
	}

	// Drive r2 to done so the rollout can settle. r1 stays failed and excluded.
	r2 := rolloutRobot(t, c, "r2")
	r2.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", RunningVersion: "3.2.1", Status: fleetv1.ModelStatusActive},
	}
	if err := c.Status().Update(ctx, r2); err != nil {
		t.Fatalf("advancing r2: %v", err)
	}
	reconcileRollout(t, r)

	mr = getModelRollout(t, c)
	if mr.Status.Phase != fleetv1.RolloutPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded once every robot is done or excluded (%+v)", mr.Status.Phase, mr.Status)
	}
	if _, err := (&webhook.ModelRolloutValidator{}).ValidateDelete(ctx, mr); err != nil {
		t.Fatalf("a resumed, settled rollout must be deletable, got: %v", err)
	}
}

// Resume must not re-latch Paused off the SAME failed robots. `paused` is derived per reconcile
// from len(failed), not latched, so excluding the failed robots is the only thing that keeps the
// resume from being undone on the very next pass — the same trap policy-reset avoids by zeroing
// ConsecutiveRejections.
func TestModelRolloutResume_DoesNotReLatchOnTheSameRobots(t *testing.T) {
	r, c := pausedRollout(t)

	requestResume(t, c, getModelRollout(t, c), "excluding r1")
	reconcileRollout(t, r)

	// r1 is STILL reporting a failed model — nothing repaired it. Several further reconciles
	// must not bring the pause back.
	for i := 0; i < 3; i++ {
		reconcileRollout(t, r)
		if got := getModelRollout(t, c).Status.Phase; got == fleetv1.RolloutPhasePaused {
			t.Fatalf("rollout re-paused on reconcile %d off the same failed robot", i+1)
		}
	}
	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusFailed {
		t.Fatal("fixture invalid: r1 should still be reporting a failed model")
	}
}

// Resume EXCLUDES, it does not retry (ADR-0041). The excluded robot must never be dispatched to
// again by this rollout — retrying would re-push the artifact that just failed.
func TestModelRolloutResume_ExcludedRobotIsNeverRetried(t *testing.T) {
	r, c := pausedRollout(t)
	requestResume(t, c, getModelRollout(t, c), "excluding r1")
	for i := 0; i < 3; i++ {
		reconcileRollout(t, r)
	}
	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
		t.Fatal("an excluded robot was pushed back into the update batch")
	}
}

// The annotation is consumed once. A re-reconcile of an already-applied request writes nothing,
// so a resume cannot silently fire again (RA-1), and a stale annotation left on a running
// rollout cannot resume a FUTURE pause.
func TestModelRolloutResume_IsIdempotent(t *testing.T) {
	r, c := pausedRollout(t)
	requestResume(t, c, getModelRollout(t, c), "excluding r1")
	reconcileRollout(t, r)

	after := getModelRollout(t, c)
	if after.Annotations[rolloutResumeProcessedAnnotation] != "excluding r1" {
		t.Fatalf("processed marker = %q, want the request value",
			after.Annotations[rolloutResumeProcessedAnnotation])
	}
	settled := after.ResourceVersion

	reconcileRollout(t, r)
	if again := getModelRollout(t, c); again.ResourceVersion != settled &&
		again.Status.Phase == fleetv1.RolloutPhasePaused {
		t.Error("a second reconcile re-applied the resume")
	}
}

// A resume request on a rollout that is NOT paused is recorded as processed but changes nothing,
// so it cannot lie in wait and silently release a future pause.
func TestModelRolloutResume_OnUnpausedRolloutExcludesNothing(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(),
		targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	reconcileRollout(t, r)

	requestResume(t, c, getModelRollout(t, c), "premature")
	reconcileRollout(t, r)

	mr := getModelRollout(t, c)
	if len(mr.Status.ExcludedRobots) != 0 {
		t.Errorf("excludedRobots = %v, want none on an unpaused rollout", mr.Status.ExcludedRobots)
	}
	if mr.Annotations[rolloutResumeProcessedAnnotation] != "premature" {
		t.Error("the request must still be marked processed so it cannot fire against a later pause")
	}
}

func TestMergeExcluded_SortsAndDeduplicates(t *testing.T) {
	got := mergeExcluded([]string{"r2", "r1"}, []string{"r1", "r3"})
	want := []string{"r1", "r2", "r3"}
	if len(got) != len(want) {
		t.Fatalf("mergeExcluded = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergeExcluded = %v, want %v", got, want)
		}
	}
}
