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
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/swarmada/swarmtop/internal/format"
	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// viewSplit renders the narrowed table beside a live detail pane for the row
// under the cursor. The same detail renderer feeds the full-screen mode.
func (m Model) viewSplit() string {
	var b strings.Builder
	b.WriteString(m.titleBar("robots · split"))
	b.WriteByte('\n')

	left := m.narrowRobotList()

	// Give the detail pane the width left over after the list, divider (1), and the
	// two pane paddings (2+2), so a wider terminal widens the detail fields instead
	// of wasting the space. Clamp to a sane minimum on a narrow window.
	detailW := m.width - lipgloss.Width(left) - 5
	if detailW < 30 {
		detailW = 30
	}

	// Window the right (detail) pane by splitScroll so a long detail doesn't
	// crowd out the list; the arrows still move the list cursor.
	rightLines := m.robotDetailLines(m.selectedRobot(), detailW)
	h := m.detailBodyHeight()
	start := clampScroll(m.splitScroll, maxScroll(len(rightLines), h))
	end := start + h
	if end > len(rightLines) {
		end = len(rightLines)
	}
	right := strings.Join(rightLines[start:end], "\n")

	leftBox := lipgloss.NewStyle().PaddingRight(2).Render(left)
	divider := m.styles.paneDivet.Render("│")
	rightBox := lipgloss.NewStyle().PaddingLeft(2).Render(right)

	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftBox, divider, rightBox))
	b.WriteByte('\n')

	scrollHint := ""
	if len(rightLines) > h {
		scrollHint = m.styles.muted.Render(fmt.Sprintf("  detail %d-%d/%d", start+1, end, len(rightLines)))
	}
	b.WriteString(m.styles.help.Render("[↑↓] move  [PgUp/PgDn] scroll detail / page list  [s] unsplit  [enter] full  [t] tasks  [a] adapters  [q] quit") + scrollHint)
	return b.String()
}

// viewDetailScreen renders the full-screen detail for the selected robot,
// windowed by the current scroll offset so long content (events, conditions)
// stays navigable in a fixed-height terminal.
func (m Model) viewDetailScreen() string {
	r := m.selectedRobot()
	name := "—"
	if r != nil {
		name = r.Name
	}
	lines := m.robotDetailLines(r, m.width)

	var b strings.Builder
	b.WriteString(m.titleBar("robots › " + name))
	b.WriteByte('\n')

	h := m.detailBodyHeight()
	start := m.detailScroll
	if start > maxScroll(len(lines), h) {
		start = maxScroll(len(lines), h)
	}
	end := start + h
	if end > len(lines) {
		end = len(lines)
	}
	b.WriteString(strings.Join(lines[start:end], "\n"))
	b.WriteByte('\n')

	scrollHint := ""
	if len(lines) > h {
		scrollHint = m.styles.muted.Render(fmt.Sprintf("  (%d-%d/%d)", start+1, end, len(lines)))
	}
	b.WriteString(m.styles.help.Render("[↑↓/PgUp/PgDn/g/G] scroll  [esc] back  [q] quit") + scrollHint)
	return b.String()
}

// detailBodyHeight is how many content lines fit between the title and help
// lines. Falls back to a sane default before the first WindowSizeMsg.
func (m Model) detailBodyHeight() int {
	const chrome = 3 // title + help + spacing
	h := m.height - chrome
	if h < 1 {
		h = 20
	}
	return h
}

// maxScroll is the largest valid top-line offset for total lines in a window
// of height h.
func maxScroll(total, h int) int {
	if total <= h {
		return 0
	}
	return total - h
}

// splitMax is the largest valid detail-pane offset for an arbitrary set of
// detail lines (used by the action/adapter split views).
func (m Model) splitMax(lines []string) int {
	return maxScroll(len(lines), m.detailBodyHeight())
}

// scrollScreen renders a full-screen, vertically-scrolled detail body (title +
// windowed lines + help), shared by the action and adapter detail views. The robot
// detail view (viewDetailScreen) keeps its own copy for its robot-count title bar.
func (m Model) scrollScreen(title string, lines []string, helpText string) string {
	var b strings.Builder
	b.WriteString(m.styles.header.Render("swarmtop · " + title))
	b.WriteByte('\n')

	h := m.detailBodyHeight()
	start := m.detailScroll
	if mx := maxScroll(len(lines), h); start > mx {
		start = mx
	}
	end := start + h
	if end > len(lines) {
		end = len(lines)
	}
	b.WriteString(strings.Join(lines[start:end], "\n"))
	b.WriteByte('\n')

	scrollHint := ""
	if len(lines) > h {
		scrollHint = m.styles.muted.Render(fmt.Sprintf("  (%d-%d/%d)", start+1, end, len(lines)))
	}
	b.WriteString(m.styles.help.Render(helpText) + scrollHint)
	return b.String()
}

