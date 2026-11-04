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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// THE DRAFT GATE HAS TO BE VISIBLE, NOT JUST CLOSED.
//
// ADR-0062 puts a person between a semantic event and physical work: an event-produced
// FleetTask is born with spec.draft true, the controller generates nothing, and it parks
// the task at status.phase Draft with AwaitingActivation=True so an operator can see
// there is something to approve. The fake client does not enforce the CRD's enum on
// status.phase, so every existing test of holdAsDraft passes whether or not the API
// server would accept the write. This one asks the API server.
//
// The failure mode it guards is quiet: if the status write is refused, the task still
// generates no child (the gate is closed), so nothing moves — but the reconcile errors
// and requeues forever, and the operator waiting for a Draft to approve sees a task with
// no phase at all.
func TestEnvtest_FleetTaskDraftPhaseIsAdmittedAndVisible(t *testing.T) {
	e := newFTEnv(t, &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "proposed"},
		Spec: fleetv1.FleetTaskSpec{
			Draft:            true,
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
	})

	if _, err := e.r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: e.ns, Name: "proposed"}}); err != nil {
		t.Fatalf("reconciling a draft FleetTask failed against a real API server: %v\n"+
			"The controller parks a draft at status.phase %q; if the CRD's phase enum does not "+
			"list it, the status write is refused, the reconcile requeues forever, and the draft "+
			"gate is never observable.", err, fleetv1.FleetTaskPhaseDraft)
	}

	got := e.task("proposed")
	if got.Status.Phase != fleetv1.FleetTaskPhaseDraft {
		t.Fatalf("status.phase = %q, want %q", got.Status.Phase, fleetv1.FleetTaskPhaseDraft)
	}
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionAwaitingActivation) {
		t.Fatalf("condition %s is not True on the stored object: %+v",
			ConditionAwaitingActivation, got.Status.Conditions)
	}

	var actions fleetv1.FleetActionList
	if err := envK8s.List(context.Background(), &actions); err != nil {
		t.Fatal(err)
	}
	for _, a := range actions.Items {
		if a.Namespace == e.ns {
			t.Fatalf("a draft generated FleetAction %q; a draft dispatches nothing", a.Name)
		}
	}

	// Idempotent and quiet: a second pass over a parked draft writes nothing.
	rv := got.ResourceVersion
	e.reconcile("proposed")
	if again := e.task("proposed"); again.ResourceVersion != rv {
		t.Fatalf("a second reconcile rewrote a parked draft (resourceVersion %s → %s)",
			rv, again.ResourceVersion)
	}

	// Activation: a person clears spec.draft. The task leaves Draft, and the condition
	// that told the operator it was waiting for them stops saying so — a Running task
	// still reporting AwaitingActivation=True would ask for an approval already given.
	activate := e.task("proposed")
	activate.Spec.Draft = false
	if err := envK8s.Update(context.Background(), activate); err != nil {
		t.Fatalf("clearing spec.draft: %v", err)
	}
	e.reconcile("proposed")
	live := e.task("proposed")
	if live.Status.Phase == fleetv1.FleetTaskPhaseDraft {
		t.Fatalf("status.phase is still %q after spec.draft was cleared", live.Status.Phase)
	}
	if apimeta.IsStatusConditionTrue(live.Status.Conditions, ConditionAwaitingActivation) {
		t.Fatalf("an activated task (phase %q) still reports %s=True", live.Status.Phase,
			ConditionAwaitingActivation)
	}
}
