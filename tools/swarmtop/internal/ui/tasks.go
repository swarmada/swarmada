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
	"strconv"
	"strings"

	"github.com/swarmada/swarmtop/internal/format"
	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// The composite screen: FleetTasks with their member actions nested beneath
// them, then the actions that no task owns. Both shapes are visible at once
// because the control plane makes them equivalent below the task level — a
// member and a standalone action are scheduled, leased, and reported the same
// way — so a screen that showed only one of them would misrepresent the fleet.
//
// The equivalence is enforced structurally, not by convention: every action row
// on this screen, member or standalone, is built by actionCells and laid out
// through one shared width vector. There is no second row builder to drift.

// Column indices for the composite list. Order is the render order.
const (
	colKind = iota
	colTaskName
	colTaskPhase
	colTaskRobot
	colTaskOwner
	colTaskPrio
	colTaskProg
	colTaskRetry
	numTaskCols
)

// taskColTitles are the header cells, index-aligned with the col* constants.
var taskColTitles = []string{"KIND", "NAME", "PHASE", "ROBOT", "TASK", "PRIO", "PROG", "RETRY"}

// taskListRow is one rendered line: either a FleetTask's own row or an action
// row. An action row carries no notion of which section it is in — whether it
// nests is decided by its OwnerTask, so the two sections cannot render the same
// action differently.
type taskListRow struct {
	// Exactly one of task / action / pending is set.
	//
	//	task    — a FleetTask's own row
	//	action  — a real FleetAction (member or standalone)
	//	pending — a member DECLARED in the task's status.actions[] whose child
	//	          FleetAction does not exist yet, with owner naming its task
	task    *k8sclient.FleetTaskView
	action  *k8sclient.FleetActionView
	pending *k8sclient.FleetTaskMemberView
	owner   *k8sclient.FleetTaskView

	// note holds the trailing-annotation parts of a task row, most important
	// first. Nil on action rows.
	note []notePart
}

// isTask reports whether this row is a FleetTask's own row.
func (r taskListRow) isTask() bool { return r.task != nil }

// taskListRows flattens the fleet into render order: each task followed by its
// members, then every action no task owns.
//
// Grouping is by the swarmada.io/fleettask label carried on FleetActionView
// .OwnerTask — never by walking ownerReferences. The label is already on the
// object the informer delivered, so this is a single pass with no join.
//
// Member ORDER and member SET come from the task's own status.actions[] (spec
// §4), not from the name-sorted action list. That matters for more than
// ordering: the controller only generates a child once its dependencies are met,
// so a task with a dependency graph has members with no FleetAction yet. Driving
// the list off the label alone would render a three-step task as a single row
// and silently regrow it as each step started — hiding the shape of the task at
// exactly the moment an operator wants to see it. Those members render as
// pending rows instead.
func taskListRows(f k8sclient.Fleet) []taskListRow {
	byOwner := make(map[string][]*k8sclient.FleetActionView, len(f.Tasks))
	known := make(map[string]bool, len(f.Tasks))
	for i := range f.Tasks {
		known[f.Tasks[i].Name] = true
	}
	var standalone []*k8sclient.FleetActionView
	for i := range f.Actions {
		a := &f.Actions[i]
		// An action naming a task we have not seen is NOT dropped: the task may
		// have been deleted, or its informer may not have synced yet. It falls
		// through to the lower section still showing its owner in the TASK cell,
		// so the row is never silently lost and never silently reparented.
		if a.OwnerTask != "" && known[a.OwnerTask] {
			byOwner[a.OwnerTask] = append(byOwner[a.OwnerTask], a)
			continue
		}
		standalone = append(standalone, a)
	}

	rows := make([]taskListRow, 0, len(f.Tasks)+len(f.Actions))
	for i := range f.Tasks {
		t := &f.Tasks[i]
		rows = append(rows, taskListRow{task: t, note: taskNoteParts(t)})

		owned := byOwner[t.Name]
		byName := make(map[string]*k8sclient.FleetActionView, len(owned))
		for _, a := range owned {
			byName[a.Name] = a
		}
		claimed := make(map[string]bool, len(owned))

		for j := range t.Members {
			mem := &t.Members[j]
			if a := byName[mem.ActionRef]; mem.ActionRef != "" && a != nil {
				rows = append(rows, taskListRow{action: a})
				claimed[a.Name] = true
				continue
			}
			rows = append(rows, taskListRow{pending: mem, owner: t})
		}

		// Anything labelled for this task that no member claimed: a compensation
		// child (the controller stamps it with the same owner label) or an object
		// adopted after a status write was lost. Appended rather than dropped —
		// a running action must never be invisible because status disagrees.
		for _, a := range owned {
			if !claimed[a.Name] {
				rows = append(rows, taskListRow{action: a})
			}
		}
	}
	for _, a := range standalone {
		rows = append(rows, taskListRow{action: a})
	}
	return rows
}

