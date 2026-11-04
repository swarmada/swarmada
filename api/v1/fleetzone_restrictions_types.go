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

// Zone restrictions — ADR-0058, rfcs/amendments/RFC-0001-zone-restrictions.md Part A.
//
// A FleetZone can carry standing statements about what may happen in the region it
// names. This is how a site policy — no robots on the patio, no mopping on the
// carpet, no cameras in patient rooms — is written ONCE, in the fleet's own
// vocabulary, rather than separately inside each application that dispatches.
//
// Restrictions split into three axes, and one block expresses all three:
//
//	1. Entry      — may a robot be here at all.
//	2. Capability — may it perform THIS JOB here.
//	3. Operating  — what may it have RUNNING while it is here.
//
// Axis 3 is not a narrower axis 2 (ADR-0058 §1). "May clean here" is about the
// ACTION; "must not have a camera running here" is about the robot's STATE WHILE
// PRESENT, and is true or false of a robot that was asked to do nothing at all. The
// deciding case is a robot TRANSITING a ward with a live microphone: no action was
// refused, no capability was required, and the constraint was still broken.
//
// What v0.4 does with these fields:
//
//   - The FleetZone admission webhook rejects a rule that weakens an ancestor's
//     (FZ-R7).
//   - The Zone Controller folds each zone's rules with its ancestors' into
//     status.restrictions.effective, and publishes the RestrictionsResolved and
//     RestrictionsConsistent conditions.
//   - Candidate filter 11 fails an action whose target zone, or an ancestor,
//     denies entry or a capability the action requires, and removes a denied
//     descendant zone from the zones the action is dispatched into.
//   - Candidate filter 12 dispatches into a zone with operating rules only a robot
//     whose adapter declares every subject in force
//     (Robot.status.honouredOperatingConstraints).
//   - The resolved operating rules are sent on AssignAction and pushed as a
//     zone_constraints command when a robot changes zone or the rules change. An
//     adapter that cannot honour one declines the assignment, and the action
//     returns to Pending.
//
// Not done at v0.4: entry.occupantAction moves no robot (every value behaves as
// Report), and nothing reports or records whether an adapter honoured an operating
// rule.

// ZoneRestrictionMode is the mode of a single zone restriction rule (ADR-0058 §2).
//
// THERE IS NO `Allowed` VALUE, AT ANY AXIS, AND ITS ABSENCE IS THE DESIGN. A rule
// exists only to narrow, and absence of a rule is permission — so a zone that takes
// no position writes nothing. This is not economy: it makes A HOLE UNWRITABLE. An
// operator cannot express "re-permit what my parent forbade" at all, so the entire
// class of "a child opens a hole in a parent's boundary" is removed at the schema
// rather than caught at admission. An `Allowed` value would have been written in
// good faith, been silently inert, and been believed, in exactly the direction where
// being believed is worst.
// +kubebuilder:validation:Enum=Denied;Limited;Required
type ZoneRestrictionMode string

// ZoneRestrictionMode values.
const (
	// ZoneRestrictionDenied: the subject is forbidden in this zone and every
	// descendant. Valid on entry, capabilities and operating. Denial is ABSORBING —
	// no resolution, merge or default may produce an effective rule more permissive
	// than one an ancestor declared (FZ-R2).
	ZoneRestrictionDenied ZoneRestrictionMode = "Denied"
	// ZoneRestrictionLimited: the subject is permitted only within Max, or only among
	// Allowed. Valid on OPERATING rules only (CEL) — a capability restriction carries
	// no limits (ADR-0058 §8).
	ZoneRestrictionLimited ZoneRestrictionMode = "Limited"
	// ZoneRestrictionRequired: the subject MUST be running — the inspection-zone
	// mirror case. Valid on OPERATING rules only (CEL). Required and Denied on one
	// subject in one ancestor chain are a CONTRADICTION, not a widening; it resolves
	// to Denied (FZ-R3), because a zone where a camera must be off and must be on is
	// a zone no robot may work in, and resolving the other way would switch a camera
	// on in a patient room by operation of a merge rule.
	ZoneRestrictionRequired ZoneRestrictionMode = "Required"
)

