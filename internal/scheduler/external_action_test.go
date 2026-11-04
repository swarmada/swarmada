// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scheduler_test

import (
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// externalAction builds a FleetAction carrying spec.executor — the shape that
// must never reach robot scheduling.
func externalAction() *fleetv1.FleetAction {
	return &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{
		Type:     fleetv1.ActionTypeExternal,
		Executor: &fleetv1.ActionExecutor{EndpointRef: "plate-reader"},
	}}
}

// idleRobot builds a single Idle robot with no capability requirements, so it
// would be selected by any action that reaches the normal eligibility path.
func idleRobot(name string, battery int32) fleetv1.Robot {
	b := battery
	return fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseIdle, BatteryPercent: &b},
	}
}

// THE CRITICAL ONE (decision doc, item 3): an external action must never
// consume a robot, even when eligible robots exist and would otherwise be picked.
func TestSelectRobot_ExternalActionNeverConsumesARobot(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{idleRobot("amr-1", 95), idleRobot("amr-2", 40)}

	got, err := s.SelectRobot(externalAction(), robots, false, false, false)
	if !errors.Is(err, scheduler.ErrExternalAction) {
		t.Fatalf("expected ErrExternalAction, got robot %q, err %v", got.Name, err)
	}
	if got.Name != "" {
		t.Fatalf("an external action must never be assigned a robot, got %q", got.Name)
	}
}

// Same assertion with an empty candidate pool — the early return must fire
// before any candidate examination, not merely as a side effect of finding none.
func TestSelectRobot_ExternalActionNeverConsumesARobot_NoCandidates(t *testing.T) {
	s := scheduler.NewDefaultScheduler()

	_, err := s.SelectRobot(externalAction(), nil, false, false, false)
	if !errors.Is(err, scheduler.ErrExternalAction) {
		t.Fatalf("expected ErrExternalAction even with no candidates, got %v", err)
	}
}

// THE OTHER HALF: adding the early return must not skip scheduling a normal
// robot action. A regression here (e.g. an inverted nil check) would silently
// strand every robot action in the fleet.
func TestSelectRobot_RobotActionIsNeverSkipped(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{idleRobot("amr-1", 30), idleRobot("amr-2", 80)}
	action := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeCustom}}

	got, err := s.SelectRobot(action, robots, false, false, false)
	if err != nil {
		t.Fatalf("a robot action with spec.executor unset must still be scheduled: %v", err)
	}
	if got.Name != "amr-2" {
		t.Fatalf("expected the healthiest eligible robot amr-2, got %q", got.Name)
	}
}

// SelectRobot is not the only door to a robot: the FleetAction
// controller's preemption search calls RobotMatchesAction directly to find a
// busy-but-otherwise-suitable robot for a Critical or High action. An External
// action passed that matcher TRIVIALLY — the CEL rule on FleetActionSpec forbids
// requiredCapabilities, constraints and robotSelector when spec.executor is set,
// so there is nothing left for the matcher to fail it on. The ErrExternalAction
// guard on SelectRobot is worth nothing if the matcher beside it says yes.
func TestRobotMatchesAction_ExternalActionNeverMatchesARobot(t *testing.T) {
	robot := idleRobot("amr-1", 95)

	if scheduler.RobotMatchesAction(&robot, externalAction()) {
		t.Fatal("an external action matched a robot — spec.executor dispatches to a ComputeEndpoint, never to a robot")
	}
}

// Zone is the one scheduling-adjacent field an External action MAY carry (it
// names the edge zone context for endpoint resolution, not a robot constraint,
// api/v1/fleetaction_types.go:178), so a zone-matching robot is the case where
// the matcher was most confidently wrong.
func TestRobotMatchesAction_ExternalActionWithZoneNeverMatchesARobot(t *testing.T) {
	robot := idleRobot("amr-1", 95)
	robot.Spec.Zone = "dock-3"
	action := externalAction()
	action.Spec.Zone = "dock-3"

	if scheduler.RobotMatchesAction(&robot, action) {
		t.Fatal("an external action matched a robot on a shared zone — spec.zone on an External action is endpoint context, not a robot constraint")
	}
}

// THE OTHER HALF, as above: excluding External from the matcher must not stop a
// robot action matching. RobotMatchesAction is the shared predicate behind both
// the normal filter phase and the preemption search, so a regression here
// unschedules the fleet.
func TestRobotMatchesAction_RobotActionStillMatches(t *testing.T) {
	robot := idleRobot("amr-1", 95)
	action := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeCustom}}

	if !scheduler.RobotMatchesAction(&robot, action) {
		t.Fatal("a robot action with spec.executor unset must still match an unconstrained robot")
	}
}
