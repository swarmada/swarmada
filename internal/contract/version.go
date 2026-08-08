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

// Package contract carries the fleet-adapter CONTRACT version this control plane implements, and
// the compatibility range it accepts from an adapter (ADR-0032).
//
// It exists as its own package so the one authoritative value is shared without an import cycle:
// the SwarmadaConfig controller advertises the range on status, and the ControlStream handshake
// gate intersects a reported version against it.
package contract

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is the fleet-adapter contract version this build implements: a semver over the proto
// surface, the SupportedAction schema (ADR-0019), and the conformance-suite revision.
//
// It is deliberately the ONLY hand-maintained value in this package — [SupportedRange] is derived
// from it, so bumping the contract cannot leave the advertised range stale.
//
// Distinct from two neighbours it is easily confused with: the wire-package identity
// ("fleet_adapter.v1", an identity string, not a semver, which is why it cannot express
// compatibility) and an adapter's own build version (two builds can implement one contract
// version; one build can span several).
//
// Bump rules (ADR-0032): a MAJOR bump is breaking — adapters re-run `make conformance` and update
// their adapters/REGISTRY.md row; MINOR and PATCH bumps are compatible and never invalidate an
// existing qualification. The conformance harness stamps this same value into a report as
// `contract_version` (adapters/conformance/report.py CONTRACT_VERSION); the two MUST move together.
const Version = "1.0.0"

// SupportedRange is the contract-version range this control plane accepts from an adapter: the
// implemented minor N and its predecessor N-1, within the current major (ADR-0032, "Supported
// range is N and N-1 minor within the current major").
//
// Two edges are deliberate:
//
//   - At minor 0 there is no N-1, so the range narrows to that minor alone rather than reaching
//     back into the previous major — a major bump is breaking by definition, so it can never be
//     inside a compatibility window.
//   - The upper bound is exclusive at N+1: an adapter built against a NEWER minor than this build
//     implements is OUT of range. Skew is backwards-compatibility for older adapters, not a
//     promise to honour a contract we have not built.
//
// Format is a two-clause space-separated range (">=x.y.0 <x.z.0"), the same shape the CRD field
// documents.
func SupportedRange() string {
	major, minor := mustParse(Version)
	low := minor - 1
	if low < 0 {
		low = 0
	}
	return fmt.Sprintf(">=%d.%d.0 <%d.%d.0", major, low, major, minor+1)
}

// mustParse splits Version into major and minor. Version is a compiled-in constant, so a malformed
// value is a build-time programming error, not a runtime condition — it panics rather than
// returning a zero that would silently advertise ">=0.0.0" and accept everything (fail closed).
func mustParse(v string) (int, int) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 3 {
		panic("contract.Version must be a full semver major.minor.patch, got " + v)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		panic("contract.Version has a non-numeric major: " + v)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		panic("contract.Version has a non-numeric minor: " + v)
	}
	return major, minor
}

// Supports reports whether a contract version reported by an adapter is inside [SupportedRange],
// and returns a human-readable reason when it is not (for the wire message).
//
// FAIL CLOSED (ADR-0032: "Missing or unparseable version data is treated as incompatible … never as
// an implicit pass"). Not supported:
//   - empty — an adapter built before contract_version existed reports nothing;
//   - malformed or non-numeric ("1", "1.0", "x.y.z");
//   - anything carrying prerelease or build metadata ("1.0.0-rc1", "1.0.0+build"). A pre-release is
//     by definition not the released contract, and accepting one would let an adapter qualify
//     against a contract revision that was never published.
//
// A DIFFERENT MAJOR is never supported, in either direction: a major bump is breaking by
// definition, so it cannot sit inside a compatibility window. A NEWER minor than this build
// implements is also refused — skew is backwards compatibility for older adapters, not a promise
// about a contract we have not built. The patch component is ignored: a patch never breaks.
func Supports(reported string) (bool, string) {
	if reported == "" {
		return false, "no contract_version reported (an adapter that predates contract versioning); " +
			"treated as incompatible"
	}
	major, minor, ok := parseStrict(reported)
	if !ok {
		return false, fmt.Sprintf("contract_version %q is not a plain semver major.minor.patch "+
			"(prerelease and build metadata are not accepted)", reported)
	}
	ourMajor, ourMinor := mustParse(Version)
	if major != ourMajor {
		return false, fmt.Sprintf("contract_version %q is major %d; this control plane implements "+
			"major %d (a major bump is breaking and requires re-qualification)", reported, major, ourMajor)
	}
	low := ourMinor - 1
	if low < 0 {
		low = 0
	}
	if minor < low || minor > ourMinor {
		return false, fmt.Sprintf("contract_version %q is outside the supported range %s "+
			"(this control plane implements %s)", reported, SupportedRange(), Version)
	}
	return true, ""
}

// parseStrict parses a plain numeric major.minor.patch. Unlike mustParse (which guards a
// compiled-in constant and panics), this handles UNTRUSTED input off the wire and never panics:
// anything it cannot read is reported as unparseable, which the caller treats as incompatible.
func parseStrict(v string) (int, int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		// strconv.Atoi rejects "1-rc1", "1+build", "", "01x" and any sign/whitespace, so
		// prerelease and build metadata fall out here rather than needing a separate check.
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], true
}
