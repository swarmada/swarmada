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
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Condition types written by this controller and the draft gate.
const (
	// ConditionAwaitingActivation is True on a FleetTask that is composed and held.
	ConditionAwaitingActivation = "AwaitingActivation"
	// ConditionTriggerReady is True on a TaskTrigger whose source resolves and whose
	// event type that source declares.
	ConditionTriggerReady = "Ready"
)

// Kubernetes Event reasons emitted by the TaskTrigger controller.
const (
	// EventTaskDrafted — a trigger fired and a draft is waiting for a human.
	EventTaskDrafted = "TaskDrafted"
	// EventEventRejected — an event named a type its source never declared (E10.1).
	EventEventRejected = "SemanticEventRejected"
)

// TaskTriggerReconciler turns a semantic event into a DRAFT FleetTask.
//
// # IT NEVER DISPATCHES, AND THAT IS THE DESIGN
//
// The reconciler's entire output is a FleetTask with spec.draft: true. The FleetTask
// controller holds such a task at phase Draft and generates no children, so at no
// point in this path has a robot been asked to do anything. A human clears the flag.
//
// The separation matters because of where the authority sits. A semantic event is
// evidence that something happened, produced by a sensor, a bridge or a model. None of
// those is accountable for a machine moving in a space people are standing in. This
// controller does the tedious, error-prone part — composing a task that is valid
// against the real schema — and stops exactly where judgement begins.
//
// # WHAT IT RECONCILES ON
//
// SemanticEvents, not TaskTriggers. The event is the thing that happens; a trigger is
// standing configuration. Reconciling the event also gives idempotency somewhere
// durable: status.draftedTasks on the event records what it produced, so a resync
// cannot draft the same task twice, and the deterministic draft name means even a lost
// status write collides on create rather than duplicating.
type TaskTriggerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// APIReader reads the API server directly, bypassing the manager's cache. It is
	// used for one thing: a task created a moment ago can be missing from the cache,
	// and a cache miss alone must not take a draft off a trigger's pendingDrafts or
	// leave a collided create undecided, so a miss on a task is confirmed here. Every
	// other read is the cache. Nil falls back to Client.
	APIReader client.Reader
}

