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

// Candidate filter 12 — adapter operating-constraint support (ADR-0058 §5, §6;
// amendment Part B.1).
//
// ROBOT-SCOPED, and that is the difference from filter 11 beside it. Filter 11 asks "may this
// action happen here at all" and eliminates the ACTION, terminally. Filter 12 asks "can THIS
// robot honour what this zone requires" and eliminates a CANDIDATE — and the action stays
// Pending, because the answer is a fact about today's fleet: a conforming adapter deployed
// tomorrow makes the same action dispatchable.
//
// These tests are pure, matching filter 11's: the decision is a function of an already-listed
// zone set and one robot, with no client.

// f12Robot builds a candidate declaring the given operating subjects.
func f12Robot(name string, honoured ...string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: "sim-adapter"}},
		Status:     fleetv1.RobotStatus{HonouredOperatingConstraints: honoured},
	}
}

// operatingRule builds one resolved axis-3 rule.
func operatingRule(subject string, mode fleetv1.ZoneRestrictionMode, from, reason string) fleetv1.EffectiveZoneRestriction {
	return fleetv1.EffectiveZoneRestriction{
		Axis: fleetv1.ZoneAxisOperating, Subject: subject,
		Mode: mode, InheritedFrom: from, Reason: reason,
	}
}

// ---------------------------------------------------------------------------
// L0's ruling: absence of a declaration is NOT participation.
// ---------------------------------------------------------------------------

// ADR-0058 Amendment 1 §A1.2, which is normative for this filter and is implemented here
// rather than re-decided:
//
//	"Candidate filter 12 withholds a candidate robot when, for any effective operating rule in
//	 force in the target zone chain, the robot's status.honouredOperatingConstraints[] does not
//	 contain that rule's subject. An empty or absent honouredOperatingConstraints[] satisfies no
//	 subject."
//
// The refused reading — treat an adapter that declares nothing as "not participating, so
// dispatch" — is the permissive resolution of an unresolvable case, and it would dispatch
// precisely the robot ADR-0058 §1 exists for: the one transiting a ward with a live microphone
// under an adapter that models no operating subjects at all.
func TestFilter12_AnAdapterThatDeclaresNothingIsWithheldNotPermitted(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward-3", "", operatingRule("microphone", fleetv1.ZoneRestrictionDenied, "ward-3", "patient privacy")),
	}
	inForce := inForceAt(zrAction("ward-3"), zones, "ward-3")
	if len(inForce) != 1 {
		t.Fatalf("the target's effective operating set = %+v, want the one microphone rule", inForce)
	}

	// Absent.
	if ex := excludedByOperatingConstraint(f12Robot("amr-1"), inForce); ex == nil {
		t.Fatal("a robot whose adapter declared NOTHING was admitted into a zone that forbids a " +
			"live microphone. ADR-0058 Amendment 1 §A1.2: an empty or absent " +
			"honouredOperatingConstraints[] satisfies no subject, and an adapter that declares " +
			"nothing is withheld on the same footing as one that declares other subjects and not " +
			"this one. The permissive reading dispatches exactly the robot §1 exists for")
	}
	// Explicitly empty, which is a different Go value and must not be a different answer.
	if ex := excludedByOperatingConstraint(f12Robot("amr-2", []string{}...), inForce); ex == nil {
		t.Fatal("an EMPTY declaration was admitted; empty and absent must be the same answer")
	}
}

func TestFilter12_ARobotDeclaringTheSubjectIsACandidate(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward-3", "", operatingRule("microphone", fleetv1.ZoneRestrictionDenied, "ward-3", "patient privacy")),
	}
	inForce := inForceAt(zrAction("ward-3"), zones, "ward-3")
	if ex := excludedByOperatingConstraint(f12Robot("amr-1", "microphone", "camera"), inForce); ex != nil {
		t.Fatalf("a robot whose adapter DECLARES microphone was withheld: %+v. Filter 12 withholds "+
			"only what cannot be shown to be honoured; withholding a declaring robot would make "+
			"the axis unusable by every fleet", ex)
	}
}

// ---------------------------------------------------------------------------
// The exclusion has to be actionable: which subject, and which zone declared it.
// ---------------------------------------------------------------------------

func TestFilter12_TheExclusionNamesTheSubjectAndTheDeclaringZone(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("site", "", operatingRule("camera", fleetv1.ZoneRestrictionDenied, "site", "patient privacy")),
		zrZone("ward-3", "site", operatingRule("camera", fleetv1.ZoneRestrictionDenied, "site", "patient privacy")),
	}
	inForce := inForceAt(zrAction("ward-3"), zones, "ward-3")
	ex := excludedByOperatingConstraint(f12Robot("amr-1", "speed"), inForce)
	if ex == nil {
		t.Fatal("a robot declaring only speed was admitted into a zone that denies camera")
	}
	if ex.Filter != filterOperatingConstraint {
		t.Errorf("exclusion reports filter %d, want %d", ex.Filter, filterOperatingConstraint)
	}
	for _, want := range []string{"camera", "site", "patient privacy"} {
		if !strings.Contains(ex.Reason, want) {
			t.Errorf("reason %q does not name %q. An operator must be able to tell WHICH SUBJECT "+
				"and WHICH ZONE declared it, or they go hunting for a setting — and the declaring "+
				"zone is very often not the target", ex.Reason, want)
		}
	}
	// What the robot COULD honour, so the gap is legible rather than inferred.
	if !strings.Contains(ex.Reason, "speed") {
		t.Errorf("reason %q does not say what the robot CAN honour; the operator needs the gap, "+
			"not just the requirement", ex.Reason)
	}
}

