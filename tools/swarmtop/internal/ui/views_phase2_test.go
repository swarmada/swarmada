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

func fleetWithAdapters() k8sclient.Fleet {
	f := sampleFleet()
	// Bind both sample robots to vda5050-a so the adapter view can show which
	// robots it serves.
	for i := range f.Robots {
		f.Robots[i].AdapterName = "vda5050-a"
	}
	f.Adapters = []k8sclient.AdapterView{
		{Name: "vda5050-a", Phase: "Connected", Conformance: "Passed", ProtocolVersion: "1.0.0",
			ConnectedRobots: 3, LastHeartbeat: f.SnapshotAt.Add(-2 * time.Second)},
		{Name: "mavlink-b", Phase: "Degraded", Conformance: "Pending", ProtocolVersion: "0.9.0",
			ConnectedRobots: 0, HeartbeatUnknown: true},
	}
	return f
}

func TestActionsView(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("t"))
	if m.mode != modeActions {
		t.Fatalf("t should switch to actions view, got %v", m.mode)
	}
	out := plain(m.View())
	for _, want := range []string{"swarmtop · tasks", "KIND", "TASK", "haul-8846", "InProgress", "robot-3", "High", "62%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("actions view missing %q in:\n%s", want, out)
		}
	}
	// esc returns to robots.
	m = step(m, key("esc"))
	if m.mode != modeList {
		t.Fatalf("esc from actions should return to list, got %v", m.mode)
	}
}

func TestAdaptersView(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("a"))
	if m.mode != modeAdapters {
		t.Fatalf("a should switch to adapters view, got %v", m.mode)
	}
	out := plain(m.View())
	for _, want := range []string{
		"swarmtop · adapters", "SERVES", "vda5050-a", "Passed", "mavlink-b", "Degraded",
		"robot-1", "robot-3", // both sample robots are bound to vda5050-a
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("adapters view missing %q in:\n%s", want, out)
		}
	}
	// Unknown heartbeat (and the unserved mavlink-b) render as a dash, not a
	// bogus value.
	if !strings.Contains(out, "—") {
		t.Fatalf("adapters view should dash an unknown heartbeat:\n%s", out)
	}
}

func TestViewToggles(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})

	// 't' from actions toggles back to robots; 'a' from actions switches straight
	// to adapters.
	m = step(m, key("t"))
	m = step(m, key("t"))
	if m.mode != modeList {
		t.Fatalf("t from actions should return to list, got %v", m.mode)
	}
	m = step(m, key("t"))
	m = step(m, key("a"))
	if m.mode != modeAdapters {
		t.Fatalf("a from actions should switch to adapters, got %v", m.mode)
	}
	m = step(m, key("a"))
	if m.mode != modeList {
		t.Fatalf("a from adapters should return to list, got %v", m.mode)
	}
}

func TestActionDetailDrillIn(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("t"))
	m = step(m, key("enter")) // drill into the selected action's detail
	if m.mode != modeActionDetail {
		t.Fatalf("enter from actions should open action detail, got %v", m.mode)
	}
	out := plain(m.View())
	for _, want := range []string{"tasks › ", "Phase", "Assigned robot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("action detail missing %q in:\n%s", want, out)
		}
	}
	// esc returns to the actions LIST, not the split view. This test never pressed [s],
	// so split was never asked for; the old expectation of modeActionSplit here meant
	// drilling into a detail and backing out silently turned the detail pane on.
	m = step(m, key("esc"))
	if m.mode != modeActions {
		t.Fatalf("esc from action detail should return to the actions list, got %v", m.mode)
	}
	m = step(m, key("s")) // list -> split
	if m.mode != modeActionSplit {
		t.Fatalf("s from action list should split, got %v", m.mode)
	}
	m = step(m, key("s")) // split -> list
	if m.mode != modeActions {
		t.Fatalf("s from action split should return to action list, got %v", m.mode)
	}
}

func TestAdapterDetailDrillIn(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("a"))
	m = step(m, key("enter")) // drill into the selected adapter's detail
	if m.mode != modeAdapterDetail {
		t.Fatalf("enter from adapters should open adapter detail, got %v", m.mode)
	}
	out := plain(m.View())
	for _, want := range []string{"adapters › ", "Conformance", "Served robots"} {
		if !strings.Contains(out, want) {
			t.Fatalf("adapter detail missing %q in:\n%s", want, out)
		}
	}
	// As above: no [s] was pressed, so esc must not leave the detail pane open.
	m = step(m, key("esc"))
	if m.mode != modeAdapters {
		t.Fatalf("esc from adapter detail should return to the adapters list, got %v", m.mode)
	}
}

