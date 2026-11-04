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
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Candidate filter 11 — zone restriction admissibility (ADR-0058 §5, §6, §9;
// amendment Part B.1).
//
// # It eliminates the ACTION, not a candidate, and the transition is terminal
//
// This is the one place the filter framing is escaped deliberately. Filter 11's
// answer does not vary by robot: the target zone either forbids the work or it does
// not. So a `Pending` requeue would report a standing, permanent refusal identically
// to a momentarily busy fleet, and "you told me never to go there" would be
// indistinguishable from "everything is busy" — precisely the confusion the
// downstream adapter's refusal wording was built to prevent, against a real household
// standing in a real room.
//
// # It reads the PROJECTION, never the tree
//
// FZ-R4: consumers read FleetZone.status.restrictions.effective[], never
// spec.restrictions and never the raw ancestor chain. The fold is defined once, in
// the Zone Controller, so "what is forbidden here" has one answer. Re-resolving the
// tree here would be a second implementation of FZ-R1 — and the specific way that
// goes wrong is named in FZ-R9: a filter-local resolver drifts to
// nearest-ancestor-wins, which does not conform.
//
// That is also why inheritance needs no code here. A leaf with no rule of its own,
// under a parent that denies, already carries the denial in its own projection with
// `inheritedFrom` naming the parent — the Zone Controller put it there.
//
// # Absorbing over the target relation, narrowing over the subtree
//
// A denial on the TARGET or an ANCESTOR refuses the whole action and is never
// trimmed. A denial on a DESCENDANT does not: the denied zone is removed from the
// resolved leaf set and every removed zone is NAMED on the action's status.
//
// Refusing a whole floor because one closet inside it is denied would make the floor
// undispatchable for a reason no operator can act on. Trimming the closet SILENTLY
// would be worse: it is how an operator comes to believe a region was serviced that
// was never entered, on a fleet where a truncated run and a complete one can be
// indistinguishable in the outcome report.
//
// AXIS 3 HAS THE SAME DESCENDANT STORY, IN A DIFFERENT SHAPE. ADR-0058 Amendment 2
// widens filter 12's scope to this same resolved leaf set, so a rule declared below the
// target is enforced there too. The shapes are deliberately not identical: THIS
// narrowing is zone-scoped — a denied leaf is out for everyone — where filter 12's is
// per (candidate, leaf), because its answer varies by robot. A leaf leaves filter 12's
// reachable set only when no candidate can honour it.

// zoneRestrictionVerdict is filter 11's answer for one action.
//
// A verdict is returned even when nothing is refused, because the narrowed leaf set
// and the list of excluded zones are results in their own right.
type zoneRestrictionVerdict struct {
	// refuse is true when the action must not be dispatched as written.
	refuse bool
	// terminal distinguishes a standing decision from a fact about today. Terminal
	// refusals go to Failed; non-terminal ones hold the action Pending.
	terminal bool
	// reason is the FleetAction.status.failureReason value (ADR-0058 §9).
	reason string
	// message is the sentence an operator reads.
	message string
	// leaves is the resolved leaf set with denied descendants removed (filter 2's
	// input). Nil when the action names no zone.
	leaves map[string]bool
	// narrowedOut names every descendant removed from the leaf set, in a stable order.
	narrowedOut []string
	// operating is FILTER 12's rule set, not filter 11's answer: the effective
	// OPERATING rules in force at each zone of the RESOLVED LEAF SET above (ADR-0058
	// Amendment 2). It rides here for the same reason leaves does — both are products of
	// the one FleetZone list this verdict was built from, and the reconciler needs them
	// where it needs the verdict. It is set by the call site (newDispatchFilterState),
	// never by evaluateZoneRestrictions, which keeps filter 11's decision function about
	// filter 11.
	operating *operatingRuleSet
}

// evaluateZoneRestrictions applies filter 11 to an action against an already-listed
// set of zones.
//
// Pure by construction: it takes the zone list rather than a client, so the decision
// is testable without a control plane and the READ — whose failure handling is the
// part that diverges from filters 2 and 8 — lives at the call site where that
// divergence can be commented.
func evaluateZoneRestrictions(action *fleetv1.FleetAction, zones []fleetv1.FleetZone) *zoneRestrictionVerdict {
	target := action.Spec.Zone
	if target == "" {
		return nil // an action naming no zone never reached filter 2 either
	}

	// FILTER 11 IS INERT IN A NAMESPACE THAT DECLARES NO RESTRICTIONS AT ALL, and this
	// is the compatibility hinge of the whole feature.
	//
	// ADR-0058's §6 argument for refusing an unknown zone is stated CONDITIONALLY:
	// resolving an unknown name to itself "was the right compatibility choice when the
	// only question was containment", and becomes wrong "ONCE A ZONE NAME CARRIES A
	// RESTRICTION SET". Where no zone carries one, the antecedent is false and the old
	// behaviour is still the right one — and the ADR promises exactly that elsewhere:
	// "no existing object changes meaning".
	//
	// Without this gate the filter is a broad regression with no safety benefit. Every
	// action naming a zone that has no FleetZone object would fail terminally, and every
	// action targeting a zone the Zone Controller has not reconciled yet would be held,
	// in deployments that never asked for any of it. Measured: 12 existing tests, none
	// of which is about restrictions.
	//
	// THIS IS NOT A SECOND RESOLVER. It reads spec.restrictions only to ask "is this
	// feature in use in this namespace", never to derive what is forbidden — that answer
	// comes from status.restrictions and nowhere else (FZ-R4). The moment any zone
	// declares any restriction, every clause below applies to every zone in the
	// namespace, including the ones that declare nothing: an unreadable or unreconciled
	// chain then holds the action, because a stale view of a namespace that HAS
	// restrictions may be hiding one.
	if !namespaceUsesRestrictions(zones) {
		return nil
	}

	byName := make(map[string]*fleetv1.FleetZone, len(zones))
	names := make([]string, 0, len(zones))
	for i := range zones {
		byName[zones[i].Name] = &zones[i]
		names = append(names, zones[i].Name)
	}
	sort.Strings(names)

	// ── ZoneUnknown ───────────────────────────────────────────────────────────
	// Resolving an unknown zone name to itself is the right compatibility choice for
	// containment and the WRONG one once a zone name carries a restriction set: a name
	// that resolves to nothing is a name whose restrictions cannot be evaluated.
	// Unknown is not permitted (ADR-0058 §6).
	self, known := byName[target]
	if !known {
		have := "no FleetZone exists in this namespace"
		if len(names) > 0 {
			have = "the zones that exist are: " + strings.Join(names, ", ")
		}
		return &zoneRestrictionVerdict{
			refuse: true, terminal: true, reason: fleetv1.ActionFailureZoneUnknown,
			message: fmt.Sprintf("spec.zone names %q, which is not a FleetZone in this namespace; %s",
				target, have),
		}
	}

	// ── ZoneRestrictionsUnresolved, on the target's own chain ─────────────────
	// Checked before any denial: an unresolvable chain means no decision can be made,
	// and reporting a denial from a half-read chain would claim the operator forbade
	// something they may not have.
	if why := unresolvedReason(self); why != "" {
		return &zoneRestrictionVerdict{
			refuse: true, terminal: false,
			reason: fleetv1.ActionFailureZoneRestrictionsUnresolved,
			message: fmt.Sprintf("the restriction set for zone %q could not be resolved (%s). "+
				"NO DECISION HAS BEEN MADE about this action: it is held, not judged, and becomes "+
				"dispatchable when the zone resolves", target, why),
		}
	}

	// ── ZoneEntryDenied, absorbing over the target relation ───────────────────
	// The target's own projection already carries any ancestor's denial, with
	// inheritedFrom naming the zone that declared it (FZ-R4).
	if e := findEffective(self, fleetv1.ZoneAxisEntry, ""); e != nil && e.Mode == fleetv1.ZoneRestrictionDenied {
		return &zoneRestrictionVerdict{
			refuse: true, terminal: true, reason: fleetv1.ActionFailureZoneEntryDenied,
			message: entryDeniedMessage(target, e),
		}
	}

	// ── ZoneCapabilityDenied ──────────────────────────────────────────────────
	for _, want := range action.Spec.RequiredCapabilities {
		e := findEffective(self, fleetv1.ZoneAxisCapability, want)
		if e == nil || e.Mode != fleetv1.ZoneRestrictionDenied {
			continue
		}
		because := ""
		if e.Reason != "" {
			because = " — " + e.Reason
		}
		return &zoneRestrictionVerdict{
			refuse: true, terminal: true, reason: fleetv1.ActionFailureZoneCapabilityDenied,
			message: fmt.Sprintf("capability %q is denied in zone %q by zone %q%s. "+
				"The zone is NOT CLOSED and remains open to work that does not need %s: "+
				"one capability is denied here, not the room",
				want, target, e.InheritedFrom, because, want),
		}
	}

	// ── Narrowing over the subtree ────────────────────────────────────────────
	leaves := resolveZoneLeaves(target, zones)
	var narrowedOut []string
	var unresolvedLeaves []string
	kept := map[string]bool{}
	for name := range leaves {
		leaf, ok := byName[name]
		if !ok {
			// resolveZoneLeaves resolves an unknown name to itself; the target was
			// proven known above, so this can only be the target with no children.
			kept[name] = true
			continue
		}
		if why := unresolvedReason(leaf); why != "" {
			unresolvedLeaves = append(unresolvedLeaves, fmt.Sprintf("%s (%s)", name, why))
			continue
		}
		if e := findEffective(leaf, fleetv1.ZoneAxisEntry, ""); e != nil && e.Mode == fleetv1.ZoneRestrictionDenied {
			narrowedOut = append(narrowedOut, name)
			continue
		}
		kept[name] = true
	}
	sort.Strings(narrowedOut)
	sort.Strings(unresolvedLeaves)

	// A descendant whose own chain does not resolve is held, not trimmed. Trimming it
	// would silently shrink the serviced region on the strength of a read that failed.
	if len(unresolvedLeaves) > 0 {
		return &zoneRestrictionVerdict{
			refuse: true, terminal: false,
			reason: fleetv1.ActionFailureZoneRestrictionsUnresolved,
			leaves: kept, narrowedOut: narrowedOut,
			message: fmt.Sprintf("zone %q contains zones whose restriction set could not be "+
				"resolved (%s). NO DECISION HAS BEEN MADE about this action: it is held, not "+
				"judged, rather than dispatched against a partial subtree",
				target, strings.Join(unresolvedLeaves, "; ")),
		}
	}

	if len(kept) == 0 {
		return &zoneRestrictionVerdict{
			refuse: true, terminal: true, reason: fleetv1.ActionFailureZoneEntryDenied,
			leaves: kept, narrowedOut: narrowedOut,
			message: fmt.Sprintf("zone %q is not itself denied, but EVERY zone within it is: %s. "+
				"The subtree is exhausted, so there is nowhere in %q to send a robot, on its own "+
				"or as part of anything else",
				target, strings.Join(narrowedOut, ", "), target),
		}
	}

	return &zoneRestrictionVerdict{leaves: kept, narrowedOut: narrowedOut}
}

// entryDeniedMessage builds the ZoneEntryDenied sentence.
//
// The wording deliberately carries three things §9 requires: the DECLARING zone
// (which may not be the target), the operator's own reason, and the absorbing
// property — "on its own or as part of anything else", which is imported in meaning
// from the shipped adapter refusal because it is the clause that stops a household
// believing a bundled request quietly included the forbidden room.
//
// It also states the honest limit. A control-plane refusal means the action was NOT
// DISPATCHED; it is not a claim that no robot will enter the zone.
func entryDeniedMessage(target string, e *fleetv1.EffectiveZoneRestriction) string {
	via := ""
	if e.InheritedFrom != "" && e.InheritedFrom != target {
		via = fmt.Sprintf(" (declared by ancestor zone %q, which %q is inside)", e.InheritedFrom, target)
	} else if e.InheritedFrom != "" {
		via = fmt.Sprintf(" (declared by zone %q)", e.InheritedFrom)
	}
	because := ""
	if e.Reason != "" {
		because = " — " + e.Reason
	}
	return fmt.Sprintf("zone %q is marked off limits%s%s, so this action will not be dispatched "+
		"there, on its own or as part of anything else. This refusal means the action was not "+
		"dispatched; it is not a guarantee that no robot enters the zone by some other path",
		target, because, via)
}

// unresolvedReason reports why a zone's restriction set cannot be trusted, or "".
//
// TWO conditions count, and the second is the one easy to miss:
//
//  1. RestrictionsResolved is explicitly False (FZ-R6) — the chain could not be read
//     or a parentZone does not resolve.
//  2. status.restrictions is NIL — the Zone Controller has not reconciled the zone at
//     all. That is UNKNOWN, not empty. Reading a nil projection as "unrestricted"
//     would make a brand-new denied zone dispatchable for exactly as long as it takes
//     the controller to catch up, which is the permissive direction.
func unresolvedReason(z *fleetv1.FleetZone) string {
	for i := range z.Status.Conditions {
		c := &z.Status.Conditions[i]
		if c.Type != fleetv1.ZoneCondRestrictionsResolved {
			continue
		}
		if c.Status == metav1.ConditionFalse {
			if c.Message != "" {
				return c.Message
			}
			return "RestrictionsResolved is False"
		}
		if z.Status.Restrictions == nil {
			return "RestrictionsResolved is True but status.restrictions is absent"
		}
		return ""
	}
	return "the Zone Controller has not published RestrictionsResolved for this zone yet"
}

// findEffective returns the resolved rule for one (axis, subject), or nil.
func findEffective(z *fleetv1.FleetZone, axis fleetv1.ZoneRestrictionAxis, subject string) *fleetv1.EffectiveZoneRestriction {
	if z.Status.Restrictions == nil {
		return nil
	}
	for i := range z.Status.Restrictions.Effective {
		e := &z.Status.Restrictions.Effective[i]
		if e.Axis == axis && e.Subject == subject {
			return e
		}
	}
	return nil
}

// namespaceUsesRestrictions reports whether any zone in the listing declares a
// restriction. See the gate in evaluateZoneRestrictions for why this exists and for
// the line it must not cross.
func namespaceUsesRestrictions(zones []fleetv1.FleetZone) bool {
	for i := range zones {
		r := zones[i].Spec.Restrictions
		if r == nil {
			continue
		}
		if r.Entry != nil || len(r.Capabilities) > 0 || len(r.Operating) > 0 {
			return true
		}
	}
	return false
}