// ZoneOccupantAction selects what happens to robots already present in a zone at the
// moment an entry denial takes effect (ADR-0058 §10). It reuses ZoneMaintenance's
// wind-down vocabulary rather than inventing a second spelling of the same choice.
// +kubebuilder:validation:Enum=Report;Graceful;Immediate
type ZoneOccupantAction string

// ZoneOccupantAction values.
const (
	// ZoneOccupantReport (the default): NO ROBOT IS MOVED, and the Zone Controller emits
	// ONE ZoneEntryDeniedWithOccupants Warning naming the robots that were inside when
	// the denial took effect.
	//
	// That is the whole of it. The event reports a moment and claims nothing more — not
	// that those robots left, were stopped, were wound down, or will be.
	//
	// Report is the default because a control plane that starts MOVING ROBOTS in response
	// to an edit of a spec field is a new autonomous motion path, and this surface is
	// explicitly not a safety system. A policy edit that commands motion is a larger
	// claim than the requirement makes.
	//
	// WITHHOLDING NEW WORK IS NOT THIS FIELD'S CONTRIBUTION. An effective entry denial
	// withholds work from the zone through candidate filter 11 whatever occupantAction
	// says. This field governs only what happens to robots ALREADY PRESENT, and at v0.4
	// what happens to them is that they are NAMED.
	//
	// THE OCCUPANT REPORT MUST NOT BECOME A STATUS STREAM. The event fires once, at the
	// transition, latched exactly like the RestrictionContradiction event beside it. NO
	// occupancy list is added to FleetZone.status: such a field would be written from
	// position telemetry, which RA-1 prohibits per tick. The producer reads
	// Robot.status.containingZones[] and status.currentZone — each written only on a
	// material transition — and never a position frame. The standing occupancy view is
	// status.robotCount, already a throttled, material-transition projection.
	//
	// # HISTORY, kept because it is why this field is shaped as it is
	//
	// Until 2026-09-14 this comment described an event that was emitted NOWHERE. The
	// producer did not exist, and three normative texts said it did. The session that
	// found that STRUCK the promise rather than leaving it — the right call, because a
	// doc comment describing an observable an operator will wait for is worse than one
	// describing nothing: the comment is read as the authoritative statement of what the
	// value does.
	//
	// For that interval all three values of this enum had ONE behaviour, and it was
	// nothing — which made Report indistinguishable from the two the admission webhook
	// warns are inert, and made that warning harder to read rather than easier. The
	// producer now exists (internal/controller/zone_occupant_report.go), so the promise
	// is restored as a fact. **None of the struck OVER-claims came back with it:** no
	// robot is moved, withholding work is still credited to the denial and not to this
	// field, and the event is still not a statement that anyone left.
	ZoneOccupantReport ZoneOccupantAction = "Report"
	// ZoneOccupantGraceful: RESERVED. Robots finish their current action and are then
	// wound down, per ZoneMaintenance's Graceful discipline. SPECIFIED, NOT IMPLEMENTED
	// at v0.4.
	ZoneOccupantGraceful ZoneOccupantAction = "Graceful"
	// ZoneOccupantImmediate: RESERVED. In-progress actions are requeued as Pending, per
	// ZoneMaintenance's Immediate discipline. SPECIFIED, NOT IMPLEMENTED at v0.4.
	ZoneOccupantImmediate ZoneOccupantAction = "Immediate"
)

