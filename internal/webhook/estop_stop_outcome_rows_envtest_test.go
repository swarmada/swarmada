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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/scheduler"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// stopOutcomeRows: what a stop makes of work its robot reports failed, of a FleetAction status
// write from a stale copy, of a robot whose recorded action was deleted, of a requeue that
// arrives by create or before the stop, and of work whose robot never confirmed its stop.

// events collects the Events a fake recorder was given, and what each was recorded on.
type events struct {
	mu  sync.Mutex
	rec *targetRecorder
	got []string
}

func newEvents() *events {
	return &events{rec: &targetRecorder{FakeRecorder: record.NewFakeRecorder(1024)}}
}

// targetRecorder is a fake recorder that also keeps the object each Event was recorded on, as
// "<reason> <Kind>/<name>": the fake's text names no object.
type targetRecorder struct {
	*record.FakeRecorder
	mu      sync.Mutex
	targets []string
}

func (r *targetRecorder) note(obj runtime.Object, reason string) {
	kind, name := fmt.Sprintf("%T", obj), ""
	switch o := obj.(type) {
	case *fleetv1.FleetAction:
		kind, name = "FleetAction", o.Name
	case *fleetv1.FleetTask:
		kind, name = "FleetTask", o.Name
	case *fleetv1.Robot:
		kind, name = "Robot", o.Name
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = append(r.targets, reason+" "+kind+"/"+name)
}

func (r *targetRecorder) Event(obj runtime.Object, eventtype, reason, message string) {
	r.note(obj, reason)
	r.FakeRecorder.Event(obj, eventtype, reason, message)
}

func (r *targetRecorder) Eventf(obj runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.note(obj, reason)
	r.FakeRecorder.Eventf(obj, eventtype, reason, messageFmt, args...)
}

func (r *targetRecorder) AnnotatedEventf(obj runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.note(obj, reason)
	r.FakeRecorder.AnnotatedEventf(obj, annotations, eventtype, reason, messageFmt, args...)
}

// recordedOn reports whether an Event with reason was recorded on the object kind/name.
func (e *events) recordedOn(reason, target string) bool {
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	for _, s := range e.rec.targets {
		if s == reason+" "+target {
			return true
		}
	}
	return false
}

func (e *events) drain() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		select {
		case s := <-e.rec.Events:
			e.got = append(e.got, s)
		default:
			return
		}
	}
}

// count is how many Events carry reason.
func (e *events) count(reason string) int {
	e.drain()
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, s := range e.got {
		if strings.Contains(s, " "+reason+" ") {
			n++
		}
	}
	return n
}

func (e *events) String() string {
	e.drain()
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.got, " | ")
}

// workEv reconciles FleetAction name once, as the control plane, pushing over c and recording
// Events into ev.
func (r *rig) workEv(c command.ActionCommander, ev *events, name string) *fleetv1.FleetAction {
	r.t.Helper()
	fr := &controller.FleetActionReconciler{GenerationStoreNamespace: rigStoreNamespace,
		Client: r.w.as(r.t, controllerUser), Scheme: r.w.scheme, Scheduler: scheduler.NewDefaultScheduler(),
		Commander: c, Recorder: ev.rec,
	}
	_, _ = fr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: name}})
	var fa fleetv1.FleetAction
	if err := r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: name}, &fa); err != nil {
		return nil
	}
	return &fa
}

// reportFailed delivers an adapter's ACTION_STATE_FAILED for name, as the stream serving its robot.
func (r *rig) reportFailed(ev *events, name, reason string) {
	r.t.Helper()
	fa := r.action(name)
	ing := &controller.ActionStatusIngestor{Client: r.w.as(r.t, controllerUser), Recorder: ev.rec}
	allow := func(context.Context, string) bool { return true }
	gen := uint64(fa.Status.AssignmentGeneration)
	r.must("ingesting FAILED", ing.IngestActionStatus(context.Background(), r.w.ns, allow,
		&fav1.ActionStatusUpdate{ActionId: name, State: fav1.ActionState_ACTION_STATE_FAILED, Message: reason, FencingToken: &gen}))
}

