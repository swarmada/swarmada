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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A paused robot's non-pauseable capabilities that are still running are recorded
// in status.continuousCapabilities; its paused (pauseable) and inactive ones are
// not.
func TestZM_RecordsContinuousCapabilities(t *testing.T) {
	now := zmBase
	zm := zoneMaint("db-maint", fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}})
	rob := zmRobot("r-paused", "z1", fleetv1.RobotPhaseMaintenance, "")
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "safety-monitor", Status: fleetv1.CapabilityStatusActive, Paused: false}, // non-pauseable, running
		{Name: "estop-relay", Status: fleetv1.CapabilityStatusDegraded, Paused: false},  // non-pauseable, degraded → still running
		{Name: "navigate", Status: fleetv1.CapabilityStatusPaused, Paused: true},        // pauseable, paused → excluded
		{Name: "camera-ai", Status: fleetv1.CapabilityStatusInactive, Paused: false},    // non-pauseable but not running → excluded
	}
	r, c := newZMReconciler(t, &now, zm, rob)

	driveToActive(t, r, "db-maint")

	got := getZM(t, c, "db-maint")
	if len(got.Status.ContinuousCapabilities) != 1 || got.Status.ContinuousCapabilities[0].RobotName != "r-paused" {
		t.Fatalf("continuousCapabilities = %+v, want one entry for r-paused", got.Status.ContinuousCapabilities)
	}
	caps := got.Status.ContinuousCapabilities[0].Capabilities
	if len(caps) != 2 || caps[0] != "estop-relay" || caps[1] != "safety-monitor" {
		t.Errorf("continuous caps = %v, want [estop-relay safety-monitor] (sorted; excludes paused + inactive)", caps)
	}
}

// A robot with only pauseable/inactive capabilities produces no continuous entry.
func TestZM_NoContinuousCapabilitiesWhenAllPaused(t *testing.T) {
	now := zmBase
	zm := zoneMaint("db-maint", fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}})
	rob := zmRobot("r-paused", "z1", fleetv1.RobotPhaseMaintenance, "")
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "navigate", Status: fleetv1.CapabilityStatusPaused, Paused: true},
		{Name: "transport", Status: fleetv1.CapabilityStatusPaused, Paused: true},
	}
	r, c := newZMReconciler(t, &now, zm, rob)

	driveToActive(t, r, "db-maint")

	if got := getZM(t, c, "db-maint").Status.ContinuousCapabilities; len(got) != 0 {
		t.Fatalf("continuousCapabilities = %+v, want empty (all pauseable)", got)
	}
}

// continuousCapabilities is cleared when the maintenance completes.
func TestZM_ContinuousCapabilitiesClearedOnComplete(t *testing.T) {
	now := zmBase
	zm := zoneMaint("timed", fleetv1.ZoneMaintenanceSpec{
		Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeNamespace}, AutoResumeAfterMinutes: 10,
	})
	rob := zmRobot("r-paused", "z1", fleetv1.RobotPhaseMaintenance, "")
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "safety-monitor", Status: fleetv1.CapabilityStatusActive, Paused: false},
	}
	r, c := newZMReconciler(t, &now, zm, rob)

	driveToActive(t, r, "timed")
	if len(getZM(t, c, "timed").Status.ContinuousCapabilities) != 1 {
		t.Fatal("expected a continuous-capability entry while active")
	}

	now = zmBase.Add(11 * time.Minute) // past auto-resume
	reconcileZM(t, r, "timed")

	got := getZM(t, c, "timed")
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseCompleted {
		t.Fatalf("phase = %s, want Completed", got.Status.Phase)
	}
	if got.Status.ContinuousCapabilities != nil {
		t.Errorf("continuousCapabilities not cleared on completion: %+v", got.Status.ContinuousCapabilities)
	}
}