// A partial declaration is withheld for the MISSING subject only, and the message says which.
func TestFilter12_APartialDeclarationIsWithheldForTheSubjectItLacks(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward-3", "",
			operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward-3", "patient privacy"),
			operatingRule("microphone", fleetv1.ZoneRestrictionDenied, "ward-3", "patient privacy"),
		),
	}
	inForce := inForceAt(zrAction("ward-3"), zones, "ward-3")
	ex := excludedByOperatingConstraint(f12Robot("amr-1", "camera"), inForce)
	if ex == nil {
		t.Fatal("a robot declaring camera but not microphone was admitted; EVERY subject in force " +
			"must be declared, not any one of them")
	}
	if !strings.Contains(ex.Reason, "microphone") {
		t.Errorf("reason %q does not name the missing subject", ex.Reason)
	}
}

// Limited and Required are modes on this axis and only on this axis, and both require the
// declaration: an adapter that cannot control the subject at all cannot hold it under a bound,
// and cannot keep it running either.
func TestFilter12_LimitedAndRequiredAlsoNeedTheDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode fleetv1.ZoneRestrictionMode
	}{
		{"limited", fleetv1.ZoneRestrictionLimited},
		{"required", fleetv1.ZoneRestrictionRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zones := []fleetv1.FleetZone{
				zrZone("ward-3", "", operatingRule("speed", tc.mode, "ward-3", "occupied ward at night")),
			}
			inForce := inForceAt(zrAction("ward-3"), zones, "ward-3")
			if ex := excludedByOperatingConstraint(f12Robot("amr-1", "camera"), inForce); ex == nil {
				t.Fatalf("a %s rule on speed admitted a robot that does not declare speed. An "+
					"adapter that cannot control a subject at all can neither hold it under a "+
					"bound nor keep it running", tc.mode)
			}
			if ex := excludedByOperatingConstraint(f12Robot("amr-2", "speed"), inForce); ex != nil {
				t.Fatalf("a robot declaring speed was withheld under a %s rule: %+v", tc.mode, ex)
			}
		})
	}
}

// An extension subject is a subject. Treating one the robot did not declare as absent would make
// the namespaced form a universal bypass (ADR-0058 §7).
func TestFilter12_AnExtensionSubjectIsNotABypass(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("bay-1", "", operatingRule("example.com/leaf-blower", fleetv1.ZoneRestrictionDenied, "bay-1", "glass roof")),
	}
	inForce := inForceAt(zrAction("bay-1"), zones, "bay-1")
	if ex := excludedByOperatingConstraint(f12Robot("amr-1", "camera", "microphone", "water",
		"chemical", "uv", "speed", "noise"), inForce); ex == nil {
		t.Fatal("a robot declaring all seven CORE subjects was admitted under an EXTENSION rule it " +
			"never declared. An unrecognised subject is not an absent subject; treating it as one " +
			"would let any site silence a constraint by namespacing it")
	}
}

// ---------------------------------------------------------------------------
// The inert gate. Without it, every robot served by an adapter that has not declared
// anything — which today is every adapter in existence — is withheld the moment any
// zone anywhere declares an operating rule.
// ---------------------------------------------------------------------------

func TestFilter12_IsInertInANamespaceThatDeclaresNoRestrictions(t *testing.T) {
	// Zones with geometry and hierarchy, and no restriction block anywhere.
	zones := []fleetv1.FleetZone{zrZone("ward-3", ""), zrZone("bay-2", "ward-3")}
	if got := inForceFor(zrAction("ward-3"), zones); got != nil {
		t.Fatalf("filter 12 found %+v rules in force in a namespace that declares NO restriction. "+
			"It must be inert there: otherwise every robot whose adapter has not declared "+
			"anything — which today is every adapter in existence — is withheld the moment any "+
			"zone anywhere declares an operating rule. That is the permanent lockout K7 refused "+
			"to ship, arriving through this filter instead", got)
	}
}

// The gate is on RESTRICTIONS IN USE, not on operating rules specifically: a namespace that
// declares only entry and capability rules puts no operating subject in force, so filter 12
// still withholds nobody.
func TestFilter12_IsInertWhenTheChainCarriesNoOperatingRule(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward-3", "", entryDenied("ward-3", "closed"), capDenied("clean.mop", "ward-3", "carpet")),
	}
	if got := inForceFor(zrAction("ward-3"), zones); got != nil {
		t.Fatalf("filter 12 found %+v in force where only entry and capability rules exist", got)
	}
	if ex := excludedByOperatingConstraint(f12Robot("amr-1"), nil); ex != nil {
		t.Fatalf("a robot was withheld with no operating rule in force: %+v", ex)
	}
}

