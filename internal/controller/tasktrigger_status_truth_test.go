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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A TaskTrigger's status is the operator's measure of how fast events arrive against
// how fast anyone acts on them. It has to tell the truth on both counts:
//
//   - draftCount is how many drafts the trigger produced — not how many times an event
//     was looked at. The controller owns the drafts it creates, so creating one
//     enqueues its event again at once, and that second pass can read the event before
//     its Accepted phase is visible. The draft's deterministic name makes the second
//     create collide, so no second draft exists; the count must not say one does.
//   - pendingDrafts is the queue of drafts still awaiting a human. A draft a person has
//     cleared, or one that has been deleted, is no longer awaiting anyone.

func getTrigger(t *testing.T, c client.Client, name string) *fleetv1.TaskTrigger {
	t.Helper()
	var tt fleetv1.TaskTrigger
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: triggerNS}, &tt); err != nil {
		t.Fatalf("get trigger %q: %v", name, err)
	}
	return &tt
}

// forgetEvaluation puts the event back to "not yet evaluated", which is what a second
// reconcile sees when it reads the event before the first reconcile's status write is
// visible to it.
func forgetEvaluation(t *testing.T, c client.Client, name string) {
	t.Helper()
	var ev fleetv1.SemanticEvent
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: triggerNS}, &ev); err != nil {
		t.Fatalf("get event: %v", err)
	}
	ev.Status = fleetv1.SemanticEventStatus{}
	if err := c.Status().Update(context.Background(), &ev); err != nil {
		t.Fatalf("reset event status: %v", err)
	}
}

func TestTrigger_DraftCountCountsDraftsNotEvaluations(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)

	reconcileEvent(t, r, "ev-1")
	forgetEvaluation(t, c, "ev-1")
	reconcileEvent(t, r, "ev-1")

	var tasks fleetv1.FleetTaskList
	if err := c.List(context.Background(), &tasks, client.InNamespace(triggerNS)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks.Items) != 1 {
		t.Fatalf("drafts = %d, want 1", len(tasks.Items))
	}
	if got := getTrigger(t, c, "intruder-response").Status.DraftCount; got != 1 {
		t.Errorf("draftCount = %d with 1 draft in existence. An event evaluated twice produced one "+
			"draft; a count that says two reads as an event that happened twice", got)
	}
}

// ET1's sequence: several events, one of them evaluated twice; the count equals the
// drafts that exist.
func TestTrigger_DraftCountEqualsDraftsAfterARepeatedEvaluation(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-2", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-3", "yard-sensors", "IntruderDetected"),
	)
	for _, ev := range []string{"ev-1", "ev-2", "ev-3"} {
		reconcileEvent(t, r, ev)
	}
	forgetEvaluation(t, c, "ev-2")
	reconcileEvent(t, r, "ev-2")

	tt := getTrigger(t, c, "intruder-response")
	if tt.Status.DraftCount != 3 {
		t.Errorf("draftCount = %d, want 3 (three events, three drafts)", tt.Status.DraftCount)
	}
	if len(tt.Status.PendingDrafts) != 3 {
		t.Errorf("pendingDrafts = %v, want the three drafts", tt.Status.PendingDrafts)
	}
}

func clearDraft(t *testing.T, c client.Client, name string) {
	t.Helper()
	task := getTask(t, c, name)
	task.Spec.Draft = false
	if err := c.Update(context.Background(), task); err != nil {
		t.Fatalf("clear draft %q: %v", name, err)
	}
}

func TestTrigger_PendingDraftsDropsADraftOnceItIsCleared(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-2", "yard-sensors", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")
	reconcileEvent(t, r, "ev-2")

	// A person clears ev-1's draft. The controller owns the draft, so the change
	// enqueues the owning event, which is already Accepted.
	clearDraft(t, c, "intruder-response-ev-1")
	reconcileEvent(t, r, "ev-1")

	tt := getTrigger(t, c, "intruder-response")
	if containsString(tt.Status.PendingDrafts, "intruder-response-ev-1") {
		t.Errorf("pendingDrafts = %v still lists a draft a person has cleared. The field is the "+
			"queue of drafts awaiting a human; this one is not awaiting anyone", tt.Status.PendingDrafts)
	}
	if !containsString(tt.Status.PendingDrafts, "intruder-response-ev-2") {
		t.Errorf("pendingDrafts = %v lost ev-2's draft, which is still held", tt.Status.PendingDrafts)
	}
	if tt.Status.DraftCount != 2 {
		t.Errorf("draftCount = %d, want 2: clearing a draft does not un-produce it", tt.Status.DraftCount)
	}
}

func TestTrigger_PendingDraftsDropsADeletedDraft(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")

	if err := c.Delete(context.Background(), getTask(t, c, "intruder-response-ev-1")); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	reconcileEvent(t, r, "ev-1")

	if got := getTrigger(t, c, "intruder-response").Status.PendingDrafts; len(got) != 0 {
		t.Errorf("pendingDrafts = %v after the draft was deleted, want empty", got)
	}
}

// Deleting the event withdraws its draft (the draft is owned by the event). The
// reconcile that follows finds no event at all, and still has to take the draft off
// the queue.
func TestTrigger_PendingDraftsDropsADraftWhoseEventIsGone(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")

	// The fake client has no garbage collector: delete both, as the API server would.
	if err := c.Delete(context.Background(), getTask(t, c, "intruder-response-ev-1")); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	if err := c.Delete(context.Background(), &fleetv1.SemanticEvent{
		ObjectMeta: metav1.ObjectMeta{Name: "ev-1", Namespace: triggerNS}}); err != nil {
		t.Fatalf("delete event: %v", err)
	}
	reconcileEvent(t, r, "ev-1")

	if got := getTrigger(t, c, "intruder-response").Status.PendingDrafts; len(got) != 0 {
		t.Errorf("pendingDrafts = %v after the event and its draft were deleted, want empty", got)
	}
}

// Pruning is a transition, not a per-reconcile write (RA-1): once the queue is right,
// further reconciles leave the trigger alone.
func TestTrigger_PruningWritesOnlyWhenTheQueueChanges(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		semanticEvent("ev-1", "yard-sensors", "IntruderDetected"),
	)
	reconcileEvent(t, r, "ev-1")
	clearDraft(t, c, "intruder-response-ev-1")
	reconcileEvent(t, r, "ev-1")

	rv := getTrigger(t, c, "intruder-response").ResourceVersion
	for i := 0; i < 3; i++ {
		reconcileEvent(t, r, "ev-1")
	}
	if got := getTrigger(t, c, "intruder-response").ResourceVersion; got != rv {
		t.Errorf("the trigger was written again (resourceVersion %s → %s) with nothing to prune", rv, got)
	}
}
