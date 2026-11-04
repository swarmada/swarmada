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
	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// FA-R2 — carry. The control plane sends the RESOLVED EFFECTIVE operating rules for the target
// zone chain on AssignAction, so "the adapter is never asked to walk a zone tree, and never
// receives a rule that resolution has already superseded".
//
// THE INPUT TYPE IS THE ENFORCEMENT. This takes []EffectiveZoneRestriction — the resolved
// projection on FleetZone.status.restrictions.effective[] — and there is deliberately no
// overload taking ZoneRestrictions (the spec block). Filling the wire from spec.restrictions
// would be the second resolver FZ-R4 forbids: a caller that wants to send these has no function
// here that will let it, and has to reach the resolved projection to get one.
//
// Called on every AssignAction (fleetaction_controller.go), with the in-force set candidate
// filter 12 (dispatch_filters.go) has already checked the robot's adapter honours.

// operatingConstraintsForWire converts a zone's resolved restriction set into the wire form,
// keeping the OPERATING axis and dropping the other two.
//
// The other two axes are not merely irrelevant to an adapter — sending them would be wrong.
// Entry and capability denials are decided by candidate filter 11 BEFORE any robot is chosen,
// and both are terminal for the action; an adapter that received one could only re-decide a
// question already answered, at the one point in the flow where a second opinion cannot help.
//
// Returns nil, never an empty slice: no rule in force is PERMISSION (ADR-0058 §2), and an empty
// repeated field that an adapter might read as "everything is denied" would invert that.
func operatingConstraintsForWire(resolved []fleetv1.EffectiveZoneRestriction) []*fav1.EffectiveOperatingConstraint {
	var out []*fav1.EffectiveOperatingConstraint
	for _, r := range resolved {
		if r.Axis != fleetv1.ZoneAxisOperating {
			continue
		}
		out = append(out, &fav1.EffectiveOperatingConstraint{
			Subject: r.Subject,
			Mode:    operatingConstraintModeOf(r.Mode),
			Max:     r.Max,
			Allowed: r.Allowed,
			Reason:  r.Reason,
			// The zone that DECLARED the rule, which is very often not the target: the
			// effective rule is the most restrictive anywhere in the ancestor chain. An
			// adapter declining under FA-R5 names it, so the refusal points an operator at
			// the object they must actually edit.
			DeclaringZone: r.InheritedFrom,
		})
	}
	return out
}

// operatingConstraintModeOf maps the CRD mode onto the wire enum.
//
// An unrecognised mode maps to UNSPECIFIED rather than being guessed at, and an adapter that
// receives UNSPECIFIED has received a rule it does not recognise — which under FA-R6 it MUST
// decline naming the subject, not proceed as though no constraint were in force. Guessing
// DENIED here would be safe-looking and wrong: it would silently convert a mode this build does
// not understand into one it does, and hide the fact that the two ends disagree.
func operatingConstraintModeOf(m fleetv1.ZoneRestrictionMode) fav1.OperatingConstraintMode {
	switch m {
	case fleetv1.ZoneRestrictionDenied:
		return fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_DENIED
	case fleetv1.ZoneRestrictionLimited:
		return fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_LIMITED
	case fleetv1.ZoneRestrictionRequired:
		return fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_REQUIRED
	default:
		return fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_UNSPECIFIED
	}
}
