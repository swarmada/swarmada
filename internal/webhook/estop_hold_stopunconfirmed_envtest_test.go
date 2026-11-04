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

package webhook

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/scheduler"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Rows of the estop hold invariant about a cancel the adapter acknowledges with
// CANCEL_DISPOSITION_STOP_UNCONFIRMED: the stop was commanded and the robot's rest could not
// be witnessed. Such an acknowledgement does not prove the lease dead
// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee). On a requeue the
// action goes Failed, and the robot stays bound until the lease has provably expired; a stop
// that holds the robot is not shed by any of it.
//
// The lease is the shortest the API allows (10 s, skew 1 s), so a row waits out one lease.

const (
	suLease = 10 * time.Second
	suSkew  = 1 * time.Second
)

// suWire acknowledges every cancel with STOP_UNCONFIRMED and records every push.
type suWire struct{ wireLog }

func (w *suWire) PushCancelAction(ctx context.Context, ns, robot string, c command.CancelAction) (command.CancelActionOutcome, error) {
	_, _ = w.wireLog.PushCancelAction(ctx, ns, robot, c)
	return command.CancelActionOutcome{Acknowledged: true, Disposition: command.CancelStopUnconfirmed}, nil
}

// workWith reconciles FleetAction name once, as the control plane, pushing over c.
func (r *rig) workWith(c command.ActionCommander, name string) *fleetv1.FleetAction {
	r.t.Helper()
	fr := &controller.FleetActionReconciler{GenerationStoreNamespace: rigStoreNamespace,
		Client: r.w.as(r.t, controllerUser), Scheme: r.w.scheme, Scheduler: scheduler.NewDefaultScheduler(),
		Commander: c, Recorder: record.NewFakeRecorder(256),
	}
	_, _ = fr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: name}})
	return r.action(name)
}

// shortLease sets the namespace's lease to suLease and its skew to suSkew.
func (r *rig) shortLease() {
	r.t.Helper()
	cfg := r.configObj()
	r.must("shortening the lease", r.update(r.w.admin, cfg, false, func() {
		cfg.Spec.Scheduling.LeaseDurationSeconds = int32(suLease / time.Second)
		cfg.Spec.Scheduling.ClockSkewMarginSeconds = int32(suSkew / time.Second)
	}))
}

