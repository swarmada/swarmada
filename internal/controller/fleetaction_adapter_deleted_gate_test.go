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
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Candidate filter 9's existence condition. After the FleetAdapter a robot names is deleted, the
// ControlStream still takes the robot's telemetry and EstopAcks (the admission gate MUST NOT
// block observation or stopping, RFC-0001 #fleetadapter-admission-gating), so the robot stays
// Idle. Its work is still refused on the stream, so it must not be selected for any.

// idleRobotOnDeletedAdapter is an Idle robot bound to an adapter that has no FleetAdapter object.
func idleRobotOnDeletedAdapter(name string) *fleetv1.Robot {
	r := robotInPhase(name, fleetv1.RobotPhaseIdle, "")
	r.Spec.Adapter.Name = "deleted-adapter"
	return r
}

// The robot's FleetAdapter is gone: the action stays Pending, the robot unbound, and the reason
// counts under filter 9.
func TestAssignmentGate_DeletedAdapterWithholdsAnIdleRobot(t *testing.T) {
	r, c := newActionReconciler(t, pendingActionFor("t1"), idleRobotOnDeletedAdapter("r1"))
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending: a robot whose FleetAdapter was deleted was given work", got)
	}
	if got := assignedActionOf(t, r, "r1"); got != "" {
		t.Errorf("robot bound to %q", got)
	}
	if msg := getAction(t, c, "t1").Status.Message; !strings.Contains(msg, "adapter unfit for dispatch") {
		t.Errorf("status.message = %q, want the withheld robot counted under filter 9", msg)
	}
}

// Only that robot is withheld: an Idle robot on a ready adapter beside it takes the work.
func TestAssignmentGate_DeletedAdapterLeavesOtherRobotsServable(t *testing.T) {
	r, c := newActionReconciler(t, pendingActionFor("t1"), idleRobotOnDeletedAdapter("r1"),
		robotInPhase("r2", fleetv1.RobotPhaseIdle, ""))
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned to r2", got)
	}
	if got := assignedActionOf(t, r, "r1"); got != "" {
		t.Errorf("r1, whose FleetAdapter was deleted, was bound to %q", got)
	}
	if got := assignedActionOf(t, r, "r2"); got != "t1" {
		t.Errorf("r2 bound to %q, want t1", got)
	}
}