// Operating-constraint subjects: the normative core is CLOSED, the extension form is
// NAMESPACED (ADR-0058 §7). A subject containing no "/" MUST be one of the seven
// below; a subject containing a "/" is an extension subject whose prefix is a DNS
// domain the extender controls (example.com/leaf-blower). The split is pattern plus
// CEL rather than an Enum marker because the set is not closed — it is closed AT THE
// CORE, which is the same split this API already applies to capability identifiers.
// Conformance covers the core seven only.
//
// THESE ARE NOT CAPABILITY IDENTIFIERS, AND THE TWO VOCABULARIES NEVER MEET.
// Robot.status.capabilities is NOT consulted to resolve a subject, and a subject that
// collides with a capability name means nothing. A capability answers "can this robot
// be asked to do X"; an operating subject answers "is X happening right now". A
// capability's status (Active/Degraded/Inactive) grades HEALTH, not whether the thing
// is switched on. A site-policy vocabulary must be authorable by the SITE, before any
// adapter exists, and must mean the same thing across every vendor: a vocabulary
// whose membership is decided by the parties it constrains cannot constrain them. The
// shape of the identifier is borrowed; the vocabulary is not.
const (
	// ZoneSubjectCamera constrains any image sensor capturing while the robot is present.
	ZoneSubjectCamera = "camera"
	// ZoneSubjectMicrophone constrains any audio sensor capturing while the robot is present.
	ZoneSubjectMicrophone = "microphone"
	// ZoneSubjectWater constrains water put on the floor or into the air.
	ZoneSubjectWater = "water"
	// ZoneSubjectChemical constrains any dispensed chemical agent; Allowed enumerates
	// the permitted agents.
	ZoneSubjectChemical = "chemical"
	// ZoneSubjectUV constrains ultraviolet emission.
	ZoneSubjectUV = "uv"
	// ZoneSubjectSpeed constrains ground speed; Max is in the namespace's distance unit
	// per second (SwarmadaConfig.spec.coordinateSystem).
	ZoneSubjectSpeed = "speed"
	// ZoneSubjectNoise constrains emitted sound; Max is in dB(A).
	ZoneSubjectNoise = "noise"
)

// ZoneOperatingSubjectPattern is THE axis-3 subject pattern, written once (ADR-0058 §7):
// an optional DNS-subdomain namespace prefix, then a DNS-subdomain-shaped name.
//
// It is duplicated, unavoidably, as a +kubebuilder:validation:Pattern marker on every field
// that carries a subject — a marker must be a literal string in a comment, so there is no way
// to point one at this constant. TestZoneOperatingSubjectPatternIsWrittenOnce gates the
// duplication: every such marker in this package must be this string, character for character.
//
// THE DRIFT WOULD BE A BYPASS, WHICH IS WHY IT IS GATED RATHER THAN TRUSTED. Filter 12 compares
// a subject an operator wrote on a FleetZone against a subject an adapter declared on a Robot.
// A value one side accepts and the other rejects can never match, and the robot is withheld
// forever from a zone for a subject it actually honours — with the declaration sitting in
// `kubectl get robot -o yaml` looking correct.
//
// The compiled form is used by the CapabilitiesIngestor to drop an out-of-vocabulary
// declaration before it reaches the API server, so one malformed subject cannot fail the whole
// status write and cost a robot the valid subjects beside it.
const ZoneOperatingSubjectPattern = `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`

// ZoneCoreOperatingSubjects is the closed normative core of the axis-3 vocabulary, in
// the order the specification lists it. Conformance covers these seven only. A
// subject carrying no "/" MUST be one of them, enforced by CEL on the schema so the
// API server rejects the object before any controller sees it.
//
// An UNRECOGNISED SUBJECT IS NOT AN ABSENT SUBJECT: an adapter receiving an effective
// operating rule whose subject it does not recognise MUST decline the assignment
// naming that subject, rather than proceeding as though no constraint were in force.
// Without that obligation the extension form would be a universal bypass — any site
// could silence a constraint by namespacing it.
var ZoneCoreOperatingSubjects = []string{
	ZoneSubjectCamera,
	ZoneSubjectMicrophone,
	ZoneSubjectWater,
	ZoneSubjectChemical,
	ZoneSubjectUV,
	ZoneSubjectSpeed,
	ZoneSubjectNoise,
}