// splitScreen renders a narrowed left list beside a windowed detail pane, shared
// by the action and adapter split views (the robot split view keeps its own copy).
func (m Model) splitScreen(title, leftPane string, rightLines []string, helpText string) string {
	var b strings.Builder
	b.WriteString(m.styles.header.Render("swarmtop · " + title))
	b.WriteByte('\n')

	h := m.detailBodyHeight()
	start := clampScroll(m.splitScroll, maxScroll(len(rightLines), h))
	end := start + h
	if end > len(rightLines) {
		end = len(rightLines)
	}
	right := strings.Join(rightLines[start:end], "\n")

	leftBox := lipgloss.NewStyle().PaddingRight(2).Render(leftPane)
	divider := m.styles.paneDivet.Render("│")
	rightBox := lipgloss.NewStyle().PaddingLeft(2).Render(right)
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftBox, divider, rightBox))
	b.WriteByte('\n')

	scrollHint := ""
	if len(rightLines) > h {
		scrollHint = m.styles.muted.Render(fmt.Sprintf("  detail %d-%d/%d", start+1, end, len(rightLines)))
	}
	b.WriteString(m.styles.help.Render(helpText) + scrollHint)
	return b.String()
}

// robotSummary cross-references a robot by name in the current snapshot and
// renders a one-line phase + battery summary — used by the action and adapter
// detail panes to show the robots they touch without a second query.
func (m Model) robotSummary(name string) string {
	// raw, not fleet: this RESOLVES a named cross-reference rather than listing. Under a
	// filter the referenced robot may not be in the visible set, and returning "unknown"
	// for a robot that plainly exists is worse than showing it.
	for i := range m.raw.Robots {
		r := &m.raw.Robots[i]
		if r.Name == name {
			battText, battLvl := format.Battery(r.BatteryPercent)
			return fmt.Sprintf("%s  %s  batt %s", pad(r.Name, 14),
				m.styles.level(pad(r.Phase, 12), format.RobotPhase(r.Phase)),
				m.styles.level(battText, battLvl))
		}
	}
	return name + m.styles.muted.Render("  (not in snapshot)")
}

// robotDetailLines is the shared detail renderer: the full status breakdown for
// one robot, used by both split and full-screen modes.
func (m Model) robotDetailLines(r *k8sclient.RobotView, width int) []string {
	if r == nil {
		return []string{m.styles.muted.Render("no robot selected")}
	}

	// Label columns widen on a wider pane so long capability/hardware/model names
	// and condition types show in full instead of clipping at the compact defaults.
	nameCol, typeCol := detailLabelWidths(width)

	var lines []string
	add := func(s string) { lines = append(lines, s) }

	battText, battLvl := format.Battery(r.BatteryPercent)
	estopText, estopLvl := format.Estop(r.Estop)
	add(m.styles.header.Render(r.Name))
	add(fmt.Sprintf("Phase   %s   Estop %s   Batt %s",
		m.styles.level(r.Phase, format.RobotPhase(r.Phase)),
		m.styles.level(estopText, estopLvl),
		m.styles.level(battText, battLvl)))

	add("Zone    " + m.zoneDetail(r))
	add("Loc     " + m.positionDetail(r))

	// Liveness is the robot's phase (above): the control plane keeps per-frame
	// connectivity off the status write path (RA-1), writing status.phase=Offline
	// only when the offline threshold trips, so there is no live "last telemetry"
	// age to show here. Adapter-level connectivity is in the adapter-health view.
	add("Adapter " + format.Dash(r.AdapterName))

	add(m.styles.colHeader.Render("Capabilities"))
	if len(r.Capabilities) == 0 {
		add("  " + m.styles.muted.Render("—"))
	}
	for _, c := range r.Capabilities {
		line := "  " + pad(c.Name, nameCol) + m.styles.level(c.Status, format.CapabilityStatus(c.Status))
		if c.Reason != "" {
			line += m.styles.muted.Render("  (" + c.Reason + ")")
		}
		add(line)
	}

	add(m.styles.colHeader.Render("Hardware"))
	if len(r.Hardware) == 0 {
		add("  " + m.styles.muted.Render("—"))
	}
	for _, h := range r.Hardware {
		line := "  " + pad(h.Name, nameCol) + m.styles.level(h.Status, format.HardwareStatus(h.Status))
		if h.Reason != "" {
			line += m.styles.muted.Render("  (" + h.Reason + ")")
		}
		add(line)
	}

	add(m.styles.colHeader.Render("Current action"))
	if r.AssignedAction == "" {
		add("  " + m.styles.muted.Render("none"))
	} else {
		add("  " + m.actionDetail(r.AssignedAction))
	}

	// Health & connectivity.
	add(m.styles.colHeader.Render("Health"))
	if r.HealthStatus == "" && r.HealthMessage == "" {
		add("  " + m.styles.muted.Render("—"))
	} else {
		line := "  " + m.styles.level(format.Dash(r.HealthStatus), format.HardwareStatus(r.HealthStatus))
		if r.HealthMessage != "" {
			line += m.styles.muted.Render("  " + r.HealthMessage)
		}
		add(line)
	}
	if r.LatencyMs != nil {
		add("  " + m.styles.muted.Render(fmt.Sprintf("ping %dms", *r.LatencyMs)))
	}

	// Firmware & models.
	if r.FirmwareVersion != "" || r.PreviousFirmwareVersion != "" || len(r.InstalledModels) > 0 || len(r.ModelGrantedCaps) > 0 {
		add(m.styles.colHeader.Render("Firmware & models"))
		if r.FirmwareVersion != "" {
			fw := "  firmware  " + r.FirmwareVersion
			if r.PreviousFirmwareVersion != "" {
				fw += m.styles.muted.Render(" (prev " + r.PreviousFirmwareVersion + ")")
			}
			add(fw)
		}
		for _, mdl := range r.InstalledModels {
			line := "  " + pad(mdl.Name, nameCol) + m.styles.muted.Render(format.Dash(mdl.Status))
			if mdl.RunningVersion != "" {
				line += m.styles.muted.Render("  v" + mdl.RunningVersion)
			}
			if mdl.FailureReason != "" {
				line += m.styles.bad.Render("  " + mdl.FailureReason)
			}
			add(line)
		}
		for _, g := range r.ModelGrantedCaps {
			add("  " + m.styles.muted.Render(fmt.Sprintf("%s grants [%s]", g.ModelName, strings.Join(g.Capabilities, ", "))))
		}
	}

	// Conditions.
	add(m.styles.colHeader.Render("Conditions"))
	if len(r.Conditions) == 0 {
		add("  " + m.styles.muted.Render("—"))
	}
	for _, c := range r.Conditions {
		line := "  " + pad(c.Type, typeCol) + m.styles.level(pad(c.Status, 8), format.ConditionStatus(c.Status))
		if c.Reason != "" {
			line += m.styles.muted.Render(c.Reason)
		}
		add(line)
		if c.Message != "" {
			add("    " + m.styles.muted.Render(c.Message))
		}
	}

	// Recent events (newest first).
	events := m.fleet.EventsByRobot[r.Name]
	add(m.styles.colHeader.Render(fmt.Sprintf("Recent events (%d)", len(events))))
	if len(events) == 0 {
		add("  " + m.styles.muted.Render("none"))
	}
	for _, e := range events {
		age := format.Age(e.Time, m.ageRef(), e.Time.IsZero())
		head := fmt.Sprintf("  %s %s %s",
			m.styles.muted.Render(pad(age, 5)),
			m.styles.level(pad(format.Dash(e.Type), 8), format.EventType(e.Type)),
			e.Reason)
		if e.Count > 1 {
			head += m.styles.muted.Render(fmt.Sprintf(" ×%d", e.Count))
		}
		add(head)
		if e.Message != "" {
			add("    " + m.styles.muted.Render(e.Message))
		}
	}

	return lines
}

