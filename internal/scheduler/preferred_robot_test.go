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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// spec.preferredRobot is a SOFT preference (ADR-0034): it reorders the eligible set and
// never filters it. These tests pin the difference from spec.robotSelector, which is a hard
// pin — the distinction is the entire reason the field exists, and it is the thing most
// likely to be eroded by a later change.

// prefRobot builds an Idle robot carrying one Active capability, so eligibility can be
// varied independently of ranking.
func prefRobot(name string, battery int32, capName string) fleetv1.Robot {
	b := battery
	r := fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseIdle, BatteryPercent: &b},
	}
	if capName != "" {
		r.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
			{Name: capName, Status: fleetv1.CapabilityStatusActive},
		}
	}
	return r
}

// prefAction requests a preferred robot (empty = no hint) and optionally a capability.
func prefAction(preferred string, requires ...string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{
		Type:                 fleetv1.ActionTypeCustom,
		PreferredRobot:       preferred,
		RequiredCapabilities: requires,
	}}
}

// The whole point: the named robot wins even with less battery.
func TestSelectRobot_PreferredRobotOutranksBattery(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{prefRobot("amr-1", 95, ""), prefRobot("robot-1", 20, "")}

	got, err := s.SelectRobot(prefAction("robot-1"), robots, false, false, true)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	if got.Name != "robot-1" {
		t.Fatalf("the preferred robot must rank first, got %q", got.Name)
	}
}

// THE CRITICAL ONE. A preferred robot that cannot do the work is not eligible, so the fleet
// takes the action. If this ever fails, preferredRobot has become a pin and will hand work to
// a robot that cannot perform it — exactly the failure mode robotSelector already has.
func TestSelectRobot_PreferredRobotNeverFilters(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	// robot-1 is preferred but lacks the required capability; amr-1 has it.
	robots := []fleetv1.Robot{prefRobot("amr-1", 50, "camera"), prefRobot("robot-1", 99, "")}

	got, err := s.SelectRobot(prefAction("robot-1", "camera"), robots, false, false, true)
	if err != nil {
		t.Fatalf("an ineligible preferred robot must not strand the action: %v", err)
	}
	if got.Name != "amr-1" {
		t.Fatalf("the fleet must take the action when the preferred robot is ineligible, got %q", got.Name)
	}
}

// Naming a robot that is not in the candidate set is not an error — the hint is simply inert.
func TestSelectRobot_UnknownPreferredRobotIsInert(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{prefRobot("amr-1", 40, ""), prefRobot("amr-2", 90, "")}

	got, err := s.SelectRobot(prefAction("does-not-exist"), robots, false, false, true)
	if err != nil {
		t.Fatalf("an unknown preferred robot must not fail selection: %v", err)
	}
	if got.Name != "amr-2" {
		t.Fatalf("ranking should fall through to battery, got %q", got.Name)
	}
}

// With the namespace flag off the hint is ignored and ranking is battery-only.
func TestSelectRobot_PreferredRobotOffIgnoresHint(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{prefRobot("amr-1", 95, ""), prefRobot("robot-1", 20, "")}

	got, err := s.SelectRobot(prefAction("robot-1"), robots, false, false, false)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	if got.Name != "amr-1" {
		t.Fatalf("with the flag off the hint must be inert, got %q", got.Name)
	}
}

// Preferred robot outranks the manufacturer preference: it names ONE robot, so anything that
// could outrank it would make the field unobservable in a fleet with a manufacturer hint set.
func TestSelectRobot_PreferredRobotOutranksManufacturer(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	match := mfrRobot("amr-borealis", "borealis", 90)
	preferred := prefRobot("robot-1", 10, "")

	action := prefAction("robot-1")
	action.Spec.PreferredManufacturer = "borealis"

	got, err := s.SelectRobot(action, []fleetv1.Robot{match, preferred}, false, true, true)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	if got.Name != "robot-1" {
		t.Fatalf("preferredRobot must outrank preferredManufacturer, got %q", got.Name)
	}
}

// No hint means byte-identical behaviour to before the field existed, whatever the flag says.
func TestSelectRobot_NoPreferredRobotHintIsBatteryOnly(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{prefRobot("amr-1", 30, ""), prefRobot("amr-2", 80, "")}

	got, err := s.SelectRobot(prefAction(""), robots, false, false, true)
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	if got.Name != "amr-2" {
		t.Fatalf("without a hint ranking is battery-only, got %q", got.Name)
	}
}