// holdWith gives robot an unpinned InProgress action with the given failure policy.
func (r *rig) holdWith(wl *wireLog, name, robot string, onFailure fleetv1.ActionFailurePolicy) {
	r.t.Helper()
	r.must("creating "+name, r.w.admin.Create(context.Background(), &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: name},
		Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: onFailure,
			// backoffSeconds 1, not 0: an omitted (zero) value takes the CRD default of 30 s,
			// and one second has long passed by the time a lease has expired.
			RetryPolicy: &fleetv1.ActionRetryPolicy{MaxRetries: 3, BackoffSeconds: 1}},
	}))
	fa := r.action(name)
	for i := 0; i < 20 && (fa.Status.Phase == "" || fa.Status.Phase == fleetv1.ActionPhasePending); i++ {
		fa = r.work(wl, name)
		if fa.Status.Phase == fleetv1.ActionPhasePending {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if fa.Status.Phase != fleetv1.ActionPhaseAssigned || fa.Status.AssignedRobot != robot {
		r.t.Fatalf("setup: %s was not assigned to %s: phase=%q assignedRobot=%q message=%q", name, robot,
			fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message)
	}
	a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
	r.must("marking "+name+" InProgress", r.update(r.w.as(r.t, controllerUser), a, true, func() {
		a.Status.Phase = fleetv1.ActionPhaseInProgress
	}))
}

// stopIntact reports whether the stop on robot for scope is still recorded where it was:
// the trigger annotation (robot scope) or the robot's estopHeldBy entry (zone, namespace),
// and an estopState of Stopped or Failed.
func (r *rig) stopIntact(robot, scope string) (bool, string) {
	rb := r.get(robot)
	state := rb.Status.EstopState
	inState := state == fleetv1.RobotEstopStopped || state == fleetv1.RobotEstopFailed
	recorded := len(rb.Status.EstopHeldBy) > 0
	if scope == "robot" {
		_, recorded = rb.Annotations["swarmada.io/estop-triggered"]
	}
	return inState && recorded, "estopState=" + string(state) + " estopHeldBy=" + holdsOf(rb.Status.EstopHeldBy)
}

// boundUntilLeaseDeath drives name with c across its lease and checks, at every pass, that
// the action never returns to Pending before the lease is provably dead, that robot stays
// bound to it until then, that nothing is assigned to robot, and (scope != "") that the stop
// on robot stays recorded. It returns once robot is released, failing if that does not
// happen within one renew interval of lease death. midway runs once, inside the window.
func (r *rig) boundUntilLeaseDeath(c *suWire, name, robot, scope string, midway func()) *fleetv1.FleetAction {
	r.t.Helper()
	fa := r.workWith(c, name)
	r.t.Logf("RESULT %s after the STOP_UNCONFIRMED acknowledgement: phase=%q failureReason=%q assignedRobot=%q "+
		"leaseExpiresAt=%v wire: %s", name, fa.Status.Phase, fa.Status.FailureReason, fa.Status.AssignedRobot,
		fa.Status.LeaseExpiresAt, c)
	if fa.Status.Phase != fleetv1.ActionPhaseFailed || fa.Status.LeaseExpiresAt == nil {
		r.t.Errorf("INVARIANT %s: an unconfirmed stop left the action %q (leaseExpiresAt %v), not Failed with "+
			"its lease horizon kept", name, fa.Status.Phase, fa.Status.LeaseExpiresAt)
	}
	horizon := time.Now().Add(suLease)
	if fa.Status.LeaseExpiresAt != nil {
		horizon = fa.Status.LeaseExpiresAt.Time
	}
	dead := horizon.Add(suSkew)
	renew := suLease / 3
	assignsBefore := c.count("assign", robot)
	ranMidway := false
	// A pass is bracketed by start and end. Something the controller did in the pass happened
	// at or after start and at or before end, so it is early only if end is before lease death,
	// and late only if start is a renew interval past it. leaseExpiresAt is stored to the
	// second, so a pass that straddles lease death proves neither.
	for passes := 0; ; passes++ {
		start := time.Now()
		fa = r.workWith(c, name)
		rb := r.get(robot)
		end := time.Now()
		if fa.Status.Phase == fleetv1.ActionPhasePending && end.Before(dead) {
			r.t.Errorf("INVARIANT %s: returned to Pending %.1fs before its lease was provably dead", name,
				dead.Sub(end).Seconds())
		}
		if n := c.count("assign", robot); n != assignsBefore {
			r.t.Errorf("INVARIANT %s: %d assignment(s) went to %s while it was bound to the unconfirmed stop",
				name, n-assignsBefore, robot)
			assignsBefore = n
		}
		if scope != "" {
			if ok, why := r.stopIntact(robot, scope); !ok {
				r.t.Errorf("INVARIANT %s: the %s stop on %s was shed (%s)", name, scope, robot, why)
			}
		}
		if rb.Status.AssignedAction != name {
			if end.Before(dead) {
				r.t.Errorf("INVARIANT %s: %s was released %.1fs before the lease was provably dead", name, robot,
					dead.Sub(end).Seconds())
			}
			r.t.Logf("RESULT %s released in a pass ending %.1fs after the lease was provably dead (%d passes): "+
				"phase=%q wire: %s", robot, end.Sub(dead).Seconds(), passes, fa.Status.Phase, c)
			return fa
		}
		if start.After(dead.Add(renew)) {
			r.t.Errorf("INVARIANT %s: %s still bound %.1fs after the lease was provably dead", name, robot,
				start.Sub(dead).Seconds())
			return fa
		}
		if !ranMidway && midway != nil && start.After(horizon.Add(-suLease/2)) {
			midway()
			ranMidway = true
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func stopUnconfirmedRows() []invariantRow {
	var rows []invariantRow

	// ── An operator requeues the work an estop paused; the cancel is acknowledged unconfirmed ──
	scoped := func(scope string, restart bool) func(t *testing.T, r *rig) {
		return func(t *testing.T, r *rig) {
			wl := &wireLog{}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.shortLease()
			r.robot("r1", "z")
			r.holdWith(wl, "a-held", "r1", fleetv1.ActionFailureAlert)
			r.robot("r2", "out")
			r.trigger(r.stopCarrier(scope), "stop")
			r.waitStopped("r1")
			if fa := r.work(wl, "a-held"); fa.Status.Phase != fleetv1.ActionPhasePaused {
				t.Fatalf("setup: a-held was not paused by the %s stop: %q", scope, fa.Status.Phase)
			}
			a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "a-held", Namespace: r.w.ns}}
			r.setAnnotation(a, "swarmada.io/requeue-requested", strp("aisle inspected"))
			su := &suWire{}
			var midway func()
			if restart {
				midway = func() {
					t.Logf("RESULT restarting the manager inside the hold")
					r.restart()
				}
			}
			// The stop holds r1 throughout, so the Failed action is held (A6) past its lease's death:
			// a dead lease does not release it while a stop holds the robot (A3).
			fa := r.heldPastLeaseDeath(su, "a-held", "r1", scope, midway)
			if fa.Status.Phase != fleetv1.ActionPhaseFailed || fa.Status.AssignedRobot != "r1" {
				t.Errorf("INVARIANT a-held: under the %s stop the action is %q bound to %q past its lease, not Failed on r1",
					scope, fa.Status.Phase, fa.Status.AssignedRobot)
			}
			if ok, why := r.stopIntact("r1", scope); !ok {
				t.Errorf("INVARIANT r1: the %s stop was shed by the release (%s)", scope, why)
			}
			if n := su.count("assign", "r2") + wl.count("assign", "r2"); n != 0 {
				t.Errorf("INVARIANT a-held: the work was assigned to r2 (%d)", n)
			}
			r.assertHeld("r1", true)
		}
	}
	for _, scope := range []string{"robot", "zone", "namespace"} {
		rows = append(rows, invariantRow{name: "requeue-stop-unconfirmed/" + scope, run: scoped(scope, false)})
	}
	rows = append(rows, invariantRow{name: "requeue-stop-unconfirmed/zone/manager-restart-mid-hold", run: scoped("zone", true)})

	// ── The same requeue with no stop and onFailure Requeue: Pending only after lease death ──
	rows = append(rows, invariantRow{name: "control-requeue-stop-unconfirmed/no-stop/onfailure-requeue", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.holdWith(wl, "a-held", "r1", fleetv1.ActionFailureRequeue)
		r.robot("r2", "out")
		a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "a-held", Namespace: r.w.ns}}
		r.setAnnotation(a, "swarmada.io/requeue-requested", strp("zone maintenance"))
		su := &suWire{}
		r.boundUntilLeaseDeath(su, "a-held", "r1", "", nil)
		var fa *fleetv1.FleetAction
		for i := 0; i < 10; i++ {
			fa = r.workWith(su, "a-held")
			if fa.Status.Phase == fleetv1.ActionPhaseAssigned {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("RESULT a-held after the release: phase=%q assignedRobot=%q retryCount=%d scheduledAt=%v "+
			"onFailure=%q retryPolicy=%+v message=%q wire: %s", fa.Status.Phase, fa.Status.AssignedRobot,
			fa.Status.RetryCount, fa.Status.ScheduledAt, fa.Spec.OnFailure, fa.Spec.RetryPolicy, fa.Status.Message, su)
		if fa.Status.RetryCount != 1 || fa.Status.Phase != fleetv1.ActionPhaseAssigned {
			t.Errorf("control: after lease death onFailure Requeue returns the action to Pending and it is "+
				"assigned again (phase %q, retryCount %d)", fa.Status.Phase, fa.Status.RetryCount)
		}
	}})

	// ── An adapter reports FAILED with the text StopUnconfirmed on an ordinary action ──
	rows = append(rows, invariantRow{name: "spoofed-failed-stopunconfirmed/no-stop", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.holdWith(wl, "a-spoof", "r1", fleetv1.ActionFailureAlert)
		r.robot("r2", "out")
		fa := r.action("a-spoof")
		ing := &controller.ActionStatusIngestor{Client: r.w.as(t, controllerUser)}
		allow := func(context.Context, string) bool { return true }
		gen := uint64(fa.Status.AssignmentGeneration)
		r.must("ingesting the spoofed report", ing.IngestActionStatus(context.Background(), r.w.ns, allow,
			&fav1.ActionStatusUpdate{ActionId: "a-spoof", State: fav1.ActionState_ACTION_STATE_FAILED,
				Message: "StopUnconfirmed", FencingToken: &gen}))
		su := &suWire{}
		fa = r.action("a-spoof")
		t.Logf("RESULT a-spoof after the spoofed report: phase=%q failureReason=%q leaseExpiresAt=%v",
			fa.Status.Phase, fa.Status.FailureReason, fa.Status.LeaseExpiresAt)
		if fa.Status.Phase != fleetv1.ActionPhaseFailed || fa.Status.FailureReason != "StopUnconfirmed" {
			t.Fatalf("setup: the spoofed report did not fail a-spoof with failureReason StopUnconfirmed")
		}
		// A second action, created while r1 is bound, goes to r2 as it would without the report.
		r.must("creating a2", r.w.admin.Create(context.Background(), &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "a2"},
			Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		}))
		var a2 *fleetv1.FleetAction
		for i := 0; i < 20; i++ {
			a2 = r.workWith(su, "a2")
			if a2.Status.Phase == fleetv1.ActionPhaseAssigned {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if a2.Status.Phase != fleetv1.ActionPhaseAssigned || a2.Status.AssignedRobot != "r2" {
			t.Errorf("INVARIANT a2: an unrelated action is %q on %q, not Assigned to r2 as without the report",
				a2.Status.Phase, a2.Status.AssignedRobot)
		}
		// The spoofed action holds only its own robot, for no longer than one lease plus skew.
		r.boundUntilLeaseDeath(su, "a-spoof", "r1", "", nil)
		fa = r.workWith(su, "a-spoof")
		if _, alerted := fa.Annotations["swarmada.io/failure-alerted"]; !alerted || fa.Status.Phase != fleetv1.ActionPhaseFailed {
			t.Errorf("INVARIANT a-spoof: after the release onFailure Alert applies as for any Failed action "+
				"(phase %q, alerted %v)", fa.Status.Phase, alerted)
		}
		if a2 = r.action("a2"); a2.Status.AssignedRobot != "r2" {
			t.Errorf("INVARIANT a2: the release of r1 moved an unrelated action (now on %q)", a2.Status.AssignedRobot)
		}
		if n := su.count("cancel", "r2") + su.count("cancel", "r1"); n != 0 {
			t.Errorf("INVARIANT: the spoofed report caused %d cancel_action", n)
		}
	}})
	return rows
}

