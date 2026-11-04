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

// spec.timeoutSeconds is measured from status.startedAt, the instant the robot reported
// execution began (RFC-0001 #fleetaction-state-machine). status.startTime is superseded
// and written by no controller, so a deadline read from it never arrives.
//
// Two transitions depend on it:
//   - InProgress → Failed once the window has elapsed, after the robot's stop is
//     confirmed (the adapter acknowledges cancel_action, or the lease is provably dead);
//   - Revoking → Cancelled under onDisconnect=WhenActionExpired
//     (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// THE DONE-WHEN. A real API server, a real dispatch, a real RUNNING report through the
// ingestor, and a 5 s window. The action must be Failed on the first reconcile after the
// window closes, with its robot released.
func TestEnvtest_TimeoutSecondsFailsAnInProgressActionAfterItsWindow(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}
	f12SeedAdapter(t, ns)
	f12SeedRobot(t, ns, "r1", "z")

	timeout := int32(5)
	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "timed", Namespace: ns},
		Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z",
			Priority: fleetv1.ActionPriorityNormal, TimeoutSeconds: &timeout,
			OnFailure: fleetv1.ActionFailureAlert},
	}
	if err := envK8s.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "timed"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := zrGetAction(t, req); got.Status.Phase != fleetv1.ActionPhaseAssigned || got.Status.AssignedRobot != "r1" {
		t.Fatalf("phase=%s robot=%q, want Assigned to r1", got.Status.Phase, got.Status.AssignedRobot)
	}

	ing := &ActionStatusIngestor{Client: envK8s}
	token := uint64(zrGetAction(t, req).Status.AssignmentGeneration)
	if err := ing.IngestActionStatus(ctx, ns, allowAnyRobot, &fav1.ActionStatusUpdate{
		ActionId: "timed", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: &token,
	}); err != nil {
		t.Fatalf("ingest RUNNING: %v", err)
	}
	running := zrGetAction(t, req)
	if running.Status.Phase != fleetv1.ActionPhaseInProgress || running.Status.StartedAt == nil {
		t.Fatalf("phase=%s startedAt=%v, want InProgress with startedAt", running.Status.Phase, running.Status.StartedAt)
	}
	if running.Status.StartTime != nil {
		t.Fatalf("status.startTime = %v; it is superseded and nothing writes it", running.Status.StartTime)
	}
	started := running.Status.StartedAt.Time

	// Inside the window: renewed, not failed, and asked back no later than the deadline.
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile inside window: %v", err)
	}
	if p := zrGetAction(t, req).Status.Phase; p != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s inside the window, want InProgress", p)
	}

	time.Sleep(time.Until(started.Add(5*time.Second + 100*time.Millisecond)))
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile after window: %v", err)
	}
	got := zrGetAction(t, req)
	t.Logf("phase %s at %s after status.startedAt; failureReason=%q",
		got.Status.Phase, time.Since(started).Round(100*time.Millisecond), got.Status.FailureReason)
	if got.Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %s %s after startedAt with timeoutSeconds=5, want Failed", got.Status.Phase,
			time.Since(started).Round(100*time.Millisecond))
	}
	if !strings.Contains(got.Status.FailureReason, "timeoutSeconds") || got.Status.FailedAt == nil {
		t.Fatalf("failureReason=%q failedAt=%v, want the timeout named and failedAt set",
			got.Status.FailureReason, got.Status.FailedAt)
	}
	if n := cmd.cancelsFor("timed", "r1"); n != 1 {
		t.Fatalf("cancel_action to r1 pushed %d time(s), want 1 (the stop is confirmed before Failed)", n)
	}
	// A live manager is asked back by the deadline, not only at the next lease renewal.
	if res.RequeueAfter <= 0 || res.RequeueAfter > 5*time.Second {
		t.Fatalf("requeue inside the window = %s, want at most the 5s window", res.RequeueAfter)
	}
	var r1 fleetv1.Robot
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "r1"}, &r1); err != nil {
		t.Fatalf("get r1: %v", err)
	}
	if r1.Status.AssignedAction != "" || r1.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("r1 assignedAction=%q phase=%s, want released to Idle", r1.Status.AssignedAction, r1.Status.Phase)
	}
}

// No confirmation from the adapter: the action holds with its robot bound and its lease
// NOT renewed, and fails once that lease is provably dead.
func TestTimeout_UnconfirmedStopHoldsUntilTheLeaseIsProvablyDead(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(20 * time.Second).Truncate(time.Second)}
	action := assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 1, lease)
	timeout := int32(5)
	action.Spec.TimeoutSeconds = &timeout
	action.Spec.OnFailure = fleetv1.ActionFailureAlert
	action.Status.StartedAt = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	r, c := newActionReconciler(t, action, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = &fakeCommander{cancelErr: errors.New("adapter unreachable")}

	reconcileAction(t, r, "t1")
	held := getAction(t, c, "t1")
	if held.Status.Phase != fleetv1.ActionPhaseInProgress || held.Status.AssignedRobot != "r1" {
		t.Fatalf("phase=%s robot=%q with no confirmation, want InProgress on r1", held.Status.Phase, held.Status.AssignedRobot)
	}
	if held.Status.Message != timingOutMessage {
		t.Fatalf("message = %q, want %q", held.Status.Message, timingOutMessage)
	}
	if !held.Status.LeaseExpiresAt.Equal(lease) {
		t.Fatalf("lease moved from %s to %s; a timed-out action must not be renewed", lease, held.Status.LeaseExpiresAt)
	}

	base := held.DeepCopy()
	held.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	if err := c.Status().Patch(context.Background(), held, client.MergeFrom(base)); err != nil {
		t.Fatalf("age lease: %v", err)
	}
	reconcileAction(t, r, "t1")
	if p := getAction(t, c, "t1").Status.Phase; p != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %s once the lease is provably dead, want Failed", p)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "" {
		t.Fatalf("r1 still records %q after the timeout failure", rob.Status.AssignedAction)
	}
}

// The disconnect-cancellation path reads the same instant. A Revoking action under
// onDisconnect=WhenActionExpired whose window, measured from startedAt, has passed is
// Cancelled; one carrying only the superseded startTime cannot expire.
func TestTimeout_WhenActionExpiredMeasuresFromStartedAt(t *testing.T) {
	deadLease := &metav1.Time{Time: time.Now().Add(-time.Minute)}
	window := int32(60)

	t.Run("startedAt past the window cancels", func(t *testing.T) {
		action := assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, deadLease)
		action.Spec.TimeoutSeconds = &window
		action.Status.StartedAt = &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}
		r, c := newActionReconciler(t, action, robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
			actionCancelConfig(fleetv1.ActionCancellationWhenActionExpired, 0))
		reconcileAction(t, r, "t1")
		if p := getAction(t, c, "t1").Status.Phase; p != fleetv1.ActionPhaseCancelled {
			t.Fatalf("phase = %s, want Cancelled: startedAt is 10m past a 60s window", p)
		}
	})

	t.Run("only the superseded startTime set holds", func(t *testing.T) {
		action := assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, deadLease)
		action.Spec.TimeoutSeconds = &window
		action.Status.StartTime = &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}
		r, c := newActionReconciler(t, action, robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
			actionCancelConfig(fleetv1.ActionCancellationWhenActionExpired, 0))
		reconcileAction(t, r, "t1")
		if p := getAction(t, c, "t1").Status.Phase; p != fleetv1.ActionPhaseRevoking {
			t.Fatalf("phase = %s, want Revoking held: startTime is not read", p)
		}
	})
}