// ZoneRestrictions is the single restriction block on FleetZone.spec, covering all
// three axes (ADR-0058 §1).
//
// ONE MODEL, THREE AXES — NOT THREE SCHEMAS. A restriction rule is, in every axis,
// the same tuple: (declaring zone, subject, mode, parameters, reason). Inheritance,
// absorbency, precedence, resolution, status projection, refusal vocabulary,
// admission validation and conformance are defined ONCE OVER RULES and never restated
// per axis. The three members exist only because each has a different natural map key
// — none, a capability name, a subject — and a named list in this API is a
// listType=map (docs/api-principles.md, "Named lists are maps"). The grouping is a
// schema affordance; the model is one.
//
// ABSENCE MEANS A ZONE STATES NO RESTRICTION, which is what every FleetZone written
// before this block existed meant. The whole block and every field in it is additive
// and optional, so no existing object changes meaning and no API version bump is
// required.
//
// A ZONE RESTRICTION IS NOT A SAFETY CONTROL, AND AN IMPLEMENTATION MUST NOT PRESENT
// IT AS ONE. A control-plane refusal governs what is dispatched THROUGH THIS FLEET.
// It cannot stop a robot driven by its own vendor application, its own buttons, its
// own schedule, or a person carrying it, and nothing in a control plane can physically
// switch off a camera. An operator surface that reports a denied zone as PROTECTED
// rather than as NOT DISPATCHED TO is claiming a property this API does not provide.
// What it does provide is narrower and worth stating: an operator declares a boundary
// once, in the fleet's vocabulary, and every path through the fleet respects it — the
// Scheduler, swarmctl, a second adapter, and any client using the Kubernetes API.
type ZoneRestrictions struct {
	// Entry is axis 1: may a robot be here at all. AT MOST ONE, AND ITS PRESENCE IS THE
	// DENIAL — there is no entry rule that permits, because there is no permissive mode
	// to write (see ZoneRestrictionMode). Absent means the zone takes no position on
	// entry, which is permission.
	//
	// The denial applies to this zone AND EVERY DESCENDANT, following spec.parentZone
	// only (FZ-R1). It is ABSORBING OVER THE TARGET RELATION: an action is refused
	// whether it targets the denying zone directly or reaches it through an ancestor.
	// +optional
	Entry *ZoneEntryRestriction `json:"entry,omitempty"`

	// Capabilities is axis 2: may a robot perform THIS JOB here. Each entry names a
	// capability identifier matched against FleetAction.spec.requiredCapabilities.
	//
	// A CAPABILITY RESTRICTION IS A DENIAL AND NOTHING ELSE (ADR-0058 §8). There is no
	// Limited mode and no limits map on this axis; "may clean here, but at 0.4 m/s" is
	// TWO RULES, ONE PER AXIS — a capability is not denied, and a speed is. Putting a
	// bound under a capability name would duplicate the axis-3 subject vocabulary in a
	// second place, with a second set of units, a second inheritance question and a
	// second conformance target.
	//
	// A capability denial does NOT close the zone: the zone stays open and one
	// capability does not. That distinction is what the ZoneCapabilityDenied refusal
	// carries, and collapsing it into an entry denial costs the operator the room.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	Capabilities []ZoneCapabilityRestriction `json:"capabilities,omitempty"`

	// Operating is axis 3: what a robot may have RUNNING while it is here — camera,
	// microphone, water, chemicals, UV, speed, noise. It constrains the robot's STATE
	// WHILE PRESENT, including while merely transiting, and is therefore true or false
	// of a robot that was asked to do nothing at all.
	//
	// THE PLANE CANNOT VERIFY THIS AXIS (ADR-0058 §12). Nothing in a control plane can
	// physically switch off a camera. What it does: the ZONE DECLARES the constraint
	// here; the ADAPTER DECLARES which subjects it can honour
	// (Robot.status.honouredOperatingConstraints); the Scheduler dispatches into the
	// zone only a robot whose adapter declares every subject in force, and the control
	// plane CARRIES the resolved set on AssignAction; an adapter that CANNOT honour a
	// constraint DECLINES the assignment naming the subject, and the action returns to
	// Pending. The adapter is then expected to honour the constraint while the robot is
	// in the zone.
	//
	// NOTHING REPORTS WHETHER IT DID. No outcome message carries which subjects were
	// honoured, and the control plane records and checks nothing about it. A declared
	// subject is a claim by the adapter, not a measurement.
	// +optional
	// +listType=map
	// +listMapKey=subject
	// +kubebuilder:validation:MaxItems=32
	Operating []ZoneOperatingRestriction `json:"operating,omitempty"`
}

