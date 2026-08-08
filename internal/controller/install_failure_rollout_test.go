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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// Rollout controllers consuming the projected install state (ADR-0033).
//
// These are the REACHABILITY tests. All three install-outcome audit rows previously had
// writers that referenced their event constant — enough for the mechanical Status check —
// but keyed on state nothing produced, so the branches could not execute. Each test below
// drives the projected state a real adapter now reports and asserts the entry appears.

// failedFirmware marks a robot as having reported a firmware install failure at reportedAt.
func failedFirmware(name, remainsOn, reason string, reportedAt time.Time) *fleetv1.Robot {
	rob := targetRobot(name, fleetv1.RobotPhaseIdle, 90)
	rob.Status.FirmwareInstall = &fleetv1.FirmwareInstallState{
		Status:         fleetv1.FirmwareInstallFailed,
		RunningVersion: remainsOn,
		FailureReason:  reason,
		ReportedAt:     &metav1.Time{Time: reportedAt},
	}
	return rob
}

// seedBatch puts a robot in the rollout's active batch as of startedAt.
func seedBatch(ro *fleetv1.FirmwareRollout, robotName string, startedAt time.Time) {
	ro.Status.Phase = fleetv1.RolloutPhaseInProgress
	ro.Status.CurrentBatch = append(ro.Status.CurrentBatch, fleetv1.RolloutBatchRobot{
		RobotName: robotName, Namespace: rolloutNS,
		UpdateStartedAt: &metav1.Time{Time: startedAt},
		PreviousVersion: "2.4.0",
		UpdatePhase:     "Installing",
	})
}

func TestInstallFailure_FirmwareIsSealedAndCounted(t *testing.T) {
	dispatched := time.Now().Add(-10 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := fwRollout(sigRef)
	seedBatch(ro, "r1", dispatched)
	rec := &captureRecorder{}
	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "flash verify failed after reboot", dispatched.Add(time.Minute)))
	r.Audit = rec

	reconcileFirmware(t, r)

	got := entriesOfType(rec, audit.EventFirmwareInstallFailed)
	if len(got) != 1 {
		t.Fatalf("want one FIRMWARE_INSTALL_FAILED, got %d", len(got))
	}
	e := got[0]
	if e.Outcome != audit.OutcomeError {
		t.Errorf("a failed install is an Error outcome, got %q", e.Outcome)
	}
	if e.Detail["reason"] != "flash verify failed after reboot" {
		t.Errorf("the adapter's reason must carry through, got %q", e.Detail["reason"])
	}
	// The required detail field, and the one an operator acts on: a failed install may
	// leave the robot anywhere, so this is reported rather than assumed.
	if e.Detail["robot_remains_on_version"] != "2.4.0" {
		t.Errorf("robot_remains_on_version = %q, want the reported 2.4.0", e.Detail["robot_remains_on_version"])
	}

	var after fleetv1.FirmwareRollout
	if err := c.Get(t.Context(), rolloutKey(), &after); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if len(after.Status.FailedRobots) != 1 || after.Status.FailedRobots[0].RobotName != "r1" {
		t.Fatalf("failedRobots not populated: %+v", after.Status.FailedRobots)
	}
	if after.Status.FailedRobots[0].Reason == "" {
		t.Error("the failure reason must be on the rollout too, not only in the chain")
	}
	// A failed robot is not outstanding work. Counting it as pending leaves the rollout
	// looking permanently unfinished.
	if after.Status.RobotsPending != 0 {
		t.Errorf("robotsPending = %d, want 0 (the only robot failed)", after.Status.RobotsPending)
	}
}

func TestInstallFailure_FirmwareSealsOnceAcrossReconciles(t *testing.T) {
	// RA-1. A failed robot stays failed; without the edge the chain gains an entry per
	// reconcile per robot and buries the incident it exists to record.
	dispatched := time.Now().Add(-10 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := fwRollout(sigRef)
	seedBatch(ro, "r1", dispatched)
	rec := &captureRecorder{}
	r, _ := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "boom", dispatched.Add(time.Minute)))
	r.Audit = rec

	reconcileFirmware(t, r)
	reconcileFirmware(t, r)
	reconcileFirmware(t, r)
	if n := len(entriesOfType(rec, audit.EventFirmwareInstallFailed)); n != 1 {
		t.Fatalf("must seal once per robot per rollout, got %d", n)
	}
}

