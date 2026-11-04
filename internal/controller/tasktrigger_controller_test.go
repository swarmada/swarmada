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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// TaskTrigger — semantic event → DRAFT FleetTask → a human activates → only then
// does anything dispatch.
//
// The property under test in most of these is a NEGATIVE one: that nothing ran. That
// is harder to assert than a positive and easier to lose, so it is asserted directly
// (the created task carries spec.draft, the FleetTask controller generates no
// children, the phase is Draft) rather than inferred from the absence of an error.

const triggerNS = "yard-west"

func newTriggerReconciler(t *testing.T, objs ...client.Object) (*TaskTriggerReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.SemanticEvent{}, &fleetv1.TaskTrigger{},
			&fleetv1.SemanticEventSource{}, &fleetv1.FleetTask{}).
		Build()
	return &TaskTriggerReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}, c
}

func eventSource(name string, types ...string) *fleetv1.SemanticEventSource {
	return &fleetv1.SemanticEventSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: triggerNS},
		Spec:       fleetv1.SemanticEventSourceSpec{EventTypes: types},
	}
}

func semanticEvent(name, source, eventType string) *fleetv1.SemanticEvent {
	return &fleetv1.SemanticEvent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: triggerNS},
		Spec:       fleetv1.SemanticEventSpec{Source: source, Type: eventType},
	}
}

func taskTrigger(name, source, eventType string) *fleetv1.TaskTrigger {
	return &fleetv1.TaskTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: triggerNS},
		Spec: fleetv1.TaskTriggerSpec{
			Source: fleetv1.TaskTriggerSourceRef{
				SemanticEventSource: source,
				EventType:           eventType,
			},
			TaskTemplate: fleetv1.FleetTaskSpec{
				Actions: []fleetv1.FleetTaskAction{{
					Name:   "investigate",
					Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
				}},
			},
		},
	}
}

func reconcileEvent(t *testing.T, r *TaskTriggerReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: triggerNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getTask(t *testing.T, c client.Client, name string) *fleetv1.FleetTask {
	t.Helper()
	var ft fleetv1.FleetTask
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: triggerNS}, &ft); err != nil {
		t.Fatalf("get task %q: %v", name, err)
	}
	return &ft
}

// ---------------------------------------------------------------------------
// THE CENTRAL GUARANTEE: a firing produces a draft, and a draft is not dispatched.
// ---------------------------------------------------------------------------

func TestTrigger_FiringProducesADraftAndNothingElse(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)

	reconcileEvent(t, r, "ev-1")

	task := getTask(t, c, "intruder-response-ev-1")
	if !task.Spec.Draft {
		t.Fatal("the created FleetTask does NOT carry spec.draft. This is auto-dispatch: the " +
			"FleetTask controller would generate children and a robot would be asked to move " +
			"because a sensor fired, with no human in between")
	}
	if len(task.Spec.Actions) != 1 || task.Spec.Actions[0].Name != "investigate" {
		t.Errorf("template not carried onto the draft: %#v", task.Spec.Actions)
	}
	if task.Labels["swarmada.io/drafted-by"] != "intruder-response" {
		t.Errorf("draft does not record which trigger produced it: %v", task.Labels)
	}

	var ev fleetv1.SemanticEvent
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "ev-1", Namespace: triggerNS}, &ev); err != nil {
		t.Fatalf("get event: %v", err)
	}
	if ev.Status.Phase != fleetv1.SemanticEventAccepted {
		t.Errorf("event phase = %q, want Accepted", ev.Status.Phase)
	}
	if len(ev.Status.DraftedTasks) != 1 {
		t.Errorf("event does not record what it drafted: %v", ev.Status.DraftedTasks)
	}
}