// taskNoteParts is the trailing annotation on a task row, in DESCENDING order of
// importance, so a narrow terminal drops the least valuable part first.
//
//  1. the current member — spec §3 rule 3 makes this the headline of a task row
//  2. the desired state — task-owned intent, and the one kubectl print column
//     with no home in the grid
//  3. the action summary — LAST because it is the least additive: the PROG cell
//     already renders the same "N/M" as a percentage
//
// kubectl's print columns are Phase · Actions · Desired · Age; Phase has a
// column of its own here, and this is where Actions and Desired land.
func taskNoteParts(t *k8sclient.FleetTaskView) []notePart {
	parts := make([]notePart, 0, 3)
	if cur := currentMemberOf(t); cur != nil {
		parts = append(parts, notePart{
			full: fmt.Sprintf("→ %s (%s · %s)",
				cur.Name, format.Dash(cur.Phase), format.Dash(cur.AssignedRobot)),
			// The member's NAME alone still satisfies rule 3; its phase and robot
			// are on its own row directly beneath, so the short form loses
			// placement, not information.
			short: "→ " + cur.Name,
		})
	}
	if t.DesiredState != "" {
		parts = append(parts, notePart{full: "desired " + t.DesiredState})
	}
	if t.ActionSummary != "" {
		parts = append(parts, notePart{full: t.ActionSummary})
	}
	return parts
}

// notePart is one annotation fragment and its degraded form. short is empty when
// the fragment cannot be usefully shortened, in which case it is simply dropped.
type notePart struct{ full, short string }

// minNoteRoom is the width the annotation is guaranteed when any task row has
// one. Without a reserve the columns take everything at their natural widths and
// the current member — which spec §3 rule 3 requires on the task row — silently
// vanishes at ordinary terminal widths. Sized for the short form ("→ " plus a
// typical member name), not the full one.
const minNoteRoom = 18

// fitNote joins as many annotation parts as room allows, in order, falling back
// to a part's short form before giving up on it, and stopping at the first that
// fits in neither. Parts are dropped WHOLE — never sliced — because a note cut
// mid-token ("1/3 Succeeded · Running ·") reads as a rendering fault, and a
// half-printed robot name or phase would be actively misleading.
func fitNote(parts []notePart, room int) string {
	out := ""
	for _, p := range parts {
		placed := false
		for _, variant := range []string{p.full, p.short} {
			if variant == "" {
				continue
			}
			candidate := variant
			if out != "" {
				candidate = out + " · " + variant
			}
			if runeLen(candidate) <= room {
				out, placed = candidate, true
				break
			}
		}
		if !placed {
			break
		}
	}
	return out
}

