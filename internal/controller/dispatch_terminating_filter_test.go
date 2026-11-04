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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A Robot with a deletionTimestamp gets no new work. filterDispatchEligible is the choke
// point for both the normal selection and the preemption search, so the exclusion is
// here as well as in the scheduler's own check. Checked ahead of the estop filter: a
// terminating robot is withheld whatever its estop state.
func TestFilterDispatchEligible_ATerminatingRobotIsWithheld(t *testing.T) {
	for _, state := range []fleetv1.RobotEstopState{"", fleetv1.RobotEstopNormal, fleetv1.RobotEstopStopped} {
		t.Run(string(state)+"/", func(t *testing.T) {
			now := metav1.Now()
			robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "")
			robot.Status.EstopState = state
			robot.DeletionTimestamp = &now
			robot.Finalizers = []string{"example.com/hold"}

			r, _ := newActionReconciler(t, robot)
			eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(),
				pendingInBand("t1", fleetv1.ActionPriorityNormal), []fleetv1.Robot{*robot})
			if len(eligible) != 0 {
				t.Fatalf("a robot being deleted is dispatch-eligible")
			}
			if len(withheld) != 1 || !strings.Contains(withheld[0].Reason, "being deleted") {
				t.Fatalf("want one exclusion naming the delete, got %+v", withheld)
			}
		})
	}
}
