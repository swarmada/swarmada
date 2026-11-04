/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package contract

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// EVERY ADAPTER'S DECLARED CONTRACT VERSION IS CHECKED FROM A SOURCE THAT IS IN THIS TREE.
//
// # Why this exists beside TestInTreeAdaptersReportASupportedContractVersion
//
// That test reads each adapter's own `CONTRACT_VERSION` constant, which is the authoritative
// value — and it can only read the three adapters whose source is in this repository. For
// `fleet-adapter-ros2`, `-vda5050` and `-mavlink` it calls t.Skipf, so the three adapters it
// was written for are the three it cannot check. MEASURED: on the bump that evicted all three,
// that test reported
//
//	--- SKIP: TestInTreeAdaptersReportASupportedContractVersion/ros2
//	--- SKIP: TestInTreeAdaptersReportASupportedContractVersion/vda5050
//	--- SKIP: TestInTreeAdaptersReportASupportedContractVersion/mavlink
//
// A skip is a green test. The eviction was real, arithmetic from the registry, and invisible
// to the suite.
//
// adapters/REGISTRY.md IS in this tree and records all six rows, so the same assertion can be
// made without the source. It is a weaker claim — the registry records what a vendor DECLARED,
// not what their code holds — and it is a claim that can never silently skip, which is the
// property the skip destroyed.
//
// # A DEBT REGISTER, NOT AN ALLOW-LIST
//
// The three external rows are out of range TODAY and cannot be repaired from here: they live in
// three repositories this checkout does not contain (ADR-0005, independent cadence). A gate that
// simply failed would be red for weeks for a reason no session could clear, and
// the specification's citation baseline already records what that costs — "a gate that is red on
// arrival gets bypassed".
//
// So known evictions are registered below, and the register is load-bearing in BOTH directions:
//
//   - an out-of-range row that is NOT registered fails — a new eviction cannot arrive silently;
//   - a registered row that has come back INTO range also fails — the register can only shrink,
//     so a vendor PR landing is what removes the entry rather than something anyone must remember.
//
// That second direction is what makes this a register and not an amnesty.
type evictedRow struct {
	// adapter is the REGISTRY.md row's first cell, without backticks.
	adapter string
	// why is what a reader needs in order to act, and is checked against nothing — it is the
	// only field here a human writes.
	why string
}

// knownEvictions are the registry rows outside SupportedRange() at the moment this file was
// last updated, each with the action that clears it.
//
// REMOVE AN ENTRY WHEN THE VENDOR PR LANDS AND THE ROW MOVES. The test fails if an entry is
// left behind, so the removal is not a courtesy.
var knownEvictions = []evictedRow{
	{"fleet-adapter-ros2", "declares 1.0.0; needs a vendor PR raising CONTRACT_VERSION into the current range, then an ADR-0007 registry PR. No code change: every 1.1.0 and 1.2.0 change was additive."},
	{"fleet-adapter-vda5050", "declares 1.0.0; same one-line declaration change and registry PR as ros2."},
	{"fleet-adapter-mavlink", "declares 1.0.0; same one-line declaration change and registry PR as ros2."},
}

// registryRow is one parsed row of the adapter registry table.
type registryRow struct {
	adapter string
	version string
	line    int
}

// parseRegistry reads adapters/REGISTRY.md's adapter table.
//
// The table's shape is pinned by this parser rather than trusted: a row whose version cell stops
// matching is reported as unparseable and fails, because a registry the gate cannot read is a
// registry the gate is not checking. That is the failure mode the skip above already produced
// once, and it must not be reintroduced through a silent regex miss.
func parseRegistry(t *testing.T) []registryRow {
	t.Helper()
	const path = "../../adapters/REGISTRY.md"
	body, err := os.ReadFile(path) // #nosec G304 -- fixed in-repo path
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	// ANCHORED ON THE TABLE HEADER, not on the row shape. The naive "a line starting with a
	// backticked name" reading also matched the Conformance-status LEGEND table further up,
	// whose first column happens to contain `template` — the same word as an adapter name. A
	// parser that reads the wrong table is the silent-skip defect again, wearing a passing grep.
	const header = "| Adapter | Robot interface | Repository | Maintained by | Contract version | Conformance |"
	lines := strings.Split(string(body), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			start = i + 2 // skip the header and its separator row
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: the adapter table header is not present verbatim. Expected:\n  %s\n"+
			"The header IS the table's identity here: without it this gate cannot tell the "+
			"adapter table from the legend table above it, and a gate reading the wrong table "+
			"asserts nothing while passing", path, header)
	}

	// Within the table: a row starts with `| ` + a backticked adapter name. The contract
	// version is the 5th cell, written as `X.Y.Z` (`fleet_adapter.v1`).
	rowRe := regexp.MustCompile("^\\| `([a-z0-9-]+)` \\|")
	verRe := regexp.MustCompile("`([0-9]+\\.[0-9]+\\.[0-9]+)`")

	var out []registryRow
	for i, line := range lines {
		if i < start {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "|") && strings.TrimSpace(line) != "" {
			break // the table ended
		}
		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 7 {
			t.Errorf("%s:%d: adapter row %q has %d cells, want at least 7 — the registry table "+
				"shape changed and this gate can no longer find the contract version. A registry "+
				"the gate cannot read is a registry the gate is not checking",
				path, i+1, m[1], len(cells))
			continue
		}
		v := verRe.FindStringSubmatch(cells[5])
		if v == nil {
			t.Errorf("%s:%d: adapter row %q carries no `X.Y.Z` contract version in its 5th cell "+
				"(%q). Every row MUST record the version its result was earned against (ADR-0032)",
				path, i+1, m[1], strings.TrimSpace(cells[5]))
			continue
		}
		out = append(out, registryRow{adapter: m[1], version: v[1], line: i + 1})
	}
	if len(out) == 0 {
		t.Fatalf("%s: parsed no adapter rows at all. This gate is now asserting nothing, which is "+
			"the silent-skip failure it was written to remove", path)
	}
	return out
}

