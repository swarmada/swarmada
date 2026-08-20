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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// cancelAction builds a bound action carrying the cancel-requested annotation.
func cancelAction(name, robot string, phase fleetv1.ActionPhase, gen int64, lease *metav1.Time, reason string) *fleetv1.FleetAction {
	ft := assignedAction(name, robot, phase, gen, lease)
	ft.Annotations = map[string]string{annCancelRequested: reason}
	return ft
}

// Adapter acknowledges the cancel → the action is finalized Cancelled and the robot
// is freed (Idle, unbound). This is a confirmed stop.
func TestCancel_AdapterAckFinalizesAndFreesRobot(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		cancelAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, live, "maintenance"),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = &fakeCommander{cancelAck: true}

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %s, want Cancelled on adapter ack", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "" || ft.Status.LeaseExpiresAt != nil {
		t.Fatalf("binding/lease not cleared: robot=%q lease=%v", ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt)
	}
	if ft.Status.Message != "cancelled: maintenance" {
		t.Errorf("message = %q", ft.Status.Message)
	}
	rob := getRobot(t, c, "r1", actionNS)
	if rob.Status.AssignedAction != "" || rob.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot not freed: assignedAction=%q phase=%s", rob.Status.AssignedAction, rob.Status.Phase)
	}
}

// Unreachable adapter with a LIVE lease → HOLD: the robot may still be executing,
// so the action is not finalized and the robot stays bound (single-executor safety).
func TestCancel_UnreachableWithLiveLeaseHolds(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		cancelAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, live, "true"),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = &fakeCommander{cancelErr: command.ErrUnreachable}

	res := reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase == fleetv1.ActionPhaseCancelled {
		t.Fatal("cancelled while the robot may still be executing (lease alive) — double-execution hazard")
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Fatalf("robot freed before provable stop: %q", ft.Status.AssignedRobot)
	}
	if ft.Status.Message != cancellingMessage {
		t.Errorf("message = %q, want cancelling", ft.Status.Message)
	}
	if res.RequeueAfter <= 0 {
		t.Error("a held cancel should requeue")
	}
	if getRobot(t, c, "r1", actionNS).Status.AssignedAction != "t1" {
		t.Error("robot binding cleared while held")
	}
}

// Unreachable adapter but the lease is PROVABLY DEAD → the robot has self-stopped,
// so the cancel is finalized.
func TestCancel_UnreachableWithDeadLeaseFinalizes(t *testing.T) {
	dead := &metav1.Time{Time: time.Now().Add(-time.Minute)}
	r, c := newActionReconciler(t,
		cancelAction("t1", "r1", fleetv1.ActionPhaseRevoking, 4, dead, "true"),
		robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
	)
	r.Commander = &fakeCommander{cancelErr: command.ErrUnreachable}

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatal("cancel not finalized after provable lease death")
	}
}

// A cancel on an unbound action (Pending, no robot) finalizes immediately.
func TestCancel_UnboundFinalizesImmediately(t *testing.T) {
	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS,
			Annotations: map[string]string{annCancelRequested: "true"}},
		Spec:   fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	r, c := newActionReconciler(t, pending)

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatal("an unbound Pending task should cancel immediately")
	}
}

// With no Commander (ControlStream disabled), a bound action with a live lease is
// held (cannot confirm the stop), and finalizes only once the lease is dead.
func TestCancel_NilCommanderHoldsThenFinalizesOnLeaseDeath(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		cancelAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, live, "true"),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	) // Commander nil

	reconcileAction(t, r, "t1")
	if getAction(t, c, "t1").Status.Phase == fleetv1.ActionPhaseCancelled {
		t.Fatal("no-wire cancel must not finalize while the lease is alive")
	}

	// Expire the lease and re-reconcile → now finalizes.
	held := getAction(t, c, "t1")
	held.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	if err := r.Status().Update(context.Background(), held); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "t1")
	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatal("no-wire cancel should finalize once the lease is provably dead")
	}
}

// ── Declarative cancel via spec.desiredState (Round-4 D3) ────────────────────
//
// spec.desiredState was read by nothing, so FleetTask's FailFast/Compensate fan-out —
// which cancels a child by writing Cancelled onto that field and doing nothing else —
// could not actually stop a child. D3 routes it into the SAME confirmed-cancel path as
// the operator annotation. These tests pin that it carries the identical single-executor
// guarantee and that it cannot fight the annotation path.

// desiredAction builds a bound action carrying the declarative cancel intent instead of
// the operator annotation.
func desiredCancelAction(name, robot string, phase fleetv1.ActionPhase, gen int64, lease *metav1.Time) *fleetv1.FleetAction {
	ft := assignedAction(name, robot, phase, gen, lease)
	ft.Spec.DesiredState = fleetv1.DesiredStateCancelled
	return ft
}

func TestDesiredStateCancel_UnboundFinalizesImmediately(t *testing.T) {
	ft := desiredCancelAction("t1", "", fleetv1.ActionPhasePending, 0, nil)
	ft.Status.AssignedRobot = ""
	r, c := newActionReconciler(t, ft)

	reconcileAction(t, r, "t1")

	got := getAction(t, c, "t1")
	if got.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %s, want Cancelled — an unread desiredState leaves the composite unable to stop children",
			got.Status.Phase)
	}
}

func TestDesiredStateCancel_LiveLeaseHoldsRobot(t *testing.T) {
	// The single-executor guarantee must be identical to the annotation path: a
	// declarative cancel NEVER frees a robot that might still be executing.
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		desiredCancelAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, live),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = &fakeCommander{cancelAck: false} // adapter does not confirm

	reconcileAction(t, r, "t1")

	got := getAction(t, c, "t1")
	if got.Status.Phase == fleetv1.ActionPhaseCancelled {
		t.Fatal("a declarative cancel finalized while the lease was live — double-execution risk")
	}
	if got.Status.AssignedRobot != "r1" {
		t.Errorf("robot released on an unconfirmed stop: %q", got.Status.AssignedRobot)
	}
	rob := getRobot(t, c, "r1", actionNS)
	if rob.Status.AssignedAction != "t1" {
		t.Errorf("robot unbound on an unconfirmed stop: assignedAction=%q", rob.Status.AssignedAction)
	}
}

func TestDesiredStateCancel_AnnotationWinsAndTheyDoNotFight(t *testing.T) {
	// Both intents set at once. The annotation is checked first so an explicit operator
	// cancel keeps its reason; both converge on the same finalizer, so the outcome is one
	// Cancelled action either way. The risk this pins is a LOOP — two paths alternately
	// re-driving the same action — not a wrong phase.
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	ft := cancelAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, live, "maintenance")
	ft.Spec.DesiredState = fleetv1.DesiredStateCancelled
	r, c := newActionReconciler(t, ft, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = &fakeCommander{cancelAck: true}

	reconcileAction(t, r, "t1")

	got := getAction(t, c, "t1")
	if got.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %s, want Cancelled", got.Status.Phase)
	}
	if got.Status.Message != "cancelled: maintenance" {
		t.Errorf("message = %q, want the operator's reason to win over desiredState=Cancelled",
			got.Status.Message)
	}
	// Terminal is terminal: re-reconciling must not re-enter either cancel path.
	rv := got.ResourceVersion
	reconcileAction(t, r, "t1")
	again := getAction(t, c, "t1")
	if again.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Errorf("phase moved off Cancelled on re-reconcile: %s", again.Status.Phase)
	}
	if again.ResourceVersion != rv && again.Status.Message != got.Status.Message {
		t.Errorf("the two cancel paths re-drove a terminal action: %q -> %q",
			got.Status.Message, again.Status.Message)
	}
}
