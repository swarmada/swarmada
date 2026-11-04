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

// Candidate filter 11 — zone restriction admissibility (ADR-0058 §5, amendment Part B.1).
//
// ACTION-SCOPED, and that is what makes it unlike every other filter in this package.
// Its answer does not vary by robot, so it does not eliminate a candidate: it
// eliminates the ACTION, terminally. A `Pending` requeue would report a standing,
// permanent refusal identically to a momentarily busy fleet — "you told me never to go
// there" reading the same as "everything is busy" — and that is precisely the
// confusion the downstream adapter's refusal wording exists to prevent.
//
// These tests are pure: the verdict function takes a zone list and an action and
// returns a decision, with no client. That is deliberate — filter 11's whole content
// is a resolution over already-listed objects, and the reads it needs are tested
// separately where the fail-closed behaviour lives.

// zrZone builds a zone carrying the given RESOLVED restrictions.
//
// It also mirrors into spec.restrictions any effective entry this zone DECLARED
// (inheritedFrom == its own name), because that is the only way such an entry could
// exist: the projection is a fold of the chain's specs. Filter 11 is inert in a
// namespace where no zone declares anything, so a fixture with a projection and no
// spec anywhere is not a zone the control plane can produce.
func zrZone(name, parent string, effective ...fleetv1.EffectiveZoneRestriction) fleetv1.FleetZone {
	var declared *fleetv1.ZoneRestrictions
	for _, e := range effective {
		if e.InheritedFrom != name {
			continue
		}
		if declared == nil {
			declared = &fleetv1.ZoneRestrictions{}
		}
		switch e.Axis {
		case fleetv1.ZoneAxisEntry:
			declared.Entry = &fleetv1.ZoneEntryRestriction{Mode: e.Mode, Reason: e.Reason}
		case fleetv1.ZoneAxisCapability:
			declared.Capabilities = append(declared.Capabilities,
				fleetv1.ZoneCapabilityRestriction{Name: e.Subject, Mode: e.Mode, Reason: e.Reason})
		case fleetv1.ZoneAxisOperating:
			declared.Operating = append(declared.Operating,
				fleetv1.ZoneOperatingRestriction{Subject: e.Subject, Mode: e.Mode, Reason: e.Reason})
		}
	}
	fz := fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent, Restrictions: declared},
		Status: fleetv1.FleetZoneStatus{
			Restrictions: &fleetv1.ZoneRestrictionsStatus{Effective: effective},
			Conditions: []metav1.Condition{{
				Type: fleetv1.ZoneCondRestrictionsResolved, Status: metav1.ConditionTrue,
				Reason: "Resolved", LastTransitionTime: metav1.Now(),
			}},
		},
	}
	return fz
}

// zrUnresolved marks a zone's chain unresolvable (FZ-R6).
func zrUnresolved(fz fleetv1.FleetZone, why string) fleetv1.FleetZone {
	fz.Status.Conditions = []metav1.Condition{{
		Type: fleetv1.ZoneCondRestrictionsResolved, Status: metav1.ConditionFalse,
		Reason: "AncestorChainUnresolvable", Message: why, LastTransitionTime: metav1.Now(),
	}}
	return fz
}

func entryDenied(from, reason string) fleetv1.EffectiveZoneRestriction {
	return fleetv1.EffectiveZoneRestriction{
		Axis: fleetv1.ZoneAxisEntry, Mode: fleetv1.ZoneRestrictionDenied,
		InheritedFrom: from, Reason: reason,
	}
}

func capDenied(capability, from, reason string) fleetv1.EffectiveZoneRestriction {
	return fleetv1.EffectiveZoneRestriction{
		Axis: fleetv1.ZoneAxisCapability, Subject: capability,
		Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: from, Reason: reason,
	}
}

func zrAction(zone string, caps ...string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "act", Namespace: "ns"},
		Spec: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeNavigate, Zone: zone, RequiredCapabilities: caps,
		},
	}
}

// ---------------------------------------------------------------------------
// ZoneUnknown — a target naming no FleetZone is refused, not dispatched.
// ---------------------------------------------------------------------------

