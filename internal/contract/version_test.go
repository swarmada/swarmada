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
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// rangeFor derives the range for an arbitrary version, mirroring SupportedRange's rule so the
// N-1 and clamp cases can be exercised without mutating the compiled-in constant.
func rangeFor(t *testing.T, v string) string {
	t.Helper()
	major, minor := mustParse(v)
	low := minor - 1
	if low < 0 {
		low = 0
	}
	return fmt.Sprintf(">=%d.%d.0 <%d.%d.0", major, low, major, minor+1)
}

// The invariant that matters: whatever the constant is, the advertised range must INCLUDE the
// version this build implements. A derivation bug that excluded it would advertise a range in which
// this control plane cannot drive its own reference adapters.
func TestSupportedRange_IncludesTheImplementedVersion(t *testing.T) {
	major, minor := mustParse(Version)
	got := SupportedRange()
	lowMinor := minor - 1
	if lowMinor < 0 {
		lowMinor = 0
	}
	wantLow := fmt.Sprintf(">=%d.%d.0", major, lowMinor)
	wantHigh := fmt.Sprintf("<%d.%d.0", major, minor+1)
	if !strings.Contains(got, wantLow) || !strings.Contains(got, wantHigh) {
		t.Fatalf("SupportedRange() = %q, want it to span %s %s (the implemented version %s must be inside)",
			got, wantLow, wantHigh, Version)
	}
}

// The N and N-1 rule, including both edges.
func TestSupportedRange_Derivation(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"1.0.0", ">=1.0.0 <1.1.0"}, // minor 0: no N-1 to reach back to, so this minor alone
		{"1.1.0", ">=1.0.0 <1.2.0"}, // N and N-1
		{"1.3.2", ">=1.2.0 <1.4.0"}, // patch is irrelevant to the window
		{"2.0.0", ">=2.0.0 <2.1.0"}, // a major bump never reaches into the previous major
	} {
		if got := rangeFor(t, tc.version); got != tc.want {
			t.Errorf("version %s -> %q, want %q", tc.version, got, tc.want)
		}
	}
}

// The current constant's range, spelled out, so a bump is a deliberate, visible test change rather
// than a silent behaviour shift.
//
// IT USED TO SKIP ON A BUMP, WHICH IS THE OPPOSITE OF WHAT THE SENTENCE ABOVE ASKS FOR. A skipped
// test is a green test: the 1.1.0 -> 1.2.0 bump silenced this file's only statement of the actual
// window, and the eviction that bump causes (ADR-0032 Amendment 1 §A1.2) went past it unremarked.
// A bump must now come here and change two strings on purpose.
func TestSupportedRange_CurrentValue(t *testing.T) {
	// Moved 1.3.0 -> 1.4.0 on 2026-10-04 for one catalog entry per Custom verb
	// (SupportedAction.custom_type), the verb on AssignAction and ValidateAction
	// (custom_type), and CANCEL_DISPOSITION_STOP_UNCONFIRMED: additive proto surface, so
	// MINOR per ADR-0032 A1.1. This window holds at 1.4.0; a later minor moves it, by the
	// N and N-1 rule.
	//
	// WHAT THE NEW WINDOW EVICTS, checked before the bump as A1.2 requires. >=1.3.0 means
	// anything declaring 1.2.0 or below is refused registration. Of the in-tree adapters, the
	// simulation adapter moved 1.2.0 -> 1.4.0 (it implements what 1.4.0 added) and
	// example-noop 1.2.0 -> 1.3.0 (it stays at N-1, so the skew is exercised); the template
	// ships at N. The three EXTERNAL reference adapters declare 1.0.0 and were already outside
	// the previous window; they move in their own repositories.
	//
	// Moved 1.2.0 -> 1.3.0 on 2026-09-14 for ADR-0059's zone_constraints Command arm and
	// its ack (FA-R7): additive proto surface, so MINOR per ADR-0032 A1.1.
	//
	// WHAT THE NEW WINDOW EVICTS, checked before the bump as A1.2 requires. >=1.2.0 means
	// anything declaring 1.1.0 or below is refused registration. The three IN-TREE
	// adapters were moved 1.1.0 -> 1.2.0 in the same change, which is a true claim: they
	// implement the FA-R5/FA-R6 decline that 1.2.0 added, and they decline the new
	// OPTIONAL zone_constraints arm with unsupported=true, which is the conformant answer
	// and the reason that arm is optional. The three EXTERNAL reference adapters declare
	// 1.0.0 and were already outside the previous window; this bump does not change their
	// state and does not add a new eviction; that one is already recorded against the
	// previous bump.
	const (
		wantVersion = "1.4.0"
		wantRange   = ">=1.3.0 <1.5.0"
	)
	if Version != wantVersion {
		t.Fatalf("contract.Version is %s, and this expectation still says %s. Update BOTH lines "+
			"below deliberately — and check what the new range EVICTS before you do: an adapter "+
			"falls out of support on the SECOND minor after the version it reports, and is then "+
			"refused registration (ADR-0032 Amendment 1 §A1.2).", Version, wantVersion)
	}
	if got := SupportedRange(); got != wantRange {
		t.Errorf("SupportedRange() = %q, want %q", got, wantRange)
	}
}

