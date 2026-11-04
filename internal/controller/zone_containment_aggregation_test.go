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
	"reflect"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/zone"
)

// AGGREGATION OVER THE CONTAINMENT SET (ADR-0059 §2).
//
// Overlapping polygons are expected and supported — a charging bay inside a warehouse
// floor, a sterile corridor crossing two wards. `DeriveCurrentZone` arbitrates them to
// one winner by nearest centroid, and that tie-break answers "where should this robot
// report it is". It does not answer "what binds it": picking one chain and discarding the
// other would be PERMISSIVE, where FZ-R2 (denial absorbs) and FZ-R6 (fail closed) both say
// union.
//
// The union is over ALREADY-RESOLVED projections. Each zone's
// status.restrictions.effective[] has already folded its own ancestor chain
// (FZ-R1/FZ-R4), so this is a second application of the same most-restrictive fold to
// values that are already answers. It is NOT a second walk of the tree — FZ-R4 forbids
// that and FZ-R9 names the drift it produces (nearest-ancestor-wins).

func czRule(subject string, mode fleetv1.ZoneRestrictionMode, from, reason string) fleetv1.EffectiveZoneRestriction {
	return fleetv1.EffectiveZoneRestriction{
		Axis: fleetv1.ZoneAxisOperating, Subject: subject,
		Mode: mode, InheritedFrom: from, Reason: reason,
	}
}

func sq(x, y, side float64) []zone.Point {
	return []zone.Point{{X: x, Y: y}, {X: x + side, Y: y}, {X: x + side, Y: y + side}, {X: x, Y: y + side}}
}

// ADR-0059's own worked case:
//
//	ward-a  leaf, polygon P1, declares operating camera: Denied
//	bay-1   leaf, polygon P2 overlapping P1, declares nothing
//	amr-1   position inside P1 ∩ P2; bay-1's centroid is nearer
//
// DeriveCurrentZone returns bay-1, whose chain forbids nothing.
func overlapFixture() ([]fleetv1.FleetZone, []zone.Candidate, zone.Position) {
	zones := []fleetv1.FleetZone{
		zrZone("ward-a", "", czRule("camera", fleetv1.ZoneRestrictionDenied, "ward-a", "patient privacy")),
		zrZone("bay-1", ""),
	}
	cands := []zone.Candidate{
		{Name: "ward-a", Floor: 0, Polygon: sq(0, 0, 10), IsLeaf: true, Depth: 1},
		{Name: "bay-1", Floor: 0, Polygon: sq(8, 8, 4), IsLeaf: true, Depth: 1},
	}
	return zones, cands, zone.Position{X: 9, Y: 9, Floor: 0}
}

// bindingAt is the seam these tests are written against: what is in force for a robot
// standing at pos, given the zone objects and their geometry.
func bindingAt(zones []fleetv1.FleetZone, cands []zone.Candidate, pos zone.Position) []fleetv1.EffectiveZoneRestriction {
	return unionEffectiveRestrictions(zones, zone.DeriveContainingZones(pos, cands))
}

// THE BYPASS. A robot inside a zone that denies cameras, whose position happens to be
// nearer another zone's centroid, is bound by the denial.
func TestContainment_ADenialInAnOverlappingZoneBinds(t *testing.T) {
	zones, cands, pos := overlapFixture()

	// The premise: the arbitration really does pick the zone that forbids nothing.
	if got := zone.DeriveCurrentZone(pos, cands); got != "bay-1" {
		t.Fatalf("fixture is not exercising the overlap: currentZone = %q, want bay-1", got)
	}

	inForce := bindingAt(zones, cands, pos)
	var camera *fleetv1.EffectiveZoneRestriction
	for i := range inForce {
		if inForce[i].Subject == "camera" {
			camera = &inForce[i]
		}
	}
	if camera == nil {
		t.Fatalf("in force = %+v, and it does not carry the camera denial. amr-1 is standing "+
			"INSIDE ward-a's polygon, which denies cameras; the nearest-centroid tie-break "+
			"picked bay-1 for the position report and the denial escaped with the loser. FZ-R2 "+
			"says denial is absorbing and FZ-R6 says fail closed — both point at a union, and a "+
			"tie-break written for position reporting was doing safety work it was never "+
			"designed for", inForce)
	}
	if camera.Mode != fleetv1.ZoneRestrictionDenied {
		t.Fatalf("camera resolved to %q, want Denied", camera.Mode)
	}
	// The zone an operator must actually edit is preserved across the union: it is the
	// zone that DECLARED the rule, not whichever containing zone happened to carry it.
	if camera.InheritedFrom != "ward-a" {
		t.Errorf("inheritedFrom = %q, want ward-a — an operator sent to bay-1 would find "+
			"nothing to edit", camera.InheritedFrom)
	}
	if camera.Reason != "patient privacy" {
		t.Errorf("reason = %q, want the operator's own words", camera.Reason)
	}
}