// currentMemberOf resolves the task's CurrentMember name to its member record.
// The selection itself is made in k8sclient (a projection over status.actions[]);
// this only looks it up for display.
func currentMemberOf(t *k8sclient.FleetTaskView) *k8sclient.FleetTaskMemberView {
	if t.CurrentMember == "" {
		return nil
	}
	for i := range t.Members {
		if t.Members[i].Name == t.CurrentMember {
			return &t.Members[i]
		}
	}
	return nil
}

// taskProgress renders a task's PROG cell from its "N/M Succeeded" summary. A
// summary that does not parse yields a dash rather than a made-up 0% — an
// unwritten summary must not read as "no progress".
func taskProgress(summary string) string {
	f := strings.Fields(summary)
	if len(f) == 0 {
		return "—"
	}
	n, m, ok := strings.Cut(f[0], "/")
	if !ok {
		return "—"
	}
	done, err1 := strconv.Atoi(n)
	total, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || total <= 0 {
		return "—"
	}
	return strconv.Itoa(done*100/total) + "%"
}

// memberDisplayName is the NAME cell for a member row: the bare member name,
// indented under its task. The child object is named "<task>-<member>", and the
// TASK column plus the indent already state the owner, so repeating the task
// name in every member's NAME cell spends width on nothing. A child that does
// not carry the expected prefix keeps its full name rather than being mangled.
func memberDisplayName(a *k8sclient.FleetActionView) string {
	return "  " + format.MemberName(a.Name, a.OwnerTask)
}

// actionCells builds the cells of one action row. This is the single row
// renderer the equivalence invariant rests on (spec §3b): a member row and a
// standalone row differ ONLY in what this function derives from the view struct
// — OwnerTask empty means standalone — never in which section called it. There
// is deliberately no parameter saying "render me as a member".
func (m Model) actionCells(a *k8sclient.FleetActionView) []string {
	name := a.Name
	if a.OwnerTask != "" {
		name = memberDisplayName(a)
	}
	retry := strconv.Itoa(int(a.RetryCount))
	if a.RetryCount > 0 {
		retry = m.styles.warn.Render(retry)
	}
	cells := make([]string, numTaskCols)
	cells[colKind] = "action"
	cells[colTaskName] = name
	cells[colTaskPhase] = m.styles.level(format.Dash(a.Phase), format.ActionPhase(a.Phase))
	cells[colTaskRobot] = format.Dash(a.AssignedRobot)
	cells[colTaskOwner] = format.Dash(a.OwnerTask)
	cells[colTaskPrio] = format.Dash(a.Priority)
	cells[colTaskProg] = fmt.Sprintf("%d%%", a.ProgressPct)
	cells[colTaskRetry] = retry
	return cells
}

// taskCells builds the cells of a FleetTask's own row. A task is not assigned to
// a robot, has no priority of its own, and cannot be retried — those cells are
// dashed rather than borrowed from a member, because showing a member's robot on
// the parent row would state something the API does not.
func (m Model) taskCells(t *k8sclient.FleetTaskView) []string {
	cells := make([]string, numTaskCols)
	cells[colKind] = "task"
	cells[colTaskName] = t.Name
	cells[colTaskPhase] = m.styles.level(format.Dash(t.Phase), format.TaskPhase(t.Phase))
	cells[colTaskRobot] = "—"
	cells[colTaskOwner] = "—"
	cells[colTaskPrio] = "—"
	cells[colTaskProg] = taskProgress(t.ActionSummary)
	cells[colTaskRetry] = "—"
	return cells
}