// ADR-0032 Amendment 1 §A1.2: a minor bump never invalidates a QUALIFICATION, and never affects an
// adapter reporting the current or the IMMEDIATELY PRECEDING minor. That is the half of the ADR's
// original promise which survived; the other half — "patch and minor releases never break an
// adapter" — is struck, because the N/N-1 window is two minors wide and moves with [Version]:
// a control plane on the SECOND minor after the version an adapter reports refuses it.
//
// This asserts the surviving half rather than assuming it: every adapter at minor N-1 must still
// pass the handshake gate and therefore the dispatch gate that reads the same [Supports]
// (robot_webhook.go, adapter_readiness.go). It says NOTHING about minor N-2, which is evicted by
// design.
//
// Guards the two ways a bump goes wrong: bumping the MAJOR instead of the minor, and hand-editing
// SupportedRange so it no longer reaches back to N-1.
func TestMinorBump_KeepsEveryPreviousMinorAdapterQualified(t *testing.T) {
	major, minor := mustParse(Version)
	if minor == 0 {
		t.Skipf("contract.Version %s is at minor 0; there is no previous minor to keep qualified", Version)
	}
	for _, patch := range []int{0, 1, 2, 17} {
		prev := fmt.Sprintf("%d.%d.%d", major, minor-1, patch)
		if ok, reason := Supports(prev); !ok {
			t.Errorf("Supports(%q) = false (%s); a MINOR bump to %s must not invalidate an adapter "+
				"qualified against the previous minor (ADR-0032 Decision, as amended by §A1.2)", prev, reason, Version)
		}
	}
	// The patch component is ignored, so the implemented minor's own patches stay in too.
	for _, cur := range []string{
		fmt.Sprintf("%d.%d.0", major, minor),
		fmt.Sprintf("%d.%d.9", major, minor),
	} {
		if ok, reason := Supports(cur); !ok {
			t.Errorf("Supports(%q) = false (%s); the implemented minor must be inside its own range", cur, reason)
		}
	}
}

// A NEWER minor than we implement is outside the advertised window: skew is backwards
// compatibility for older adapters, not a promise about a contract we have not built.
func TestSupportedRange_ExcludesANewerMinor(t *testing.T) {
	_, minor := mustParse(Version)
	upper := fmt.Sprintf("<1.%d.0", minor+1)
	if !strings.Contains(SupportedRange(), upper) {
		t.Fatalf("SupportedRange() = %q, want an exclusive upper bound %q", SupportedRange(), upper)
	}
}

// Version must be a full, numeric semver — the range derivation and the handshake gate both depend
// on it parsing.
func TestVersion_IsSemver(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Fatalf("contract.Version = %q, want major.minor.patch", Version)
	}
	major, _ := mustParse(Version)
	if !strings.HasPrefix(SupportedRange(), ">="+strconv.Itoa(major)+".") {
		t.Errorf("range %q does not open on the implemented major %d", SupportedRange(), major)
	}
}

// A malformed constant must panic rather than return a zero that would advertise ">=0.0.0" and
// accept every adapter — fail closed, at build/test time.
func TestMustParse_PanicsOnMalformed(t *testing.T) {
	for _, bad := range []string{"1.0", "", "x.0.0", "1.y.0"} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("mustParse(%q) did not panic; a malformed constant must never silently widen the range", bad)
				}
			}()
			mustParse(bad)
		})
	}
}

// The Go constant and the conformance harness's CONTRACT_VERSION are two halves of one fact: the
// harness stamps a report with the version a result was earned against, and this build advertises
// the range it accepts. If they drift, an adapter can be qualified against a version the control
// plane does not implement — so they are asserted equal here.
func TestVersion_MatchesConformanceHarness(t *testing.T) {
	// A literal path (not a composed variable) so this stays clean under gosec's G304: the file is
	// a fixed in-repo source, never caller-supplied.
	const path = "../../adapters/conformance/report.py"
	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}
	m := regexp.MustCompile(`(?m)^CONTRACT_VERSION\s*=\s*"([^"]+)"`).FindSubmatch(body)
	if m == nil {
		t.Fatalf("no CONTRACT_VERSION found in %s — the harness must stamp the contract version", path)
	}
	if got := string(m[1]); got != Version {
		t.Errorf("harness CONTRACT_VERSION = %q but contract.Version = %q; they must be bumped together "+
			"(a report would attest a version this control plane does not implement)", got, Version)
	}
}
