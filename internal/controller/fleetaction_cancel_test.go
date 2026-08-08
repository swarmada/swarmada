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
	live := &metav1.Time{Time: time.Now().Add(leaseDuration)}
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
	live := &metav1.Time{Time: time.Now().Add(leaseDuration)}
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
	live := &metav1.Time{Time: time.Now().Add(leaseDuration)}
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
