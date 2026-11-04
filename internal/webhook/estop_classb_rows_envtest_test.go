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
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// movedUnderStopRows: work a stop covers is not moved by a requeue the stop's fan-out has not yet
// reached, by a requeue asked before a stop its robot never confirmed, or by a report from an
// earlier run of an action with the same name.

// report delivers an adapter's report of state for name, as a stream that serves every robot,
// carrying token when it is not nil.
func (r *rig) report(name string, state fav1.ActionState, token *uint64) {
	r.t.Helper()
	ing := &controller.ActionStatusIngestor{Client: r.w.as(r.t, controllerUser), Recorder: newEvents().rec}
	allow := func(context.Context, string) bool { return true }
	r.must("ingesting a report for "+name, ing.IngestActionStatus(context.Background(), r.w.ns, allow,
		&fav1.ActionStatusUpdate{ActionId: name, State: state, Message: "from the adapter", FencingToken: token}))
}

func movedUnderStopRows() []invariantRow {
	var rows []invariantRow

	// ── A requeue does not move work a stop covers before the stop's fan-out reaches the robot ──
	// z's stop is issued and the control plane has not yet written it onto r1 (no manager runs).
	// A requeue the manager writes (a ZoneMaintenance window, a capability loss), or one an
	// operator asked before the stop, ran on the robot's record alone, so x went to r2 while
	// z's stop stood.
	for _, who := range []string{"manager", "operator-before-the-stop"} {
		who := who
		rows = append(rows, invariantRow{name: "guard/requeue-under-an-issued-stop-does-not-move-work/" + who, opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			ev := newEvents()
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.robot("r1", "z")
			r.inProgressOn(wl, "x", "r1")
			r.robot("r2", "out")
			x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			if who == "operator-before-the-stop" {
				r.must("the operator requeues x, no stop standing", r.annotateAs(r.operator(), x, requeueAnnotation, "op"))
			}
			r.trigger(r.zoneObj("z"), "z-stop") // issued: no control plane runs, so nothing is written onto r1
			if who == "manager" {
				r.must("the manager writes a requeue on x", r.annotateAs(r.w.as(t, controllerUser), x, requeueAnnotation, "window"))
			}
			for i := 0; i < 10 && wl.count("assign", "r2") == 0; i++ {
				r.workEv(wl, ev, "x")
				time.Sleep(100 * time.Millisecond)
			}
			fa := r.action("x")
			t.Logf("RESULT x after a requeue under z's issued stop: %s; r1 %s; events: %s; wire: %s",
				r.actionLine("x"), r.guardState("r1"), ev, wl)
			if wl.count("assign", "r2") > 0 || fa.Status.Phase == fleetv1.ActionPhasePending || fa.Status.AssignedRobot != "r1" {
				t.Errorf("INVARIANT: a requeue moved x off r1 while z's stop, issued over r1, stood: %s", r.actionLine("x"))
				return
			}
			if n := ev.count("RequeueOvertakenByEstop"); n < 1 {
				t.Errorf("no RequeueOvertakenByEstop Event told the operator the request was not carried out: %s", ev)
			}
		}})
	}

	// ── Nothing else the control plane does moves work under a stop not yet written onto the robot ──
	// The same window: the completion timeout of r1's work and a ZoneMaintenance window over z each
	// moved work off r1 on the robot's record alone. A Critical action does not preempt r1's work:
	// dispatch withholds a robot under an issued stop before the preemption search sees it, and the
	// row pins that.
	rows = append(rows, invariantRow{name: "guard/issued-stop-holds-work-against/preemption", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.inProgressOn(wl, "x", "r1")
		r.trigger(r.zoneObj("z"), "z-stop")
		r.must("creating a Critical action", r.w.admin.Create(context.Background(), &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "crit"},
			Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Priority: fleetv1.ActionPriorityCritical},
		}))
		for i := 0; i < 5; i++ {
			r.workEv(wl, ev, "crit")
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("RESULT under z's issued stop: x %s; crit %s; wire: %s", r.actionLine("x"), r.actionLine("crit"), wl)
		if x := r.action("x"); x.Status.Phase != fleetv1.ActionPhaseInProgress || x.Status.AssignedRobot != "r1" ||
			wl.count("assign", "r1") > 1 {
			t.Errorf("INVARIANT: a preemption moved r1's work while z's stop, issued over r1, stood: x %s; crit %s",
				r.actionLine("x"), r.actionLine("crit"))
		}
	}})
	rows = append(rows, invariantRow{name: "guard/issued-stop-holds-work-against/completion-timeout", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		one := int32(1)
		r.must("creating x with a one-second timeout", r.w.admin.Create(context.Background(), &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "x"},
			Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, TimeoutSeconds: &one},
		}))
		r.assignedTo(wl, "x", "r1")
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("x InProgress, started a minute ago", r.update(r.w.as(t, controllerUser), x, true, func() {
			x.Status.Phase = fleetv1.ActionPhaseInProgress
			started := metav1.NewTime(time.Now().Add(-time.Minute))
			x.Status.StartedAt = &started
		}))
		r.trigger(r.zoneObj("z"), "z-stop")
		for i := 0; i < 5; i++ {
			r.workEv(wl, ev, "x")
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("RESULT x past its timeout under z's issued stop: %s; wire: %s", r.actionLine("x"), wl)
		if fa := r.action("x"); fa.Status.Phase != fleetv1.ActionPhaseInProgress || fa.Status.AssignedRobot != "r1" {
			t.Errorf("INVARIANT: the completion timeout ended r1's work while z's stop, issued over r1, stood: %s", r.actionLine("x"))
		}
	}})
	rows = append(rows, invariantRow{name: "guard/issued-stop-holds-work-against/zone-maintenance", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.inProgressOn(wl, "x", "r1")
		r.trigger(r.zoneObj("z"), "z-stop")
		r.window("w", fleetv1.ZoneMaintenanceModeImmediate, false)
		got := r.action("x").Annotations[requeueAnnotation]
		t.Logf("RESULT x after an Immediate window over z under z's issued stop: requeue annotation %q; %s", got, r.actionLine("x"))
		if got != "" {
			t.Errorf("INVARIANT: a ZoneMaintenance window asked a requeue of r1's work while z's stop, issued over r1, stood")
		}
	}})

	// ── The fencing tokens a robot is given rise across its actions ──
	// Each action's generation started at 1, so two actions given to r1 one after the other
	// carried the same token, and an adapter that fences per robot refused the second.
	rows = append(rows, invariantRow{name: "guard/a-robots-tokens-rise-across-its-actions", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.createAction("a")
		first := r.assignedTo(wl, "a", "r1").Status.AssignmentGeneration
		r.endOnServer("a", fleetv1.ActionPhaseSucceeded)
		r.waitFor("r1 free of a", 10*time.Second, func() (bool, string) {
			r.workEv(wl, ev, "a")
			got := r.get("r1").Status.AssignedAction
			return got == "", got
		})
		r.createAction("b")
		second := r.assignedTo(wl, "b", "r1").Status.AssignmentGeneration
		t.Logf("RESULT r1's tokens: a %d, then b %d", first, second)
		if second <= first {
			t.Errorf("INVARIANT: r1 was given b at token %d after a at token %d: a robot's tokens must rise", second, first)
		}
	}})

	// ── A report from an earlier run of a re-created action does not end the new one ──
	// x ran on r1 and was deleted; x was created again under the same name and given to r2. Its
	// assignment generation started again at the value the first x had used, so r1's late FAILED
	// report carried a token that matched: x on r2 was ended Failed and r2 freed while it ran.
	rows = append(rows, invariantRow{name: "guard/a-re-created-action-is-not-ended-by-its-earlier-run", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		first := r.inProgressOn(wl, "x", "r1")
		token := uint64(first.Status.AssignmentGeneration)
		r.must("the admin deletes x", r.w.admin.Delete(context.Background(), &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}))
		r.waitFor("r1's record of x released", 10*time.Second, func() (bool, string) {
			r.workEv(wl, ev, "x")
			got := r.get("r1").Status.AssignedAction
			return got == "", got
		})
		holdSetPhase(r.t, r.w, "r1", fleetv1.RobotPhaseOffline, false)
		r.robot("r2", "z")
		second := r.inProgressOn(wl, "x", "r2")
		t.Logf("RESULT generations: the first x %d, the second x %d", token, second.Status.AssignmentGeneration)
		if uint64(second.Status.AssignmentGeneration) <= token {
			t.Errorf("INVARIANT: the second x was given token %d, not above the first x's %d", second.Status.AssignmentGeneration, token)
		}
		r.report("x", fav1.ActionState_ACTION_STATE_FAILED, &token)
		fa := r.workEv(wl, ev, "x")
		t.Logf("RESULT the second x after r1's late FAILED at token %d: %s; r2 records %q", token, r.actionLine("x"),
			r.get("r2").Status.AssignedAction)
		if fa.Status.Phase != fleetv1.ActionPhaseInProgress || fa.Status.AssignedRobot != "r2" {
			t.Errorf("INVARIANT: a report from the first x's run on r1 ended the second x on r2: %s", r.actionLine("x"))
		}
	}})

	// ── A namespace's users cannot lower its generation store ──
	// The store sat in the namespace it served, where anyone who may write Leases there (stock
	// Kubernetes edit or admin, no Swarmada role) could lower it: the next action created under x's
	// name then drew the first x's token again, and the first x's late report ended it.
	rows = append(rows, invariantRow{name: "guard/a-namespace-editor-cannot-lower-the-generation-store", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		editor := r.leaseEditor()
		first := r.inProgressOn(wl, "x", "r1")
		token := uint64(first.Status.AssignmentGeneration)
		r.must("the admin deletes x", r.w.admin.Delete(context.Background(), &fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}))
		r.waitFor("r1's record of x released", 10*time.Second, func() (bool, string) {
			r.workEv(wl, ev, "x")
			got := r.get("r1").Status.AssignedAction
			return got == "", got
		})
		t.Logf("RESULT the editor lowers every generation store it can reach: %s", r.lowerStores(editor, int64(token)-1))
		holdSetPhase(r.t, r.w, "r1", fleetv1.RobotPhaseOffline, false)
		r.robot("r2", "z")
		second := r.inProgressOn(wl, "x", "r2")
		r.report("x", fav1.ActionState_ACTION_STATE_FAILED, &token)
		fa := r.workEv(wl, ev, "x")
		t.Logf("RESULT the second x got token %d after the first x's %d; after the first x's late FAILED: %s",
			second.Status.AssignmentGeneration, token, r.actionLine("x"))
		if uint64(second.Status.AssignmentGeneration) <= token {
			t.Errorf("INVARIANT: a namespace editor made the second x reuse token %d", second.Status.AssignmentGeneration)
		}
		if fa.Status.Phase != fleetv1.ActionPhaseInProgress || fa.Status.AssignedRobot != "r2" {
			t.Errorf("INVARIANT: after a namespace editor lowered the store, the first x's report ended the second x: %s", r.actionLine("x"))
		}
	}})

	// ── A stop issued over a robot holds the stop finalizer on it and on its work at once ──
	// The finalizers followed the robot's record alone, so between a stop's issue and its fan-out
	// neither r1 nor x carried one.
	rows = append(rows, invariantRow{name: "guard/issued-stop-holds-the-stop-finalizer", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.inProgressOn(wl, "x", "r1")
		r.trigger(r.zoneObj("z"), "z-stop") // issued: no control plane runs, so nothing is written onto r1
		r.workEv(wl, ev, "x")
		re := &controller.RobotEstopReconciler{Client: r.w.as(t, controllerUser), Scheme: r.w.scheme,
			Estopper: &confirmingEstopper{c: r.w.as(t, controllerUser)}, APIReader: r.w.as(t, controllerUser),
			Recorder: newEvents().rec}
		_, _ = re.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: "r1"}})
		x, r1 := r.action("x"), r.get("r1")
		t.Logf("RESULT under z's issued stop: x finalizers %v; r1 finalizers %v", x.Finalizers, r1.Finalizers)
		if !hasStopFinalizer(x) || !hasStopFinalizer(r1) {
			t.Errorf("INVARIANT: a stop issued over r1 did not hold the stop finalizer on x (%v) and r1 (%v)",
				hasStopFinalizer(x), hasStopFinalizer(r1))
		}
	}})

	// ── A report without a fencing token does not move an action ──
	// The report names no robot; with no token nothing ties it to the run it came from, and it was
	// applied as current.
	rows = append(rows, invariantRow{name: "guard/a-report-without-a-fencing-token-is-refused", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.inProgressOn(wl, "x", "r1")
		r.report("x", fav1.ActionState_ACTION_STATE_FAILED, nil)
		fa := r.workEv(wl, ev, "x")
		t.Logf("RESULT x after a FAILED report with no token: %s; r1 records %q", r.actionLine("x"), r.get("r1").Status.AssignedAction)
		if fa.Status.Phase != fleetv1.ActionPhaseInProgress || fa.Status.AssignedRobot != "r1" {
			t.Errorf("INVARIANT: a report with no fencing token ended x: %s", r.actionLine("x"))
		}
	}})

	// ── A requeue asked before a stop the robot never confirmed does not move started work ──
	// r1 never answers the stop. x was started on r1 and an operator with no estop-clear asked for a
	// requeue before the stop. Once r1's lease was provably dead the request ran as if a holder of
	// estop-clear had asked it under the stop (Revoking), or after another hold rewrote the message
	// that recorded when it was asked (Paused): x went to r2 with r1's stop standing.
	for _, hold := range []string{"revoking", "paused"} {
		hold := hold
		rows = append(rows, invariantRow{name: "guard/requeue-before-an-unconfirmed-stop-does-not-move-started-work/" + hold, run: func(t *testing.T, r *rig) {
			dw := &deafWire{}
			ev := newEvents()
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.shortLease()
			r.robot("r1", "z")
			r.setMode("r1", ackNone)
			r.inProgressOn(dw, "x", "r1")
			x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			if hold == "revoking" {
				r.revoke(&dw.wireLog, "x", "r1")
			} else {
				r.must("desiredState Paused", r.update(r.w.admin, x, false, func() { x.Spec.DesiredState = fleetv1.DesiredStatePaused }))
				r.workWith(dw, "x")
			}
			r.must("the operator requeues x, no stop standing", r.annotateAs(r.operator(), x, requeueAnnotation, "op"))
			r.workEv(dw, ev, "x")
			t.Logf("RESULT x after the operator's requeue: %s", r.actionLine("x"))
			dead := r.action("x").Status.LeaseExpiresAt.Add(suSkew)
			r.robot("r2", "out")
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitFor("r1 Failed", 20*time.Second, func() (bool, string) {
				s := r.get("r1").Status.EstopState
				return s == fleetv1.RobotEstopFailed, string(s)
			})
			for end := dead.Add(4 * time.Second); time.Now().Before(end) && dw.count("assign", "r2") == 0; time.Sleep(300 * time.Millisecond) {
				r.workEv(dw, ev, "x")
			}
			t.Logf("RESULT x past r1's lease with z's stop standing: %s; r1 %s; events: %s", r.actionLine("x"), r.guardState("r1"), ev)
			if dw.count("assign", "r2") > 0 && r.zoneStands("z") {
				t.Errorf("INVARIANT: a requeue asked before z's stop moved work r1 had started to r2 while the stop stands (wire: %s)", &dw.wireLog)
				return
			}
			if n := ev.count("ActionAwaitingOperatorDecision") + ev.count("RequeueHeldByEstop"); n < 1 {
				t.Errorf("no Event told the operator the work waits for a decision: %s", ev)
			}
		}})
	}
	return rows
}

