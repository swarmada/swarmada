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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// A cancel acknowledged CANCEL_DISPOSITION_STOP_UNCONFIRMED: the stop was commanded and the
// robot's rest could not be witnessed. The acknowledgement does not prove the lease dead
// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee): on a requeue the
// action goes Failed and the robot stays bound until the lease has provably expired; every
// other path that waits for a confirmed stop keeps waiting for that expiry.

func unconfirmed() *fakeCommander {
	return &fakeCommander{cancelAck: true, cancelDisp: command.CancelStopUnconfirmed}
}

func ageLease(t *testing.T, c client.Client, name string, by time.Duration) {
	t.Helper()
	ft := getAction(t, c, name)
	base := ft.DeepCopy()
	ft.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(-by)}
	if err := c.Status().Patch(context.Background(), ft, client.MergeFrom(base)); err != nil {
		t.Fatalf("age lease: %v", err)
	}
}

func assertBound(t *testing.T, c client.Client, action, robot string) {
	t.Helper()
	if rob := getRobot(t, c, robot, actionNS); rob.Status.AssignedAction != action {
		t.Errorf("%s released (assignedAction %q) before the lease was provably dead", robot, rob.Status.AssignedAction)
	}
}

// ── The requeue path: Failed now, the robot bound until the lease expires ─────────

func TestRequeue_StopUnconfirmed_FailsAndKeepsTheRobotBound(t *testing.T) {
	for _, reason := range []string{"zone maintenance", reasonCapabilityLost} {
		t.Run(reason, func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration).Truncate(time.Second)}
			a := requeueReqAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease)
			a.Annotations[annRequeueRequested] = reason
			a.Spec.OnFailure = fleetv1.ActionFailureRequeue
			r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
			r.Commander = unconfirmed()

			reconcileAction(t, r, "t1")

			ft := getAction(t, c, "t1")
			if ft.Status.Phase != fleetv1.ActionPhaseFailed || ft.Status.FailureReason != "StopUnconfirmed" {
				t.Fatalf("phase=%s failureReason=%q, want Failed / StopUnconfirmed", ft.Status.Phase, ft.Status.FailureReason)
			}
			if ft.Status.AssignedRobot != "r1" || ft.Status.LeaseExpiresAt == nil || !ft.Status.LeaseExpiresAt.Equal(lease) {
				t.Fatalf("assignedRobot=%q lease=%v, want r1 and the horizon kept at %v", ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt, lease)
			}
			if _, ok := ft.Annotations[annRequeueRequested]; ok {
				t.Error("requeue annotation not cleared")
			}
			assertBound(t, c, "t1", "r1")

			// Further passes while the lease is live: held, no retry, no release.
			for i := 0; i < 3; i++ {
				reconcileAction(t, r, "t1")
			}
			ft = getAction(t, c, "t1")
			if ft.Status.Phase != fleetv1.ActionPhaseFailed || ft.Status.RetryCount != 0 {
				t.Fatalf("phase=%s retryCount=%d during the hold, want Failed and no retry", ft.Status.Phase, ft.Status.RetryCount)
			}
			assertBound(t, c, "t1", "r1")
		})
	}
}

func TestRequeue_StopUnconfirmed_NilHorizonIsStamped(t *testing.T) {
	a := requeueReqAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, nil)
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = unconfirmed()
	before := time.Now()

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.LeaseExpiresAt == nil {
		t.Fatal("no lease horizon stamped; a nil horizon proves nothing and would never release")
	}
	if got := ft.Status.LeaseExpiresAt.Sub(before); got < defaultLeaseDuration-time.Second || got > defaultLeaseDuration+5*time.Second {
		t.Fatalf("stamped horizon is %s after the acknowledgement, want one lease duration (%s)", got, defaultLeaseDuration)
	}
	assertBound(t, c, "t1", "r1")
}

func TestFailedStopUnconfirmed_ReleasedAtLeaseDeathThenOnFailureApplies(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := requeueReqAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease)
	a.Spec.OnFailure = fleetv1.ActionFailureRequeue
	a.Spec.RetryPolicy = &fleetv1.ActionRetryPolicy{MaxRetries: 3, BackoffSeconds: 0}
	a.Status.ScheduledAt = &metav1.Time{Time: time.Now()}
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = unconfirmed()
	reconcileAction(t, r, "t1")
	assertBound(t, c, "t1", "r1")

	ageLease(t, c, "t1", time.Hour)
	reconcileAction(t, r, "t1")

	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "" || rob.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot not released at lease death: assignedAction=%q phase=%s", rob.Status.AssignedAction, rob.Status.Phase)
	}
	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePending || ft.Status.RetryCount != 1 {
		t.Fatalf("phase=%s retryCount=%d after release, want onFailure Requeue: Pending, retry 1", ft.Status.Phase, ft.Status.RetryCount)
	}
}

