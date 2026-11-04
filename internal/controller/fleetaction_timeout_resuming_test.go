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

// A stop holds the work a robot records until the operator's resume, Resuming included
// (RFC-0001 #safety-action-table, I1). The completion timeout is no exception: row A19's
// timeout waits while a stop holds the robot, so a Resuming robot's InProgress action whose
// window has elapsed is neither cancelled on the wire nor failed until the operator releases
// the robot. Then the timeout applies as usual.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

func timedOutOnResumingRobot(t *testing.T) (*FleetActionReconciler, client.Client, *fakeCommander) {
	t.Helper()
	lease := &metav1.Time{Time: time.Now().Add(20 * time.Second).Truncate(time.Second)}
	action := assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 1, lease)
	timeout := int32(5)
	action.Spec.TimeoutSeconds = &timeout
	action.Spec.OnFailure = fleetv1.ActionFailureAlert
	action.Status.StartedAt = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	robot := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1")
	robot.Status.EstopState = fleetv1.RobotEstopResuming
	r, c := newActionReconciler(t, action, robot)
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}
	r.Commander = cmd
	return r, c, cmd
}

func TestTimeout_ResumingRobot_NoCancelAndNoFailureBeforeTheRelease(t *testing.T) {
	r, c, cmd := timedOutOnResumingRobot(t)
	for i := 0; i < 2; i++ {
		reconcileAction(t, r, "t1")
		got := getAction(t, c, "t1")
		if got.Status.Phase != fleetv1.ActionPhaseInProgress || got.Status.AssignedRobot != "r1" {
			t.Fatalf("reconcile %d: phase=%s robot=%q on a Resuming robot, want InProgress on r1 (A19 waits while a stop holds the robot)",
				i+1, got.Status.Phase, got.Status.AssignedRobot)
		}
		if got.Status.Message == timingOutMessage {
			t.Fatalf("reconcile %d: message %q: the timeout started on a Resuming robot", i+1, got.Status.Message)
		}
		if len(cmd.cancels) != 0 {
			t.Fatalf("reconcile %d: %d cancel(s) sent to a Resuming robot (%v), want none", i+1, len(cmd.cancels), cmd.cancelTo)
		}
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "t1" {
		t.Fatalf("r1 records %q, want t1 still bound", rob.Status.AssignedAction)
	}
}

func TestTimeout_ResumingRobot_TimeoutAppliesAfterTheRelease(t *testing.T) {
	r, c, cmd := timedOutOnResumingRobot(t)
	reconcileAction(t, r, "t1")
	if p := getAction(t, c, "t1").Status.Phase; p != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s before the release, want InProgress", p)
	}

	// The operator's resume: the robot leaves Resuming for Normal (row S6).
	rob := getRobot(t, c, "r1", actionNS)
	base := rob.DeepCopy()
	rob.Status.EstopState = fleetv1.RobotEstopNormal
	if err := c.Status().Patch(context.Background(), rob, client.MergeFrom(base)); err != nil {
		t.Fatalf("release r1: %v", err)
	}

	reconcileAction(t, r, "t1")
	if p := getAction(t, c, "t1").Status.Phase; p != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %s after the release, want Failed: the window has elapsed and the cancel is acknowledged", p)
	}
	if len(cmd.cancels) != 1 || cmd.cancelTo[0] != "r1" {
		t.Fatalf("cancels after the release = %d to %v, want one to r1", len(cmd.cancels), cmd.cancelTo)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "" {
		t.Fatalf("r1 still records %q after the timeout failure", rob.Status.AssignedAction)
	}
}