func TestFilter11_UnknownZoneIsRefusedAndEnumeratesTheZonesThatExist(t *testing.T) {
	// A namespace that USES restrictions: ward-3 denies a camera. Without that,
	// filter 11 is inert and an unknown zone name behaves as it did before ADR-0058.
	zones := []fleetv1.FleetZone{
		zrZone("ward-3", "", fleetv1.EffectiveZoneRestriction{
			Axis: fleetv1.ZoneAxisOperating, Subject: "camera",
			Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "ward-3",
		}),
		zrZone("bay-2", "ward-3"),
	}

	v := evaluateZoneRestrictions(zrAction("stockroom"), zones)
	if v == nil || !v.refuse {
		t.Fatalf("an action naming a zone that does not exist was not refused: %+v\n"+
			"ADR-0058 §6: resolving an unknown zone to itself was right when the only question "+
			"was containment, and is wrong once a zone name carries a restriction set — a name "+
			"that resolves to nothing is a name whose restrictions cannot be evaluated. "+
			"Unknown is not permitted", v)
	}
	if v.reason != fleetv1.ActionFailureZoneUnknown {
		t.Errorf("failureReason = %q, want %q", v.reason, fleetv1.ActionFailureZoneUnknown)
	}
	if !v.terminal {
		t.Error("ZoneUnknown must be terminal: no retry and no fleet change satisfies a name that does not exist")
	}
	// "Ambiguity asks; it never guesses" — the refusal enumerates what does exist.
	for _, want := range []string{"stockroom", "ward-3", "bay-2"} {
		if !strings.Contains(v.message, want) {
			t.Errorf("message %q does not name %q; the refusal must carry the name as written "+
				"AND an enumeration of the zones that exist", v.message, want)
		}
	}
}

func TestFilter11_AnActionNamingNoZoneIsUntouched(t *testing.T) {
	// Zone-less actions never reached filter 2 either. A namespace with no zone
	// geometry must keep dispatching exactly as before.
	if v := evaluateZoneRestrictions(zrAction(""), nil); v != nil && v.refuse {
		t.Errorf("an action naming no zone was refused: %+v", v)
	}
}

// ---------------------------------------------------------------------------
// ZoneEntryDenied — absorbing over the target relation, and inherited.
// ---------------------------------------------------------------------------

func TestFilter11_EntryDeniedOnTheTargetRefusesTerminally(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("patio", "", entryDenied("patio", "outdoors behind the fence; no robot access")),
	}
	v := evaluateZoneRestrictions(zrAction("patio"), zones)
	if v == nil || !v.refuse {
		t.Fatal("an action targeting a zone that denies entry was not refused")
	}
	if v.reason != fleetv1.ActionFailureZoneEntryDenied {
		t.Errorf("failureReason = %q, want %q", v.reason, fleetv1.ActionFailureZoneEntryDenied)
	}
	if !v.terminal {
		t.Error("ZoneEntryDenied must be TERMINAL. A Pending requeue would report a standing, " +
			"permanent refusal identically to a momentarily busy fleet")
	}
	for _, want := range []string{"patio", "outdoors behind the fence"} {
		if !strings.Contains(v.message, want) {
			t.Errorf("message %q does not carry %q — the declaring zone and the operator's reason "+
				"are what a person standing in a room reads", v.message, want)
		}
	}
	// The absorbing property, restated: the refusal holds whether the zone is the
	// target or is reached through an ancestor.
	if !strings.Contains(strings.ToLower(v.message), "on its own or as part of anything else") {
		t.Errorf("message %q does not state the absorbing property. The adapter's shipped "+
			"wording carries it verbatim and ADR-0058 §9 carries it over: the refusal holds "+
			"whether the zone is targeted directly or reached through an ancestor", v.message)
	}
}

// THE INHERITANCE TEST. §3: the effective rule is the most restrictive in the
// containment chain, down parentZone edges. A leaf with NO rule of its own, under a
// parent that denies, is denied.
func TestFilter11_LeafWithNoRuleUnderADenyingParentIsDenied(t *testing.T) {
	zones := []fleetv1.FleetZone{
		// hospital is the only zone that DECLARES anything. ward-3 and bay-2 declare
		// nothing; their projections carry the denial the Zone Controller resolved from
		// the ancestor, which is exactly what inheritedFrom means and exactly what
		// filter 11 relies on instead of walking the tree itself.
		zrZone("hospital", "", entryDenied("hospital", "restricted wing")),
		zrZone("ward-3", "hospital", entryDenied("hospital", "restricted wing")),
		zrZone("bay-2", "ward-3", entryDenied("hospital", "restricted wing")),
	}
	v := evaluateZoneRestrictions(zrAction("bay-2"), zones)
	if v == nil || !v.refuse {
		t.Fatal("a leaf two levels below the denying zone was NOT refused. §3: the effective " +
			"rule is the most restrictive in the containment chain; a leaf with no rule under " +
			"a parent that denies is denied")
	}
	if !strings.Contains(v.message, "hospital") {
		t.Errorf("message %q does not name the DECLARING zone (hospital). The target is bay-2 "+
			"and the rule is not there; an operator sent to bay-2 cannot act", v.message)
	}
}

