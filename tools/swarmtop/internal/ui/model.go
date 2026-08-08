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

// Package ui is the Bubble Tea front end. In the snapshot model (option C) the
// UI holds no incremental fleet state of its own: it re-reads a whole
// consistent Fleet from the Store on every Changed() nudge and on a slow
// wall-clock tick (so relative "age" columns keep advancing), then rebuilds the
// robot-list table from the components view-model and renders it. All cluster
// access is behind k8sclient.Store, so the whole model is exercisable in tests
// by driving a StaticStore — no cluster, no program loop.
package ui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/swarmada/swarmtop/internal/k8sclient"
	"github.com/swarmada/swarmtop/internal/ui/components"
)

// mode is the current robot-level layout.
type mode int

const (
	// modeList is the full-width robot table.
	modeList mode = iota
	// modeSplit is the narrowed table + live detail pane for the cursor row.
	modeSplit
	// modeDetail is the full-screen detail for the selected robot.
	modeDetail
	// modeActions is the FleetAction table.
	modeActions
	// modeActionSplit is the narrowed action table + detail pane for the cursor action.
	modeActionSplit
	// modeActionDetail is the full-screen detail for the selected action.
	modeActionDetail
	// modeAdapters is the Fleet Adapter health table.
	modeAdapters
	// modeAdapterSplit is the narrowed adapter table + detail pane for the cursor adapter.
	modeAdapterSplit
	// modeAdapterDetail is the full-screen detail for the selected adapter.
	modeAdapterDetail
	// modeZones is the FleetZone table; Split/Detail mirror the other families.
	modeZones
	modeZoneSplit
	modeZoneDetail
)

// inActions / inAdapters report whether the current mode belongs to that section's
// list/split/detail family — used to toggle [t]/[a] back to the robot list.
func (m Model) inActions() bool {
	return m.mode == modeActions || m.mode == modeActionSplit || m.mode == modeActionDetail
}

func (m Model) inAdapters() bool {
	return m.mode == modeAdapters || m.mode == modeAdapterSplit || m.mode == modeAdapterDetail
}

func (m Model) inZones() bool {
	return m.mode == modeZones || m.mode == modeZoneSplit || m.mode == modeZoneDetail
}

// screen is which of the three tables is on show, independent of whether the detail
// pane is open beside it.
//
// The mode constants above conflate those two questions: modeActions and modeActionSplit
// are the same screen at different widths. That is why split could not survive a screen
// change — every [t]/[a] transition had to name a concrete mode, and naming one always
// meant choosing a split-ness, so each of them hard-coded the unsplit variant and quietly
// discarded the operator's preference. Splitting the two ideas apart is the fix: a screen
// says WHERE you are, Model.split says HOW you want it drawn, and modeFor recombines them.
type screen int

const (
	screenRobots screen = iota
	screenActions
	screenAdapters
	screenZones
)

// screen reports which table the current mode belongs to.
func (m Model) screen() screen {
	switch {
	case m.inActions():
		return screenActions
	case m.inAdapters():
		return screenAdapters
	case m.inZones():
		return screenZones
	default:
		return screenRobots
	}
}

// modeFor is the ONLY place a (screen, split) pair becomes a mode. Every navigation key
// routes through it, so a new screen or a new transition cannot reintroduce the bug by
// forgetting to consult m.split.
func modeFor(s screen, split bool) mode {
	switch s {
	case screenActions:
		if split {
			return modeActionSplit
		}
		return modeActions
	case screenAdapters:
		if split {
			return modeAdapterSplit
		}
		return modeAdapters
	case screenZones:
		if split {
			return modeZoneSplit
		}
		return modeZones
	default:
		if split {
			return modeSplit
		}
		return modeList
	}
}

// goTo moves to a screen, preserving the app-wide split preference. splitScroll is reset
// because the detail pane now describes a different object; carrying the old offset over
// would open the pane already scrolled into the middle of unrelated content.
//
// It also records where you came from, so [esc] can go back there. Only a real change is
// recorded: re-entering the screen you are already on must not make esc a no-op that
// strands you, and [t] pressed twice (out to actions and back) should leave prev pointing
// at actions, not at robots.
func (m Model) goTo(s screen) Model {
	if cur := m.screen(); cur != s {
		m.prevScreen = cur
	}
	m.mode, m.splitScroll = modeFor(s, m.split), 0
	return m
}

// tickInterval is the wall-clock cadence for refreshing relative ages.
const tickInterval = time.Second

