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

package scheduler_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// capRobot builds an Idle robot whose camera.rgb capability has the given status.
func capRobot(name string, status fleetv1.CapabilityStatus) fleetv1.Robot {
	return fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: fleetv1.RobotStatus{
			Phase:        fleetv1.RobotPhaseIdle,
			Capabilities: []fleetv1.CapabilityStatusEntry{{Name: "camera.rgb", Status: status}},
		},
	}
}

// camAction requires the camera.rgb capability; acceptDegraded sets the action field.
func camAction(acceptDegraded *bool) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		Spec: fleetv1.FleetActionSpec{
			Type:                       fleetv1.ActionTypeCustom,
			RequiredCapabilities:       []string{"camera.rgb"},
			AcceptDegradedCapabilities: acceptDegraded,
		},
	}
}

// By default a Degraded required capability is not schedulable.
func TestSelectRobot_DegradedRejectedByDefault(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	if _, err := s.SelectRobot(camAction(nil),
		[]fleetv1.Robot{capRobot("r1", fleetv1.CapabilityStatusDegraded)}, false, false, false); err == nil {
		t.Fatal("a Degraded required capability must be rejected when acceptDegraded=false")
	}
}

// With acceptDegraded=true a Degraded required capability satisfies the requirement.
func TestSelectRobot_DegradedAcceptedWhenFlagged(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	got, err := s.SelectRobot(camAction(nil),
		[]fleetv1.Robot{capRobot("r1", fleetv1.CapabilityStatusDegraded)}, true, false, false)
	if err != nil {
		t.Fatalf("a Degraded required capability should be accepted when acceptDegraded=true: %v", err)
	}
	if got.Name != "r1" {
		t.Fatalf("selected %q, want r1", got.Name)
	}
}

// acceptDegraded must not conjure a capability the robot lacks entirely.
func TestSelectRobot_MissingCapabilityStillExcluded(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	bare := fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseIdle},
	}
	if _, err := s.SelectRobot(camAction(nil), []fleetv1.Robot{bare}, true, false, false); err == nil {
		t.Fatal("acceptDegraded must not satisfy a capability the robot does not report at all")
	}
}

// The exported 2-arg matcher resolves acceptance from the action's own field.
func TestRobotMatchesAction_AcceptDegradedFromActionField(t *testing.T) {
	yes := true
	degraded := capRobot("r1", fleetv1.CapabilityStatusDegraded)
	if !scheduler.RobotMatchesAction(&degraded, camAction(&yes)) {
		t.Fatal("task field acceptDegradedCapabilities=true should match a Degraded capability")
	}
	if scheduler.RobotMatchesAction(&degraded, camAction(nil)) {
		t.Fatal("unset acceptDegradedCapabilities must not match a Degraded capability")
	}
}

// RobotSatisfiesActionCapabilities is the continued-execution predicate used for
// capability-loss reassignment: an Active required capability satisfies; a Degraded
// one satisfies only when acceptDegraded is true; a missing/other-status one never
// does. It must mirror the assignment-time filter so eligibility can't diverge.
func TestRobotSatisfiesActionCapabilities(t *testing.T) {
	active := capRobot("r1", fleetv1.CapabilityStatusActive)
	degraded := capRobot("r1", fleetv1.CapabilityStatusDegraded)
	bare := fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r1"}}

	if !scheduler.RobotSatisfiesActionCapabilities(&active, camAction(nil), false) {
		t.Fatal("Active required capability should satisfy")
	}
	if scheduler.RobotSatisfiesActionCapabilities(&degraded, camAction(nil), false) {
		t.Fatal("Degraded required capability must NOT satisfy when acceptDegraded=false (triggers reassignment)")
	}
	if !scheduler.RobotSatisfiesActionCapabilities(&degraded, camAction(nil), true) {
		t.Fatal("Degraded required capability should satisfy when acceptDegraded=true (no reassignment)")
	}
	if scheduler.RobotSatisfiesActionCapabilities(&bare, camAction(nil), true) {
		t.Fatal("a capability the robot no longer reports must not satisfy")
	}
}

// degradedPayloadRobot has a Degraded transport.payload capability whose reduced
// resolved maxPayloadKg is maxKg; schedulable sets degradedPolicy's stamped flag.
func degradedPayloadRobot(schedulable bool, maxKg float64) fleetv1.Robot {
	return fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Status: fleetv1.RobotStatus{
			Phase: fleetv1.RobotPhaseIdle,
			Capabilities: []fleetv1.CapabilityStatusEntry{{
				Name:                "transport.payload",
				Status:              fleetv1.CapabilityStatusDegraded,
				DegradedSchedulable: schedulable,
				ResolvedParameters:  map[string]float64{"maxPayloadKg": maxKg},
			}},
		},
	}
}

// constraintOnlyAction declares a parametric constraint with no requiredCapabilities
// entry, isolating the degradedPolicy (constraint) path from the F-i name check.
func constraintOnlyAction(minKg float64) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		Spec: fleetv1.FleetActionSpec{
			Type:        fleetv1.ActionTypeCustom,
			Constraints: map[string]float64{"maxPayloadKg": minKg},
		},
	}
}

// degradedPolicy: a Degraded-schedulable capability satisfies a constraint its
// reduced parameters still meet, but not one they no longer meet.
func TestRobotMatchesAction_DegradedPolicySatisfiesLowerConstraint(t *testing.T) {
	robot := degradedPayloadRobot(true, 10)
	if !scheduler.RobotMatchesAction(&robot, constraintOnlyAction(10)) {
		t.Fatal("degraded-schedulable capability should satisfy a constraint its reduced params still meet (10>=10)")
	}
	if scheduler.RobotMatchesAction(&robot, constraintOnlyAction(30)) {
		t.Fatal("degraded capability must be excluded from a constraint its reduced params no longer meet (10<30)")
	}
}

// Without degradedPolicy (DegradedSchedulable=false) a Degraded capability's
// parameters never satisfy a constraint.
func TestRobotMatchesAction_DegradedWithoutPolicyExcluded(t *testing.T) {
	robot := degradedPayloadRobot(false, 100)
	if scheduler.RobotMatchesAction(&robot, constraintOnlyAction(10)) {
		t.Fatal("a Degraded capability without degradedPolicy must not satisfy any parametric constraint")
	}
}