// TestSplitPreferenceIsAppWide pins the behaviour that split is an OPERATOR PREFERENCE, not a
// property of one screen. It used to be encoded in the mode enum alone, so every [t]/[a]
// transition had to name a concrete mode and each named the unsplit one — setting split on
// robots and pressing [t] dropped you into an unsplit action table.
func TestSplitPreferenceIsAppWide(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})

	m = step(m, key("s"))
	if m.mode != modeSplit {
		t.Fatalf("s on robots should split, got %v", m.mode)
	}
	for _, tc := range []struct {
		key  string
		want mode
	}{
		{"t", modeActionSplit},
		{"a", modeAdapterSplit},
		{"r", modeSplit},
	} {
		m = step(m, key(tc.key))
		if m.mode != tc.want {
			t.Fatalf("with split on, %q should land split (%v), got %v", tc.key, tc.want, m.mode)
		}
	}

	// And the preference is genuinely two-way: turning it off must also carry across.
	m = step(m, key("s"))
	if m.mode != modeList {
		t.Fatalf("s should unsplit robots, got %v", m.mode)
	}
	for _, tc := range []struct {
		key  string
		want mode
	}{
		{"t", modeActions},
		{"a", modeAdapters},
		{"r", modeList},
	} {
		m = step(m, key(tc.key))
		if m.mode != tc.want {
			t.Fatalf("with split off, %q should land unsplit (%v), got %v", tc.key, tc.want, m.mode)
		}
	}
}

// TestEscReturnsToRobotsPreservingSplit covers the esc change: it is now consistently
// "back to robots" rather than a second, screen-local way to toggle the detail pane.
func TestEscReturnsToRobotsPreservingSplit(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("s")) // split on
	m = step(m, key("a"))
	if m.mode != modeAdapterSplit {
		t.Fatalf("expected adapter split, got %v", m.mode)
	}
	m = step(m, key("esc"))
	if m.mode != modeSplit {
		t.Fatalf("esc should return to robots WITH split still on, got %v", m.mode)
	}
}

// TestEscGoesBackToThePreviousScreen covers esc as back-navigation between the two screens
// an operator is actually working across — not a fixed jump to robots.
func TestEscGoesBackToThePreviousScreen(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("t")) // robots -> actions
	m = step(m, key("a")) // actions -> adapters
	m = step(m, key("esc"))
	if m.mode != modeActions {
		t.Fatalf("esc from adapters should go back to actions (where we came from), got %v", m.mode)
	}
	m = step(m, key("esc"))
	if m.mode != modeAdapters {
		t.Fatalf("esc again should go back to adapters, got %v", m.mode)
	}
}

// TestScreenKeysWorkFromFullScreenDetail pins that [r]/[t]/[a] mean the same thing everywhere.
// A full-screen detail used to swallow them, making it a dead end you had to esc out of first.
func TestScreenKeysWorkFromFullScreenDetail(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("enter")) // robot full-screen detail
	if m.mode != modeDetail {
		t.Fatalf("expected robot detail, got %v", m.mode)
	}
	m = step(m, key("t"))
	if m.mode != modeActions {
		t.Fatalf("t from a robot detail should open actions, got %v", m.mode)
	}
	m = step(m, key("enter")) // action full-screen detail
	m = step(m, key("a"))
	if m.mode != modeAdapters {
		t.Fatalf("a from an action detail should open adapters, got %v", m.mode)
	}
	m = step(m, key("enter")) // adapter full-screen detail
	m = step(m, key("r"))
	if m.mode != modeList {
		t.Fatalf("r from an adapter detail should open robots, got %v", m.mode)
	}
}

