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
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The TaskTrigger path against a REAL API server.
//
// The fake-client tests cover the controller's logic; this covers what only a real
// API server can answer — that the three new kinds are actually in the generated
// CRDs, that their schemas admit the objects the public sample writes, and that a
// field written on one is not pruned on the way back. A kind that is declared in Go
// and missing from the CRD behaves, against the fake client, exactly like one that
// works.

func TestEnvtest_TaskTriggerDraftsAndDoesNotDispatch(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	src := &fleetv1.SemanticEventSource{
		ObjectMeta: metav1.ObjectMeta{Name: "yard-sensors", Namespace: ns},
		Spec: fleetv1.SemanticEventSourceSpec{
			DisplayName: "Perimeter sensors",
			EventTypes:  []string{"IntruderDetected", "ShiftStarted"},
		},
	}
	if err := envK8s.Create(ctx, src); err != nil {
		t.Fatalf("the API server rejected a SemanticEventSource: %v", err)
	}

	trig := &fleetv1.TaskTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "intruder-response", Namespace: ns},
		Spec: fleetv1.TaskTriggerSpec{
			Source: fleetv1.TaskTriggerSourceRef{
				SemanticEventSource: "yard-sensors",
				EventType:           "IntruderDetected",
			},
			TaskTemplate: fleetv1.FleetTaskSpec{
				CompletionPolicy: fleetv1.CompletionPolicyAll,
				Actions: []fleetv1.FleetTaskAction{{
					Name: "investigate",
					Action: fleetv1.FleetActionSpec{
						Type:     fleetv1.ActionTypeNavigate,
						Zone:     "yard-west",
						Priority: fleetv1.ActionPriorityHigh,
					},
				}},
			},
		},
	}
	if err := envK8s.Create(ctx, trig); err != nil {
		t.Fatalf("the API server rejected a TaskTrigger: %v", err)
	}

	ev := &fleetv1.SemanticEvent{
		ObjectMeta: metav1.ObjectMeta{Name: "ev-1", Namespace: ns},
		Spec: fleetv1.SemanticEventSpec{
			Source:  "yard-sensors",
			Type:    "IntruderDetected",
			Payload: map[string]string{"camera": "cam-07", "confidence": "0.94"},
		},
	}
	if err := envK8s.Create(ctx, ev); err != nil {
		t.Fatalf("the API server rejected a SemanticEvent: %v", err)
	}
	// The payload must survive the round trip, or a trigger author would be writing
	// data the API server silently discards.
	var readBack fleetv1.SemanticEvent
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "ev-1", Namespace: ns}, &readBack); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if readBack.Spec.Payload["camera"] != "cam-07" {
		t.Errorf("payload did not round-trip: %v", readBack.Spec.Payload)
	}

	r := &TaskTriggerReconciler{Client: envK8s, Scheme: envK8s.Scheme()}
	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ev-1", Namespace: ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var task fleetv1.FleetTask
	if err := envK8s.Get(ctx, types.NamespacedName{
		Name: "intruder-response-ev-1", Namespace: ns}, &task); err != nil {
		t.Fatalf("no draft FleetTask was created: %v", err)
	}
	if !task.Spec.Draft {
		t.Fatal("the drafted FleetTask came back with spec.draft FALSE. Either the controller " +
			"did not set it or the field is absent from the generated CRD and was pruned — " +
			"and the second is indistinguishable from the first without a real API server. " +
			"Either way this is auto-dispatch")
	}

	// The negative half, against a real API server: nothing was dispatched.
	var actions fleetv1.FleetActionList
	if err := envK8s.List(ctx, &actions, client.InNamespace(ns)); err != nil {
		t.Fatalf("list actions: %v", err)
	}
	if len(actions.Items) != 0 {
		t.Fatalf("%d FleetAction(s) exist after a trigger fired. A draft dispatches nothing",
			len(actions.Items))
	}
}