// The template is not trusted to hold the gate. An operator-authored template that
// omits spec.draft — or sets it false — must still produce a held task.
func TestTrigger_TemplateCannotOptOutOfTheDraftGate(t *testing.T) {
	trig := taskTrigger("eager", "yard-sensors", "IntruderDetected")
	trig.Spec.TaskTemplate.Draft = false // explicitly trying to auto-dispatch

	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		trig,
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")

	if !getTask(t, c, "eager-ev-1").Spec.Draft {
		t.Fatal("a template that set draft:false produced a dispatchable task. The gate is " +
			"forced by the controller precisely so that a template — which is ordinary " +
			"operator-authored data — cannot switch it off")
	}
}

// ---------------------------------------------------------------------------
// The other half of the guarantee, in the FleetTask controller: a draft generates
// no children and reaches no dispatchable phase.
// ---------------------------------------------------------------------------

func TestDraftTask_GeneratesNoChildrenAndHoldsAtDraft(t *testing.T) {
	draft := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "held", Namespace: triggerNS},
		Spec: fleetv1.FleetTaskSpec{
			Draft: true,
			Actions: []fleetv1.FleetTaskAction{{
				Name:   "investigate",
				Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
			}},
		},
	}
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(draft).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}).Build()
	r := &FleetTaskReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "held", Namespace: triggerNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var actions fleetv1.FleetActionList
	if err := c.List(context.Background(), &actions, client.InNamespace(triggerNS)); err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions.Items) != 0 {
		t.Fatalf("a draft generated %d child action(s). Nothing may be dispatched for a task "+
			"a human has not activated", len(actions.Items))
	}

	got := getTask(t, c, "held")
	if got.Status.Phase != fleetv1.FleetTaskPhaseDraft {
		t.Errorf("phase = %q, want Draft", got.Status.Phase)
	}
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionAwaitingActivation) {
		t.Error("AwaitingActivation condition not set — an operator has no way to see that " +
			"this task is waiting for them rather than stuck")
	}
}

// Activation: clearing the flag lets the SAME object dispatch normally.
func TestDraftTask_ActivationReleasesIt(t *testing.T) {
	draft := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "held", Namespace: triggerNS},
		Spec: fleetv1.FleetTaskSpec{
			Draft: false, // the human has activated it
			Actions: []fleetv1.FleetTaskAction{{
				Name:   "investigate",
				Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
			}},
		},
	}
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(draft).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}).Build()
	r := &FleetTaskReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "held", Namespace: triggerNS},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var actions fleetv1.FleetActionList
	if err := c.List(context.Background(), &actions, client.InNamespace(triggerNS)); err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions.Items) == 0 {
		t.Fatal("an ACTIVATED task generated no children — the gate must release, not just hold")
	}
	if got := getTask(t, c, "held").Status.Phase; got == fleetv1.FleetTaskPhaseDraft {
		t.Error("an activated task is still at phase Draft")
	}
}

// ---------------------------------------------------------------------------
// E10.1 — an undeclared type is rejected and fires nothing.
// ---------------------------------------------------------------------------

func TestTrigger_UndeclaredEventTypeIsRejectedAndFiresNothing(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		// The trigger WOULD match the type — so if the declaration gate were missing
		// this would draft a task, and the test would catch exactly that.
		taskTrigger("typo-response", "yard-sensors", "IntruderDetcted"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetcted"),
	)

	reconcileEvent(t, r, "ev-1")

	var ev fleetv1.SemanticEvent
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "ev-1", Namespace: triggerNS}, &ev); err != nil {
		t.Fatalf("get event: %v", err)
	}
	if ev.Status.Phase != fleetv1.SemanticEventRejected {
		t.Fatalf("phase = %q, want Rejected: the source never declared this type, and an "+
			"undeclared type must not fire a trigger", ev.Status.Phase)
	}
	if ev.Status.Message == "" {
		t.Error("a rejected event carries no message — the message is the entire diagnostic " +
			"surface, and a typo that silently does nothing is the failure this gate exists for")
	}
	var tasks fleetv1.FleetTaskList
	if err := c.List(context.Background(), &tasks, client.InNamespace(triggerNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks.Items) != 0 {
		t.Errorf("a rejected event drafted %d task(s)", len(tasks.Items))
	}
}