// TestFilterNarrowsListsButNotCrossReferences pins the two halves of [/]: the visible lists
// narrow, and detail cross-references still resolve against the whole fleet. A filtered
// adapter detail that dropped robots would read as robots having left the adapter.
func TestFilterNarrowsListsButNotCrossReferences(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	total := len(m.fleet.Robots)
	if total < 2 {
		t.Skipf("need >1 robot to exercise filtering, got %d", total)
	}
	name := m.fleet.Robots[0].Name

	m = step(m, key("/"))
	if !m.filtering {
		t.Fatal("/ should start capturing filter keystrokes")
	}
	for _, r := range name {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if len(m.fleet.Robots) >= total {
		t.Fatalf("filter %q should narrow %d robots, still %d", name, total, len(m.fleet.Robots))
	}
	if len(m.raw.Robots) != total {
		t.Fatalf("raw fleet must stay whole, got %d want %d", len(m.raw.Robots), total)
	}
	// A robot outside the filter must still resolve by name in a detail pane.
	if s := m.robotSummary(m.raw.Robots[total-1].Name); strings.Contains(s, "unknown") || s == "" {
		t.Fatalf("cross-reference should resolve against the whole fleet, got %q", s)
	}

	// esc abandons the filter — the way out when you have mistyped into an empty list.
	m = step(m, key("esc"))
	if m.filtering || m.filter != "" || len(m.fleet.Robots) != total {
		t.Fatalf("esc should clear the filter, got filtering=%v filter=%q n=%d",
			m.filtering, m.filter, len(m.fleet.Robots))
	}
}

// TestFilterSuspendsCommandKeys covers the trap that makes a naive filter unusable: typing a
// name containing r/t/a/s/q must not fire those commands.
func TestFilterSuspendsCommandKeys(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, key("/"))
	for _, r := range "rats" {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if m.mode != modeList {
		t.Fatalf("typing 'rats' into the filter must not change screen, got %v", m.mode)
	}
	if m.filter != "rats" {
		t.Fatalf("filter should capture the literal text, got %q", m.filter)
	}
}

// TestPauseFreezesTheSnapshot covers [p]: new snapshots are dropped while paused, and the
// title bar stops claiming the view is live.
func TestPauseFreezesTheSnapshot(t *testing.T) {
	m := step(New(k8sclient.NewStaticStore(fleetWithAdapters())), tea.WindowSizeMsg{Width: 120, Height: 40})
	before := len(m.fleet.Robots)
	m = step(m, key("p"))
	if !m.paused {
		t.Fatal("p should pause")
	}
	if out := plain(m.View()); !strings.Contains(out, "PAUSED") {
		t.Fatalf("a paused view must not claim to be live:\n%s", out)
	}
	// A snapshot arriving while paused is dropped.
	mm, _ := m.Update(fleetMsg{f: k8sclient.Fleet{}})
	if got := len(mm.(Model).fleet.Robots); got != before {
		t.Fatalf("paused view adopted a new snapshot: %d -> %d", before, got)
	}
}

// TestZonesScreen covers the zones family end to end: the [z] key, split persistence into it,
// navigation, and drill-in — the same contract the other three screens have.
func TestZonesScreen(t *testing.T) {
	f := fleetWithAdapters()
	f.Zones = []k8sclient.ZoneView{
		{Name: "dock-a", EstopStatus: "Clear", RobotCount: 2, IsLeaf: true, LastEstopUnknown: true},
		{Name: "yard", EstopStatus: "Triggered", RobotCount: 5, ChildZones: []string{"dock-a"},
			MaxConcurrentRobots: 4, CurrentConcurrent: 3, EdgeFeedUnavailable: []string{"amr-1"}, HasEdgeNode: true},
	}
	m := step(New(k8sclient.NewStaticStore(f)), tea.WindowSizeMsg{Width: 140, Height: 40})

	m = step(m, key("z"))
	if m.mode != modeZones {
		t.Fatalf("z should open zones, got %v", m.mode)
	}
	out := plain(m.View())
	for _, want := range []string{"zones", "dock-a", "yard", "Triggered"} {
		if !strings.Contains(out, want) {
			t.Fatalf("zones list missing %q in:\n%s", want, out)
		}
	}
	// An unset capacity is unlimited, not zero — "0/0" would read as a cap of zero. Note this is
	// CONFIRMED OCCUPANCY, not RobotCount: dock-a has 2 robots in its tree and 0 confirmed
	// reservations, which are different numbers and deliberately different columns.
	if !strings.Contains(out, "0/∞") {
		t.Fatalf("unset maxConcurrentRobots should render as unlimited:\n%s", out)
	}

	m = step(m, key("down"))
	m = step(m, key("enter"))
	if m.mode != modeZoneDetail {
		t.Fatalf("enter should open zone detail, got %v", m.mode)
	}
	d := plain(m.View())
	// The safety-degradation case must be stated, not left as a bare field.
	if !strings.Contains(d, "boundary-breach detection is degraded") {
		t.Fatalf("zone detail must call out degraded edge feeds:\n%s", d)
	}

	// Split is app-wide, so it must carry into zones like every other screen.
	m = step(m, key("esc"))
	m = step(m, key("s"))
	if m.mode != modeZoneSplit {
		t.Fatalf("s on zones should split, got %v", m.mode)
	}
	m = step(m, key("r"))
	if m.mode != modeSplit {
		t.Fatalf("r from zone split should land on robots, still split, got %v", m.mode)
	}
	m = step(m, key("z"))
	if m.mode != modeZoneSplit {
		t.Fatalf("z should return to zones, still split, got %v", m.mode)
	}
}

// TestZoneNeverStoppedShowsNever pins that an absent estop timestamp is not rendered as the epoch.
func TestZoneNeverStoppedShowsNever(t *testing.T) {
	f := fleetWithAdapters()
	f.Zones = []k8sclient.ZoneView{{Name: "quiet", EstopStatus: "Clear", LastEstopUnknown: true}}
	m := step(New(k8sclient.NewStaticStore(f)), tea.WindowSizeMsg{Width: 140, Height: 40})
	m = step(m, key("z"))
	m = step(m, key("enter"))
	d := plain(m.View())
	if !strings.Contains(d, "never") {
		t.Fatalf("a zone that was never stopped should say never:\n%s", d)
	}
	if strings.Contains(d, "1970") {
		t.Fatalf("absent timestamp rendered as the epoch:\n%s", d)
	}
}