// reader is the uncached APIReader when one is wired, else Client (see APIReader).
func (r *TaskTriggerReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=swarmada.io,resources=semanticevents,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=semanticevents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=semanticeventsources,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=semanticeventsources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=tasktriggers,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=tasktriggers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleettasks,verbs=get;list;watch;create

// Reconcile evaluates one SemanticEvent against every TaskTrigger in its namespace.
func (r *TaskTriggerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Every reconcile in the namespace first takes settled drafts off the triggers'
	// queues. It runs before the event is read because the reconciles that matter here
	// are the ones a draft's change enqueues: its owning event is already evaluated, or
	// already deleted, and returns below without reaching the trigger loop.
	if err := r.prunePendingDrafts(ctx, req.Namespace); err != nil {
		return ctrl.Result{}, err
	}

	var event fleetv1.SemanticEvent
	if err := r.Get(ctx, req.NamespacedName, &event); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// An event is evaluated ONCE. It is a record that something happened at a moment,
	// not desired state to be converged on, so re-evaluating it on every resync would
	// draft a fresh task each time the controller restarted.
	//
	// The reconcile that lands here is the one the event's own Accepted or Rejected write
	// triggers once it reaches the cache, so it is where the source's counts are brought
	// up to date: they then include this event.
	if event.Status.Phase == fleetv1.SemanticEventAccepted ||
		event.Status.Phase == fleetv1.SemanticEventRejected {
		return ctrl.Result{}, r.recountSource(ctx, event.Namespace, event.Spec.Source)
	}

	// E10.1 — an undeclared type MUST be rejected and MUST NOT fire a trigger.
	var source fleetv1.SemanticEventSource
	err := r.Get(ctx, types.NamespacedName{Namespace: event.Namespace, Name: event.Spec.Source}, &source)
	switch {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, r.reject(ctx, &event,
			fmt.Sprintf("no SemanticEventSource %q in namespace %q", event.Spec.Source, event.Namespace))
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("fetching semantic event source: %w", err)
	}
	if !declaresType(&source, event.Spec.Type) {
		// Named rather than silently ignored: a typo in an event type and a type
		// nobody has written a trigger for yet both otherwise do nothing, and a
		// silent no-op is the worst outcome for a mechanism whose job is to notice.
		return ctrl.Result{}, r.reject(ctx, &event, fmt.Sprintf(
			"source %q does not declare event type %q (declares %v)",
			source.Name, event.Spec.Type, source.Spec.EventTypes))
	}

	var triggers fleetv1.TaskTriggerList
	if err := r.List(ctx, &triggers, client.InNamespace(event.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing task triggers: %w", err)
	}

	drafted := []string{}
	var conflicts []string
	for i := range triggers.Items {
		t := &triggers.Items[i]
		if !triggerMatches(t, &event) {
			continue
		}
		name, created, err := r.draftTask(ctx, t, &event)
		var taken *draftNameTakenError
		if errors.As(err, &taken) {
			// Not a firing: the task of that name is not this event's draft. It is
			// not listed as drafted, and the event's message names it.
			conflicts = append(conflicts, taken.Error())
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		drafted = append(drafted, name)
		if err := r.recordFiring(ctx, t, &event, name, created); err != nil {
			return ctrl.Result{}, err
		}
	}
	sort.Strings(drafted)
	sort.Strings(conflicts)
	return ctrl.Result{}, r.accept(ctx, &event, drafted, conflicts)
}

// declaresType reports whether the source declared this event type (E10.1).
func declaresType(s *fleetv1.SemanticEventSource, eventType string) bool {
	for _, t := range s.Spec.EventTypes {
		if t == eventType {
			return true
		}
	}
	return false
}

// triggerMatches reports whether this trigger fires on this event.
//
// A SUSPENDED TRIGGER DOES NOT MATCH, and the event still records that it was
// evaluated — so an operator can tell "nothing happened" from "this was switched
// off", which a deleted trigger could not have told them.
func triggerMatches(t *fleetv1.TaskTrigger, e *fleetv1.SemanticEvent) bool {
	if t.Spec.Suspend {
		return false
	}
	return t.Spec.Source.SemanticEventSource == e.Spec.Source &&
		t.Spec.Source.EventType == e.Spec.Type
}

// draftTaskName is deterministic, so a retry after a lost status write collides on
// create instead of drafting a second copy of the same task.
func draftTaskName(trigger, event string) string {
	return trigger + "-" + event
}

// draftTask creates the held FleetTask. spec.draft is forced true HERE rather than
// trusted from the template: the template is operator-authored, and a template that
// forgot the flag — or set it false — would auto-dispatch, which is the one outcome
// this controller exists to prevent.
//
// created is false when the draft already existed: the event was evaluated before
// and this pass collided on the deterministic name. A collision with a task this
// event does not control is a *draftNameTakenError (see collidedDraft).
func (r *TaskTriggerReconciler) draftTask(ctx context.Context, t *fleetv1.TaskTrigger, e *fleetv1.SemanticEvent) (string, bool, error) {
	name := draftTaskName(t.Name, e.Name)
	spec := *t.Spec.TaskTemplate.DeepCopy()
	spec.Draft = true

	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: e.Namespace,
			Labels: map[string]string{
				"swarmada.io/drafted-by":    t.Name,
				"swarmada.io/drafted-from":  e.Name,
				"swarmada.io/drafted-event": e.Spec.Type,
			},
			Annotations: map[string]string{
				"swarmada.io/draft-origin": fmt.Sprintf(
					"SemanticEvent %s/%s (type %s) via TaskTrigger %s",
					e.Namespace, e.Name, e.Spec.Type, t.Name),
			},
		},
		Spec: spec,
	}
	// Owned by the EVENT, not the trigger: the draft is the consequence of that one
	// occurrence. Deleting a standing trigger must not delete drafts a human is still
	// reviewing, and deleting the event that caused one is a reasonable way to
	// withdraw it.
	if err := ctrl.SetControllerReference(e, task, r.Scheme); err != nil {
		return "", false, fmt.Errorf("setting owner on draft: %w", err)
	}

	if err := r.Create(ctx, task); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := r.collidedDraft(ctx, t, e, name); err != nil {
				return "", false, err
			}
			return name, false, nil // idempotent: this event already drafted this task
		}
		return "", false, fmt.Errorf("creating draft task: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(t, corev1.EventTypeNormal, EventTaskDrafted,
			"drafted FleetTask %q from %s %q — AWAITING ACTIVATION, nothing dispatched",
			name, e.Spec.Type, e.Name)
	}
	log.FromContext(ctx).Info("drafted task awaiting human activation",
		"task", name, "trigger", t.Name, "event", e.Name, "eventType", e.Spec.Type)
	return name, true, nil
}