// inProgressOn gives robot an unpinned InProgress action over cmd, with startedAt as the robot's
// running report writes it.
func (r *rig) inProgressOn(cmd command.ActionCommander, name, robot string) *fleetv1.FleetAction {
	r.t.Helper()
	r.createAction(name)
	fa := r.assignedTo(cmd, name, robot)
	a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
	r.must("marking "+name+" InProgress", r.update(r.w.as(r.t, controllerUser), a, true, func() {
		a.Status.Phase = fleetv1.ActionPhaseInProgress
		started := metav1.Now()
		a.Status.StartedAt = &started
	}))
	return fa
}

func (r *rig) zoneStands(zone string) bool {
	var z fleetv1.FleetZone
	if err := r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: zone}, &z); err != nil {
		return false
	}
	return z.GetAnnotations()[estopTriggeredAnnotation] != ""
}

// staleReads returns snap for every read of the action it names: the controller's cache lags
// the API server by one write.
type staleReads struct {
	client.Client
	name string
	mu   sync.Mutex
	snap *fleetv1.FleetAction
}

func (s *staleReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if fa, ok := obj.(*fleetv1.FleetAction); ok && key.Name == s.name {
		s.mu.Lock()
		snap := s.snap
		s.mu.Unlock()
		if snap != nil {
			snap.DeepCopyInto(fa)
			return nil
		}
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// workStale reconciles name once with the controller reading snap for it.
func (r *rig) workStale(cmd command.ActionCommander, snap *fleetv1.FleetAction, name string) {
	r.t.Helper()
	s := &staleReads{Client: r.w.as(r.t, controllerUser), name: name, snap: snap}
	fr := &controller.FleetActionReconciler{GenerationStoreNamespace: rigStoreNamespace, Client: s, Scheme: r.w.scheme, Scheduler: scheduler.NewDefaultScheduler(),
		Commander: cmd, Recorder: record.NewFakeRecorder(256)}
	_, _ = fr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: name}})
}

// endOnServer writes a terminal phase onto name as the ingestor would.
func (r *rig) endOnServer(name string, phase fleetv1.ActionPhase) {
	r.t.Helper()
	a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
	r.must("ending "+name+" on the server", r.update(r.w.as(r.t, controllerUser), a, true, func() {
		now := metav1.Now()
		a.Status.Phase = phase
		a.Status.CompletionTime = &now
		if phase == fleetv1.ActionPhaseFailed {
			a.Status.FailedAt = &now
			a.Status.FailureReason = "the adapter reported a failure"
		}
	}))
}

// completedWire acknowledges a cancel as Completed: the action had already finished.
type completedWire struct{ wireLog }

func (w *completedWire) PushCancelAction(ctx context.Context, ns, robot string, c command.CancelAction) (command.CancelActionOutcome, error) {
	_, _ = w.wireLog.PushCancelAction(ctx, ns, robot, c)
	return command.CancelActionOutcome{Acknowledged: true, Disposition: command.CancelCompleted}, nil
}

func terminalPhase(p fleetv1.ActionPhase) bool {
	return p == fleetv1.ActionPhaseSucceeded || p == fleetv1.ActionPhaseFailed || p == fleetv1.ActionPhaseCancelled
}

