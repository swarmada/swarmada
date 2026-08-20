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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// spec.desiredState: Paused / Returning (ADR-0045).
//
// Both enter the SAME operator-gated Paused phase the estop path uses, through the same
// transitions, because two implementations of a single-executor rule is how one of them
// stops matching the other. Writing Running back does not lift the hold.

func heldAction(name, robot string, phase fleetv1.ActionPhase, ds fleetv1.DesiredState,
	lease *metav1.Time) *fleetv1.FleetAction {
	a := assignedAction(name, robot, phase, 4, lease)
	a.Spec.DesiredState = ds
	return a
}

// ── Paused: the §9.6.2.4 split, reached declaratively ────────────────────────

func TestDesiredPaused_AssignedReleasesRobotAndZoneSlot(t *testing.T) {
	// Table §9.6.2.4 row `Assigned`: the robot never started, so the binding goes back and
	// the zone slot is freed for someone who can use it.
	a := heldAction("t1", "r1", fleetv1.ActionPhaseAssigned, fleetv1.DesiredStatePaused, nil)
	a.Spec.Zone = "z1"
	tdeDouble := &fakeTDE{}
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseAssigned, "t1"))
	r.TDE = tdeDouble

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s, want Paused", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "" || ft.Status.LeaseExpiresAt != nil {
		t.Errorf("Assigned hold kept the binding: robot=%q lease=%v",
			ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "" {
		t.Errorf("robot still bound to %q after an Assigned hold", rob.Status.AssignedAction)
	}
	if tdeDouble.releases != 1 {
		t.Errorf("zone slot releases = %d, want 1 — a robot that never entered the zone "+
			"must not hold its slot through the pause", tdeDouble.releases)
	}
}

func TestDesiredPaused_InProgressKeepsRobotBoundAndLeaseRenewed(t *testing.T) {
	// Table §9.6.2.4 row `InProgress`: the robot is physically committed. Releasing it
	// here would let a second robot take the action — the double-execution §9.6.3.5
	// exists to prevent — and the reason for the hold does not change that.
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := heldAction("t1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.DesiredStatePaused, lease)
	tdeDouble := &fakeTDE{}
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.TDE = tdeDouble

	res := reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s, want Paused", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Fatalf("robot released on an InProgress hold (%q) — single-executor violation",
			ft.Status.AssignedRobot)
	}
	if ft.Status.LeaseExpiresAt == nil || ft.Status.AssignmentGeneration != 4 {
		t.Errorf("lease not retained: gen=%d lease=%v",
			ft.Status.AssignmentGeneration, ft.Status.LeaseExpiresAt)
	}
	if res.RequeueAfter == 0 {
		t.Error("no requeue scheduled — the lease must keep being renewed while held")
	}
	if tdeDouble.releases != 0 {
		t.Errorf("zone slot released = %d, want 0 — an in-zone robot keeps its slot",
			tdeDouble.releases)
	}
}

func TestDesiredPaused_MessageNamesTheIntake(t *testing.T) {
	// The phase alone cannot say what stopped the action, and the two causes call for
	// different investigation before a human resumes. Assert both directions so neither
	// message can drift into the other's wording.
	held := heldAction("t1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.DesiredStatePaused,
		&metav1.Time{Time: time.Now().Add(defaultLeaseDuration)})
	r1, c1 := newActionReconciler(t, held, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	reconcileAction(t, r1, "t1")
	msg := getAction(t, c1, "t1").Status.Message
	if !strings.Contains(msg, "desiredState=Paused") {
		t.Errorf("declarative hold message = %q, want it to name desiredState", msg)
	}
	if strings.Contains(msg, "estop") {
		t.Errorf("declarative hold message = %q claims an estop", msg)
	}

	stopped := assignedAction("t2", "r2", fleetv1.ActionPhaseInProgress, 4,
		&metav1.Time{Time: time.Now().Add(defaultLeaseDuration)})
	r2, c2 := newActionReconciler(t, stopped, robotEstopped("r2", fleetv1.RobotEstopStopped, "t2"))
	reconcileAction(t, r2, "t2")
	emsg := getAction(t, c2, "t2").Status.Message
	if !strings.Contains(emsg, "estop") {
		t.Errorf("estop hold message = %q, want it to name the estop", emsg)
	}
	if strings.Contains(emsg, "desiredState") {
		t.Errorf("estop hold message = %q claims a declarative hold", emsg)
	}
}

// ── Running does not resume ──────────────────────────────────────────────────

func TestDesiredRunning_DoesNotResumeAHeldAction(t *testing.T) {
	// The decision ADR-0045 turns on. If Running resumed, an operator could not read
	// `phase: Paused` and know whether it would move on its own — and Running would
	// contend with the estop-pause check on every reconcile.
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := heldAction("t1", "r1", fleetv1.ActionPhasePaused, fleetv1.DesiredStateRunning, lease)
	a.Status.Message = desiredPausedInProgressMessage
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))

	for i := 0; i < 3; i++ {
		reconcileAction(t, r, "t1")
	}

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s after writing Running back — a hold is lifted only by an "+
			"operator through the verb-gated intake (ADR-0045)", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Errorf("robot released while held: %q", ft.Status.AssignedRobot)
	}
}

// ── Returning: confirmed recovery, then the same hold ────────────────────────