// ZoneEntryRestriction is axis 1 — whether a robot may be in this zone at all
// (ADR-0058 §1, §10). At most one per zone, because its PRESENCE is the denial.
// +kubebuilder:validation:XValidation:rule="self.mode == 'Denied'",message="spec.restrictions.entry.mode must be Denied: a rule exists only to restrict, and the presence of an entry rule IS the denial"
type ZoneEntryRestriction struct {
	// Mode must be Denied (CEL, on this type). Limited and Required are valid only on
	// operating rules — there is no bound or requirement that makes sense of "may a
	// robot be here at all", and there is no permissive mode at any axis.
	//
	// The field exists even though only one value is admissible because the rule tuple
	// is one shape in all three axes, and because a restriction an operator reads back
	// should say what it does rather than leaving the reader to infer a denial from a
	// key's existence.
	Mode ZoneRestrictionMode `json:"mode"`

	// Reason is the operator's own words for WHY, carried verbatim into the
	// ZoneEntryDenied refusal an operator reads when a robot is not moving
	// ("…is marked off limits — {reason} — so I will not send the robot there, on its
	// own or as part of anything else"). Optional, because a site may state a boundary
	// without explaining it, but strongly recommended: the refusal is a sentence a
	// person standing in a room reads, and without a reason it cannot say why.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason,omitempty"`

	// OccupantAction selects what happens to robots already inside when this denial
	// takes effect. ONLY Report is specified as implemented at v0.4; Graceful and
	// Immediate are reserved (see ZoneOccupantAction).
	// +optional
	// +kubebuilder:default=Report
	OccupantAction ZoneOccupantAction `json:"occupantAction,omitempty"`
}

// ZoneCapabilityRestriction is axis 2 — whether a robot may perform a given job here
// (ADR-0058 §1, §8). Keyed by Name, so server-side apply merges per capability.
// +kubebuilder:validation:XValidation:rule="self.mode == 'Denied'",message="spec.restrictions.capabilities[].mode must be Denied: mode Limited and mode Required are valid only on spec.restrictions.operating[], because a capability restriction carries no limits — a bound on what runs is an operating rule on its own subject"
type ZoneCapabilityRestriction struct {
	// Name is the capability identifier this zone denies, matched against
	// FleetAction.spec.requiredCapabilities (e.g. "clean.mop", "clean_area").
	//
	// Deliberately NOT pattern-validated beyond a length bound:
	// FleetAction.spec.requiredCapabilities is an unvalidated []string, and capability
	// identifiers in the field today take both dotted and underscored forms. A pattern
	// here that the action side does not apply would make some capability impossible to
	// deny, which is the one outcome this axis must not produce. Write the identifier
	// exactly as it appears in requiredCapabilities; the match is literal.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Mode must be Denied (CEL, on this type). See ZoneRestrictions.Capabilities.
	Mode ZoneRestrictionMode `json:"mode"`

	// Reason is the operator's own words for WHY, carried into the ZoneCapabilityDenied
	// refusal alongside the capability and the declaring zone — and alongside the fact
	// that the ZONE IS NOT CLOSED, one capability is. Optional; strongly recommended.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason,omitempty"`
}

