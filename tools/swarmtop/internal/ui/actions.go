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

// The full-width action table lives in tasks.go now: spec §3 rule 7 folds
// "swarmtop · actions" into the combined composite screen, and §3b requires a
// single row builder for member and standalone rows alike. What remains here is
// the split pane, the detail pane, and the narrow list — none of which duplicate
// a row renderer.

// viewActionSplit renders the narrowed action list beside a live detail pane for the
// action under the cursor — the FleetAction analogue of the robot split view.
func (m Model) viewActionSplit() string {
	return m.splitScreen("tasks · split", m.narrowActionList(),
		m.compositeDetailLines(),
		"[↑↓] move  [PgUp/PgDn] scroll detail  [s] unsplit  [enter] full  [r] robots  [a] adapters  [esc] back  [q] quit")
}

// viewActionDetail renders the full-screen detail for the selected action.
func (m Model) viewActionDetail() string {
	t := m.selectedAction()
	name := "—"
	if t != nil {
		name = t.Name
	}
	return m.scrollScreen("tasks › "+name, m.actionDetailLines(t),
		"[↑↓/PgUp/PgDn/g/G] scroll  [esc] back  [q] quit")
}

// narrowActionList is the compact left pane for the split view: name, phase, and
// assigned robot. It renders the SAME flattened rows as the full screen, tasks
// included, because both are driven by taskCursor. Members indent under their
// task; the narrow pane has no room for a TASK column, so the indent is the
// hierarchy cue and the detail pane is the authoritative answer (spec §3a).
func (m Model) narrowActionList() string {
	rows := m.taskRows()
	if len(rows) == 0 {
		return m.styles.muted.Render("  no tasks or actions")
	}
	var b strings.Builder
	b.WriteString(m.styles.colHeader.Render(pad("NAME", 16) + pad("PHASE", 12) + "ROBOT"))
	b.WriteByte('\n')
	for i, r := range rows {
		name, phase, robot, lvl := m.narrowCells(r)
		row := pad(name, 16) + m.styles.level(pad(phase, 12), lvl) + robot
		if i == m.taskCursor {
			row = m.styles.selected.Render(stripANSI(row))
		}
		b.WriteString(row)
		if i < len(rows)-1 {
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

	// The width-independent answer to "which task does this belong to", present on
	// EVERY action — member or standalone. This is why the narrow list can drop
	// the TASK column without losing the information (spec §3a).
	if t.OwnerTask != "" {
		add(fmt.Sprintf("Task      %s   (member: %s)", t.OwnerTask,
			strings.TrimPrefix(t.Name, t.OwnerTask+"-")))
	} else {
		add("Task      " + m.styles.muted.Render("—   (standalone)"))
	}

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
