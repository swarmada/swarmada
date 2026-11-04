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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/webhook"
)

// Terminal Failed on both rollout kinds.
//
// RolloutPhaseFailed had no writer on either install path: the only Failed write anywhere was
// the firmware pre-dispatch verification refusal. With pauseOnError: false a rollout that
// accumulated install failures settled into nothing — Succeeded needs every robot updated or
// excluded, and no branch produced Failed, so it sat InProgress forever. The delete webhook
// admits a terminal record only, so the record could not be deleted either: a ModelRollout
// stranded that way leaves model-granted capabilities Inactive with no owner left to restore
// them.
//
// The rule the controllers now apply: once nothing is left in flight and nothing is left to
// dispatch, a rollout carrying failures that were neither repaired nor excluded is Failed.
// Paused still wins while pauseOnError holds the rollout, because that state has an exit an
// operator drives (`swarmctl rollout resume`, ADR-0041) and is not terminal.

// unpausingFwRollout is fwRollout with pauseOnError explicitly OFF — the configuration that
// could not terminate.
func unpausingFwRollout(sigRef string) *fleetv1.FirmwareRollout {
	ro := fwRollout(sigRef)
	ro.Spec.Strategy.RollingUpdate.PauseOnError = apiBool(false)
	return ro
}

// THE BUG, firmware side. Every targeted robot has settled and one of them failed; there is
// no further work this rollout can do, and it must say so.
func TestFirmwareRollout_UnpausedFailureReachesFailed(t *testing.T) {
	dispatched := time.Now().Add(-10 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := unpausingFwRollout(sigRef)
	seedBatch(ro, "r1", dispatched)
	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "flash verify failed after reboot", dispatched.Add(time.Minute)))

	reconcileFirmware(t, r)

	fw := getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Fatalf("phase = %s, want Failed: pauseOnError is off and the only robot failed (%+v)", fw.Status.Phase, fw.Status)
	}
}

// The point of reaching a terminal phase: the record can be removed. Without it the rollout is
// stranded live — undeletable by the delete guard it can never satisfy.
func TestFirmwareRollout_FailedRecordIsDeletable(t *testing.T) {
	dispatched := time.Now().Add(-10 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := unpausingFwRollout(sigRef)
	seedBatch(ro, "r1", dispatched)
	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "flash verify failed", dispatched.Add(time.Minute)))

	reconcileFirmware(t, r)

	fw := getFirmwareRollout(t, c)
	if _, err := (&webhook.FirmwareRolloutValidator{}).ValidateDelete(context.Background(), fw); err != nil {
		t.Fatalf("a settled, failed firmware rollout must be deletable, got: %v", err)
	}
}

// A rollout with work still outstanding must NOT go Failed early — one robot failed, another
// is still installing. Failing here would abandon a robot mid-update.
func TestFirmwareRollout_FailedOnlyOnceNothingIsOutstanding(t *testing.T) {
	dispatched := time.Now().Add(-10 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := unpausingFwRollout(sigRef)
	ro.Spec.Strategy.RollingUpdate.MaxUnavailable = "2"
	seedBatch(ro, "r1", dispatched)
	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "flash verify failed", dispatched.Add(time.Minute)),
		targetRobot("r2", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)

	fw := getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhaseInProgress {
		t.Fatalf("phase = %s, want InProgress while r2 is still installing (%+v)", fw.Status.Phase, fw.Status)
	}
	if !robotPending(t, c, "r2") {
		t.Fatal("with pauseOnError off the rollout should still have dispatched to r2")
	}
}

// pauseOnError: true keeps Paused, which is NOT terminal: it has an operator-driven exit and
// the record deliberately stays undeletable until the operator resumes past the failure.
func TestFirmwareRollout_PausedStillBeatsFailed(t *testing.T) {
	_, c, _ := pausedFirmware(t)
	fw := getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused; pauseOnError must keep the operator's exit", fw.Status.Phase)
	}
}

// THE PREREQUISITE, and a defect in its own right. A firmware failure must survive the pass
// that records it. installFailedForRollout keys on status.currentBatch, and currentBatch is
// rebuilt from the UPDATING robots only — so a robot dropped out of the batch the moment it
// was recorded failed re-classified as eligible (or, in production, as a phantom updater on
// its own leftover pending-firmware annotation) on the very next reconcile. The failure list
// emptied, `paused` is derived from it, and the pause released itself one reconcile after it
// latched.
func TestFirmwareRollout_FailureSurvivesAcrossReconciles(t *testing.T) {
	r, c, _ := pausedFirmware(t)

	for i := 2; i <= 4; i++ {
		reconcileFirmware(t, r)
		fw := getFirmwareRollout(t, c)
		if fw.Status.Phase != fleetv1.RolloutPhasePaused {
			t.Fatalf("pass %d: phase = %s, want Paused; the halt released itself (%+v)", i, fw.Status.Phase, fw.Status)
		}
		if len(fw.Status.FailedRobots) != 1 || fw.Status.FailedRobots[0].RobotName != "r1" {
			t.Fatalf("pass %d: failedRobots = %+v, want r1 still recorded", i, fw.Status.FailedRobots)
		}
		if robotPending(t, c, "r1") {
			t.Fatalf("pass %d: the failed robot was re-dispatched to", i)
		}
		if robotPending(t, c, "r2") {
			t.Fatalf("pass %d: a paused rollout dispatched to another robot", i)
		}
	}
}

// The failed robot's recorded previousVersion must survive too — it leaves status.currentBatch
// on the pass it fails, so the value has to be carried forward from the failure record.
func TestFirmwareRollout_FailedResultKeepsPreviousVersion(t *testing.T) {
	r, c, _ := pausedFirmware(t)
	reconcileFirmware(t, r)

	fw := getFirmwareRollout(t, c)
	if len(fw.Status.FailedRobots) != 1 {
		t.Fatalf("failedRobots = %+v, want one entry", fw.Status.FailedRobots)
	}
	if got := fw.Status.FailedRobots[0].PreviousVersion; got != "2.4.0" {
		t.Errorf("failedRobots[0].previousVersion = %q, want 2.4.0 carried forward", got)
	}
}

// THE BUG, model side. Same rule, and the stakes named in the item: a stranded ModelRollout
// leaves model-granted capabilities suspended with no owner.
func TestModelRollout_UnpausedFailureReachesFailed(t *testing.T) {
	roll := pickerRollout()
	roll.Spec.Strategy.RollingUpdate.PauseOnError = apiBool(false)
	r, c := newRolloutReconciler(t, roll, failedModelRobot("r1", "inference health check failed"))

	reconcileRollout(t, r)

	got := getRollout(t, c, "roll")
	if got.Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Fatalf("phase = %s, want Failed: pauseOnError is off and the only robot failed (%+v)", got.Status.Phase, got.Status)
	}
	if _, err := (&webhook.ModelRolloutValidator{}).ValidateDelete(context.Background(), got); err != nil {
		t.Fatalf("a settled, failed model rollout must be deletable, got: %v", err)
	}
}

// Model side control: Manual + pauseOnError (the default) stays Paused, not Failed.
func TestModelRollout_PausedStillBeatsFailed(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(), failedModelRobot("r1", "health check failed"))
	reconcileRollout(t, r)

	if got := getRollout(t, c, "roll").Status.Phase; got != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused under the default pauseOnError", got)
	}
}
