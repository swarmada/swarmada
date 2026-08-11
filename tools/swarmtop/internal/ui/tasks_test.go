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

// compositeFleet is a three-member sequential task where only the first member
// has been generated — the other two are still gated behind dependsOn, so they
// exist in status.actions[] with no child FleetAction — plus one standalone
// action so both sections are populated.
func compositeFleet() k8sclient.Fleet {
	return k8sclient.Fleet{
		SnapshotAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
		Tasks: []k8sclient.FleetTaskView{{
			Name: "receiving-round-001", Phase: "Running", ActionSummary: "0/3 Succeeded",
			DesiredState: "Running", CompletionPolicy: "All", FailurePolicy: "FailFast",
			CurrentMember:         "approach-dock",
			StartedAt:             time.Date(2026, 8, 10, 11, 58, 0, 0, time.UTC),
			CompletionTimeUnknown: true,
			Members: []k8sclient.FleetTaskMemberView{
				{Name: "approach-dock", ActionRef: "receiving-round-001-approach-dock",
					Phase: "InProgress", AssignedRobot: "sim-robot-002", DependenciesMet: true},
				{Name: "inspect-dock"},
				{Name: "return-to-bay"},
			},
		}},
		Actions: []k8sclient.FleetActionView{
			{Name: "deliver-pallet-001", Phase: "InProgress", AssignedRobot: "sim-robot-003",
				Priority: "High", ProgressPct: 60},
			{Name: "receiving-round-001-approach-dock", Phase: "InProgress",
				AssignedRobot: "sim-robot-002", Priority: "High", ProgressPct: 45,
				OwnerTask: "receiving-round-001"},
		},
	}
}

func compositeScreen(t *testing.T) Model {
	t.Helper()
	m := step(New(k8sclient.NewStaticStore(compositeFleet())), tea.WindowSizeMsg{Width: 130, Height: 40})
	return step(m, key("t"))
}

// TestOwnedActionDoesNotAppearInStandaloneSection is acceptance criterion 3: an
// action bearing the label appears under its task exactly once, and never below.
func TestOwnedActionDoesNotAppearInStandaloneSection(t *testing.T) {
	out := plain(compositeScreen(t).View())

	head := strings.Index(out, "ACTIONS (no owning task)")
	if head < 0 {
		t.Fatalf("standalone section header missing:\n%s", out)
	}
	above, below := out[:head], out[head:]

	// The member is rendered by its bare name; the owning task names it in TASK.
	if strings.Count(above, "approach-dock") == 0 {
		t.Fatalf("member does not appear under its task:\n%s", above)
	}
	if strings.Contains(below, "approach-dock") {
		t.Fatalf("member leaked into the standalone section:\n%s", below)
	}
	if !strings.Contains(below, "deliver-pallet-001") {
		t.Fatalf("standalone action is not in the standalone section:\n%s", below)
	}
	// Exactly once overall — nested, not duplicated.
	if n := strings.Count(out, "approach-dock"); n != 2 {
		// 2 = the member row + the task row's "→ approach-dock" current marker.
		t.Fatalf("member should appear once as a row (plus the task's current-member note), got %d mentions:\n%s", n, out)
	}
}

// TestCurrentMemberIsNamedOnTheTaskRow is acceptance criterion 4 at the UI level:
// the task row names its current member. (The selection itself is pinned in
// k8sclient's TestCurrentMemberSelection.)
func TestCurrentMemberIsNamedOnTheTaskRow(t *testing.T) {
	out := plain(compositeScreen(t).View())
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "task ") {
			if !strings.Contains(l, "→ approach-dock") {
				t.Fatalf("task row does not name its current member:\n%s", l)
			}
			return
		}
	}
	t.Fatalf("no task row found:\n%s", out)
}

