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

package registrar

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func admittedRobot(assignedAction, zone string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: regRobotID},
		Spec:       fleetv1.RobotSpec{Zone: zone},
		Status:     fleetv1.RobotStatus{AssignedAction: assignedAction},
	}
}

func doRegister(t *testing.T, r *Registrar) *fav1.RegisterAck {
	t.Helper()
	return r.Register(context.Background(), adapterID(), &fav1.RegisterRobot{RobotId: regRobotID})
}

// An executing action projects into IN_PROGRESS with the fencing token / lease
// generation, so the adapter resumes under the same lease.
func TestRegister_AuthoritativeActionStateInProgress(t *testing.T) {
	robot := admittedRobot("task-1", "dock-1")
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "task-1"},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress, AssignmentGeneration: 5},
	}
	r, _ := newRegistrar(t, robot, action)

	ts := doRegister(t, r).GetAuthoritativeActionState()
	if ts.GetPhase() != fav1.RobotActionPhase_ROBOT_ACTION_PHASE_IN_PROGRESS {
		t.Fatalf("phase = %v, want IN_PROGRESS", ts.GetPhase())
	}
	if ts.GetActionId() != "task-1" || ts.GetFencingToken() != 5 || ts.GetLeaseGeneration() != 5 {
		t.Fatalf("task state = %+v (want task-1, fencing/gen 5)", ts)
	}
}

// A Revoking action self-stops on the wire.
func TestRegister_AuthoritativeActionStateRevoking(t *testing.T) {
	robot := admittedRobot("task-1", "dock-1")
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "task-1"},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseRevoking, AssignmentGeneration: 3},
	}
	r, _ := newRegistrar(t, robot, action)
	if p := doRegister(t, r).GetAuthoritativeActionState().GetPhase(); p != fav1.RobotActionPhase_ROBOT_ACTION_PHASE_REVOKING {
		t.Fatalf("phase = %v, want REVOKING", p)
	}
}

// No assigned action → IDLE with no action id.
func TestRegister_AuthoritativeActionStateIdle(t *testing.T) {
	r, _ := newRegistrar(t, admittedRobot("", "dock-1"))
	ts := doRegister(t, r).GetAuthoritativeActionState()
	if ts.GetPhase() != fav1.RobotActionPhase_ROBOT_ACTION_PHASE_IDLE || ts.GetActionId() != "" {
		t.Fatalf("idle task state = %+v", ts)
	}
}

// A claimed action with no control-plane record → UNKNOWN (halt and report).
func TestRegister_AuthoritativeActionStateUnknown(t *testing.T) {
	r, _ := newRegistrar(t, admittedRobot("ghost-task", "dock-1"))
	ts := doRegister(t, r).GetAuthoritativeActionState()
	if ts.GetPhase() != fav1.RobotActionPhase_ROBOT_ACTION_PHASE_UNKNOWN || ts.GetActionId() != "ghost-task" {
		t.Fatalf("unknown task state = %+v", ts)
	}
}

// active_capabilities lists only Active capabilities, sorted.
func TestRegister_ActiveCapabilities(t *testing.T) {
	robot := admittedRobot("", "dock-1")
	robot.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "navigate", Status: fleetv1.CapabilityStatusActive},
		{Name: "pick", Status: fleetv1.CapabilityStatusInactive},
		{Name: "inspect", Status: fleetv1.CapabilityStatusActive},
		{Name: "transport", Status: fleetv1.CapabilityStatusDegraded},
	}
	r, _ := newRegistrar(t, robot)

	caps := doRegister(t, r).GetActiveCapabilities()
	if len(caps) != 2 || caps[0] != "inspect" || caps[1] != "navigate" {
		t.Fatalf("active capabilities = %v, want [inspect navigate]", caps)
	}
}

// edge_endpoints collects the robot's zone chain (leaf first, then ancestors),
// skipping zones with no edge node.
func TestRegister_EdgeEndpoints(t *testing.T) {
	robot := admittedRobot("", "dock-1")
	leaf := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "dock-1"},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: "floor-1", EdgeNode: &fleetv1.EdgeNodeConfig{Address: "edge-dock:8443"}},
	}
	mid := &fleetv1.FleetZone{ // no edge node → skipped
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "floor-1"},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: "site-1"},
	}
	root := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "site-1"},
		Spec:       fleetv1.FleetZoneSpec{EdgeNode: &fleetv1.EdgeNodeConfig{Address: "edge-site:8443"}},
	}
	r, _ := newRegistrar(t, robot, leaf, mid, root)

	eps := doRegister(t, r).GetEdgeEndpoints()
	if len(eps) != 2 {
		t.Fatalf("edge endpoints = %+v, want 2 (dock + site, floor skipped)", eps)
	}
	if eps[0].GetZone() != "dock-1" || eps[0].GetAddress() != "edge-dock:8443" {
		t.Errorf("first endpoint = %+v, want leaf dock-1 first", eps[0])
	}
	if eps[1].GetZone() != "site-1" || eps[1].GetAddress() != "edge-site:8443" {
		t.Errorf("second endpoint = %+v, want ancestor site-1", eps[1])
	}
}