// Model is the Bubble Tea model. It satisfies tea.Model.
type Model struct {
	store  k8sclient.Store
	styles styles

	// tbl is the Bubbles table backing the robot list; its cursor is the single
	// source of truth for which robot is selected (list, split, and detail all
	// read tbl.Cursor()).
	tbl table.Model

	// actionCursor / adapterCursor are the selected-row indices for the FleetAction
	// and Fleet Adapter tables (index-aligned with fleet.Actions / fleet.Adapters,
	// both sorted by name), mirroring what tbl.Cursor() is for robots.
	actionCursor    int
	adapterCursor int
	zoneCursor    int

	fleet k8sclient.Fleet
	mode  mode

	// split is the operator's app-wide preference for the detail pane, held separately
	// from mode so it survives moving between robots, actions and adapters. [s] sets it;
	// every screen change reads it back through modeFor.
	split bool

	// prevScreen is where [esc] goes back to — the last screen actually left. A single
	// slot rather than a stack: with three screens, "back" that alternates between the
	// two you are working across is what an operator predicts, whereas a deep stack
	// would replay a long trail they no longer remember.
	prevScreen screen

	// paused freezes the fleet snapshot. swarmtop is level-triggered and repaints under
	// you, so a row can move mid-read; [p] holds the picture still. It stops the UI
	// ADOPTING new snapshots — it does not stop the watch, so resuming shows current
	// truth rather than replaying a backlog.
	paused bool

	// showHelp overlays the full key reference. The footer can only carry so many keys
	// before it wraps, and it is already long.
	showHelp bool

	// raw is the UNFILTERED snapshot; fleet is raw with the visible lists narrowed by
	// filter. Keeping both is what lets the filter be applied in ONE place instead of at
	// 46 call sites — and, more importantly, lets DETAIL panes keep resolving against the
	// whole fleet. An adapter's "served robots" list must not shrink because you typed a
	// name filter: that would read as robots having left the adapter.
	raw    k8sclient.Fleet
	filter string

	// filtering is true while [/] is capturing keystrokes. Command keys are suspended for
	// the duration — otherwise typing a robot called "rack-2" would fire [r] and [a].
	filtering bool

	// detailScroll is the top-line offset of the full-screen detail view;
	// splitScroll is the offset of the split view's right (detail) pane.
	detailScroll int
	splitScroll  int

	width, height int
	now           time.Time

	// focusRobot, when set, makes the UI open directly into that robot's detail
	// view once it appears in a snapshot (used by `swarmtop --robot`).
	// focusPending guards that this happens only once.
	focusRobot   string
	focusPending bool
}

// New builds a Model over any Store (the live cache store in main, or a
// StaticStore in tests / offline demo).
func New(store k8sclient.Store) Model {
	m := Model{
		store:  store,
		styles: newStyles(),
		tbl:    components.NewRobotTable(),
		now:    time.Now(),
	}
	m.raw = store.Snapshot()
	m.fleet = m.raw
	m.tbl.SetRows(components.RobotRows(m.fleet, m.now))
	m.applyFocus()
	return m
}

// NewFocused is New, but if robot is non-empty the UI opens directly into that
// robot's detail view as soon as it appears in a snapshot (`swarmtop --robot`).
func NewFocused(store k8sclient.Store, robot string) Model {
	m := New(store)
	if robot != "" {
		m.focusRobot = robot
		m.focusPending = true
		m.applyFocus()
	}
	return m
}

// applyFocus opens the detail view on focusRobot the first time it is present in
// the fleet, then clears the pending flag. No-op when no focus is requested.
func (m *Model) applyFocus() {
	if !m.focusPending || m.focusRobot == "" {
		return
	}
	for i := range m.fleet.Robots {
		if m.fleet.Robots[i].Name == m.focusRobot {
			m.tbl.SetCursor(i)
			m.mode = modeDetail
			m.detailScroll = 0
			m.focusPending = false
			return
		}
	}
}

// --- messages & commands ---------------------------------------------------

type fleetMsg struct{ f k8sclient.Fleet }
type changedMsg struct{}
type tickMsg time.Time

// snapshotCmd reads a fresh snapshot. Cheap (a warm-cache read), so it runs on
// every nudge and at startup.
func snapshotCmd(s k8sclient.Store) tea.Cmd {
	return func() tea.Msg { return fleetMsg{s.Snapshot()} }
}

