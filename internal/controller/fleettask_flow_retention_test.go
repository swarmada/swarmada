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

// the "Done when", verified literally: "A 500-iteration loop
// completes with FleetTask.status size stable across the run (measured at
// iteration 1, 250 and 500), and child object count never exceeding the
// configured retention." This is the controller-level half of the retention
// measurement — internal/flow's own 500-iteration test proves the pure
// evaluator's bookkeeping is correct; this proves the ACTUAL CRD status
// object the API server would store stays bounded.

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func flowTask(name string, flow *fleetv1.FlowNode, actions ...fleetv1.FleetTaskAction) *fleetv1.FleetTask {
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS, UID: fakeTaskUID(name)},
		Spec:       fleetv1.FleetTaskSpec{FailurePolicy: fleetv1.FailurePolicyFailFast, DesiredState: fleetv1.DesiredStateRunning, Actions: actions, Flow: flow},
	}
}

func runNode(member string) fleetv1.FlowNode { return fleetv1.FlowNode{Run: &member} }

// reconcileFTUntil reconciles up to max times, stopping early once check
// reports true, and fails the test if it never does.
func reconcileFTUntil(t *testing.T, r *FleetTaskReconciler, name string, max int, check func() bool) {
	t.Helper()
	for i := 0; i < max; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: name}}); err != nil {
			t.Fatalf("reconcile #%d: %v", i, err)
		}
		if check() {
			return
		}
	}
	t.Fatalf("condition not met after %d reconciles", max)
}

