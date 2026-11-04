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
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

func pendingAction(name string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
}

// An EXPLICIT adapter rejection of the pushed assign_action releases the just-
// committed assignment back to Pending and frees the robot — no phantom
// Assigned/InProgress. Generation is preserved (next commit issues strictly-greater).
func TestAssign_RejectionReleasesAndReschedules(t *testing.T) {
	r, c := newActionReconciler(t, pendingAction("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = &fakeCommander{assignReject: true}

	res := reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending (rejected assignment released)", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "" || ft.Status.LeaseExpiresAt != nil {
		t.Fatalf("binding/lease not cleared: robot=%q lease=%v", ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt)
	}
	if ft.Status.AssignmentGeneration < 1 {
		t.Fatalf("generation = %d, want the committed one preserved (committed then released, never reused)", ft.Status.AssignmentGeneration)
	}
	rob := getRobot(t, c, "r1", actionNS)
	if rob.Status.AssignedAction != "" || rob.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot not freed after rejection: assignedAction=%q phase=%s", rob.Status.AssignedAction, rob.Status.Phase)
	}
	if res.RequeueAfter <= 0 {
		t.Error("a released assignment should requeue to reschedule")
	}
}

// An accepted assign_action leaves the committed assignment standing.
func TestAssign_AcceptedStands(t *testing.T) {
	r, c := newActionReconciler(t, pendingAction("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = &fakeCommander{} // accepts by default

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseAssigned || ft.Status.AssignedRobot != "r1" {
		t.Fatalf("accepted assignment did not stand: phase=%s robot=%q", ft.Status.Phase, ft.Status.AssignedRobot)
	}
}

// An unreachable push leaves the assignment standing (best-effort; the lease
// machinery governs a truly-lost robot, never freed on unconfirmed loss).
func TestAssign_UnreachableStands(t *testing.T) {
	r, c := newActionReconciler(t, pendingAction("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = &fakeCommander{assignErr: command.ErrUnreachable}

	reconcileAction(t, r, "t1")

	if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatalf("unreachable push disturbed the assignment: phase=%s", ft.Status.Phase)
	}
}

// An assignment the adapter rejected for its leader term was not acted on, and it is no refusal:
// the assignment stands, with no A24 revert, and the robot keeps its record (RFC-0001
// #fleet-adapter-protocol-the-stop-latch, The leader term).
func TestAssign_StaleLeaderTermStands(t *testing.T) {
	r, c := newActionReconciler(t, pendingAction("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = &fakeCommander{assignErr: fmt.Errorf("%w (command 4 sent with term 2)", command.ErrStaleLeaderTerm)}

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseAssigned || ft.Status.AssignedRobot != "r1" {
		t.Fatalf("a stale-term rejection reverted the assignment: phase=%s robot=%q", ft.Status.Phase, ft.Status.AssignedRobot)
	}
}

// An assignment the command dispatcher did not send because a stop holds the robot, or because the
// robot's register is not yet processed on its stream, went nowhere and is no refusal: the commit
// stands, with no A24 revert, and the stop's own rows decide the action from here (A1).
func TestAssign_NotSentUnderAStopStands(t *testing.T) {
	for _, err := range []error{command.ErrHeldByStop, command.ErrNotRegistered} {
		t.Run(err.Error(), func(t *testing.T) {
			r, c := newActionReconciler(t, pendingAction("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
			r.Commander = &fakeCommander{assignErr: err}

			reconcileAction(t, r, "t1")

			ft := getAction(t, c, "t1")
			if ft.Status.Phase != fleetv1.ActionPhaseAssigned || ft.Status.AssignedRobot != "r1" {
				t.Fatalf("an assignment not sent was reverted: phase=%s robot=%q", ft.Status.Phase, ft.Status.AssignedRobot)
			}
		})
	}
}
