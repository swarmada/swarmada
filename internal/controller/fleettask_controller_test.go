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

// The FleetTask state machine, on envtest.
//
// These seven tests were written against the fake client, which performs no
// admission: it would have accepted every fixture below whether or not the object
// was legal, so each of them asserted a reconcile path without establishing that
// the path was reachable in a cluster. They now run against a real API server with
// the generated CRDs installed — same assertions, but the fixture has to survive
// the enums, the bounds, the four XValidation rules and CRD defaulting first.
//
// The harness (newFTEnv and friends) lives in fleettask_envtest_test.go; the
// fixture builders and the fake-client harness that the cancel, nil-quorum and
// flow tests still use live in fleettask_fake_test.go.

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A root action is generated; a dependent action is not created until its predecessor is ready.
func TestFleetTask_GeneratesRootBlocksDependent(t *testing.T) {
	task := fleetTask("t1", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task)
	e.reconcile("t1")

	if e.action(childName("t1", "a")) == nil {
		t.Errorf("expected child action 'a' to be generated")
	}
	if e.action(childName("t1", "b")) != nil {
		t.Errorf("child 'b' must not be generated before 'a' succeeds")
	}
	if got := e.task("t1").Status.Phase; got != fleetv1.FleetTaskPhaseRunning {
		t.Errorf("phase = %q, want Running", got)
	}
}

// completionPolicy All: every child Succeeded → task Succeeded.
func TestFleetTask_AllSucceeded(t *testing.T) {
	task := fleetTask("t2", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task,
		childAction("t2", "a", fleetv1.ActionPhaseSucceeded),
		childAction("t2", "b", fleetv1.ActionPhaseSucceeded),
	)
	e.reconcile("t2")

	if got := e.task("t2").Status.Phase; got != fleetv1.FleetTaskPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded", got)
	}
}

// completionPolicy Any: one Succeeded suffices even with another still running.
func TestFleetTask_AnySucceeds(t *testing.T) {
	task := fleetTask("t3", fleetv1.CompletionPolicyAny, fleetv1.FailurePolicyContinueOthers, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task,
		childAction("t3", "a", fleetv1.ActionPhaseSucceeded),
		childAction("t3", "b", fleetv1.ActionPhaseInProgress),
	)
	e.reconcile("t3")

	if got := e.task("t3").Status.Phase; got != fleetv1.FleetTaskPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded (Any)", got)
	}
}

// completionPolicy Quorum: reaching the quorum of successes → Succeeded.
//
// The quorum is set, which the API server now insists on: completionPolicy Quorum
// with no spec.quorum is refused at admission (TestEnvtest_FleetTaskQuorumCELRequired),
// so this fixture could not be written the wrong way here even by accident.
func TestFleetTask_QuorumMet(t *testing.T) {
	q := int32(2)
	task := fleetTask("t4", fleetv1.CompletionPolicyQuorum, fleetv1.FailurePolicyContinueOthers, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("c", nil, fleetv1.ActionPhaseSucceeded),
	)
	task.Spec.Quorum = &q
	e := newFTEnv(t, task,
		childAction("t4", "a", fleetv1.ActionPhaseSucceeded),
		childAction("t4", "b", fleetv1.ActionPhaseSucceeded),
		childAction("t4", "c", fleetv1.ActionPhaseInProgress),
	)
	e.reconcile("t4")

	if got := e.task("t4").Status.Phase; got != fleetv1.FleetTaskPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded (Quorum 2/3)", got)
	}
}

// FailFast: a terminally-failed action fails the task and cancels non-terminal siblings.
func TestFleetTask_FailFastCancelsSiblings(t *testing.T) {
	task := fleetTask("t5", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task,
		childAction("t5", "a", fleetv1.ActionPhaseFailed),
		childAction("t5", "b", fleetv1.ActionPhaseInProgress),
	)
	e.reconcile("t5")

	if got := e.task("t5").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Errorf("phase = %q, want Failed", got)
	}
	if b := e.action(childName("t5", "b")); b == nil || b.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Errorf("sibling 'b' should be desiredState Cancelled, got %+v", b)
	}
}

// desiredState on the task fans out to non-terminal children.
func TestFleetTask_DesiredStateFanout(t *testing.T) {
	task := fleetTask("t6", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStatePaused,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task, childAction("t6", "a", fleetv1.ActionPhaseInProgress))
	e.reconcile("t6")

	if a := e.action(childName("t6", "a")); a == nil || a.Spec.DesiredState != fleetv1.DesiredStatePaused {
		t.Errorf("child 'a' desiredState = %v, want Paused", a)
	}
}

// A dependency cycle is rejected: task → Failed with DependencyGraphValid=False.
//
// As with the unknown-dependency case (TestFleetTask_UnknownDependencyIsFailed),
// the API server accepts this object: no CEL rule on FleetTaskSpec inspects the
// dependency graph, so validateGraph is the only thing that catches it.
func TestFleetTask_CycleIsFailed(t *testing.T) {
	task := fleetTask("t7", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", []string{"b"}, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
	)
	e := newFTEnv(t, task)
	e.reconcile("t7")

	ft := e.task("t7")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		t.Errorf("phase = %q, want Failed", ft.Status.Phase)
	}
	cond := condOf(ft, "DependencyGraphValid")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected DependencyGraphValid=False condition, got %v", cond)
	}
	// Distinguish this return from the unknown-dependency return, which writes the
	// same condition type, status and reason.
	if !strings.Contains(cond.Message, "cycle detected") {
		t.Errorf("condition message = %q, want it to name the cycle", cond.Message)
	}
}