// THE UNION IS MOST-RESTRICTIVE, NOT FIRST-WINS. Two containing zones each bound the
// same subject; the tighter bound is what binds, exactly as it does within one chain.
func TestContainment_TwoBoundsOnOneSubjectIntersect(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("floor", "", fleetv1.EffectiveZoneRestriction{
			Axis: fleetv1.ZoneAxisOperating, Subject: "speed",
			Mode: fleetv1.ZoneRestrictionLimited, Max: "1.5", InheritedFrom: "floor",
		}),
		zrZone("corridor", "", fleetv1.EffectiveZoneRestriction{
			Axis: fleetv1.ZoneAxisOperating, Subject: "speed",
			Mode: fleetv1.ZoneRestrictionLimited, Max: "0.4", InheritedFrom: "corridor",
			Reason: "occupied ward at night",
		}),
	}
	cands := []zone.Candidate{
		{Name: "floor", Floor: 0, Polygon: sq(0, 0, 100), IsLeaf: true},
		{Name: "corridor", Floor: 0, Polygon: sq(0, 0, 10), IsLeaf: true},
	}
	inForce := bindingAt(zones, cands, zone.Position{X: 5, Y: 5})
	if len(inForce) != 1 {
		t.Fatalf("in force = %+v, want one speed rule", inForce)
	}
	if inForce[0].Max != "0.4" {
		t.Fatalf("speed max = %q, want 0.4 — bounds INTERSECT across containing zones for the "+
			"same reason they intersect along a chain: a union that took the looser bound "+
			"would emit something weaker than a rule it read, which FZ-R2 forbids", inForce[0].Max)
	}
	if inForce[0].InheritedFrom != "corridor" {
		t.Errorf("inheritedFrom = %q, want corridor — the zone that supplied the BINDING bound",
			inForce[0].InheritedFrom)
	}
}

// DENIAL ABSORBS ACROSS THE SET. One containing zone denies, another merely bounds:
// the denial wins and the bound is not resurrected.
func TestContainment_DenialAbsorbsAcrossContainingZones(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward", "", czRule("camera", fleetv1.ZoneRestrictionDenied, "ward", "patient privacy")),
		zrZone("bay", "", fleetv1.EffectiveZoneRestriction{
			Axis: fleetv1.ZoneAxisOperating, Subject: "camera",
			Mode: fleetv1.ZoneRestrictionLimited, Max: "30", InheritedFrom: "bay",
		}),
	}
	cands := []zone.Candidate{
		{Name: "ward", Floor: 0, Polygon: sq(0, 0, 10), IsLeaf: true},
		{Name: "bay", Floor: 0, Polygon: sq(0, 0, 10), IsLeaf: true},
	}
	inForce := bindingAt(zones, cands, zone.Position{X: 5, Y: 5})
	if len(inForce) != 1 || inForce[0].Mode != fleetv1.ZoneRestrictionDenied {
		t.Fatalf("in force = %+v, want camera Denied. Denial is ABSORBING (FZ-R2): no union may "+
			"produce a rule more permissive than one it read", inForce)
	}
	if inForce[0].Max != "" {
		t.Errorf("a Denied rule kept a bound (%q); a forbidden subject needs none, and carrying "+
			"one suggests the subject is permitted within it", inForce[0].Max)
	}
}

