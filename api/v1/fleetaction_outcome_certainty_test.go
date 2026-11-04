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

package v1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two additions this file gates are SCHEMA, not controller behaviour: an enum value
// and two CEL rules. What makes them work is that they reach the generated CRD, because
// the API server is the only thing that enforces them — no Go code path evaluates either
// rule. A unit test over the Go constants alone would pass on a tree where
// `make manifests` had never been run, which is the one failure that matters here.
// So these read the generated CRD.

// crdRules returns the CRD text with whitespace collapsed and YAML's doubled
// single-quote escaping undone, so a CEL rule can be compared as it is written in the
// Go marker rather than as YAML chose to fold and quote it.
func crdRules(t *testing.T) string {
	t.Helper()
	return strings.ReplaceAll(strings.Join(strings.Fields(fleetActionCRD(t)), " "), "''", "'")
}

func fleetActionCRD(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "config", "crd", "bases", "swarmada.io_fleetactions.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated FleetAction CRD: %v (run `make manifests`)", err)
	}
	return string(b)
}

// TestOutcomeCertaintyFailsClosedInTheSchema is the whole point of the field.
//
// An outcome nobody could establish must never be readable as a success. That is not
// enforced by asking every consumer to handle it — RFC-0001's phase-enum extension policy
// exists because consumers do not handle values they were not compiled against. It is
// enforced by making the combination unstorable: the CEL rule on `status` admits
// `Unestablished` only alongside `phase: Failed`, so no writer — the reconciler, a raw
// `kubectl patch --subresource=status`, or a downgraded controller — can put it anywhere
// else. A CRD's CEL rules apply to the whole object regardless of which subresource wrote
// it, which is what makes a rule on status enforceable at all.
func TestOutcomeCertaintyFailsClosedInTheSchema(t *testing.T) {
	flat := crdRules(t)
	const rule = "!has(self.outcomeCertainty) || self.outcomeCertainty != 'Unestablished' || self.phase == 'Failed'"
	if !strings.Contains(flat, strings.Join(strings.Fields(rule), " ")) {
		t.Errorf("generated CRD is missing the fail-closed CEL rule on status.\n"+
			"want rule: %s\n"+
			"Without it, `status.outcomeCertainty: Unestablished` can be written onto "+
			"Succeeded — the single failure mode the field exists to remove.", rule)
	}
}

// TestOutcomeCertaintyEnumIsClosed guards the two-value vocabulary. A third value added
// without thought is how "Unestablished" quietly acquires a neighbour that consumers
// bucket with Established.
func TestOutcomeCertaintyEnumIsClosed(t *testing.T) {
	if OutcomeCertaintyEstablished != "Established" {
		t.Errorf("OutcomeCertaintyEstablished = %q, want %q", OutcomeCertaintyEstablished, "Established")
	}
	if OutcomeCertaintyUnestablished != "Unestablished" {
		t.Errorf("OutcomeCertaintyUnestablished = %q, want %q", OutcomeCertaintyUnestablished, "Unestablished")
	}

	crd := fleetActionCRD(t)
	idx := strings.Index(crd, "outcomeCertainty:")
	if idx < 0 {
		t.Fatal("generated CRD has no status.outcomeCertainty property (run `make manifests`)")
	}
	// The enum block follows the property's description; bound the window generously and
	// assert on what it contains rather than on exact indentation.
	window := crd[idx:min(idx+4000, len(crd))]
	for _, want := range []string{"- Established", "- Unestablished"} {
		if !strings.Contains(window, want) {
			t.Errorf("status.outcomeCertainty enum is missing %q", want)
		}
	}
}

// TestOutcomeCertaintyIsSurfacedToOperators. An unestablished outcome ALWAYS needs a
// person, and the only surface that carries it at v0.4 is the printer column: no v0.4
// controller writes a FleetAction condition, and the event is specified-not-implemented.
// If this column is dropped, an operator sees `Failed` and nothing distinguishes a
// failure the robot observed from one nobody could establish.
func TestOutcomeCertaintyIsSurfacedToOperators(t *testing.T) {
	crd := fleetActionCRD(t)
	flat := strings.Join(strings.Fields(crd), " ")
	for _, want := range []string{
		"- jsonPath: .status.outcomeCertainty name: Outcome",
		"- jsonPath: .spec.customType name: Verb",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("generated CRD is missing printer column %q", want)
		}
	}
}

// TestCleanIsInTheActionTypeEnum. `Clean` is not cosmetic: FleetAdapter.status.
// supportedActions[].actionType is matched to spec.type by EXACT string equality and
// admission refuses an action no connected adapter advertises, so without the value a
// cleaning adapter cannot advertise a catalog describing what it does.
func TestCleanIsInTheActionTypeEnum(t *testing.T) {
	if ActionTypeClean != "Clean" {
		t.Errorf("ActionTypeClean = %q, want %q", ActionTypeClean, "Clean")
	}
	crd := fleetActionCRD(t)
	flat := strings.Join(strings.Fields(crd), " ")
	if !strings.Contains(flat, "- Charge - Clean - Custom - External") {
		t.Error("generated CRD's spec.type enum does not carry Clean between Charge and Custom")
	}
}

// TestCustomTypeIsRefusedOnANamedVerb. spec.customType is a printer column, so a
// Navigate action carrying `customType: clean` would print a claim its own type
// contradicts. The rule REFUSES that pair rather than ignoring the field — a field a
// caller cannot meaningfully set is self-documenting at apply time.
//
// It is deliberately not the mirror rule. Requiring customType on `type: Custom` would
// tighten validation on a published CRD and reject every Custom action already in the
// field on its next update (docs/api-principles.md, "Versioning and compatibility").
func TestCustomTypeIsRefusedOnANamedVerb(t *testing.T) {
	flat := crdRules(t)
	const rule = "!has(self.customType) || self.type == 'Custom'"
	if !strings.Contains(flat, strings.Join(strings.Fields(rule), " ")) {
		t.Errorf("generated CRD is missing the customType discriminator rule.\nwant: %s", rule)
	}
	if strings.Contains(flat, "self.type != 'Custom' || has(self.customType)") {
		t.Error("generated CRD REQUIRES customType when type is Custom. That tightens " +
			"validation on a published CRD and rejects every existing Custom action on " +
			"its next update; it is available only at a major revision.")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