// A manager restart re-derives the hold from what the API holds: a fresh reconciler over
// the same objects still holds, and still releases at lease death.
func TestFailedStopUnconfirmed_HeldAcrossARestart(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := requeueReqAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease)
	a.Spec.OnFailure = fleetv1.ActionFailureAlert
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = unconfirmed()
	reconcileAction(t, r, "t1")

	restarted := &FleetActionReconciler{Client: c, Scheme: r.Scheme, Scheduler: r.Scheduler, Commander: unconfirmed()}
	res := reconcileAction(t, restarted, "t1")
	assertBound(t, c, "t1", "r1")
	if res.RequeueAfter <= 0 || res.RequeueAfter > defaultLeaseDuration+defaultLeaseClockSkew {
		t.Errorf("requeue after a restart = %s, want a wake-up at the lease horizon", res.RequeueAfter)
	}

	ageLease(t, c, "t1", time.Hour)
	reconcileAction(t, restarted, "t1")
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "" {
		t.Fatalf("not released at lease death after a restart: %q", rob.Status.AssignedAction)
	}
	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseFailed || ft.Status.LeaseExpiresAt != nil {
		t.Fatalf("phase=%s lease=%v, want Failed (onFailure Alert) and the horizon cleared", ft.Status.Phase, ft.Status.LeaseExpiresAt)
	}
	// The hold does not come back once the horizon is cleared.
	reconcileAction(t, restarted, "t1")
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "" {
		t.Fatalf("robot re-bound after release: %q", rob.Status.AssignedAction)
	}
}

// ── Every other path that waits for a confirmed stop: not yet confirmed ───────────

func TestCancel_StopUnconfirmed_HoldsUntilLeaseDeathThenCancelled(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		cancelAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease, "maintenance"),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = unconfirmed()

	reconcileAction(t, r, "t1")
	if ft := getAction(t, c, "t1"); ft.Status.Phase == fleetv1.ActionPhaseCancelled {
		t.Fatal("finalised Cancelled on a STOP_UNCONFIRMED acknowledgement, before the lease was provably dead")
	}
	assertBound(t, c, "t1", "r1")

	ageLease(t, c, "t1", time.Hour)
	reconcileAction(t, r, "t1")
	if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %s at lease death, want Cancelled (the operator's cancel still ends Cancelled)", ft.Status.Phase)
	}
}

func TestTimeout_StopUnconfirmed_HoldsUntilLeaseDeath(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(20 * time.Second)}
	action := assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 1, lease)
	timeout := int32(5)
	action.Spec.TimeoutSeconds = &timeout
	action.Spec.OnFailure = fleetv1.ActionFailureAlert
	action.Status.StartedAt = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	r, c := newActionReconciler(t, action, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = unconfirmed()

	reconcileAction(t, r, "t1")
	if held := getAction(t, c, "t1"); held.Status.Phase != fleetv1.ActionPhaseInProgress || held.Status.Message != timingOutMessage {
		t.Fatalf("phase=%s message=%q, want the timeout held awaiting a confirmed stop", held.Status.Phase, held.Status.Message)
	}
	ageLease(t, c, "t1", time.Hour)
	reconcileAction(t, r, "t1")
	if p := getAction(t, c, "t1").Status.Phase; p != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %s at lease death, want Failed", p)
	}
}

func TestDesiredReturning_StopUnconfirmed_HoldsThenNamesTheUnwitnessedStop(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := heldAction("t1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.DesiredStateReturning, lease)
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = unconfirmed()

	reconcileAction(t, r, "t1")
	if ft := getAction(t, c, "t1"); ft.Status.Phase == fleetv1.ActionPhasePaused ||
		!strings.Contains(ft.Status.Message, "awaiting confirmed recovery") {
		t.Fatalf("phase=%s message=%q, want the Returning recovery still awaited", ft.Status.Phase, ft.Status.Message)
	}
	ageLease(t, c, "t1", time.Hour)
	reconcileAction(t, r, "t1")
	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePaused || !strings.Contains(ft.Status.Message, "rest was not witnessed") {
		t.Fatalf("phase=%s message=%q, want Paused naming the unwitnessed stop", ft.Status.Phase, ft.Status.Message)
	}
}

// The gate itself: an acknowledged STOP_UNCONFIRMED is not a confirmed stop while the lease
// lives, and is one once it has provably expired, still saying it was unconfirmed.
func TestConfirmedStop_StopUnconfirmedIsNotProofWhileTheLeaseLives(t *testing.T) {
	cases := []struct {
		name      string
		cmd       *fakeCommander
		leaseIn   time.Duration
		confirmed bool
		disp      command.CancelDisposition
	}{
		{"unconfirmed, lease live", unconfirmed(), defaultLeaseDuration, false, command.CancelStopUnconfirmed},
		{"unconfirmed, lease dead", unconfirmed(), -time.Hour, true, command.CancelStopUnconfirmed},
		{"stopped safely, lease live", &fakeCommander{cancelAck: true}, defaultLeaseDuration, true, command.CancelStoppedSafely},
		{"not acknowledged, lease live", &fakeCommander{cancelAck: false}, defaultLeaseDuration, false, command.CancelStoppedSafely},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, &metav1.Time{Time: time.Now().Add(tc.leaseIn)})
			r, _ := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
			r.Commander = tc.cmd
			ok, disp := r.confirmedStopWithDisposition(context.Background(), a, "r1",
				unattributedCancelIntent("test", command.CancelCauseUnspecified))
			if ok != tc.confirmed || disp != tc.disp {
				t.Fatalf("confirmed=%v disp=%v, want %v %v", ok, disp, tc.confirmed, tc.disp)
			}
		})
	}
}
