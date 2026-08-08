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
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const minutesPerWeek = 7 * 24 * 60

// dayOfWeekIndex maps a MaintenanceWindow.DayOfWeek to a Monday=0 … Sunday=6
// index (an unknown value → -1).
func dayOfWeekIndex(d fleetv1.DayOfWeek) int {
	switch d {
	case fleetv1.Monday:
		return 0
	case fleetv1.Tuesday:
		return 1
	case fleetv1.Wednesday:
		return 2
	case fleetv1.Thursday:
		return 3
	case fleetv1.Friday:
		return 4
	case fleetv1.Saturday:
		return 5
	case fleetv1.Sunday:
		return 6
	default:
		return -1
	}
}

// withinMaintenanceWindow reports whether now (evaluated in UTC) falls inside the
// recurring weekly window. It measures the offset from the window's start in
// "minutes since the start of the week", so a window that spans midnight or the
// Sunday→Monday boundary (e.g. Monday 23:00 for 480 minutes) is handled correctly.
// A nil window is never open.
func withinMaintenanceWindow(w *fleetv1.MaintenanceWindow, now time.Time) bool {
	if w == nil {
		return false
	}
	start := dayOfWeekIndex(w.DayOfWeek)
	if start < 0 {
		return false
	}
	u := now.UTC()
	// Go's time.Weekday is Sunday=0..Saturday=6; shift to Monday=0..Sunday=6.
	nowIdx := (int(u.Weekday()) + 6) % 7
	nowMin := nowIdx*24*60 + u.Hour()*60 + u.Minute()
	startMin := start*24*60 + int(w.StartHour)*60

	offset := ((nowMin-startMin)%minutesPerWeek + minutesPerWeek) % minutesPerWeek
	return offset < int(w.DurationMinutes)
}

// withinRolloutWindow gates a robot's rollout eligibility on the maintenance
// window. When the rollout is not windowOnly it is always allowed; when it is
// windowOnly the robot must be inside its configured spec.maintenanceWindow — a
// robot with no window configured is never eligible under windowOnly (§ rollout
// strategy: "only update robots during their configured MaintenanceWindow").
func withinRolloutWindow(rob *fleetv1.Robot, windowOnly bool, now time.Time) bool {
	if !windowOnly {
		return true
	}
	return withinMaintenanceWindow(rob.Spec.MaintenanceWindow, now)
}
