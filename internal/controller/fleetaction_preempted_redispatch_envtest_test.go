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

// Can a preemption victim be dispatched to a second robot while the first may still be
// running it?
//
// The scenario is the one the single-executor guarantee exists for (RFC-0001
// #safety-assignment-lease-and-the-single-executor-guarantee). A Normal action runs on r1
// under a live lease. A Critical action of the SAME type preempts it on r1, so no
// cancel_action is required before the handover, and the preemptor's assign_action does
// not reach the adapter. r1 was never told to stop the victim and its lease on r1 has not
// expired. A second robot then becomes free.
//
// The victim may leave Preempted only once r1 is proven off it: the adapter confirms a
// cancel_action for it, or its frozen lease horizon has passed. The control plane's own
// write of r1's status.assignedAction is not that proof, because it records what the
// plane decided, not what the robot is doing.

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// redispatchFleet seeds an adapter, zone z, and two Idle robots, r1 ahead of r2 on battery
// so a Normal action lands on r1 first.
func redispatchFleet(t *testing.T) (string, *FleetActionReconciler, *fakeCommander) {
	t.Helper()
	ns := envtestNamespace(t)
	cmd := &fakeCommander{}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}
	f12SeedAdapter(t, ns)
	f12SeedRobot(t, ns, "r1", "z")
	f12SeedRobot(t, ns, "r2", "z")
	seedBattery(t, ns, "r1", 90)
	seedBattery(t, ns, "r2", 50)
	return ns, r, cmd
}

func redispatchSeed(t *testing.T, ns, name string, prio fleetv1.ActionPriority) ctrl.Request {
	t.Helper()
	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z",
			Priority: prio},
	}
	if err := envK8s.Create(context.Background(), a); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func redispatchReconcile(t *testing.T, r *FleetActionReconciler, req ctrl.Request) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile %s: %v", req.Name, err)
	}
}

// cancelsFor counts the cancel_action pushes naming action to robot.
func (f *fakeCommander) cancelsFor(action, robot string) int {
	n := 0
	for i, c := range f.cancels {
		if c.ActionID == action && f.cancelTo[i] == robot {
			n++
		}
	}
	return n
}

// preemptWithUndeliveredHandover drives the fleet to the state the question is about:
// victim Preempted on r1 with a live lease, preemptor committed to r1 but undelivered, and
// r2 free. It returns the victim's request and its frozen lease horizon on r1.
func preemptWithUndeliveredHandover(t *testing.T, ns string, r *FleetActionReconciler,
	cmd *fakeCommander) (ctrl.Request, time.Time) {
	t.Helper()
	ctx := context.Background()

	victim := redispatchSeed(t, ns, "norm", fleetv1.ActionPriorityNormal)
	redispatchReconcile(t, r, victim)
	if v := zrGetAction(t, victim); v.Status.AssignedRobot != "r1" {
		t.Fatalf("victim assigned to %q, want r1", v.Status.AssignedRobot)
	}
	// r2 is taken by work a preemptor may not displace, so the preemptor must take r1.
	blocker := redispatchSeed(t, ns, "high", fleetv1.ActionPriorityHigh)
	redispatchReconcile(t, r, blocker)
	if b := zrGetAction(t, blocker); b.Status.AssignedRobot != "r2" {
		t.Fatalf("blocker assigned to %q, want r2", b.Status.AssignedRobot)
	}

	// The preemptor's assign_action does not reach the adapter.
	cmd.assignErr = errors.New("adapter unreachable")
	crit := redispatchSeed(t, ns, "crit", fleetv1.ActionPriorityCritical)
	redispatchReconcile(t, r, crit)
	if c := zrGetAction(t, crit); c.Status.AssignedRobot != "r1" {
		t.Fatalf("preemptor assigned to %q, want r1", c.Status.AssignedRobot)
	}
	v := zrGetAction(t, victim)
	if v.Status.Phase != fleetv1.ActionPhasePreempted || v.Status.LeaseExpiresAt == nil {
		t.Fatalf("victim phase=%s lease=%v, want Preempted with a lease", v.Status.Phase, v.Status.LeaseExpiresAt)
	}
	horizon := v.Status.LeaseExpiresAt.Time
	cmd.assignErr = nil

	// r2 becomes free: the blocker finishes and is retired.
	b := zrGetAction(t, blocker)
	b.Status.Phase = fleetv1.ActionPhaseSucceeded
	if err := envK8s.Status().Update(ctx, b); err != nil {
		t.Fatalf("finish blocker: %v", err)
	}
	redispatchReconcile(t, r, blocker)
	var r2 fleetv1.Robot
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "r2"}, &r2); err != nil {
		t.Fatalf("get r2: %v", err)
	}
	if r2.Status.Phase != fleetv1.RobotPhaseIdle || r2.Status.AssignedAction != "" {
		t.Fatalf("r2 phase=%s assignedAction=%q, want Idle and free", r2.Status.Phase, r2.Status.AssignedAction)
	}
	return victim, horizon
}

