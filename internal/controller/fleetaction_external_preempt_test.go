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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// externalPendingInBand builds a Pending External action — spec.executor set,
// spec.type External (the two imply each other, api/v1/fleetaction_types.go:166)
// — in the given priority band. An External action is dispatched to a
// ComputeEndpoint, never to a robot: scheduler.ErrExternalAction exists to keep
// it off the robot path entirely.
func externalPendingInBand(name string, prio fleetv1.ActionPriority) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetActionSpec{
			Type:     fleetv1.ActionTypeExternal,
			Priority: prio,
			Executor: &fleetv1.ActionExecutor{EndpointRef: "plate-reader"},
		},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
}

// The scheduler's robot-selection entry point returns
// ErrExternalAction for an action carrying spec.executor, so an External action
// never wins a robot on the ordinary path. The preemption search is the OTHER
// path to a robot, and it did not check spec.executor at all: a Critical
// External action found no Idle robot, fell through to findPreemptionCandidate,
// matched a busy robot trivially (an External action can carry no
// requiredCapabilities, constraints or robotSelector), and displaced a running
// Normal action to take a robot it must never be given.
//
// Both halves are asserted: the victim keeps running, AND the External action
// does not end up bound to the robot.
func TestPreempt_ExternalActionNeverDisplacesARobot(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		externalPendingInBand("ext", fleetv1.ActionPriorityCritical),
		bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"),
	)

	reconcileAction(t, r, "ext")

	if v := getAction(t, c, "norm"); v.Status.Phase == fleetv1.ActionPhasePreempted {
		t.Fatal("a Critical External action preempted a robot action — an external action must never displace a robot")
	}
	ext := getAction(t, c, "ext")
	if ext.Status.AssignedRobot != "" {
		t.Fatalf("External action bound to robot %q — spec.executor dispatches to a ComputeEndpoint, never a robot", ext.Status.AssignedRobot)
	}
	if ext.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("External action phase = %s, want Pending (no dispatch path exists at v0.4)", ext.Status.Phase)
	}
	if rob := getRobot(t, c, "r1", actionNS); rob.Status.AssignedAction != "norm" {
		t.Fatalf("robot switched off its running action to an External one: assignedAction=%q", rob.Status.AssignedAction)
	}
}

// The High band is the other preemptor band (§9.1.4.3), and it reaches the same
// search. A band-by-band table keeps a future band addition from reopening the
// hole on the path this test does not name.
func TestFindPreemptionCandidate_ExternalActionYieldsNoCandidate(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	for _, prio := range []fleetv1.ActionPriority{fleetv1.ActionPriorityCritical, fleetv1.ActionPriorityHigh} {
		t.Run(string(prio), func(t *testing.T) {
			victim := bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease)
			robot := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm")
			r, _ := newActionReconciler(t, victim, robot)

			cand, v, err := r.findPreemptionCandidate(context.Background(),
				externalPendingInBand("ext", prio), []fleetv1.Robot{*robot})
			if err != nil {
				t.Fatalf("findPreemptionCandidate: %v", err)
			}
			if cand != nil || v != nil {
				t.Fatalf("External action found a preemption candidate: robot=%v victim=%v", cand, v)
			}
		})
	}
}
