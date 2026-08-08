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

// viewAdapters renders the Fleet Adapter health table (Phase 2): one row per
// connected adapter, with conformance, negotiated protocol, robot count, and
// last-heartbeat freshness.
func (m Model) viewAdapters() string {
	var b strings.Builder
	left := m.styles.header.Render("swarmtop · adapters")
	right := m.styles.muted.Render(fmt.Sprintf("%d adapters  live ●", len(m.fleet.Adapters)))
	b.WriteString(left + "   " + right)
	b.WriteByte('\n')

	if len(m.fleet.Adapters) == 0 {
		b.WriteString(m.styles.muted.Render("  no adapters"))
		b.WriteByte('\n')
		b.WriteString(m.styles.help.Render("[esc] robots  [t] actions  [q] quit"))
		return b.String()
	}

	b.WriteString(m.styles.colHeader.Render(
		pad("NAME", 18) + pad("PHASE", 14) + pad("CONFORMANCE", 13) +
			pad("PROTO", 9) + pad("HANDSHAKE", 11) + "SERVES"))
	b.WriteByte('\n')

	for i, a := range m.fleet.Adapters {
		row := m.adapterRow(a)
		if i == m.adapterCursor {
			row = m.styles.selected.Render(stripANSI(row))
		}
		b.WriteString(row)
		if i < len(m.fleet.Adapters)-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	b.WriteString(m.styles.help.Render("[↑↓] move  [s] split  [enter] detail  [r] robots  [t] actions  [/] filter  [?] keys"))
	return b.String()
}

// viewAdapterSplit renders the narrowed adapter list beside a live detail pane
// for the adapter under the cursor — the FleetAdapter analogue of the robot split.
func (m Model) viewAdapterSplit() string {
	return m.splitScreen("adapters · split", m.narrowAdapterList(),
		m.adapterDetailLines(m.selectedAdapter()),
		"[↑↓] move  [PgUp/PgDn] scroll detail  [s] unsplit  [enter] full  [r] robots  [t] actions  [esc] back  [q] quit")
}

// viewAdapterDetail renders the full-screen detail for the selected adapter.
func (m Model) viewAdapterDetail() string {
	a := m.selectedAdapter()
	name := "—"
	if a != nil {
		name = a.Name
	}
	return m.scrollScreen("adapters › "+name, m.adapterDetailLines(a),
		"[↑↓/PgUp/PgDn/g/G] scroll  [esc] back  [q] quit")
}

// narrowAdapterList is the compact left pane for the adapter split view: name,
// phase, and handshake freshness, with the cursor row highlighted.
func (m Model) narrowAdapterList() string {
	if len(m.fleet.Adapters) == 0 {
		return m.styles.muted.Render("  no adapters")
	}
	var b strings.Builder
	b.WriteString(m.styles.colHeader.Render(pad("NAME", 18) + pad("PHASE", 14) + "HANDSHAKE"))
	b.WriteByte('\n')
	for i, a := range m.fleet.Adapters {
		hbText, hbLvl := format.TelemetryAge(a.LastHeartbeat, m.ageRef(), a.HeartbeatUnknown, staleTelemetryAfter)
		row := pad(a.Name, 18) +
			m.styles.level(pad(format.Dash(a.Phase), 14), format.AdapterPhase(a.Phase)) +
			m.styles.level(hbText, hbLvl)
		if i == m.adapterCursor {
			row = m.styles.selected.Render(stripANSI(row))
		}
		b.WriteString(row)
		if i < len(m.fleet.Adapters)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// adapterDetailLines is the shared FleetAdapter detail renderer (split +
// full-screen): the adapter's health fields plus the list of robots it serves,
// each with its live phase and battery, resolved from the same snapshot.
func (m Model) adapterDetailLines(a *k8sclient.AdapterView) []string {
	if a == nil {
		return []string{m.styles.muted.Render("no adapter selected")}
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	add(m.styles.header.Render(a.Name))
	add("Phase       " + m.styles.level(format.Dash(a.Phase), format.AdapterPhase(a.Phase)))
	add("Conformance " + m.styles.level(format.Dash(a.Conformance), format.Conformance(a.Conformance)))
	add("Protocol    " + format.Dash(a.ProtocolVersion))

	hbText, hbLvl := format.TelemetryAge(a.LastHeartbeat, m.ageRef(), a.HeartbeatUnknown, staleTelemetryAfter)
	add("Handshake   " + m.styles.level(hbText, hbLvl))
	add(fmt.Sprintf("Robots      %d connected", a.ConnectedRobots))

	served := m.robotsServedBy(a.Name)
	add(m.styles.colHeader.Render(fmt.Sprintf("Served robots (%d)", len(served))))
	if len(served) == 0 {
		add("  " + m.styles.muted.Render("none"))
	}
	for _, name := range served {
		add("  " + m.robotSummary(name))
	}

	if a.Message != "" {
		add(m.styles.colHeader.Render("Message"))
		add("  " + m.styles.muted.Render(a.Message))
	}
	return lines
}

func (m Model) adapterRow(a k8sclient.AdapterView) string {
	name := pad(a.Name, 18)
	phase := m.styles.level(pad(format.Dash(a.Phase), 14), format.AdapterPhase(a.Phase))
	conf := m.styles.level(pad(format.Dash(a.Conformance), 13), format.Conformance(a.Conformance))
	proto := pad(format.Dash(a.ProtocolVersion), 9)
	hbText, hbLvl := format.TelemetryAge(a.LastHeartbeat, m.ageRef(), a.HeartbeatUnknown, staleTelemetryAfter)
	hb := m.styles.level(pad(hbText, 11), hbLvl)
	serves := servesCell(m.robotsServedBy(a.Name))
	return name + phase + conf + proto + hb + serves
}

// robotsServedBy returns the names of robots whose adapter binding points at
// this adapter, derived from the same snapshot — swarmtop reads, it doesn't ask
// the adapter.
func (m Model) robotsServedBy(adapter string) []string {
	var names []string
	// raw: an adapter's served-robot list is a fact about the adapter, not about what the
	// operator has filtered to. Narrowing it would read as robots having left the adapter.
	for i := range m.raw.Robots {
		if m.raw.Robots[i].AdapterName == adapter {
			names = append(names, m.raw.Robots[i].Name)
		}
	}
	return names
}

// servesCell renders up to a few served-robot names, summarizing the rest as
// "+N" so a busy adapter's row stays one line.
func servesCell(names []string) string {
	if len(names) == 0 {
		return format.Dash("")
	}
	const maxShown = 3
	if len(names) <= maxShown {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxShown], ", ") + fmt.Sprintf(" +%d", len(names)-maxShown)
}