// draftNameTakenError is a collision on the draft name with a FleetTask this event does
// not control. It is not a firing: the trigger records nothing and the event does not
// list the task as drafted. Its text goes into the event's status.message.
type draftNameTakenError struct {
	trigger, task, namespace string
	// controller is the existing task's controller reference, nil when it has none.
	controller *metav1.OwnerReference
}

func (e *draftNameTakenError) Error() string {
	whose := "has no controller"
	if e.controller != nil {
		whose = fmt.Sprintf("is controlled by %s %q (uid %s)", e.controller.Kind, e.controller.Name, e.controller.UID)
	}
	return fmt.Sprintf("TaskTrigger %q did not fire: FleetTask %q in namespace %q already exists and %s",
		e.trigger, e.task, e.namespace, whose)
}

// collidedDraft decides a create that collided on the draft name. The collision is
// success only when the task there is this event's own draft: the event evaluated again,
// after a lost status write or before its Accepted phase reached the cache.
//
// THE CONTROLLER REFERENCE IS WHAT IDENTIFIES IT. draftTask sets it, and it carries the
// event's UID, which neither a task created by hand nor the surviving draft of an earlier
// event of the same name can match. The drafted-by and drafted-from labels are not
// evidence: anyone who can create a FleetTask can set them. Any other task returns a
// *draftNameTakenError and is left as it is.
func (r *TaskTriggerReconciler) collidedDraft(ctx context.Context, t *fleetv1.TaskTrigger, e *fleetv1.SemanticEvent, name string) error {
	key := types.NamespacedName{Namespace: e.Namespace, Name: name}
	var existing fleetv1.FleetTask
	err := r.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		err = r.reader().Get(ctx, key, &existing)
	}
	if err != nil {
		// Gone since the create collided, or unreadable: the event is evaluated again.
		return fmt.Errorf("reading FleetTask %q after its create collided: %w", name, err)
	}
	if metav1.IsControlledBy(&existing, e) {
		return nil
	}
	return &draftNameTakenError{
		trigger: t.Name, task: name, namespace: e.Namespace,
		controller: metav1.GetControllerOf(&existing),
	}
}

