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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The lease after a clear (RFC-0001 #safety-action-table, The lease). After the clear nothing about
// the work the robot records resumes automatically, lease renewals included, until the robot's own
// report or an operator's decision settles it; the operator release (S6) releases the robot, not
// its work (F12). The clear ends the confirmation, so the lease of a Paused action whose robot is
// Resuming is frozen, not advanced (F14).

func setActionRobotEstop(t *testing.T, c client.Client, name string, state fleetv1.RobotEstopState) {
	t.Helper()
	r := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: name}, r); err != nil {
		t.Fatal(err)
	}
	r.Status.EstopState = state
	if err := c.Status().Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func renewalHeldEvents(rec *record.FakeRecorder, reason string) int {
	n := 0
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				n++
			}
		default:
			return n
		}
	}
}

// F12: an InProgress action whose robot's stop was confirmed and cleared before row A1 paused it.
// Three reconciles while Resuming, then the release to Normal and three more: no RenewActionLease,
// the lease unchanged, the condition set once and the Event raised once.
func TestLeaseAfterClear_WorkA1NeverPausedIsNotRenewed(t *testing.T) {
	for _, phase := range []fleetv1.ActionPhase{fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress} {
		t.Run(string(phase), func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration).Truncate(time.Second)}
			robot := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1")
			robot.Status.EstopState = fleetv1.RobotEstopResuming
			cmd := &fakeCommander{}
			r, c := newActionReconciler(t, assignedAction("t1", "r1", phase, 3, lease), robot)
			r.Commander = cmd
			rec := record.NewFakeRecorder(20)
			r.Recorder = rec
			for i := 0; i < 3; i++ {
				reconcileAction(t, r, "t1")
			}
			setActionRobotEstop(t, c, "r1", fleetv1.RobotEstopNormal)
			for i := 0; i < 3; i++ {
				reconcileAction(t, r, "t1")
			}
			got := getAction(t, c, "t1")
			if len(cmd.renews) != 0 {
				t.Fatalf("%d RenewActionLease sent after the clear; want none", len(cmd.renews))
			}
			if got.Status.LeaseExpiresAt == nil || !got.Status.LeaseExpiresAt.Equal(lease) {
				t.Fatalf("leaseExpiresAt %v; want it frozen at %v", got.Status.LeaseExpiresAt, lease)
			}
			cond := apimeta.FindStatusCondition(got.Status.Conditions, conditionRenewalHeld)
			if cond == nil || cond.Reason != reasonStopClearedBeforePause || got.Status.Phase != phase {
				t.Fatalf("phase %s, condition %+v; want the phase unchanged and RenewalHeld/StopClearedBeforePause", got.Status.Phase, cond)
			}
			if n := renewalHeldEvents(rec, conditionRenewalHeld); n != 1 {
				t.Fatalf("%d RenewalHeld Events; want one, on the transition", n)
			}
		})
	}
}

// F12 ends when the work leaves Assigned and InProgress: a declarative pause moves it to Paused,
// where the lease stays frozen under no confirmed stop (F14). The next assignment drops the
// condition.
func TestLeaseAfterClear_TheHoldEndsWhenTheWorkLeavesAssignedAndInProgress(t *testing.T) {
	// Half a duration ahead, so a stamp (now plus one duration) is told apart from it.
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration / 2).Truncate(time.Second)}
	robot := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1")
	robot.Status.EstopState = fleetv1.RobotEstopResuming
	cmd := &fakeCommander{}
	r, c := newActionReconciler(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 3, lease), robot)
	r.Commander = cmd
	reconcileAction(t, r, "t1")
	a := getAction(t, c, "t1")
	a.Spec.DesiredState = fleetv1.DesiredStatePaused
	if err := c.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "t1")
	reconcileAction(t, r, "t1")
	got := getAction(t, c, "t1")
	if got.Status.Phase != fleetv1.ActionPhasePaused || len(cmd.renews) != 0 {
		t.Fatalf("phase %s, %d renewals; want Paused and none", got.Status.Phase, len(cmd.renews))
	}
	if !got.Status.LeaseExpiresAt.Equal(lease) {
		t.Fatalf("leaseExpiresAt %v; want it frozen at %v", got.Status.LeaseExpiresAt, lease)
	}
	if renewalHeldAfterClear(got, robot, true) {
		t.Fatal("a Paused action still reads as renewal-held")
	}
}

// A Revoking action whose robot is Resuming is held at Revoking, as before: unchanged by F12.
func TestLeaseAfterClear_ARevokingActionOnAResumingRobotIsUnchanged(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration).Truncate(time.Second)}
	robot := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1")
	robot.Status.EstopState = fleetv1.RobotEstopResuming
	cmd := &fakeCommander{}
	r, c := newActionReconciler(t, assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, lease), robot)
	r.Commander = cmd
	reconcileAction(t, r, "t1")
	got := getAction(t, c, "t1")
	if got.Status.Phase != fleetv1.ActionPhaseRevoking || len(cmd.renews) != 0 || !got.Status.LeaseExpiresAt.Equal(lease) {
		t.Fatalf("phase %s, %d renewals, lease %v; want Revoking, none, frozen", got.Status.Phase, len(cmd.renews), got.Status.LeaseExpiresAt)
	}
}

// F14: a Paused action's lease advances only while its robot has confirmed its stop (Stopped), is
// connected, and its horizon has not passed. The clear (Resuming), the release (Normal), and a
// horizon that passed while the robot was lost each leave it frozen.
func TestLeaseAfterClear_APausedLeaseAdvancesOnlyUnderAConfirmedStop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		estop    fleetv1.RobotEstopState
		lease    time.Duration // from now
		advances bool
	}{
		{"stopped", fleetv1.RobotEstopStopped, defaultLeaseDuration / 2, true},
		{"resuming-after-the-clear", fleetv1.RobotEstopResuming, defaultLeaseDuration / 2, false},
		{"normal-after-the-release", fleetv1.RobotEstopNormal, defaultLeaseDuration / 2, false},
		{"stopped-horizon-passed", fleetv1.RobotEstopStopped, -time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(tc.lease).Truncate(time.Second)}
			robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "t1")
			robot.Status.EstopState = tc.estop
			a := assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 3, lease)
			a.Status.Message = estopPausedInProgressMessage
			cmd := &fakeCommander{}
			r, c := newActionReconciler(t, a, robot)
			r.Commander = cmd
			reconcileAction(t, r, "t1")
			reconcileAction(t, r, "t1")
			got := getAction(t, c, "t1")
			advanced := got.Status.LeaseExpiresAt != nil && got.Status.LeaseExpiresAt.After(lease.Time)
			if advanced != tc.advances || len(cmd.renews) != 0 {
				t.Fatalf("lease %v (was %v), advanced %v, %d wire renewals; want advanced %v and none",
					got.Status.LeaseExpiresAt, lease, advanced, len(cmd.renews), tc.advances)
			}
		})
	}
}