// ---------------------------------------------------------------------------
// ZoneCapabilityDenied — the zone is NOT closed; one capability is.
// ---------------------------------------------------------------------------

func TestFilter11_CapabilityDeniedRefusesAndSaysTheZoneIsStillOpen(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("office", "", capDenied("clean.mop", "office", "carpet that must not get wet")),
	}
	v := evaluateZoneRestrictions(zrAction("office", "clean.mop"), zones)
	if v == nil || !v.refuse {
		t.Fatal("an action requiring a capability the zone denies was not refused")
	}
	if v.reason != fleetv1.ActionFailureZoneCapabilityDenied {
		t.Errorf("failureReason = %q, want %q", v.reason, fleetv1.ActionFailureZoneCapabilityDenied)
	}
	if !v.terminal {
		t.Error("ZoneCapabilityDenied must be terminal")
	}
	for _, want := range []string{"clean.mop", "office", "carpet"} {
		if !strings.Contains(v.message, want) {
			t.Errorf("message %q does not carry %q", v.message, want)
		}
	}
	// The distinction the adapter's no-mop refusal was designed around: a capability
	// denial that read like an entry denial would cost the operator the room.
	low := strings.ToLower(v.message)
	if !strings.Contains(low, "not closed") && !strings.Contains(low, "still open") {
		t.Errorf("message %q does not say the ZONE is not closed. §9: a capability denial names "+
			"the capability and says the zone is not closed — one capability is. The adapter's "+
			"shipped no-mop refusal offers the way through for exactly this reason", v.message)
	}

	// An action requiring a DIFFERENT capability in the same zone is admitted. Without
	// this the test cannot tell a correct filter from one that closes the zone.
	if v := evaluateZoneRestrictions(zrAction("office", "clean.vacuum"), zones); v != nil && v.refuse {
		t.Errorf("an action requiring an unrestricted capability was refused in a zone that "+
			"denies only clean.mop: %+v — that is the room being taken away", v)
	}
}

// ---------------------------------------------------------------------------
// ZoneRestrictionsUnresolved — held, not judged. NOT terminal.
// ---------------------------------------------------------------------------

func TestFilter11_UnresolvedChainHoldsTheActionWithoutJudgingIt(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrUnresolved(zrZone("orphan", "gone"), `zone "orphan" names parentZone "gone", which does not exist`),
		zrZone("elsewhere", "", entryDenied("elsewhere", "the namespace uses restrictions")),
	}
	v := evaluateZoneRestrictions(zrAction("orphan"), zones)
	if v == nil || !v.refuse {
		t.Fatal("an action targeting a zone whose restriction chain does not resolve was " +
			"dispatched. An unresolvable restriction is not an absent restriction")
	}
	if v.reason != fleetv1.ActionFailureZoneRestrictionsUnresolved {
		t.Errorf("failureReason = %q, want %q", v.reason, fleetv1.ActionFailureZoneRestrictionsUnresolved)
	}
	if v.terminal {
		t.Error("ZoneRestrictionsUnresolved must NOT be terminal. It is a fact about today — a " +
			"FleetZone that becomes readable makes the same action dispatchable — so the action " +
			"waits and fails on its deadline like any other unsatisfied Pending")
	}
	low := strings.ToLower(v.message)
	if !strings.Contains(low, "no decision") && !strings.Contains(low, "not been judged") {
		t.Errorf("message %q must say NO DECISION WAS MADE. §9: the action is held, not judged — "+
			"reporting it as a denial would claim the operator forbade something they did not", v.message)
	}
}

// ---------------------------------------------------------------------------
// Narrowing: absorbing over the target relation, narrowing over the subtree.
// ---------------------------------------------------------------------------

