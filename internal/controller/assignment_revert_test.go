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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/metrics"
)

// Row A24 (RFC-0001 #safety-action-table): an assignment the robot never took up returns to
// Pending. These tests cover its second trigger, the command dispatcher's check at the send
// refusing it (assignment_revert.go), and the revert both triggers now share.

// hookCommander runs hook after the commit, while the AssignAction is "in flight", then answers
// with err, a refusal (reject), or an acceptance.
type hookCommander struct {
	fakeCommander
	hook   func(ctx context.Context) error
	err    error
	reject bool
	epoch  uint64 // RegisterEpoch's answer for every robot
}

func (f *hookCommander) RegisterEpoch(string, string) uint64 { return f.epoch }

func (f *hookCommander) PushAssignAction(
	ctx context.Context, _, robotID string, a command.AssignAction,
) (command.AssignActionOutcome, error) {
	f.assigns = append(f.assigns, a)
	f.assignTo = append(f.assignTo, robotID)
	if f.hook != nil {
		if err := f.hook(ctx); err != nil {
			return command.AssignActionOutcome{}, err
		}
	}
	if f.err != nil {
		return command.AssignActionOutcome{}, f.err
	}
	if f.reject {
		return command.AssignActionOutcome{Accepted: false, Rejection: "ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE"}, nil
	}
	return command.AssignActionOutcome{Accepted: true, AcceptedFencingToken: a.FencingToken}, nil
}

// notSentErrors are the refusals at the send A24 reverts on, as the dispatcher returns them.
func notSentErrors() []error {
	return []error{
		command.ErrNotRegistered,
		fmt.Errorf("%w: the stream negotiated 1.3.0", command.ErrBelowLatchContract),
	}
}

// notSentMessage is the message the revert writes for sendErr.
func notSentMessage(sendErr error) string {
	return fmt.Sprintf("the assignment was not sent (%s); rescheduling", notSentReason(sendErr))
}

// waitingAction is pendingAction with the time its wait began.
func waitingAction(name string, since time.Time) *fleetv1.FleetAction {
	a := pendingAction(name)
	t := metav1.NewTime(since).Rfc3339Copy()
	a.Status.PendingSince = &t
	return a
}

