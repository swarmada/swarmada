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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// A child that onFailure: Requeue will retry sits at Failed for its backoff. failurePolicy fires on
// that first Failed (RFC-0001 #fleettask-completion-failure-and-compensation), and the retry is
// then meant never to run, because FailFast, Compensate and a cancel of the task write Cancelled
// onto the child's spec.desiredState. The task counted the child terminal and wrote nothing to it,
// so once the backoff ended row A23 returned it to Pending and it was assigned again, under a task
// that was already Failed and is never reconciled again.

// retryingChild is a child of taskName that failed on a robot a second ago and will be retried:
// onFailure Requeue with retries left, inside the default 30 s backoff, its robot already released.
func retryingChild(taskName, member string) *fleetv1.FleetAction {
	a := childAction(taskName, member, fleetv1.ActionPhaseFailed)
	a.Spec.OnFailure = fleetv1.ActionFailureRequeue
	a.Spec.DesiredState = fleetv1.DesiredStateRunning
	a.Status.ScheduledAt = ago(time.Minute)
	a.Status.FailedAt = ago(time.Second)
	a.Status.AssignmentGeneration = 1
	return a
}

// executingChild is a member still executing on its robot.
func executingChild(taskName, member string) *fleetv1.FleetAction {
	a := childAction(taskName, member, fleetv1.ActionPhaseInProgress)
	a.Spec.DesiredState = fleetv1.DesiredStateRunning
	a.Status.AssignedRobot = "r1"
	return a
}

// FailFast writes Cancelled onto a member that failed and awaits its retry, in the pass that
// fails the task.
func TestFleetTask_FailFastCancelsAFailedMemberAwaitingRetry(t *testing.T) {
	ft := fleetTask("ft-rb", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, ""), ftAction("b", nil, ""))
	r, c := newFleetTaskReconciler(t, ft, retryingChild("ft-rb", "a"), executingChild("ft-rb", "b"))

	reconcileFT(t, r, "ft-rb")

	if got := getFT(t, c, "ft-rb").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("task phase = %q, want Failed (FailFast on a's first Failed)", got)
	}
	if got := getActionOrNil(t, c, childName("ft-rb", "a")).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
		t.Errorf("a failed and awaits its retry, and its desiredState is %q under a Failed task, not Cancelled: the retry runs", got)
	}
	if got := getActionOrNil(t, c, childName("ft-rb", "b")).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
		t.Errorf("b's desiredState = %q, want Cancelled", got)
	}
}

// Compensate writes Cancelled onto a member awaiting its retry in the pass that enters the saga, as
// it does onto a running member: the saga must not race a retry of the work it rolls back. b
// succeeded and declares a compensation, so the saga has an undo to run and stays Compensating.
func TestFleetTask_CompensateCancelsAFailedMemberAwaitingRetry(t *testing.T) {
	b := ftAction("b", nil, "")
	b.Compensation = &fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}
	ft := fleetTask("ft-rb", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyCompensate, fleetv1.DesiredStateRunning,
		ftAction("a", nil, ""), b)
	done := childAction("ft-rb", "b", fleetv1.ActionPhaseSucceeded)
	done.Spec.DesiredState = fleetv1.DesiredStateRunning
	r, c := newFleetTaskReconciler(t, ft, retryingChild("ft-rb", "a"), done)

	reconcileFT(t, r, "ft-rb")

	if got := getFT(t, c, "ft-rb").Status.Phase; got != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("task phase = %q, want Compensating", got)
	}
	if got := getActionOrNil(t, c, childName("ft-rb", "a")).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
		t.Errorf("a awaits its retry and its desiredState is %q as the task enters Compensating, not Cancelled", got)
	}
}

// An operator's cancel of the task reaches a member awaiting its retry: the task ends Cancelled
// once no member is non-terminal, and a member it never wrote would run again under it.
func TestFleetTask_CancelReachesAFailedMemberAwaitingRetry(t *testing.T) {
	ft := fleetTask("ft-rb", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateCancelled,
		ftAction("a", nil, ""), ftAction("b", nil, ""))
	r, c := newFleetTaskReconciler(t, ft, retryingChild("ft-rb", "a"), executingChild("ft-rb", "b"))

	reconcileFT(t, r, "ft-rb")

	if got := getActionOrNil(t, c, childName("ft-rb", "a")).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
		t.Errorf("the task is cancelled, and a, awaiting its retry, has desiredState %q, not Cancelled", got)
	}
	if got := getFT(t, c, "ft-rb").Status.Phase; isGroupTerminal(got) {
		t.Errorf("task phase = %q while b is still running, want non-terminal", got)
	}
}