func TestTrigger_MissingSourceIsRejected(t *testing.T) {
	r, c := newTriggerReconciler(t,
		taskTrigger("orphan", "gone", "IntruderDetected"),
		semanticEvent("ev-1", "gone", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")

	var ev fleetv1.SemanticEvent
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "ev-1", Namespace: triggerNS}, &ev); err != nil {
		t.Fatalf("get event: %v", err)
	}
	if ev.Status.Phase != fleetv1.SemanticEventRejected {
		t.Errorf("phase = %q, want Rejected for an event naming no existing source", ev.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Idempotency: an event is a record of a moment, not desired state.
// ---------------------------------------------------------------------------

func TestTrigger_ReconcilingTwiceDraftsOnce(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)

	reconcileEvent(t, r, "ev-1")
	reconcileEvent(t, r, "ev-1")

	var tasks fleetv1.FleetTaskList
	if err := c.List(context.Background(), &tasks, client.InNamespace(triggerNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks.Items) != 1 {
		t.Fatalf("two reconciles drafted %d tasks, want 1. An event records that something "+
			"happened once; converging on it would draft a fresh task on every resync and "+
			"every controller restart", len(tasks.Items))
	}
}

// A suspended trigger does not fire, and the event still records that it was seen —
// so "switched off" is distinguishable from "nothing happened".
func TestTrigger_SuspendedDoesNotFireButEventIsStillEvaluated(t *testing.T) {
	trig := taskTrigger("paused-response", "yard-sensors", "IntruderDetected")
	trig.Spec.Suspend = true

	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		trig,
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")

	var tasks fleetv1.FleetTaskList
	if err := c.List(context.Background(), &tasks, client.InNamespace(triggerNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks.Items) != 0 {
		t.Errorf("a suspended trigger drafted %d task(s)", len(tasks.Items))
	}

	var ev fleetv1.SemanticEvent
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "ev-1", Namespace: triggerNS}, &ev); err != nil {
		t.Fatalf("get event: %v", err)
	}
	if ev.Status.Phase != fleetv1.SemanticEventAccepted {
		t.Errorf("phase = %q, want Accepted — the event was valid; the trigger was off", ev.Status.Phase)
	}
	if ev.Status.Message == "" {
		t.Error("an accepted event that matched nothing must say so: 'accepted and matched " +
			"nothing' and 'not yet evaluated' otherwise look identical")
	}
}

// A non-matching event type does not fire an unrelated trigger.
func TestTrigger_NonMatchingTypeDoesNotFire(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected", "ShiftStarted"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "ShiftStarted"),
	)
	reconcileEvent(t, r, "ev-1")

	var tasks fleetv1.FleetTaskList
	if err := c.List(context.Background(), &tasks, client.InNamespace(triggerNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks.Items) != 0 {
		t.Errorf("a ShiftStarted event fired an IntruderDetected trigger (%d drafts)", len(tasks.Items))
	}
}

// The trigger's own status is the operator's queue of drafts awaiting a human.
func TestTrigger_StatusRecordsThePendingQueue(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")

	var trig fleetv1.TaskTrigger
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: "intruder-response", Namespace: triggerNS}, &trig); err != nil {
		t.Fatalf("get trigger: %v", err)
	}
	if trig.Status.DraftCount != 1 {
		t.Errorf("draftCount = %d, want 1", trig.Status.DraftCount)
	}
	if len(trig.Status.PendingDrafts) != 1 || trig.Status.PendingDrafts[0] != "intruder-response-ev-1" {
		t.Errorf("pendingDrafts = %v, want [intruder-response-ev-1] — this is the operator's "+
			"work queue, and a growing list is the signal that events are arriving faster "+
			"than anyone is acting on them", trig.Status.PendingDrafts)
	}
	if trig.Status.LastEventName != "ev-1" {
		t.Errorf("lastEventName = %q, want ev-1", trig.Status.LastEventName)
	}
}
