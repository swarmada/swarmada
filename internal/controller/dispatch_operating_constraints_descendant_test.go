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
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Filter 12's SCOPE: the resolved leaf set, not the target zone chain
// (ADR-0058 Amendment 2; amendment Part B.1).
//
// Filter 2 admits a robot from anywhere in the SUBTREE of the action's zone. A filter
// that read only the target's own projection therefore had a bypass built into it: an
// operating rule declared on a descendant leaf was enforced nowhere, and the operator
// who wrote the rule on the most specific zone — the place inheritedFrom and FZ-R7
// train them to write it — got the weakest enforcement.
//
// The leaf's own projection is the fold of ITSELF AND ITS ANCESTORS (FZ-R1, FZ-R4), so
// evaluating a candidate against the leaf it occupies ALREADY INCLUDES the target
// chain. There is no union to compute and no second walk; a filter that merged two
// rule sets would have reintroduced resolution here, which FZ-R4 forbids.

// inForceAt resolves filter 12's rule set for an action over the action's own resolved
// leaf set, and returns the rules in force AT ONE ZONE of it — which is the slice one
// candidate standing in that zone is judged against.
func inForceAt(action *fleetv1.FleetAction, zones []fleetv1.FleetZone, at string) []fleetv1.EffectiveZoneRestriction {
	return inForceFor(action, zones).at(at)
}

// inForceFor resolves filter 12's whole rule set for an action, over the leaf set
// filter 2 resolves from the same zone list — which is how newDispatchFilterState calls
// it, minus filter 11's narrowing.
func inForceFor(action *fleetv1.FleetAction, zones []fleetv1.FleetZone) *operatingRuleSet {
	return operatingConstraintsInForce(action, zones, resolveZoneLeaves(action.Spec.Zone, zones))
}

