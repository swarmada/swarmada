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

package controller

import (
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Required and Limited on one subject in one chain: CARRY BOTH (ADR-0058).
//
// Required mandates the subject be running; Limited bounds it. They are two statements
// about one subject, not two strengths of one statement, so neither is dropped: the
// resolved rule is Required, and it keeps the bound. "This must be running, and within
// these limits." It is not a contradiction (only Denied+Required is, FZ-R3), and the
// order of the two in the chain does not change the answer.
func TestResolveZoneRestrictions_RequiredPlusLimitedCarriesBoth(t *testing.T) {
	required := &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
		opRule("uv", fleetv1.ZoneRestrictionRequired, "", nil),
	}}
	limited := &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
		opRule("uv", fleetv1.ZoneRestrictionLimited, "5", nil),
	}}

	for _, c := range []struct {
		name  string
		chain []fleetv1.FleetZone
	}{
		{"Required above Limited", []fleetv1.FleetZone{
			zoneWithRules("hospital", required), zoneWithRules("store-room", limited),
		}},
		{"Limited above Required", []fleetv1.FleetZone{
			zoneWithRules("hospital", limited), zoneWithRules("store-room", required),
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			eff, contradictions := resolveZoneRestrictions(c.chain)
			if len(contradictions) != 0 {
				t.Fatalf("Required+Limited reported a contradiction %+v; only Denied+Required is one (FZ-R3)", contradictions)
			}
			uv, ok := effBySubject(eff)[string(fleetv1.ZoneAxisOperating)+"/uv"]
			if !ok {
				t.Fatal("uv resolved to no rule at all")
			}
			if uv.Mode != fleetv1.ZoneRestrictionRequired {
				t.Errorf("mode = %s, want Required: dropping to Limited loses the mandate", uv.Mode)
			}
			if uv.Max != "5" {
				t.Errorf("max = %q, want \"5\": Required keeps the bound it was folded with", uv.Max)
			}
		})
	}
}

// The same holds for an allow-list: Required keeps the intersected list.
func TestResolveZoneRestrictions_RequiredPlusLimitedCarriesTheAllowList(t *testing.T) {
	chain := []fleetv1.FleetZone{
		zoneWithRules("site", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"bleach", "peroxide"}),
		}}),
		zoneWithRules("theatre", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("chemical", fleetv1.ZoneRestrictionRequired, "", nil),
		}}),
	}
	chem := effBySubject(resolveFirst(t, chain))[string(fleetv1.ZoneAxisOperating)+"/chemical"]
	if chem.Mode != fleetv1.ZoneRestrictionRequired {
		t.Errorf("mode = %s, want Required", chem.Mode)
	}
	if len(chem.Allowed) != 2 || chem.Allowed[0] != "bleach" || chem.Allowed[1] != "peroxide" {
		t.Errorf("allowed = %v, want [bleach peroxide]: Required keeps the allow-list", chem.Allowed)
	}
}
