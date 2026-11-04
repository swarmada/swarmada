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
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// arbiterRows: while a stop covers the robot that holds the work, the work's record survives
// every delete path, and only a holder of estop-clear on the stop decides where it goes.

// pausedHeld gives r1 (in z) InProgress work x, stops z, waits for the pause, and puts r2 outside z.
func (r *rig) pausedHeld(cmd *wireLog) {
	r.t.Helper()
	r.zone("z", "", nil)
	r.zone("out", "", nil)
	r.config()
	r.robot("r1", "z")
	r.inProgressOn(cmd, "x", "r1")
	r.trigger(r.zoneObj("z"), "z-stop")
	r.waitStopped("r1")
	r.waitMarker(r.zoneObj("z"), true)
	r.work(cmd, "x")
	r.robot("r2", "out")
}

// awaitsTheArbiter requires the arbiter's two signs on action name, held under stop: its status
// message says it awaits a holder of estop-clear on that stop, and a Warning Event with reason
// ActionAwaitingOperatorDecision was recorded on the action itself.
func awaitsTheArbiter(t *testing.T, r *rig, ev *events, name, stop string) {
	t.Helper()
	if msg := r.action(name).Status.Message; !strings.HasPrefix(msg, "awaiting operator decision (estop-clear on "+stop+"): ") {
		t.Errorf("INVARIANT: %s's message does not say it awaits a holder of estop-clear on %s: %q", name, stop, msg)
	}
	if !ev.recordedOn("ActionAwaitingOperatorDecision", "FleetAction/"+name) {
		t.Errorf("INVARIANT: no ActionAwaitingOperatorDecision Warning Event on FleetAction %s: %s", name, ev)
	}
}

func hasStopFinalizer(obj client.Object) bool {
	return controllerutil.ContainsFinalizer(obj, estopscope.StopFinalizer)
}

