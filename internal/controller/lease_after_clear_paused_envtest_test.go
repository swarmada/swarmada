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
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The Paused lease after a clear on a real API server (RFC-0001 #safety-action-table, The lease):
// it advances under the robot's confirmed stop, is frozen once the clear ends the confirmation
// (Resuming), and stays frozen under a stop the robot confirms after the clear, until the work is
// settled.
func TestEnvtest_LeaseAfterClear_APausedLeaseStaysFrozenUnderAStopConfirmedAfterTheClear(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatal(err)
	}
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatal(err)
	}
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, time.Now())
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z-a", Priority: fleetv1.ActionPriorityNormal},
	}
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatal(err)
	}
	setLease := func(at time.Time) metav1.Time {
		t.Helper()
		var a fleetv1.FleetAction
		if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "task-1"}, &a); err != nil {
			t.Fatal(err)
		}
		lease := metav1.NewTime(at.Truncate(time.Second))
		a.Status = fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePaused, AssignedRobot: "amr-1",
			AssignmentGeneration: 4, LeaseExpiresAt: &lease, Message: estopPausedInProgressMessage,
			Conditions: a.Status.Conditions}
		if err := envK8s.Status().Update(ctx, &a); err != nil {
			t.Fatal(err)
		}
		return lease
	}
	leaseNow := func() (*metav1.Time, *metav1.Condition) {
		t.Helper()
		var a fleetv1.FleetAction
		if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "task-1"}, &a); err != nil {
			t.Fatal(err)
		}
		return a.Status.LeaseExpiresAt, apimeta.FindStatusCondition(a.Status.Conditions, conditionRenewalHeld)
	}
	cmd := &fakeCommander{}
	actions := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Commander: cmd}

	// Under the confirmed stop the lease advances.
	before := setLease(time.Now().Add(defaultLeaseDuration / 2))
	reconcileResumeAction(t, actions, ns)
	if got, _ := leaseNow(); got == nil || !got.After(before.Time) {
		t.Fatalf("leaseExpiresAt %v under the confirmed stop; want it advanced past %v", got, before)
	}

	// The clear: frozen, and the hold is recorded.
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopResuming, time.Now())
	held := setLease(time.Now().Add(defaultLeaseDuration / 2))
	reconcileResumeAction(t, actions, ns)
	reconcileResumeAction(t, actions, ns)
	if got, _ := leaseNow(); got == nil || !got.Equal(&held) {
		t.Fatalf("after the clear: lease %v; want it frozen at %v", got, held)
	}

	// A fresh stop the robot confirms: still frozen.
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, time.Now())
	reconcileResumeAction(t, actions, ns)
	reconcileResumeAction(t, actions, ns)
	got, cond := leaseNow()
	if got == nil || !got.Equal(&held) || len(cmd.renews) != 0 {
		t.Errorf("under a stop confirmed after the clear: lease %v, %d wire renewals; want it frozen at %v and none",
			got, len(cmd.renews), held)
	}
	// The hold is recorded on the work, so it outlives the robot's Resuming.
	if cond == nil || cond.Reason != reasonStopClearedWhilePaused {
		t.Errorf("condition %+v; want RenewalHeld/%s", cond, reasonStopClearedWhilePaused)
	}
}
