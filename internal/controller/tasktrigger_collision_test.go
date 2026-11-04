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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The draft name is deterministic, so a create can collide with a FleetTask that is
// already there. That is success only when the task there is this event's own draft,
// which the draft records as its controller reference. Any other task of that name
// is somebody else's, and listing it as drafted would report a held draft that may be
// running work.
//
// The fake client assigns no UIDs, and a controller reference is matched by UID, so
// every event here carries one.

func eventWithUID(name, source, eventType string, uid types.UID) *fleetv1.SemanticEvent {
	e := semanticEvent(name, source, eventType)
	e.UID = uid
	return e
}

func getEvent(t *testing.T, r *TaskTriggerReconciler, name string) *fleetv1.SemanticEvent {
	t.Helper()
	var ev fleetv1.SemanticEvent
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: triggerNS}, &ev); err != nil {
		t.Fatalf("get event: %v", err)
	}
	return &ev
}

// foreignTask is a running, non-draft FleetTask that happens to carry the name the
// trigger would draft under.
func foreignTask(name string) *fleetv1.FleetTask {
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: triggerNS, UID: "uid-foreign-task"},
		Spec: fleetv1.FleetTaskSpec{
			Actions: []fleetv1.FleetTaskAction{{
				Name:   "patrol",
				Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
			}},
		},
	}
}

func TestTrigger_CollisionWithAForeignTaskIsNotRecordedAsDrafted(t *testing.T) {
	controlled := true
	for _, tc := range []struct {
		name  string
		owner []metav1.OwnerReference
		// whose is how the message names the controller, or that there is none.
		whose string
	}{
		{name: "no controller", whose: "has no controller"},
		{
			// Same kind and name as the event, another UID: an earlier event of that
			// name whose draft outlived it. It is not this event's draft.
			name: "an earlier event of the same name",
			owner: []metav1.OwnerReference{{
				APIVersion: fleetv1.GroupVersion.String(), Kind: "SemanticEvent",
				Name: "ev-1", UID: "uid-earlier-ev-1", Controller: &controlled,
			}},
			whose: `SemanticEvent "ev-1" (uid uid-earlier-ev-1)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := foreignTask("intruder-response-ev-1")
			existing.OwnerReferences = tc.owner
			r, c := newTriggerReconciler(t,
				eventSource("yard-sensors", "IntruderDetected"),
				taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
				eventWithUID("ev-1", "yard-sensors", "IntruderDetected", "uid-ev-1"),
				existing,
			)

			reconcileEvent(t, r, "ev-1")

			ev := getEvent(t, r, "ev-1")
			if ev.Status.Phase != fleetv1.SemanticEventAccepted {
				t.Errorf("phase = %q, want Accepted: a draft-name collision is neither cause of Rejected", ev.Status.Phase)
			}
			if len(ev.Status.DraftedTasks) != 0 {
				t.Errorf("draftedTasks = %v, want none. The FleetTask of that name is not this event's draft, "+
					"and listing it reports a held draft that may be running work", ev.Status.DraftedTasks)
			}
			for _, want := range []string{`FleetTask "intruder-response-ev-1"`, `TaskTrigger "intruder-response"`, tc.whose} {
				if !strings.Contains(ev.Status.Message, want) {
					t.Errorf("status.message = %q, want it to name %s", ev.Status.Message, want)
				}
			}

			tt := getTrigger(t, c, "intruder-response")
			if tt.Status.DraftCount != 0 || len(tt.Status.PendingDrafts) != 0 || tt.Status.LastFiredAt != nil {
				t.Errorf("trigger status = draftCount %d, pendingDrafts %v, lastFiredAt %v: a collision with "+
					"another task is not a firing", tt.Status.DraftCount, tt.Status.PendingDrafts, tt.Status.LastFiredAt)
			}

			task := getTask(t, c, "intruder-response-ev-1")
			if task.Spec.Draft || len(task.Spec.Actions) != 1 || task.Spec.Actions[0].Name != "patrol" {
				t.Errorf("the existing task was changed: draft %v, actions %#v", task.Spec.Draft, task.Spec.Actions)
			}
		})
	}
}

// The collision a lost status write causes: the event is evaluated again, its create
// collides with the draft it made the first time, and that is still success.
func TestTrigger_ReevaluatingTheSameEventKeepsItsOwnDraft(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		eventWithUID("ev-1", "yard-sensors", "IntruderDetected", "uid-ev-1"),
	)

	reconcileEvent(t, r, "ev-1")
	forgetEvaluation(t, c, "ev-1")
	reconcileEvent(t, r, "ev-1")

	ev := getEvent(t, r, "ev-1")
	if ev.Status.Phase != fleetv1.SemanticEventAccepted {
		t.Errorf("phase = %q, want Accepted", ev.Status.Phase)
	}
	if len(ev.Status.DraftedTasks) != 1 || ev.Status.DraftedTasks[0] != "intruder-response-ev-1" {
		t.Errorf("draftedTasks = %v, want [intruder-response-ev-1]: the collision is with this event's own draft",
			ev.Status.DraftedTasks)
	}
	if want := "accepted; drafted 1 task(s) AWAITING ACTIVATION"; ev.Status.Message != want {
		t.Errorf("status.message = %q, want %q", ev.Status.Message, want)
	}
	if got := getTrigger(t, c, "intruder-response").Status.DraftCount; got != 1 {
		t.Errorf("draftCount = %d, want 1", got)
	}
}

// One trigger collides and one does not. The one that does not drafts exactly as it
// would alone.
func TestTrigger_ACollisionLeavesTheOtherTriggersFiring(t *testing.T) {
	r, c := newTriggerReconciler(t,
		eventSource("yard-sensors", "IntruderDetected"),
		taskTrigger("intruder-response", "yard-sensors", "IntruderDetected"),
		taskTrigger("notify-security", "yard-sensors", "IntruderDetected"),
		eventWithUID("ev-1", "yard-sensors", "IntruderDetected", "uid-ev-1"),
		foreignTask("intruder-response-ev-1"),
	)

	reconcileEvent(t, r, "ev-1")

	ev := getEvent(t, r, "ev-1")
	if len(ev.Status.DraftedTasks) != 1 || ev.Status.DraftedTasks[0] != "notify-security-ev-1" {
		t.Errorf("draftedTasks = %v, want [notify-security-ev-1]", ev.Status.DraftedTasks)
	}
	if !strings.HasPrefix(ev.Status.Message, "accepted; drafted 1 task(s) AWAITING ACTIVATION; ") ||
		!strings.Contains(ev.Status.Message, `FleetTask "intruder-response-ev-1"`) {
		t.Errorf("status.message = %q, want the one draft and the conflict", ev.Status.Message)
	}
	draft := getTask(t, c, "notify-security-ev-1")
	if !draft.Spec.Draft || !metav1.IsControlledBy(draft, ev) {
		t.Errorf("notify-security's draft: draft %v, controlled by the event %v", draft.Spec.Draft, metav1.IsControlledBy(draft, ev))
	}
	if got := getTrigger(t, c, "notify-security").Status.DraftCount; got != 1 {
		t.Errorf("notify-security draftCount = %d, want 1", got)
	}
	if got := getTrigger(t, c, "intruder-response").Status.DraftCount; got != 0 {
		t.Errorf("intruder-response draftCount = %d, want 0", got)
	}
}