// pendingMemberCells builds the row for a member declared in status.actions[]
// whose child FleetAction does not exist yet — typically one still gated behind
// dependsOn.
//
// This is NOT a second action renderer, and it does not weaken the equivalence
// invariant (spec §3b): there is no FleetAction here to render, so there is no
// standalone counterpart it could differ from. What it must not do is fabricate
// values — a child that does not exist has no priority, no progress and no retry
// count, so those cells are dashed rather than defaulted to "Normal 0% 0", which
// would read as a live action sitting at zero progress.
func (m Model) pendingMemberCells(t *k8sclient.FleetTaskView, mem *k8sclient.FleetTaskMemberView) []string {
	cells := make([]string, numTaskCols)
	cells[colKind] = "action"
	cells[colTaskName] = "  " + mem.Name
	cells[colTaskPhase] = m.styles.level(format.Dash(mem.Phase), format.ActionPhase(mem.Phase))
	cells[colTaskRobot] = format.Dash(mem.AssignedRobot)
	cells[colTaskOwner] = t.Name
	cells[colTaskPrio] = "—"
	cells[colTaskProg] = "—"
	cells[colTaskRetry] = "—"
	return cells
}

// cellsFor renders any row kind through the appropriate builder.
func (m Model) cellsFor(r taskListRow) []string {
	switch {
	case r.isTask():
		return m.taskCells(r.task)
	case r.pending != nil:
		return m.pendingMemberCells(r.owner, r.pending)
	default:
		return m.actionCells(r.action)
	}
}

// isStandaloneAction reports whether this row is an action that no task owns —
// the test for where the lower section begins. A pending member is never
// standalone, and dereferencing r.action for one would panic.
func (r taskListRow) isStandaloneAction() bool {
	return r.action != nil && r.action.OwnerTask == ""
}

// narrowCells returns the three fields the split pane has room for, derived from
// the SAME row model the full list uses. Deriving both from one place is what
// keeps the two panes agreeing about what row N is; the split pane formatting its
// own list is how they would drift.
func (m Model) narrowCells(r taskListRow) (name, phase, robot string, lvl format.Level) {
	switch {
	case r.isTask():
		return r.task.Name, format.Dash(r.task.Phase), "—", format.TaskPhase(r.task.Phase)
	case r.pending != nil:
		return "  " + r.pending.Name, format.Dash(r.pending.Phase),
			format.Dash(r.pending.AssignedRobot), format.ActionPhase(r.pending.Phase)
	default:
		n := r.action.Name
		if r.action.OwnerTask != "" {
			n = memberDisplayName(r.action)
		}
		return n, format.Dash(r.action.Phase),
			format.Dash(r.action.AssignedRobot), format.ActionPhase(r.action.Phase)
	}
}

// taskRows is the flattened composite list for the CURRENT (filtered) fleet.
// Selection indexes this, so every caller must derive it the same way.
func (m Model) taskRows() []taskListRow { return taskListRows(m.fleet) }

// selectedTaskRow returns the row under the composite cursor.
func (m Model) selectedTaskRow() (taskListRow, bool) {
	rows := m.taskRows()
	if m.taskCursor < 0 || m.taskCursor >= len(rows) {
		return taskListRow{}, false
	}
	return rows[m.taskCursor], true
}

// selectedTask returns the FleetTask under the cursor, or nil when the cursor is
// on an action row.
func (m Model) selectedTask() *k8sclient.FleetTaskView {
	r, ok := m.selectedTaskRow()
	if !ok {
		return nil
	}
	return r.task
}