func arbiterRows() []invariantRow {
	var rows []invariantRow

	// ── Deleting held work: refused to the operator, kept for the admin ──
	for _, who := range []string{"operator", "admin"} {
		who := who
		rows = append(rows, invariantRow{name: "guard/held-action-delete/" + who, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			ev := newEvents()
			r.pausedHeld(wl)
			x := r.workEv(wl, ev, "x")
			if !hasStopFinalizer(x) {
				t.Errorf("INVARIANT: x, recorded by r1 under z's stop, does not hold the stop finalizer: %v", x.Finalizers)
			}
			if !hasStopFinalizer(r.get("r1")) {
				t.Errorf("INVARIANT: r1, recording x under z's stop, does not hold the stop finalizer: %v", r.get("r1").Finalizers)
			}
			c := r.w.admin
			if who == "operator" {
				c = r.operator()
			}
			err := c.Delete(context.Background(), &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}})
			t.Logf("RESULT %s deletes x, held by r1 under z's stop: %v", who, err)
			if who == "operator" {
				if err == nil {
					t.Errorf("INVARIANT: the operator, who holds no estop-clear on z, deleted held work")
				} else if msg := err.Error(); !strings.Contains(msg, "estop-clear") || !strings.Contains(msg, "kubectl annotate fleetaction x") ||
					!strings.Contains(msg, "swarmctl cancel action x") {
					t.Errorf("the refusal does not name who decides and the commands: %v", err)
				}
				// Removing the finalizer by hand is the same decision.
				fa := r.action("x")
				err := r.update(c, fa, false, func() { controllerutil.RemoveFinalizer(fa, estopscope.StopFinalizer) })
				if err == nil {
					t.Errorf("INVARIANT: the operator removed the stop finalizer of held work")
				}
				return
			}
			if err != nil {
				t.Fatalf("control: the admin, who holds estop-clear on z, was refused: %v", err)
			}
			// Admitted, the action is kept: Terminating, its name taken, its robot's record kept.
			for i := 0; i < 5; i++ {
				r.workEv(wl, ev, "x")
				time.Sleep(200 * time.Millisecond)
			}
			fa := &fleetv1.FleetAction{}
			if err := r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: "x"}, fa); err != nil {
				t.Fatalf("INVARIANT: held work deleted by the admin is gone while z's stop stands: %v", err)
			}
			t.Logf("RESULT x after the admin's delete: deleting=%v %s; events: %s", fa.DeletionTimestamp != nil, r.actionLine("x"), ev)
			awaitsTheArbiter(t, r, ev, "x", "zone/z")
			err = r.w.admin.Create(context.Background(), &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns},
				Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}})
			if err == nil || !apierrors.IsAlreadyExists(err) {
				t.Errorf("INVARIANT: x was created again while the deleted, held x is kept: %v", err)
			}
			if got := r.get("r1").Status.AssignedAction; got != "x" {
				t.Errorf("INVARIANT: r1's record of x was released while z's stop stands (records %q)", got)
			}
			if wl.count("assign", "r2") > 0 {
				t.Errorf("INVARIANT: held work reached r2 (wire: %s)", wl)
			}
			// Once the stop is cleared and r1 resumed, the action goes, and r1's record of it is
			// released on its adapter's acknowledgement.
			r.clearStop(r.zoneObj("z"))
			r.waitMarker(r.zoneObj("z"), false)
			r.resume("r1", "after-delete")
			gone := false
			for i := 0; i < 50 && !gone; i++ {
				r.workEv(wl, ev, "x")
				gone = apierrors.IsNotFound(r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: "x"}, &fleetv1.FleetAction{}))
				time.Sleep(200 * time.Millisecond)
			}
			if !gone {
				t.Errorf("the deleted x stayed after the stop was cleared and r1 resumed: %s; r1 %s", r.actionLine("x"), r.guardState("r1"))
			}
			for i := 0; i < 20 && r.get("r1").Status.AssignedAction != ""; i++ {
				r.workEv(wl, ev, "x")
				time.Sleep(200 * time.Millisecond)
			}
			if rb := r.get("r1"); rb.Status.AssignedAction != "" || hasStopFinalizer(rb) {
				t.Errorf("r1 still holds the record or the finalizer after the clear and the acknowledgement: %s", r.guardState("r1"))
			}
		}})
	}

	// ── The Pending work behind a covered recorder names who decides ──
	// x, held Paused while Assigned (spec.desiredState: Paused), is released from r1, whose adapter
	// never acknowledges the cancel the hold sends, so r1 keeps its record; then z's stop covers r1.
	// The stop's authority requeues x, and nothing proves r1 off x: x waits in Pending behind r1
	// (the executor gate), and says so. (A stop alone no longer releases the binding: row A1.)
	rows = append(rows, invariantRow{name: "guard/held-work-awaits-the-arbiter", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		dw := &deafWire{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.robot("r1", "z")
		r.createAction("x")
		r.assignedTo(wl, "x", "r1")
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("the admin holds x", r.update(r.w.admin, x, false, func() { x.Spec.DesiredState = fleetv1.DesiredStatePaused }))
		for i := 0; i < 10 && r.action("x").Status.Phase != fleetv1.ActionPhasePaused; i++ {
			r.workEv(dw, ev, "x")
		}
		if fa := r.action("x"); fa.Status.AssignedRobot != "" || r.get("r1").Status.AssignedAction != "x" {
			t.Fatalf("setup: the hold did not release x from r1 with r1's record kept: %s; r1 %s", r.actionLine("x"), r.guardState("r1"))
		}
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj("z"), true)
		r.robot("r2", "out")
		x = &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("the admin requeues x", r.annotateAs(r.w.admin, x, requeueAnnotation, "transfer"))
		for i := 0; i < 10; i++ {
			r.workEv(dw, ev, "x")
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("RESULT x after its requeue under z's stop: %s; r1 %s; events: %s; wire: %s", r.actionLine("x"), r.guardState("r1"), ev, &dw.wireLog)
		if got := r.get("r1").Status.AssignedAction; got != "x" {
			t.Fatalf("setup: r1 does not keep its record of x under z's stop (records %q)", got)
		}
		if p := r.action("x").Status.Phase; p != fleetv1.ActionPhasePending {
			t.Fatalf("setup: the requeued x is not Pending behind r1 (phase %q): the executor gate was not reached", p)
		}
		if wl.count("assign", "r2")+dw.count("assign", "r2") > 0 {
			t.Errorf("INVARIANT: held work reached r2 (wire: %s)", &dw.wireLog)
		}
		if !hasStopFinalizer(r.action("x")) {
			t.Errorf("INVARIANT: x, recorded by r1 under z's stop, does not hold the stop finalizer")
		}
		awaitsTheArbiter(t, r, ev, "x", "zone/z")
	}})
	return rows
}