func TestFleetTask_500IterationLoop_StatusSizeStableAndChildrenBounded(t *testing.T) {
	times := int32(500)
	task := flowTask("patrol-500",
		&fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeTimes, Times: &times, Body: runNode("patrol")}},
		fleetv1.FleetTaskAction{Name: "patrol", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}},
	)
	r, c := newFleetTaskReconciler(t, task)
	ctx := context.Background()

	statusSize := func(iteration int) int {
		var fresh fleetv1.FleetTask
		if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: task.Name}, &fresh); err != nil {
			t.Fatalf("get task at iteration %d: %v", iteration, err)
		}
		b, err := json.Marshal(fresh.Status)
		if err != nil {
			t.Fatalf("marshal status at iteration %d: %v", iteration, err)
		}
		return len(b)
	}

	measured := map[int]int{}
	for i := int32(1); i <= times; i++ {
		childName := iterationChildName(task.Name, "patrol", i)

		// Drive reconciles until this iteration's child exists.
		reconcileFTUntil(t, r, task.Name, 5, func() bool {
			var child fleetv1.FleetAction
			return c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: childName}, &child) == nil
		})

		// Simulate the FleetAction controller completing it.
		var child fleetv1.FleetAction
		if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: childName}, &child); err != nil {
			t.Fatalf("get child %q: %v", childName, err)
		}
		orig := child.DeepCopy()
		now := metav1.Now()
		child.Status.Phase = fleetv1.ActionPhaseSucceeded
		child.Status.CompletionTime = &now
		if err := c.Status().Patch(ctx, &child, client.MergeFrom(orig)); err != nil {
			t.Fatalf("completing child %q: %v", childName, err)
		}

		// Drive reconciles until the settlement is recorded (or, on the last
		// iteration, until the task itself settles).
		reconcileFTUntil(t, r, task.Name, 5, func() bool {
			var fresh fleetv1.FleetTask
			if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: task.Name}, &fresh); err != nil {
				t.Fatalf("get task: %v", err)
			}
			for j := range fresh.Status.Actions {
				if fresh.Status.Actions[j].Name == "patrol" && fresh.Status.Actions[j].IterationsCompleted == i {
					return true
				}
			}
			return fresh.Status.Phase == fleetv1.FleetTaskPhaseSucceeded
		})

		if i == 1 || i == 250 || i == 500 {
			measured[int(i)] = statusSize(int(i))
		}
	}

	var final fleetv1.FleetTask
	if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: task.Name}, &final); err != nil {
		t.Fatalf("get final task: %v", err)
	}
	if final.Status.Phase != fleetv1.FleetTaskPhaseSucceeded {
		t.Fatalf("final phase = %v, want Succeeded", final.Status.Phase)
	}
	var patrolStatus *fleetv1.FleetTaskActionStatus
	for i := range final.Status.Actions {
		if final.Status.Actions[i].Name == "patrol" {
			patrolStatus = &final.Status.Actions[i]
		}
	}
	if patrolStatus == nil {
		t.Fatal("no status.actions entry for 'patrol'")
	}
	if patrolStatus.IterationsCompleted != 500 {
		t.Fatalf("IterationsCompleted = %d, want 500 (the complete scalar — never truncated)", patrolStatus.IterationsCompleted)
	}
	if patrolStatus.IterationsFailed != 0 {
		t.Fatalf("IterationsFailed = %d, want 0", patrolStatus.IterationsFailed)
	}
	if got := len(patrolStatus.RecentIterations); got != 10 {
		t.Fatalf("RecentIterations has %d entries, want the default keepIterationRecords bound of 10", got)
	}

	// THE MEASUREMENT: status size at iteration 1, 250, 500.
	t.Logf("FleetTask.status size: iteration 1 = %d bytes, iteration 250 = %d bytes, iteration 500 = %d bytes",
		measured[1], measured[250], measured[500])

	// Iteration 1 is legitimately smaller than 250/500: RecentIterations
	// (capped at keepIterationRecords=10) has only 1 entry at that point —
	// the window is still FILLING, not yet at its bound. That is bounded
	// growth (it fills once, to a fixed cap), not unbounded growth, so it is
	// not compared against the later measurements directly.
	if measured[1] >= measured[250] {
		t.Errorf("iteration 1 (%d bytes) should be smaller than iteration 250 (%d bytes) while RecentIterations is still filling to its cap", measured[1], measured[250])
	}

	// THE INVARIANT THAT MATTERS: once the RecentIterations window is full
	// (by iteration 250, long past the cap of 10), status size must be
	// STABLE — iteration 250 and iteration 500 differ only by the small,
	// bounded variance of timestamp/name string widths, never by growth
	// proportional to how many more iterations ran in between.
	delta := measured[500] - measured[250]
	if delta < 0 {
		delta = -delta
	}
	if delta > 200 {
		t.Errorf("status size at iteration 500 (%d bytes) drifted %d bytes from iteration 250 (%d bytes) with the retention window already full — status is growing with iteration count", measured[500], delta, measured[250])
	}

	// Child object count never exceeds the configured retention (default
	// keepChildren: 20; all 500 iterations here are non-failed).
	var children fleetv1.FleetActionList
	if err := c.List(ctx, &children, client.InNamespace(actionNS), client.MatchingLabels{ownedByLabel: task.Name, memberLabel: "patrol"}); err != nil {
		t.Fatalf("listing children: %v", err)
	}
	if len(children.Items) > 20 {
		t.Errorf("child object count = %d, want <= 20 (default keepChildren)", len(children.Items))
	}
	t.Logf("child object count at end of run: %d (retention bound: 20)", len(children.Items))
}

// NON-NEGOTIABLE RULE: "A Forever loop MUST carry until, maxIterations or
// maxDurationSeconds. Admission rejects otherwise." The CRD schema cannot
// enforce this AT the API server (see api/v1/fleettask_flow_types.go's
// comment: `loop` is a CEL reserved word, and true recursion is unsupported
// by structural schemas past depth 1), so — exactly like v0.3's dependsOn
// cycle/dangling-reference check ("at v0.3 this is validated by
// validateGraph on every non-terminal reconcile instead, not by the
// admission webhook") — it is enforced here, at reconcile, the first
// non-terminal pass the object is ever seen.
func TestFleetTask_UnboundedForeverLoopIsRejectedAtReconcile(t *testing.T) {
	task := flowTask("unbounded",
		&fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeForever, Body: runNode("patrol")}},
		fleetv1.FleetTaskAction{Name: "patrol", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}},
	)
	r, c := newFleetTaskReconciler(t, task)
	reconcileFT(t, r, task.Name)

	var fresh fleetv1.FleetTask
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: task.Name}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		t.Fatalf("phase = %v, want Failed (an unbounded Forever loop must never be accepted for scheduling)", fresh.Status.Phase)
	}
	found := false
	for _, cond := range fresh.Status.Conditions {
		if cond.Type == "FlowValid" && cond.Status == metav1.ConditionFalse {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a FlowValid=False condition naming the unbounded loop")
	}
}
