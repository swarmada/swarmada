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

// Resolved zone restrictions — the status half of ADR-0058 (FZ-R1 … FZ-R6).
//
// spec.restrictions is what an operator WROTE on one zone. This is what is in
// force AT a zone, after the whole ancestor chain has been folded together.
//
// CONSUMERS READ THIS, NEVER THE TREE (FZ-R4). The reason is the same one that
// makes status.geometry exist rather than leaving readers to interpret
// spec.physicalBounds: "what is forbidden here" must be decided in ONE place. A
// second implementation of the fold is a second answer, and the two drift.

// ZoneRestrictionAxis names which of the three axes an effective rule came from
// (ADR-0058 §1). It exists on the resolved entry and not on the spec rule, because
// in spec the axis IS the field the rule was written under, and here every axis is
// flattened into one list.
// +kubebuilder:validation:Enum=Entry;Capability;Operating
type ZoneRestrictionAxis string

// ZoneRestrictionAxis values.
const (
	// ZoneAxisEntry is axis 1 — may a robot be here at all. Its effective entry
	// carries no Subject: entry is about the zone, not about a named thing in it.
	ZoneAxisEntry ZoneRestrictionAxis = "Entry"
	// ZoneAxisCapability is axis 2 — may a robot perform this job here. Subject is
	// the capability identifier, matched against FleetAction.spec.requiredCapabilities.
	ZoneAxisCapability ZoneRestrictionAxis = "Capability"
	// ZoneAxisOperating is axis 3 — what a robot may have running while present.
	// Subject is an operating subject from the normative core seven, or a namespaced
	// extension. It is NOT a capability identifier (ADR-0058 §7).
	ZoneAxisOperating ZoneRestrictionAxis = "Operating"
)

// FleetZone condition types written by the Zone Controller for restrictions
// (ADR-0058 §3, FZ-R3/FZ-R4/FZ-R6). Exported because the Scheduler's candidate
// filters read them (amendment Part B.3) and a consumer must not re-spell them.
const (
	// ZoneCondRestrictionsResolved reports whether the effective set below is
	// trustworthy. False means the ancestor chain could not be read or does not
	// resolve, and per FZ-R6 the zone is then NOT DISPATCHABLE — an unresolvable
	// restriction is not an absent one. It is False, never absent, in that case:
	// a missing condition is indistinguishable from a controller that has not run.
	ZoneCondRestrictionsResolved = "RestrictionsResolved"
	// ZoneCondRestrictionsConsistent reports whether the chain contains a
	// contradiction — Denied and Required on one subject (FZ-R3). False does NOT
	// mean unresolved: the set below is still correct and still enforceable, and the
	// contradiction has already been resolved to Denied. It means an operator wrote
	// two rules that cannot both be honoured and should be told which.
	ZoneCondRestrictionsConsistent = "RestrictionsConsistent"
)

// ZoneRestrictionsStatus is the resolved restriction projection for one zone
// (FZ-R4).
//
// FZ-R5 GOVERNS WHEN THIS IS WRITTEN, AND IT IS RA-1. This is a projection of
// spec and the zone tree. It MUST be recomputed on a spec or topology change and
// MUST NOT be recomputed on a telemetry tick. It carries no per-robot and no
// positional data — deliberately: the obvious next field, "which robots are
// currently inside this denied zone", would be written from position telemetry on
// every frame, which is the one write this project prohibits outright. The standing
// occupancy view is status.robotCount, already a throttled material-transition
// projection, and ADR-0058 §10 routes the occupant question to a one-shot event
// instead of a status list for exactly this reason.
type ZoneRestrictionsStatus struct {
	// Effective is every restriction in force at this zone, from any zone in its
	// ancestor chain including itself, already folded to one entry per
	// (axis, subject).
	//
	// Empty means nothing is restricted here — which is what every zone with no
	// restrictions anywhere in its chain reports, and is permission.
	//
	// The atomic list type is deliberate: this list is written only by the Zone
	// Controller, never merged by server-side apply from two writers, so a map key
	// would buy nothing and would force a key onto the Entry axis, which has no
	// natural one.
	// +optional
	// +listType=atomic
	Effective []EffectiveZoneRestriction `json:"effective,omitempty"`
}

// EffectiveZoneRestriction is one resolved rule: what is in force at this zone for
// one (axis, subject), and which zone put it there.
type EffectiveZoneRestriction struct {
	// Axis is which of the three axes this rule constrains.
	Axis ZoneRestrictionAxis `json:"axis"`

	// Subject is the capability identifier (Capability) or operating subject
	// (Operating) this rule constrains. EMPTY for Entry, which has no subject.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Subject string `json:"subject,omitempty"`

	// Mode is the resolved mode — the most restrictive found anywhere in the chain.
	// Denied is absorbing (FZ-R2). A Denied and a Required on one subject resolve
	// HERE to Denied and are reported as a contradiction on
	// RestrictionsConsistent (FZ-R3), because a zone where a camera must be off and
	// must be on is a zone no robot may work in — and resolving the other way would
	// switch a camera on in a patient room by operation of a merge rule.
	Mode ZoneRestrictionMode `json:"mode"`

	// Max is the tightest bound found for this subject anywhere in the chain: two
	// Limited rules INTERSECT rather than override, so a deep tree can become
	// arbitrarily restrictive in a way no single object shows. That is exactly why
	// this projection is load-bearing rather than a convenience.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	Max string `json:"max,omitempty"`

	// Allowed is the INTERSECTION of every allow-list found for this subject in the
	// chain. It reads the same way it does in spec: everything not listed is denied,
	// which inverts the reading of every other field here.
	//
	// An empty list is never published. When two allow-lists in one chain do not
	// overlap, the chain permits nothing — and because this field carries omitempty,
	// an empty list would serialise away and read back as "no allow-list", meaning
	// permit EVERYTHING. So that case resolves to Mode: Denied instead, which is the
	// only expressible answer that is not weaker than what the chain declared.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	Allowed []string `json:"allowed,omitempty"`

	// Reason is the operator's own words, carried from the zone named in
	// InheritedFrom. It is what a refusal shows a person asking why a robot is not
	// moving, so it travels with the rule rather than being looked up again.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason,omitempty"`

	// InheritedFrom names the zone that DECLARED the rule this entry resolved to —
	// which is very often not this zone. It is the single most useful field here: an
	// operator looking at a zone that denies cameras needs to know whether to edit
	// this object or one three levels up, and FZ-R7 means they may not be able to
	// weaken it from here at all.
	//
	// When several zones declared the same subject, this names the zone whose
	// declaration is ACTUALLY IN FORCE — the one that supplied the winning mode, or
	// for a Limited rule the one that supplied the binding bound. Ties go to the
	// outermost (closest to the root), because that is the declaration a local edit
	// cannot remove.
	//
	// Naming the merely-first declarer instead would be worse than naming nothing: an
	// operator sent to edit a zone that declared a looser bound would change it and
	// see no effect, because a tighter rule further down the chain was the one
	// binding all along.
	// +kubebuilder:validation:MinLength=1
	InheritedFrom string `json:"inheritedFrom"`
}