func TestInstallFailure_StaleFailureFromAnEarlierRolloutIsNotAttributed(t *testing.T) {
	// THE MIS-ATTRIBUTION CASE. A robot still carrying a Failed state from a PREVIOUS
	// rollout would otherwise be counted as having failed this one the instant it was
	// dispatched — before it had done anything at all — and a false failure entry would be
	// sealed into a chain that cannot be corrected.
	dispatched := time.Now().Add(-5 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := fwRollout(sigRef)
	seedBatch(ro, "r1", dispatched)
	rec := &captureRecorder{}
	r, _ := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		// Reported an hour BEFORE this rollout dispatched to the robot.
		failedFirmware("r1", "2.4.0", "an older rollout's failure", dispatched.Add(-time.Hour)))
	r.Audit = rec

	reconcileFirmware(t, r)
	if n := len(entriesOfType(rec, audit.EventFirmwareInstallFailed)); n != 0 {
		t.Fatalf("a pre-dispatch failure must not be attributed to this rollout, got %d entries", n)
	}
}

func TestInstallFailure_UnorderableReportIsNotAttributed(t *testing.T) {
	// Without both timestamps the order cannot be established. Refusing to attribute is the
	// fail-closed choice: a wrong entry in the chain is worse than a missing one, because
	// the chain is the thing an incident review is supposed to be able to trust.
	dispatched := time.Now().Add(-5 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := fwRollout(sigRef)
	seedBatch(ro, "r1", dispatched)
	rob := failedFirmware("r1", "2.4.0", "no timestamp", dispatched)
	rob.Status.FirmwareInstall.ReportedAt = nil
	rec := &captureRecorder{}
	r, _ := newFirmwareReconciler(t, ro, signingConfig(true), secret, rob)
	r.Audit = rec

	reconcileFirmware(t, r)
	if n := len(entriesOfType(rec, audit.EventFirmwareInstallFailed)); n != 0 {
		t.Fatalf("an unorderable report must not be attributed, got %d entries", n)
	}
}

func TestInstallFailure_RobotOutsideTheBatchIsNotAttributed(t *testing.T) {
	// A robot this rollout never dispatched to cannot have failed this rollout's install.
	secret, sigRef := signingFixture(t, fwChecksum)
	rec := &captureRecorder{}
	r, _ := newFirmwareReconciler(t, fwRollout(sigRef), signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "unrelated", time.Now()))
	r.Audit = rec

	reconcileFirmware(t, r)
	if n := len(entriesOfType(rec, audit.EventFirmwareInstallFailed)); n != 0 {
		t.Fatalf("a robot outside the batch must not be attributed, got %d entries", n)
	}
}

// ── Model rows: the reachability the projection restores ──────────────────────

func TestInstallFailure_ModelUpdateFailedIsNowReachable(t *testing.T) {
	// Before the projection, Robot.status.installedModels[] was written only by the control
	// plane and only ever with Updating, so classifyModel could never reach modelFailed and
	// this entry could not be produced by any input.
	rec := &recordingAudit{}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusFailed,
		RunningVersion: "3.0.0", FailureReason: "inference health check failed",
	}}
	r, _ := newRolloutReconciler(t, pickerRollout(), rob)
	r.Audit = rec

	reconcileRollout(t, r)
	got := rec.ofType(audit.EventModelUpdateFailed)
	if len(got) != 1 {
		t.Fatalf("want one MODEL_UPDATE_FAILED, got %d", len(got))
	}
	if got[0].Detail["reason"] != "inference health check failed" {
		t.Errorf("the adapter's reason must carry through, got %q", got[0].Detail["reason"])
	}
}

func TestInstallFailure_ModelUpdateSucceededIsNowReachable(t *testing.T) {
	// The other half: modelDone requires Active AT THE TARGET VERSION, and runningVersion
	// had no production writer at all. A robot could enter Updating and never leave, so a
	// ModelRollout could not complete — this is a functional fix, not only an evidence one.
	rec := &recordingAudit{}
	rob := targetRobot("r1", fleetv1.RobotPhaseIdle, 90)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.2.1",
	}}
	ro := pickerRollout()
	ro.Status.Phase = fleetv1.RolloutPhaseInProgress
	ro.Status.CurrentBatch = []fleetv1.RolloutBatchRobot{{RobotName: "r1", Namespace: rolloutNS}}
	r, c := newRolloutReconciler(t, ro, rob)
	r.Audit = rec

	reconcileRollout(t, r)
	if n := len(rec.ofType(audit.EventModelUpdateSucceeded)); n != 1 {
		t.Fatalf("want one MODEL_UPDATE_SUCCEEDED, got %d", n)
	}
	var after fleetv1.ModelRollout
	if err := c.Get(t.Context(), modelRolloutKey(), &after); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if after.Status.Phase != fleetv1.RolloutPhaseSucceeded {
		t.Errorf("rollout phase = %s, want Succeeded — a rollout must be able to finish", after.Status.Phase)
	}
}

func rolloutKey() types.NamespacedName {
	return types.NamespacedName{Name: "fw", Namespace: rolloutNS}
}

func modelRolloutKey() types.NamespacedName {
	return types.NamespacedName{Name: "roll", Namespace: rolloutNS}
}
