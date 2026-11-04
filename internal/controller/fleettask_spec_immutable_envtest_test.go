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

// spec.inputs and spec.flow are immutable after a FleetTask is created
// (RFC-0001 #fleettask-composition-dependency-graph). A task edited mid-run would
// otherwise disagree with the children it already created: a child copies what it
// consumed when it is created, and a later edit to the task never reaches it. The
// rules are CEL transition rules, so they are proven against the real API server,
// with no webhook in the way: they hold on an install that serves none.

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const (
	inputsImmutableRule = "spec.inputs is immutable after creation"
	flowImmutableRule   = "spec.flow is immutable after creation"
)

func TestEnvtest_FleetTaskInputsAndFlowAreImmutable(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	def := "north"
	withBoth := celFixture("ft-immutable", ns)
	withBoth.Spec.Inputs = []fleetv1.TaskInput{{Name: "region", Kind: fleetv1.ValueKindString, Default: &def}}
	withBoth.Spec.Flow = &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{{Run: strPtr("a")}}}
	bare := celFixture("ft-bare", ns)
	for _, ft := range []*fleetv1.FleetTask{withBoth, bare} {
		if err := envK8s.Create(ctx, ft); err != nil {
			t.Fatalf("creating %s: %v", ft.Name, err)
		}
	}

	update := func(name string, mutate func(*fleetv1.FleetTask)) error {
		cur := &fleetv1.FleetTask{}
		if err := envK8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cur); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		base := cur.DeepCopy()
		mutate(cur)
		return envK8s.Patch(ctx, cur, client.MergeFrom(base))
	}
	refused := func(what, rule string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s was admitted; the task would then disagree with the children it already created", what)
			return
		}
		if !strings.Contains(err.Error(), rule) {
			t.Errorf("%s was refused, but not by %q: %v", what, rule, err)
		}
	}

	// Writes that leave both fields alone stay possible on a running task.
	if err := update("ft-immutable", func(*fleetv1.FleetTask) {}); err != nil {
		t.Errorf("a no-op update was refused: %v", err)
	}
	if err := update("ft-immutable", func(ft *fleetv1.FleetTask) {
		ft.Labels = map[string]string{"shift": "night"}
	}); err != nil {
		t.Errorf("a label update was refused: %v", err)
	}

	// spec.inputs: change, add, remove, clear, and give inputs to a task born without.
	other := "south"
	refused("changing an input's default", inputsImmutableRule, update("ft-immutable", func(ft *fleetv1.FleetTask) {
		ft.Spec.Inputs[0].Default = &other
	}))
	refused("adding an input", inputsImmutableRule, update("ft-immutable", func(ft *fleetv1.FleetTask) {
		ft.Spec.Inputs = append(ft.Spec.Inputs, fleetv1.TaskInput{Name: "speed", Kind: fleetv1.ValueKindNumber})
	}))
	refused("clearing spec.inputs", inputsImmutableRule, update("ft-immutable", func(ft *fleetv1.FleetTask) {
		ft.Spec.Inputs = nil
	}))
	refused("giving inputs to a task created without any", inputsImmutableRule, update("ft-bare", func(ft *fleetv1.FleetTask) {
		ft.Spec.Inputs = []fleetv1.TaskInput{{Name: "region", Kind: fleetv1.ValueKindString}}
	}))

	// spec.flow: change, remove, and give a flow to a task born without one.
	refused("changing the flow", flowImmutableRule, update("ft-immutable", func(ft *fleetv1.FleetTask) {
		ft.Spec.Flow = &fleetv1.FlowNode{Run: strPtr("a")}
	}))
	refused("removing the flow", flowImmutableRule, update("ft-immutable", func(ft *fleetv1.FleetTask) {
		ft.Spec.Flow = nil
	}))
	refused("giving a flow to a task created without one", flowImmutableRule, update("ft-bare", func(ft *fleetv1.FleetTask) {
		ft.Spec.Flow = &fleetv1.FlowNode{Run: strPtr("a")}
	}))

	// A TaskTrigger's taskTemplate is a FleetTaskSpec too, and it stays editable: the
	// rules belong to a created task, not to every object that embeds its spec.
	trig := &fleetv1.TaskTrigger{
		ObjectMeta: metav1.ObjectMeta{Name: "tt-editable", Namespace: ns},
		Spec: fleetv1.TaskTriggerSpec{
			Source:       fleetv1.TaskTriggerSourceRef{SemanticEventSource: "yard-sensors", EventType: "IntruderDetected"},
			TaskTemplate: withBoth.Spec,
		},
	}
	if err := envK8s.Create(ctx, trig); err != nil {
		t.Fatalf("creating the trigger: %v", err)
	}
	cur := &fleetv1.TaskTrigger{}
	if err := envK8s.Get(ctx, client.ObjectKeyFromObject(trig), cur); err != nil {
		t.Fatal(err)
	}
	base := cur.DeepCopy()
	cur.Spec.TaskTemplate.Inputs = append(cur.Spec.TaskTemplate.Inputs, fleetv1.TaskInput{Name: "speed", Kind: fleetv1.ValueKindNumber})
	cur.Spec.TaskTemplate.Flow = &fleetv1.FlowNode{Run: strPtr("a")}
	if err := envK8s.Patch(ctx, cur, client.MergeFrom(base)); err != nil {
		t.Errorf("editing a TaskTrigger's template inputs and flow was refused; only a created FleetTask is immutable: %v", err)
	}
}
