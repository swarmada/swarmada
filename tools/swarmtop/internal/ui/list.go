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

package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/swarmada/swarmtop/internal/format"
	"github.com/swarmada/swarmtop/internal/ui/components"
)

// ansiSeq matches SGR color escapes, used to strip color from the selected row
// before applying the reverse-video highlight (nested color resets would
// otherwise break the highlight mid-row).
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// staleTelemetryAfter is the adapter-health view's last-handshake staleness
// threshold (FleetAdapter.status.lastHeartbeat). Display-only.
const staleTelemetryAfter = components.StaleTelemetryAfter

// viewList renders the full-width robot list. Selection and navigation come
// from the Bubbles table.Model (m.tbl.Cursor()), but the rows are rendered here
// with lipgloss so per-cell color survives — Bubbles v0.20's table.View
// corrupts embedded ANSI (see components.RobotColumnLayout).
func (m Model) viewList() string {
	var b strings.Builder
	b.WriteString(m.titleBar("robots"))
	b.WriteByte('\n')
	if len(m.fleet.Robots) == 0 {
		b.WriteString(m.styles.muted.Render("  no robots — is the control plane reachable?"))
	} else {
		b.WriteString(m.renderRobotTable())
	}
	b.WriteByte('\n')
	b.WriteString(m.styles.help.Render("[↑↓] move  [s] split  [enter] detail  [t] actions  [a] adapters  [z] zones  [/] filter  [?] keys"))
	return b.String()
}

// renderRobotTable lays out the colored robot rows in aligned columns, with the
// table's current row reverse-highlighted.
func (m Model) renderRobotTable() string {
	titles, _ := components.RobotColumnLayout()
	rows := components.RobotRows(m.fleet, m.ageRef())
	// Size columns to their content and the terminal width: tight columns for
	// CAPS/ADAPTER/EVT/etc., leftover width to TASK, and graceful shrink when narrow.
	widths := m.robotColWidths(titles, rows)
	sel := m.tbl.Cursor()

	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, m.styles.colHeader.Render(layoutCells(titles, widths)))

	for i, r := range rows {
		if i == sel {
			plain := make([]string, len(r))
			for j := range r {
				plain[j] = stripANSI(r[j])
			}
			lines = append(lines, m.styles.selected.Render(layoutCells(plain, widths)))
			continue
		}
		lines = append(lines, layoutCells(r, widths))
	}
	return strings.Join(lines, "\n")
}

// robotColWidths sizes the robot-list columns to their content and the available
// terminal width. Each column starts at the width of its widest cell (header
// included), bounded by a per-column cap. Any leftover width is handed to the TASK
// column (rightmost, the most useful free-text field) so the table fills the screen;
// when the terminal is too narrow for the natural layout, the low-signal columns
// (CAPS, ADAPTER, NAME, ZONE) shrink toward their minimums first, so TASK stays
// readable instead of being cut off the right edge. Order matches RobotColumns.
func (m Model) robotColWidths(titles []string, rows []table.Row) []int {
	// caps/mins are indexed by column (NAME, PHASE, BATT, ZONE, CAPS, ADAPTER, EVT,
	// TASK). If the schema ever changes shape, fall back to the fixed layout.
	caps := []int{20, 11, 6, 10, 22, 20, 9, 48}
	mins := []int{9, 6, 4, 3, 8, 6, 6, 10}
	if len(titles) != len(caps) {
		_, base := components.RobotColumnLayout()
		return base
	}

	// Natural width: the widest plain (ANSI-stripped) cell per column, header incl.
	w := make([]int, len(titles))
	for i, t := range titles {
		w[i] = len(t)
	}
	for _, r := range rows {
		for i := 0; i < len(w) && i < len(r); i++ {
			if l := len(stripANSI(r[i])); l > w[i] {
				w[i] = l
			}
		}
	}
	for i := range w {
		if w[i] > caps[i] {
			w[i] = caps[i]
		}
		if w[i] < mins[i] {
			w[i] = mins[i]
		}
	}

	avail := m.width
	if avail <= 0 {
		return w
	}
	total := len(w) - 1 // single-space gutters
	for _, x := range w {
		total += x
	}
	switch {
	case total < avail:
		// Surplus → TASK (last column) absorbs it, filling the row to the edge.
		w[components.ColAction] += avail - total
	case total > avail:
		// Deficit → shrink the low-signal columns toward their mins before TASK.
		need := total - avail
		for _, idx := range []int{components.ColCaps, components.ColAdapter, components.ColName, components.ColZone} {
			give := w[idx] - mins[idx]
			if give <= 0 {
				continue
			}
			if give > need {
				give = need
			}
			w[idx] -= give
			need -= give
			if need == 0 {
				break
			}
		}
	}
	return w
}

