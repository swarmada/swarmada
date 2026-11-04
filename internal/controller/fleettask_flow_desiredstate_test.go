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

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A task-level desiredState written after a flow's child exists reaches that child, as it does
// on the dependsOn path: an operator's cancel of a flow task cancels a member an estop paused.
func TestFlow_TaskDesiredStateReachesAnExistingChild(t *testing.T) {
	task := flowTask("h", &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("t1"), runNode("t2")}},
		fleetv1.FleetTaskAction{Name: "t1", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}},
		fleetv1.FleetTaskAction{Name: "t2", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}})
	r, c := newFleetTaskReconciler(t, task)
	child := &fleetv1.FleetAction{}
	reconcileFTUntil(t, r, "h", 5, func() bool {
		return c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: iterationChildName("h", "t1", 0)}, child) == nil
	})
	// The child is held by a stop: Paused, bound.
	child.Status.Phase = fleetv1.ActionPhasePaused
	child.Status.AssignedRobot = "r1"
	if err := c.Status().Update(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	// The operator cancels the task.
	cur := &fleetv1.FleetTask{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: "h"}, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.DesiredState = fleetv1.DesiredStateCancelled
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	reconcileFTUntil(t, r, "h", 3, func() bool {
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: child.Name}, child)
		return child.Spec.DesiredState == fleetv1.DesiredStateCancelled
	})
	if child.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Fatalf("the task's cancel did not reach its paused flow member: desiredState %q", child.Spec.DesiredState)
	}
}