// icuZones is ADR-0058 §1's own deciding case, in the shape Amendment 2 §A2.1 tabulates it:
//
//	ward           — declares nothing; effective set empty
//	ward-icu       — leaf, child of ward; declares operating camera: Denied
//
// An action targeting `ward` is admitted by filter 11 (ward denies no entry, and the
// descendant is not entry-denied, so no narrowing occurs). Filter 2 admits a robot in
// ward-icu, because ward-icu is in the resolved leaf set.
func icuZones() []fleetv1.FleetZone {
	return []fleetv1.FleetZone{
		zrZone("ward", ""),
		zrZone("ward-icu", "ward",
			operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward-icu", "patient privacy")),
	}
}

// THE ICU CASE. A robot is dispatched into the ICU with a camera.
func TestFilter12_ARuleOnADescendantLeafIsEnforcedAgainstTheRobotDispatchedIntoIt(t *testing.T) {
	zones := icuZones()
	action := zrAction("ward")

	// Nothing is in force at ward itself: ward declares nothing and inherits nothing.
	// That is correct and is not the bug — the bug was stopping there.
	if got := inForceAt(action, zones, "ward"); len(got) != 0 {
		t.Fatalf("in force at ward = %+v, want nothing: ward declares no rule and has no ancestor "+
			"that does. A leaf's rule must NOT be folded upward into its parent's projection — "+
			"that would be a union, and a union makes a large target zone undispatchable for a "+
			"rule on one closet", got)
	}

	// ward-icu is where the robot actually is, and its projection carries its own rule.
	inICU := inForceAt(action, zones, "ward-icu")
	if len(inICU) != 1 || inICU[0].Subject != "camera" {
		t.Fatalf("in force at ward-icu = %+v, want the one camera rule. Filter 2 admits a robot "+
			"from ward-icu because it is in the resolved leaf set of ward; a filter 12 that reads "+
			"only ward's projection is a rule with a bypass, which is the thing ADR-0058 §5 "+
			"exists to prevent", inICU)
	}

	if ex := excludedByOperatingConstraint(f12Robot("amr-1"), inICU); ex == nil {
		t.Fatal("amr-1 sits in ward-icu, which denies a camera, and its adapter declares NO " +
			"operating subjects — and it was admitted as a candidate for an action targeting " +
			"ward. A ROBOT IS DISPATCHED INTO THE ICU WITH A CAMERA. That is ADR-0058 §1's own " +
			"deciding case, and it is the defect ADR-0058 Amendment 2 §A2.1 records: filter 12 " +
			"scoped to the " +
			"target zone chain never sees a rule declared BELOW the target, while filter 2 " +
			"admits robots from the whole subtree")
	}
}

// A LEAF DROPS OUT FOR ONE CANDIDATE AND IS KEPT FOR ANOTHER, and that is the whole
// difference from filter 11's narrowing.
//
// Filter 11's narrowing is ZONE-SCOPED: an entry-denied leaf is out for everyone, and
// the zone is removed from the leaf set. Filter 12's is not. A leaf leaves the reachable
// set only when NO candidate can honour it; a leaf one candidate can serve stays, and
// the action dispatches to that candidate. A filter 12 that removed the leaf outright
// would be filter 11 with extra steps.
func TestFilter12_ALeafDroppedForOneCandidateIsKeptForAnother(t *testing.T) {
	zones := icuZones()
	inICU := inForceAt(zrAction("ward"), zones, "ward-icu")

	if ex := excludedByOperatingConstraint(f12Robot("amr-1"), inICU); ex == nil {
		t.Fatal("amr-1 declares nothing and was admitted into ward-icu")
	}
	if ex := excludedByOperatingConstraint(f12Robot("amr-2", "camera"), inICU); ex != nil {
		t.Fatalf("amr-2 DECLARES camera and was still withheld from ward-icu: %+v. Filter 12 "+
			"narrows per (candidate, leaf), never per zone: a leaf drops out of the reachable "+
			"set only when NO candidate can honour it. Removing the leaf for everyone because "+
			"one robot cannot serve it would be filter 11 with extra steps", ex)
	}
}

// NOT A UNION. A rule on one leaf must not withhold a candidate standing in a sibling
// leaf that does not carry it.
//
// This is the property the rejected first option destroys. Unioning the target's
// projection with every leaf's is fail-closed and cheap, and it makes a large target
// zone undispatchable because of a rule on one closet — which also destroys the
// property filter 11 is careful to preserve: "the zone is NOT CLOSED".
func TestFilter12_ARuleOnOneLeafDoesNotWithholdACandidateInASibling(t *testing.T) {
	zones := append(icuZones(), zrZone("ward-store", "ward"))
	action := zrAction("ward")

	inStore := inForceAt(action, zones, "ward-store")
	if len(inStore) != 0 {
		t.Fatalf("in force at ward-store = %+v, want nothing: ward-store declares no rule and "+
			"its ancestor ward declares none either. A camera rule on the sibling ward-icu is "+
			"NOT in force here", inStore)
	}
	if ex := excludedByOperatingConstraint(f12Robot("amr-3"), inStore); ex != nil {
		t.Fatalf("a robot in ward-store was withheld by a rule declared on its sibling "+
			"ward-icu: %+v. That is the union option, rejected on the record: it makes a large "+
			"target zone undispatchable for a rule on one closet and destroys the property "+
			"filter 11 is careful to preserve — the zone is NOT CLOSED", ex)
	}
}

// The projection is read, never merged. A leaf that declares nothing under a target
// that DOES declare carries the target's rule in its own effective set already, with
// inheritedFrom naming the declaring zone — so the target chain needs no code here and
// no second rule set to fold in (FZ-R4; FZ-R9 names the drift a filter-local resolver
// produces).
func TestFilter12_TheLeafProjectionAlreadyCarriesTheTargetChain(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward", "", operatingRule("microphone", fleetv1.ZoneRestrictionDenied, "ward", "patient privacy")),
		// ward-icu declares NOTHING of its own; the Zone Controller folded ward's rule in.
		zrZone("ward-icu", "ward", operatingRule("microphone", fleetv1.ZoneRestrictionDenied, "ward", "patient privacy")),
	}
	inICU := inForceAt(zrAction("ward"), zones, "ward-icu")
	if len(inICU) != 1 || inICU[0].InheritedFrom != "ward" {
		t.Fatalf("in force at ward-icu = %+v, want the microphone rule inherited from ward. The "+
			"leaf's effective set is the fold of ITSELF AND ITS ANCESTORS (FZ-R1, FZ-R4), so "+
			"evaluating against the leaf already includes the target chain; merging two rule "+
			"sets here would reintroduce resolution in the filter", inICU)
	}
	ex := excludedByOperatingConstraint(f12Robot("amr-1"), inICU)
	if ex == nil {
		t.Fatal("a rule the target declared and the leaf inherited withheld nobody")
	}
	if !strings.Contains(ex.Reason, "ward") {
		t.Errorf("reason %q does not name the DECLARING zone", ex.Reason)
	}
}