// TestTaskDetailListsAllMembers is acceptance criterion 5: enter on a task lists
// every member, with the current one marked — including members whose child
// action does not exist yet.
func TestTaskDetailListsAllMembers(t *testing.T) {
	m := compositeScreen(t)
	if m.selectedTask() == nil {
		t.Fatalf("cursor should start on the task row")
	}
	m = step(m, key("enter"))
	if m.mode != modeTaskDetail {
		t.Fatalf("enter on a task should open task detail, got mode %v", m.mode)
	}
	out := plain(m.View())

	for _, want := range []string{
		"receiving-round-001",
		"approach-dock", "inspect-dock", "return-to-bay", // ALL three members
		"0/3 Succeeded", "Running", "All", "FailFast", // summary + policies
		"Members (3)",
		"no action generated yet", // the ungenerated members say so
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("task detail missing %q:\n%s", want, out)
		}
	}

	// The current member is marked, and only it.
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, currentMemberMark) && strings.Contains(l, "= current") {
			continue // the legend
		}
		if strings.Contains(l, currentMemberMark) && !strings.Contains(l, "approach-dock") {
			t.Fatalf("a member other than the current one is marked:\n%s", l)
		}
	}

	// An unfinished task must not render the epoch as a completion time.
	if strings.Contains(out, "1970") {
		t.Fatalf("absent completionTime rendered as the epoch:\n%s", out)
	}
}

// TestEnterOnActionRowOpensActionDetail is the other half of criterion 5: an
// action row — member or standalone — opens the EXISTING action detail unchanged.
func TestEnterOnActionRowOpensActionDetail(t *testing.T) {
	m := step(compositeScreen(t), key("down")) // onto the generated member
	if m.selectedAction() == nil {
		t.Fatalf("row 1 should be the generated member action")
	}
	m = step(m, key("enter"))
	if m.mode != modeActionDetail {
		t.Fatalf("enter on an action row should open action detail, got mode %v", m.mode)
	}
	out := plain(m.View())
	if !strings.Contains(out, "receiving-round-001-approach-dock") {
		t.Fatalf("action detail is not for the member action:\n%s", out)
	}
	// Spec §3a: every action detail states its owning task.
	if !strings.Contains(out, "Task") || !strings.Contains(out, "member: approach-dock") {
		t.Fatalf("action detail missing the Task line:\n%s", out)
	}
}

// TestPendingMemberOpensItsOwnPane covers the row kind the spec does not name:
// a member with no child action must not open an action pane full of dashes.
func TestPendingMemberOpensItsOwnPane(t *testing.T) {
	m := compositeScreen(t)
	m = step(m, key("down"))
	m = step(m, key("down")) // onto "inspect-dock", not yet generated
	r, ok := m.selectedTaskRow()
	if !ok || r.pending == nil {
		t.Fatalf("row 2 should be a pending member, got %+v", r)
	}
	m = step(m, key("enter"))
	if m.mode != modeTaskDetail {
		t.Fatalf("enter on a pending member should open the member pane, got mode %v", m.mode)
	}
	out := plain(m.View())
	if !strings.Contains(out, "member of receiving-round-001") {
		t.Fatalf("pending-member pane does not name its task:\n%s", out)
	}
	if !strings.Contains(out, "No FleetAction has been generated") {
		t.Fatalf("pending-member pane does not explain the absence:\n%s", out)
	}
}

// TestFilterKeepsParentAndFindsPendingMember pins the filtering rule: a member is
// findable by name even with no child action, and its parent comes with it.
func TestFilterKeepsParentAndFindsPendingMember(t *testing.T) {
	filtered := func(q string) string {
		m := compositeScreen(t)
		m = step(m, key("/"))
		for _, r := range q {
			m = step(m, key(string(r)))
		}
		return plain(m.View())
	}

	// "inspect-dock" has NO FleetAction; an actions-only filter would lose it.
	out := filtered("inspect")
	if !strings.Contains(out, "inspect-dock") {
		t.Fatalf("a pending member must be findable by name:\n%s", out)
	}
	if !strings.Contains(out, "receiving-round-001") {
		t.Fatalf("the parent task must survive with its matching member:\n%s", out)
	}
	if strings.Contains(out, "deliver-pallet-001") {
		t.Fatalf("non-matching standalone action should be filtered out:\n%s", out)
	}

	// Narrowing is uniform: a member that did not match is gone too.
	if strings.Contains(out, "return-to-bay") {
		t.Fatalf("non-matching member should be filtered out:\n%s", out)
	}
}
