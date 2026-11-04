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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A robot under an emergency stop, or in the post-clear window, is not entered into a
// firmware or a model rollout's batch, whatever else it satisfies.
func TestRolloutEligibility_ARobotUnderAStopIsHeldBack(t *testing.T) {
	pct := int32(100)
	ready := func(mut func(*fleetv1.Robot)) *fleetv1.Robot {
		r := &fleetv1.Robot{Status: fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseIdle, BatteryPercent: &pct}}
		if mut != nil {
			mut(r)
		}
		return r
	}
	fw := &fleetv1.FirmwareRollout{}
	mr := &fleetv1.ModelRollout{}
	if !eligibleForFirmwareUpdate(ready(nil), fw) || !eligibleForModelUpdate(ready(nil), mr) {
		t.Fatal("control: an Idle, charged robot with no stop is not eligible")
	}
	for name, mut := range map[string]func(*fleetv1.Robot){
		"Stopped":  func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopped },
		"Stopping": func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopping },
		"Failed":   func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopFailed },
		"Resuming": func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopResuming },
		"its own trigger": func(r *fleetv1.Robot) {
			r.Annotations = map[string]string{annEstopTriggered: "own"}
		},
		"a recorded zone stop": func(r *fleetv1.Robot) {
			r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "z"}}
		},
	} {
		if eligibleForFirmwareUpdate(ready(mut), fw) {
			t.Errorf("%s: eligible for a firmware update", name)
		}
		if eligibleForModelUpdate(ready(mut), mr) {
			t.Errorf("%s: eligible for a model update", name)
		}
	}
}
