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
	"fmt"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The zone-restriction resolver (ADR-0058 §3, amendment FZ-R1 … FZ-R3).
//
// These tests run with no client and no control plane: resolution is a pure fold
// over an ancestor chain, and keeping it pure is what makes FZ-R2 testable as a
// PROPERTY rather than as a handful of examples.
//
// The rule these tests exist for is FZ-R2, and it is written as a conformance
// obligation rather than a behaviour: "An implementation whose resolver can emit a
// weaker effective rule than some rule it read does not conform, whatever its
// admission checks do." That is a statement about EVERY input, which is why the
// test below sweeps a chain space rather than asserting a few outputs.

// zoneWithRules is a chain link: a zone name plus the rules it declares.
func zoneWithRules(name string, r *fleetv1.ZoneRestrictions) fleetv1.FleetZone {
	return fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       fleetv1.FleetZoneSpec{Restrictions: r},
	}
}

func opRule(subject string, mode fleetv1.ZoneRestrictionMode, max string, allowed []string) fleetv1.ZoneOperatingRestriction {
	return fleetv1.ZoneOperatingRestriction{Subject: subject, Mode: mode, Max: max, Allowed: allowed, Reason: "r-" + subject}
}

// ---------------------------------------------------------------------------
// FZ-R1 — most restrictive in the chain, root-downward. NOT nearest-ancestor.
// ---------------------------------------------------------------------------

func TestResolveZoneRestrictions_MostRestrictiveInChainNotNearest(t *testing.T) {
	// root denies the camera; the leaf says only "Limited". Nearest-ancestor-wins
	// would emit Limited and open a hole three levels down from the declaration.
	chain := []fleetv1.FleetZone{
		zoneWithRules("site", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("camera", fleetv1.ZoneRestrictionDenied, "", nil),
		}}),
		zoneWithRules("ward-3", nil),
		zoneWithRules("bay-2", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("camera", fleetv1.ZoneRestrictionLimited, "5", nil),
		}}),
	}

	eff, contradictions := resolveZoneRestrictions(chain)
	if len(contradictions) != 0 {
		t.Fatalf("Denied over Limited is a tightening, not a contradiction; got %v", contradictions)
	}
	got := effBySubject(eff)
	cam, ok := got[string(fleetv1.ZoneAxisOperating)+"/camera"]
	if !ok {
		t.Fatal("camera resolved to no rule at all — a declared denial vanished")
	}
	if cam.Mode != fleetv1.ZoneRestrictionDenied {
		t.Errorf("camera resolved to %s, want Denied. FZ-R1: the effective rule is the most "+
			"restrictive ANYWHERE in the chain, root-downward — not the nearest ancestor. "+
			"A nearest-ancestor-wins resolution does not conform", cam.Mode)
	}
	if cam.InheritedFrom != "site" {
		t.Errorf("inheritedFrom = %q, want \"site\" — the operator needs to know the denial is "+
			"three levels up and cannot be removed from here", cam.InheritedFrom)
	}
}

func TestResolveZoneRestrictions_TighterBoundAndAllowedIntersectionWin(t *testing.T) {
	chain := []fleetv1.FleetZone{
		zoneWithRules("site", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("speed", fleetv1.ZoneRestrictionLimited, "1.5", nil),
			opRule("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"quat-ammonium-7", "bleach", "peroxide"}),
		}}),
		zoneWithRules("ward-3", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("speed", fleetv1.ZoneRestrictionLimited, "0.4", nil),
			opRule("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"bleach", "quat-ammonium-7"}),
		}}),
		zoneWithRules("bay-2", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("speed", fleetv1.ZoneRestrictionLimited, "2.0", nil),
			opRule("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"quat-ammonium-7", "vinegar"}),
		}}),
	}

	got := effBySubject(resolveFirst(t, chain))

	speed := got[string(fleetv1.ZoneAxisOperating)+"/speed"]
	if speed.Max != "0.4" {
		t.Errorf("speed max = %q, want \"0.4\" — between two Limited rules the TIGHTER bound "+
			"wins; bounds intersect, they do not override", speed.Max)
	}
	if speed.InheritedFrom != "ward-3" {
		t.Errorf("speed inheritedFrom = %q, want ward-3 (the zone that supplied the winning bound)", speed.InheritedFrom)
	}

	chem := got[string(fleetv1.ZoneAxisOperating)+"/chemical"]
	if len(chem.Allowed) != 1 || chem.Allowed[0] != "quat-ammonium-7" {
		t.Errorf("chemical allowed = %v, want [quat-ammonium-7] — between two allow-lists the "+
			"INTERSECTION wins, never the union: an allow-list is a denial of everything else, "+
			"and a union would re-permit what an ancestor forbade", chem.Allowed)
	}
}

