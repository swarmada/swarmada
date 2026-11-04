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

// Which exit a Preempted action takes.
//
// A Preempted action leaves for Pending only once its former robot is PROVEN off it: the
// adapter acknowledges a cancel_action for it, or its frozen lease is provably dead
// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee, the Preemption
// row; RFC-0001 #tde-preemption-algorithm). What the robot's status.assignedAction says is
// not that proof, and the two preemption paths show why:
//
//   - ROBOT preemption: the preemptor's own commit rewrites the robot's
//     status.assignedAction before any adapter hears of it. Releasing on that write
//     releases on the plane's own decision, with the robot possibly still running the
//     victim under a live lease.
//   - ZONE-RESERVATION preemption by the TDE: the preemptor takes the victim's zone slot
//     on a different robot, and nothing rewrites the victim robot's record at all. Waiting
//     for it to change held the victim, and its robot, without bound.
//
// So on both paths a reachable robot is sent cancel_action, and a robot that is lost is
// bounded by the lease.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The decision table itself, for the Preempted phase: a reachable robot, whatever it
// records, is asked to confirm the stop; for a lost robot the lease decides.
func TestEvaluateLease_PreemptedAsksAReachableRobotToConfirmTheStop(t *testing.T) {
	now := time.Now()
	dead := now.Add(-time.Hour)
	alive := now.Add(time.Hour)
	for _, tc := range []struct {
		name  string
		reach reachability
		lease *time.Time
		want  leaseAction
	}{
		{"still claimed, lease alive", robotExecuting, &alive, actionConfirmStop},
		{"still claimed, lease an hour dead", robotExecuting, &dead, actionConfirmStop}, // the gate reads the lease
		{"still claimed, no lease", robotExecuting, nil, actionConfirmStop},
		{"record switched off it", robotFree, &alive, actionConfirmStop}, // the plane's own write is not proof
		{"lost, lease alive", robotLost, &alive, actionHold},
		{"lost, lease dead", robotLost, &dead, actionReassign}, // condition 3
	} {
		if got := evaluateLease(fleetv1.ActionPhasePreempted, tc.reach, tc.lease, now, defaultLeaseClockSkew); got != tc.want {
			t.Errorf("%s: evaluateLease = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ROBOT preemption path. No Commander is wired, so the adapter never hears of the
// preemptor and never confirms anything. The preemptor's own write to the robot does not
// release the victim: it holds, bound, until a confirmation or the lease horizon.
func TestPreempted_RobotPreemption_VictimWaitsForAConfirmedStopNotThePlanesOwnWrite(t *testing.T) {
	r, c := preemptScenario(t, tde.ReservationResult{Status: tde.Granted})

	reconcileAction(t, r, "crit")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s, want Preempted", v.Status.Phase)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "crit" {
		t.Fatalf("robot assignedAction = %q, want crit — the preemptor's commit writes it", rob.Status.AssignedAction)
	}

	reconcileAction(t, r, "norm")
	v := getAction(t, c, "norm")
	if v.Status.Phase != fleetv1.ActionPhasePreempted || v.Status.AssignedRobot != "r1" {
		t.Fatalf("victim phase=%s robot=%q with its lease live and no confirmation; want held Preempted on r1",
			v.Status.Phase, v.Status.AssignedRobot)
	}

	// The adapter confirms the cancel: now it leaves.
	cmd := &fakeCommander{cancelAck: true}
	r.Commander = cmd
	reconcileAction(t, r, "norm")
	v = getAction(t, c, "norm")
	if v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q after a confirmed cancel; want Pending and unbound",
			v.Status.Phase, v.Status.AssignedRobot)
	}
	if cmd.cancelsFor("norm", "r1") != 1 || cmd.cancels[0].Cause != command.CancelCauseSuperseded {
		t.Fatalf("cancels = %+v to %v; want one SUPERSEDED cancel of the victim to r1", cmd.cancels, cmd.cancelTo)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "crit" {
		t.Fatalf("r1 assignedAction = %q; releasing the victim must not unbind the preemptor", rob.Status.AssignedAction)
	}
}

// zoneVictimScenario builds the ZONE-RESERVATION preemption path over the fake client: a
// Normal victim InProgress on r1 in zone z, an Idle r2 in zone z, and a Critical action in
// zone z whose reservation the TDE grants by evicting the victim's slot.
func zoneVictimScenario(t *testing.T) (*FleetActionReconciler, *fakeTDE, func(string) *fleetv1.FleetAction) {
	t.Helper()
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	crit := criticalPending("crit")
	crit.Spec.Zone = "z"
	victim := bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease)
	victim.Spec.Zone = "z"
	r1 := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm")
	r1.Spec.Zone, r1.Status.CurrentZone = "z", "z"
	r, c := newActionReconciler(t, crit, victim, r1, idleRobotInZone("r2", "z"))
	ft := &fakeTDE{result: tde.ReservationResult{Status: tde.PreemptedGranted, PreemptedActionIDs: []string{"norm"}}}
	r.TDE = ft
	return r, ft, func(n string) *fleetv1.FleetAction { return getAction(t, c, n) }
}

// ZONE-RESERVATION path, no adapter confirmation. The victim holds Preempted with r1
// bound only while its frozen lease is live; once the lease is provably dead it is
// released, r1 returns to Idle, and r1 takes work again.
func TestPreempted_ZoneReservationPreemption_ReleasesAtTheLeaseHorizon(t *testing.T) {
	r, ft, get := zoneVictimScenario(t)
	c := r.Client

	reconcileAction(t, r, "crit")
	if crit := get("crit"); crit.Status.AssignedRobot != "r2" {
		t.Fatalf("critical assigned to %q, want r2 (the Idle robot); this is not the zone path", crit.Status.AssignedRobot)
	}
	if v := get("norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s, want Preempted by the TDE eviction", v.Status.Phase)
	}

	// Freeze check: a hold reconcile does not renew the lease, writes nothing, and comes
	// back no later than the renew interval.
	frozen := get("norm").Status.LeaseExpiresAt.DeepCopy()
	if res := reconcileAction(t, r, "norm"); res.RequeueAfter <= 0 || res.RequeueAfter > defaultLeaseDuration/3 {
		t.Fatalf("hold requeue = %s, want within the %s renew interval", res.RequeueAfter, defaultLeaseDuration/3)
	}
	if got := get("norm"); !got.Status.LeaseExpiresAt.Equal(frozen) || got.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("phase=%s lease %s→%s while Preempted with a live lease; want held with the lease frozen",
			got.Status.Phase, frozen, got.Status.LeaseExpiresAt)
	}

	// An adapter report does not move a Preempted action.
	ing := &ActionStatusIngestor{Client: c}
	if err := ing.IngestActionStatus(context.Background(), actionNS, allowAnyRobot, &fav1.ActionStatusUpdate{
		ActionId: "norm", State: fav1.ActionState_ACTION_STATE_SUCCEEDED,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if p := get("norm").Status.Phase; p != fleetv1.ActionPhasePreempted {
		t.Fatalf("an adapter SUCCEEDED report moved the victim to %s", p)
	}

	// Stand in for elapsed time: the frozen lease is a stored instant, so moving it an hour
	// past the horizon is the same state the victim reaches by waiting.
	v := get("norm")
	base := v.DeepCopy()
	v.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	if err := c.Status().Patch(context.Background(), v, client.MergeFrom(base)); err != nil {
		t.Fatalf("age lease: %v", err)
	}
	reconcileAction(t, r, "norm")
	if v := get("norm"); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q with its lease an hour dead; want Pending and unbound",
			v.Status.Phase, v.Status.AssignedRobot)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "" || rob.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("r1 assignedAction=%q phase=%s; want released to Idle", rob.Status.AssignedAction, rob.Status.Phase)
	}

	// r1 takes work again.
	ft.result = tde.ReservationResult{Status: tde.Granted}
	next := pinnedAction("next", "r1")
	next.Spec.Zone = "z"
	if err := c.Create(context.Background(), next); err != nil {
		t.Fatalf("create next: %v", err)
	}
	reconcileAction(t, r, "next")
	if n := get("next"); n.Status.AssignedRobot != "r1" {
		t.Fatalf("next assigned to %q; r1 is free and should take it", n.Status.AssignedRobot)
	}
}

