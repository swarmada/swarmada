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

var goDayToDOW = map[time.Weekday]fleetv1.DayOfWeek{
	time.Sunday:    fleetv1.Sunday,
	time.Monday:    fleetv1.Monday,
	time.Tuesday:   fleetv1.Tuesday,
	time.Wednesday: fleetv1.Wednesday,
	time.Thursday:  fleetv1.Thursday,
	time.Friday:    fleetv1.Friday,
	time.Saturday:  fleetv1.Saturday,
}

func dowOf(t time.Time) fleetv1.DayOfWeek { return goDayToDOW[t.UTC().Weekday()] }

func TestWithinMaintenanceWindow(t *testing.T) {
	// A fixed reference instant; DayOfWeek is derived from it so the test never
	// depends on hand-computed calendar arithmetic.
	ref := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC) // 10:00 UTC
	today := dowOf(ref)
	yesterday := dowOf(ref.AddDate(0, 0, -1))

	win := func(d fleetv1.DayOfWeek, startHour, dur int32) *fleetv1.MaintenanceWindow {
		return &fleetv1.MaintenanceWindow{DayOfWeek: d, StartHour: startHour, DurationMinutes: dur}
	}

	cases := []struct {
		name string
		w    *fleetv1.MaintenanceWindow
		now  time.Time
		want bool
	}{
		{"inside", win(today, 9, 120), ref, true},                                             // 09:00–11:00, now 10:00
		{"at open edge", win(today, 10, 60), ref, true},                                       // 10:00–11:00, now 10:00
		{"just before open", win(today, 11, 60), ref, false},                                  // 11:00–12:00
		{"after close", win(today, 8, 60), ref, false},                                        // 08:00–09:00
		{"wrong day", win(yesterday, 9, 120), ref, false},                                     // right time, wrong weekday
		{"spans midnight into today", win(yesterday, 23, 180), ref.Add(-9 * time.Hour), true}, // yest 23:00 +3h → 02:00 today; now 01:00
		{"nil window", nil, ref, false},
	}
	for _, tc := range cases {
		if got := withinMaintenanceWindow(tc.w, tc.now); got != tc.want {
			t.Errorf("%s: withinMaintenanceWindow = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWithinMaintenanceWindow_WeekWrap(t *testing.T) {
	// Sunday 23:00 for 180 min runs into Monday 02:00 — the week boundary.
	sunLate := time.Date(2026, 7, 5, 23, 30, 0, 0, time.UTC) // a Sunday 23:30
	if dowOf(sunLate) != fleetv1.Sunday {
		t.Skipf("fixture date is %s, not Sunday; skipping", dowOf(sunLate))
	}
	w := &fleetv1.MaintenanceWindow{DayOfWeek: fleetv1.Sunday, StartHour: 23, DurationMinutes: 180}
	monEarly := sunLate.Add(90 * time.Minute) // Monday 01:00
	if !withinMaintenanceWindow(w, monEarly) {
		t.Error("a Sunday-night window should still be open early Monday (week wrap)")
	}
}

func TestWithinRolloutWindow(t *testing.T) {
	ref := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	robNoWindow := &fleetv1.Robot{}
	robInWindow := &fleetv1.Robot{Spec: fleetv1.RobotSpec{
		MaintenanceWindow: &fleetv1.MaintenanceWindow{DayOfWeek: dowOf(ref), StartHour: 9, DurationMinutes: 120}}}

	// Not windowOnly → always allowed, even with no window.
	if !withinRolloutWindow(robNoWindow, false, ref) {
		t.Error("non-windowOnly rollout must always be allowed")
	}
	// windowOnly + no window → never allowed.
	if withinRolloutWindow(robNoWindow, true, ref) {
		t.Error("windowOnly with no configured window must be skipped")
	}
	// windowOnly + inside window → allowed.
	if !withinRolloutWindow(robInWindow, true, ref) {
		t.Error("windowOnly inside the window must be allowed")
	}
}