func TestResolveZoneRestrictions_EntryAndCapabilityAxes(t *testing.T) {
	chain := []fleetv1.FleetZone{
		zoneWithRules("site", &fleetv1.ZoneRestrictions{
			Entry: &fleetv1.ZoneEntryRestriction{Mode: fleetv1.ZoneRestrictionDenied, Reason: "patio"},
			Capabilities: []fleetv1.ZoneCapabilityRestriction{
				{Name: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied, Reason: "carpet"},
			},
		}),
		zoneWithRules("leaf", nil),
	}
	got := effBySubject(resolveFirst(t, chain))

	entry, ok := got[string(fleetv1.ZoneAxisEntry)+"/"]
	if !ok {
		t.Fatal("an entry denial declared on an ancestor did not reach the descendant — " +
			"entry is absorbing over the target relation, it applies to every descendant")
	}
	if entry.Mode != fleetv1.ZoneRestrictionDenied || entry.InheritedFrom != "site" || entry.Reason != "patio" {
		t.Errorf("entry resolved to %+v; want Denied inheritedFrom=site reason=patio", entry)
	}
	if entry.Subject != "" {
		t.Errorf("entry carried subject %q; entry has no subject", entry.Subject)
	}
	cap, ok := got[string(fleetv1.ZoneAxisCapability)+"/clean.mop"]
	if !ok || cap.Mode != fleetv1.ZoneRestrictionDenied || cap.InheritedFrom != "site" {
		t.Errorf("capability clean.mop resolved to %+v (present=%t); want Denied from site", cap, ok)
	}
}

// ---------------------------------------------------------------------------
// FZ-R3 — Denied + Required is a CONTRADICTION, not a widening.
// ---------------------------------------------------------------------------

func TestResolveZoneRestrictions_DeniedPlusRequiredIsContradictionResolvingToDenied(t *testing.T) {
	chain := []fleetv1.FleetZone{
		zoneWithRules("hospital", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("camera", fleetv1.ZoneRestrictionDenied, "", nil),
		}}),
		zoneWithRules("inspection-bay", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("camera", fleetv1.ZoneRestrictionRequired, "", nil),
		}}),
	}

	eff, contradictions := resolveZoneRestrictions(chain)

	cam := effBySubject(eff)[string(fleetv1.ZoneAxisOperating)+"/camera"]
	if cam.Mode != fleetv1.ZoneRestrictionDenied {
		t.Errorf("camera resolved to %s, want Denied. FZ-R3: resolving the other way would "+
			"switch a camera on in a patient room by operation of a merge rule", cam.Mode)
	}
	if len(contradictions) != 1 {
		t.Fatalf("got %d contradictions, want exactly 1: %+v", len(contradictions), contradictions)
	}
	c := contradictions[0]
	if c.Subject != "camera" {
		t.Errorf("contradiction subject = %q, want camera", c.Subject)
	}
	// BOTH declaring zones must be named — an operator told only one of them cannot
	// act, because they do not know which edit to make or whether they own it.
	if c.DeniedBy != "hospital" || c.RequiredBy != "inspection-bay" {
		t.Errorf("contradiction named deniedBy=%q requiredBy=%q; want hospital / inspection-bay — "+
			"FZ-R3 requires BOTH declaring zones", c.DeniedBy, c.RequiredBy)
	}

	// And the reverse declaration order must reach the same answer: Denied is
	// absorbing regardless of which zone is deeper.
	rev := []fleetv1.FleetZone{
		zoneWithRules("hospital", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("camera", fleetv1.ZoneRestrictionRequired, "", nil),
		}}),
		zoneWithRules("inspection-bay", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			opRule("camera", fleetv1.ZoneRestrictionDenied, "", nil),
		}}),
	}
	revEff, revContra := resolveZoneRestrictions(rev)
	if m := effBySubject(revEff)[string(fleetv1.ZoneAxisOperating)+"/camera"].Mode; m != fleetv1.ZoneRestrictionDenied {
		t.Errorf("reversed chain resolved camera to %s, want Denied — Denied is absorbing "+
			"whichever end of the chain declared it", m)
	}
	if len(revContra) != 1 || revContra[0].DeniedBy != "inspection-bay" || revContra[0].RequiredBy != "hospital" {
		t.Errorf("reversed chain contradiction = %+v; want deniedBy=inspection-bay requiredBy=hospital", revContra)
	}
}

