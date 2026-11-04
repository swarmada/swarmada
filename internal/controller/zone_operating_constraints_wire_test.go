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
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// FA-R2 — carry. "The control plane MUST send the RESOLVED EFFECTIVE operating rules for the
// target zone chain on AssignAction … The adapter is never asked to walk a zone tree, and never
// receives a rule that resolution has already superseded."
//
// The only legal source is FleetZone.status.restrictions.effective[], the projection K6b landed.
// Reading spec.restrictions here would reintroduce the second resolver FZ-R4 forbids, so the
// mapper takes the RESOLVED type as its input and there is no overload that takes a spec type.

func TestOperatingConstraintsForWire_KeepsOnlyTheOperatingAxis(t *testing.T) {
	resolved := []fleetv1.EffectiveZoneRestriction{
		{Axis: fleetv1.ZoneAxisEntry, Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "site"},
		{Axis: fleetv1.ZoneAxisCapability, Subject: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "floor-2"},
		{Axis: fleetv1.ZoneAxisOperating, Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied,
			Reason: "patient privacy", InheritedFrom: "ward-3"},
	}
	got := operatingConstraintsForWire(resolved)
	if len(got) != 1 {
		t.Fatalf("want only the operating axis on the wire, got %d entries: %+v", len(got), got)
	}
	if got[0].GetSubject() != "camera" {
		t.Fatalf("wrong subject: %+v", got[0])
	}
	if got[0].GetMode() != fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_DENIED {
		t.Fatalf("wrong mode: %v", got[0].GetMode())
	}
	if got[0].GetDeclaringZone() != "ward-3" {
		t.Fatalf("the DECLARING zone must travel — an adapter declining a rule names the zone that "+
			"declared it, which is very often not the target: %+v", got[0])
	}
	if got[0].GetReason() != "patient privacy" {
		t.Fatalf("the operator's reason must travel with the rule: %+v", got[0])
	}
}

func TestOperatingConstraintsForWire_CarriesBothLimitedShapes(t *testing.T) {
	got := operatingConstraintsForWire([]fleetv1.EffectiveZoneRestriction{
		{Axis: fleetv1.ZoneAxisOperating, Subject: "speed", Mode: fleetv1.ZoneRestrictionLimited,
			Max: "0.4", InheritedFrom: "ward-3"},
		{Axis: fleetv1.ZoneAxisOperating, Subject: "chemical", Mode: fleetv1.ZoneRestrictionLimited,
			Allowed: []string{"quat-ammonium-7"}, InheritedFrom: "kitchen"},
		{Axis: fleetv1.ZoneAxisOperating, Subject: "camera", Mode: fleetv1.ZoneRestrictionRequired,
			InheritedFrom: "inspection-bay"},
	})
	if len(got) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(got), got)
	}
	if got[0].GetMax() != "0.4" {
		t.Fatalf("a Limited bound must travel verbatim, not canonicalised: %+v", got[0])
	}
	if len(got[1].GetAllowed()) != 1 || got[1].GetAllowed()[0] != "quat-ammonium-7" {
		t.Fatalf("the allow-list must travel: %+v", got[1])
	}
	if got[2].GetMode() != fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_REQUIRED {
		t.Fatalf("Required is a mode on this axis and only on this axis: %v", got[2].GetMode())
	}
}

// An empty resolved set is not a rule set with nothing in it — it is no rules, which is
// permission (ADR-0058 §2). nil, so the field serialises away rather than travelling as an
// empty list an adapter might read as "everything is denied".
func TestOperatingConstraintsForWire_NoRulesIsNil(t *testing.T) {
	if got := operatingConstraintsForWire(nil); got != nil {
		t.Fatalf("want nil for no rules, got %+v", got)
	}
	if got := operatingConstraintsForWire([]fleetv1.EffectiveZoneRestriction{
		{Axis: fleetv1.ZoneAxisEntry, Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "site"},
	}); got != nil {
		t.Fatalf("a chain with no OPERATING rule must produce nil, got %+v", got)
	}
}

// FA-R2's field has to be on AssignAction itself, or there is nothing for L2 to fill.
func TestAssignActionCarriesTheResolvedOperatingConstraints(t *testing.T) {
	a := &fav1.AssignAction{
		ActionId: "ns/act-1",
		OperatingConstraints: []*fav1.EffectiveOperatingConstraint{
			{Subject: "microphone", Mode: fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_DENIED,
				DeclaringZone: "ward-3"},
		},
	}
	if len(a.GetOperatingConstraints()) != 1 {
		t.Fatalf("AssignAction does not carry the resolved operating constraint set")
	}
}

// FA-R5 — decline. The enum needs a value naming this refusal, and the result needs somewhere to
// put the subjects it could not honour. SURFACE ONLY: the decline BEHAVIOUR is an adapter
// obligation and is tested by the conformance suite, not here.
func TestAssignActionRejectionHasAnOperatingConstraintValue(t *testing.T) {
	v := fav1.AssignActionRejection_ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET
	if v == fav1.AssignActionRejection_ASSIGN_ACTION_REJECTION_UNSPECIFIED {
		t.Fatal("the new rejection value collides with UNSPECIFIED, which means ACCEPTED")
	}
	// Additive: it must take a new number, never reuse one of the five that shipped.
	if int32(v) != 6 {
		t.Fatalf("want the next free value 6, got %d — renumbering an enum that is on the wire "+
			"is a breaking change", int32(v))
	}
	res := &fav1.AssignActionResult{
		Accepted:             false,
		Rejection:            v,
		UnhonourableSubjects: []string{"microphone"},
	}
	if len(res.GetUnhonourableSubjects()) != 1 {
		t.Fatal("AssignActionResult carries no subject list, so a decline cannot NAME the subject — " +
			"and naming the subject is the whole of what FA-R5 makes testable")
	}
}
