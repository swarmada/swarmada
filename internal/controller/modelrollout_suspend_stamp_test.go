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

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// When a robot enters the model-update batch (its model is marked Updating, which
// suspends the model-driven capabilities), the controller stamps
// currentBatch[].capabilitiesSuspendedAt for that robot (ADR-0023).
func TestModelRollout_StampsCapabilitiesSuspendedAt(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(), targetRobot("r1", fleetv1.RobotPhaseIdle, 90))
	reconcileRollout(t, r)

	// The robot's model is now Updating (suspended)...
	if e := modelEntry(getRobot(t, c, "r1", rolloutNS), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusUpdating {
		t.Fatal("r1 did not enter the batch (model not Updating)")
	}

	// ...and the batch entry carries the suspension stamp.
	ro := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "roll", Namespace: rolloutNS}, ro); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	var entry *fleetv1.RolloutBatchRobot
	for i := range ro.Status.CurrentBatch {
		if ro.Status.CurrentBatch[i].RobotName == "r1" {
			entry = &ro.Status.CurrentBatch[i]
		}
	}
	if entry == nil {
		t.Fatalf("r1 not present in currentBatch: %+v", ro.Status.CurrentBatch)
	}
	if entry.CapabilitiesSuspendedAt == nil {
		t.Fatal("capabilitiesSuspendedAt not stamped on suspend")
	}
}
