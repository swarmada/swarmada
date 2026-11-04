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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// recordKeyRows: the robot's record of an action (status.assignedAction) is the executor
// guard's key and the one link from work to the stop that paused it; each row is a way that
// key used to be lost while the reason for it stood. And a zone stop over a tree that two
// concurrent edits left past the bound stops every robot it cannot place, never none.

// actionLine is one line describing an action for a row's log.
func (r *rig) actionLine(name string) string {
	fa := r.action(name)
	lease := "<nil>"
	if fa.Status.LeaseExpiresAt != nil {
		lease = fa.Status.LeaseExpiresAt.UTC().Format("15:04:05")
	}
	return fmt.Sprintf("phase=%q assignedRobot=%q lease=%s message=%q", fa.Status.Phase, fa.Status.AssignedRobot, lease,
		fa.Status.Message)
}

func recordKeyRows() []invariantRow {
	var rows []invariantRow

	// ── A robot that records an action it is no longer bound to is not deleted ──
	// x is paused while Assigned to r1, whose adapter never acknowledges the cancel: the binding
	// is released and r1 keeps its record, the executor guard's only key. Deleting r1 would take
	// the key with it, and a requeue would give x to r2 while r1 may still run it.
	rows = append(rows, invariantRow{name: "guard/recorder-robot-is-not-deleted/delete", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		fm := r.fleetManager()
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.createAction("x")
		fa := r.assignedTo(dw, "x", "r1")
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("desiredState Paused", r.update(r.w.admin, x, false, func() { x.Spec.DesiredState = fleetv1.DesiredStatePaused }))
		r.workWith(dw, "x")
		t.Logf("RESULT x after the hold: %s; r1 %s", r.actionLine("x"), r.guardState("r1"))
		if r.get("r1").Status.AssignedAction != "x" {
			t.Fatalf("setup: r1 does not record x after the hold")
		}
		r.robot("r2", "out")
		err := fm.Delete(context.Background(), r.robotObj("r1"))
		t.Logf("RESULT the fleet-manager deletes r1, which records x: %v", err)
		if err == nil {
			t.Errorf("INVARIANT: the fleet-manager deleted r1 while it records x and may still run it")
			r.waitGone(r.robotObj("r1"))
		} else if !strings.Contains(err.Error(), `"x"`) {
			t.Errorf("the refusal does not name the action: %v", err)
		}
		if err := r.w.admin.Delete(context.Background(), r.robotObj("r1")); err == nil {
			t.Errorf("INVARIANT: the admin deleted r1 while it records x and may still run it")
		}
		r.setAnnotation(x, requeueAnnotation, strp("admin"))
		if r.noSecondExecutorBefore(dw, &dw.wireLog, "x", "r1", "r2", dead, 3*time.Second) && time.Now().Before(dead) {
			t.Errorf("INVARIANT: x went to r2 before r1's lease was provably dead")
		}
	}})

	// ── The last finalizer of a robot that records an action ──
	// The delete check has failurePolicy Ignore: a delete admitted while no manager replica served
	// it leaves a terminating robot, kept by a finalizer, that may still record an action. The
	// state is written as the control plane would have left it; removing the last finalizer would
	// complete the delete and take the executor guard's key with it.
	rows = append(rows, invariantRow{name: "guard/recorder-robot-is-not-deleted/last-finalizer", run: func(t *testing.T, r *rig) {
		fm := r.fleetManager()
		cp := r.w.as(t, controllerUser)
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		rb := r.robotObj("r1")
		r.must("fleet-manager adds a finalizer", r.update(fm, rb, false, func() {
			rb.Finalizers = append(rb.Finalizers, "example.com/keep")
		}))
		r.must("deleting r1 while it records nothing", fm.Delete(context.Background(), r.robotObj("r1")))
		r.createAction("x")
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("x paused with its binding released", r.update(cp, x, true, func() { x.Status.Phase = fleetv1.ActionPhasePaused }))
		rb = r.robotObj("r1")
		r.must("r1 records x", r.update(cp, rb, true, func() { rb.Status.AssignedAction = "x" }))
		t.Logf("RESULT r1 %s; x %s", r.guardState("r1"), r.actionLine("x"))
		rb = r.robotObj("r1")
		err := r.update(fm, rb, false, func() { rb.Finalizers = nil })
		t.Logf("RESULT the fleet-manager removes r1's last finalizer while it records x: %v", err)
		if err == nil {
			t.Errorf("INVARIANT: r1's last finalizer was removed while it records x and may still run it")
		} else if !strings.Contains(err.Error(), `"x"`) {
			t.Errorf("the refusal does not name the action: %v", err)
		}
	}})

	// ── A declarative Paused on an InProgress action whose adapter does not answer ──
	// The message says what the code does: the cancel is sent once and not repeated, the lease is
	// no longer renewed to the robot, and the action stays bound until an operator acts.
	rows = append(rows, invariantRow{name: "guard/paused-inprogress-deaf-adapter-is-told-once", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		r.zone("z", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.createAction("x")
		r.assignedTo(dw, "x", "r1")
		a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("marking x InProgress", r.update(r.w.as(t, controllerUser), a, true, func() { a.Status.Phase = fleetv1.ActionPhaseInProgress }))
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("desiredState Paused", r.update(r.w.admin, x, false, func() { x.Spec.DesiredState = fleetv1.DesiredStatePaused }))
		renewsBefore := dw.count("renew", "r1")
		for i := 0; i < 5; i++ {
			r.workWith(dw, "x")
			time.Sleep(300 * time.Millisecond)
		}
		fa := r.action("x")
		t.Logf("RESULT after five passes: %s; wire: %s", r.actionLine("x"), dw)
		if n := dw.count("cancel", "r1"); n != 1 {
			t.Errorf("the hold sent r1 %d cancels; the message says once", n)
		}
		if n := dw.count("renew", "r1") - renewsBefore; n != 0 {
			t.Errorf("the lease was renewed to r1 %d times after the hold; the message says it is not", n)
		}
		if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" {
			t.Errorf("x is %q bound to %q, not Paused on r1", fa.Status.Phase, fa.Status.AssignedRobot)
		}
		for _, want := range []string{"sent once and not repeated", "stops on its own lease", "until an operator requeues or cancels it"} {
			if !strings.Contains(fa.Status.Message, want) {
				t.Errorf("the message does not say %q: %q", want, fa.Status.Message)
			}
		}
		if strings.Contains(fa.Status.Message, "provably expired") {
			t.Errorf("the message promises a lease expiry the server keeps renewing: %q", fa.Status.Message)
		}
	}})

	// ── A connected adapter that never acknowledges the cancel: the requeue waits out the lease ──
	// x was InProgress on r1 when z's stop paused it; r1's stop is confirmed. A requeue by a holder of
	// estop-clear sends cancel_action and waits: a confirmed stop is not proof the robot dropped x.
	// While the requeue waits the lease is no longer renewed (a Paused action's lease is never renewed
	// to the robot), so the requeue completes once that lease is provably dead, and not before.
	rows = append(rows, invariantRow{name: "guard/paused-work-of-a-silent-adapter-moves-once-its-lease-dies", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.createAction("x")
		r.assignedTo(dw, "x", "r1")
		a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("marking x InProgress", r.update(r.w.as(t, controllerUser), a, true, func() { a.Status.Phase = fleetv1.ActionPhaseInProgress }))
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj("z"), true)
		r.workWith(dw, "x")
		r.robot("r2", "out")
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("the admin requeues x", r.annotateAs(r.w.admin, x, requeueAnnotation, "admin"))
		fa := r.workWith(dw, "x")
		t.Logf("RESULT x after the requeue, r1 Stopped and its adapter silent: %s; r1 %s", r.actionLine("x"), r.guardState("r1"))
		if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" || fa.Status.LeaseExpiresAt == nil {
			t.Fatalf("setup: x is not Paused on r1 with a lease after the requeue: %s", r.actionLine("x"))
		}
		if !strings.Contains(fa.Status.Message, "awaiting confirmed stop") {
			t.Errorf("the requeue's hold does not say it awaits the stop: %q", fa.Status.Message)
		}
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		if !r.noSecondExecutorBefore(dw, &dw.wireLog, "x", "r1", "r2", dead, 4*time.Second) {
			t.Errorf("x never left r1 after its lease died: %s; r1 %s", r.actionLine("x"), r.guardState("r1"))
		}
		if n := dw.count("renew", "r1"); n != 0 {
			t.Errorf("the lease was renewed to r1 %d times while x was paused", n)
		}
	}})

	// ── A robot bound to an action is not deleted while it may still run it ──
	// The action keeps the horizon while it exists, but it can be deleted and created again
	// under its name (a FleetTask member is): with r1 gone as well, nothing records that r1 may
	// still run it, and the new object went to r2 at once.
	rows = append(rows, invariantRow{name: "guard/recorder-robot-is-not-deleted/bound-then-action-recreated", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		fm := r.fleetManager()
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.createAction("x")
		fa := r.assignedTo(dw, "x", "r1")
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		r.robot("r2", "out")
		err := fm.Delete(context.Background(), r.robotObj("r1"))
		t.Logf("RESULT the fleet-manager deletes r1, bound to x: %v", err)
		if err == nil {
			t.Errorf("INVARIANT: the fleet-manager deleted r1 while it is bound to x and may still run it")
			r.waitGone(r.robotObj("r1"))
		} else if !strings.Contains(err.Error(), `"x"`) {
			t.Errorf("the refusal does not name the action: %v", err)
		}
		r.must("deleting x", r.w.admin.Delete(context.Background(), r.action("x")))
		r.waitGone(&fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "x"}})
		r.createAction("x")
		if r.noSecondExecutorBefore(dw, &dw.wireLog, "x", "r1", "r2", dead, 2*time.Second) && time.Now().Before(dead) {
			t.Errorf("INVARIANT: x, created again, went to r2 before r1's lease was provably dead")
		}
	}})

	// ── A stop over a tree two concurrent re-parents left past the bound ──
	// Each re-parent alone keeps every zone inside the 64-ancestor bound; issued together, each
	// webhook judges its edit against the tree without the other, and the tree ends 90 deep.
	// The stop of ar must still reach ra (inside the bound) and rk (past it): a robot whose chain
	// cannot be walked is stopped with the stop, not skipped with the whole pass.
	rows = append(rows, invariantRow{name: "guard/concurrent-reparents-past-the-bound-still-stop", run: func(t *testing.T, r *rig) {
		chain := func(root, prefix string, n int) {
			r.zone(root, "", nil)
			parent := root
			for i := 1; i <= n; i++ {
				name := fmt.Sprintf("%s%d", prefix, i)
				r.zone(name, parent, nil)
				parent = name
			}
		}
		chain("xr", "x", 39)
		chain("ar", "a", 24)
		chain("cr", "c", 25)
		r.zone("out", "", nil)
		r.robot("rk", "c25")
		r.hold("a-rk", "rk", true)
		r.robot("ra", "a24")
		r.hold("a-ra", "ra", true)
		depth := func(zone string) string {
			var zl fleetv1.FleetZoneList
			r.must("listing zones", r.w.admin.List(context.Background(), &zl, client.InNamespace(r.w.ns)))
			d, err := estopscope.TreeOf(zl.Items).Depth(zone)
			return fmt.Sprintf("depth(%s)=%d err=%v", zone, d, err)
		}
		both := false
		for attempt := 1; attempt <= 40 && !both; attempt++ {
			var wg sync.WaitGroup
			start := make(chan struct{})
			var errA, errC error
			wg.Add(2)
			go func() {
				defer wg.Done()
				z := r.zoneObj("ar")
				<-start
				errA = r.update(r.w.admin, z, false, func() { z.Spec.ParentZone = "x39" })
			}()
			go func() {
				defer wg.Done()
				z := r.zoneObj("cr")
				<-start
				errC = r.update(r.w.admin, z, false, func() { z.Spec.ParentZone = "a24" })
			}()
			close(start)
			wg.Wait()
			both = errA == nil && errC == nil
			if !both {
				for name, err := range map[string]error{"ar": errA, "cr": errC} {
					if err == nil {
						z := r.zoneObj(name)
						r.must("undo "+name, r.update(r.w.admin, z, false, func() { z.Spec.ParentZone = "" }))
					}
				}
				time.Sleep(100 * time.Millisecond)
			} else {
				t.Logf("RESULT both re-parents admitted on attempt %d: %s", attempt, depth("c25"))
			}
		}
		if !both {
			t.Skipf("the two re-parents were never admitted together in 40 attempts (%s); the unit row pins the fan-out", depth("c25"))
		}
		r.trigger(r.zoneObj("ar"), "ar-stop")
		r.waitMarker(r.zoneObj("ar"), true)
		r.assertHeld("ra", true, "a-ra")
		r.assertHeld("rk", true, "a-rk")
		t.Logf("RESULT ra %s; rk %s", r.guardState("ra"), r.guardState("rk"))
		// The tree is repaired once the stop is cleared: an edit that returns it inside the bound
		// is admitted with no stop standing in the namespace.
		cr := r.zoneObj("cr")
		if err := r.update(r.w.admin, cr, false, func() { cr.Spec.ParentZone = "" }); err == nil {
			t.Errorf("INVARIANT: a re-parent under a tree past the bound was admitted while ar's stop stands")
		}
		r.clearStop(r.zoneObj("ar"))
		r.waitMarker(r.zoneObj("ar"), false)
		cr = r.zoneObj("cr")
		if err := r.update(r.w.admin, cr, false, func() { cr.Spec.ParentZone = "" }); err != nil {
			t.Errorf("the edit that returns the tree inside the bound was refused with no stop standing: %v", err)
		}
		t.Logf("RESULT after the repair: %s", depth("c25"))
	}})

	// ── A requeue of work a stop paused while Assigned needs the stop's own authority ──
	// The confirmed stop on an Assigned action keeps the binding and the robot's record (row A1),
	// so the requeue check finds the stop. The admin, who holds estop-clear on z, moves it: x goes
	// to r2 once r1 acknowledges the cancel the requeue sends (A2).
	for _, who := range []string{"operator", "fleet-manager", "admin"} {
		who := who
		rows = append(rows, invariantRow{name: "guard/requeue-of-estop-paused-assigned-work-needs-estop-clear/" + who, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.robot("r1", "z")
			r.createAction("x")
			r.assignedTo(wl, "x", "r1")
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitStopped("r1")
			r.waitMarker(r.zoneObj("z"), true)
			fa := r.work(wl, "x")
			if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" {
				t.Fatalf("setup: x is not Paused and still bound to r1: %s", r.actionLine("x"))
			}
			if got := r.get("r1").Status.AssignedAction; got != "x" {
				t.Errorf("INVARIANT: r1's record of x was released while z's stop stands (records %q)", got)
			}
			r.robot("r2", "out")
			var c client.Client
			switch who {
			case "operator":
				c = r.operator()
			case "fleet-manager":
				c = r.fleetManager()
			default:
				c = r.w.admin
			}
			x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			err := r.annotateAs(c, x, requeueAnnotation, who)
			t.Logf("RESULT %s requeues x, paused by z's stop while Assigned: %v", who, err)
			if who != "admin" {
				if err == nil {
					t.Errorf("INVARIANT: %s, who holds no estop-clear on z, requeued work z's stop paused", who)
				} else if !strings.Contains(err.Error(), "estop-clear") || !strings.Contains(err.Error(), `"z"`) {
					t.Errorf("the refusal does not name the estop-clear verb and the stop: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("control: the admin, who holds estop-clear on z, was refused: %v", err)
			}
			for i := 0; i < 30 && wl.count("assign", "r2") == 0; i++ {
				r.work(wl, "x")
				time.Sleep(100 * time.Millisecond)
			}
			if wl.count("assign", "r2") == 0 {
				t.Errorf("control: the admin's requeue did not give x to r2: %s; r1 %s; wire: %s", r.actionLine("x"), r.guardState("r1"), wl)
			}
		}})
	}

	// ── A member a stop paused is not moved by a cancel, a delete and its regeneration ──
	// The operator holds cancel and delete on fleetactions, not estop-clear. Cancelling the
	// paused member and deleting it, or deleting it outright, made the task create it again and
	// gave it to r2 while z's stop stood. The operator's delete of held work is refused; the
	// admin's is admitted and the member is kept, Terminating, so the task cannot create it
	// again; once the stop is cleared it goes, and the member created again is given to a robot.
	for _, how := range []string{"cancel-then-delete", "delete"} {
		how := how
		rows = append(rows, invariantRow{name: "guard/estop-paused-member-is-not-moved-by-its-regeneration/" + how, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.robot("r1", "z")
			tr := &controller.FleetTaskReconciler{Client: r.w.as(t, controllerUser), Scheme: r.w.scheme}
			r.must("creating task h", r.w.admin.Create(context.Background(), &fleetv1.FleetTask{
				ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "h"},
				Spec: fleetv1.FleetTaskSpec{Actions: []fleetv1.FleetTaskAction{
					{Name: "t1", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: fleetv1.ActionFailureAlert}},
				}},
			}))
			reconcileTask := func() {
				_, _ = tr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: "h"}})
			}
			member := func() (*fleetv1.FleetAction, bool) {
				var got fleetv1.FleetAction
				if err := r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: "h-t1"}, &got); err != nil {
					return nil, false
				}
				// reconciled, a deleted member whose stop is cleared goes: nil, false then
				fa := r.workEv(wl, newEvents(), "h-t1")
				return fa, fa != nil
			}
			for i := 0; i < 30; i++ {
				reconcileTask()
				if fa, ok := member(); ok && fa.Status.Phase == fleetv1.ActionPhaseAssigned {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "h-t1", Namespace: r.w.ns}}
			r.must("marking h-t1 InProgress", r.update(r.w.as(t, controllerUser), a, true, func() { a.Status.Phase = fleetv1.ActionPhaseInProgress }))
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitStopped("r1")
			r.waitMarker(r.zoneObj("z"), true)
			if fa, _ := member(); fa == nil || fa.Status.Phase != fleetv1.ActionPhasePaused {
				t.Fatalf("setup: the member is not Paused: %s", r.actionLine("h-t1"))
			}
			r.robot("r2", "out")
			op := r.operator()
			if how == "cancel-then-delete" {
				r.must("operator cancels the paused member", r.annotateAs(op, a, cancelRequestedAnnotation, "op"))
				for i := 0; i < 30; i++ {
					if fa, _ := member(); fa != nil && fa.Status.Phase == fleetv1.ActionPhaseCancelled {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				t.Logf("RESULT the member after the cancel: %s; r1 %s", r.actionLine("h-t1"), r.guardState("r1"))
			}
			if err := op.Delete(context.Background(), &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "h-t1", Namespace: r.w.ns}}); err == nil {
				t.Errorf("INVARIANT: the operator, who holds no estop-clear on z, deleted the held member")
			} else if !strings.Contains(err.Error(), "estop-clear") {
				t.Errorf("the refusal does not name the estop-clear verb: %v", err)
			}
			r.must("admin deletes the member", r.w.admin.Delete(context.Background(), &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "h-t1", Namespace: r.w.ns}}))
			for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
				reconcileTask()
				member()
			}
			fa, kept := member()
			t.Logf("RESULT the deleted member under z's stop: kept=%v %s; r1 %s; wire: %s", kept, r.actionLine("h-t1"), r.guardState("r1"), wl)
			if !kept || fa.DeletionTimestamp == nil {
				t.Errorf("INVARIANT: the deleted held member is not kept Terminating while z's stop stands (kept=%v)", kept)
			}
			if wl.count("assign", "r2") > 0 {
				t.Errorf("INVARIANT: work z's stop paused was given to r2 while the stop stands, by a cancel, a delete and the task's regeneration")
			}
			if msg := r.action("h-t1").Status.Message; !strings.Contains(msg, "r1") || !strings.Contains(msg, "z") {
				t.Errorf("the hold does not name the robot and the stop: %q", msg)
			}
			// Once the stop is cleared, the regenerated member is given to a robot.
			r.clearStop(r.zoneObj("z"))
			r.waitMarker(r.zoneObj("z"), false)
			if how == "cancel-then-delete" {
				// The member was cancelled, and its task ended with it: once the stop no longer holds
				// r1 the deleted member goes, and nothing creates it again.
				gone := false
				for i := 0; i < 50 && !gone; i++ {
					reconcileTask()
					_, ok := member()
					gone = !ok
					time.Sleep(200 * time.Millisecond)
				}
				if !gone || wl.count("assign", "r2") > 0 {
					t.Errorf("after the clear the cancelled, deleted member is gone=%v; wire: %s", gone, wl)
				}
				return
			}
			for i := 0; i < 50 && wl.count("assign", "r2") == 0; i++ {
				reconcileTask()
				member()
				time.Sleep(200 * time.Millisecond)
			}
			if wl.count("assign", "r2") == 0 {
				t.Errorf("after the clear the regenerated member was not given to r2: r1 %s; wire: %s", r.guardState("r1"), wl)
			}
		}})
	}

	// ── The post-clear window: the stop is cleared, the robot is Resuming ──
	// The operator's requeue is admitted once the clear is applied (the robot is no longer under
	// a stop). Work the stop paused must still not reach a second robot before the first is
	// provably off it: its adapter acknowledged a cancel, or its lease is provably dead.
	for _, phase := range []string{"inprogress", "assigned"} {
		phase := phase
		rows = append(rows, invariantRow{name: "guard/requeue-in-the-post-clear-window-deaf-adapter/" + phase, run: func(t *testing.T, r *rig) {
			dw := &deafWire{}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.shortLease()
			r.robot("r1", "z")
			r.createAction("x")
			fa := r.assignedTo(dw, "x", "r1")
			if phase == "inprogress" {
				a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
				r.must("marking x InProgress", r.update(r.w.as(t, controllerUser), a, true, func() { a.Status.Phase = fleetv1.ActionPhaseInProgress }))
			}
			dead := fa.Status.LeaseExpiresAt.Add(suSkew)
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitStopped("r1")
			r.waitMarker(r.zoneObj("z"), true)
			r.workWith(dw, "x")
			t.Logf("RESULT x after z's stop: %s; r1 %s", r.actionLine("x"), r.guardState("r1"))
			r.robot("r2", "out")
			r.clearStop(r.zoneObj("z"))
			r.waitMarker(r.zoneObj("z"), false)
			r.waitFor("r1 Resuming", 10*time.Second, func() (bool, string) {
				s := r.get("r1").Status.EstopState
				return s == fleetv1.RobotEstopResuming, string(s)
			})
			r.workWith(dw, "x")
			x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			err := r.annotateAs(r.operator(), x, requeueAnnotation, "operator")
			t.Logf("RESULT the operator's requeue in the post-clear window: %v; x %s; r1 %s", err, r.actionLine("x"), r.guardState("r1"))
			if err != nil {
				return
			}
			if r.noSecondExecutorBefore(dw, &dw.wireLog, "x", "r1", "r2", dead, 3*time.Second) && time.Now().Before(dead) {
				t.Errorf("INVARIANT: x went to r2 in the post-clear window before r1's lease was provably dead")
			}
		}})
	}
	return rows
}