func TestDesiredReturning_ConfirmedRecoveryHoldsInPaused(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := heldAction("t1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.DesiredStateReturning, lease)
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelRecovered}
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = cmd

	reconcileAction(t, r, "t1")

	// No new wire message: the recovery rides the existing cancel_action path.
	if len(cmd.cancels) != 1 {
		t.Fatalf("cancel_action pushes = %d, want 1 (Returning reuses the cancel path)",
			len(cmd.cancels))
	}
	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s, want Paused — Returning holds, it does not terminate",
			ft.Status.Phase)
	}
	if !strings.Contains(ft.Status.Message, "Returning") {
		t.Errorf("message = %q, want it to name the Returning intake", ft.Status.Message)
	}
	// The disposition is the adapter's account of how far the robot got, and it decides
	// what an operator does next.
	if !strings.Contains(ft.Status.Message, "recovered mid-commitment") {
		t.Errorf("message = %q, want the CancelRecovered disposition surfaced", ft.Status.Message)
	}
}

func TestDesiredReturning_UnconfirmedStopNeverFreesTheRobot(t *testing.T) {
	// Unreachable is not stopped (RA-4). Until the adapter confirms, the robot may still
	// be executing, so the action holds and the binding stands.
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := heldAction("t1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.DesiredStateReturning, lease)
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	r.Commander = &fakeCommander{cancelAck: false}

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase == fleetv1.ActionPhasePaused {
		t.Fatal("Returning settled into the hold without a confirmed stop")
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Errorf("robot freed on an unconfirmed recovery: %q", ft.Status.AssignedRobot)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "t1" {
		t.Errorf("robot unbound on an unconfirmed recovery: %q", rob.Status.AssignedAction)
	}
	if !strings.Contains(ft.Status.Message, "awaiting confirmed recovery") {
		t.Errorf("message = %q, want it to say the recovery is unconfirmed", ft.Status.Message)
	}
}

// ── Ordering hazard: estop and desiredState must not oscillate ───────────────

func TestDesiredPaused_UnderLiveEstopDoesNotOscillate(t *testing.T) {
	// Both intents live at once. The estop check runs first, so it names the hold; the
	// declarative check then finds the phase already Paused and does nothing. Neither
	// re-enters, so successive reconciles must converge rather than trade the message
	// back and forth — a loop here would be a loop on the safety path.
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := heldAction("t1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.DesiredStatePaused, lease)
	r, c := newActionReconciler(t, a, robotEstopped("r1", fleetv1.RobotEstopStopped, "t1"))

	reconcileAction(t, r, "t1")
	first := getAction(t, c, "t1")
	if first.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s, want Paused", first.Status.Phase)
	}
	if !strings.Contains(first.Status.Message, "estop") {
		t.Errorf("message = %q — the estop is the more urgent cause and must name the hold",
			first.Status.Message)
	}

	for i := 0; i < 4; i++ {
		reconcileAction(t, r, "t1")
		got := getAction(t, c, "t1")
		if got.Status.Phase != fleetv1.ActionPhasePaused {
			t.Fatalf("reconcile %d: phase moved to %s", i+2, got.Status.Phase)
		}
		if got.Status.Message != first.Status.Message {
			t.Fatalf("reconcile %d: message oscillated %q -> %q — the two intents are "+
				"trading the hold", i+2, first.Status.Message, got.Status.Message)
		}
		if got.Status.AssignedRobot != "r1" {
			t.Fatalf("reconcile %d: robot released while estopped", i+2)
		}
	}
}

func TestDesiredPaused_EstopArrivingAfterADeclarativeHoldDoesNotRePause(t *testing.T) {
	// The reverse order. Once Paused, the estop switch no longer matches (it only handles
	// Assigned/InProgress), so the standing hold is left alone rather than rewritten.
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	a := heldAction("t1", "r1", fleetv1.ActionPhaseInProgress, fleetv1.DesiredStatePaused, lease)
	r, c := newActionReconciler(t, a, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))

	reconcileAction(t, r, "t1") // declarative hold lands first
	held := getAction(t, c, "t1")
	if !strings.Contains(held.Status.Message, "desiredState=Paused") {
		t.Fatalf("message = %q, want the declarative hold", held.Status.Message)
	}

	// Now the robot is estopped while the action is already held.
	rob := getRobot(t, c, "r1", actionNS)
	rob.Status.EstopState = fleetv1.RobotEstopStopped
	if err := r.Status().Update(t.Context(), rob); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "t1")

	got := getAction(t, c, "t1")
	if got.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s, want a stable Paused", got.Status.Phase)
	}
	if got.Status.AssignedRobot != "r1" {
		t.Errorf("robot released: %q", got.Status.AssignedRobot)
	}
}

// A Pending action is not executing, so §9.6.2.4's table says `Pending` → no change and
// the hold reuses that verbatim. It is worth pinning: a hold that silently terminated
// queued work would be a very different feature.
func TestDesiredPaused_PendingActionIsNotHeld(t *testing.T) {
	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeNavigate, DesiredState: fleetv1.DesiredStatePaused,
		},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	r, c := newActionReconciler(t, a)

	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1").Status.Phase; got == fleetv1.ActionPhasePaused {
		t.Error("a Pending action was moved to Paused; §9.6.2.4 specifies no change")
	}
}