// ZONE-RESERVATION path, adapter confirms: the victim's robot is sent cancel_action, and
// on the acknowledgement the victim leaves and r1 is freed, well inside the lease.
func TestPreempted_ZoneReservationPreemption_ReleasesOnAConfirmedCancel(t *testing.T) {
	r, _, get := zoneVictimScenario(t)
	reconcileAction(t, r, "crit")
	cmd := &fakeCommander{cancelAck: true}
	r.Commander = cmd

	reconcileAction(t, r, "norm")
	if v := get("norm"); v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q after a confirmed cancel; want Pending and unbound",
			v.Status.Phase, v.Status.AssignedRobot)
	}
	if cmd.cancelsFor("norm", "r1") != 1 || cmd.cancels[0].Cause != command.CancelCauseUnspecified {
		t.Fatalf("cancels = %+v to %v; want one cancel of the victim to r1 with no cause stated", cmd.cancels, cmd.cancelTo)
	}
	if rob := getRobot(t, r.Client, "r1", actionNS); rob.Status.AssignedAction != "" || rob.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("r1 assignedAction=%q phase=%s; want released to Idle", rob.Status.AssignedAction, rob.Status.Phase)
	}
}

// The zone-reservation release against a real API server, so the robot binding and the
// frozen lease are read back through the status subresource rather than asserted on a fake.
func TestEnvtest_Preempted_ZoneReservationPreemption_ReleasesAtTheLeaseHorizon(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	ft := &fakeTDE{result: tde.ReservationResult{Status: tde.Granted}}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), TDE: ft}

	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "z", "", nil, []fleetv1.EffectiveZoneRestriction{}, true)
	f12SeedRobot(t, ns, "r1", "z")
	f12SeedRobot(t, ns, "r2", "z")
	seedBattery(t, ns, "r1", 90) // the victim is dispatched first, and takes the higher battery
	seedBattery(t, ns, "r2", 50)

	victim := zrSeedAction(t, ns, "norm", "z")
	if _, err := r.Reconcile(ctx, victim); err != nil {
		t.Fatalf("dispatch victim: %v", err)
	}
	if v := zrGetAction(t, victim); v.Status.AssignedRobot != "r1" {
		t.Fatalf("victim assigned to %q, want r1", v.Status.AssignedRobot)
	}

	crit := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "crit", Namespace: ns},
		Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z",
			Priority: fleetv1.ActionPriorityCritical},
	}
	if err := envK8s.Create(ctx, crit); err != nil {
		t.Fatalf("create crit: %v", err)
	}
	critReq := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "crit"}}
	ft.result = tde.ReservationResult{Status: tde.PreemptedGranted, PreemptedActionIDs: []string{"norm"}}
	if _, err := r.Reconcile(ctx, critReq); err != nil {
		t.Fatalf("dispatch crit: %v", err)
	}
	if c := zrGetAction(t, critReq); c.Status.AssignedRobot != "r2" {
		t.Fatalf("crit assigned to %q, want r2", c.Status.AssignedRobot)
	}
	ft.result = tde.ReservationResult{Status: tde.Granted}
	if _, err := r.Reconcile(ctx, victim); err != nil {
		t.Fatalf("reconcile victim, lease live: %v", err)
	}
	v := zrGetAction(t, victim)
	if v.Status.Phase != fleetv1.ActionPhasePreempted || v.Status.AssignedRobot != "r1" {
		t.Fatalf("victim phase=%s robot=%q with its lease live, want held Preempted on r1",
			v.Status.Phase, v.Status.AssignedRobot)
	}

	v.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	if err := envK8s.Status().Update(ctx, v); err != nil {
		t.Fatalf("age lease: %v", err)
	}
	if _, err := r.Reconcile(ctx, victim); err != nil {
		t.Fatalf("reconcile victim, lease dead: %v", err)
	}
	v = zrGetAction(t, victim)
	if v.Status.Phase != fleetv1.ActionPhasePending || v.Status.AssignedRobot != "" {
		t.Fatalf("victim phase=%s robot=%q with the lease an hour dead, want Pending and unbound",
			v.Status.Phase, v.Status.AssignedRobot)
	}
	var r1 fleetv1.Robot
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "r1"}, &r1); err != nil {
		t.Fatalf("get r1: %v", err)
	}
	if r1.Status.AssignedAction != "" || r1.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("r1 assignedAction=%q phase=%s, want released to Idle", r1.Status.AssignedAction, r1.Status.Phase)
	}
}
