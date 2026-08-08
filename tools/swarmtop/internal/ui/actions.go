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

	"github.com/swarmada/swarmtop/internal/format"
	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// viewActions renders the FleetAction table with a selectable cursor. Read-only:
// the action lifecycle is the control plane's, swarmtop just watches it — but a row
// can be opened into a live detail view ([enter]) or split pane ([s]), the same
// as the robot list.
func (m Model) viewActions() string {
	var b strings.Builder
	left := m.styles.header.Render("swarmtop · actions")
	right := m.styles.muted.Render(fmt.Sprintf("%d actions  live ●", len(m.fleet.Actions)))
	b.WriteString(left + "   " + right)
	b.WriteByte('\n')

	if len(m.fleet.Actions) == 0 {
		b.WriteString(m.styles.muted.Render("  no actions"))
		b.WriteByte('\n')
		b.WriteString(m.styles.help.Render("[esc] robots  [a] adapters  [q] quit"))
		return b.String()
	}

	b.WriteString(m.styles.colHeader.Render(
		pad("NAME", 16) + pad("PHASE", 12) + pad("ROBOT", 12) +
			pad("PRIO", 9) + pad("DEADLINE", 10) + pad("PROG", 6) + "RETRY"))
	b.WriteByte('\n')

	for i, t := range m.fleet.Actions {
		row := m.actionRow(t)
		if i == m.actionCursor {
			row = m.styles.selected.Render(stripANSI(row))
		}
		b.WriteString(row)
		if i < len(m.fleet.Actions)-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	b.WriteString(m.styles.help.Render("[↑↓] move  [s] split  [enter] detail  [r] robots  [a] adapters  [/] filter  [?] keys"))
	return b.String()
}

// viewActionSplit renders the narrowed action list beside a live detail pane for the
// action under the cursor — the FleetAction analogue of the robot split view.
func (m Model) viewActionSplit() string {
	return m.splitScreen("actions · split", m.narrowActionList(),
		m.actionDetailLines(m.selectedAction()),
		"[↑↓] move  [PgUp/PgDn] scroll detail  [s] unsplit  [enter] full  [r] robots  [a] adapters  [esc] back  [q] quit")
}

// viewActionDetail renders the full-screen detail for the selected action.
func (m Model) viewActionDetail() string {
	t := m.selectedAction()
	name := "—"
	if t != nil {
		name = t.Name
	}
	return m.scrollScreen("actions › "+name, m.actionDetailLines(t),
		"[↑↓/PgUp/PgDn/g/G] scroll  [esc] back  [q] quit")
}

// narrowActionList is the compact left pane for the action split view: name, phase,
// and assigned robot, with the cursor row highlighted.
func (m Model) narrowActionList() string {
	if len(m.fleet.Actions) == 0 {
		return m.styles.muted.Render("  no actions")
	}
	var b strings.Builder
	b.WriteString(m.styles.colHeader.Render(pad("NAME", 16) + pad("PHASE", 12) + "ROBOT"))
	b.WriteByte('\n')
	for i, t := range m.fleet.Actions {
		row := pad(t.Name, 16) +
			m.styles.level(pad(format.Dash(t.Phase), 12), format.ActionPhase(t.Phase)) +
			format.Dash(t.AssignedRobot)
		if i == m.actionCursor {
			row = m.styles.selected.Render(stripANSI(row))
		}
		b.WriteString(row)
		if i < len(m.fleet.Actions)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// actionDetailLines is the shared FleetAction detail renderer (split + full-screen):
// the action's lifecycle fields plus a cross-reference to its assigned robot's
// live phase and battery, resolved from the same snapshot.
func (m Model) actionDetailLines(t *k8sclient.FleetActionView) []string {
	if t == nil {
		return []string{m.styles.muted.Render("no action selected")}
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	add(m.styles.header.Render(t.Name))
	add(fmt.Sprintf("Phase     %s   Prio %s",
		m.styles.level(format.Dash(t.Phase), format.ActionPhase(t.Phase)),
		format.Dash(t.Priority)))
	add(fmt.Sprintf("Progress  %d%%", t.ProgressPct))

	retry := fmt.Sprintf("%d", t.RetryCount)
	if t.RetryCount > 0 {
		retry = m.styles.warn.Render(retry)
	}
	add("Retries   " + retry)

	dlText, dlLvl := format.Deadline(t.Deadline, m.ageRef())
	add("Deadline  " + m.styles.level(dlText, dlLvl))

	add(m.styles.colHeader.Render("Assigned robot"))
	if t.AssignedRobot == "" {
		add("  " + m.styles.muted.Render("unassigned"))
	} else {
		add("  " + m.robotSummary(t.AssignedRobot))
	}

	if t.Message != "" {
		add(m.styles.colHeader.Render("Message"))
		add("  " + m.styles.muted.Render(t.Message))
	}
	return lines
}

func (m Model) actionRow(t k8sclient.FleetActionView) string {
	name := pad(t.Name, 16)
	phase := m.styles.level(pad(format.Dash(t.Phase), 12), format.ActionPhase(t.Phase))
	robot := pad(format.Dash(t.AssignedRobot), 12)
	prio := pad(format.Dash(t.Priority), 9)
	dlText, dlLvl := format.Deadline(t.Deadline, m.ageRef())
	deadline := m.styles.level(pad(dlText, 10), dlLvl)
	prog := pad(fmt.Sprintf("%d%%", t.ProgressPct), 6)
	retry := fmt.Sprintf("%d", t.RetryCount)
	if t.RetryCount > 0 {
		retry = m.styles.warn.Render(retry)
	}
	return name + phase + robot + prio + deadline + prog + retry
}
