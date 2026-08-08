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

// Package components is the swarmtop view-model layer: pure functions that turn
// a k8sclient.Fleet snapshot into Bubbles table columns and rows. It is
// deliberately separable from the Bubble Tea program loop — RobotColumns and
// RobotRows are ordinary functions returning table.Column/table.Row values, so
// the entire rendering decision (which cell, what text, what color) is unit
// tested by calling them directly, with no tea.Program, no terminal, and no
// cluster. internal/ui wires the resulting values into a live table.Model.
package components

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/swarmada/swarmtop/internal/format"
	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// StaleTelemetryAfter is when a FleetAdapter's last-handshake age flips to red in
// the adapter-health view. (Per-robot connectivity is NOT shown this way: the
// control plane keeps per-frame liveness off the status write path (RA-1), so
// robot liveness is read from status.phase, not a telemetry age.) Display only —
// swarmtop never infers control-plane state from silence.
const StaleTelemetryAfter = 30 * time.Second

// Column indices, exported so the UI and tests can address cells by name.
const (
	ColName = iota
	ColPhase
	ColEstop
	ColBattery
	ColZone
	ColCaps
	ColAdapter
	ColEvents
	ColAction
)

// RobotColumns is the robot-list table schema. Widths are chosen so the common
// cell contents fit without truncation.
func RobotColumns() []table.Column {
	return []table.Column{
		{Title: "NAME", Width: 14},
		{Title: "PHASE", Width: 11},
		// Estop is INDEPENDENT of Phase (RFC-0001 §9.6.2.3) — a robot can be
		// Stopped while its Phase still reads Idle. Without this column an
		// emergency stop is invisible on the default screen, which is the one
		// the quickstart tells you to watch during the estop drill; it showed
		// only in the detail view, so the drill looked like nothing happened.
		{Title: "ESTOP", Width: 9},
		{Title: "BATT", Width: 6},
		{Title: "ZONE", Width: 6},
		{Title: "CAPABILITIES", Width: 20},
		{Title: "ADAPTER", Width: 18},
		{Title: "EVENTS", Width: 6},
		{Title: "TASK", Width: 16},
	}
}

// RobotColumnLayout returns the list column titles and widths, so the UI can
// render rows with an ANSI-aware layout (lipgloss) without importing the table
// package. This is needed because Bubbles v0.20's table.View truncates cells
// with an ANSI-unaware routine that corrupts embedded color; the UI keeps the
// table.Model for cursor/navigation state but renders the colored rows itself.
func RobotColumnLayout() (titles []string, widths []int) {
	cols := RobotColumns()
	titles = make([]string, len(cols))
	widths = make([]int, len(cols))
	for i, c := range cols {
		titles[i] = c.Title
		widths[i] = c.Width
	}
	return titles, widths
}

// RobotRows builds one table.Row per robot in the snapshot. now is retained for
// relative-time columns; robot liveness itself is read from status.phase (the
// PHASE column), since the control plane deliberately keeps per-frame
// connectivity off the status write path (RA-1), so status.connectivity is not
// a reliable staleness source.
func RobotRows(f k8sclient.Fleet, now time.Time) []table.Row {
	rows := make([]table.Row, 0, len(f.Robots))
	for i := range f.Robots {
		rows = append(rows, RobotRow(f, f.Robots[i], now))
	}
	return rows
}

// RobotRow renders a single robot into a table.Row. Each cell is a
// display-ready string, color-coded where the field carries severity.
func RobotRow(f k8sclient.Fleet, r k8sclient.RobotView, _ time.Time) table.Row {
	battText, battLvl := format.Battery(r.BatteryPercent)
	capText, capLvl := format.CapSummary(r.Caps)
	estopText, estopLvl := format.Estop(r.Estop)

	return table.Row{
		r.Name,
		Colorize(format.Dash(r.Phase), format.RobotPhase(r.Phase)),
		Colorize(estopText, estopLvl),
		Colorize(battText, battLvl),
		zoneCell(r),
		Colorize(capText, capLvl),
		format.Dash(r.AdapterName), // the serving adapter; liveness is the PHASE column
		eventBadge(f, r.Name),
		format.Dash(r.AssignedAction),
	}
}

// zoneCell shows the current zone, marking spec/current drift with a trailing
// asterisk.
func zoneCell(r k8sclient.RobotView) string {
	z := format.Dash(r.CurrentZone)
	if r.ZoneDrift {
		z += "*"
	}
	return z
}

// eventBadge summarizes a robot's recent events by Kubernetes event type. The
// Event API defines only two types — Warning and Normal — so the cell reads
// "<w>W <n>N" (e.g. "1W 12N"), with the warning count colored by severity and the
// normal count muted. Either part is omitted when its count is zero; the whole
// cell is empty when the robot has no events.
func eventBadge(f k8sclient.Fleet, robot string) string {
	var warn, normal int
	for _, e := range f.EventsByRobot[robot] {
		if e.Type == "Warning" {
			warn++
		} else {
			normal++ // Normal is the only other Kubernetes event type
		}
	}
	if warn == 0 && normal == 0 {
		return ""
	}
	parts := make([]string, 0, 2)
	if warn > 0 {
		lvl := format.LevelWarn
		if warn >= 3 {
			lvl = format.LevelBad
		}
		parts = append(parts, Colorize(fmt.Sprintf("%dW", warn), lvl))
	}
	if normal > 0 {
		parts = append(parts, Colorize(fmt.Sprintf("%dN", normal), format.LevelMuted))
	}
	return strings.Join(parts, " ")
}

// Colorize wraps text in the terminal color for a severity Level. Centralizing
// it here keeps the swarmctl-derived thresholds and the palette in one place;
// internal/ui reuses it so list and detail views read identically.
func Colorize(text string, lvl format.Level) string {
	return paletteStyle(lvl).Render(text)
}

func paletteStyle(lvl format.Level) lipgloss.Style {
	switch lvl {
	case format.LevelGood:
		return lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"})
	case format.LevelWarn:
		return lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	case format.LevelBad:
		return lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"})
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})
	}
}

// NewRobotTable builds a focused, styled Bubbles table for the robot list. The
// UI feeds it rows via table.Model.SetRows(RobotRows(...)) and reads the
// selection via Cursor(); navigation is the table's built-in key handling.
func NewRobotTable() table.Model {
	t := table.New(
		table.WithColumns(RobotColumns()),
		table.WithFocused(true),
	)
	s := table.DefaultStyles()
	s.Header = s.Header.Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})
	s.Selected = s.Selected.Reverse(true).Bold(false)
	t.SetStyles(s)
	return t
}