// ---------------------------------------------------------------------------
// FZ-R2 — the property. Nothing the resolver emits may be weaker than anything
// it read. This is the test the rule is written as an obligation for.
// ---------------------------------------------------------------------------

func TestResolveZoneRestrictions_FZR2_NoEmittedRuleIsWeakerThanAnyRuleRead(t *testing.T) {
	// A chain space, not a handful of examples: every combination of three zones
	// each declaring one of a set of camera/speed/chemical rules. FZ-R2 is a
	// statement about every input the resolver can be handed, so the test sweeps.
	variants := []*fleetv1.ZoneRestrictions{
		nil,
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("camera", fleetv1.ZoneRestrictionDenied, "", nil)}},
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("camera", fleetv1.ZoneRestrictionRequired, "", nil)}},
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("camera", fleetv1.ZoneRestrictionLimited, "3", nil)}},
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("speed", fleetv1.ZoneRestrictionLimited, "0.4", nil)}},
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("speed", fleetv1.ZoneRestrictionLimited, "2.0", nil)}},
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("speed", fleetv1.ZoneRestrictionDenied, "", nil)}},
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"a", "b", "c"})}},
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"b", "c", "d"})}},
		{Operating: []fleetv1.ZoneOperatingRestriction{opRule("chemical", fleetv1.ZoneRestrictionLimited, "", []string{"z"})}},
		{Entry: &fleetv1.ZoneEntryRestriction{Mode: fleetv1.ZoneRestrictionDenied, Reason: "x"}},
		{Capabilities: []fleetv1.ZoneCapabilityRestriction{{Name: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied}}},
	}

	checked := 0
	for i := range variants {
		for j := range variants {
			for k := range variants {
				chain := []fleetv1.FleetZone{
					zoneWithRules("z0", variants[i]),
					zoneWithRules("z1", variants[j]),
					zoneWithRules("z2", variants[k]),
				}
				eff, _ := resolveZoneRestrictions(chain)
				checked++

				byKey := effBySubject(eff)
				for _, link := range chain {
					for _, in := range declaredRulesOf(link) {
						out, ok := byKey[in.key]
						if !ok {
							t.Fatalf("chain(%d,%d,%d): zone %q declared %s/%s but the resolver "+
								"emitted NO effective rule for it — a rule that vanishes is "+
								"infinitely weaker than the rule that was read (FZ-R2)",
								i, j, k, link.Name, in.axis, in.subject)
						}
						if why := weakerThan(out, in); why != "" {
							t.Fatalf("chain(%d,%d,%d): FZ-R2 VIOLATED. zone %q declared "+
								"{axis:%s subject:%s mode:%s max:%q allowed:%v} and the resolver "+
								"emitted {mode:%s max:%q allowed:%v}: %s\n"+
								"FZ-R2: \"No resolution, merge, or default may produce an effective "+
								"rule more permissive than a rule declared by an ancestor.\"",
								i, j, k, link.Name, in.axis, in.subject, in.mode, in.max, in.allowed,
								out.Mode, out.Max, out.Allowed, why)
						}
					}
				}

				// The mirror obligation: the resolver must not invent a rule either.
				for key, out := range byKey {
					if !anyChainDeclares(chain, key) {
						t.Fatalf("chain(%d,%d,%d): the resolver emitted %s for %q, which NO zone "+
							"in the chain declared. Absence of a rule is permission; inventing "+
							"one restricts what the operator did not", i, j, k, out.Mode, key)
					}
				}
			}
		}
	}
	t.Logf("FZ-R2 held over %d chains (%d rule comparisons per chain, 3 zones each)", checked, 3)
	if checked != len(variants)*len(variants)*len(variants) {
		t.Errorf("swept %d chains, expected %d", checked, len(variants)*len(variants)*len(variants))
	}
}

// ---------------------------------------------------------------------------
// helpers — the FZ-R2 predicate is the load-bearing one.
// ---------------------------------------------------------------------------

type declaredRule struct {
	key     string
	axis    fleetv1.ZoneRestrictionAxis
	subject string
	mode    fleetv1.ZoneRestrictionMode
	max     string
	allowed []string
}