// waitForChange blocks on the Store's coalesced nudge, then re-arms itself in
// Update: nudge -> snapshot -> re-arm.
func waitForChange(s k8sclient.Store) tea.Cmd {
	return func() tea.Msg {
		<-s.Changed()
		return changedMsg{}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init kicks off the first snapshot, the change-watch loop, and the age tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(snapshotCmd(m.store), waitForChange(m.store), tickCmd())
}

// Update is the state transition. Fleet data is always whatever the last
// snapshot returned; layout/selection is local state (the table + a couple of
// scroll offsets).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.tbl.SetHeight(tableHeight(m.height))
		m.tbl.SetWidth(m.width)
		return m, nil

	case fleetMsg:
		if m.paused {
			// Drop the snapshot, but keep listening — see Model.paused.
			return m, nil
		}
		m.raw = msg.f
		m.applyFilter()
		return m, nil

	case changedMsg:
		return m, tea.Batch(snapshotCmd(m.store), waitForChange(m.store))

	case tickMsg:
		m.now = time.Time(msg)
		m.refreshRows() // advance relative ages
		return m, tickCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// refreshRows rebuilds the table rows from the current fleet and keeps the
// cursor in range after the row set shrinks.
// applyFilter recomputes the visible fleet from raw, clamps the cursors that index into
// it, and rebuilds the table. Every path that changes either the snapshot or the filter
// text goes through here, so the two can never disagree.
func (m *Model) applyFilter() {
	m.fleet = filterFleet(m.raw, m.filter)
	// Cursors index the VISIBLE slices. Narrowing the list can strand a cursor past the
	// end, and the detail pane would then render whatever is at an out-of-range index —
	// or panic. Clamp on every recompute rather than at each use site.
	m.actionCursor = clampIndex(m.actionCursor, len(m.fleet.Actions))
	m.adapterCursor = clampIndex(m.adapterCursor, len(m.fleet.Adapters))
	m.zoneCursor = clampIndex(m.zoneCursor, len(m.fleet.Zones))
	m.refreshRows()
	if c := m.tbl.Cursor(); c >= len(m.fleet.Robots) {
		m.tbl.SetCursor(clampIndex(c, len(m.fleet.Robots)))
	}
}

func clampIndex(i, n int) int {
	if n == 0 || i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// filterFleet narrows the three visible lists to entries whose name contains q, folded to
// lower case. EventsByRobot and every other cross-reference field is carried over intact —
// callers that resolve relationships read Model.raw, not this.
func filterFleet(f k8sclient.Fleet, q string) k8sclient.Fleet {
	if q == "" {
		return f
	}
	q = strings.ToLower(q)
	out := f
	out.Robots = nil
	for _, r := range f.Robots {
		if strings.Contains(strings.ToLower(r.Name), q) {
			out.Robots = append(out.Robots, r)
		}
	}
	out.Actions = nil
	for _, a := range f.Actions {
		if strings.Contains(strings.ToLower(a.Name), q) {
			out.Actions = append(out.Actions, a)
		}
	}
	out.Adapters = nil
	for _, a := range f.Adapters {
		if strings.Contains(strings.ToLower(a.Name), q) {
			out.Adapters = append(out.Adapters, a)
		}
	}
	out.Zones = nil
	for _, z := range f.Zones {
		if strings.Contains(strings.ToLower(z.Name), q) {
			out.Zones = append(out.Zones, z)
		}
	}
	return out
}

func (m *Model) refreshRows() {
	m.tbl.SetRows(components.RobotRows(m.fleet, m.ageRef()))
	if n := len(m.fleet.Robots); m.tbl.Cursor() >= n {
		if n == 0 {
			m.tbl.SetCursor(0)
		} else {
			m.tbl.SetCursor(n - 1)
		}
	}
	m.actionCursor = clampCursor(m.actionCursor, len(m.fleet.Actions))
	m.adapterCursor = clampCursor(m.adapterCursor, len(m.fleet.Adapters))
	m.applyFocus()
}

// clampCursor keeps a list cursor within [0, n-1] (0 when the list is empty),
// so a shrinking Action/Adapter snapshot never leaves the cursor out of range.
func clampCursor(cur, n int) int {
	if n == 0 {
		return 0
	}
	if cur >= n {
		return n - 1
	}
	if cur < 0 {
		return 0
	}
	return cur
}

// tableHeight is how many rows the list table shows given the terminal height,
// leaving room for the header and help lines.
func tableHeight(h int) int {
	const chrome = 4
	if h-chrome < 3 {
		return 3
	}
	return h - chrome
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// In full-screen detail, arrows/paging scroll the body; esc returns to that
	// section's split view.
	// FILTER CAPTURE comes before everything, including the global keys: while typing, a
	// robot named "rack-2" must not fire [r], [a] and [c] as commands. Only esc/enter and
	// editing keys are interpreted here.
	if m.filtering {
		switch msg.String() {
		case "esc":
			// esc ABANDONS the filter — the way out for someone who mistyped and is now
			// looking at an empty list and cannot tell why.
			m.filtering, m.filter = false, ""
			m.applyFilter()
		case "enter":
			// enter KEEPS the filter and hands the keys back, so you can navigate the
			// narrowed list normally.
			m.filtering = false
		case "backspace":
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
				m.applyFilter()
			}
		default:
			if r := msg.Runes; len(r) > 0 {
				m.filter += string(r)
				m.applyFilter()
			}
		}
		return m, nil
	}

	// TRULY GLOBAL keys first, before the detail early-return below. Pause and help are
	// properties of the whole session, not of one view, and a [p] that silently stopped
	// working the moment you drilled into a detail would be worse than not having it.
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "p":
		m.paused = !m.paused
		return m, nil
	case "?":
		m.showHelp = !m.showHelp
		return m, nil
	case "/":
		m.filtering = true
		return m, nil
	}
	// Any key dismisses the help overlay, so it can never trap you.
	if m.showHelp {
		m.showHelp = false
		return m, nil
	}

	// esc returns to the screen you came from, at the CURRENT split preference. These
	// used to hard-code the split variant, which meant leaving a detail view silently
	// switched split on for an operator who had never asked for it.
	switch m.mode {
	case modeDetail:
		return m.handleDetailKey(msg, m.robotDetailLines(m.selectedRobot(), m.width), modeFor(screenRobots, m.split))
	case modeActionDetail:
		return m.handleDetailKey(msg, m.actionDetailLines(m.selectedAction()), modeFor(screenActions, m.split))
	case modeAdapterDetail:
		return m.handleDetailKey(msg, m.adapterDetailLines(m.selectedAdapter()), modeFor(screenAdapters, m.split))
	case modeZoneDetail:
		return m.handleDetailKey(msg, m.zoneDetailLines(m.selectedZone()), modeFor(screenZones, m.split))
	}

	// Command keys, handled the same in every list/split view.
	switch msg.String() {
	case "s":
		// Toggles the preference, not just this screen's layout — so it still does the
		// obvious thing here and also carries to wherever you go next.
		m.split = !m.split
		m.mode, m.splitScroll = modeFor(m.screen(), m.split), 0
		return m, nil
	case "r":
		return m.goTo(screenRobots), nil
	case "t":
		if m.inActions() {
			return m.goTo(screenRobots), nil
		}
		return m.goTo(screenActions), nil
	case "a":
		if m.inAdapters() {
			return m.goTo(screenRobots), nil
		}
		return m.goTo(screenAdapters), nil
	case "z":
		if m.inZones() {
			return m.goTo(screenRobots), nil
		}
		return m.goTo(screenZones), nil
	case "enter":
		switch m.mode {
		case modeList, modeSplit:
			if len(m.fleet.Robots) > 0 {
				m.mode, m.detailScroll = modeDetail, 0
			}
		case modeActions, modeActionSplit:
			if len(m.fleet.Actions) > 0 {
				m.mode, m.detailScroll = modeActionDetail, 0
			}
		case modeAdapters, modeAdapterSplit:
			if len(m.fleet.Adapters) > 0 {
				m.mode, m.detailScroll = modeAdapterDetail, 0
			}
		case modeZones, modeZoneSplit:
			if len(m.fleet.Zones) > 0 {
				m.mode, m.detailScroll = modeZoneDetail, 0
			}
		}
		return m, nil
	case "esc":
		// Back to the previous screen, at whatever split-ness is set. It used to mean
		// "un-split this screen" first and only leave on a second press, but that made esc
		// a second, screen-local way to change split — exactly the per-screen behaviour
		// being removed here. [s] owns split; esc owns going back.
		return m.goTo(m.prevScreen), nil
	}

	// Navigation.
	switch m.mode {
	case modeList:
		// The table owns list navigation (up/down/j/k/paging/g/G).
		var cmd tea.Cmd
		m.tbl, cmd = m.tbl.Update(msg)
		return m, cmd

	case modeSplit:
		// Arrows move the list cursor (resetting the detail pane). Paging keys scroll
		// the detail pane WHEN it overflows; when the detail already fits the height
		// there is nothing to scroll, so they page the robot list instead — otherwise
		// the key would be a silent no-op, unlike every other view.
		switch msg.String() {
		case "up", "k", "down", "j":
			before := m.tbl.Cursor()
			m.tbl, _ = m.tbl.Update(msg)
			if m.tbl.Cursor() != before {
				m.splitScroll = 0
			}
		case "pgup", "b", "home", "pgdown", "f", " ", "end":
			if m.splitMaxScroll() > 0 {
				switch msg.String() {
				case "pgup", "b", "home":
					m.splitScroll = clampScroll(m.splitScroll-m.splitPage(msg.String()), m.splitMaxScroll())
				default:
					m.splitScroll = clampScroll(m.splitScroll+m.splitPage(msg.String()), m.splitMaxScroll())
				}
			} else {
				before := m.tbl.Cursor()
				m.tbl, _ = m.tbl.Update(msg)
				if m.tbl.Cursor() != before {
					m.splitScroll = 0
				}
			}
		}
		return m, nil

	case modeActions:
		m.actionCursor = moveCursor(m.actionCursor, len(m.fleet.Actions), msg)
		return m, nil

	case modeActionSplit:
		m.actionCursor, m.splitScroll = m.splitNav(msg, m.actionCursor, len(m.fleet.Actions),
			m.actionDetailLines(m.selectedAction()))
		return m, nil

	case modeZones:
		m.zoneCursor = moveCursor(m.zoneCursor, len(m.fleet.Zones), msg)
		return m, nil

	case modeZoneSplit:
		m.zoneCursor, m.splitScroll = m.splitNav(msg, m.zoneCursor, len(m.fleet.Zones),
			m.zoneDetailLines(m.selectedZone()))
		return m, nil

	case modeAdapters:
		m.adapterCursor = moveCursor(m.adapterCursor, len(m.fleet.Adapters), msg)
		return m, nil

	case modeAdapterSplit:
		m.adapterCursor, m.splitScroll = m.splitNav(msg, m.adapterCursor, len(m.fleet.Adapters),
			m.adapterDetailLines(m.selectedAdapter()))
		return m, nil
	}
	return m, nil
}

