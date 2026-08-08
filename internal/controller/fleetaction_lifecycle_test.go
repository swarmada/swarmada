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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// failedAction builds a FleetAction already in the Failed phase for exercising the
// onFailure/retryPolicy contract in the reconciler's terminal branch.
func failedAction(name, robot string, retryCount int32, onFailure fleetv1.ActionFailurePolicy, scheduled, failedAt *metav1.Time) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: onFailure},
		Status: fleetv1.FleetActionStatus{
			Phase:                fleetv1.ActionPhaseFailed,
			AssignedRobot:        robot,
			AssignmentGeneration: 5,
			RetryCount:           retryCount,
			ScheduledAt:          scheduled,
			FailedAt:             failedAt,
			StartedAt:            scheduled,
		},
	}
}

func ago(d time.Duration) *metav1.Time  { t := metav1.NewTime(time.Now().Add(-d)); return &t }
func soon(d time.Duration) *metav1.Time { t := metav1.NewTime(time.Now().Add(d)); return &t }

// A Succeeded action retires: the robot is freed for reuse and the lease cleared.
func TestReconcile_Succeeded_RetiresAndFreesRobot(t *testing.T) {
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseSucceeded, 5, soon(leaseDuration)),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)

	reconcileAction(t, r, "t1")

	robot := getRobot(t, c, "r1", actionNS)
	if robot.Status.AssignedAction != "" {
		t.Fatalf("robot still bound to a completed task: assignedAction=%q", robot.Status.AssignedAction)
	}
	if robot.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot phase = %s, want Idle after task success", robot.Status.Phase)
	}
	if getAction(t, c, "t1").Status.LeaseExpiresAt != nil {
		t.Fatal("leaseExpiresAt not cleared on a terminal task")
	}
}

// onFailure: Requeue with retries remaining and the backoff elapsed returns the
// action to Pending, increments retryCount, frees the robot, and clears the
// current-attempt timestamps.
func TestReconcile_Failed_RequeueAfterBackoff(t *testing.T) {
	r, c := newActionReconciler(t,
		failedAction("t1", "r1", 0, fleetv1.ActionFailureRequeue, ago(time.Minute), ago(time.Minute)),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending after requeue", ft.Status.Phase)
	}
	if ft.Status.RetryCount != 1 {
		t.Fatalf("retryCount = %d, want 1", ft.Status.RetryCount)
	}
	if ft.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q, want cleared on requeue", ft.Status.AssignedRobot)
	}
	if ft.Status.FailedAt != nil || ft.Status.ScheduledAt != nil || ft.Status.StartedAt != nil {
		t.Fatal("current-attempt timestamps not cleared on requeue")
	}
	if getRobot(t, c, "r1", actionNS).Status.AssignedAction != "" {
		t.Fatal("robot not freed on requeue")
	}
}

// Within the backoff window the action stays Failed (robot already freed) and the
// reconcile is requeued for a later retry.
func TestReconcile_Failed_BackoffNotElapsed_Holds(t *testing.T) {
	r, c := newActionReconciler(t,
		failedAction("t1", "r1", 0, fleetv1.ActionFailureRequeue, ago(time.Minute), ago(time.Second)),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)

	res := reconcileAction(t, r, "t1")

	if res.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want a positive backoff", res.RequeueAfter)
	}
	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %s, want Failed during backoff", ft.Status.Phase)
	}
	if ft.Status.RetryCount != 0 {
		t.Fatalf("retryCount = %d, want 0 (not yet requeued)", ft.Status.RetryCount)
	}
	if getRobot(t, c, "r1", actionNS).Status.AssignedAction != "" {
		t.Fatal("robot should be freed immediately on failure, even during retry backoff")
	}
}

// When retries are exhausted the action stays Failed and a one-shot operator alert
// is emitted (exhausted Requeue falls through to Alert behaviour).
func TestReconcile_Failed_RetriesExhausted_TerminalAlert(t *testing.T) {
	action := failedAction("t1", "r1", 1, fleetv1.ActionFailureRequeue, ago(time.Minute), ago(time.Minute))
	action.Spec.RetryPolicy = &fleetv1.ActionRetryPolicy{MaxRetries: 1, BackoffSeconds: 30}
	r, c := newActionReconciler(t, action, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	rec := record.NewFakeRecorder(4)
	r.Recorder = rec

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %s, want Failed (retries exhausted)", ft.Status.Phase)
	}
	assertEvent(t, rec, "FleetActionFailed")
	if _, ok := ft.Annotations[annFailureAlerted]; !ok {
		t.Fatal("failure-alerted annotation not set after alert")
	}
}

// onFailure: Alert leaves the action Failed and emits an operator event.
func TestReconcile_Failed_Alert_EmitsEvent(t *testing.T) {
	r, c := newActionReconciler(t,
		failedAction("t1", "r1", 0, fleetv1.ActionFailureAlert, ago(time.Minute), ago(time.Minute)),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	rec := record.NewFakeRecorder(4)
	r.Recorder = rec

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatal("Alert policy must leave the task Failed")
	}
	assertEvent(t, rec, "FleetActionFailed")
}

// onFailure: Abandon leaves the action Failed silently — no requeue, no alert.
func TestReconcile_Failed_Abandon_NoAlert(t *testing.T) {
	r, c := newActionReconciler(t,
		failedAction("t1", "r1", 0, fleetv1.ActionFailureAbandon, ago(time.Minute), ago(time.Minute)),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	rec := record.NewFakeRecorder(4)
	r.Recorder = rec

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatal("Abandon policy must leave the task Failed")
	}
	assertNoEvent(t, rec)
}

// A pre-scheduling deadline failure (never assigned, so scheduledAt is nil) is
// permanently unstartable and stays Failed even under onFailure: Requeue.
func TestReconcile_Failed_PreSchedulingDeadline_StaysTerminal(t *testing.T) {
	// scheduled=nil marks a action that was never assigned to a robot.
	r, c := newActionReconciler(t,
		failedAction("t1", "", 0, fleetv1.ActionFailureRequeue, nil, ago(time.Minute)),
	)
	r.Recorder = record.NewFakeRecorder(4)

	reconcileAction(t, r, "t1")

	if ph := getAction(t, c, "t1").Status.Phase; ph != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %s, want Failed — an unstartable task must not be requeued", ph)
	}
}

func assertEvent(t *testing.T, rec *record.FakeRecorder, want string) {
	t.Helper()
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, want) {
			t.Fatalf("event = %q, want it to contain %q", e, want)
		}
	default:
		t.Fatalf("expected an event containing %q, got none", want)
	}
}

func assertNoEvent(t *testing.T, rec *record.FakeRecorder) {
	t.Helper()
	select {
	case e := <-rec.Events:
		t.Fatalf("expected no event, got %q", e)
	default:
	}
}
