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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// A Robot with a deletionTimestamp is going: an object a finalizer keeps after its
// delete takes no new work, whatever it holds. Without this a terminating robot stayed
// a candidate while its delete waited on the finalizer (RFC-0001
// #control-plane-scheduler).
func TestExcludedByForAssignment_ATerminatingRobotIsExcluded(t *testing.T) {
	now := metav1.Now()
	robot := capRobot("r1", fleetv1.CapabilityStatusActive)
	robot.DeletionTimestamp = &now
	robot.Finalizers = []string{"example.com/hold"}

	ex := scheduler.ExcludedByForAssignment(&robot, camAction(nil), false)
	if ex == nil {
		t.Fatal("a robot being deleted is a candidate for new work")
	}
	if !strings.Contains(ex.Reason, "being deleted") {
		t.Errorf("the exclusion does not say the robot is being deleted: %+v", ex)
	}

	s := scheduler.NewDefaultScheduler()
	if got, err := s.SelectRobot(camAction(nil), []fleetv1.Robot{robot}, false, false, false); err == nil {
		t.Fatalf("the scheduler selected %s, which is being deleted", got.Name)
	}

	// The control: the same robot, not being deleted, is a candidate.
	robot.DeletionTimestamp = nil
	if ex := scheduler.ExcludedByForAssignment(&robot, camAction(nil), false); ex != nil {
		t.Fatalf("the control robot was excluded: %+v", ex)
	}
}