// moveCursor advances a plain list cursor by one row (arrows/jk), a page
// (pgup/pgdown), or to the ends (home/g, end/G), clamped to [0, n-1].
func moveCursor(cur, n int, msg tea.KeyMsg) int {
	if n == 0 {
		return 0
	}
	switch msg.String() {
	case "up", "k":
		cur--
	case "down", "j":
		cur++
	case "pgup", "b":
		cur -= 10
	case "pgdown", "f", " ":
		cur += 10
	case "home", "g":
		cur = 0
	case "end", "G":
		cur = n - 1
	}
	if cur < 0 {
		cur = 0
	}
	if cur >= n {
		cur = n - 1
	}
	return cur
}

// splitNav is the shared split-view navigation for the int-cursor sections
// (actions, adapters): arrows/jk move the list cursor (resetting the detail pane),
// paging keys scroll the detail pane. Returns the new (cursor, splitScroll).
func (m Model) splitNav(msg tea.KeyMsg, cursor, n int, lines []string) (int, int) {
	scroll := m.splitScroll
	switch msg.String() {
	case "up", "k", "down", "j":
		before := cursor
		cursor = moveCursor(cursor, n, msg)
		if cursor != before {
			scroll = 0
		}
	case "pgup", "b", "home":
		scroll = clampScroll(scroll-m.splitPage(msg.String()), m.splitMax(lines))
	case "pgdown", "f", " ", "end":
		scroll = clampScroll(scroll+m.splitPage(msg.String()), m.splitMax(lines))
	}
	return cursor, scroll
}

