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

// Zone-restriction refusal reasons for FleetAction.status.failureReason
// (ADR-0058 §9, amendment Part C).
//
// These are constants over an existing free-form string field, NOT a new schema
// surface: status.failureReason is documented as "a human-readable reason" and is
// unconstrained. Naming the five here is what stops the control plane and a consumer
// spelling one two ways.
//
// # Why five, and why collapsing any two costs the operator the difference
//
// Refusal wording was not invented for this specification. A private household
// deployment already had four refusal kinds in production against a real device and a
// real family, distinguished by a machine-readable `kind` so a surface could tell them
// apart without parsing prose — designed against the case where a person is standing
// in a room asking why the robot is not moving. The five below are measured against
// those four (ADR-0058 §9).
//
// The five are genuinely different sentences: THERE IS NO SUCH ZONE; YOU TOLD ME NOT
// TO GO THERE; THE ZONE IS OPEN BUT THIS JOB IS NOT; NO ROBOT HERE CAN HONOUR THE
// RULE; and I COULD NOT WORK OUT WHAT THE RULE IS.
//
// # Terminal versus Pending, which is the split that matters
//
// ZoneEntryDenied and ZoneCapabilityDenied are TERMINAL: they are standing decisions
// that no retry and no fleet change will satisfy. ZoneOperatingConstraintUnmet and
// ZoneRestrictionsUnresolved are NOT: they are facts about today — a conforming
// adapter deployed tomorrow, or a FleetZone that becomes readable, makes the same
// action dispatchable — so the action waits and fails on its deadline like any other
// unsatisfied Pending. ZoneUnknown is terminal.
//
// # A refusal MUST NOT claim more than it does
//
// ZoneEntryDenied means the action was NOT DISPATCHED. It does not mean no robot will
// enter the zone: a control-plane refusal cannot stop a robot driven by its own vendor
// application, its own buttons, its own schedule, or a person carrying it. An operator
// surface that renders this as PROTECTED rather than as NOT DISPATCHED TO is asserting
// a property this specification does not provide.
const (
	// ActionFailureZoneEntryDenied: the target zone, or any ancestor of it, carries an
	// effective entry denial — or the subtree narrowing emptied the leaf set. Terminal.
	//
	// The refusal carries the DECLARING zone (which is very often not the target), the
	// operator's reason, and the fact that it holds whether the zone is targeted
	// directly or reached through an ancestor. That last clause is the absorbing
	// property, and it is carried over verbatim in meaning from the shipped adapter
	// refusal: "…so I will not send the robot there, on its own or as part of anything
	// else."
	ActionFailureZoneEntryDenied = "ZoneEntryDenied"

	// ActionFailureZoneCapabilityDenied: spec.requiredCapabilities intersects the
	// effective denied-capability set. Terminal.
	//
	// The refusal names the capability, the declaring zone, the reason — AND that the
	// zone itself is not closed, one capability is. A capability denial that read like
	// an entry denial would cost the operator the room, which is why the shipped
	// adapter refusal for this case offers the way through rather than just refusing.
	ActionFailureZoneCapabilityDenied = "ZoneCapabilityDenied"

	// ActionFailureZoneOperatingConstraintUnmet: no candidate robot's adapter declares
	// every effective operating subject in force AT THE LEAF ZONE THAT ROBOT OCCUPIES.
	// Pending, then Failed on deadline.
	//
	// The scope is the RESOLVED LEAF SET, not the target's own chain (ADR-0058
	// Amendment 2). Filter 2 admits robots from the whole subtree of the action's zone,
	// so a filter reading only the target's projection would never see a rule declared
	// on a descendant — a rule with a bypass. The refusal names the subject and every
	// zone of the leaf set it is in force in, because the declaring zone is very often
	// not the target.
	//
	// IMPLEMENTED. Candidate filter 12 is the producer
	// (internal/controller/dispatch_operating_constraints.go): it reads
	// Robot.status.honouredOperatingConstraints[], projected from
	// CapabilitiesSnapshot.honoured_operating_constraints, and both now exist.
	//
	// An empty or absent declaration satisfies NO subject (ADR-0058 Amendment 1 §A1.2),
	// so an adapter that declares nothing is withheld on the same footing as one that
	// declares other subjects and not this one. It is not in breach and is not marked
	// non-conformant — its robots are simply not dispatched into a zone restricting a
	// subject.
	//
	// WHAT THIS REASON DOES NOT CLAIM: withholding is a refusal to ASSIGN WORK. It does
	// not stop a robot entering the zone under its own vendor application, its own
	// buttons, a schedule, or a person carrying it — and a robot that PASSES filter 12
	// has an adapter that merely CLAIMS the subject. An attestation is a claim, not a
	// measurement, and the control plane cannot verify it (ADR-0058 §12).
	//
	// Nor does the widened scope cover a zone the robot CROSSES on the way to the leaf:
	// the control plane does no path planning, so a transit corridor is not a zone this
	// filter can see (ADR-0058 Amendment 2 §A2.4). A transiting robot is covered by the
	// adapter obligation, not by this reason.
	ActionFailureZoneOperatingConstraintUnmet = "ZoneOperatingConstraintUnmet"

	// ActionFailureZoneRestrictionsUnresolved: the zone chain could not be read, a
	// parentZone does not resolve, or the zone has no restriction projection yet.
	// Pending — NOT terminal.
	//
	// The refusal says WHICH resolution failed and that NO DECISION WAS MADE. The
	// action is held, not judged. Reporting it as a denial would claim the operator
	// forbade something they did not; reporting it as success would dispatch into a
	// zone whose rules are unknown.
	ActionFailureZoneRestrictionsUnresolved = "ZoneRestrictionsUnresolved"

	// ActionFailureZoneUnknown: spec.zone names no FleetZone in the namespace.
	// Terminal.
	//
	// The refusal carries the name AS WRITTEN and an enumeration of the zones that do
	// exist. Ambiguity asks; it never guesses — the shipped adapter refusal offers the
	// list for the same reason, because the overwhelmingly common cause is a typo and
	// the list is the fix.
	ActionFailureZoneUnknown = "ZoneUnknown"
)
