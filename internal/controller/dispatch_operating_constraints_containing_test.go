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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Filter 12 over the CONTAINMENT SET (FZ-R10, FZ-R11; amendment Part B.1, filter 12).
//
// A robot is bound by the operating rules of EVERY zone whose polygon contains it, not only
// the one arbitrated onto status.currentZone. currentZone answers "where does it report it
// is"; status.containingZones[] answers "what binds it". Judging a candidate at the winner
// alone discards the loser's rules, which is the permissive direction, and FZ-R10 says in
// terms that the winner alone does not conform.
//
// The leaf's own projection still needs no merge with the TARGET chain — it already folds
// every ancestor. What is folded in here is the robot's OTHER containing zones, each an
// already-resolved projection, by the same fold the FA-R7 push uses (FZ-R11).
//
// These drive st.excludedBy, the seam filterDispatchEligible calls, so the rows read the
// same state the dispatch path builds.

// corridorZones is a ward whose one leaf, ward-bay, is crossed by a sterile corridor that
// belongs to a DIFFERENT tree:
//
//	ward        — the action's target; declares nothing
//	ward-bay    — leaf under ward; declares bayRules
//	corridor    — top-level, overlapping ward-bay; declares operating microphone: Denied
//
// A robot standing where the two overlap reports currentZone ward-bay (the arbitrated
// winner) and containingZones [corridor, ward-bay].
func corridorZones(bayRules ...fleetv1.EffectiveZoneRestriction) []fleetv1.FleetZone {
	return []fleetv1.FleetZone{
		zrZone("ward", ""),
		zrZone("ward-bay", "ward", bayRules...),
		zrZone("corridor", "",
			operatingRule("microphone", fleetv1.ZoneRestrictionDenied, "corridor", "sterile corridor")),
	}
}

// containingState builds the filter state newDispatchFilterState builds for an action over an
// already-listed zone set, minus filter 11's narrowing.
func containingState(action *fleetv1.FleetAction, zones []fleetv1.FleetZone) *dispatchFilterState {
	leaves := resolveZoneLeaves(action.Spec.Zone, zones)
	return &dispatchFilterState{zoneLeaves: leaves, operating: operatingConstraintsInForce(action, zones, leaves)}
}

// standingIn places a candidate at its arbitrated zone and inside every zone of containing.
func standingIn(robot *fleetv1.Robot, current string, containing ...string) *fleetv1.Robot {
	robot.Status.CurrentZone = current
	robot.Status.ContainingZones = containing
	return robot
}

