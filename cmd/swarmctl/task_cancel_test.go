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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

func testTask(ns, name string, ds fleetv1.DesiredState, members int) *fleetv1.FleetTask {
	acts := make([]fleetv1.FleetTaskAction, members)
	for i := range acts {
		acts[i] = fleetv1.FleetTaskAction{
			Name:   string(rune('a' + i)),
			Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		}
	}
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetTaskSpec{DesiredState: ds, Actions: acts},
	}
}

// `swarmctl cancel task` writes spec.desiredState and NOTHING else. The composite
// controller owns its children and fans the intent out; a CLI that also wrote them
// would be a second writer racing the controller for the same fields.
func TestTaskCancelSetsDesiredStateOnly(t *testing.T) {
	ns := "warehouse-a"
	task := testTask(ns, "restock-7", fleetv1.DesiredStateRunning, 3)
	// A member action that exists independently: the CLI must not touch it.
	child := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "restock-7-a", Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, DesiredState: fleetv1.DesiredStateRunning},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task, child).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.taskCancel(context.Background(), c, authorizer(true), ns, "restock-7", "", true); err != nil {
		t.Fatalf("cancel task: %v", err)
	}

	got := &fleetv1.FleetTask{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "restock-7"}, got)
	if got.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Errorf("desiredState = %q, want Cancelled", got.Spec.DesiredState)
	}

	gotChild := &fleetv1.FleetAction{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "restock-7-a"}, gotChild)
	if gotChild.Spec.DesiredState != fleetv1.DesiredStateRunning {
		t.Errorf("the CLI wrote a member action (desiredState = %q); only the controller may",
			gotChild.Spec.DesiredState)
	}

	// The operator is told the blast radius, not just that something happened.
	if !strings.Contains(out.String(), "3 member action(s)") {
		t.Errorf("output does not name the member count: %q", out.String())
	}
}

// Fail closed: a denied cancel writes nothing.
func TestTaskCancelDeniedFailsClosed(t *testing.T) {
	ns := "warehouse-a"
	task := testTask(ns, "restock-7", fleetv1.DesiredStateRunning, 1)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task).Build()
	o := newTestOptions(&bytes.Buffer{}, cli.OutputTable)

	err := o.taskCancel(context.Background(), c, authorizer(false), ns, "restock-7", "", true)
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("expected denial, got %v", err)
	}
	got := &fleetv1.FleetTask{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "restock-7"}, got)
	if got.Spec.DesiredState == fleetv1.DesiredStateCancelled {
		t.Error("a denied cancel must not write desiredState")
	}
}

// Re-cancelling is a no-op that says so. desiredState is level-triggered, so a
// repeat write is harmless — but reporting "cancelled" twice would let an operator
// think a second, stuck cancellation had been re-issued.
func TestTaskCancelAlreadyCancellingIsANoop(t *testing.T) {
	ns := "warehouse-a"
	task := testTask(ns, "restock-7", fleetv1.DesiredStateCancelled, 2)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.taskCancel(context.Background(), c, authorizer(true), ns, "restock-7", "", true); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if !strings.Contains(out.String(), "already cancelling") {
		t.Errorf("want an already-cancelling notice, got %q", out.String())
	}
}

// The reason has nowhere to live on a FleetTask — there is no cancel-requested
// annotation, because the mechanism is a spec write rather than a request the
// controller finalizes. Inventing an annotation would add an API field RFC-0001
// does not define, so the CLI says so instead of dropping it silently.
func TestTaskCancelReasonIsDisclosedNotInvented(t *testing.T) {
	ns := "warehouse-a"
	task := testTask(ns, "restock-7", fleetv1.DesiredStateRunning, 1)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(task).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.taskCancel(context.Background(), c, authorizer(true), ns, "restock-7", "shift ended", true); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if !strings.Contains(out.String(), "--reason is not recorded on a FleetTask") {
		t.Errorf("a dropped --reason must be disclosed, got %q", out.String())
	}
	got := &fleetv1.FleetTask{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "restock-7"}, got)
	for k := range got.Annotations {
		if strings.Contains(k, "cancel") {
			t.Errorf("the CLI invented annotation %q; RFC-0001 defines no such field on FleetTask", k)
		}
	}
}