func latencySamples(t *testing.T, priority fleetv1.ActionPriority) uint64 {
	t.Helper()
	m := &dto.Metric{}
	obs := metrics.SchedulerAssignmentLatencySeconds.WithLabelValues(actionNS, priorityLabel(priority))
	if err := obs.(prometheus.Metric).Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// A refusal at the send reverts the commit: the action is Pending and unbound, its generation
// kept and its retry count unchanged, pendingSince written again (v0.4: on each row whose target
// phase is Pending); the robot is Idle and records nothing; the pass requeues; the Event is emitted
// once over two passes; and the assignment latency is observed once, at the commit (the Assigned
// transition happened).
func TestA24_NotSent_RevertsToPending(t *testing.T) {
	for _, sendErr := range notSentErrors() {
		t.Run(sendErr.Error(), func(t *testing.T) {
			since := time.Now().Add(-time.Minute)
			r, c := newActionReconciler(t, waitingAction("t1", since), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
			fc := &hookCommander{err: sendErr}
			r.Commander = fc
			rec := &objectRecorder{}
			r.Recorder = rec
			clock := time.Now()
			r.holdOffs.clock = func() time.Time { return clock }
			before := latencySamples(t, "")

			res := reconcileAction(t, r, "t1")

			a := getAction(t, c, "t1")
			if a.Status.Phase != fleetv1.ActionPhasePending || a.Status.AssignedRobot != "" || a.Status.LeaseExpiresAt != nil {
				t.Fatalf("INVARIANT (A24): an assignment not sent is %s on %q (lease %v); want Pending, unbound, no lease",
					a.Status.Phase, a.Status.AssignedRobot, a.Status.LeaseExpiresAt)
			}
			gen1 := a.Status.AssignmentGeneration
			if gen1 < 1 {
				t.Fatalf("assignmentGeneration = %d, want the committed one kept", gen1)
			}
			if a.Status.RetryCount != 0 {
				t.Errorf("retryCount = %d, want 0: a revert is not a failure", a.Status.RetryCount)
			}
			if !strings.Contains(a.Status.Message, "was not sent") || !strings.Contains(a.Status.Message, notSentReason(sendErr)) {
				t.Errorf("message %q does not say the assignment was not sent and why", a.Status.Message)
			}
			if a.Status.PendingSince == nil || !a.Status.PendingSince.After(metav1.NewTime(since).Rfc3339Copy().Time) {
				t.Errorf("pendingSince = %v, want written again at the revert, after %v", a.Status.PendingSince, since)
			}
			if rb := getRobot(t, c, "r1", actionNS); rb.Status.AssignedAction != "" || rb.Status.Phase != fleetv1.RobotPhaseIdle {
				t.Fatalf("robot r1 records %q in phase %s; want released to Idle", rb.Status.AssignedAction, rb.Status.Phase)
			}
			if res.RequeueAfter <= 0 {
				t.Error("a reverted assignment should requeue to reschedule")
			}
			if got := latencySamples(t, ""); got != before+1 {
				t.Errorf("assignment latency observed %d times; want 1, at the commit", got-before)
			}

			// The next pass, right away, offers r1 nothing: the hold-off keeps it off t1, and the
			// revert's message stands.
			reconcileAction(t, r, "t1")
			if a = getAction(t, c, "t1"); len(fc.assigns) != 1 || a.Status.AssignmentGeneration != gen1 ||
				a.Status.Message != notSentMessage(sendErr) {
				t.Fatalf("pass inside the hold-off: %d pushes, generation %d, message %q; want 1, %d, the revert's",
					len(fc.assigns), a.Status.AssignmentGeneration, a.Status.Message, gen1)
			}
			// Once the hold-off is over the next pass commits again at a higher generation, is
			// refused again, and reverts again; AssignmentNotSent is not repeated for the same reason.
			clock = clock.Add(holdOffFloor + time.Second)
			reconcileAction(t, r, "t1")
			a = getAction(t, c, "t1")
			if len(fc.assigns) != 2 || a.Status.Phase != fleetv1.ActionPhasePending || a.Status.AssignmentGeneration <= gen1 {
				t.Fatalf("second pass: %d pushes, %s at generation %d; want 2 pushes, Pending, generation > %d",
					len(fc.assigns), a.Status.Phase, a.Status.AssignmentGeneration, gen1)
			}
			if n := len(rec.on("FleetAction", "t1", EventAssignmentNotSent)); n != 1 {
				t.Errorf("%d AssignmentNotSent Events over two reverts; want 1", n)
			}
			if n := len(rec.on("FleetAction", "t1", EventRobotHeldOff)); n != 2 {
				t.Errorf("%d RobotHeldOff Events over two reverts; want 2", n)
			}
		})
	}
}

// An assignment that stands observes its latency once, and is sent; the not-sent memo is
// forgotten, so a later refusal is reported again.
func TestA24_Accepted_ObservesLatencyAndForgetsTheEvent(t *testing.T) {
	r, c := newActionReconciler(t, waitingAction("t1", time.Now().Add(-time.Minute)),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = &hookCommander{}
	key := types.NamespacedName{Namespace: actionNS, Name: "t1"}
	r.notSent.first(key, "r1", notSentReason(command.ErrNotRegistered))
	before := latencySamples(t, "")

	reconcileAction(t, r, "t1")

	if a := getAction(t, c, "t1"); a.Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatalf("accepted assignment is %s, want Assigned", a.Status.Phase)
	}
	if got := latencySamples(t, ""); got != before+1 {
		t.Errorf("assignment latency observed %d times for an assignment that stands; want 1", got-before)
	}
	if !r.notSent.first(key, "r1", notSentReason(command.ErrNotRegistered)) {
		t.Error("the not-sent memo was not forgotten when the assignment was sent")
	}
}

// A stop issued before the revert holds the action on its robot, as for a refusal (A1): nothing
// is released, on the robot's own stop, on a zone's and on the namespace's.
func TestA24_NotSent_UnderAStopHolds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue func(c client.Client) func(context.Context) error
	}{
		{"robot-recorded", func(c client.Client) func(context.Context) error { return estopStateOn(c, fleetv1.RobotEstopStopping) }},
		{"robot-resuming", func(c client.Client) func(context.Context) error { return estopStateOn(c, fleetv1.RobotEstopResuming) }},
		{"zone", func(c client.Client) func(context.Context) error { return triggerOn(c, &fleetv1.FleetZone{}, "z") }},
		{"namespace", func(c client.Client) func(context.Context) error {
			return triggerOn(c, &fleetv1.SwarmadaConfig{}, SwarmadaConfigName)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, _ := refusalRig(t)
			fc := &hookCommander{err: command.ErrNotRegistered, hook: tc.issue(c)}
			r.Commander = fc
			reconcileAction(t, r, "t1")
			a := getAction(t, c, "t1")
			if a.Status.Phase != fleetv1.ActionPhasePaused || a.Status.AssignedRobot != "r1" {
				t.Fatalf("INVARIANT (A1): an assignment not sent under a %s stop is %s on %q (%q); want Paused on r1",
					tc.name, a.Status.Phase, a.Status.AssignedRobot, a.Status.Message)
			}
			if rb := refusalRobot(t, c); rb.Status.AssignedAction != "t1" {
				t.Fatalf("robot r1 records %q, want t1: nothing is released under a stop", rb.Status.AssignedAction)
			}
		})
	}
}

