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

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/webhook"
)

// firmware pauseOnError (§9.1.8.5). The firmware controller had ZERO references to
// PauseOnError and computeFirmwareStatus could not return Paused at all, so a rollout with
// the documented default kept dispatching after a robot failed — a bad image reached the
// whole fleet one batch at a time. Firmware has no rollbackPolicy: Auto either, so this is
// the ONLY guard on that path.

// pausingFwRollout is fwRollout with pauseOnError explicitly on. The CRD default is true, but
// a fake client applies no CRD defaulting, so the fixture states it.
func pausingFwRollout(sigRef string) *fleetv1.FirmwareRollout {
	ro := fwRollout(sigRef)
	ro.Spec.Strategy.RollingUpdate.PauseOnError = true
	return ro
}

// pausedFirmware drives a firmware rollout to Paused: r1 failed its install, r2 is untouched.
func pausedFirmware(t *testing.T) (*FirmwareRolloutReconciler, client.Client, *captureRecorder) {
	t.Helper()
	dispatched := time.Now().Add(-10 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := pausingFwRollout(sigRef)
	seedBatch(ro, "r1", dispatched)
	rec := &captureRecorder{}
	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "flash verify failed after reboot", dispatched.Add(time.Minute)),
		targetRobot("r2", fleetv1.RobotPhaseIdle, 90))
	r.Audit = rec
	reconcileFirmware(t, r)
	return r, c, rec
}

// THE BUG. A failed install must stop the rollout dispatching to anyone else.
func TestFirmwareRollout_PauseOnErrorHaltsDispatch(t *testing.T) {
	r, c, _ := pausedFirmware(t)
	_ = r

	fw := getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s, want Paused after an install failure with pauseOnError (%+v)", fw.Status.Phase, fw.Status)
	}
	if robotPending(t, c, "r2") {
		t.Fatal("a paused firmware rollout dispatched to another robot — the bad image is still spreading")
	}
	if fw.Status.PausedAt == nil {
		t.Error("status.pausedAt must be stamped when the rollout pauses")
	}
}

// pauseOnError=false keeps the old behaviour: the rollout carries on past a failure. This is
// the control — without it, a passing pause test could just mean the fixture never dispatched.
func TestFirmwareRollout_PauseOnErrorOffKeepsDispatching(t *testing.T) {
	dispatched := time.Now().Add(-10 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := fwRollout(sigRef) // PauseOnError defaults to the Go zero value: false
	ro.Spec.Strategy.RollingUpdate.MaxUnavailable = "2"
	seedBatch(ro, "r1", dispatched)
	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "flash verify failed", dispatched.Add(time.Minute)),
		targetRobot("r2", fleetv1.RobotPhaseIdle, 90))

	reconcileFirmware(t, r)

	if got := getFirmwareRollout(t, c).Status.Phase; got == fleetv1.RolloutPhasePaused {
		t.Fatalf("phase = %s; pauseOnError is off, the rollout must not pause", got)
	}
	if !robotPending(t, c, "r2") {
		t.Fatal("with pauseOnError off the rollout should have dispatched to r2")
	}
}

// §9.5.4 requires FIRMWARE_ROLLOUT_PAUSED. Until pauseOnError existed the transition it keys on
// could not occur, so the required event had no reachable writer.
func TestFirmwareRollout_PauseIsSealed(t *testing.T) {
	_, _, rec := pausedFirmware(t)
	entries := entriesOfType(rec, audit.EventFirmwareRolloutPaused)
	if len(entries) != 1 {
		t.Fatalf("FIRMWARE_ROLLOUT_PAUSED entries = %d, want exactly 1", len(entries))
	}
	if entries[0].Detail["failed_robots"] != "r1" {
		t.Errorf("detail.failed_robots = %q, want r1", entries[0].Detail["failed_robots"])
	}
}

// The entry is sealed on the EDGE: a rollout sitting Paused across many reconciles must not
// seal one entry per pass, or the audit log becomes unreadable exactly when it matters.
func TestFirmwareRollout_PauseSealedOncePerTransition(t *testing.T) {
	r, _, rec := pausedFirmware(t)
	for i := 0; i < 3; i++ {
		reconcileFirmware(t, r)
	}
	if n := len(entriesOfType(rec, audit.EventFirmwareRolloutPaused)); n != 1 {
		t.Fatalf("FIRMWARE_ROLLOUT_PAUSED entries = %d after repeated reconciles, want 1", n)
	}
}

// The acceptance case for firmware: a Paused firmware rollout resumes and then deletes.
func TestFirmwareRolloutResume_ThenDeletable(t *testing.T) {
	r, c, rec := pausedFirmware(t)
	ctx := context.Background()

	fw := getFirmwareRollout(t, c)
	if _, err := (&webhook.FirmwareRolloutValidator{}).ValidateDelete(ctx, fw); err == nil {
		t.Fatal("a Paused firmware rollout must not be deletable; the wedge premise no longer holds")
	}

	base := fw.DeepCopy()
	fw.Annotations = map[string]string{rolloutResumeAnnotation: "r1 flash chip is dead; excluding it"}
	if err := c.Patch(ctx, fw, client.MergeFrom(base)); err != nil {
		t.Fatalf("requesting resume: %v", err)
	}
	reconcileFirmware(t, r) // consumes the annotation
	reconcileFirmware(t, r) // r2 enters the batch

	fw = getFirmwareRollout(t, c)
	if fw.Status.Phase == fleetv1.RolloutPhasePaused {
		t.Fatalf("still Paused after resume: %+v", fw.Status)
	}
	if len(fw.Status.ExcludedRobots) != 1 || fw.Status.ExcludedRobots[0] != "r1" {
		t.Fatalf("excludedRobots = %v, want [r1]", fw.Status.ExcludedRobots)
	}
	if n := len(entriesOfType(rec, audit.EventRolloutResumed)); n != 1 {
		t.Errorf("ROLLOUT_RESUMED entries = %d, want 1", n)
	}

	// Bring r2 to the new version so the rollout can settle; r1 stays excluded.
	r2 := getRobot(t, c, "r2", rolloutNS)
	r2.Status.FirmwareVersion = "2.5.0"
	if err := c.Status().Update(ctx, r2); err != nil {
		t.Fatalf("advancing r2: %v", err)
	}
	reconcileFirmware(t, r)

	fw = getFirmwareRollout(t, c)
	if fw.Status.Phase != fleetv1.RolloutPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded once every robot is done or excluded (%+v)", fw.Status.Phase, fw.Status)
	}
	if _, err := (&webhook.FirmwareRolloutValidator{}).ValidateDelete(ctx, fw); err != nil {
		t.Fatalf("a resumed, settled firmware rollout must be deletable, got: %v", err)
	}
}

// Resume must not re-latch Paused off the same failed robot.
func TestFirmwareRolloutResume_DoesNotReLatch(t *testing.T) {
	r, c, _ := pausedFirmware(t)
	ctx := context.Background()

	fw := getFirmwareRollout(t, c)
	base := fw.DeepCopy()
	fw.Annotations = map[string]string{rolloutResumeAnnotation: "excluding r1"}
	if err := c.Patch(ctx, fw, client.MergeFrom(base)); err != nil {
		t.Fatalf("requesting resume: %v", err)
	}
	reconcileFirmware(t, r)

	for i := 0; i < 3; i++ {
		reconcileFirmware(t, r)
		if got := getFirmwareRollout(t, c).Status.Phase; got == fleetv1.RolloutPhasePaused {
			t.Fatalf("re-paused on reconcile %d off the same failed robot", i+1)
		}
	}
}
