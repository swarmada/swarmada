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

// THE GAP THIS TEST EXISTS FOR. spec.payload.raw must ride the ASSIGNMENT, not just
// validate_action: §9.1.4.5 tells adapter implementers "an adapter MUST read the payload
// from payload_json on assignment rather than relying on what it retained at validation",
// and since proto field 3 `destination` is deprecated ("destination now travels in
// payload_json") this is the only channel for one. Before this, pushAssignAction built
// seven fields and none of them was the payload, so every Navigate reached its robot with
// no destination — and no test noticed.
func TestWire_AssignActionCarriesTheSpecPayload(t *testing.T) {
	payload := []byte(`{"destination":{"x":3,"y":4}}`)
	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec: fleetv1.FleetActionSpec{
			Type:    fleetv1.ActionTypeNavigate,
			Payload: &fleetv1.ActionPayload{Raw: payload},
		},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	cmd := &fakeCommander{}
	r, _ := newActionReconciler(t, pending, robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = cmd

	reconcileAction(t, r, "t1")

	if len(cmd.assigns) != 1 {
		t.Fatalf("assign pushes = %+v, want exactly one", cmd.assigns)
	}
	if got := string(cmd.assigns[0].Payload); got != string(payload) {
		t.Errorf("assign_action payload = %q, want the spec.payload.raw %q", got, payload)
	}
}

// An absent spec.payload sends NO bytes, not an empty non-nil slice — the explicit-presence
// discipline in docs/api-principles.md. An adapter distinguishing "no payload" from "empty
// payload" must not be handed the latter for the former.
func TestWire_AssignActionSendsNoBytesWhenPayloadIsAbsent(t *testing.T) {
	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}, // Payload is nil
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	cmd := &fakeCommander{}
	r, _ := newActionReconciler(t, pending, robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Commander = cmd

	reconcileAction(t, r, "t1")

	if len(cmd.assigns) != 1 {
		t.Fatalf("assign pushes = %+v, want exactly one", cmd.assigns)
	}
	if cmd.assigns[0].Payload != nil {
		t.Errorf("assign_action payload = %#v, want nil for an absent spec.payload", cmd.assigns[0].Payload)
	}
}

// On lease renewal (steady-state), the reconciler pushes renew_lease at the
// current generation.
func TestWire_RenewalPushesRenewLease(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
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

// TestWire_ConfiguredLeaseHorizonReachesTheRobot is the regression test for the
// divergence ADR-0044 exists to prevent: the control plane resolves the namespace's
// lease horizon for status.leaseExpiresAt, but pushes a DIFFERENT (constant) value as
// lease_duration_ms. That would arm the robot's self-stop timer at 30s while the
// control plane waited out a 90s horizon before reassigning — both halves of the
// single-executor guarantee computed from different numbers.
//
// The assertion is deliberately a comparison between the two, not against a literal:
// it fails if either half stops tracking spec.scheduling.leaseDurationSeconds.
func TestWire_ConfiguredLeaseHorizonReachesTheRobot(t *testing.T) {
	const leaseSeconds = 90

	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	cfg := configWithSpec(actionNS, fleetv1.SwarmadaConfigSpec{
		Scheduling: fleetv1.SwarmadaSchedulingConfig{LeaseDurationSeconds: leaseSeconds},
	})
	cmd := &fakeCommander{}
	r, c := newActionReconciler(t, pending, robotInPhase("r1", fleetv1.RobotPhaseIdle, ""), cfg)
	r.Commander = cmd

	before := time.Now()
	reconcileAction(t, r, "t1")

	if len(cmd.assigns) != 1 {
		t.Fatalf("assign pushes = %d, want 1", len(cmd.assigns))
	}
	// The wire carries the CONFIGURED horizon, not defaultLeaseDuration.
	wantMs := command.LeaseDurationMs(leaseSeconds * time.Second)
	if got := cmd.assigns[0].LeaseDurationMs; got != wantMs {
		t.Errorf("assign_action lease_duration_ms = %d, want %d (the namespace horizon); default constant would be %d",
			got, wantMs, command.LeaseDurationMs(defaultLeaseDuration))
	}
	// ...and the server-side horizon agrees with what the robot was told.
	lease := getAction(t, c, "t1").Status.LeaseExpiresAt
	if lease == nil {
		t.Fatal("no leaseExpiresAt written")
	}
	horizon := lease.Sub(before)
	if horizon < leaseSeconds*time.Second-5*time.Second || horizon > leaseSeconds*time.Second+5*time.Second {
		t.Errorf("status.leaseExpiresAt is %v out, want ~%ds — control plane and robot disagree",
			horizon, leaseSeconds)
	}
}
