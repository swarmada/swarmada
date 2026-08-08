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

package cli

import (
	"fmt"
	"strings"
)

// None is the placeholder for an empty optional value, matching kubectl's
// "<none>".
const None = "<none>"

// Unknown is rendered where a coarse status projection has never been written
// (e.g. a robot that has not reported battery yet), matching the style guide's
// em-dash convention.
const Unknown = "—"

// RobotPhaseCell colorizes a Robot's status.phase. The color *scale* is the one
// the style guide defines (§ "Phase"), applied to the actual RobotPhase enum
// values (api/v1/robot_types.go): green = healthy/available, cyan = working,
// yellow = expected/self-resolving, red = impaired, reverse = unmissable
// (Offline), dim = inert/not-an-action. An unrecognized phase renders plain so
// the CLI never hides a value it has no color for.
func RobotPhaseCell(phase string) Cell {
	switch phase {
	case "Idle":
		return styledCell(sgrGreen, phase)
	case "Assigned", "InProgress":
		return styledCell(sgrCyan, phase)
	case "Charging":
		return styledCell(sgrYellow, phase)
	case "Error":
		return styledCell(sgrRed, phase)
	case "Offline":
		return styledCell(sgrOffline, phase)
	case "Discovered", "Maintenance":
		return styledCell(sgrDim, phase)
	case "":
		return TextCell(Unknown)
	default:
		return TextCell(phase)
	}
}

// BatteryCell colorizes status.batteryPercent by the level thresholds
// (50-100 green, 20-49 yellow, 0-19 bold red). A nil percent renders as the
// em-dash placeholder — the projection has never been written. When the robot
// is charging the value is prefixed with a yellow ⚡ regardless of level, since
// a robot at 8%-and-charging is not the same concern as 8%-and-idle.
func BatteryCell(percent *int, charging bool) Cell {
	if percent == nil {
		return TextCell(Unknown)
	}
	p := *percent
	if charging {
		return styledCell(sgrYellow, fmt.Sprintf("⚡ %d%%", p))
	}
	switch {
	case p >= 50:
		return styledCell(sgrGreen, fmt.Sprintf("%d%%", p))
	case p >= 20:
		return styledCell(sgrYellow, fmt.Sprintf("%d%%", p))
	default:
		return styledCell(sgrBoldRed, fmt.Sprintf("%d%%", p))
	}
}

// BatteryBar renders the 20-cell describe-view battery bar
// (filled = round(pct/5)), colored by the same thresholds as BatteryCell.
// Returns the colored bar and the colored numeric label as separate strings so
// the caller controls layout.
func BatteryBar(enabled bool, percent int, charging bool) (label, bar string) {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := (percent + 2) / 5 // round to nearest cell
	if filled > 20 {
		filled = 20
	}
	cells := strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)

	var sgr string
	switch {
	case charging:
		sgr = sgrYellow
	case percent >= 50:
		sgr = sgrGreen
	case percent >= 20:
		sgr = sgrYellow
	default:
		sgr = sgrBoldRed
	}
	return paint(enabled, sgr, fmt.Sprintf("%d%%", percent)), paint(enabled, sgr, cells)
}

// HardwareStatusCell colorizes a status.hardware[*] component health value.
func HardwareStatusCell(status string) Cell {
	switch status {
	case "Healthy":
		return styledCell(sgrGreen, status)
	case "Degraded":
		return styledCell(sgrYellow, status)
	case "Absent":
		return styledCell(sgrRed, status)
	default:
		return TextCell(status)
	}
}

// OutcomeCell colorizes an audit-log outcome (§9.5.4): Allowed green, Denied
// red, Error yellow. A denied action is never silently dropped, so it must read
// as distinct from a healthy allow at a glance.
func OutcomeCell(outcome string) Cell {
	switch outcome {
	case "Allowed":
		return styledCell(sgrGreen, outcome)
	case "Denied":
		return styledCell(sgrRed, outcome)
	case "Error":
		return styledCell(sgrYellow, outcome)
	default:
		return TextCell(outcome)
	}
}

// VerdictCell colors a pass/fail verdict word — green when ok, bold red when not
// — for a single-glance integrity result (e.g. audit verify's OK / TAMPERED).
func VerdictCell(ok bool, text string) Cell {
	if ok {
		return styledCell(sgrGreen, text)
	}
	return styledCell(sgrBoldRed, text)
}

// ConditionStatusCell colorizes a metav1.Condition status (True/False/Unknown).
func ConditionStatusCell(status string) Cell {
	switch status {
	case "True":
		return styledCell(sgrGreen, status)
	case "False":
		return styledCell(sgrRed, status)
	case "Unknown":
		return styledCell(sgrDim, status)
	default:
		return TextCell(status)
	}
}