// recordFiring updates the trigger's own status: when it last fired, how many drafts it
// has produced, and the queue of drafts still waiting for someone.
//
// A firing is recorded once per draft, not once per evaluation. The controller owns
// the drafts it creates, so a create enqueues the event again at once, and that pass
// can read the event before its Accepted phase is visible; its create then collides on
// the draft's name. A collided draft is recorded only if the trigger does not already
// list it and it is still held, which is the retry after a lost status write. The
// trigger is re-read and patched under its resourceVersion, so the decision is taken
// against the current queue, never a stale one.
func (r *TaskTriggerReconciler) recordFiring(ctx context.Context, t *fleetv1.TaskTrigger, e *fleetv1.SemanticEvent, draft string, created bool) error {
	if !created {
		held, err := r.draftStillHeld(ctx, t.Namespace, draft)
		if err != nil || !held {
			return err
		}
	}
	return r.patchTriggerStatus(ctx, client.ObjectKeyFromObject(t), func(tt *fleetv1.TaskTrigger) bool {
		if !created && containsString(tt.Status.PendingDrafts, draft) {
			return false // already recorded
		}
		now := metav1.Now()
		tt.Status.LastFiredAt = &now
		tt.Status.LastEventName = e.Name
		tt.Status.DraftCount++
		if !containsString(tt.Status.PendingDrafts, draft) {
			tt.Status.PendingDrafts = append(tt.Status.PendingDrafts, draft)
			sort.Strings(tt.Status.PendingDrafts)
		}
		tt.Status.ObservedGeneration = tt.Generation
		apimeta.SetStatusCondition(&tt.Status.Conditions, metav1.Condition{
			Type:    ConditionTriggerReady,
			Status:  metav1.ConditionTrue,
			Reason:  "SourceResolved",
			Message: "source and event type resolve; drafts are produced held",
		})
		return true
	})
}

// patchTriggerStatus re-reads a trigger from the cache, applies mutate, and patches its
// status under the resourceVersion it read, retrying on conflict. A stale read cannot
// write: its resourceVersion is refused, and the retry reads again. mutate returns false
// when there is nothing to write, and then nothing is written (RA-1).
func (r *TaskTriggerReconciler) patchTriggerStatus(ctx context.Context, key client.ObjectKey, mutate func(*fleetv1.TaskTrigger) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var t fleetv1.TaskTrigger
		if err := r.Get(ctx, key, &t); err != nil {
			return client.IgnoreNotFound(err)
		}
		original := t.DeepCopy()
		if !mutate(&t) {
			return nil
		}
		return r.Status().Patch(ctx, &t, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	})
}

// draftStillHeld reports whether the named draft exists and still carries spec.draft.
// A cache miss is confirmed against the API server: a draft created a moment ago may
// not be in the cache yet, and it is held, not gone.
func (r *TaskTriggerReconciler) draftStillHeld(ctx context.Context, namespace, name string) (bool, error) {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	var task fleetv1.FleetTask
	err := r.Get(ctx, key, &task)
	if apierrors.IsNotFound(err) {
		err = r.reader().Get(ctx, key, &task)
	}
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading draft %q: %w", name, err)
	}
	return task.Spec.Draft && task.DeletionTimestamp == nil, nil
}

// prunePendingDrafts takes off each trigger's pendingDrafts every draft that is no
// longer awaiting a human: one a person has cleared, and one that has been deleted,
// directly or with the event that owns it. It writes a trigger only when its queue
// changes.
func (r *TaskTriggerReconciler) prunePendingDrafts(ctx context.Context, namespace string) error {
	var triggers fleetv1.TaskTriggerList
	if err := r.List(ctx, &triggers, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing task triggers: %w", err)
	}
	for i := range triggers.Items {
		t := &triggers.Items[i]
		settled := map[string]bool{}
		for _, draft := range t.Status.PendingDrafts {
			held, err := r.draftStillHeld(ctx, namespace, draft)
			if err != nil {
				return err
			}
			if !held {
				settled[draft] = true
			}
		}
		if len(settled) == 0 {
			continue
		}
		err := r.patchTriggerStatus(ctx, client.ObjectKeyFromObject(t), func(tt *fleetv1.TaskTrigger) bool {
			kept := make([]string, 0, len(tt.Status.PendingDrafts))
			for _, draft := range tt.Status.PendingDrafts {
				if !settled[draft] {
					kept = append(kept, draft)
				}
			}
			if len(kept) == len(tt.Status.PendingDrafts) {
				return false
			}
			tt.Status.PendingDrafts = kept
			return true
		})
		if err != nil {
			return fmt.Errorf("pruning pendingDrafts of %q: %w", t.Name, err)
		}
		log.FromContext(ctx).Info("drafts no longer awaiting a human removed from pendingDrafts",
			"trigger", t.Name, "count", len(settled))
	}
	return nil
}