// layoutCells pads/truncates each cell to its column width (ANSI-aware) and
// joins them with a single-space gutter.
func layoutCells(cells []string, widths []int) string {
	var b strings.Builder
	for j, cell := range cells {
		w := widths[j]
		b.WriteString(lipgloss.NewStyle().Width(w).MaxWidth(w).Inline(true).Render(cell))
		if j < len(cells)-1 {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// narrowRobotList renders the compact left pane for the split view: name,
// phase, and battery, with the table's current selection highlighted. The full
// list uses the Bubbles table (viewList); this pane stays hand-rendered so it
// can sit tight beside the detail pane.
func (m Model) narrowRobotList() string {
	if len(m.fleet.Robots) == 0 {
		return m.styles.muted.Render("  no robots")
	}
	nameW := m.narrowNameWidth()
	var b strings.Builder
	b.WriteString(m.styles.colHeader.Render(pad("NAME", nameW) + pad("PHASE", 11) + "BATT"))
	b.WriteByte('\n')

	sel := m.tbl.Cursor()
	for i, r := range m.fleet.Robots {
		battText, battLvl := format.Battery(r.BatteryPercent)
		row := pad(r.Name, nameW) + m.styles.level(pad(r.Phase, 11), format.RobotPhase(r.Phase)) +
			m.styles.level(battText, battLvl)
		if i == sel {
			row = m.styles.selected.Render(row)
		}
		b.WriteString(row)
		if i < len(m.fleet.Robots)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// narrowNameWidth sizes the split-view NAME column to the longest robot name in
// the fleet (plus a one-space gutter) so names like "sim-robot-001" aren't clipped,
// bounded so a single outlier name can't crowd out the detail pane. Floors at the
// header width ("NAME").
func (m Model) narrowNameWidth() int {
	w := len("NAME")
	for _, r := range m.fleet.Robots {
		if len(r.Name) > w {
			w = len(r.Name)
		}
	}
	w++ // one-space gutter before PHASE
	const maxNameW = 28
	if w > maxNameW {
		w = maxNameW
	}
	return w
}

// titleBar is the top status line shared by every view.
func (m Model) titleBar(section string) string {
	left := m.styles.header.Render(fmt.Sprintf("swarmtop · %s", section))
	// "live ●" is a claim about the data, so it must stop making that claim while paused —
	// a frozen view that still says live is how someone acts on a stale reading.
	state := "live ●"
	if m.paused {
		state = "PAUSED ‖"
	}
	// A filtered count MUST NOT read as the fleet size — "2 robots" during a filter would
	// say the fleet shrank. Show both the match count and the total, and the query itself,
	// so the number on screen is always explained.
	count := fmt.Sprintf("%d robots", len(m.raw.Robots))
	if m.filter != "" {
		count = fmt.Sprintf("%d/%d robots  /%s", len(m.fleet.Robots), len(m.raw.Robots), m.filter)
		if m.filtering {
			count += "▌" // caret: keystrokes are being captured, command keys are suspended
		}
	}
	right := m.styles.muted.Render(count + "  " + state)
	return left + "   " + right
}

// helpOverlay is the full key reference, shown by [?]. It exists so the footer does not have
// to grow a hint for every key: the footer carries the common ones, this carries all of them.
func (m Model) helpOverlay() string {
	rows := [][2]string{
		{"r / t / a / z", "robots · actions · adapters · zones — from any screen, detail included"},
		{"s", "split: show the detail pane. An app-wide preference, kept across screens"},
		{"enter", "full-screen detail for the selected row"},
		{"esc", "back to the previous screen"},
		{"/", "filter the current list by name; esc or enter leaves the filter"},
		{"p", "pause — freeze the snapshot so rows stop moving while you read"},
		{"↑ ↓ / k j", "move the cursor"},
		{"PgUp PgDn / b f", "page the list, or scroll the detail pane when it overflows"},
		{"g / G", "jump to first / last"},
		{"?", "this help"},
		{"q", "quit"},
	}
	var b strings.Builder
	b.WriteString(m.styles.header.Render("swarmtop · keys") + "\n\n")
	for _, r := range rows {
		b.WriteString("  " + m.styles.colHeader.Render(pad(r[0], 18)) + m.styles.muted.Render(r[1]) + "\n")
	}
	b.WriteString("\n" + m.styles.help.Render("any key closes this"))
	return b.String()
}

// pad right-pads (or truncates) s to width n plain columns.
func pad(s string, n int) string {
	if n == 0 {
		return s
	}
	// RUNES, not bytes. len() counts bytes, so a single "∞" or "—" counted as three and the
	// column came out short by two spaces; worse, the truncation branch sliced by byte offset and
	// could cut a multi-byte rune in half, emitting invalid UTF-8 into the terminal. Every column
	// here is ASCII today, which is why this was invisible — the zones screen is the first to pad
	// a non-ASCII value.
	r := []rune(s)
	if len(r) >= n {
		if n > 1 {
			return string(r[:n-1]) + " "
		}
		return string(r[:n])
	}
	return s + strings.Repeat(" ", n-len(r))
}