func TestFilter12_AnActionNamingNoZoneIsUntouched(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward-3", "", operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward-3", "privacy")),
	}
	if got := inForceFor(zrAction(""), zones); got != nil {
		t.Fatalf("an action naming no zone put %+v in force; it never reached filter 2 either", got)
	}
}

// ---------------------------------------------------------------------------
// Robot-scoped means exactly that: the SAME robot is withheld here and a candidate
// there. Filter 11 could never produce this, which is why both filters exist.
// ---------------------------------------------------------------------------

func TestFilter12_ARobotWithheldForOneZoneIsStillACandidateInAnother(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("ward-3", "", operatingRule("microphone", fleetv1.ZoneRestrictionDenied, "ward-3", "patient privacy")),
		zrZone("loading-bay", "", operatingRule("noise", fleetv1.ZoneRestrictionLimited, "loading-bay", "night shift")),
	}
	robot := f12Robot("amr-1", "noise")

	inWard := inForceAt(zrAction("ward-3"), zones, "ward-3")
	if ex := excludedByOperatingConstraint(robot, inWard); ex == nil {
		t.Fatal("amr-1 declares noise and not microphone; it must be withheld from ward-3")
	}
	inBay := inForceAt(zrAction("loading-bay"), zones, "loading-bay")
	if ex := excludedByOperatingConstraint(robot, inBay); ex != nil {
		t.Fatalf("the SAME robot was withheld from loading-bay, whose only rule is noise — which it "+
			"declares: %+v. Filter 12 is ROBOT-SCOPED and zone-specific; a robot withheld for one "+
			"subject stays a candidate everywhere that subject is not in force. A filter that "+
			"withheld globally would be filter 11 with extra steps", ex)
	}
}

// An inherited rule needs no code here: the target's own projection already carries an
// ancestor's rule with inheritedFrom naming it (FZ-R4). A filter that walked the tree would
// drift to nearest-ancestor-wins, which FZ-R9 names as the specific failure mode.
func TestFilter12_ReadsTheProjectionSoInheritanceNeedsNoCode(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("site", "", operatingRule("camera", fleetv1.ZoneRestrictionDenied, "site", "site-wide privacy policy")),
		// bay-7 declares NOTHING of its own; the Zone Controller folded the ancestor's
		// rule into its projection with inheritedFrom = site.
		zrZone("bay-7", "site", operatingRule("camera", fleetv1.ZoneRestrictionDenied, "site", "site-wide privacy policy")),
	}
	inForce := inForceAt(zrAction("bay-7"), zones, "bay-7")
	if len(inForce) != 1 || inForce[0].InheritedFrom != "site" {
		t.Fatalf("in force at bay-7 = %+v, want the camera rule inherited from site", inForce)
	}
	ex := excludedByOperatingConstraint(f12Robot("amr-1"), inForce)
	if ex == nil {
		t.Fatal("a rule a leaf inherited but did not declare withheld nobody")
	}
	if !strings.Contains(ex.Reason, "site") {
		t.Errorf("reason %q does not name the DECLARING zone; an operator looking at bay-7 needs "+
			"to know to edit site, and FZ-R7 may mean they cannot weaken it from bay-7 at all",
			ex.Reason)
	}
}

// The one case where the inert gate is load-bearing rather than redundant.
//
// Filter 12's inertness is carried mostly by reading the TARGET'S OWN PROJECTION: a zone with no
// operating rule in its effective set puts nothing in force. Measured — removing
// namespaceUsesRestrictions from operatingConstraintsInForce breaks zero existing tests, where
// removing it from filter 11 broke twelve.
//
// What the gate covers is the STALE PROJECTION window: an operator deletes spec.restrictions and
// the Zone Controller has not re-reconciled, so status.restrictions still carries the withdrawn
// rule. Without the gate, a rule the operator has already removed keeps withholding robots until
// the controller catches up. ADR-0058 §11 is explicit that a principal who may write the zone may
// remove the wall at its source.
//
// This is the test that stops the gate being an unexercised line — which is the defect class this
// repository keeps paying for.
func TestFilter12_AStaleProjectionWithNoSpecBehindItWithholdsNobody(t *testing.T) {
	stale := zrZone("ward-3", "", operatingRule("camera", fleetv1.ZoneRestrictionDenied, "ward-3", "patient privacy"))
	// The operator has deleted the rule; the projection has not caught up.
	stale.Spec.Restrictions = nil

	inForce := inForceFor(zrAction("ward-3"), []fleetv1.FleetZone{stale})
	if inForce != nil {
		t.Fatalf("a WITHDRAWN rule was still in force from a stale projection: %+v. spec is what "+
			"says the feature is in use; without the namespaceUsesRestrictions gate a rule the "+
			"operator has already deleted keeps withholding robots until the Zone Controller "+
			"re-reconciles", inForce)
	}
	if ex := excludedByOperatingConstraint(f12Robot("amr-1"), inForce.at("ward-3")); ex != nil {
		t.Fatalf("a robot was withheld by a withdrawn rule: %+v", ex)
	}
}
