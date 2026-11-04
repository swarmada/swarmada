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

// ensureStatus handed out pointers INTO task.Status.Actions and
// cached them. A later append reallocated the backing array, and every
// projection write through a cached pointer then landed on the array nobody
// reads. The pass where a member's entry is first created is the one that
// loses the write; the next reconcile re-does it, so the symptom was a
// one-reconcile lag, not permanent loss — which is exactly why it needs a
// test that looks at ONE pass.

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func flowMember(name string) fleetv1.FleetTaskAction {
	return fleetv1.FleetTaskAction{Name: name, Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}}
}

func actionStatusOf(t *testing.T, task *fleetv1.FleetTask, member string) fleetv1.FleetTaskActionStatus {
	t.Helper()
	for i := range task.Status.Actions {
		if task.Status.Actions[i].Name == member {
			return task.Status.Actions[i]
		}
	}
	t.Fatalf("no status.actions entry for member %q", member)
	return fleetv1.FleetTaskActionStatus{}
}

// A member whose status entry ALREADY exists is the one that loses its write.
// reconcileFlow caches every pre-existing entry at the top of the pass; the
// projection loop then appends an entry for the member the flow never
// schedules, that append reallocates the backing array, and the write for the
// pre-existing member — projected afterwards, because spec.actions lists it
// second — lands on the array nobody reads.
func TestFleetTask_FlowProjectionSurvivesAppendRealloc(t *testing.T) {
	task := flowTask("proj",
		&fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("patrol")}},
		flowMember("z"),      // never scheduled by the flow; appended during projection
		flowMember("patrol"), // projected AFTER that append
	)
	// The entry exists from a prior reconcile, with exactly enough capacity
	// for itself — the next append reallocates.
	task.Status.Actions = []fleetv1.FleetTaskActionStatus{{Name: "patrol", DependenciesMet: true}}

	child := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: childName("proj", "patrol"), Namespace: actionNS,
			OwnerReferences: []metav1.OwnerReference{fakeTaskControllerRef("proj")}},
		Spec:   fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress, AssignedRobot: "r1"},
	}

	r, c := newFleetTaskReconciler(t, task, child)
	reconcileFT(t, r, "proj")

	got := actionStatusOf(t, getFT(t, c, "proj"), "patrol")
	if got.ActionRef != child.Name {
		t.Errorf("actionRef = %q, want %q — the projection write landed on a stale backing array", got.ActionRef, child.Name)
	}
	if got.Phase != fleetv1.ActionPhaseInProgress {
		t.Errorf("phase = %q, want InProgress — projection write dropped", got.Phase)
	}
	if got.AssignedRobot != "r1" {
		t.Errorf("assignedRobot = %q, want r1 — projection write dropped", got.AssignedRobot)
	}
}

// The loop path, where a member's entry is first created by the
// StartIteration write and the iteration counter must survive every later
// append in the same pass. This passed before the fix too — it is here to
// keep the by-index rewrite honest, not to reproduce the defect.
func TestFleetTask_FlowIterationBookkeepingLandsOnTheObject(t *testing.T) {
	times := int32(3)
	task := flowTask("proj2",
		&fleetv1.FlowNode{Loop: &fleetv1.LoopNode{Mode: fleetv1.LoopModeTimes, Times: &times, Body: runNode("patrol")}},
		flowMember("z"),
		flowMember("patrol"),
	)

	r, c := newFleetTaskReconciler(t, task,
		&fleetv1.FleetAction{
			ObjectMeta: metav1.ObjectMeta{Name: iterationChildName("proj2", "patrol", 1), Namespace: actionNS,
				OwnerReferences: []metav1.OwnerReference{fakeTaskControllerRef("proj2")}},
			Spec:   fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
			Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseSucceeded},
		})

	// Pass 1 persists iteration 1; pass 2 settles it and advances to 2.
	reconcileFT(t, r, "proj2")
	reconcileFT(t, r, "proj2")

	got := actionStatusOf(t, getFT(t, c, "proj2"), "patrol")
	if got.IterationsCompleted != 1 {
		t.Errorf("iterationsCompleted = %d, want 1", got.IterationsCompleted)
	}
	if len(got.RecentIterations) != 1 {
		t.Errorf("recentIterations = %d entries, want 1", len(got.RecentIterations))
	}
	if got.Iteration != 2 {
		t.Errorf("iteration = %d, want 2", got.Iteration)
	}
}