// accept marks the event evaluated and names what it drafted, and each draft-name
// collision with a task it does not control (conflicts).
//
// A collision does not reject the event. Rejected means the event named a source that
// does not exist or a type that source does not declare, and fired nothing; a collision
// is neither, and the event's other triggers fire as usual.
func (r *TaskTriggerReconciler) accept(ctx context.Context, e *fleetv1.SemanticEvent, drafted, conflicts []string) error {
	original := e.DeepCopy()
	e.Status.Phase = fleetv1.SemanticEventAccepted
	e.Status.DraftedTasks = drafted
	e.Status.ObservedGeneration = e.Generation
	if len(drafted) == 0 && len(conflicts) == 0 {
		// Explicit, because "accepted and matched nothing" and "not yet evaluated"
		// look identical otherwise, and the first is a configuration question an
		// operator can act on.
		e.Status.Message = "accepted; no TaskTrigger matched"
	} else {
		e.Status.Message = fmt.Sprintf("accepted; drafted %d task(s) AWAITING ACTIVATION", len(drafted))
	}
	for _, c := range conflicts {
		e.Status.Message += "; " + c
	}
	return r.Status().Patch(ctx, e, client.MergeFrom(original))
}

// reject records an event that fired nothing, and why (E10.1).
func (r *TaskTriggerReconciler) reject(ctx context.Context, e *fleetv1.SemanticEvent, msg string) error {
	original := e.DeepCopy()
	e.Status.Phase = fleetv1.SemanticEventRejected
	e.Status.Message = msg
	e.Status.ObservedGeneration = e.Generation
	if r.Recorder != nil {
		r.Recorder.Event(e, corev1.EventTypeWarning, EventEventRejected, msg)
	}
	return r.Status().Patch(ctx, e, client.MergeFrom(original))
}

// recountSource sets a SemanticEventSource's acceptedCount and rejectedCount to the
// number of evaluated events in its namespace that name it, by outcome.
//
// The counts are derived, not incremented. An increment written after the event's own
// status write double-counts when that write is retried and undercounts when the second
// write fails, and nothing afterwards could tell which; a count recomputed from the
// events is right after any failure, at the next evaluation. It is a count of the events
// that exist: deleting an event takes it out at the next recount.
//
// RA-1: the source is written only when a count changes. That is at most one write per
// evaluated event, none on a resync or a restart, and a burst whose outcomes reach the
// cache together is written once. A source that does not exist, the refusal the event
// itself records, has nothing to count on.
func (r *TaskTriggerReconciler) recountSource(ctx context.Context, namespace, name string) error {
	var source fleetv1.SemanticEventSource
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &source); err != nil {
		return client.IgnoreNotFound(err)
	}
	var events fleetv1.SemanticEventList
	if err := r.List(ctx, &events, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing semantic events: %w", err)
	}
	var accepted, rejected int64
	for i := range events.Items {
		e := &events.Items[i]
		if e.Spec.Source != name {
			continue
		}
		switch e.Status.Phase {
		case fleetv1.SemanticEventAccepted:
			accepted++
		case fleetv1.SemanticEventRejected:
			rejected++
		}
	}
	if source.Status.AcceptedCount == accepted && source.Status.RejectedCount == rejected {
		return nil
	}
	original := source.DeepCopy()
	source.Status.AcceptedCount = accepted
	source.Status.RejectedCount = rejected
	return r.Status().Patch(ctx, &source, client.MergeFrom(original))
}

// containsString reports whether s contains v.
func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// SetupWithManager wires the reconciler to SemanticEvents.
func (r *TaskTriggerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.SemanticEvent{}).
		Owns(&fleetv1.FleetTask{}).
		Named("tasktrigger").
		Complete(r)
}