// handleDetailKey scrolls a full-screen detail body and handles exit keys. back
// is the mode esc returns to (each section's split view).
func (m Model) handleDetailKey(msg tea.KeyMsg, detailLines []string, back mode) (tea.Model, tea.Cmd) {
	lines := len(detailLines)
	maxOff := maxScroll(lines, m.detailBodyHeight())
	page := m.detailBodyHeight() - 1
	if page < 1 {
		page = 1
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.mode = back
	// The screen keys work from a full-screen detail too, so [r]/[t]/[a] mean the same thing
	// everywhere. Without these a detail view was a dead end you had to esc out of first, which
	// is the one place an operator most often wants to jump straight to the related screen —
	// looking at a robot, then wanting its actions.
	//
	// [s] is deliberately NOT bound here: it sets the split preference, and there is no
	// meaningful "split" of a full-screen detail. esc already returns to whatever split-ness
	// is set, which is the coherent way out.
	case "r":
		return m.goTo(screenRobots), nil
	case "t":
		return m.goTo(screenActions), nil
	case "a":
		return m.goTo(screenAdapters), nil
	case "z":
		return m.goTo(screenZones), nil
	case "up", "k":
		m.detailScroll--
	case "down", "j":
		m.detailScroll++
	case "pgup", "b":
		m.detailScroll -= page
	case "pgdown", " ", "f":
		m.detailScroll += page
	case "home", "g":
		m.detailScroll = 0
	case "end", "G":
		m.detailScroll = maxOff
	}

	if m.detailScroll > maxOff {
		m.detailScroll = maxOff
	}
	if m.detailScroll < 0 {
		m.detailScroll = 0
	}
	return m, nil
}

