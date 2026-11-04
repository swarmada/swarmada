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

// completionPolicy: Quorum with no spec.quorum is refused at admission by a
// CEL rule on spec (api/v1/fleettask_types.go). This is the reconcile-time
// re-check — the same belt-and-suspenders relationship validateGraph already
// has with the CEL rules — and it must fail CLOSED. Defaulting the threshold
// to 0 made `succeeded >= 0` true on the first pass, so the task reported
// Succeeded having run nothing.

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func condOf(task *fleetv1.FleetTask, condType string) *metav1.Condition {
	for i := range task.Status.Conditions {
		if task.Status.Conditions[i].Type == condType {
			return &task.Status.Conditions[i]
		}
	}
	return nil
}

// THE FAIL-OPEN CASE. Nothing has succeeded; a threshold of 0 would report
// Succeeded on the spot.
func TestFleetTask_QuorumNilFailsClosed(t *testing.T) {
	task := fleetTask("q1", fleetv1.CompletionPolicyQuorum, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	task.Spec.Quorum = nil
	r, c := newFleetTaskReconciler(t, task,
		childAction("q1", "a", fleetv1.ActionPhaseInProgress),
		childAction("q1", "b", fleetv1.ActionPhaseInProgress),
	)
	reconcileFT(t, r, "q1")

	got := getFT(t, c, "q1")
	if got.Status.Phase == fleetv1.FleetTaskPhaseSucceeded {
		t.Fatal("completionPolicy: Quorum with no quorum reported Succeeded having run nothing")
	}
	if got.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed: a task whose completion policy is incomplete cannot complete", got.Status.Phase)
	}
	cond := condOf(got, "CompletionPolicyValid")
	if cond == nil {
		t.Fatal("no CompletionPolicyValid condition: the failure must say why, not just fail")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("CompletionPolicyValid = %q, want False", cond.Status)
	}
}

// It must fail closed even when every member HAS succeeded: All would have
// been met here, but the author did not write All, and inferring it invents a
// policy nobody stated.
func TestFleetTask_QuorumNilFailsClosedWithEverythingSucceeded(t *testing.T) {
	task := fleetTask("q2", fleetv1.CompletionPolicyQuorum, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
	)
	task.Spec.Quorum = nil
	r, c := newFleetTaskReconciler(t, task, childAction("q2", "a", fleetv1.ActionPhaseSucceeded))
	reconcileFT(t, r, "q2")

	if got := getFT(t, c, "q2").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got)
	}
}

// A non-terminal child of a task that cannot complete is confirmed-cancelled,
// not abandoned — the failure runs through the same enactFailure path every
// other unmeetable policy does, so no robot keeps a lease on work whose task
// has been failed.
func TestFleetTask_QuorumNilCancelsLiveChildren(t *testing.T) {
	task := fleetTask("q3", fleetv1.CompletionPolicyQuorum, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
	)
	task.Spec.Quorum = nil
	r, c := newFleetTaskReconciler(t, task, childAction("q3", "a", fleetv1.ActionPhaseInProgress))
	reconcileFT(t, r, "q3")

	child := getActionOrNil(t, c, childName("q3", "a"))
	if child == nil {
		t.Fatal("child action disappeared")
	}
	if child.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Fatalf("child desiredState = %q, want Cancelled", child.Spec.DesiredState)
	}
}

// A quorum that IS set still behaves exactly as before — the guard must not
// change the meaning of a well-formed policy.
func TestFleetTask_QuorumSetUnmetStaysRunning(t *testing.T) {
	q := int32(2)
	task := fleetTask("q4", fleetv1.CompletionPolicyQuorum, fleetv1.FailurePolicyContinueOthers, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	task.Spec.Quorum = &q
	r, c := newFleetTaskReconciler(t, task,
		childAction("q4", "a", fleetv1.ActionPhaseSucceeded),
		childAction("q4", "b", fleetv1.ActionPhaseInProgress),
	)
	reconcileFT(t, r, "q4")

	if got := getFT(t, c, "q4").Status.Phase; got != fleetv1.FleetTaskPhaseRunning {
		t.Fatalf("phase = %q, want Running: 1 of a quorum of 2, one still in flight", got)
	}
}
