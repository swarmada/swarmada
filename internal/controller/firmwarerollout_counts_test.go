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
)

// The FirmwareRollout aggregate counters and the count identity they have to satisfy.
//
// status.robotsFailed had no writer anywhere on the firmware path — the only RobotsFailed
// write in the tree was the model controller's — and status.robotsSkipped had none on either.
// Both read 0 for the life of every rollout, so a client watching the counters saw a rollout
// with failures report none, and a robot held back by a safety constraint was invisible.
//
// The identity the chapter publishes:
//
//	robotsTotal = robotsUpdated + robotsPending + robotsFailed + len(excludedRobots)
//
// currentBatch is not a term in it: a robot mid-install has not settled, so it is counted in
// robotsPending. robotsSkipped is likewise a sub-count of robotsPending, not a sibling — a
// robot skipped for battery or maintenance window is still awaiting its update.

// countIdentity asserts the published identity closes on a live rollout.
func countIdentity(t *testing.T, st fleetv1.FirmwareRolloutStatus) {
	t.Helper()
	sum := st.RobotsUpdated + st.RobotsPending + st.RobotsFailed + int32(len(st.ExcludedRobots))
	if sum != st.RobotsTotal {
		t.Errorf("identity does not close: updated %d + pending %d + failed %d + excluded %d = %d, robotsTotal = %d",
			st.RobotsUpdated, st.RobotsPending, st.RobotsFailed, len(st.ExcludedRobots), sum, st.RobotsTotal)
	}
	if st.RobotsSkipped > st.RobotsPending {
		t.Errorf("robotsSkipped %d exceeds robotsPending %d; it is a sub-count of pending, not a sibling",
			st.RobotsSkipped, st.RobotsPending)
	}
}

// r1 failed its install, r2 is held back by a safety constraint, r3 already runs the target.
func countedFirmware(t *testing.T) fleetv1.FirmwareRolloutStatus {
	t.Helper()
	dispatched := time.Now().Add(-10 * time.Minute)
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := pausingFwRollout(sigRef)
	seedBatch(ro, "r1", dispatched)

	done := targetRobot("r3", fleetv1.RobotPhaseIdle, 90)
	done.Status.FirmwareVersion = "2.5.0"

	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret,
		failedFirmware("r1", "2.4.0", "flash verify failed", dispatched.Add(time.Minute)),
		targetRobot("r2", fleetv1.RobotPhaseIdle, 10), // below minBatteryPct: skipped this cycle
		done)
	reconcileFirmware(t, r)
	return getFirmwareRollout(t, c).Status
}

// THE BUG. A rollout with a failed install must report it in robotsFailed.
func TestFirmwareCounts_RobotsFailedIsWritten(t *testing.T) {
	st := countedFirmware(t)
	if st.RobotsFailed != 1 {
		t.Fatalf("robotsFailed = %d, want 1 (r1 failed its install); status=%+v", st.RobotsFailed, st)
	}
	if len(st.FailedRobots) != int(st.RobotsFailed) {
		t.Errorf("robotsFailed = %d but failedRobots has %d entries; the counter and the list must agree",
			st.RobotsFailed, len(st.FailedRobots))
	}
}

// A robot held back by a safety constraint is surfaced rather than silently absent.
func TestFirmwareCounts_RobotsSkippedIsWritten(t *testing.T) {
	st := countedFirmware(t)
	if st.RobotsSkipped != 1 {
		t.Fatalf("robotsSkipped = %d, want 1 (r2 is below minBatteryPct); status=%+v", st.RobotsSkipped, st)
	}
}

// The identity has to close on the same live rollout, not only on the worked example.
func TestFirmwareCounts_IdentityReproduces(t *testing.T) {
	st := countedFirmware(t)
	if st.RobotsTotal != 3 || st.RobotsUpdated != 1 || st.RobotsPending != 1 {
		t.Fatalf("counts = total %d / updated %d / pending %d, want 3 / 1 / 1 (%+v)",
			st.RobotsTotal, st.RobotsUpdated, st.RobotsPending, st)
	}
	countIdentity(t, st)
}

// And after an operator resume moves the failure into excludedRobots, which is the one case
// where the identity's last term is non-zero.
func TestFirmwareCounts_IdentityHoldsAfterResume(t *testing.T) {
	r, c, _ := pausedFirmware(t)
	fw := getFirmwareRollout(t, c)
	base := fw.DeepCopy()
	fw.Annotations = map[string]string{rolloutResumeAnnotation: "r1 flash chip is dead"}
	if err := c.Patch(context.Background(), fw, client.MergeFrom(base)); err != nil {
		t.Fatalf("requesting resume: %v", err)
	}
	reconcileFirmware(t, r)
	reconcileFirmware(t, r)

	st := getFirmwareRollout(t, c).Status
	if len(st.ExcludedRobots) != 1 {
		t.Fatalf("excludedRobots = %v, want one entry", st.ExcludedRobots)
	}
	if st.RobotsFailed != 0 {
		t.Errorf("robotsFailed = %d after resume, want 0: the failure moved to excludedRobots", st.RobotsFailed)
	}
	countIdentity(t, st)
}