// Every registry row declares a contract version this control plane accepts — or is registered
// above as a known eviction with the action that clears it.
//
// THIS CHECK CANNOT SKIP. adapters/REGISTRY.md is in this tree and records all six adapters,
// including the three whose source is not here.
func TestRegistryRowsDeclareASupportedContractVersion(t *testing.T) {
	registered := make(map[string]string, len(knownEvictions))
	for _, e := range knownEvictions {
		registered[e.adapter] = e.why
	}

	seen := map[string]bool{}
	for _, row := range parseRegistry(t) {
		seen[row.adapter] = true
		ok, reason := Supports(row.version)
		_, isRegistered := registered[row.adapter]

		switch {
		case ok && isRegistered:
			// The register can only shrink. A row that has come back into range is a vendor PR
			// that landed, and leaving the entry behind would let the next real eviction hide
			// inside a stale register — which is how an allow-list forms out of a debt register.
			t.Errorf("adapters/REGISTRY.md:%d: %q now declares %s, which IS in range (%s) — "+
				"remove its entry from knownEvictions in this file. The register can only "+
				"shrink; a repaired row left registered is a slot a future eviction hides in",
				row.line, row.adapter, row.version, SupportedRange())
		case !ok && !isRegistered:
			t.Errorf("adapters/REGISTRY.md:%d: %q declares contract version %s, which this "+
				"control plane REFUSES: %s.\n"+
				"That adapter keeps its session, keeps streaming telemetry and still receives "+
				"emergency stop — and is refused RegisterRobot/DiscoverRobot with "+
				"VERSION_MISMATCH, so it can bring no robot under management. Its conformance "+
				"result is NOT invalidated (ADR-0032 Amendment 1 §A1.2: a qualification that "+
				"survives is not the same thing as an adapter that still works).\n"+
				"Either raise the declared version, or — if it cannot be raised from this tree — "+
				"add it to knownEvictions with the action that clears it. Do NOT relax this check: "+
				"it is the only thing in the tree that names an eviction before it reaches a "+
				"cluster",
				row.line, row.adapter, row.version, reason)
		case !ok && isRegistered:
			// The intended steady state for an adapter this tree cannot change. Logged, not
			// silent: a reader of the run sees the outstanding debt and what clears it.
			t.Logf("%s is a KNOWN eviction at %s (range %s): %s",
				row.adapter, row.version, SupportedRange(), registered[row.adapter])
		}
	}

	// An entry naming a row that no longer exists is debt pointing at nothing.
	for _, e := range knownEvictions {
		if !seen[e.adapter] {
			t.Errorf("knownEvictions names %q, which is not a row in adapters/REGISTRY.md. "+
				"Either the row was renamed or the adapter was removed; a register entry that "+
				"matches nothing silently stops registering anything", e.adapter)
		}
	}
}

// The registry records all six adapters, so the gate above covers the three the source-reading
// test can only skip.
//
// It is asserted rather than assumed because the coverage is the whole point of the file: if a
// registry row for an external adapter were ever dropped, both checks would go quiet about it at
// once — the source one by skipping, this one by having nothing to read.
func TestRegistryCoversEveryAdapterTheSourceCheckCannotRead(t *testing.T) {
	rows := parseRegistry(t)
	have := map[string]bool{}
	for _, r := range rows {
		have[r.adapter] = true
	}
	for _, want := range []string{
		"simulation", "example-noop",
		"fleet-adapter-ros2", "fleet-adapter-vda5050", "fleet-adapter-mavlink",
	} {
		if !have[want] {
			t.Errorf("adapters/REGISTRY.md has no row for %q. The source-reading check skips the "+
				"three external adapters; this file is what covers them, and it can only cover a "+
				"row that exists", want)
		}
	}
}