// stopAfterRead wraps c so that the first read of robot r1 after armed returns the robot as it
// was, then records the robot's own stop at the API server: a stop written between the read the
// guard judges and the write that read's resourceVersion goes under.
func stopAfterRead(t *testing.T, c client.Client, armed *bool) client.Client {
	t.Helper()
	fired := false
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, isRobot := obj.(*fleetv1.Robot); !isRobot || key.Name != "r1" || !*armed || fired {
				return nil
			}
			fired = true
			cur := &fleetv1.Robot{}
			if err := cl.Get(ctx, key, cur); err != nil {
				return err
			}
			base := cur.DeepCopy()
			cur.Status.EstopState = fleetv1.RobotEstopStopping
			return cl.Status().Patch(ctx, cur, client.MergeFrom(base))
		},
	})
}

// A stop recorded on the robot after the revert's read and before its write fails the write
// (the resourceVersion moved), and the next attempt reads the stop and holds: on the not-sent
// path and on the refusal path.
func TestA24_AStopBetweenTheReadAndTheWriteHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		fc   func() *hookCommander
	}{
		{"not-sent", func() *hookCommander { return &hookCommander{err: command.ErrNotRegistered} }},
		{"refused", func() *hookCommander { return &hookCommander{reject: true} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newActionReconciler(t, pendingAction("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
			armed := false
			r.Client = stopAfterRead(t, c, &armed)
			fc := tc.fc()
			fc.hook = func(context.Context) error { armed = true; return nil }
			r.Commander = fc

			reconcileAction(t, r, "t1")

			a := getAction(t, c, "t1")
			if a.Status.Phase != fleetv1.ActionPhasePaused || a.Status.AssignedRobot != "r1" {
				t.Fatalf("INVARIANT (A1): a stop recorded during the revert left the action %s on %q (%q); want Paused on r1",
					a.Status.Phase, a.Status.AssignedRobot, a.Status.Message)
			}
			rb := getRobot(t, c, "r1", actionNS)
			if rb.Status.AssignedAction != "t1" {
				t.Fatalf("robot r1 records %q under a stop, want t1: the revert released it on a read older than the stop",
					rb.Status.AssignedAction)
			}
			if rb.Status.EstopState != fleetv1.RobotEstopStopping {
				t.Fatalf("robot r1 estopState %q: the test's stop was not recorded", rb.Status.EstopState)
			}
		})
	}
}

// The revert reads the robot at the API server: with a cache that never saw the commit, the
// robot is still freed there.
func TestA24_NotSent_FreesTheRobotAtTheAPIServer(t *testing.T) {
	s := newStaleCache(t, []client.Object{zoneObj("yard", ""), freeRobotAt("r1", "", "yard"), pendingTo("a1", "")})
	s.r.Commander = &hookCommander{err: command.ErrNotRegistered}

	s.reconcile(t, "a1")

	if a := s.action(t, "a1"); a.Status.Phase != fleetv1.ActionPhasePending || a.Status.AssignedRobot != "" {
		t.Fatalf("action %s on %q at the API server; want Pending, unbound", a.Status.Phase, a.Status.AssignedRobot)
	}
	if rb := getRobot(t, s.api, "r1", actionNS); rb.Status.AssignedAction != "" || rb.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot r1 at the API server records %q in phase %s; want released to Idle: the revert read the cache",
			rb.Status.AssignedAction, rb.Status.Phase)
	}
}