// ZoneOperatingRestriction is axis 3 — what a robot may have RUNNING while it is in
// this zone (ADR-0058 §1, §7). Keyed by Subject, so server-side apply merges per
// subject.
//
// The two cross-field rules below are on the schema as CEL rather than in a
// controller, so the API server rejects a malformed rule before anything reads it:
// Max and Allowed are meaningful only under Limited, and a Limited rule sets exactly
// one of them — Max bounds a scalar, Allowed enumerates a set, and a subject is one or
// the other.
// +kubebuilder:validation:XValidation:rule="self.mode == 'Limited' || (!has(self.max) && !has(self.allowed))",message="spec.restrictions.operating[]: max and allowed are meaningful only when mode is Limited; a Denied or Required rule sets neither"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Limited' || (has(self.max) != has(self.allowed))",message="spec.restrictions.operating[]: set exactly one of max or allowed on a Limited rule — max bounds a scalar, allowed enumerates a set, and a subject is one or the other"
// +kubebuilder:validation:XValidation:rule="self.subject.contains('/') || self.subject in ['camera','microphone','water','chemical','uv','speed','noise']",message="spec.restrictions.operating[].subject must be one of the normative core seven (camera, microphone, water, chemical, uv, speed, noise), or a namespaced extension subject such as 'example.com/lidar'"
type ZoneOperatingRestriction struct {
	// Subject names what is constrained. A subject with no "/" MUST be one of the
	// normative core seven — camera, microphone, water, chemical, uv, speed, noise
	// (CEL, on this type). A subject with a "/" is an extension subject whose prefix is
	// a DNS subdomain the extender controls, e.g. "example.com/leaf-blower" (pattern).
	//
	// These are NOT capability identifiers and the two vocabularies never meet; see the
	// package comment on ZoneCoreOperatingSubjects. An unrecognised subject is NOT an
	// absent subject — an adapter that does not recognise one MUST decline the
	// assignment naming it, or the extension form becomes a universal bypass.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Subject string `json:"subject"`

	// Mode is Denied, Limited or Required. All three are valid on this axis and only on
	// this axis. Required is the inspection-zone mirror case: the subject MUST be
	// running. Required under an ancestor's Denied (or the reverse) is a contradiction
	// that resolves to Denied (FZ-R3).
	Mode ZoneRestrictionMode `json:"mode"`

	// Max is the upper bound a Limited rule permits, as a plain non-negative decimal
	// string: "0.4" for a speed, "65" for a noise cap. Set only under mode: Limited,
	// and never together with Allowed (CEL).
	//
	// THE UNIT IS A PROPERTY OF THE SUBJECT, NOT OF THIS VALUE. speed is in the
	// namespace's distance unit per second (SwarmadaConfig.spec.coordinateSystem);
	// noise is in dB(A). The pattern admits no suffix and no exponent for exactly that
	// reason: a resource.Quantity-style "400m" would be ambiguous between milli- and
	// metres in a field whose subjects include both a speed and a sound level, and the
	// string round-trips an operator's site policy back verbatim rather than
	// canonicalising what they wrote.
	//
	// Between two Limited rules on one subject in an ancestor chain the TIGHTER bound
	// wins (FZ-R1) — bounds are intersected, never overridden, so a deep zone tree can
	// become arbitrarily restrictive in a way no single object shows.
	// +optional
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]*)(\.[0-9]+)?$`
	// +kubebuilder:validation:MaxLength=32
	Max string `json:"max,omitempty"`

	// Allowed IS AN ALLOW-LIST: the subject is permitted ONLY as one of the values
	// named here, and EVERYTHING NOT LISTED IS DENIED. This inverts the reading of
	// every other field in this block — elsewhere absence is permission, and here
	// absence from this list is denial — so it is called out rather than left to be
	// inferred. `allowed: ["quat-ammonium-7"]` on a chemical rule denies every other
	// chemical agent, including ones nobody had thought of when the rule was written,
	// which is the point: a food-preparation surface is an allow-list case and a
	// deny-list of known agents would be wrong the first time a new one arrives.
	//
	// Set only under mode: Limited, and never together with Max (CEL). Between two
	// Limited rules on one subject in an ancestor chain the INTERSECTION of the lists
	// wins (FZ-R1), for the same reason the tighter Max wins.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Allowed []string `json:"allowed,omitempty"`

	// Reason is the operator's own words for WHY, carried into the
	// ZoneOperatingConstraintUnmet refusal alongside the subject, the declaring zone,
	// and which adapters were asked. Optional; strongly recommended.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Reason string `json:"reason,omitempty"`
}
