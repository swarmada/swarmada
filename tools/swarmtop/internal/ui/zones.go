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

// The zones screen. FleetZone is where a fleet's SAFETY state lives — an estop applies to a zone
// tree, and the boundary-breach trigger is a zone's edge node — so this screen leads with estop and
// degraded feeds rather than with the hierarchy. A zone list that showed only names and robot
// counts would bury the two facts an operator must not miss.

func (m Model) viewZones() string {
	var b strings.Builder
	left := m.styles.header.Render("swarmtop · zones")
	right := m.styles.muted.Render(fmt.Sprintf("%d zones  %s", len(m.fleet.Zones), m.liveState()))
	b.WriteString(left + "   " + right)
	b.WriteByte('\n')

	if len(m.fleet.Zones) == 0 {
		b.WriteString(m.styles.muted.Render("  no zones"))
		b.WriteByte('\n')
		b.WriteString(m.styles.help.Render("[r] robots  [t] actions  [a] adapters  [?] keys  [q] quit"))
		return b.String()
	}

	b.WriteString(m.styles.colHeader.Render(
		pad("NAME", 20) + pad("ESTOP", 12) + pad("ROBOTS", 8) + pad("OCCUPANCY", 11) +
			pad("KIND", 8) + "PARENT"))
	b.WriteByte('\n')

	for i, z := range m.fleet.Zones {
		row := m.zoneRow(z)
		if i == m.zoneCursor {
			row = m.styles.selected.Render(stripANSI(row))
		}
		b.WriteString(row)
		if i < len(m.fleet.Zones)-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	b.WriteString(m.styles.help.Render("[↑↓] move  [s] split  [enter] detail  [r] robots  [t] actions  [a] adapters  [/] filter  [?] keys"))
	return b.String()
}

func (m Model) viewZoneSplit() string {
	return m.splitScreen("zones · split", m.narrowZoneList(),
		m.zoneDetailLines(m.selectedZone()),
		"[↑↓] move  [PgUp/PgDn] scroll detail  [s] unsplit  [enter] full  [r] robots  [a] adapters  [esc] back  [q] quit")
}

func (m Model) viewZoneDetail() string {
	z := m.selectedZone()
	name := "—"
	if z != nil {
		name = z.Name
	}
	return m.scrollScreen("zones › "+name, m.zoneDetailLines(z),
		"[↑↓/PgUp/PgDn/g/G] scroll  [esc] back  [q] quit")
}

func (m Model) zoneRow(z k8sclient.ZoneView) string {
	return pad(z.Name, 20) +
		m.styles.level(pad(z.EstopStatus, 12), zoneEstopLevel(z.EstopStatus)) +
		pad(fmt.Sprintf("%d", z.RobotCount), 8) +
		pad(zoneOccupancy(z), 11) +
		pad(zoneKind(z), 8) +
		format.Dash(z.ParentZone)
}

func (m Model) narrowZoneList() string {
	if len(m.fleet.Zones) == 0 {
		return m.styles.muted.Render("  no zones")
	}
	var b strings.Builder
	b.WriteString(m.styles.colHeader.Render(pad("NAME", 20) + pad("ESTOP", 12) + "ROBOTS"))
	b.WriteByte('\n')
	for i, z := range m.fleet.Zones {
		row := pad(z.Name, 20) +
			m.styles.level(pad(z.EstopStatus, 12), zoneEstopLevel(z.EstopStatus)) +
			fmt.Sprintf("%d", z.RobotCount)
		if i == m.zoneCursor {
			row = m.styles.selected.Render(stripANSI(row))
		}
		b.WriteString(row)
		if i < len(m.fleet.Zones)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (m Model) zoneDetailLines(z *k8sclient.ZoneView) []string {
	if z == nil {
		return []string{m.styles.muted.Render("no zone selected")}
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	add(m.styles.header.Render(z.Name))
	if z.DisplayName != "" {
		add(m.styles.muted.Render(z.DisplayName))
	}
	add("Estop       " + m.styles.level(z.EstopStatus, zoneEstopLevel(z.EstopStatus)))
	// A zero LastEstopAt is the epoch, not a real event — showing it would date every quiet zone
	// to 1970. "never" is the honest reading of an absent timestamp.
	if z.LastEstopUnknown {
		add("Last estop  " + m.styles.muted.Render("never"))
	} else {
		ageText, ageLvl := format.TelemetryAge(z.LastEstopAt, m.ageRef(), false, staleTelemetryAfter)
		add("Last estop  " + m.styles.level(ageText, ageLvl))
	}
	add("")
	add("Robots      " + fmt.Sprintf("%d", z.RobotCount) + m.styles.muted.Render("  (this zone and every descendant)"))
	add("Occupancy   " + zoneOccupancy(z2v(z)) + m.styles.muted.Render("  confirmed in this zone / cap"))
	add("Kind        " + zoneKind(z2v(z)))
	add("Parent      " + format.Dash(z.ParentZone))
	if len(z.ChildZones) > 0 {
		add("Children    " + strings.Join(z.ChildZones, ", "))
	}
	add("Waypoints   " + fmt.Sprintf("%d", z.Waypoints))

	// The safety-degradation case, called out rather than left as a field. An edge node that is
	// not receiving position frames for a robot cannot detect that robot leaving the zone — the
	// boundary guarantee is not being met for it, which is not obvious from a list of names.
	add("")
	if z.HasEdgeNode {
		add("Edge node   " + m.styles.level("declared", format.LevelGood))
	} else {
		add("Edge node   " + m.styles.muted.Render("none — no zone-boundary breach guard"))
	}
	if len(z.EdgeFeedUnavailable) > 0 {
		add(m.styles.level(fmt.Sprintf("⚠ %d robot(s) with NO edge feed — boundary-breach detection is degraded for them:",
			len(z.EdgeFeedUnavailable)), format.LevelWarn))
		for _, r := range z.EdgeFeedUnavailable {
			add("   " + r + "   " + m.styles.muted.Render(m.robotSummary(r)))
		}
	}

	// Which robots are actually here, resolved from the same snapshot — the question a zone screen
	// exists to answer. Reads m.raw so a name filter narrows the LIST without making a zone look
	// emptier than it is.
	add("")
	add(m.styles.colHeader.Render("Robots in this zone"))
	var found int
	for i := range m.raw.Robots {
		r := &m.raw.Robots[i]
		if r.SpecZone != z.Name && r.CurrentZone != z.Name {
			continue
		}
		found++
		add("   " + m.robotSummary(r.Name))
	}
	if found == 0 {
		add(m.styles.muted.Render("   none reported in this zone"))
	}
	return lines
}

// z2v dereferences for the shared row helpers, which take a value because the list path already
// has one.
func z2v(z *k8sclient.ZoneView) k8sclient.ZoneView { return *z }

// zoneOccupancy renders confirmed occupancy against the cap. An unset cap is UNLIMITED, not zero —
// rendering "3/0" would read as over capacity when no cap was ever configured.
func zoneOccupancy(z k8sclient.ZoneView) string {
	if z.MaxConcurrentRobots <= 0 {
		return fmt.Sprintf("%d/∞", z.CurrentConcurrent)
	}
	return fmt.Sprintf("%d/%d", z.CurrentConcurrent, z.MaxConcurrentRobots)
}

// zoneKind reports the structural role. A zone not yet reconciled reports IsLeaf false as a SAFE
// default (robot admission requires a leaf), so "node" here means "leaf not confirmed", which is
// why it is not phrased as a positive claim about having children.
func zoneKind(z k8sclient.ZoneView) string {
	if z.IsLeaf {
		return "leaf"
	}
	if len(z.ChildZones) > 0 {
		return "parent"
	}
	return "node"
}

// zoneEstopLevel colours the estop column. Anything that is not Clear is at least a warning: a
// zone under an emergency stop is the single most important thing this screen can say.
func zoneEstopLevel(s string) format.Level {
	switch s {
	case "", "Clear":
		return format.LevelGood
	case "Clearing":
		return format.LevelWarn
	default: // Triggered, and any future non-clear state
		return format.LevelBad
	}
}