// splitMaxScroll is the largest valid offset for the robot split view's right pane.
func (m Model) splitMaxScroll() int {
	// Line count is independent of the pane width (widening only changes column
	// widths, not the number of lines), so any width gives the correct max offset.
	return m.splitMax(m.robotDetailLines(m.selectedRobot(), m.width))
}

// splitPage is how far a paging key moves the split pane. home/end jump all the
// way (a large delta clampScroll pins to 0 / max); everything else moves a
// near-full page.
func (m Model) splitPage(key string) int {
	if key == "home" || key == "end" {
		return 1 << 30
	}
	if p := m.detailBodyHeight() - 1; p > 1 {
		return p
	}
	return 1
}

// clampScroll pins v into [0, hi].
func clampScroll(v, hi int) int {
	if v < 0 {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
}

// ageRef is the reference "now" for relative age columns. It uses the ticking
// wall clock so ages advance between snapshots, falling back to the snapshot
// time before the first tick arrives.
func (m Model) ageRef() time.Time {
	if m.now.IsZero() {
		return m.fleet.SnapshotAt
	}
	return m.now
}

// selectedRobot returns the robot under the table cursor, or nil when the fleet
// is empty. Rows are index-aligned with fleet.Robots (both sorted by name).
func (m Model) selectedRobot() *k8sclient.RobotView {
	i := m.tbl.Cursor()
	if i < 0 || i >= len(m.fleet.Robots) {
		return nil
	}
	return &m.fleet.Robots[i]
}

// selectedAction / selectedAdapter return the row under the respective cursor, or
// nil when that list is empty.
func (m Model) selectedAction() *k8sclient.FleetActionView {
	i := m.actionCursor
	if i < 0 || i >= len(m.fleet.Actions) {
		return nil
	}
	return &m.fleet.Actions[i]
}

func (m Model) selectedZone() *k8sclient.ZoneView {
	i := m.zoneCursor
	if i < 0 || i >= len(m.fleet.Zones) {
		return nil
	}
	return &m.fleet.Zones[i]
}

// liveState is the shared "live ●" / "PAUSED ‖" indicator. A frozen view must never keep
// claiming to be live — see Model.paused.
func (m Model) liveState() string {
	if m.paused {
		return "PAUSED ‖"
	}
	return "live ●"
}

func (m Model) selectedAdapter() *k8sclient.AdapterView {
	i := m.adapterCursor
	if i < 0 || i >= len(m.fleet.Adapters) {
		return nil
	}
	return &m.fleet.Adapters[i]
}

// View renders the current mode.
func (m Model) View() string {
	// The overlay replaces the screen rather than drawing over it: composing a floating
	// panel over a table means clipping every line under it, and a half-covered fleet
	// reads as a fleet that has changed.
	if m.showHelp {
		return m.helpOverlay()
	}
	switch m.mode {
	case modeDetail:
		return m.viewDetailScreen()
	case modeSplit:
		return m.viewSplit()
	case modeActions:
		return m.viewActions()
	case modeActionSplit:
		return m.viewActionSplit()
	case modeActionDetail:
		return m.viewActionDetail()
	case modeAdapters:
		return m.viewAdapters()
	case modeZones:
		return m.viewZones()
	case modeZoneSplit:
		return m.viewZoneSplit()
	case modeZoneDetail:
		return m.viewZoneDetail()
	case modeAdapterSplit:
		return m.viewAdapterSplit()
	case modeAdapterDetail:
		return m.viewAdapterDetail()
	default:
		return m.viewList()
	}
}