// THE MEASUREMENT. With no adapter confirmation and the lease on r1 still live, the
// victim must not be dispatched to r2. The failure message carries what was measured.
func TestEnvtest_PreemptedVictimIsNotRedispatchedWhileItsRobotMayStillRunIt(t *testing.T) {
	requireEnvtest(t)
	ns, r, cmd := redispatchFleet(t)
	cmd.cancelErr = errors.New("adapter unreachable") // no confirmation of any cancel
	victim, horizon := preemptWithUndeliveredHandover(t, ns, r, cmd)

	for i := 0; i < 3; i++ {
		redispatchReconcile(t, r, victim)
	}
	v := zrGetAction(t, victim)
	left := time.Until(horizon).Round(time.Second)
	t.Logf("after 3 victim reconciles: phase=%s robot=%q; lease on r1 live for %s more; "+
		"cancel_action for the victim pushed to r1 %d time(s), none confirmed",
		v.Status.Phase, v.Status.AssignedRobot, left, cmd.cancelsFor("norm", "r1"))
	if v.Status.AssignedRobot == "r2" {
		t.Fatalf("DUAL-EXECUTION WINDOW: the victim was dispatched to r2 (generation %d) while r1 may "+
			"still run it under generation %d's lease for %s more; r1 was sent no confirmed cancel",
			v.Status.AssignmentGeneration, v.Status.AssignmentGeneration-1, left)
	}
	if v.Status.Phase != fleetv1.ActionPhasePreempted || v.Status.AssignedRobot != "r1" {
		t.Fatalf("victim phase=%s robot=%q, want held Preempted on r1 until the stop is proven",
			v.Status.Phase, v.Status.AssignedRobot)
	}
	if n := cmd.cancelsFor("norm", "r1"); n == 0 {
		t.Fatal("no cancel_action for the victim was sent to r1; the robot is never told to stop")
	}
}

// The adapter confirms the cancel: the victim leaves Preempted and may go to r2.
func TestEnvtest_PreemptedVictimLeavesOnAConfirmedCancel(t *testing.T) {
	requireEnvtest(t)
	ns, r, cmd := redispatchFleet(t)
	cmd.cancelAck, cmd.cancelDisp = true, command.CancelStoppedSafely
	victim, horizon := preemptWithUndeliveredHandover(t, ns, r, cmd)

	redispatchReconcile(t, r, victim)
	if v := zrGetAction(t, victim); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q after a confirmed cancel, want Pending and unbound",
			v.Status.Phase, v.Status.AssignedRobot)
	}
	if n := cmd.cancelsFor("norm", "r1"); n != 1 {
		t.Fatalf("cancel_action for the victim to r1 pushed %d time(s), want 1", n)
	}
	redispatchReconcile(t, r, victim)
	if v := zrGetAction(t, victim); v.Status.AssignedRobot != "r2" {
		t.Fatalf("victim assigned to %q after release, want r2", v.Status.AssignedRobot)
	}
	if !time.Now().Before(horizon) {
		t.Fatal("the lease had already expired; this case must be the confirmed-cancel exit")
	}
}

// No confirmation ever comes: the victim leaves once its frozen lease horizon has passed.
func TestEnvtest_PreemptedVictimLeavesOnceItsLeaseIsProvablyDead(t *testing.T) {
	requireEnvtest(t)
	ns, r, cmd := redispatchFleet(t)
	cmd.cancelErr = errors.New("adapter unreachable")
	victim, _ := preemptWithUndeliveredHandover(t, ns, r, cmd)

	// Stand in for elapsed time: the frozen horizon is a stored instant.
	v := zrGetAction(t, victim)
	v.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	if err := envK8s.Status().Update(context.Background(), v); err != nil {
		t.Fatalf("age lease: %v", err)
	}
	redispatchReconcile(t, r, victim)
	if v := zrGetAction(t, victim); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q with its lease an hour dead, want Pending and unbound",
			v.Status.Phase, v.Status.AssignedRobot)
	}
}
