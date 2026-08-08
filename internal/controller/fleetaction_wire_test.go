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

// fakeCommander records assign/renew/cancel pushes and returns scripted outcomes.
type fakeCommander struct {
	assignErr    error
	assignReject bool
	renewErr     error
	cancelErr    error
	cancelAck    bool
	cancelDisp   command.CancelDisposition
	assigns      []command.AssignAction
	renews       []command.RenewLease
	cancels      []command.CancelAction
	assignTo     []string
	renewTo      []string
	cancelTo     []string
}

func (f *fakeCommander) PushAssignAction(_ context.Context, _, robotID string, a command.AssignAction) (command.AssignActionOutcome, error) {
	f.assigns = append(f.assigns, a)
	f.assignTo = append(f.assignTo, robotID)
	if f.assignErr != nil {
		return command.AssignActionOutcome{}, f.assignErr
	}
	if f.assignReject {
		return command.AssignActionOutcome{Accepted: false, Rejection: "ASSIGN_ACTION_REJECTION_ROBOT_BUSY"}, nil
	}
	return command.AssignActionOutcome{Accepted: true, AcceptedFencingToken: a.FencingToken}, nil
}

func (f *fakeCommander) PushRenewLease(_ context.Context, _, robotID string, r command.RenewLease) (command.RenewLeaseOutcome, error) {
	f.renews = append(f.renews, r)
	f.renewTo = append(f.renewTo, robotID)
	if f.renewErr != nil {
		return command.RenewLeaseOutcome{}, f.renewErr
	}
	return command.RenewLeaseOutcome{Renewed: true, Running: true, CurrentGeneration: r.LeaseGeneration}, nil
}

func (f *fakeCommander) PushCancelAction(_ context.Context, _, robotID string, c command.CancelAction) (command.CancelActionOutcome, error) {
	f.cancels = append(f.cancels, c)
	f.cancelTo = append(f.cancelTo, robotID)
	if f.cancelErr != nil {
		return command.CancelActionOutcome{}, f.cancelErr
	}
	return command.CancelActionOutcome{Acknowledged: f.cancelAck, Disposition: f.cancelDisp}, nil
}

// On assignment commit, the reconciler delivers assign_action over the wire with the
// freshly-minted generation as the fencing token.
func TestWire_CommitPushesAssignAction(t *testing.T) {
	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	cmd := &fakeCommander{}
	r, c := newActionReconciler(t, pending, robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = cmd

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatal("task did not commit to Assigned")
	}
	if len(cmd.assigns) != 1 || cmd.assignTo[0] != "r1" {
		t.Fatalf("assign pushes = %+v to %v", cmd.assigns, cmd.assignTo)
	}
	if got := cmd.assigns[0]; got.ActionID != "t1" || got.FencingToken != 1 || got.LeaseGeneration != 1 {
		t.Errorf("pushed assign_action = %+v (want fencing/gen 1)", got)
	}
}

// On lease renewal (steady-state), the reconciler pushes renew_lease at the
// current generation.
func TestWire_RenewalPushesRenewLease(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	cmd := &fakeCommander{}
	r, _ := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 3, live),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = cmd

	reconcileAction(t, r, "t1")

	if len(cmd.renews) != 1 || cmd.renewTo[0] != "r1" {
		t.Fatalf("renew pushes = %+v to %v", cmd.renews, cmd.renewTo)
	}
	if cmd.renews[0].ActionID != "t1" || cmd.renews[0].LeaseGeneration != 3 {
		t.Errorf("pushed renew_lease = %+v (want gen 3)", cmd.renews[0])
	}
	if len(cmd.assigns) != 0 {
		t.Errorf("renewal must not push assign_action: %+v", cmd.assigns)
	}
}

// The wire is NON-GATING: an unreachable/erroring push never rolls back the
// authoritative committed assignment.
func TestWire_PushFailureDoesNotAffectAssignment(t *testing.T) {
	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	r, c := newActionReconciler(t, pending, robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = &fakeCommander{assignErr: command.ErrUnreachable}

	reconcileAction(t, r, "t1") // must not error out of the reconcile

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseAssigned || ft.Status.AssignedRobot != "r1" || ft.Status.AssignmentGeneration != 1 {
		t.Fatalf("a failed wire push disturbed the authoritative assignment: phase=%s robot=%q gen=%d",
			ft.Status.Phase, ft.Status.AssignedRobot, ft.Status.AssignmentGeneration)
	}
}

// A nil Commander (ControlStream disabled) commits without any wire push and
// without panicking.
func TestWire_NilCommanderCommitsCleanly(t *testing.T) {
	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	r, c := newActionReconciler(t, pending, robotInPhase("r1", fleetv1.RobotPhaseIdle, "")) // Commander nil by default

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatal("commit failed with a nil Commander")
	}
}