func declaredRulesOf(fz fleetv1.FleetZone) []declaredRule {
	r := fz.Spec.Restrictions
	if r == nil {
		return nil
	}
	var out []declaredRule
	if r.Entry != nil {
		out = append(out, declaredRule{
			key: string(fleetv1.ZoneAxisEntry) + "/", axis: fleetv1.ZoneAxisEntry, mode: r.Entry.Mode,
		})
	}
	for _, c := range r.Capabilities {
		out = append(out, declaredRule{
			key: string(fleetv1.ZoneAxisCapability) + "/" + c.Name, axis: fleetv1.ZoneAxisCapability,
			subject: c.Name, mode: c.Mode,
		})
	}
	for _, o := range r.Operating {
		out = append(out, declaredRule{
			key: string(fleetv1.ZoneAxisOperating) + "/" + o.Subject, axis: fleetv1.ZoneAxisOperating,
			subject: o.Subject, mode: o.Mode, max: o.Max, allowed: o.Allowed,
		})
	}
	return out
}

func anyChainDeclares(chain []fleetv1.FleetZone, key string) bool {
	for _, z := range chain {
		for _, d := range declaredRulesOf(z) {
			if d.key == key {
				return true
			}
		}
	}
	return false
}

// weakerThan returns a non-empty explanation when the emitted rule permits
// something the declared rule forbade. It is the machine form of FZ-R2.
func weakerThan(out fleetv1.EffectiveZoneRestriction, in declaredRule) string {
	// Denied forbids the subject outright. Anything else permits more.
	if in.mode == fleetv1.ZoneRestrictionDenied && out.Mode != fleetv1.ZoneRestrictionDenied {
		return fmt.Sprintf("a declared Denied resolved to %s", out.Mode)
	}
	// Required mandates the subject be running. Dropping it to Limited or to
	// nothing loses the mandate; only Denied may absorb it (FZ-R3).
	if in.mode == fleetv1.ZoneRestrictionRequired &&
		out.Mode != fleetv1.ZoneRestrictionRequired && out.Mode != fleetv1.ZoneRestrictionDenied {
		return fmt.Sprintf("a declared Required resolved to %s, losing the mandate", out.Mode)
	}
	// Denied needs no parameters; it is stronger than any bound.
	if out.Mode == fleetv1.ZoneRestrictionDenied {
		return ""
	}
	if in.max != "" {
		if out.Max == "" {
			return fmt.Sprintf("a declared max %q resolved to no bound at all", in.max)
		}
		inV, err1 := strconv.ParseFloat(in.max, 64)
		outV, err2 := strconv.ParseFloat(out.Max, 64)
		if err1 != nil || err2 != nil {
			return fmt.Sprintf("unparseable bound (in %q, out %q)", in.max, out.Max)
		}
		if outV > inV {
			return fmt.Sprintf("a declared max %q resolved to a LOOSER %q", in.max, out.Max)
		}
	}
	if len(in.allowed) > 0 {
		if len(out.Allowed) == 0 && out.Mode != fleetv1.ZoneRestrictionDenied {
			// An empty intersection is legitimate ONLY if it really is the
			// intersection; "no list" would permit everything, which is weaker.
			// resolveZoneRestrictions must distinguish these, so an emitted
			// Limited with no list where one was declared is a violation.
			return fmt.Sprintf("a declared allow-list %v resolved to no list, permitting everything", in.allowed)
		}
		permitted := map[string]bool{}
		for _, a := range in.allowed {
			permitted[a] = true
		}
		for _, a := range out.Allowed {
			if !permitted[a] {
				return fmt.Sprintf("the resolved allow-list %v contains %q, which the declared "+
					"list %v did not permit", out.Allowed, a, in.allowed)
			}
		}
	}
	return ""
}

func effBySubject(eff []fleetv1.EffectiveZoneRestriction) map[string]fleetv1.EffectiveZoneRestriction {
	m := make(map[string]fleetv1.EffectiveZoneRestriction, len(eff))
	for _, e := range eff {
		m[string(e.Axis)+"/"+e.Subject] = e
	}
	return m
}

func resolveFirst(t *testing.T, chain []fleetv1.FleetZone) []fleetv1.EffectiveZoneRestriction {
	t.Helper()
	eff, contradictions := resolveZoneRestrictions(chain)
	if len(contradictions) != 0 {
		t.Fatalf("unexpected contradictions: %+v", contradictions)
	}
	return eff
}
