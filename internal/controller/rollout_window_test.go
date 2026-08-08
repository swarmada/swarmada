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

// windowNow returns a maintenance window that is open at the current UTC hour, and
// one two days away (closed). MaxUnavailable is widened by the caller so the window
// gate — not the batch cap — is what excludes the closed robot.
func windowNow() (open, closed *fleetv1.MaintenanceWindow) {
	now := time.Now().UTC()
	open = &fleetv1.MaintenanceWindow{DayOfWeek: dowOf(now), StartHour: int32(now.Hour()), DurationMinutes: 60}
	other := now.AddDate(0, 0, 2)
	closed = &fleetv1.MaintenanceWindow{DayOfWeek: dowOf(other), StartHour: 0, DurationMinutes: 60}
	return open, closed
}

// A FirmwareRollout with maintenanceWindowOnly updates only robots currently inside
// their window; a robot outside its window is skipped even with batch slots free.
func TestFirmwareRollout_MaintenanceWindowOnly(t *testing.T) {
	open, closed := windowNow()
	secret, sigRef := signingFixture(t, fwChecksum)
	ro := fwRollout(sigRef)
	ro.Spec.Strategy.RollingUpdate = &fleetv1.RollingUpdateStrategy{MaxUnavailable: "2"} // both would update but for the window gate
	ro.Spec.SafetyConstraints.MaintenanceWindowOnly = true

	inRobot := targetRobot("in-window", fleetv1.RobotPhaseIdle, 90)
	inRobot.Spec.MaintenanceWindow = open
	outRobot := targetRobot("out-window", fleetv1.RobotPhaseIdle, 90)
	outRobot.Spec.MaintenanceWindow = closed

	r, c := newFirmwareReconciler(t, ro, signingConfig(true), secret, inRobot, outRobot)
	reconcileFirmware(t, r)

	if !robotPending(t, c, "in-window") {
		t.Error("robot inside its maintenance window should have been updated")
	}
	if robotPending(t, c, "out-window") {
		t.Error("robot outside its maintenance window must be skipped (maintenanceWindowOnly)")
	}
}

// Same for ModelRollout batch entry.
func TestModelRollout_MaintenanceWindowOnly(t *testing.T) {
	open, closed := windowNow()
	ro := pickerRollout()
	ro.Spec.Strategy.RollingUpdate = &fleetv1.RollingUpdateStrategy{MaxUnavailable: "2"}
	ro.Spec.SafetyConstraints.MaintenanceWindowOnly = true

	inRobot := targetRobot("in-window", fleetv1.RobotPhaseIdle, 90)
	inRobot.Spec.MaintenanceWindow = open
	outRobot := targetRobot("out-window", fleetv1.RobotPhaseIdle, 90)
	outRobot.Spec.MaintenanceWindow = closed

	r, c := newRolloutReconciler(t, ro, inRobot, outRobot)
	reconcileRollout(t, r)

	if e := modelEntry(getRobot(t, c, "in-window", rolloutNS), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusUpdating {
		t.Error("robot inside its maintenance window should have entered the batch")
	}
	if e := modelEntry(getRobot(t, c, "out-window", rolloutNS), "item-recognition"); e != nil && e.Status == fleetv1.ModelStatusUpdating {
		t.Error("robot outside its maintenance window must be skipped (maintenanceWindowOnly)")
	}
}