// heldPastLeaseDeath drives name, Failed (StopUnconfirmed) on robot, through its lease's death and
// one renew interval past it while scope's stop holds robot, failing if robot is released or the
// stop shed on any pass: a stop holds the action (A6), and A3's release waits for the stop to go.
// midway runs once, inside the window.
func (r *rig) heldPastLeaseDeath(c *suWire, name, robot, scope string, midway func()) *fleetv1.FleetAction {
	r.t.Helper()
	fa := r.workWith(c, name)
	if fa.Status.Phase != fleetv1.ActionPhaseFailed || fa.Status.LeaseExpiresAt == nil {
		r.t.Errorf("INVARIANT %s: an unconfirmed stop left the action %q (leaseExpiresAt %v), not Failed with "+
			"its lease horizon kept", name, fa.Status.Phase, fa.Status.LeaseExpiresAt)
		return fa
	}
	horizon := fa.Status.LeaseExpiresAt.Time
	dead := horizon.Add(suSkew)
	renew := suLease / 3
	ranMidway := false
	for {
		start := time.Now()
		fa = r.workWith(c, name)
		rb := r.get(robot)
		if fa.Status.Phase != fleetv1.ActionPhaseFailed || fa.Status.AssignedRobot != robot || rb.Status.AssignedAction != name {
			r.t.Errorf("INVARIANT %s: released under the %s stop %.1fs from lease death: %q bound to %q; %s records %q",
				name, scope, start.Sub(dead).Seconds(), fa.Status.Phase, fa.Status.AssignedRobot, robot, rb.Status.AssignedAction)
			return fa
		}
		if ok, why := r.stopIntact(robot, scope); !ok {
			r.t.Errorf("INVARIANT %s: the %s stop on %s was shed (%s)", name, scope, robot, why)
		}
		if start.After(dead.Add(renew)) {
			r.t.Logf("RESULT %s still Failed on %s %.1fs after the lease was provably dead, under the %s stop",
				name, robot, start.Sub(dead).Seconds(), scope)
			return fa
		}
		if !ranMidway && midway != nil && start.After(horizon.Add(-suLease/2)) {
			midway()
			ranMidway = true
		}
		time.Sleep(500 * time.Millisecond)
	}
}
