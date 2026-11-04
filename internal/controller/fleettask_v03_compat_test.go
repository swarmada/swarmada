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

// ABSENT FLOW, BEHAVIOUR IS IDENTICAL TO v0.3 — the first of S3's
// non-negotiable rules, verified against an actual v0.3 FleetTask fixture
// rather than against a fixture written to demonstrate it.
//
// # WHY THE SHIPPED SAMPLE AND NOT A PURPOSE-BUILT FIXTURE
//
// The other dependsOn tests in this package build their own fixtures, which
// means they establish that the dependsOn path still works for objects THIS
// SUITE writes. That is not the same claim. `config/samples/fleettask_sample.yaml`
// is a real v0.3 object — checked in before flow existed, documented as the
// composite example an operator applies, and the thing that would actually
// break if adding `spec.flow` had perturbed the pre-flow path. Driving it
// through the reconciler ties the promise to the artefact it is a promise about.
//
// test/samples/samples_test.go already guards the same file for strict DECODING
// — a renamed field. This is the other half: decoding cleanly and then
// reconciling differently is exactly the failure a decode check cannot see.

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

// loadSampleFleetTask decodes the checked-in v0.3 composite sample STRICTLY, so
// a field this API no longer has is an error rather than a silently ignored key.
func loadSampleFleetTask(t *testing.T, namespace string) *fleetv1.FleetTask {
	t.Helper()
	path := filepath.Join("..", "..", "config", "samples", "fleettask_sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	doc, err := utilyaml.ToJSON(raw)
	if err != nil {
		t.Fatalf("converting %s to JSON: %v", path, err)
	}
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	ser := json.NewSerializerWithOptions(json.DefaultMetaFactory, scheme, scheme,
		json.SerializerOptions{Strict: true})
	var task fleetv1.FleetTask
	if _, _, err := ser.Decode(doc, nil, &task); err != nil {
		t.Fatalf("strict-decoding %s against the current api/v1: %v", path, err)
	}
	// The test harness reconciles in its own namespace.
	task.Namespace = namespace
	task.ResourceVersion = ""
	return &task
}

// THE RULE. The shipped v0.3 sample carries no spec.flow, and reconciling it
// takes the dependsOn path exactly as it did before flow existed: the root
// member is generated, the dependent is withheld until its predecessor
// succeeds, and the task runs on completionPolicy rather than on a root node's
// outcome.
func TestFleetTask_ShippedV03Sample_TakesTheDependsOnPathUnchanged(t *testing.T) {
	task := loadSampleFleetTask(t, actionNS)

	// The premise, asserted rather than assumed: if someone adds a flow to the
	// sample, this test is measuring something else and should say so loudly.
	if task.Spec.Flow != nil {
		t.Fatal("config/samples/fleettask_sample.yaml has grown a spec.flow; it is the " +
			"PRE-FLOW fixture and this test's whole premise is that it has none")
	}
	if len(task.Spec.Actions) != 3 {
		t.Fatalf("the sample declares %d members, want the 3 the dependsOn chain needs",
			len(task.Spec.Actions))
	}

	r, c := newFleetTaskReconciler(t, task)
	reconcileFT(t, r, task.Name)

	// Root member generated.
	if getActionOrNil(t, c, childName(task.Name, "approach-dock")) == nil {
		t.Fatal("the root member's child was not generated; the v0.3 dependsOn path no longer starts")
	}
	// Dependents withheld — the gate dependsOn exists for.
	for _, blocked := range []string{"inspect-dock", "return-to-bay"} {
		if getActionOrNil(t, c, childName(task.Name, blocked)) != nil {
			t.Fatalf("member %q was generated before its predecessor succeeded; the dependsOn gate is not holding", blocked)
		}
	}
	if got := getFT(t, c, task.Name).Status.Phase; got != fleetv1.FleetTaskPhaseRunning {
		t.Fatalf("phase = %q, want Running", got)
	}

	// Advance the chain one link and confirm the gate OPENS — a test that only
	// showed members being withheld would pass on a controller that generated
	// nothing at all.
	succeed(t, c, childName(task.Name, "approach-dock"))
	reconcileFT(t, r, task.Name)
	if getActionOrNil(t, c, childName(task.Name, "inspect-dock")) == nil {
		t.Fatal("the dependent member was not generated after its predecessor succeeded; the gate opens for nobody")
	}
	if getActionOrNil(t, c, childName(task.Name, "return-to-bay")) != nil {
		t.Fatal("the third member was generated a link early; dependsOn is not being walked in order")
	}
}

// No flow means no flow bookkeeping. A task with no loop must carry no
// iteration counters and no FlowValid condition: the fields are additive, and a
// purely additive field that writes itself onto every object is not additive.
func TestFleetTask_ShippedV03Sample_WritesNoFlowBookkeeping(t *testing.T) {
	task := loadSampleFleetTask(t, actionNS)
	r, c := newFleetTaskReconciler(t, task)
	reconcileFT(t, r, task.Name)

	got := getFT(t, c, task.Name)
	for _, m := range got.Status.Actions {
		if m.Iteration != 0 || m.IterationsCompleted != 0 || m.IterationsFailed != 0 {
			t.Errorf("member %q carries loop bookkeeping (iteration=%d completed=%d failed=%d) on a task with no flow",
				m.Name, m.Iteration, m.IterationsCompleted, m.IterationsFailed)
		}
		if len(m.RecentIterations) != 0 {
			t.Errorf("member %q carries %d iteration records on a task with no flow", m.Name, len(m.RecentIterations))
		}
	}
	for _, cond := range got.Status.Conditions {
		if cond.Type == "FlowValid" || cond.Type == "FlowEvaluable" {
			t.Errorf("a task with no flow carries a %q condition", cond.Type)
		}
	}
}

// The evaluator is never CONSULTED for a task with no flow, and the two tests
// above are what establish that: the sample takes the dependsOn path and carries
// no flow bookkeeping. This third one pins the reason the guard has to be at the
// CALLER rather than inside the evaluator — an empty node settles Succeeded
// immediately, by design.
//
// `{}` is the explicit no-op the grammar requires: `select.default` is REQUIRED,
// so an author with nothing further to do writes `{}`, and that has to mean
// "this branch is done" rather than "this branch is stuck". A nil tree takes the
// same arm. Which is precisely why `spec.flow == nil` must be branched on BEFORE
// the evaluator is entered — handing it a nil tree would report the task
// Succeeded without running anything, and the fact that it does not is the
// controller's guard, not the evaluator's.
func TestFlowEvaluate_AnEmptyNodeSettlesSucceededWhichIsWhyTheGuardIsAtTheCaller(t *testing.T) {
	got := flow.Evaluate(nil, &flow.Context{
		Child: func(string, int32) flow.ChildState { return flow.ChildState{} },
	})
	if got.Outcome != flow.OutcomeSucceeded {
		t.Fatalf("outcome = %s, want Succeeded — `{}` is the explicit no-op select.default relies on", got.Outcome)
	}
	// It asks for nothing, so the no-op cannot start or cancel work.
	if len(got.Ensure) != 0 || len(got.Cancel) != 0 {
		t.Fatalf("the no-op node asked for work: ensure=%v cancel=%v", got.Ensure, got.Cancel)
	}
}

// succeed drives one child FleetAction to Succeeded, so the dependsOn gate above
// has something to open on.
func succeed(t *testing.T, c client.Client, name string) {
	t.Helper()
	child := getActionOrNil(t, c, name)
	if child == nil {
		t.Fatalf("no child %q to complete", name)
	}
	orig := child.DeepCopy()
	child.Status.Phase = fleetv1.ActionPhaseSucceeded
	if err := c.Status().Patch(t.Context(), child, client.MergeFrom(orig)); err != nil {
		t.Fatalf("completing %q: %v", name, err)
	}
}
