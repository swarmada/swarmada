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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Candidate filter 9's class condition. After an edit drops a robot's class from its adapter's
// spec.servesRobotClasses, the ControlStream still takes the robot's telemetry and EstopAcks
// (the admission gate MUST NOT block observation or stopping, RFC-0001
// #fleetadapter-admission-gating), so the robot stays Idle. Its work is still refused on the
// stream, so it must not be selected for any: the class is re-checked here, as admission
// checks it.

// classAdapter is the ready fixture adapter listing classes.
func classAdapter(classes ...string) *fleetv1.FleetAdapter {
	a := readyActionAdapter()
	a.Spec.ServesRobotClasses = classes
	return a
}

// idleRobotOfClass is an Idle robot of class bound to the fixture adapter.
func idleRobotOfClass(name, class string) *fleetv1.Robot {
	r := robotInPhase(name, fleetv1.RobotPhaseIdle, "")
	r.Spec.RobotClass = class
	return r
}

func assignedActionOf(t *testing.T, r *FleetActionReconciler, name string) string {
	t.Helper()
	var robot fleetv1.Robot
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: actionNS}, &robot); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return robot.Status.AssignedAction
}

// The robot's class has left the adapter's list: the action stays Pending, the robot unbound,
// and the reason counts under filter 9.
func TestAssignmentGate_ClassLeftTheAdapterListWithholdsDispatch(t *testing.T) {
	r, c := newActionReconciler(t, classAdapter("forklift"), pendingActionFor("t1"), idleRobotOfClass("r1", "amr"))
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending: a robot whose class its adapter no longer lists was given work", got)
	}
	if got := assignedActionOf(t, r, "r1"); got != "" {
		t.Errorf("robot bound to %q", got)
	}
	if msg := getAction(t, c, "t1").Status.Message; !strings.Contains(msg, "adapter unfit for dispatch") {
		t.Errorf("status.message = %q, want the withheld robot counted under filter 9", msg)
	}
}

// The control: a list that names the robot's class among others assigns.
func TestAssignmentGate_ListedClassAssigns(t *testing.T) {
	r, c := newActionReconciler(t, classAdapter("forklift", "amr"), pendingActionFor("t1"), idleRobotOfClass("r1", "amr"))
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned", got)
	}
}

// An empty list serves every class, at dispatch as at admission.
func TestAssignmentGate_EmptyClassListServesEveryClass(t *testing.T) {
	r, c := newActionReconciler(t, classAdapter(), pendingActionFor("t1"), idleRobotOfClass("r1", "amr"))
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned", got)
	}
}
