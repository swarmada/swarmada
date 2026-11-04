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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Row A1: an Assigned action paused under a stop the robot had not confirmed keeps its binding
// once the lease has provably expired, whether the stop is now confirmed or cleared. Only a row out
// of Paused (A2, A4, A5, A24, A25) releases the binding and the robot's record; the dead lease is
// then that row's proof. It was released here, and with a cleared stop the record went too.
func TestEstop_UnconfirmedAssignedPauseKeepsItsRobotPastTheLease(t *testing.T) {
	for _, state := range []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopNormal} {
		t.Run(string(state), func(t *testing.T) {
			dead := &metav1.Time{Time: time.Now().Add(-time.Hour)}
			action := assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 4, dead)
			action.Status.Message = estopPausedUnconfirmedMessage
			r, c := newActionReconciler(t, action, robotEstopped("r1", state, "t1"))

			reconcileAction(t, r, "t1")

			ft := getAction(t, c, "t1")
			if ft.Status.Phase != fleetv1.ActionPhasePaused || ft.Status.AssignedRobot != "r1" {
				t.Fatalf("phase=%s robot=%q; want Paused, still bound to r1 (A1)", ft.Status.Phase, ft.Status.AssignedRobot)
			}
			if got := getRobot(t, c, "r1", actionNS).Status.AssignedAction; got != "t1" {
				t.Fatalf("r1's record of t1 = %q; want it kept until a row out of Paused releases it", got)
			}
		})
	}
}