func stopOutcomeRows() []invariantRow {
	var rows []invariantRow

	// ── A failure an adapter reports while a stop covers its robot is a stop outcome ──
	// The stop closed the adapter's gate and the adapter reported the action failed. With the
	// default onFailure Requeue the action went Pending and then to r2 while r1's stop stood.
	rows = append(rows, invariantRow{name: "guard/failure-under-a-stop-is-a-stop-outcome/requeue", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.robot("r2", "out")
		r.holdWith(wl, "x", "r1", fleetv1.ActionFailureRequeue)
		r.trigger(r.robotObj("r1"), "r1-stop")
		r.waitStopped("r1")
		r.reportFailed(ev, "x", "aborted: a stop closed the gate")
		fa := r.action("x")
		t.Logf("RESULT x after the adapter's FAILED under r1's stop: %s; events: %s", r.actionLine("x"), ev)
		if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" {
			t.Errorf("INVARIANT: a failure reported under a stop is not a stop outcome: x is %q bound to %q, not Paused on r1",
				fa.Status.Phase, fa.Status.AssignedRobot)
		}
		for _, want := range []string{"aborted: a stop closed the gate", "estop-clear"} {
			if !strings.Contains(fa.Status.Message, want) {
				t.Errorf("the message does not say %q: %q", want, fa.Status.Message)
			}
		}
		if fa.Status.FailureReason != "" || fa.Status.FailedAt != nil {
			t.Errorf("a Paused action carries failure fields: failureReason=%q failedAt=%v", fa.Status.FailureReason, fa.Status.FailedAt)
		}
		if n := ev.count("ActionFailedUnderEstop"); n < 1 {
			t.Errorf("no ActionFailedUnderEstop Warning Event: %s", ev)
		}
		var seen []string
		for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
			fa := r.workEv(wl, ev, "x")
			seen = append(seen, string(fa.Status.Phase))
			if terminalPhase(fa.Status.Phase) || fa.Status.Phase == fleetv1.ActionPhasePending {
				t.Errorf("INVARIANT: x left Paused under the stop: %s", r.actionLine("x"))
				break
			}
		}
		if wl.count("assign", "r2") > 0 {
			t.Errorf("INVARIANT: x, failed under r1's stop, was retried on r2 while the stop stands (wire: %s)", wl)
		}
		t.Logf("RESULT phases while the stop stands: %v; wire: %s", seen, wl)
	}})

	// The same failure on a FailFast task's member: the task does not fail, and a cancel of the
	// task reaches the member.
	rows = append(rows, invariantRow{name: "guard/failure-under-a-stop-is-a-stop-outcome/failfast-task", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		tr := &controller.FleetTaskReconciler{Client: r.w.as(t, controllerUser), Scheme: r.w.scheme, Recorder: ev.rec}
		r.must("creating task h", r.w.admin.Create(context.Background(), &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "h"},
			Spec: fleetv1.FleetTaskSpec{FailurePolicy: fleetv1.FailurePolicyFailFast, Actions: []fleetv1.FleetTaskAction{
				{Name: "t1", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}},
			}},
		}))
		reconcileTask := func() *fleetv1.FleetTask {
			_, _ = tr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: "h"}})
			var task fleetv1.FleetTask
			r.must("reading h", r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: "h"}, &task))
			return &task
		}
		for i := 0; i < 40; i++ {
			reconcileTask()
			var m fleetv1.FleetAction
			if r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: "h-t1"}, &m) == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		r.assignedTo(wl, "h-t1", "r1")
		a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "h-t1", Namespace: r.w.ns}}
		r.must("marking h-t1 InProgress", r.update(r.w.as(t, controllerUser), a, true, func() { a.Status.Phase = fleetv1.ActionPhaseInProgress }))
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj("z"), true)
		r.reportFailed(ev, "h-t1", "aborted: a stop closed the gate; the robot was stopped and teleop no longer reaches it")
		r.workEv(wl, ev, "h-t1")
		task := reconcileTask()
		t.Logf("RESULT after the member's FAILED under z's stop: member %s; task phase=%q; events: %s", r.actionLine("h-t1"), task.Status.Phase, ev)
		if task.Status.Phase == fleetv1.FleetTaskPhaseFailed {
			t.Errorf("INVARIANT: a failure reported under a stop failed the task (FailFast fired on a stop outcome)")
		}
		if p := r.action("h-t1").Status.Phase; p != fleetv1.ActionPhasePaused {
			t.Errorf("INVARIANT: the member is %q, not Paused", p)
		}
		if n := ev.count("ActionFailedUnderEstop"); n < 2 {
			t.Errorf("want the Warning Event on the action and on its FleetTask, got %d: %s", n, ev)
		}
		// The documented recovery: cancel the task. The member ends Cancelled and so does the task.
		hk := &fleetv1.FleetTask{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: r.w.ns}}
		r.must("cancelling task h", r.update(r.w.admin, hk, false, func() { hk.Spec.DesiredState = fleetv1.DesiredStateCancelled }))
		for i := 0; i < 30; i++ {
			reconcileTask()
			r.workEv(wl, ev, "h-t1")
			if terminalPhase(r.action("h-t1").Status.Phase) && isTaskTerminal(reconcileTask().Status.Phase) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		task = reconcileTask()
		t.Logf("RESULT after the task's cancel: member %s; task phase=%q", r.actionLine("h-t1"), task.Status.Phase)
		if p := r.action("h-t1").Status.Phase; p != fleetv1.ActionPhaseCancelled {
			t.Errorf("the task's cancel did not reach the member: %q", p)
		}
		if task.Status.Phase != fleetv1.FleetTaskPhaseCancelled {
			t.Errorf("the task is %q after its cancel, not Cancelled", task.Status.Phase)
		}
	}})

	// ── No FleetAction leaves a terminal phase ──
	// The controller computes a write from its cached copy while the API server already holds a
	// terminal phase the ingestor wrote: the pause path wrote Paused over Failed, a requeue wrote
	// Pending over Succeeded, entering Revoking wrote Revoking over Failed.
	for _, c := range []struct{ name, setup string }{
		{"failed-then-estop-pause", "pause"}, {"succeeded-then-requeue", "requeue"}, {"failed-then-robot-lost", "lost"},
	} {
		c := c
		rows = append(rows, invariantRow{name: "guard/no-terminal-phase-is-left/" + c.name, run: func(t *testing.T, r *rig) {
			var cmd command.ActionCommander
			wl := &wireLog{}
			cmd = wl
			r.zone("z", "", nil)
			r.config()
			r.shortLease()
			r.robot("r1", "z")
			want := fleetv1.ActionPhaseFailed
			switch c.setup {
			case "pause":
				r.inProgressOn(wl, "x", "r1")
				r.trigger(r.zoneObj("z"), "z-stop")
				r.waitStopped("r1")
				r.waitMarker(r.zoneObj("z"), true)
			case "requeue":
				cw := &completedWire{}
				cmd = cw
				r.inProgressOn(cw, "x", "r1")
				xa := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
				r.must("requeue-requested", r.annotateAs(r.w.admin, xa, requeueAnnotation, "admin"))
				want = fleetv1.ActionPhaseSucceeded
			case "lost":
				r.inProgressOn(wl, "x", "r1")
				holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseOffline, false)
			}
			snap := r.action("x")
			r.endOnServer("x", want)
			for i := 0; i < 3; i++ {
				r.workStale(cmd, snap, "x")
				if p := r.action("x").Status.Phase; p != want {
					t.Errorf("INVARIANT: a write from a stale %q copy moved x from %q to %q", snap.Status.Phase, want, p)
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Logf("RESULT x after three passes on a stale copy: %s", r.actionLine("x"))
		}})
	}

	// ── A robot whose recorded action was deleted is not deleted on that ground ──
	// The action is deleted first; the delete check then found nothing to protect and admitted the
	// robot's delete, and the action created again went to r2 inside r1's lease.
	rows = append(rows, invariantRow{name: "guard/recorder-robot-is-not-deleted/action-deleted-first", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		fm := r.fleetManager()
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		fa := r.inProgressOn(dw, "x", "r1")
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		r.robot("r2", "out")
		r.must("fleet-manager deletes x", fm.Delete(context.Background(), &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}))
		r.waitGone(&fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}})
		r.workEv(dw, newEvents(), "x") // the controller sees the delete
		err := fm.Delete(context.Background(), r.robotObj("r1"))
		t.Logf("RESULT the fleet-manager deletes r1, which records the deleted x: %v", err)
		if err == nil {
			t.Errorf("INVARIANT: r1 was deleted while it records x, which was deleted, and may still run it")
			r.waitGone(r.robotObj("r1"))
		} else if !strings.Contains(err.Error(), `"x"`) {
			t.Errorf("the refusal does not name the action: %v", err)
		}
		r.createAction("x")
		if r.noSecondExecutorBefore(dw, &dw.wireLog, "x", "r1", "r2", dead, 2*time.Second) && time.Now().Before(dead) {
			t.Errorf("INVARIANT: x, created again, went to r2 before r1's lease was provably dead")
		}
	}})

	// ── The record of a deleted action is released on the adapter's word ──
	// Nothing released it: a robot whose action was deleted stayed InProgress, recording it.
	rows = append(rows, invariantRow{name: "guard/record-of-a-deleted-action-is-released-on-the-cancel-ack", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.inProgressOn(wl, "x", "r1")
		r.must("deleting x", r.w.admin.Delete(context.Background(), &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}))
		r.waitGone(&fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}})
		for i := 0; i < 10 && r.get("r1").Status.AssignedAction != ""; i++ {
			r.workEv(wl, newEvents(), "x")
			time.Sleep(100 * time.Millisecond)
		}
		rb := r.get("r1")
		t.Logf("RESULT r1 after x was deleted and its adapter acknowledged the cancel: %s; wire: %s", r.guardState("r1"), wl)
		if rb.Status.AssignedAction != "" {
			t.Errorf("r1 still records the deleted x after its adapter acknowledged the cancel")
		}
		if wl.count("cancel", "r1") == 0 {
			t.Errorf("r1 was not sent a cancel for the deleted x")
		}
		if err := r.fleetManager().Delete(context.Background(), r.robotObj("r1")); err != nil {
			t.Errorf("r1, released, cannot be deleted: %v", err)
		}
	}})

	// ── A create carrying requeue-requested is held to the requeue rule ──
	// The operator deletes work z's stop paused and creates it again carrying the annotation.
	for _, who := range []string{"operator", "admin"} {
		who := who
		rows = append(rows, invariantRow{name: "guard/create-carrying-requeue-needs-estop-clear/" + who, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.shortLease()
			r.robot("r1", "z")
			r.inProgressOn(wl, "x", "r1")
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitStopped("r1")
			r.waitMarker(r.zoneObj("z"), true)
			paused := r.workWith(wl, "x")
			r.robot("r2", "out")
			c := r.w.admin
			if who == "operator" {
				c = r.operator()
			}
			r.must("deleting x", r.w.admin.Delete(context.Background(), &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}))
			nx := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns,
				Annotations: map[string]string{requeueAnnotation: who}}, Spec: *paused.Spec.DeepCopy()}
			err := c.Create(context.Background(), nx)
			t.Logf("RESULT %s creates x again carrying requeue-requested, r1 recording it under z's stop: %v", who, err)
			if who == "operator" {
				if err == nil {
					r.workWith(wl, "x")
					r.workWith(wl, "x")
					t.Errorf("INVARIANT: the operator, who holds no estop-clear on z, created paused work again carrying a requeue (wire: %s)", wl)
				} else if !strings.Contains(err.Error(), "estop-clear") || !strings.Contains(err.Error(), `"z"`) {
					t.Errorf("the refusal does not name the estop-clear verb and the stop: %v", err)
				}
				return
			}
			// The deleted x is kept, Terminating, while r1 holds it under z's stop: the name is taken,
			// and the admin moves held work with a requeue of x, not by creating it again.
			if err == nil || !apierrors.IsAlreadyExists(err) {
				t.Errorf("INVARIANT: x was created again while the deleted, held x is kept: %v", err)
			}
			if wl.count("assign", "r2") > 0 {
				t.Errorf("INVARIANT: held work reached r2 (wire: %s)", wl)
			}
		}})
	}

	// ── A requeue asked before a stop waits while the stop stands ──
	// A declaratively Paused action bound to r1, whose adapter does not answer, is requeued by the
	// operator with no stop standing; then z's stop covers r1. The stop wins: the request is held
	// while it stands and carried out after the clear.
	rows = append(rows, invariantRow{name: "guard/requeue-asked-before-a-stop-waits-for-the-clear", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		ev := newEvents()
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		fa := r.inProgressOn(dw, "x", "r1")
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		r.robot("r2", "out")
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("desiredState Paused", r.update(r.w.admin, x, false, func() { x.Spec.DesiredState = fleetv1.DesiredStatePaused }))
		r.workWith(dw, "x")
		r.must("the operator requeues x, no stop standing", r.annotateAs(r.operator(), x, requeueAnnotation, "op"))
		r.workEv(dw, ev, "x")
		t.Logf("RESULT x after the operator's requeue: %s", r.actionLine("x"))
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj("z"), true)
		for end := dead.Add(4 * time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
			r.workEv(dw, ev, "x")
			if dw.count("assign", "r2") > 0 {
				break
			}
		}
		t.Logf("RESULT x past r1's lease with z's stop standing: %s; r1 %s; events: %s", r.actionLine("x"), r.guardState("r1"), ev)
		if dw.count("assign", "r2") > 0 && r.zoneStands("z") {
			t.Errorf("INVARIANT: a requeue asked before z's stop moved x to r2 while the stop stands (wire: %s)", &dw.wireLog)
			return
		}
		if n := ev.count("RequeueHeldByEstop"); n < 1 {
			t.Errorf("no RequeueHeldByEstop Event told the operator the request is held: %s", ev)
		}
		r.clearStop(r.zoneObj("z"))
		r.waitMarker(r.zoneObj("z"), false)
		for end := time.Now().Add(suLease + 6*time.Second); time.Now().Before(end) && dw.count("assign", "r2") == 0; time.Sleep(300 * time.Millisecond) {
			r.workEv(dw, ev, "x")
		}
		if dw.count("assign", "r2") == 0 {
			t.Errorf("the held requeue was not carried out after the clear: %s; r1 %s; wire: %s", r.actionLine("x"), r.guardState("r1"), &dw.wireLog)
		}
	}})

	// ── Work of a robot whose stop is not confirmed stays once its lease is provably dead ──
	// r1's adapter never answers the stop (estopState Failed). Under a stop the work moves only on
	// the robot's own end report or an operator's decision (invariant I1), so once r1's lease is
	// provably dead the work stays with r1, started or not: r1 may still be moving, a running report
	// may have been lost, and r1 may hold the payload. A Warning Event asks an operator, and a requeue
	// by a holder of estop-clear on the stop then transfers it; r1 stays stopped.
	for _, phase := range []string{"assigned", "assigned-revoking", "inprogress", "inprogress-revoking"} {
		phase := phase
		started := strings.HasPrefix(phase, "inprogress")
		rows = append(rows, invariantRow{name: "guard/unconfirmed-stop-dead-lease-moves-work/" + phase, run: func(t *testing.T, r *rig) {
			dw := &deafWire{}
			ev := newEvents()
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.shortLease()
			r.robot("r1", "z")
			r.setMode("r1", ackNone)
			var fa *fleetv1.FleetAction
			if started {
				fa = r.inProgressOn(dw, "x", "r1")
			} else {
				r.createAction("x")
				fa = r.assignedTo(dw, "x", "r1")
			}
			if strings.HasSuffix(phase, "-revoking") {
				r.revoke(&dw.wireLog, "x", "r1")
				fa = r.action("x")
			}
			dead := fa.Status.LeaseExpiresAt.Add(suSkew)
			r.robot("r2", "out")
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitFor("r1 Failed", 20*time.Second, func() (bool, string) {
				s := r.get("r1").Status.EstopState
				return s == fleetv1.RobotEstopFailed, string(s)
			})
			r.workEv(dw, ev, "x")
			t.Logf("RESULT x under r1's unconfirmed stop: %s; r1 %s", r.actionLine("x"), r.guardState("r1"))
			var moved time.Time
			for end := time.Now().Add(time.Until(dead) + 6*time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
				r.workEv(dw, ev, "x")
				if dw.count("assign", "r2") > 0 || r.action("x").Status.Phase == fleetv1.ActionPhasePending {
					moved = time.Now()
					break
				}
			}
			t.Logf("RESULT x after r1's lease: %s; r1 %s; events: %s; wire: %s", r.actionLine("x"), r.guardState("r1"), ev, &dw.wireLog)
			if s := r.get("r1").Status.EstopState; s != fleetv1.RobotEstopFailed {
				t.Errorf("r1 left its stop: estopState=%q", s)
			}
			if !r.zoneStands("z") {
				t.Errorf("z's stop no longer stands")
			}
			if n := dw.count("assign", "r1"); n != 1 {
				t.Errorf("INVARIANT: x was given back to r1 (%d assigns to r1)", n)
			}
			if !moved.IsZero() {
				t.Errorf("INVARIANT: work under r1's unconfirmed stop moved with no operator (started=%v): %s; wire: %s",
					started, r.actionLine("x"), &dw.wireLog)
				return
			}
			if a := r.action("x"); a.Status.AssignedRobot != "r1" || r.get("r1").Status.AssignedAction != "x" {
				t.Errorf("INVARIANT: x is %q bound to %q, r1 records %q: not kept with r1", a.Status.Phase, a.Status.AssignedRobot,
					r.get("r1").Status.AssignedAction)
			}
			awaitsTheArbiter(t, r, ev, "x", "zone/z")
			msg := r.action("x").Status.Message
			if !strings.Contains(msg, "payload") || !strings.Contains(msg, "kubectl annotate fleetaction x") {
				t.Errorf("the message does not say why and how the operator decides: %q", msg)
			}
			if !started && !strings.Contains(msg, "never reported this action started") {
				t.Errorf("the message does not say r1 never reported x started: %q", msg)
			}
			// The operator's decision: a holder of estop-clear on z transfers the work.
			x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			r.must("the admin requeues x", r.annotateAs(r.w.admin, x, requeueAnnotation, "transfer"))
			for i := 0; i < 30 && dw.count("assign", "r2") == 0; i++ {
				r.workEv(dw, ev, "x")
				time.Sleep(100 * time.Millisecond)
			}
			if dw.count("assign", "r2") == 0 {
				t.Errorf("the operator's requeue did not transfer x to r2: %s; wire: %s", r.actionLine("x"), &dw.wireLog)
			}
		}})
	}

	// (b) A confirmed stop: nothing moves, whatever the lease shows.
	rows = append(rows, invariantRow{name: "guard/unconfirmed-stop-dead-lease-moves-work/confirmed-stop-nothing-moves", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		fa := r.inProgressOn(dw, "x", "r1")
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		r.robot("r2", "out")
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj("z"), true)
		for end := dead.Add(4 * time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
			r.workWith(dw, "x")
		}
		t.Logf("RESULT x past the lease under r1's confirmed stop: %s; r1 %s", r.actionLine("x"), r.guardState("r1"))
		if dw.count("assign", "r2") > 0 {
			t.Errorf("INVARIANT: work a confirmed stop paused moved to r2 while the stop stands (wire: %s)", &dw.wireLog)
		}
	}})

	// An operator's own hold (spec.desiredState: Paused) is the operator's decision: the work stays
	// with its robot under an unconfirmed stop, past the lease, until the operator requeues it.
	rows = append(rows, invariantRow{name: "guard/unconfirmed-stop-dead-lease-moves-work/operator-hold-stays", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.setMode("r1", ackNone)
		fa := r.inProgressOn(dw, "x", "r1")
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("desiredState Paused", r.update(r.w.admin, x, false, func() { x.Spec.DesiredState = fleetv1.DesiredStatePaused }))
		r.workWith(dw, "x")
		r.robot("r2", "out")
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitFor("r1 Failed", 20*time.Second, func() (bool, string) {
			s := r.get("r1").Status.EstopState
			return s == fleetv1.RobotEstopFailed, string(s)
		})
		for end := dead.Add(4 * time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
			r.workWith(dw, "x")
		}
		t.Logf("RESULT x held by desiredState Paused past r1's lease, r1's stop never confirmed: %s", r.actionLine("x"))
		if dw.count("assign", "r2") > 0 {
			t.Errorf("INVARIANT: work the operator holds moved to r2 (wire: %s)", &dw.wireLog)
		}
		if a := r.action("x"); a.Status.Phase != fleetv1.ActionPhasePaused || a.Status.AssignedRobot != "r1" {
			t.Errorf("x is %q bound to %q, not held Paused on r1", a.Status.Phase, a.Status.AssignedRobot)
		}
	}})

	// (c) An unconfirmed stop with the lease alive: nothing moves.
	rows = append(rows, invariantRow{name: "guard/unconfirmed-stop-dead-lease-moves-work/lease-alive-nothing-moves", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.robot("r1", "z")
		r.setMode("r1", ackNone)
		fa := r.inProgressOn(dw, "x", "r1")
		r.robot("r2", "out")
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitFor("r1 Failed", 20*time.Second, func() (bool, string) {
			s := r.get("r1").Status.EstopState
			return s == fleetv1.RobotEstopFailed, string(s)
		})
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
			r.workWith(dw, "x")
		}
		t.Logf("RESULT x with r1's lease alive (horizon %s): %s", fa.Status.LeaseExpiresAt.UTC().Format("15:04:05"), r.actionLine("x"))
		if dw.count("assign", "r2") > 0 {
			t.Errorf("INVARIANT: x moved to r2 while r1's lease is alive (wire: %s)", &dw.wireLog)
		}
	}})

	// (d) Every other robot is under the stop too: the work stays with r1 past its lease, is not given
	// to r3 when a robot no stop covers appears, and goes there on the operator's requeue.
	rows = append(rows, invariantRow{name: "guard/unconfirmed-stop-dead-lease-moves-work/every-other-robot-covered", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		r.setMode("r1", ackNone)
		r.createAction("x")
		fa := r.assignedTo(dw, "x", "r1")
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		r.robot("r2", "z")
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r2")
		r.waitFor("r1 Failed", 20*time.Second, func() (bool, string) {
			s := r.get("r1").Status.EstopState
			return s == fleetv1.RobotEstopFailed, string(s)
		})
		for end := dead.Add(4 * time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
			r.workWith(dw, "x")
		}
		fa = r.action("x")
		t.Logf("RESULT x past r1's lease with every robot under z's stop: %s; wire: %s", r.actionLine("x"), &dw.wireLog)
		if dw.count("assign", "r2") > 0 || dw.count("assign", "r1") != 1 {
			t.Errorf("INVARIANT: x was given to a robot z's stop covers (wire: %s)", &dw.wireLog)
		}
		if fa.Status.Phase == fleetv1.ActionPhasePending || fa.Status.AssignedRobot != "r1" {
			t.Errorf("INVARIANT: x is %q bound to %q, not kept with r1 under its unconfirmed stop", fa.Status.Phase, fa.Status.AssignedRobot)
		}
		r.robot("r3", "out")
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
			r.workWith(dw, "x")
		}
		if dw.count("assign", "r3") > 0 {
			t.Errorf("INVARIANT: x went to r3 with no operator's decision: %s; wire: %s", r.actionLine("x"), &dw.wireLog)
		}
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("the admin requeues x", r.annotateAs(r.w.admin, x, requeueAnnotation, "transfer"))
		for i := 0; i < 30 && dw.count("assign", "r3") == 0; i++ {
			r.workWith(dw, "x")
			time.Sleep(100 * time.Millisecond)
		}
		if dw.count("assign", "r3") == 0 {
			t.Errorf("the operator's requeue did not give x to r3, the robot no stop covers: %s; wire: %s", r.actionLine("x"), &dw.wireLog)
		}
	}})
	return rows
}

func isTaskTerminal(p fleetv1.FleetTaskPhase) bool {
	return p == fleetv1.FleetTaskPhaseSucceeded || p == fleetv1.FleetTaskPhaseFailed || p == fleetv1.FleetTaskPhaseCancelled
}
