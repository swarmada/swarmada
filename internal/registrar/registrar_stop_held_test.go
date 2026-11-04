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

// A robot that a stop holds is not told to continue on reconnect: an action that would
// project IN_PROGRESS or REVOKING is projected IDLE, with no action id or lease (RFC-0001
// #safety-action-table, rows A6 and A13). A stop holds the robot
// while its estopState is Stopping, Stopped, Failed or Resuming, and whenever it records
// one, whatever its estopState reads.

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func TestRegister_AnActionAStopHoldsIsToldIdle(t *testing.T) {
	held := map[string]func(*fleetv1.Robot){
		"Stopping": func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopping },
		"Stopped":  func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopped },
		"Failed":   func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopFailed },
		"Resuming": func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopResuming },
		"own trigger on record": func(r *fleetv1.Robot) {
			r.Status.EstopState = fleetv1.RobotEstopNormal
			r.Annotations = map[string]string{estopscope.TriggerAnnotation: "stop"}
		},
		"zone stop on record": func(r *fleetv1.Robot) {
			r.Status.EstopState = fleetv1.RobotEstopNormal
			r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: "FleetZone", Name: "z", Since: metav1.Now()}}
		},
	}
	for name, hold := range held {
		for _, phase := range []fleetv1.ActionPhase{fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress, fleetv1.ActionPhaseRevoking} {
			t.Run(name+"/"+string(phase), func(t *testing.T) {
				robot := admittedRobot("task-1", "dock-1")
				hold(robot)
				action := &fleetv1.FleetAction{
					ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "task-1"},
					Status:     fleetv1.FleetActionStatus{Phase: phase, AssignmentGeneration: 4},
				}
				r, _ := newRegistrar(t, robot, action)
				ts := doRegister(t, r).GetAuthoritativeActionState()
				if ts.GetPhase() != fav1.RobotActionPhase_ROBOT_ACTION_PHASE_IDLE || ts.GetActionId() != "" ||
					ts.GetFencingToken() != 0 || ts.GetLeaseGeneration() != 0 {
					t.Fatalf("a stop holds the robot and its %s action was projected %+v; want IDLE with no action or lease", phase, ts)
				}
			})
		}
	}
}

// A Cancelled action still projects CANCELLED under a stop: it tells the robot to halt.
func TestRegister_ACancelledActionAStopHoldsStaysCancelled(t *testing.T) {
	robot := admittedRobot("task-1", "dock-1")
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: regNS, Name: "task-1"},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseCancelled, AssignmentGeneration: 2},
	}
	r, _ := newRegistrar(t, robot, action)
	if p := doRegister(t, r).GetAuthoritativeActionState().GetPhase(); p != fav1.RobotActionPhase_ROBOT_ACTION_PHASE_CANCELLED {
		t.Fatalf("phase = %v, want CANCELLED", p)
	}
}
