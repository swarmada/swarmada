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
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swarmada/swarmtop/internal/k8sclient"
)

// These are the two acceptance tests for the composite FleetTask view
// (ITEM-0015, spec §7 criteria 1 and 2). They drive the real screen through the
// key handler and assert on rendered text, rather than calling a row helper
// directly, so they keep testing the operator-visible contract when the
// renderer's internals change.

// equivFleet is the spec §9 reproduction as view types: one standalone
// FleetAction and one single-member FleetTask whose member carries an identical
// FleetActionSpec. Every action-level field is deliberately the same on both —
// only the name and the owning-task label differ.
func equivFleet() k8sclient.Fleet {
	const (
		phase = "InProgress"
		robot = "sim-robot-002"
		prio  = "Normal"
		prog  = int32(40)
	)
	return k8sclient.Fleet{
		SnapshotAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		Robots: []k8sclient.RobotView{
			{Name: robot, Phase: "InProgress", Estop: "Normal", BatteryPercent: b32(72), CurrentZone: "warehouse-a"},
		},
		Tasks: []k8sclient.FleetTaskView{
			{
				Name:             "equiv-task",
				Phase:            "Running",
				ActionSummary:    "0/1 Succeeded",
				DesiredState:     "Running",
				CompletionPolicy: "All",
				FailurePolicy:    "FailFast",
				CurrentMember:    "probe",
				Members: []k8sclient.FleetTaskMemberView{
					{
						Name:            "probe",
						ActionRef:       "equiv-task-probe",
						Phase:           phase,
						AssignedRobot:   robot,
						DependenciesMet: true,
						Attempt:         1,
					},
				},
			},
		},
		// Sorted by name, as the reducer Store hands them out.
		Actions: []k8sclient.FleetActionView{
			{Name: "equiv-standalone", Phase: phase, AssignedRobot: robot, Priority: prio, ProgressPct: prog},
			{Name: "equiv-task-probe", Phase: phase, AssignedRobot: robot, Priority: prio, ProgressPct: prog,
				OwnerTask: "equiv-task"},
		},
	}
}

// rowSignature reduces a rendered action row to the columns acceptance
// criterion 1 governs. It removes the two cells that are ALLOWED to differ —
// NAME, and the TASK cell naming the owner — then collapses whitespace, leaving
// KIND + PHASE + ROBOT + PRIO + PROG + RETRY. Comparing signatures rather than
// raw lines keeps the test about equivalence instead of column arithmetic, so it
// survives the responsive column widths without being rewritten.
func rowSignature(line, name, task string) string {
	s := strings.Replace(line, name, "", 1)
	s = strings.Replace(s, task, "", 1)
	return strings.Join(strings.Fields(s), " ")
}

// linesContaining returns every rendered line holding sub.
func linesContaining(out, sub string) []string {
	var hits []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, sub) {
			hits = append(hits, l)
		}
	}
	return hits
}

