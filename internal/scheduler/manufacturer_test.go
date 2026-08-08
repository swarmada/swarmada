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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// mfrRobot builds an Idle robot with a manufacturer and battery level, no
// capability requirements so eligibility is trivial and ranking is the only
// variable under test.
func mfrRobot(name, manufacturer string, battery int32) fleetv1.Robot {
	b := battery
	return fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       fleetv1.RobotSpec{Manufacturer: manufacturer},
		Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseIdle, BatteryPercent: &b},
	}
}

// hintAction requests a preferred manufacturer (empty = no hint).
func hintAction(preferred string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{
		Type:                  fleetv1.ActionTypeCustom,
		PreferredManufacturer: preferred,
	}}
}

// With the flag on and a hint set, a manufacturer match outranks battery: the
// matching robot wins even though a non-matching robot has more battery.
func TestSelectRobot_ManufacturerMatchOutranksBattery(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{
		mfrRobot("high-other", "Fetch", 95),
		mfrRobot("low-match", "borealis", 40),
	}
	got, err := s.SelectRobot(hintAction("borealis"), robots, false, true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "low-match" {
		t.Fatalf("selected %q, want low-match (manufacturer match must outrank battery)", got.Name)
	}
}

// The tiebreak is soft: with no matching-manufacturer robot the highest-battery
// robot is still selected — the hint never filters the candidate set.
func TestSelectRobot_ManufacturerPreferenceNeverFilters(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{
		mfrRobot("low-other", "Fetch", 30),
		mfrRobot("high-other", "Boston Dynamics", 88),
	}
	got, err := s.SelectRobot(hintAction("borealis"), robots, false, true, false)
	if err != nil {
		t.Fatalf("no matching manufacturer must not exclude anyone: %v", err)
	}
	if got.Name != "high-other" {
		t.Fatalf("selected %q, want high-other (battery order among non-matches)", got.Name)
	}
}

// Within the matching group, battery still decides.
func TestSelectRobot_ManufacturerTieBrokenByBattery(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{
		mfrRobot("match-low", "borealis", 55),
		mfrRobot("match-high", "borealis", 90),
		mfrRobot("other-full", "Fetch", 100),
	}
	got, err := s.SelectRobot(hintAction("borealis"), robots, false, true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "match-high" {
		t.Fatalf("selected %q, want match-high (battery breaks the manufacturer tie)", got.Name)
	}
}

// With the namespace flag off, the hint is ignored: pure battery order.
func TestSelectRobot_ManufacturerPreferenceOffIgnoresHint(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{
		mfrRobot("high-other", "Fetch", 95),
		mfrRobot("low-match", "borealis", 40),
	}
	got, err := s.SelectRobot(hintAction("borealis"), robots, false, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "high-other" {
		t.Fatalf("selected %q, want high-other (flag off ⇒ battery-only)", got.Name)
	}
}

// With no hint the flag has no effect: pure battery order even when on.
func TestSelectRobot_NoHintIsBatteryOnly(t *testing.T) {
	s := scheduler.NewDefaultScheduler()
	robots := []fleetv1.Robot{
		mfrRobot("high-other", "Fetch", 95),
		mfrRobot("low-match", "borealis", 40),
	}
	got, err := s.SelectRobot(hintAction(""), robots, false, true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "high-other" {
		t.Fatalf("selected %q, want high-other (no hint ⇒ battery-only)", got.Name)
	}
}