func TestFilter11_ADeniedDescendantNarrowsTheLeafSetAndIsNamed(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("floor-2", ""),
		zrZone("office", "floor-2"),
		zrZone("under-bed-strip", "floor-2", entryDenied("under-bed-strip", "the robot cannot get out")),
	}
	v := evaluateZoneRestrictions(zrAction("floor-2"), zones)
	if v != nil && v.refuse {
		t.Fatalf("a denial on a DESCENDANT refused the whole action: %+v\n"+
			"§9: refusing a whole floor because one closet inside it is denied would make the "+
			"floor undispatchable for a reason no operator can act on. The denied zone is "+
			"removed from the leaf set and NAMED", v)
	}
	if v == nil {
		t.Fatal("no verdict returned; the narrowing must be reported even when nothing is refused")
	}
	if len(v.narrowedOut) != 1 || v.narrowedOut[0] != "under-bed-strip" {
		t.Errorf("narrowedOut = %v, want [under-bed-strip]. Trimming SILENTLY is how an operator "+
			"comes to believe a region was serviced that was never entered", v.narrowedOut)
	}
	if v.leaves["under-bed-strip"] {
		t.Error("the denied descendant is still in the resolved leaf set")
	}
	if !v.leaves["office"] {
		t.Error("the permitted sibling was dropped from the leaf set; the action proceeds against the remainder")
	}
}

func TestFilter11_NarrowingToEmptyRefusesWithTheSubtreeExhaustedDetail(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("wing", ""),
		zrZone("a", "wing", entryDenied("a", "closed")),
		zrZone("b", "wing", entryDenied("b", "closed")),
	}
	v := evaluateZoneRestrictions(zrAction("wing"), zones)
	if v == nil || !v.refuse {
		t.Fatal("narrowing emptied the leaf set and the action was not refused")
	}
	if v.reason != fleetv1.ActionFailureZoneEntryDenied {
		t.Errorf("failureReason = %q, want %q — §9 routes the subtree-exhausted case to "+
			"ZoneEntryDenied", v.reason, fleetv1.ActionFailureZoneEntryDenied)
	}
	low := strings.ToLower(v.message)
	if !strings.Contains(low, "every") && !strings.Contains(low, "exhaust") {
		t.Errorf("message %q lacks the subtree-exhausted detail: the operator needs to know the "+
			"target itself is open and everything in it is denied, which is a different "+
			"situation from the target being denied", v.message)
	}
	for _, want := range []string{"a", "b"} {
		if !strings.Contains(v.message, want) {
			t.Errorf("message %q does not name excluded zone %q", v.message, want)
		}
	}
}

// ---------------------------------------------------------------------------
// A zone that restricts nothing changes nothing.
// ---------------------------------------------------------------------------

func TestFilter11_AnUnrestrictedZoneIsUnaffected(t *testing.T) {
	zones := []fleetv1.FleetZone{
		zrZone("floor-2", ""), zrZone("office", "floor-2"),
		zrZone("locked", "", entryDenied("locked", "the namespace uses restrictions")),
	}
	v := evaluateZoneRestrictions(zrAction("floor-2", "clean.vacuum"), zones)
	if v != nil && v.refuse {
		t.Fatalf("an unrestricted zone refused an action: %+v", v)
	}
	if v == nil || !v.leaves["office"] {
		t.Errorf("the leaf set was not resolved for an unrestricted zone: %+v", v)
	}
	if v != nil && len(v.narrowedOut) != 0 {
		t.Errorf("narrowedOut = %v on an unrestricted tree", v.narrowedOut)
	}
}

// A zone the Zone Controller has not reconciled yet carries a NIL projection. That is
// not "unrestricted" — it is "unknown", and FZ-R6's posture applies.
func TestFilter11_AnUnreconciledZoneIsHeldNotDispatched(t *testing.T) {
	unreconciled := fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "ns"},
	}
	v := evaluateZoneRestrictions(zrAction("fresh"), []fleetv1.FleetZone{
		unreconciled,
		zrZone("locked", "", entryDenied("locked", "the namespace uses restrictions")),
	})
	if v == nil || !v.refuse {
		t.Fatal("a zone with no restriction projection at all was dispatched into. status." +
			"restrictions nil means the Zone Controller has not reconciled it — the restriction " +
			"set is UNKNOWN, not empty, and reading nil as unrestricted fails permissively")
	}
	if v.reason != fleetv1.ActionFailureZoneRestrictionsUnresolved {
		t.Errorf("failureReason = %q, want %q", v.reason, fleetv1.ActionFailureZoneRestrictionsUnresolved)
	}
	if v.terminal {
		t.Error("an unreconciled zone is a fact about today, not a standing refusal")
	}
}