// REQUIRED SURVIVES THE UNION. The union answers "what capabilities can be active", and
// Required is the inspection-zone mirror case: entering a zone can oblige a subject to be
// ON. A union that only ever forbade would implement half the enum.
func TestContainment_RequiredSurvivesTheUnion(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("terminal-clean", "", czRule("uv", fleetv1.ZoneRestrictionRequired,
			"terminal-clean", "terminal clean; the lamp must be running")),
		zrZone("corridor", ""),
	}
	cands := []zone.Candidate{
		{Name: "terminal-clean", Floor: 0, Polygon: sq(0, 0, 10), IsLeaf: true},
		{Name: "corridor", Floor: 0, Polygon: sq(8, 8, 4), IsLeaf: true},
	}
	inForce := bindingAt(zones, cands, zone.Position{X: 9, Y: 9})
	if len(inForce) != 1 || inForce[0].Mode != fleetv1.ZoneRestrictionRequired {
		t.Fatalf("in force = %+v, want uv Required. A push that only ever forbids implements "+
			"half the enum: entering a zone can oblige a subject to be ON", inForce)
	}
}

// DENIED AND REQUIRED IN TWO DIFFERENT CONTAINING ZONES IS FZ-R3's CONTRADICTION,
// resolved the same way: to Denied. A robot standing where a lamp must be on and must
// be off is standing where no robot may work, and resolving the other way would switch
// a UV lamp on by operation of a merge rule.
func TestContainment_DeniedAndRequiredAcrossZonesResolvesToDenied(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward", "", czRule("uv", fleetv1.ZoneRestrictionDenied, "ward", "occupied")),
		zrZone("clean", "", czRule("uv", fleetv1.ZoneRestrictionRequired, "clean", "terminal clean")),
	}
	cands := []zone.Candidate{
		{Name: "ward", Floor: 0, Polygon: sq(0, 0, 10), IsLeaf: true},
		{Name: "clean", Floor: 0, Polygon: sq(0, 0, 10), IsLeaf: true},
	}
	inForce := bindingAt(zones, cands, zone.Position{X: 5, Y: 5})
	if len(inForce) != 1 || inForce[0].Mode != fleetv1.ZoneRestrictionDenied {
		t.Fatalf("in force = %+v, want uv Denied (FZ-R3 across the containment set)", inForce)
	}
}

// A ZONE WHOSE CHAIN DID NOT RESOLVE CONTRIBUTES NOTHING AND IS NOT SILENTLY SKIPPED.
// FZ-R6: an unresolvable restriction is not an absent restriction. The union reports
// it so the caller can fail closed rather than publish a set that is quietly partial.
func TestContainment_AnUnresolvedContainingZoneIsReportedNotDropped(t *testing.T) {
	unresolved := zrZone("ward", "", czRule("camera", fleetv1.ZoneRestrictionDenied, "ward", "privacy"))
	unresolved.Status.Conditions[0].Status = "False"
	zones := []fleetv1.FleetZone{unresolved, zrZone("bay", "")}

	_, unresolvable := unionEffectiveRestrictionsWithUnresolved(zones, []string{"bay", "ward"})
	if len(unresolvable) != 1 || unresolvable[0] != "ward" {
		t.Fatalf("unresolvable = %v, want [ward]. FZ-R6: an unresolvable restriction is not an "+
			"absent restriction, and a union that silently omits a zone it could not read "+
			"publishes a set that is weaker than the truth without saying so", unresolvable)
	}
}

// THE UNION IS SORTED, for the same reason the chain fold is: the result is compared
// for equality to decide whether anything changed, and an order that varies makes an
// unchanged fleet look like a transition (RA-1).
func TestContainment_TheUnionIsSorted(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("a", "", czRule("water", fleetv1.ZoneRestrictionDenied, "a", "")),
		zrZone("b", "", czRule("camera", fleetv1.ZoneRestrictionDenied, "b", "")),
	}
	forward := unionEffectiveRestrictions(zones, []string{"a", "b"})
	reversed := unionEffectiveRestrictions(zones, []string{"b", "a"})
	if len(forward) != 2 || forward[0].Subject != "camera" || forward[1].Subject != "water" {
		t.Fatalf("union = %+v, want sorted by subject", forward)
	}
	if !reflect.DeepEqual(forward, reversed) {
		t.Fatalf("the order of the containment set changed the result: %+v vs %+v",
			forward, reversed)
	}
}

// AN EMPTY CONTAINMENT SET IS PERMISSION, not "everything is denied" (ADR-0058 §2).
func TestContainment_NoContainingZoneIsPermission(t *testing.T) {
	if got := unionEffectiveRestrictions(nil, nil); got != nil {
		t.Fatalf("union over an empty containment set = %+v, want nil. Absence of a rule is "+
			"permission at every axis, and it stays permission", got)
	}
}
