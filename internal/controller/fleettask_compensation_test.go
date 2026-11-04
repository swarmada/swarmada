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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The compensation saga (failurePolicy: Compensate), on envtest.
//
// These five ran on the fake client, which does not apply the XValidation rule
// that a member carrying spec.executor may not declare a compensation
// (TestEnvtest_FleetTaskCompensationExcludesExecutor). The saga's whole promise is
// that a partly-completed task is rolled back; the rule exists because an external
// executor is the one thing the control plane cannot roll back. Running the saga
// tests behind that rule means the fixtures they assert on are fixtures a cluster
// would actually hold.
//
// ── local helpers for the compensation saga ─────────────────────────────────

// ftActionComp is ftAction with an optional compensation FleetActionSpec.
func ftActionComp(name string, deps []string, comp bool) fleetv1.FleetTaskAction {
	a := ftAction(name, deps, fleetv1.ActionPhaseSucceeded)
	if comp {
		a.Compensation = &fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}
	}
	return a
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
	e := newFTEnv(t, task,
		childAction("c1", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c1", "b", fleetv1.ActionPhaseFailed),
	)

	e.reconcile("c1")
	ft := e.task("c1")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("phase = %q, want Compensating", ft.Status.Phase)
	}
	if e.action(compChildName("c1", "a")) == nil {
		t.Fatalf("expected compensation child for 'a' to be generated")
	}
	if got := compPhaseOf(ft, "a"); got != compPhaseInProgress {
		t.Errorf("a compensationPhase = %q, want InProgress", got)
	}

	// The compensation succeeds → task Compensated.
	e.setPhase(compChildName("c1", "a"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("c1")
	ft = e.task("c1")
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
	e := newFTEnv(t, task,
		childAction("c2", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c2", "b", fleetv1.ActionPhaseSucceeded),
		childAction("c2", "c", fleetv1.ActionPhaseFailed),
	)

	// Pass 1: only b (the successor) is compensated; a waits.
	e.reconcile("c2")
	ft := e.task("c2")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("phase = %q, want Compensating", ft.Status.Phase)
	}
	if e.action(compChildName("c2", "b")) == nil {
		t.Fatalf("expected b's compensation to be generated first")
	}
	if e.action(compChildName("c2", "a")) != nil {
		t.Fatalf("a's compensation must NOT be generated before b's completes")
	}
	if got := compPhaseOf(ft, "a"); got != compPhasePending {
		t.Errorf("a compensationPhase = %q, want Pending", got)
	}
	if got := compPhaseOf(ft, "b"); got != compPhaseInProgress {
		t.Errorf("b compensationPhase = %q, want InProgress", got)
	}

	// Pass 2: b's compensation succeeds → a's compensation is now generated.
	e.setPhase(compChildName("c2", "b"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("c2")
	ft = e.task("c2")
	if e.action(compChildName("c2", "a")) == nil {
		t.Fatalf("expected a's compensation after b's succeeded")
	}
	if got := compPhaseOf(ft, "a"); got != compPhaseInProgress {
		t.Errorf("a compensationPhase = %q, want InProgress", got)
	}
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("phase = %q, want Compensating (a still undoing)", ft.Status.Phase)
	}

	// Pass 3: a's compensation succeeds → task Compensated.
	e.setPhase(compChildName("c2", "a"), fleetv1.ActionPhaseSucceeded)
	e.reconcile("c2")
	ft = e.task("c2")
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
	e := newFTEnv(t, task,
		childAction("c3", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c3", "b", fleetv1.ActionPhaseFailed),
	)

	e.reconcile("c3") // enters Compensating, generates a's compensation
	e.setPhase(compChildName("c3", "a"), fleetv1.ActionPhaseFailed)
	e.reconcile("c3")

	ft := e.task("c3")
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
	e := newFTEnv(t, task,
		childAction("c4", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c4", "b", fleetv1.ActionPhaseFailed),
	)

	e.reconcile("c4")
	ft := e.task("c4")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensated {
		t.Fatalf("phase = %q, want Compensated", ft.Status.Phase)
	}
	if e.action(compChildName("c4", "a")) != nil {
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

	e := newFTEnv(t, task,
		childAction("c5", "a", fleetv1.ActionPhaseSucceeded),
		childAction("c5", "b", fleetv1.ActionPhaseInProgress),
		childAction("c5", "x", fleetv1.ActionPhaseFailed),
	)

	e.reconcile("c5")
	ft := e.task("c5")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Fatalf("phase = %q, want Compensating", ft.Status.Phase)
	}
	// b (still running) must have been cancelled, and a's undo must NOT have started yet.
	if b := e.action(childName("c5", "b")); b == nil || b.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Errorf("running successor 'b' should be desiredState Cancelled, got %+v", b)
	}
	if e.action(compChildName("c5", "a")) != nil {
		t.Fatalf("a's compensation must not start while successor 'b' is not yet terminal")
	}

	// b finishes cancelling → a's compensation is now eligible.
	e.setPhase(childName("c5", "b"), fleetv1.ActionPhaseCancelled)
	e.reconcile("c5")
	if e.action(compChildName("c5", "a")) == nil {
		t.Fatalf("a's compensation should start once 'b' is terminal")
	}
}
