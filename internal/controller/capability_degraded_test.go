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

// The capability derivation stamps DegradedSchedulable from the capability
// definition's degradedPolicy.schedulable, so the Scheduler (which reads only
// Robot.status) can serve lower-constraint actions on a Degraded capability.
func TestDeriveCapabilities_DegradedSchedulableFromPolicy(t *testing.T) {
	withPolicy := hwNative("payload", true, "load")
	withPolicy.DegradedPolicy = &fleetv1.CapabilityDegradedPolicy{Schedulable: true}
	withoutPolicy := hwNative("nav", true, "lidar")

	robot := &fleetv1.Robot{
		Spec: fleetv1.RobotSpec{
			Capabilities: []fleetv1.ClassCapability{withPolicy, withoutPolicy},
		},
		Status: fleetv1.RobotStatus{
			Hardware: []fleetv1.HardwareComponentStatus{
				{Name: "load", Status: fleetv1.HardwareDegraded},
				{Name: "lidar", Status: fleetv1.HardwareDegraded},
			},
		},
	}

	byName := map[string]fleetv1.CapabilityStatusEntry{}
	for _, e := range deriveCapabilities(robot) {
		byName[e.Name] = e
	}

	if byName["payload"].Status != fleetv1.CapabilityStatusDegraded {
		t.Fatalf("payload status = %s, want Degraded", byName["payload"].Status)
	}
	if !byName["payload"].DegradedSchedulable {
		t.Fatal("degradedPolicy.schedulable=true must stamp DegradedSchedulable=true")
	}
	if byName["nav"].DegradedSchedulable {
		t.Fatal("a capability without degradedPolicy must not be DegradedSchedulable")
	}
}
