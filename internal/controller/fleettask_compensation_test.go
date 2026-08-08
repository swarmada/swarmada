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

// ── local helpers for the compensation saga (failurePolicy: Compensate) ──────────

// ftActionComp is ftAction with an optional compensation FleetActionSpec.
func ftActionComp(name string, deps []string, comp bool) fleetv1.FleetTaskAction {
	a := ftAction(name, deps, fleetv1.ActionPhaseSucceeded)
	if comp {
		a.Compensation = &fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}
	}
	return a
}

// setActionPhase drives an existing (primary or compensation) child FleetAction to a phase.
func setActionPhase(t *testing.T, c client.Client, name string, phase fleetv1.ActionPhase) {
	t.Helper()
	var a fleetv1.FleetAction
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: name}, &a); err != nil {
		t.Fatalf("get action %s: %v", name, err)
	}
	a.Status.Phase = phase
	if err := c.Status().Update(context.Background(), &a); err != nil {
		t.Fatalf("update %s status: %v", name, err)
	}
}

func compPhaseOf(ft *fleetv1.FleetTask, name string) string {
	for i := range ft.Status.Actions {
		if ft.Status.Actions[i].Name == name {
			return ft.Status.Actions[i].CompensationPhase
		}
	}
	return ""
}

func hasCond(ft *fleetv1.FleetTask, typ string, status metav1.ConditionStatus, reason string) bool {
	for _, c := range ft.Status.Conditions {
		if c.Type == typ && c.Status == status && c.Reason == reason {
			return true
		}
	}
	return false
}

// A Succeeded action with a declared compensation is undone when the task fails under Compensate:
// the controller enters Compensating, generates the compensation child, and reaches Compensated
// once that child succeeds.
func TestFleetTask_CompensateSingle(t *testing.T) {
	task := fleetTask("c1", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyCompensate, fleetv1.DesiredStateRunning,
		ftActionComp("a", nil, true),
		ftActionComp("b", []string{"a"}, false),
	)
	r, c := newFleetTaskReconciler(t, task,
		childAction("c1", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c1", "b", fleetv1.ActionPhaseFailed),
	)

	reconcileFT(t, r, "c1")
	ft := getFT(t, c, "c1")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("phase = %q, want Compensating", ft.Status.Phase)
	}
	if getActionOrNil(t, c, compChildName("c1", "a")) == nil {
		t.Fatalf("expected compensation child for 'a' to be generated")
	}
	if got := compPhaseOf(ft, "a"); got != compPhaseInProgress {
		t.Errorf("a compensationPhase = %q, want InProgress", got)
	}

	// The compensation succeeds → task Compensated.
	setActionPhase(t, c, compChildName("c1", "a"), fleetv1.ActionPhaseSucceeded)
	reconcileFT(t, r, "c1")
	ft = getFT(t, c, "c1")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensated {
		t.Fatalf("phase = %q, want Compensated", ft.Status.Phase)
	}
	if ft.Status.CompletionTime == nil {
		t.Errorf("expected CompletionTime to be set on a terminal task")
	}
	if !hasCond(ft, "CompensationComplete", metav1.ConditionTrue, "Compensated") {
		t.Errorf("expected CompensationComplete=True/Compensated condition")
	}
}

// Reverse dependency order: with a → b → c and both a and b compensable, the successor b is undone
// before the predecessor a; a's compensation is not generated until b's has succeeded.
func TestFleetTask_CompensateReverseOrder(t *testing.T) {
	task := fleetTask("c2", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyCompensate, fleetv1.DesiredStateRunning,
		ftActionComp("a", nil, true),
		ftActionComp("b", []string{"a"}, true),
		ftActionComp("c", []string{"b"}, false),
	)
	r, c := newFleetTaskReconciler(t, task,
		childAction("c2", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c2", "b", fleetv1.ActionPhaseSucceeded),
		childAction("c2", "c", fleetv1.ActionPhaseFailed),
	)

	// Pass 1: only b (the successor) is compensated; a waits.
	reconcileFT(t, r, "c2")
	ft := getFT(t, c, "c2")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("phase = %q, want Compensating", ft.Status.Phase)
	}
	if getActionOrNil(t, c, compChildName("c2", "b")) == nil {
		t.Fatalf("expected b's compensation to be generated first")
	}
	if getActionOrNil(t, c, compChildName("c2", "a")) != nil {
		t.Fatalf("a's compensation must NOT be generated before b's completes")
	}
	if got := compPhaseOf(ft, "a"); got != compPhasePending {
		t.Errorf("a compensationPhase = %q, want Pending", got)
	}
	if got := compPhaseOf(ft, "b"); got != compPhaseInProgress {
		t.Errorf("b compensationPhase = %q, want InProgress", got)
	}

	// Pass 2: b's compensation succeeds → a's compensation is now generated.
	setActionPhase(t, c, compChildName("c2", "b"), fleetv1.ActionPhaseSucceeded)
	reconcileFT(t, r, "c2")
	ft = getFT(t, c, "c2")
	if getActionOrNil(t, c, compChildName("c2", "a")) == nil {
		t.Fatalf("expected a's compensation after b's succeeded")
	}
	if got := compPhaseOf(ft, "a"); got != compPhaseInProgress {
		t.Errorf("a compensationPhase = %q, want InProgress", got)
	}
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("phase = %q, want Compensating (a still undoing)", ft.Status.Phase)
	}

	// Pass 3: a's compensation succeeds → task Compensated.
	setActionPhase(t, c, compChildName("c2", "a"), fleetv1.ActionPhaseSucceeded)
	reconcileFT(t, r, "c2")
	ft = getFT(t, c, "c2")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensated {
		t.Fatalf("phase = %q, want Compensated", ft.Status.Phase)
	}
}