// taskColWidths sizes the composite list to its content and the terminal width,
// the same way robotColWidths sizes the robot table. Every column takes the width
// its widest cell needs (bounded by a cap), so NAME is as wide as the longest
// name present rather than a fixed 16 that cuts ordinary names while the right
// of the screen sits empty. When the terminal cannot fit that, the low-signal
// columns give ground first and NAME is the LAST to be cut.
//
// Surplus width is deliberately NOT poured into NAME. A task row carries a
// trailing annotation (summary · desired · current member) that lives past the
// last column; stretching NAME to fill the line would leave that annotation
// nowhere to go. Sizing to content and leaving the remainder for the annotation
// is what lets both fit — and an over-wide NAME full of padding buys nothing.
//
// DEADLINE is not a column on this screen. Spec §3a names it as the one to drop
// if KIND and TASK make the row too wide, and it is "—" on every sample action
// and already present in the detail pane.
func (m Model) taskColWidths(cells [][]string, reserve int) []int {
	caps := []int{6, 60, 12, 16, 20, 8, 6, 5}
	mins := []int{4, 8, 6, 5, 4, 4, 4, 5}

	w := make([]int, numTaskCols)
	for i, t := range taskColTitles {
		w[i] = len(t)
	}
	for _, c := range cells {
		for i := 0; i < numTaskCols && i < len(c); i++ {
			if l := runeLen(stripANSI(c[i])); l > w[i] {
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
	total := numTaskCols - 1 + reserve // single-space gutters, plus the note's floor
	for _, x := range w {
		total += x
	}
	if total > avail {
		// Shrink the low-signal columns toward their minimums before NAME, so the
		// name is the last thing to be cut rather than the first.
		need := total - avail
		for _, idx := range []int{colTaskPrio, colTaskOwner, colTaskRobot, colTaskPhase, colTaskName} {
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

// noteRoom is the width left for a task row's trailing annotation once the
// columns have taken theirs. Zero means there is no room and the annotation is
// dropped rather than wrapped — a wrapped line would break the table's alignment
// for every row below it.
func noteRoom(widths []int, avail int) int {
	if avail <= 0 {
		return 0
	}
	used := len(widths) - 1
	for _, x := range widths {
		used += x
	}
	room := avail - used - 1 // gutter before the note
	if room < 0 {
		return 0
	}
	return room
}

// viewTasks renders the composite screen: FLEETTASKS with their members nested,
// then the actions no task owns. Read-only — swarmtop watches the task
// lifecycle, it never drives it.
func (m Model) viewTasks() string {
	var b strings.Builder
	b.WriteString(m.titleBarTasks())
	b.WriteByte('\n')

	rows := taskListRows(m.fleet)
	if len(rows) == 0 {
		b.WriteString(m.styles.muted.Render("  no tasks or actions"))
		b.WriteByte('\n')
		b.WriteString(m.styles.help.Render("[esc] robots  [a] adapters  [q] quit"))
		return b.String()
	}

	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = m.cellsFor(r)
	}
	// The columns yield minNoteRoom only when a task row actually has an
	// annotation; an all-standalone screen gives the grid the whole line.
	reserve := 0
	for _, r := range rows {
		if len(r.note) > 0 {
			reserve = minNoteRoom
			break
		}
	}
	widths := m.taskColWidths(cells, reserve)
	room := noteRoom(widths, m.width)

	b.WriteString(m.styles.colHeader.Render(layoutCells(taskColTitles, widths)))
	b.WriteByte('\n')

	// Section headers appear ONLY when the section has content, so a fleet with no
	// composites renders as the flat action list it renders today (spec §3 rule 4).
	hasTasks := len(m.fleet.Tasks) > 0
	if hasTasks {
		b.WriteString(m.styles.colHeader.Render("FLEETTASKS"))
		b.WriteByte('\n')
	}

	standaloneHeaderPending := hasTasks
	for i, r := range rows {
		if standaloneHeaderPending && r.isStandaloneAction() {
			b.WriteString(m.styles.colHeader.Render("ACTIONS (no owning task)"))
			b.WriteByte('\n')
			standaloneHeaderPending = false
		}
		line := layoutCells(cells[i], widths)
		if note := fitNote(r.note, room); note != "" {
			line += " " + m.styles.muted.Render(note)
		}
		if i == m.taskCursor {
			line = m.styles.selected.Render(stripANSI(line))
		}
		b.WriteString(line)
		if i < len(rows)-1 {
			b.WriteByte('\n')
		}
	}

	b.WriteByte('\n')
	b.WriteString(m.styles.help.Render(
		"[↑↓] move  [s] split  [enter] detail  [r] robots  [a] adapters  [z] zones  [/] filter  [?] keys"))
	return b.String()
}

// titleBarTasks is the composite screen's header. Spec §3 rule 7 folds the old
// "swarmtop · actions" title into this screen; the count states both shapes so
// the header never implies the fleet holds only one of them.
func (m Model) titleBarTasks() string {
	left := m.styles.header.Render("swarmtop · tasks")
	state := "live ●"
	if m.paused {
		state = "PAUSED ‖"
	}
	count := fmt.Sprintf("%s  %s", plural(len(m.fleet.Tasks), "task"), plural(len(m.fleet.Actions), "action"))
	return left + "   " + m.styles.muted.Render(count+"  "+state)
}

// plural renders "1 task" / "2 tasks" — a count line that reads "1 tasks" looks
// like a rendering fault and makes a reader distrust the number beside it.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// runeLen counts display columns as runes, not bytes — the em dash and arrow
// used here are multi-byte, and len() would over-count them exactly as it did
// before pad() was fixed to count runes.
func runeLen(s string) int { return len([]rune(s)) }

// currentMemberMark is the cursor-independent marker for the member a task is
// currently working (spec §3 rule 3). Distinct from the selection highlight, so
// "the row I am on" and "the action the task is running" never look alike.
const currentMemberMark = "▸ "

// compositeDetailLines is the detail body for whatever the composite cursor is
// on: a task, a member with no child yet, or a real action. Both the split pane
// and the full-screen detail read through it, so opening [s] and pressing enter
// can never show two different things about the same row.
func (m Model) compositeDetailLines() []string {
	r, ok := m.selectedTaskRow()
	if !ok {
		return []string{m.styles.muted.Render("nothing selected")}
	}
	switch {
	case r.task != nil:
		return m.taskDetailLines(r.task)
	case r.pending != nil:
		return m.pendingMemberDetailLines(r.owner, r.pending)
	default:
		// Spec §3 rule 6: an action row — member or standalone — opens the EXISTING
		// action detail, unchanged.
		return m.actionDetailLines(r.action)
	}
}

// taskDetailLines is the composite's detail pane (spec §3 rule 5): every member
// recorded in status.actions[] with the current one marked, plus the policies and
// timestamps that have no column on the list.
func (m Model) taskDetailLines(t *k8sclient.FleetTaskView) []string {
	if t == nil {
		return []string{m.styles.muted.Render("no task selected")}
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	add(m.styles.header.Render(t.Name))
	add("Phase       " + m.styles.level(format.Dash(t.Phase), format.TaskPhase(t.Phase)))
	add("Actions     " + format.Dash(t.ActionSummary))
	add("Desired     " + format.Dash(t.DesiredState))
	// Age completes the four facts kubectl's print columns carry — Phase, Actions,
	// Desired, Age (api/v1/fleettask_types.go) — so the pane opens with the same
	// summary a `kubectl get fleettasks` line gives.
	add("Age         " + format.Age(t.CreatedAt, m.ageRef(), false))
	add("Completion  " + format.Dash(t.CompletionPolicy))
	add("Failure     " + format.Dash(t.FailurePolicy))

	// An absent timestamp means "not yet", never the epoch — rendering a zero time
	// would date every unstarted task to 1970.
	if t.StartedAtUnknown {
		add("Started     " + m.styles.muted.Render("not started"))
	} else {
		add("Started     " + format.Age(t.StartedAt, m.ageRef(), false) + m.styles.muted.Render(" ago"))
	}
	if t.CompletionTimeUnknown {
		add("Completed   " + m.styles.muted.Render("—"))
	} else {
		add("Completed   " + format.Age(t.CompletionTime, m.ageRef(), false) + m.styles.muted.Render(" ago"))
	}

	add("")
	header := fmt.Sprintf("Members (%d)", len(t.Members))
	if t.CurrentMember != "" {
		header += "   " + currentMemberMark + "= current"
	}
	add(m.styles.colHeader.Render(header))
	if len(t.Members) == 0 {
		add("  " + m.styles.muted.Render("none recorded yet"))
	}
	for i := range t.Members {
		mem := &t.Members[i]
		mark := "  "
		if mem.Name == t.CurrentMember {
			mark = currentMemberMark
		}
		line := mark + pad(mem.Name, 20) +
			m.styles.level(pad(format.Dash(mem.Phase), 12), format.ActionPhase(mem.Phase)) +
			pad(format.Dash(mem.AssignedRobot), 16)
		if n := memberNotes(mem); n != "" {
			line += m.styles.muted.Render(n)
		}
		add(line)
	}

	if len(t.Conditions) > 0 {
		add("")
		add(m.styles.colHeader.Render("Conditions"))
		for _, c := range t.Conditions {
			add("  " + pad(c.Type, 24) +
				m.styles.level(pad(c.Status, 8), format.ConditionStatus(c.Status)) +
				m.styles.muted.Render(c.Reason))
			if c.Message != "" {
				add("    " + m.styles.muted.Render(c.Message))
			}
		}
	}
	return lines
}

// memberNotes is the parenthetical after a member row: the facts that explain why
// it is not running, or that it has been retried. Empty when there is nothing to
// say, so an ordinary member reads clean.
func memberNotes(mem *k8sclient.FleetTaskMemberView) string {
	var notes []string
	if mem.ActionRef == "" {
		notes = append(notes, "no action generated yet")
	}
	if !mem.DependenciesMet {
		notes = append(notes, "waiting on dependencies")
	}
	if mem.Attempt > 1 {
		notes = append(notes, fmt.Sprintf("attempt %d", mem.Attempt))
	}
	if mem.CompensationPhase != "" && mem.CompensationPhase != "None" {
		notes = append(notes, "compensation "+mem.CompensationPhase)
	}
	if len(notes) == 0 {
		return ""
	}
	return "(" + strings.Join(notes, ", ") + ")"
}

// pendingMemberDetailLines is the pane for a member whose child FleetAction does
// not exist yet. It deliberately does NOT reuse the action detail: there is no
// action, and a pane full of dashes under an action heading would suggest one
// exists and is failing to report.
func (m Model) pendingMemberDetailLines(t *k8sclient.FleetTaskView, mem *k8sclient.FleetTaskMemberView) []string {
	if t == nil || mem == nil {
		return []string{m.styles.muted.Render("no member selected")}
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	add(m.styles.header.Render(mem.Name))
	add(m.styles.muted.Render("member of " + t.Name))
	add("")
	add("Phase        " + m.styles.level(format.Dash(mem.Phase), format.ActionPhase(mem.Phase)))
	add("Robot        " + format.Dash(mem.AssignedRobot))
	deps := m.styles.warn.Render("not met")
	if mem.DependenciesMet {
		deps = "met"
	}
	add("Dependencies " + deps)
	if mem.Attempt > 0 {
		add("Attempt      " + strconv.Itoa(int(mem.Attempt)))
	}
	if mem.CompensationPhase != "" && mem.CompensationPhase != "None" {
		add("Compensation " + mem.CompensationPhase)
	}
	add("")
	add(m.styles.muted.Render("No FleetAction has been generated for this member yet. The control"))
	add(m.styles.muted.Render("plane creates a member's action only once its dependencies are met,"))
	add(m.styles.muted.Render("so there is no assignment, no lease and no progress to report."))
	return lines
}

// viewTaskDetail is the full-screen detail for the composite cursor row.
func (m Model) viewTaskDetail() string {
	title := "tasks › "
	if r, ok := m.selectedTaskRow(); ok {
		switch {
		case r.task != nil:
			title += r.task.Name
		case r.pending != nil:
			title += r.owner.Name + " › " + r.pending.Name
		}
	}
	return m.scrollScreen(title, m.compositeDetailLines(),
		"[↑↓/PgUp/PgDn/g/G] scroll  [esc] back  [q] quit")
}