func preemptionRig(t *testing.T) (*FleetActionReconciler, client.Client) {
	t.Helper()
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	return newActionReconciler(t,
		criticalPending("crit"),
		bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"),
	)
}

// A preemptor whose assignment was not sent never reached the robot, which is still running the
// victim: the robot's record goes back to the victim, InProgress, and the victim stays Preempted
// (it leaves through row A12; no row takes it back).
func TestA24_NotSent_RestoresTheRobotToItsVictim(t *testing.T) {
	r, c := preemptionRig(t)
	r.Commander = &hookCommander{err: command.ErrNotRegistered}

	reconcileAction(t, r, "crit")

	if a := getAction(t, c, "crit"); a.Status.Phase != fleetv1.ActionPhasePending || a.Status.AssignedRobot != "" {
		t.Fatalf("preemptor %s on %q; want Pending, unbound", a.Status.Phase, a.Status.AssignedRobot)
	}
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted || v.Status.AssignedRobot != "r1" {
		t.Fatalf("victim %s on %q; want Preempted, still bound to r1", v.Status.Phase, v.Status.AssignedRobot)
	}
	if rb := getRobot(t, c, "r1", actionNS); rb.Status.AssignedAction != "norm" || rb.Status.Phase != fleetv1.RobotPhaseInProgress {
		t.Fatalf("robot r1 records %q in phase %s; want the victim norm, InProgress", rb.Status.AssignedAction, rb.Status.Phase)
	}
}

// A victim released since (row A12) is not restored: the robot goes Idle and records nothing.
func TestA24_NotSent_AReleasedVictimIsNotRestored(t *testing.T) {
	r, c := preemptionRig(t)
	r.Commander = &hookCommander{err: command.ErrNotRegistered, hook: func(ctx context.Context) error {
		v := &fleetv1.FleetAction{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: "norm"}, v); err != nil {
			return err
		}
		base := v.DeepCopy()
		v.Status.Phase = fleetv1.ActionPhasePending
		v.Status.AssignedRobot = ""
		return c.Status().Patch(ctx, v, client.MergeFrom(base))
	}}

	reconcileAction(t, r, "crit")

	if rb := getRobot(t, c, "r1", actionNS); rb.Status.AssignedAction != "" || rb.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot r1 records %q in phase %s; want Idle and nothing: its victim was released",
			rb.Status.AssignedAction, rb.Status.Phase)
	}
}

// The refusals at the send A24 does not revert on stand, as before.
func TestA24_OtherPushErrorsStand(t *testing.T) {
	for _, sendErr := range []error{command.ErrHeldByStop, command.ErrUnreachable,
		fmt.Errorf("%w: no stream", command.ErrUnreachable)} {
		if notSentReason(sendErr) != "" {
			t.Errorf("notSentReason(%v) = %q; want \"\": it must not revert", sendErr, notSentReason(sendErr))
		}
	}
	for _, sendErr := range notSentErrors() {
		if notSentReason(sendErr) == "" {
			t.Errorf("notSentReason(%v) is empty; want a reason", sendErr)
		}
	}
}
