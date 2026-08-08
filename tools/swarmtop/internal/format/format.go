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

// Package format turns view values into display strings plus a semantic
// severity Level. It deliberately does not import lipgloss: it decides *what*
// the text says and *how severe* it is; internal/ui maps Level to concrete
// terminal colors. That split keeps these helpers pure and table-testable, and
// keeps the swarmctl color thresholds defined in exactly one place.
package format

import (
	"fmt"
	"time"

	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// Level is a semantic severity the UI maps to a color. It intentionally mirrors
// the swarmctl CLI convention so swarmtop and swarmctl read consistently.
type Level int

// Severity levels, ordered from least to most severe. The UI maps each to a
// terminal color.
const (
	LevelMuted Level = iota // unknown / not-applicable / dim
	LevelGood               // healthy / active / green
	LevelWarn               // degraded / caution / yellow
	LevelBad                // failed / offline / red
)

// Battery thresholds match the swarmctl CLI color convention:
// green > 50, yellow 20–50, red < 20.
const (
	batteryWarnAtOrBelow = 50
	batteryBadBelow      = 20
)

// Battery renders a battery percentage cell and its severity. A nil percent is
// "--" at LevelMuted (unreported), distinct from a real 0%.
func Battery(pct *int32) (string, Level) {
	if pct == nil {
		return "--", LevelMuted
	}
	p := *pct
	text := fmt.Sprintf("%d%%", p)
	switch {
	case p < batteryBadBelow:
		return text, LevelBad
	case p <= batteryWarnAtOrBelow:
		return text, LevelWarn
	default:
		return text, LevelGood
	}
}

// RobotPhase maps a robot phase to a severity for coloring the phase cell.
func RobotPhase(phase string) Level {
	switch phase {
	case "Idle", "InProgress", "Assigned", "Charging":
		return LevelGood
	case "Maintenance", "Discovered":
		return LevelWarn
	case "Error", "Offline":
		return LevelBad
	default:
		return LevelMuted
	}
}

// CapabilityStatus maps a capability status to a severity.
func CapabilityStatus(status string) Level {
	switch status {
	case "Active":
		return LevelGood
	case "Degraded", "Paused", "Inactive":
		return LevelWarn
	case "Unavailable", "Failed":
		return LevelBad
	default:
		return LevelMuted
	}
}

// HardwareStatus maps a hardware component status to a severity.
func HardwareStatus(status string) Level {
	switch status {
	case "Healthy":
		return LevelGood
	case "Degraded":
		return LevelWarn
	case "Failed":
		return LevelBad
	default:
		return LevelMuted
	}
}

// ActionPhase maps a FleetAction phase to a severity for the action view.
func ActionPhase(phase string) Level {
	switch phase {
	case "Succeeded":
		return LevelGood
	case "Pending", "Assigned", "InProgress":
		return LevelMuted
	case "Revoking":
		return LevelWarn
	case "Failed":
		return LevelBad
	default:
		return LevelMuted
	}
}

// AdapterPhase maps a FleetAdapter phase to a severity for the adapter view.
func AdapterPhase(phase string) Level {
	switch phase {
	case "Connected", "Ready":
		return LevelGood
	case "Connecting", "Degraded":
		return LevelWarn
	case "Disconnected", "Rejected", "Error":
		return LevelBad
	default:
		return LevelMuted
	}
}

// Conformance maps a FleetAdapter conformance state to a severity. Only Passed
// is good — robots are admitted only against a Passed adapter.
func Conformance(state string) Level {
	switch state {
	case "Passed":
		return LevelGood
	case "Pending", "Unknown", "":
		return LevelWarn
	case "Failed":
		return LevelBad
	default:
		return LevelMuted
	}
}

// EventType maps a core/v1 Event type to a severity. Warning stands out;
// Normal is dim so the eye lands on problems.
func EventType(t string) Level {
	switch t {
	case "Warning":
		return LevelWarn
	case "Normal":
		return LevelMuted
	default:
		return LevelMuted
	}
}

// ConditionStatus maps a condition's Status to a severity as a neutral
// heuristic (True=good, False=warn, Unknown=muted). Condition semantics vary by
// type, so this is a display convenience, not a judgment about the condition.
func ConditionStatus(status string) Level {
	switch status {
	case "True":
		return LevelGood
	case "False":
		return LevelWarn
	default:
		return LevelMuted
	}
}

// CapSummary renders the list-view capability roll-up. When every capability is
// active it shows just the count (e.g. "3") in green — no "N/N" noise when nothing
// is wrong. When some are degraded it shows "active/total" plus the headline
// problem (e.g. "2/3 cam_front"), colored by the worst state.
func CapSummary(s k8sclient.CapSummary) (string, Level) {
	if s.Total == 0 {
		return "—", LevelMuted
	}
	if s.Active == s.Total {
		return fmt.Sprintf("%d", s.Active), LevelGood
	}
	return fmt.Sprintf("%d/%d %s", s.Active, s.Total, s.FirstProblem), CapabilityStatus(s.FirstProblemState)
}

// Estop renders an estop cell; anything other than Normal is a warning/bad.
func Estop(state string) (string, Level) {
	switch state {
	case "", "Normal":
		return "Normal", LevelGood
	case "Resuming":
		return state, LevelWarn
	default: // Stopping, Stopped, Failed
		return state, LevelBad
	}
}

// Age renders a compact relative age ("3s", "4m", "2h", "1d") measured from
// ref to t. It is used for telemetry/heartbeat freshness. unknown is true when
// there is no timestamp at all.
func Age(t, ref time.Time, unknown bool) string {
	if unknown || t.IsZero() {
		return "—"
	}
	d := ref.Sub(t)
	if d < 0 {
		d = 0
	}
	return compactDuration(d)
}

// compactDuration renders a non-negative duration as "3s"/"4m"/"2h"/"1d".
func compactDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Deadline renders the time remaining until a FleetAction deadline, measured from
// now. Nil is "—" (muted); a passed deadline is "overdue" (bad); within five
// minutes is a warning; otherwise the remaining time is shown muted.
func Deadline(deadline *time.Time, now time.Time) (string, Level) {
	if deadline == nil {
		return "—", LevelMuted
	}
	rem := deadline.Sub(now)
	if rem <= 0 {
		return "overdue", LevelBad
	}
	text := "in " + compactDuration(rem)
	if rem <= 5*time.Minute {
		return text, LevelWarn
	}
	return text, LevelMuted
}

// TelemetryAge renders a robot's last-telemetry freshness and flags staleness.
// Anything older than staleAfter is LevelBad (likely disconnected); within it,
// LevelGood. This is a display heuristic only — swarmtop never infers safety
// state from silence (that discipline lives in the control plane).
func TelemetryAge(last, ref time.Time, unknown bool, staleAfter time.Duration) (string, Level) {
	if unknown || last.IsZero() {
		return "—", LevelMuted
	}
	text := Age(last, ref, false)
	if ref.Sub(last) > staleAfter {
		return text, LevelBad
	}
	return text, LevelGood
}

// Dash returns s, or "—" when s is empty, for optional string cells.
func Dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
