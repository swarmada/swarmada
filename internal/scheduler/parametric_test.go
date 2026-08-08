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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// payloadRobot builds a robot whose transport.payload capability has the given
// status and resolves maxPayloadKg to the given value.
func payloadRobot(status fleetv1.CapabilityStatus, maxPayloadKg float64) *fleetv1.Robot {
	return &fleetv1.Robot{
		Status: fleetv1.RobotStatus{
			Phase: fleetv1.RobotPhaseIdle,
			Capabilities: []fleetv1.CapabilityStatusEntry{{
				Name:               "transport.payload",
				Status:             status,
				ResolvedParameters: map[string]float64{"maxPayloadKg": maxPayloadKg},
			}},
		},
	}
}

// payloadAction builds a action requiring the transport.payload capability with a
// minimum maxPayloadKg parametric constraint.
func payloadAction(minKg float64) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		Spec: fleetv1.FleetActionSpec{
			Type:                 fleetv1.ActionTypeCustom,
			RequiredCapabilities: []string{"transport.payload"},
			Constraints:          map[string]float64{"maxPayloadKg": minKg},
		},
	}
}

// The parametric filter admits a robot that meets the constraint and excludes one
// below it, on top of the capability-name match (§6.10.3).
func TestRobotMatchesAction_ParametricConstraint(t *testing.T) {
	cases := []struct {
		name     string
		capValue float64
		minKg    float64
		want     bool
	}{
		{"above the floor matches", 45, 30, true},
		{"exactly at the floor matches", 30, 30, true},
		{"below the floor excluded", 25, 30, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			robot := payloadRobot(fleetv1.CapabilityStatusActive, tc.capValue)
			if got := scheduler.RobotMatchesAction(robot, payloadAction(tc.minKg)); got != tc.want {
				t.Errorf("RobotMatchesTask = %v, want %v (cap=%v min=%v)", got, tc.want, tc.capValue, tc.minKg)
			}
		})
	}
}

// FAIL-CLOSED: a parameter that no Active capability resolves is unsatisfied, so
// the robot is excluded — a robot that cannot prove it meets the constraint never
// gets the action.
func TestRobotMatchesAction_ParametricAbsentParameterFailsClosed(t *testing.T) {
	robot := &fleetv1.Robot{
		Status: fleetv1.RobotStatus{
			Phase: fleetv1.RobotPhaseIdle,
			Capabilities: []fleetv1.CapabilityStatusEntry{{
				Name:   "transport.payload",
				Status: fleetv1.CapabilityStatusActive,
				// No ResolvedParameters at all — the parameter is unresolved.
			}},
		},
	}
	if scheduler.RobotMatchesAction(robot, payloadAction(30)) {
		t.Fatal("a robot that does not resolve the constrained parameter must be excluded (fail-closed)")
	}
}

// A capability that resolves a high value but is NOT Active (Degraded here) does
// not satisfy a parametric constraint — only Active capabilities are schedulable.
func TestRobotMatchesAction_ParametricNonActiveCapabilityExcluded(t *testing.T) {
	robot := payloadRobot(fleetv1.CapabilityStatusDegraded, 100)
	if scheduler.RobotMatchesAction(robot, payloadAction(30)) {
		t.Fatal("a Degraded capability's parameters must not satisfy a constraint (only Active is schedulable)")
	}
}

// A action with no parametric constraints is unaffected by the filter (the plain
// capability-name match still governs).
func TestRobotMatchesAction_NoConstraintsUnaffected(t *testing.T) {
	robot := payloadRobot(fleetv1.CapabilityStatusActive, 10)
	action := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{
		Type:                 fleetv1.ActionTypeCustom,
		RequiredCapabilities: []string{"transport.payload"},
	}}
	if !scheduler.RobotMatchesAction(robot, action) {
		t.Fatal("with no constraints, a capability-name match should still succeed")
	}
}