// leaseEditor binds a role that may read and write Leases in the row's namespace, and nothing else,
// to a user of the row's own, and returns a client acting as that user: what stock Kubernetes edit
// or admin grants a namespace's users.
func (r *rig) leaseEditor() client.Client {
	r.t.Helper()
	user := "lease-editor-" + r.w.ns
	role := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]interface{}{"name": "lease-editor", "namespace": r.w.ns},
		"rules": []interface{}{map[string]interface{}{"apiGroups": []interface{}{"coordination.k8s.io"},
			"resources": []interface{}{"leases"}, "verbs": []interface{}{"get", "list", "create", "update", "patch", "delete"}}},
	}}
	binding := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]interface{}{"name": "lease-editor", "namespace": r.w.ns},
		"roleRef":  map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "lease-editor"},
		"subjects": []interface{}{map[string]interface{}{"kind": "User", "name": user, "apiGroup": "rbac.authorization.k8s.io"}},
	}}
	r.must("creating the lease-editor role", r.w.admin.Create(context.Background(), role))
	r.must("binding the lease-editor role", r.w.admin.Create(context.Background(), binding))
	return asIdentity(r.t, r.w, user)
}

// lowerStores sets, as c, the mark of every generation store the row's namespace could have (in the
// namespace itself, and in the control plane's namespace) to mark, and says what each write did.
func (r *rig) lowerStores(c client.Client, mark int64) string {
	r.t.Helper()
	var out []string
	for _, key := range []types.NamespacedName{
		{Namespace: r.w.ns, Name: "swarmada-fencing-tokens"},
		{Namespace: rigStoreNamespace, Name: "fencing-tokens." + r.w.ns},
	} {
		l := &unstructured.Unstructured{}
		l.SetGroupVersionKind(schema.GroupVersionKind{Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"})
		err := c.Get(context.Background(), key, l)
		if err == nil {
			ann := l.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann["swarmada.io/fencing-high-water"] = strconv.FormatInt(mark, 10)
			l.SetAnnotations(ann)
			err = c.Update(context.Background(), l)
		}
		res := "lowered"
		if err != nil {
			res = err.Error()
		}
		out = append(out, key.String()+": "+res)
	}
	return strings.Join(out, "; ")
}
