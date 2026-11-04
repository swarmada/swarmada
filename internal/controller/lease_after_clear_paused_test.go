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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A stop confirmed after the clear (RFC-0001 #safety-action-table, The lease). The control plane
// advances no leaseExpiresAt for the work the robot records until the robot's own report or an
// operator's decision settles it, whatever the work's phase. A fresh stop the robot confirms after
// the clear is not that settlement, so it does not advance the lease again: neither the Paused
// branch nor row A1's pause of work F12 already holds.

// A Paused action whose robot's stop is cleared, then a fresh stop confirmed: the lease stays where
// the clear left it. Through the release (Normal) too. The condition and its Event come once.
func TestLeaseAfterClear_AStopConfirmedAfterTheClearDoesNotAdvanceAPausedLease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		between []fleetv1.RobotEstopState // the robot's states after the clear, before the fresh stop
	}{
		{"resuming-then-stopped", []fleetv1.RobotEstopState{fleetv1.RobotEstopResuming}},
		{"resuming-released-then-stopped", []fleetv1.RobotEstopState{fleetv1.RobotEstopResuming, fleetv1.RobotEstopNormal}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration / 2).Truncate(time.Second)}
			robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "t1")
			robot.Status.EstopState = fleetv1.RobotEstopStopped
			a := assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 3, lease)
			a.Status.Message = estopPausedInProgressMessage
			cmd := &fakeCommander{}
			r, c := newActionReconciler(t, a, robot)
			r.Commander = cmd
			rec := record.NewFakeRecorder(20)
			r.Recorder = rec

			// Under the confirmed stop the lease advances (the third bullet of The lease).
			reconcileAction(t, r, "t1")
			atClear := getAction(t, c, "t1").Status.LeaseExpiresAt
			if atClear == nil || !atClear.After(lease.Time) {
				t.Fatalf("leaseExpiresAt %v under the confirmed stop; want it advanced past %v", atClear, lease)
			}

			for _, st := range tc.between {
				setActionRobotEstop(t, c, "r1", st)
				reconcileAction(t, r, "t1")
				reconcileAction(t, r, "t1")
			}
			if got := getAction(t, c, "t1").Status.LeaseExpiresAt; got == nil || !got.Equal(atClear) {
				t.Fatalf("leaseExpiresAt %v after the clear; want it frozen at %v", got, atClear)
			}
			// Time passes: the frozen lease is half a duration ahead. The store keeps seconds, so
			// a stamp (now plus one duration) is told apart from it.
			atClear = &metav1.Time{Time: time.Now().Add(defaultLeaseDuration / 2).Truncate(time.Second)}
			held := getAction(t, c, "t1")
			held.Status.LeaseExpiresAt = atClear
			if err := c.Status().Update(context.Background(), held); err != nil {
				t.Fatal(err)
			}
			// A fresh stop, confirmed.
			setActionRobotEstop(t, c, "r1", fleetv1.RobotEstopStopped)
			reconcileAction(t, r, "t1")
			reconcileAction(t, r, "t1")

			got := getAction(t, c, "t1")
			if got.Status.LeaseExpiresAt == nil || !got.Status.LeaseExpiresAt.Equal(atClear) {
				t.Fatalf("leaseExpiresAt %v after a stop confirmed after the clear; want it frozen at %v", got.Status.LeaseExpiresAt, atClear)
			}
			if len(cmd.renews) != 0 || got.Status.Phase != fleetv1.ActionPhasePaused {
				t.Fatalf("phase %s, %d wire renewals; want Paused and none", got.Status.Phase, len(cmd.renews))
			}
			cond := apimeta.FindStatusCondition(got.Status.Conditions, conditionRenewalHeld)
			if cond == nil || cond.Reason != reasonStopClearedWhilePaused {
				t.Fatalf("condition %+v; want RenewalHeld/%s", cond, reasonStopClearedWhilePaused)
			}
			if n := renewalHeldEvents(rec, conditionRenewalHeld); n != 1 {
				t.Fatalf("%d RenewalHeld Events; want one, on the transition", n)
			}
		})
	}
}

// Work F12 holds (its robot's stop was cleared before row A1 paused it) that a fresh confirmed stop
// then pauses: row A1 keeps the lease, it does not stamp a new one, and the Paused branch does not
// advance it after.
func TestLeaseAfterClear_RowA1DoesNotStampTheLeaseOfWorkTheClearHolds(t *testing.T) {
	for _, phase := range []fleetv1.ActionPhase{fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress} {
		t.Run(string(phase), func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration / 2).Truncate(time.Second)}
			robot := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1")
			robot.Status.EstopState = fleetv1.RobotEstopResuming
			cmd := &fakeCommander{}
			r, c := newActionReconciler(t, assignedAction("t1", "r1", phase, 3, lease), robot)
			r.Commander = cmd
			reconcileAction(t, r, "t1")
			if cond := apimeta.FindStatusCondition(getAction(t, c, "t1").Status.Conditions, conditionRenewalHeld); cond == nil {
				t.Fatal("no RenewalHeld after the clear; the setup is not F12's")
			}
			setActionRobotEstop(t, c, "r1", fleetv1.RobotEstopStopped)
			reconcileAction(t, r, "t1")
			reconcileAction(t, r, "t1")
			got := getAction(t, c, "t1")
			if got.Status.Phase != fleetv1.ActionPhasePaused {
				t.Fatalf("phase %s; want Paused by row A1", got.Status.Phase)
			}
			if got.Status.LeaseExpiresAt == nil || !got.Status.LeaseExpiresAt.Equal(lease) {
				t.Fatalf("leaseExpiresAt %v; want it kept at %v", got.Status.LeaseExpiresAt, lease)
			}
			if len(cmd.renews) != 0 {
				t.Fatalf("%d wire renewals; want none", len(cmd.renews))
			}
		})
	}
}

// The hold is the clear's: a Paused action whose robot was never cleared still advances under its
// confirmed stop, and a Paused action no robot is bound to sets nothing.
func TestLeaseAfterClear_APausedLeaseWithNoClearStillAdvances(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration / 2).Truncate(time.Second)}
	robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "t1")
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	a := assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 3, lease)
	a.Status.Message = estopPausedInProgressMessage
	r, c := newActionReconciler(t, a, robot)
	r.Commander = &fakeCommander{}
	for i := 0; i < 3; i++ {
		reconcileAction(t, r, "t1")
	}
	got := getAction(t, c, "t1")
	if got.Status.LeaseExpiresAt == nil || !got.Status.LeaseExpiresAt.After(lease.Time) {
		t.Fatalf("leaseExpiresAt %v; want it advanced past %v", got.Status.LeaseExpiresAt, lease)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, conditionRenewalHeld); cond != nil {
		t.Fatalf("condition %+v on work no clear reached; want none", cond)
	}
}