// On the spec.flow path the task's own cancel reaches a member awaiting its retry too. The
// parallel node keeps the flow running while b runs, so the task's cancel is what must reach a.
func TestFlow_TaskCancelReachesAFailedMemberAwaitingRetry(t *testing.T) {
	task := flowTask("fl-rb", &fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{
		Branches: []fleetv1.FlowNode{runNode("a"), runNode("b")}}},
		fleetv1.FleetTaskAction{Name: "a", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}},
		fleetv1.FleetTaskAction{Name: "b", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}})
	task.Spec.DesiredState = fleetv1.DesiredStateCancelled
	r, c := newFleetTaskReconciler(t, task, retryingChild("fl-rb", "a"), executingChild("fl-rb", "b"))

	reconcileFT(t, r, "fl-rb")

	if got := getActionOrNil(t, c, iterationChildName("fl-rb", "a", 0)).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
		t.Errorf("the flow task is cancelled, and a, awaiting its retry, has desiredState %q, not Cancelled", got)
	}
	if got := getActionOrNil(t, c, iterationChildName("fl-rb", "b", 0)).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
		t.Errorf("b's desiredState = %q, want Cancelled", got)
	}
}

// A member that will not run again is settled, and the task leaves it as it is: only the retry a
// Failed child will take makes it take the write. Row A3's child is the one exception (its row
// below). x, failed for good, is what fires FailFast.
func TestFleetTask_NoWriteToASettledOrStopUnconfirmedMember(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(a *fleetv1.FleetAction)
	}{
		{"onFailure Alert", func(a *fleetv1.FleetAction) { a.Spec.OnFailure = fleetv1.ActionFailureAlert }},
		{"onFailure Abandon", func(a *fleetv1.FleetAction) { a.Spec.OnFailure = fleetv1.ActionFailureAbandon }},
		{"retryPolicy spent", func(a *fleetv1.FleetAction) {
			a.Spec.RetryPolicy = &fleetv1.ActionRetryPolicy{MaxRetries: 2, BackoffSeconds: 30}
			a.Status.RetryCount = 2
		}},
		{"default retries spent", func(a *fleetv1.FleetAction) { a.Status.RetryCount = defaultMaxRetries }},
		{"never assigned", func(a *fleetv1.FleetAction) { a.Status.ScheduledAt = nil }},
		{"Succeeded", func(a *fleetv1.FleetAction) { a.Status.Phase = fleetv1.ActionPhaseSucceeded }},
		{"Cancelled", func(a *fleetv1.FleetAction) { a.Status.Phase = fleetv1.ActionPhaseCancelled }},
		// Row A3's child awaits its retry too, but it keeps its robot until A3 releases it, and a
		// stop on that robot holds it (row A6). The task's cancel is not held under a stop at v0.4
		// (row A4's note), so the task leaves this child as it is rather than end held work.
		{"StopUnconfirmed (row A3)", func(a *fleetv1.FleetAction) {
			a.Status.FailureReason = reasonStopUnconfirmed
			a.Status.AssignedRobot = "r1"
			a.Status.LeaseExpiresAt = soon(time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ft := fleetTask("ft-rb", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
				ftAction("a", nil, ""), ftAction("b", nil, ""), ftAction("x", nil, ""))
			a := retryingChild("ft-rb", "a")
			tc.edit(a)
			x := retryingChild("ft-rb", "x")
			x.Spec.OnFailure = fleetv1.ActionFailureAbandon
			r, c := newFleetTaskReconciler(t, ft, a, executingChild("ft-rb", "b"), x)

			reconcileFT(t, r, "ft-rb")

			if got := getFT(t, c, "ft-rb").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
				t.Fatalf("task phase = %q, want Failed", got)
			}
			if got := getActionOrNil(t, c, childName("ft-rb", "a")).Spec.DesiredState; got != fleetv1.DesiredStateRunning {
				t.Errorf("a is settled or held by row A3, and its desiredState was rewritten to %q", got)
			}
			if got := getActionOrNil(t, c, childName("ft-rb", "x")).Spec.DesiredState; got != fleetv1.DesiredStateRunning {
				t.Errorf("x will not run again and its desiredState was rewritten to %q", got)
			}
			if got := getActionOrNil(t, c, childName("ft-rb", "b")).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
				t.Errorf("b's desiredState = %q, want Cancelled", got)
			}
		})
	}
}

