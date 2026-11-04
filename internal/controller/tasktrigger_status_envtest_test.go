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
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// draftCount and pendingDrafts against a REAL API server: an event evaluated twice
// before its phase lands counts one draft, and a draft a person clears or deletes
// leaves the queue.
func TestEnvtest_TaskTriggerStatusCountsDraftsAndPrunesTheQueue(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	src := &fleetv1.SemanticEventSource{
		ObjectMeta: metav1.ObjectMeta{Name: "building-doors", Namespace: ns},
		Spec:       fleetv1.SemanticEventSourceSpec{EventTypes: []string{"door.opened.after-hours"}},
	}
	trig := &fleetv1.TaskTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "door-after-hours-patrol", Namespace: ns},
		Spec: fleetv1.TaskTriggerSpec{
			Source: fleetv1.TaskTriggerSourceRef{SemanticEventSource: "building-doors", EventType: "door.opened.after-hours"},
			TaskTemplate: fleetv1.FleetTaskSpec{
				CompletionPolicy: fleetv1.CompletionPolicyAll,
				Actions: []fleetv1.FleetTaskAction{{
					Name: "patrol",
					Action: fleetv1.FleetActionSpec{
						Type: fleetv1.ActionTypePatrol, Zone: "warehouse-a", Priority: fleetv1.ActionPriorityNormal,
					},
				}},
			},
		},
	}
	if err := envK8s.Create(ctx, src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := envK8s.Create(ctx, trig); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	r := &TaskTriggerReconciler{Client: envK8s, APIReader: envK8s, Scheme: envScheme}
	fire := func(name string) {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}
	status := func() fleetv1.TaskTriggerStatus {
		t.Helper()
		var tt fleetv1.TaskTrigger
		if err := envK8s.Get(ctx, types.NamespacedName{Name: trig.Name, Namespace: ns}, &tt); err != nil {
			t.Fatalf("get trigger: %v", err)
		}
		return tt.Status
	}
	draftOf := func(ev string) *fleetv1.FleetTask {
		t.Helper()
		var ft fleetv1.FleetTask
		if err := envK8s.Get(ctx, types.NamespacedName{Name: draftTaskName(trig.Name, ev), Namespace: ns}, &ft); err != nil {
			t.Fatalf("get draft for %s: %v", ev, err)
		}
		return &ft
	}

	for _, ev := range []string{"evb-1", "evb-2"} {
		if err := envK8s.Create(ctx, &fleetv1.SemanticEvent{
			ObjectMeta: metav1.ObjectMeta{Name: ev, Namespace: ns},
			Spec:       fleetv1.SemanticEventSpec{Source: "building-doors", Type: "door.opened.after-hours"},
		}); err != nil {
			t.Fatalf("create event %s: %v", ev, err)
		}
		fire(ev)
	}

	// evb-1 evaluated a second time, as if read before its Accepted phase was visible.
	var ev1 fleetv1.SemanticEvent
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "evb-1", Namespace: ns}, &ev1); err != nil {
		t.Fatalf("get evb-1: %v", err)
	}
	ev1.Status = fleetv1.SemanticEventStatus{}
	if err := envK8s.Status().Update(ctx, &ev1); err != nil {
		t.Fatalf("reset evb-1 status: %v", err)
	}
	fire("evb-1")
	if got := status(); got.DraftCount != 2 || len(got.PendingDrafts) != 2 {
		t.Fatalf("after two events (one evaluated twice): draftCount %d, pendingDrafts %v; want 2 and both drafts",
			got.DraftCount, got.PendingDrafts)
	}

	// A person clears evb-1's draft.
	d1 := draftOf("evb-1")
	d1.Spec.Draft = false
	if err := envK8s.Update(ctx, d1); err != nil {
		t.Fatalf("clear draft: %v", err)
	}
	fire("evb-1")
	if got := status().PendingDrafts; containsString(got, d1.Name) || !containsString(got, draftTaskName(trig.Name, "evb-2")) {
		t.Errorf("pendingDrafts = %v after %s was cleared, want only evb-2's draft", got, d1.Name)
	}

	// evb-2's draft is deleted.
	if err := envK8s.Delete(ctx, draftOf("evb-2")); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	fire("evb-2")
	got := status()
	if len(got.PendingDrafts) != 0 {
		t.Errorf("pendingDrafts = %v after one draft was cleared and the other deleted, want empty", got.PendingDrafts)
	}
	if got.DraftCount != 2 {
		t.Errorf("draftCount = %d, want 2: clearing or deleting a draft does not un-produce it", got.DraftCount)
	}
}