// A compensation that itself fails makes the task fail closed: phase Failed, no further
// compensations issued, an operator-facing condition set.
func TestFleetTask_CompensateFailsClosed(t *testing.T) {
	task := fleetTask("c3", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyCompensate, fleetv1.DesiredStateRunning,
		ftActionComp("a", nil, true),
		ftActionComp("b", []string{"a"}, false),
	)
	r, c := newFleetTaskReconciler(t, task,
		childAction("c3", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c3", "b", fleetv1.ActionPhaseFailed),
	)

	reconcileFT(t, r, "c3") // enters Compensating, generates a's compensation
	setActionPhase(t, c, compChildName("c3", "a"), fleetv1.ActionPhaseFailed)
	reconcileFT(t, r, "c3")

	ft := getFT(t, c, "c3")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed (fail closed)", ft.Status.Phase)
	}
	if got := compPhaseOf(ft, "a"); got != compPhaseFailed {
		t.Errorf("a compensationPhase = %q, want Failed", got)
	}
	if !hasCond(ft, "CompensationComplete", metav1.ConditionFalse, "CompensationFailed") {
		t.Errorf("expected CompensationComplete=False/CompensationFailed condition")
	}
}

// Under Compensate, a Succeeded action that declared no compensation has nothing to undo, so the
// task reaches Compensated immediately with no compensation children.
func TestFleetTask_CompensateNothingToUndo(t *testing.T) {
	task := fleetTask("c4", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyCompensate, fleetv1.DesiredStateRunning,
		ftActionComp("a", nil, false),
		ftActionComp("b", []string{"a"}, false),
	)
	r, c := newFleetTaskReconciler(t, task,
		childAction("c4", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c4", "b", fleetv1.ActionPhaseFailed),
	)

	reconcileFT(t, r, "c4")
	ft := getFT(t, c, "c4")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensated {
		t.Fatalf("phase = %q, want Compensated", ft.Status.Phase)
	}
	if getActionOrNil(t, c, compChildName("c4", "a")) != nil {
		t.Errorf("no compensation child expected when nothing declares compensation")
	}
}

// Compensate must not begin undoing a Succeeded action while a successor is still running: the
// in-flight successor is cancelled first, and only once it is terminal does the undo start.
func TestFleetTask_CompensateWaitsForRunningSuccessor(t *testing.T) {
	// completionPolicy All with an already-failed independent action 'x' makes the task
	// unmeetable while successor 'b' is still InProgress, forcing Compensate early. 'a' is
	// Succeeded and compensable; its undo must wait until 'b' is terminal.
	task := fleetTask("c5", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyCompensate, fleetv1.DesiredStateRunning,
		ftActionComp("a", nil, true),
		ftActionComp("b", []string{"a"}, false),
		ftActionComp("x", nil, false),
	)

	r, c := newFleetTaskReconciler(t, task,
		childAction("c5", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c5", "b", fleetv1.ActionPhaseInProgress),
		childAction("c5", "x", fleetv1.ActionPhaseFailed),
	)

	reconcileFT(t, r, "c5")
	ft := getFT(t, c, "c5")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("phase = %q, want Compensating", ft.Status.Phase)
	}
	// b (still running) must have been cancelled, and a's undo must NOT have started yet.
	if b := getActionOrNil(t, c, childName("c5", "b")); b == nil || b.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Errorf("running successor 'b' should be desiredState Cancelled, got %+v", b)
	}
	if getActionOrNil(t, c, compChildName("c5", "a")) != nil {
		t.Fatalf("a's compensation must not start while successor 'b' is not yet terminal")
	}

	// b finishes cancelling → a's compensation is now eligible.
	setActionPhase(t, c, childName("c5", "b"), fleetv1.ActionPhaseCancelled)
	reconcileFT(t, r, "c5")
	if getActionOrNil(t, c, compChildName("c5", "a")) == nil {
		t.Fatalf("a's compensation should start once 'b' is terminal")
	}
}