// detailLabelWidths sizes the detail pane's label columns to the available width,
// so a wider window reveals more of long capability/hardware/model names and
// condition types instead of clipping them at the compact defaults. Falls back to
// the compact widths on a narrow pane (or before the first WindowSizeMsg).
func detailLabelWidths(width int) (nameCol, typeCol int) {
	switch {
	case width >= 110:
		return 24, 32
	case width >= 80:
		return 18, 26
	default:
		return 12, 20
	}
}

func (m Model) zoneDetail(r *k8sclient.RobotView) string {
	cur := format.Dash(r.CurrentZone)
	if r.ZoneDrift {
		return m.styles.warn.Render(cur) + m.styles.muted.Render(fmt.Sprintf(" (spec=%s, drift)", format.Dash(r.SpecZone)))
	}
	return cur + m.styles.muted.Render(fmt.Sprintf(" (spec=%s, no drift)", format.Dash(r.SpecZone)))
}

func (m Model) positionDetail(r *k8sclient.RobotView) string {
	if !r.HasPosition {
		return m.styles.muted.Render("unreported")
	}
	floor := "?"
	if r.Position.Floor != nil {
		floor = fmt.Sprintf("%d", *r.Position.Floor)
	}
	// Position is a coarse, throttled projection (RA-1); label it as such.
	return fmt.Sprintf("x=%.1f y=%.1f floor=%s %s",
		r.Position.X, r.Position.Y, floor, m.styles.muted.Render("(coarse)"))
}

// actionDetail looks up the assigned action in the same snapshot so the detail pane
// can show its phase/progress without a second query.
func (m Model) actionDetail(name string) string {
	for _, t := range m.raw.Actions { // resolve against the whole fleet — see robotSummary
		if t.Name == name {
			line := fmt.Sprintf("%s  %s  %d%%  prio=%s",
				t.Name, format.Dash(t.Phase), t.ProgressPct, format.Dash(t.Priority))
			// The owning task lives here, not in the narrow list — the detail pane
			// is the width-independent answer to "which task is this part of".
			if t.OwnerTask != "" {
				line += m.styles.muted.Render("  task=" + t.OwnerTask)
			}
			return line
		}
	}
	return name + m.styles.muted.Render("  (action not in snapshot)")
}