// The whole of the defect, across both controllers: FailFast fails the task on a's first Failed,
// a's backoff then ends, and a is not assigned again. It ends Cancelled, and the idle robot stays
// free.
func TestFleetTask_FailFastStopsTheRetryOfAFailedMember(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	ft := fleetTask("ft-rb", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, ""), ftAction("b", nil, ""))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(readyActionAdapter(), ft, retryingChild("ft-rb", "a"), executingChild("ft-rb", "b"),
			robotInPhase("r2", fleetv1.RobotPhaseIdle, "")).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}, &fleetv1.Robot{}).
		Build()
	tasks := &FleetTaskReconciler{Client: c, Scheme: scheme}
	actions := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler()}
	a := childName("ft-rb", "a")

	reconcileFT(t, tasks, "ft-rb")
	if got := getFT(t, c, "ft-rb").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("task phase = %q, want Failed", got)
	}
	if got := getActionOrNil(t, c, a).Spec.DesiredState; got != fleetv1.DesiredStateCancelled {
		t.Errorf("a's desiredState = %q after FailFast, want Cancelled", got)
	}

	// The backoff ends.
	child := getActionOrNil(t, c, a)
	child.Status.FailedAt = ago(time.Minute)
	if err := c.Status().Update(context.Background(), child); err != nil {
		t.Fatalf("end the backoff: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := actions.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: actionNS, Name: a}}); err != nil {
			t.Fatalf("reconcile a #%d: %v", i, err)
		}
		got := getActionOrNil(t, c, a)
		if got.Status.Phase == fleetv1.ActionPhaseAssigned || got.Status.AssignedRobot != "" {
			t.Fatalf("a's retry ran under a Failed task: phase %s, robot %q (reconcile #%d)",
				got.Status.Phase, got.Status.AssignedRobot, i)
		}
	}
	if got := getActionOrNil(t, c, a).Status.Phase; got != fleetv1.ActionPhaseCancelled {
		t.Errorf("a's phase = %q after its backoff, want Cancelled", got)
	}
	if got := getRobot(t, c, "r2", actionNS).Status.AssignedAction; got != "" {
		t.Errorf("the idle robot took %q", got)
	}
	if got := getFT(t, c, "ft-rb").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Errorf("task phase = %q, want it still Failed", got)
	}
}

// awaitsRetry is the composite's reading of row A23, and the child's own: a Failed action it
// reports is the one the FleetAction controller returns to Pending once the backoff is spent, and
// no other.
func TestAwaitsRetry_AgreesWithTheRetryTheActionTakes(t *testing.T) {
	two := &fleetv1.ActionRetryPolicy{MaxRetries: 2, BackoffSeconds: 0}
	for _, tc := range []struct {
		name string
		edit func(a *fleetv1.FleetAction)
	}{
		{"Requeue with retries left", func(*fleetv1.FleetAction) {}},
		{"onFailure unset", func(a *fleetv1.FleetAction) { a.Spec.OnFailure = "" }},
		{"retryPolicy with one left", func(a *fleetv1.FleetAction) { a.Spec.RetryPolicy = two; a.Status.RetryCount = 1 }},
		{"retryPolicy spent", func(a *fleetv1.FleetAction) { a.Spec.RetryPolicy = two; a.Status.RetryCount = 2 }},
		{"default retries spent", func(a *fleetv1.FleetAction) { a.Status.RetryCount = defaultMaxRetries }},
		{"onFailure Alert", func(a *fleetv1.FleetAction) { a.Spec.OnFailure = fleetv1.ActionFailureAlert }},
		{"onFailure Abandon", func(a *fleetv1.FleetAction) { a.Spec.OnFailure = fleetv1.ActionFailureAbandon }},
		{"never assigned", func(a *fleetv1.FleetAction) { a.Status.ScheduledAt = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := failedAction("t1", "", 0, fleetv1.ActionFailureRequeue, ago(time.Minute), ago(time.Minute))
			tc.edit(a)
			want := awaitsRetry(a)
			r, c := newActionReconciler(t, a)

			reconcileAction(t, r, "t1")

			if retried := getAction(t, c, "t1").Status.Phase == fleetv1.ActionPhasePending; retried != want {
				t.Errorf("awaitsRetry = %v, and the action was retried: %v", want, retried)
			}
		})
	}
	if awaitsRetry(&fleetv1.FleetAction{Status: fleetv1.FleetActionStatus{
		Phase: fleetv1.ActionPhaseInProgress, ScheduledAt: &metav1.Time{}}}) {
		t.Errorf("awaitsRetry reports an action that has not failed")
	}
}