// TestEquivalentActionRowsRenderIdentically is acceptance criterion 1: a
// standalone FleetAction and the single member of a FleetTask carrying an
// identical spec must render identical action rows.
//
// NOTE ON THE CRITERION'S WORDING. §7.1 says the rows must be identical "in
// every column except name". Taken literally that is unsatisfiable: §3a requires
// the TASK column to name the owner for a member and show "—" for a standalone,
// so TASK must differ too — and §3a's own mockup shows exactly that. This test
// therefore holds the action-level lifecycle columns identical (KIND, PHASE,
// ROBOT, PRIO, PROG, RETRY) and allows NAME and TASK to differ, which is the
// only coherent reading of the two sections together.
func TestEquivalentActionRowsRenderIdentically(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(equivFleet())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("t"))
	out := plain(m.View())

	// The composite section must exist at all before equivalence means anything.
	if !strings.Contains(out, "FLEETTASKS") {
		t.Fatalf("no FLEETTASKS section — the composite view is not rendering:\n%s", out)
	}
	if !strings.Contains(out, "equiv-task") {
		t.Fatalf("the task itself is not on screen:\n%s", out)
	}

	// Both action rows carry 40%; the task's own row carries its 0/1 summary, so
	// this locates exactly the two action rows without depending on how the
	// member's name is shortened.
	rows := linesContaining(out, "40%")
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 action rows (member + standalone), got %d:\n%s", len(rows), out)
	}

	var standalone, member string
	for _, r := range rows {
		if strings.Contains(r, "equiv-standalone") {
			standalone = r
			continue
		}
		member = r
	}
	if standalone == "" || member == "" {
		t.Fatalf("could not identify both rows: standalone=%q member=%q", standalone, member)
	}

	// Per the §3-over-§3a ruling, a member shows its BARE name, not the child
	// object name — the TASK column and the indent already state the owner.
	if strings.Contains(member, "equiv-task-probe") {
		t.Fatalf("member row should show the bare member name %q, not the child name:\n%s", "probe", member)
	}

	// Both rows must declare KIND=action; neither is a task row.
	for _, r := range []string{standalone, member} {
		if !strings.Contains(r, "action") {
			t.Fatalf("every action row must state KIND=action, missing in:\n%s", r)
		}
	}

	// The standalone's TASK cell is a dash; the member's names its task.
	gotStandalone := rowSignature(standalone, "equiv-standalone", "—")
	gotMember := rowSignature(member, "probe", "equiv-task")
	if gotStandalone != gotMember {
		t.Fatalf("action rows are not equivalent:\n  standalone %q\n  member     %q\n\nfull screen:\n%s",
			gotStandalone, gotMember, out)
	}
}

// TestNoTasksRendersAsTodayList is acceptance criterion 2: with no FleetTask
// present, the t screen must render what it renders today — every existing
// quickstart scenario runs with zero composites.
//
// NOTE ON WHAT "EXACTLY" MEANS HERE. §3 rule 4 words this as "renders exactly
// what the t view renders today", but a byte-for-byte golden cannot be the test:
// the approved responsive column layout changes NAME's width and drops DEADLINE,
// so identical bytes are impossible by construction and a golden would fail on
// the very change it is meant to permit. The invariant that actually matters is
// the one rule 4 gives its reason for — empty sections collapse and no
// information is lost — so that is what this pins: no composite chrome appears,
// and every action still renders with every value it renders today.
func TestNoTasksRendersAsTodayList(t *testing.T) {
	f := equivFleet()
	f.Tasks = nil
	// Drop the member action too: with no task there is nothing owning it.
	f.Actions = f.Actions[:1]
	// Deliberately short values, chosen to survive TODAY's fixed cells: NAME is
	// 16 wide and ROBOT 12, so "equiv-standalone" (16) and "sim-robot-002" (13)
	// both render CUT — "equiv-standalon", "sim-robot-0". That truncation is the
	// defect the responsive layout fixes; asserting the full values here would
	// turn this into a test of the width change rather than of the zero-composite
	// invariant, and it would have to fail today by construction. Short values
	// isolate the variable this test is for, so it holds before and after.
	f.Actions[0].Name = "equiv-solo"
	f.Actions[0].AssignedRobot = "robot-3"

	m := step(New(k8sclient.NewStaticStore(f)), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("t"))
	out := plain(m.View())

	// No composite chrome may appear when there are no composites.
	for _, forbidden := range []string{"FLEETTASKS", "no owning task"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("zero-composite screen must not render %q:\n%s", forbidden, out)
		}
	}

	// The standalone action still renders, with every value intact.
	row := linesContaining(out, "equiv-solo")
	if len(row) != 1 {
		t.Fatalf("expected exactly 1 row for the standalone action, got %d:\n%s", len(row), out)
	}
	for _, want := range []string{"InProgress", "robot-3", "Normal", "40%", "0"} {
		if !strings.Contains(row[0], want) {
			t.Fatalf("standalone row lost %q:\n%s", want, row[0])
		}
	}

	// Exactly as many action rows as actions — no phantom rows, no dropped ones.
	if got := len(linesContaining(out, "40%")); got != len(f.Actions) {
		t.Fatalf("expected %d action rows, got %d:\n%s", len(f.Actions), got, out)
	}
}