// THE ITEM'S OWN ROW. The robot's adapter honours the rule at its leaf and not the rule of the
// second zone containing it; it is withheld, by filter 12, naming that zone's subject.
func TestFilter12_TheSecondContainingZonesRuleWithholdsTheRobot(t *testing.T) {
	zones := corridorZones(operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward-bay", "patient privacy"))
	action := zrAction("ward")
	robot := standingIn(f12Robot("amr-1", "camera"), "ward-bay", "corridor", "ward-bay")

	ex := containingState(action, zones).excludedBy(robot, action)
	if ex == nil {
		t.Fatalf("a robot inside corridor, which denies microphone, was admitted by an adapter that " +
			"declares only camera. Filter 12 MUST judge every operating rule in force at its leaf OR " +
			"IN ANY ZONE OF status.containingZones[]; judging status.currentZone alone discards the " +
			"loser's rules, and FZ-R10 says the winner alone does not conform")
	}
	if ex.Filter != filterOperatingConstraint {
		t.Fatalf("withheld by filter %d (%s), want filter 12", ex.Filter, ex.Reason)
	}
	for _, want := range []string{"microphone", `"corridor"`, "sterile corridor"} {
		if !strings.Contains(ex.Reason, want) {
			t.Errorf("exclusion %q does not name %s — the operator must be sent to the zone that "+
				"declares the rule, which here is not the robot's currentZone", ex.Reason, want)
		}
	}
	if wants, _, _ := strings.Cut(ex.Reason, "; its adapter"); strings.Contains(wants, "camera") {
		t.Errorf("exclusion %q asks for camera, which the adapter declared", ex.Reason)
	}
}

// The reachable leaf set carries NO operating rule at all, so the old inert floor returned a
// nil set and nobody was judged. A rule in a containing zone outside the action's subtree
// still binds the robot standing in it.
func TestFilter12_AContainingZoneOutsideTheSubtreeBindsWhenTheLeafDeclaresNothing(t *testing.T) {
	zones := corridorZones()
	action := zrAction("ward")
	robot := standingIn(f12Robot("amr-1"), "ward-bay", "corridor", "ward-bay")

	ex := containingState(action, zones).excludedBy(robot, action)
	if ex == nil || ex.Filter != filterOperatingConstraint {
		t.Fatalf("exclusion = %+v, want filter 12 for microphone: corridor contains the robot and "+
			"denies microphone, and an adapter that declares nothing satisfies no subject "+
			"(ADR-0058 Amendment 1 §A1.2)", ex)
	}
}

// Not over-withholding: the adapter honours both subjects, so the robot is a candidate.
func TestFilter12_ARobotHonouringEveryContainingZonesRuleIsACandidate(t *testing.T) {
	zones := corridorZones(operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward-bay", "patient privacy"))
	action := zrAction("ward")
	robot := standingIn(f12Robot("amr-1", "camera", "microphone"), "ward-bay", "corridor", "ward-bay")

	if ex := containingState(action, zones).excludedBy(robot, action); ex != nil {
		t.Fatalf("a robot whose adapter declares every subject in force where it stands was "+
			"withheld: %+v", ex)
	}
}

// A robot NOT inside the corridor is untouched by the corridor's rule: containment is the set
// the robot is in, not every zone in the namespace.
func TestFilter12_AZoneThatDoesNotContainTheRobotDoesNotBindIt(t *testing.T) {
	zones := corridorZones(operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward-bay", "patient privacy"))
	action := zrAction("ward")
	robot := standingIn(f12Robot("amr-1", "camera"), "ward-bay", "ward-bay")

	if ex := containingState(action, zones).excludedBy(robot, action); ex != nil {
		t.Fatalf("a robot outside corridor was withheld for corridor's rule: %+v. Containment is "+
			"the set the robot stands in; a union over the namespace would make every robot answer "+
			"for every zone", ex)
	}
}

// A PINNED robot standing outside the subtree is judged at the target's chain (the floor
// at() falls back to) AND at every zone containing it.
func TestFilter12_APinnedRobotOutsideTheSubtreeIsJudgedAtItsContainingZonesToo(t *testing.T) {
	zones := append(corridorZones(), zrZone("lab", ""))
	zones[0] = zrZone("ward", "", operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward", "patient privacy"))
	action := zrAction("ward")
	action.Spec.RobotSelector = "amr-1"
	robot := standingIn(f12Robot("amr-1", "camera"), "lab", "corridor", "lab")

	ex := containingState(action, zones).excludedBy(robot, action)
	if ex == nil || ex.Filter != filterOperatingConstraint || !strings.Contains(ex.Reason, "microphone") {
		t.Fatalf("exclusion = %+v, want filter 12 naming microphone: a pin skips filter 2, never "+
			"filter 12, and the rules of the zones containing the robot bind it wherever it stands", ex)
	}
}

// FZ-R6 on the dispatch path: a containing zone whose rules cannot be read is not a zone with
// no rules. The candidate is withheld, by filter 12, naming the zone.
func TestFilter12_AnUnresolvableContainingZoneWithholdsTheRobot(t *testing.T) {
	zones := corridorZones(operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward-bay", "patient privacy"))
	zones[2].Status.Conditions = []metav1.Condition{{
		Type: fleetv1.ZoneCondRestrictionsResolved, Status: metav1.ConditionFalse,
		Reason: "ChainUnresolvable", Message: "ancestor chain does not resolve", LastTransitionTime: metav1.Now(),
	}}
	action := zrAction("ward")
	robot := standingIn(f12Robot("amr-1", "camera", "microphone"), "ward-bay", "corridor", "ward-bay")

	ex := containingState(action, zones).excludedBy(robot, action)
	if ex == nil || ex.Filter != filterOperatingConstraint || !strings.Contains(ex.Reason, `"corridor"`) {
		t.Fatalf("exclusion = %+v, want filter 12 naming corridor: its restriction set could not be "+
			"resolved, and an unresolvable restriction is not an absent one (FZ-R6). Dropping it from "+
			"the fold would judge the robot against a set weaker than the truth", ex)
	}
}
