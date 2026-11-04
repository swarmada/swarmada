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
	"maps"
	"slices"
	"sort"
	"strings"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Candidate filter 12 — adapter operating-constraint support (ADR-0058 §5, §6,
// Amendment 2; amendment Part B.1).
//
// # ROBOT-SCOPED, and that is the whole difference from filter 11 beside it
//
// Filter 11 asks "may this action happen here at all". Its answer does not vary by
// robot, so it eliminates the ACTION and the transition is terminal. Filter 12 asks
// "can THIS robot honour what this zone requires", eliminates a CANDIDATE, and the
// action stays PENDING — because the answer is a fact about today's fleet, not a
// standing decision. A conforming adapter deployed tomorrow makes the same action
// dispatchable, and "no robot here can honour the rule" is a different sentence from
// "you told me never to go there". Same refusal vocabulary, different verb.
//
// This is the second of the two places ADR-0058 §5 requires a rule to be enforced:
// "a rule enforced in one place is a rule with a bypass". Without a control-plane
// filter, kubectl, swarmctl, a second adapter and any scheduler dispatch into the
// restricted zone unimpeded.
//
// # It reads the PROJECTION, never the tree
//
// FZ-R4. The effective rule set comes from
// FleetZone.status.restrictions.effective[], which already carries every ancestor's
// rule folded to one entry per subject with inheritedFrom naming the declaring zone.
// Inheritance therefore needs no code here. FZ-R9 names the specific way a
// filter-local resolver goes wrong: it drifts to nearest-ancestor-wins, which does
// not conform.
//
// # An empty declaration satisfies no subject, and that is a ruling, not a choice
//
// ADR-0058 Amendment 1 §A1.2 settles what a non-declaring adapter means, and this
// filter implements it rather than re-deciding it:
//
//	"Candidate filter 12 withholds a candidate robot when, for any effective
//	 operating rule in force in the target zone chain, the robot's
//	 status.honouredOperatingConstraints[] does not contain that rule's subject. An
//	 empty or absent honouredOperatingConstraints[] satisfies no subject."
//
// ADR-0058 Amendment 2 widens the SCOPE of that quote and nothing else: "the target
// zone chain" is read as the RESOLVED LEAF SET, evaluated per (candidate, leaf). What
// a non-declaring adapter means is untouched, and Amendment 2 does not narrow what
// filter 12 withholds either.
//
// The refused alternative — treat an adapter that declares nothing as "not
// participating, so dispatch" — is the permissive resolution of an unresolvable
// case, which decision 4 forbids ("the unresolvable case is never the permissive
// one"), and it would dispatch precisely the robot decision 1 exists for: the one
// transiting a ward with a live microphone under an adapter that models no operating
// subjects at all.
//
// What this does NOT protect against is stated in the same amendment (§A1.4, extended
// by Amendment 2 §A2.4) and is not restated here beyond the essential: withholding a
// robot from being ASSIGNED WORK does not stop the robot; a robot that PASSES this
// filter has an adapter that merely CLAIMS the subject — an attestation is a claim, not
// a measurement; and the widened scope still does not cover a zone the robot CROSSES on
// its way to the leaf, because the control plane does no path planning, so a transit
// corridor is not a zone this filter can see.

// operatingRuleSet is filter 12's rule set for one action: the effective OPERATING
// rules in force AT EACH ZONE THE ACTION CAN BE DISPATCHED INTO.
//
// # THE SCOPE IS THE RESOLVED LEAF SET, NOT THE TARGET ZONE CHAIN (ADR-0058 Amendment 2)
//
// Filter 2 admits a robot from anywhere in the SUBTREE of the action's zone. A filter 12
// that read only the target's own projection therefore had a bypass built into it, which
// is the thing ADR-0058 §5 exists to prevent: an action targets ward, ward declares
// nothing, ward-icu declares operating camera: Denied, and a robot sitting in ward-icu
// under an adapter that declares nothing is dispatched into the ICU with a camera —
// §1's own deciding case, tabulated in Amendment 2 §A2.1.
//
// # There is no union with the TARGET CHAIN, and there must not be one
//
// A leaf's status.restrictions.effective[] is the fold of ITSELF AND ITS ANCESTORS
// (FZ-R1, FZ-R4). So evaluating a candidate against the leaf it occupies ALREADY
// INCLUDES the target chain: there is nothing to merge and no second walk to make. Code
// that merged the target's rules in again here would have reintroduced resolution in the
// filter, which FZ-R4 forbids and FZ-R9 says drifts to nearest-ancestor-wins.
//
// Unioning every leaf's rules INTO the target's was the rejected first option: fail
// closed and cheap, and it makes a large target zone undispatchable because of a rule on
// one closet, destroying the property filter 11 is careful to preserve — "the zone is
// NOT CLOSED".
//
// # There IS a union over the zones CONTAINING THE ROBOT (FZ-R10, FZ-R11)
//
// currentZone is one arbitrated member of the robot's containment set: it answers where
// the robot REPORTS it is, not what BINDS it. A robot standing where a sterile corridor
// crosses a ward bay is inside both, and judging it at the winner alone discards the
// loser's rules — the permissive direction, and FZ-R10 says in terms that the winner alone
// does not conform. So forRobot folds the leaf's projection with the projection of every
// zone of status.containingZones[], each ALREADY RESOLVED, through the same fold the FA-R7
// push uses (unionEffectiveRestrictionsWithUnresolved). That is a union of answers, not a
// second resolver: no tree is walked, and inheritedFrom still names the declaring zone.
// It is per CANDIDATE, so it cannot close the target zone either: a corridor's rule binds
// the robots standing in the corridor and nobody else.
//
// # Per (candidate, leaf), which is NOT filter 11's shape
//
// Filter 11's narrowing is ZONE-SCOPED: an entry-denied leaf is removed from the leaf
// set and is out for everyone. This one is not, and that difference is the whole design.
// A robot is eligible for a leaf when its adapter honours every operating subject in
// force AT THAT LEAF, so the same leaf can drop out for one candidate and be kept for
// another; the leaf leaves the reachable set only when NO candidate can honour it, which
// falls out of evaluating each candidate where it stands rather than being computed. The
// next reader will assume this mirrors filter 11 exactly. It does not.
type operatingRuleSet struct {
	// target is the action's spec.zone, and the key the fallback below resolves to.
	target string
	// byZone holds the rules in force at each resolved leaf, plus the target itself.
	byZone map[string][]fleetv1.EffectiveZoneRestriction
	// zones is the FleetZone listing the set was built from. forRobot folds a candidate's
	// containing zones out of it, so a zone outside the action's subtree that contains the
	// robot is read from the same listing filters 2, 11 and 12 already consume.
	zones []fleetv1.FleetZone
}

// forRobot returns the rules one candidate is judged against by filter 12 and carries on
// AssignAction under FA-R2, and the zones containing it whose rules could not be read.
//
// THE RULES AT ITS LEAF, FOLDED PER FZ-R11 WITH EVERY ZONE OF status.containingZones[].
// The leaf is at()'s answer, the target's chain included, and for a pinned robot standing
// outside the subtree it is the target's chain itself; the containment set is added to it
// whichever it is. A robot whose containment set names nothing beyond that zone is judged
// exactly as at() judges it, with no fold at all.
//
// The second return is FZ-R6 on the dispatch path. A containing zone that is unknown, has
// published no projection, or reports RestrictionsResolved: False contributes no rule to
// the fold, and the caller must not read that as "no rule": the robot cannot be shown to
// honour a constraint nobody could read, so filter 12 withholds it. The FA-R7 push reads
// the same return and sends it as unresolved_zones instead, because a push takes no
// decision; a dispatch does.
func (s *operatingRuleSet) forRobot(robot *fleetv1.Robot) ([]fleetv1.EffectiveZoneRestriction, []string) {
	if s == nil {
		return nil, nil
	}
	leaf := robot.Status.CurrentZone
	if _, ok := s.byZone[leaf]; !ok {
		leaf = s.target // at()'s fallback, for the same pinned robot
	}
	names := []string{leaf}
	for _, z := range robot.Status.ContainingZones {
		if z != leaf {
			names = append(names, z)
		}
	}
	if len(names) == 1 {
		return s.byZone[leaf], nil
	}
	folded, unresolved := unionEffectiveRestrictionsWithUnresolved(s.zones, names)
	// Only the OPERATING axis: entry and capability rules are filter 11's, decided for the
	// action before any candidate is judged.
	var inForce []fleetv1.EffectiveZoneRestriction
	for _, e := range folded {
		if e.Axis == fleetv1.ZoneAxisOperating {
			inForce = append(inForce, e)
		}
	}
	return inForce, unresolved
}

// at returns the rules one candidate standing in zone is judged against.
//
// A zone that is not in the set falls back to the TARGET'S own projection, and that
// fallback is load-bearing exactly once: a robot the action PINS skips filter 2
// (RFC-0001 filter 6) and so may be standing outside the subtree entirely. Filter 12
// does not exempt a pin — whether a vendor's adapter honours a camera constraint is not
// a judgement an operator can assert into being, and exempting a pin would make
// spec.robotSelector an off switch for the zone's rule. The target's chain is the floor
// such a robot is held to, which is what this filter checked everywhere before the scope
// widened.
func (s *operatingRuleSet) at(zone string) []fleetv1.EffectiveZoneRestriction {
	if s == nil {
		return nil
	}
	if rules, ok := s.byZone[zone]; ok {
		return rules
	}
	return s.byZone[s.target]
}

// operatingRequirement is one distinct rule in force, and every zone of the reachable
// set it is in force in.
type operatingRequirement struct {
	rule  fleetv1.EffectiveZoneRestriction
	zones []string
}

// requirements folds the set to one entry per distinct rule, in a stable order, over the
// reachable set and every zone in also: the zones containing a withheld candidate, which
// since FZ-R10 bind it from outside the action's subtree as well as from inside it. It
// returns, beside them, the zones of also whose rules could not be read (FZ-R6), sorted.
//
// Stable because the string it feeds is patched onto a Pending action on every requeue:
// rules sort by subject then declaring zone, and the zones each is in force in sort by
// name, so an unchanged fleet and an unchanged zone tree produce a byte-identical
// message and an empty patch (RA-1).
func (s *operatingRuleSet) requirements(also []string) ([]operatingRequirement, []string) {
	if s == nil {
		return nil, nil
	}
	inForce := s.byZone
	var unreadable []string
	if len(also) > 0 {
		inForce = maps.Clone(s.byZone)
		for _, name := range also {
			if _, seen := inForce[name]; seen {
				continue
			}
			rules, unresolved := unionEffectiveRestrictionsWithUnresolved(s.zones, []string{name})
			if len(unresolved) > 0 {
				unreadable = append(unreadable, name)
				continue
			}
			for _, e := range rules {
				if e.Axis == fleetv1.ZoneAxisOperating {
					inForce[name] = append(inForce[name], e)
				}
			}
		}
		sort.Strings(unreadable)
	}
	byRule := map[string]*operatingRequirement{}
	for zone, rules := range inForce {
		for _, e := range rules {
			k := strings.Join([]string{e.Subject, string(e.Mode), e.InheritedFrom, e.Reason}, "\x00")
			req, seen := byRule[k]
			if !seen {
				req = &operatingRequirement{rule: e}
				byRule[k] = req
			}
			req.zones = append(req.zones, zone)
		}
	}
	out := make([]operatingRequirement, 0, len(byRule))
	for _, req := range byRule {
		sort.Strings(req.zones)
		out = append(out, *req)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].rule.Subject != out[j].rule.Subject {
			return out[i].rule.Subject < out[j].rule.Subject
		}
		return out[i].rule.InheritedFrom < out[j].rule.InheritedFrom
	})
	return out, unreadable
}

// effectiveOperatingRules extracts one zone's axis-3 projection, sorted by subject.
//
// Sorted so the exclusion reason — which is folded into a status message a Pending
// action re-patches on every requeue — is a stable string for a stable fleet (RA-1).
func effectiveOperatingRules(z *fleetv1.FleetZone) []fleetv1.EffectiveZoneRestriction {
	if z.Status.Restrictions == nil {
		return nil
	}
	var inForce []fleetv1.EffectiveZoneRestriction
	for i := range z.Status.Restrictions.Effective {
		if e := z.Status.Restrictions.Effective[i]; e.Axis == fleetv1.ZoneAxisOperating {
			inForce = append(inForce, e)
		}
	}
	sort.Slice(inForce, func(i, j int) bool { return inForce[i].Subject < inForce[j].Subject })
	return inForce
}

// operatingConstraintsInForce returns the effective OPERATING rules in force at every
// zone of the action's RESOLVED LEAF SET — and at the target itself — or nil when
// filter 12 has nothing to say.
//
// leaves is filter 2's input, already computed once in this same call and already
// NARROWED by filter 11 (an entry-denied descendant is gone from it before this runs, so
// no candidate is held for a rule in a zone nobody may enter at all). There is no second
// tree walk here and no second listing.
//
// # FILTER 12 IS INERT IN A NAMESPACE THAT DECLARES NO RESTRICTIONS, and this is not
// optional
//
// It reuses filter 11's namespaceUsesRestrictions rather than writing a parallel
// one, and inherits its discipline: spec.restrictions is read for PRESENCE ONLY —
// "is this feature in use here" — never to derive what is forbidden. That answer
// comes from status.restrictions and nowhere else (FZ-R4).
//
// MEASURED, BECAUSE THE SHAPE DIFFERS FROM FILTER 11'S AND THE DIFFERENCE MATTERS.
// Filter 12's inertness is carried mostly by the projections it reads: a SUBTREE in
// which no zone carries an operating rule in its effective set puts nothing in force and
// withholds nobody, whatever the rest of the namespace declares — which is why the
// function returns nil rather than an empty set when it finds none. Filter 11 has no
// such structural floor: its ZoneUnknown and unresolved clauses fire on a zone that
// declares nothing at all, which is why it needed the gate to avoid being a broad
// regression.
//
// That floor is NARROWER since ADR-0058 Amendment 2 — the scope is the whole reachable
// leaf set now, so a rule on any one leaf puts the subject in force for the candidates
// standing in THAT leaf. It is still a floor, because it is still per zone: a leaf that
// carries no rule withholds nobody, and the rules of its siblings are not folded in.
//
// It is narrower again since FZ-R10 reached this filter: a zone outside the subtree that
// CONTAINS a candidate binds it, so the set is nil only when no zone of the namespace
// carries an operating rule and every zone resolved. It is still per candidate: a zone's
// rule binds the robots it contains and nobody else.
//
// The gate is kept, and not as decoration. It is the only thing covering the window in
// which a projection is STALE: an operator deletes spec.restrictions and, until the
// Zone Controller re-reconciles, status.restrictions still carries the withdrawn rule.
// Presence in spec is what says the feature is in use; the gate makes the withdrawal
// take effect at the spec rather than one reconcile later
// (TestFilter12_AStaleProjectionWithNoSpecBehindItWithholdsNobody). Consistent with
// ADR-0058 §11, which states plainly that a principal who may write the declaring
// FleetZone may remove the wall at its source.
//
// # The nil returns are safe because filter 11 has already refused
//
// An unknown target zone and an unresolvable chain both return nil here, which would
// be fail-OPEN if this ran alone. It does not: filter 11 evaluates the same zone list
// first and refuses the action outright in both cases — ZoneUnknown terminally,
// ZoneRestrictionsUnresolved as a hold — so the controller returns before any
// candidate is selected. The call site makes that ordering explicit by computing this
// only when filter 11 did not refuse. The guards are kept here as well, so this
// function is still correct if it is ever called on its own.
func operatingConstraintsInForce(
	action *fleetv1.FleetAction, zones []fleetv1.FleetZone, leaves map[string]bool,
) *operatingRuleSet {
	target := action.Spec.Zone
	if target == "" {
		return nil // an action naming no zone never reached filter 2 either
	}
	if !namespaceUsesRestrictions(zones) {
		return nil
	}
	byName := make(map[string]*fleetv1.FleetZone, len(zones))
	for i := range zones {
		byName[zones[i].Name] = &zones[i]
	}
	self, known := byName[target]
	if !known {
		return nil // ZoneUnknown — filter 11 has already refused the action terminally
	}
	if unresolvedReason(self) != "" {
		return nil // ZoneRestrictionsUnresolved — filter 11 has already held the action
	}

	set := &operatingRuleSet{target: target, byZone: map[string][]fleetv1.EffectiveZoneRestriction{}, zones: zones}
	// The target is always a key, so at() has a floor to fall back to for a pinned robot
	// standing outside the subtree. When the target is itself a leaf this is also the only
	// entry, and the set is exactly what this filter computed before the scope widened.
	set.byZone[target] = effectiveOperatingRules(self)

	for name := range leaves {
		if name == target {
			continue
		}
		leaf, ok := byName[name]
		if !ok {
			// resolveZoneLeaves resolves an unknown name to itself; the target was proven
			// known above, so this cannot be reached for a target with children. Skipping is
			// safe rather than fail-open: filter 11 refuses an unknown target outright.
			continue
		}
		if unresolvedReason(leaf) != "" {
			// Filter 11 HOLDS the whole action on an unresolvable descendant rather than
			// trimming it, so this leaf never reaches a dispatch decision. Reading its
			// projection here would be reading a chain that did not resolve.
			continue
		}
		set.byZone[name] = effectiveOperatingRules(leaf)
	}

	for _, rules := range set.byZone {
		if len(rules) > 0 {
			return set
		}
	}
	// No operating rule in the reachable set. A zone OUTSIDE it can still contain a
	// candidate and bind it (FZ-R10), so the set is kept while any zone of the listing
	// carries an operating rule or cannot be read; forRobot decides per candidate whether
	// one of those zones contains it.
	for i := range zones {
		if unresolvedReason(&zones[i]) != "" || len(effectiveOperatingRules(&zones[i])) > 0 {
			return set
		}
	}
	// No operating rule anywhere in the namespace: filter 12 has nothing to say, and says
	// it the same way it did before — with a nil set that withholds nobody.
	return nil
}

// excludedByUnreadableContainment applies filter 12's FZ-R6 half to one robot: a zone
// containing it whose rules could not be read (forRobot's second return) withholds it.
//
// A held candidate, not a held action, and that is the difference from filter 11's
// unresolved clause: the unreadable zone is one THIS robot stands in, which says nothing
// about any other candidate, so the action stays Pending and is offered to the rest.
func excludedByUnreadableContainment(unresolved []string) *scheduler.Exclusion {
	if len(unresolved) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(unresolved))
	for _, name := range unresolved {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return &scheduler.Exclusion{
		Filter: filterOperatingConstraint,
		Reason: fmt.Sprintf("the operating rules in force in zone %s, which contains the robot, could "+
			"not be resolved, so the robot cannot be shown to honour them (an unresolvable "+
			"restriction is not an absent one)", strings.Join(quoted, ", ")),
	}
}

// excludedByOperatingConstraint applies filter 12 to one robot.
//
// EVERY subject in force must be declared, not any one of them: a robot that can turn
// its camera off and cannot silence its microphone has not satisfied a chain that
// restricts both.
//
// Limited and Required count exactly as Denied does. An adapter that cannot control a
// subject at all can neither hold it under a bound nor keep it running, so the mode
// changes what the adapter must DO and not whether it must be able to.
//
// It writes nothing and reads nothing but the two values passed in. A candidate
// filter decides eligibility; it never transitions a robot (RA-1).
func excludedByOperatingConstraint(robot *fleetv1.Robot, inForce []fleetv1.EffectiveZoneRestriction) *scheduler.Exclusion {
	if len(inForce) == 0 {
		return nil
	}
	// ADR-0058 Amendment 1 §A1.2: an empty or absent list satisfies no subject. This
	// line is the ruling — nil, empty and "declares other subjects" all reach the same
	// slices.Contains miss below, and there is deliberately no branch that treats an
	// absent declaration as participation.
	declared := robot.Status.HonouredOperatingConstraints

	var missing []fleetv1.EffectiveZoneRestriction
	for _, e := range inForce {
		if !slices.Contains(declared, e.Subject) {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	wants := make([]string, 0, len(missing))
	for _, e := range missing {
		via := e.InheritedFrom
		if via == "" {
			via = "an unnamed zone"
		}
		part := fmt.Sprintf("%s (%s, declared by zone %q)", e.Subject, strings.ToLower(string(e.Mode)), via)
		if e.Reason != "" {
			part += " — " + e.Reason
		}
		wants = append(wants, part)
	}
	// What the robot CAN honour, so an operator reads the GAP rather than inferring it
	// from the requirement alone. "declares nothing" is spelled out because it is the
	// overwhelmingly common case and the one with a different fix: it needs a rebuilt
	// or replaced adapter, where a partial declaration needs a subject added.
	can := "its adapter declares no operating subjects at all"
	if len(declared) > 0 {
		can = "its adapter declares only: " + strings.Join(declared, ", ")
	}
	return &scheduler.Exclusion{
		Filter: filterOperatingConstraint,
		Reason: fmt.Sprintf("robot cannot be shown to honour %s; %s",
			strings.Join(wants, "; "), can),
	}
}

// operatingConstraintMessage is the action-level sentence for
// ZoneOperatingConstraintUnmet, built when filter 12 withheld candidates and nothing
// was dispatched.
//
// It carries the four things §9 requires of this refusal and one it requires of all
// five. WHAT WAS REQUIRED: every subject in force and the zone that declared it,
// which is very often not the target. WHAT THE CANDIDATES COULD HONOUR: named per
// robot, because "no robot can" and "this robot cannot" send an operator to different
// places. HOW MANY were withheld for it. And that the action is HELD, NOT JUDGED —
// the distinction between a fact about today and a standing decision, which is the
// whole reason this reason is Pending and ZoneEntryDenied is not.
//
// # IT NAMES THE ZONES THE RULES ARE IN FORCE IN, not only the target
//
// Since ADR-0058 Amendment 2 the scope is the resolved leaf set, so the rule that
// withheld every candidate is very often declared on a zone BELOW the one the operator
// asked for. Naming only the target would leave them reading "ward requires camera" with
// nothing on ward saying so, and they would go hunting for an adapter setting. This
// names every zone of the reachable set each rule is in force in, the way filter 11's
// subtree-exhausted message names every narrowed zone.
//
// It is a stable string over a stable fleet: the rules and their zones are sorted by
// requirements() and the robots are sorted here, so a Pending action re-patching this
// message every requeue produces no etcd write while nothing changes (RA-1).
func operatingConstraintMessage(
	action *fleetv1.FleetAction, inForce *operatingRuleSet, withheld []dispatchExclusion,
	candidates []fleetv1.Robot,
) string {
	names := make([]string, 0, len(withheld))
	held := map[string]bool{}
	for _, w := range withheld {
		if w.Filter == filterOperatingConstraint {
			names = append(names, w.Robot)
			held[w.Robot] = true
		}
	}
	sort.Strings(names)

	// The zones containing a withheld robot, which can bind it from outside the action's
	// subtree (FZ-R10). Without them the sentence would name only the reachable set's rules
	// and send the operator to a zone with nothing in it.
	var containing []string
	for i := range candidates {
		if held[candidates[i].Name] {
			containing = append(containing, candidates[i].Status.ContainingZones...)
		}
	}
	slices.Sort(containing)
	reqs, unreadable := inForce.requirements(slices.Compact(containing))

	subjects := make([]string, 0, len(reqs)+1)
	for _, r := range reqs {
		part := fmt.Sprintf("%s (declared by zone %q", r.rule.Subject, r.rule.InheritedFrom)
		if r.rule.Reason != "" {
			part += ", " + r.rule.Reason
		}
		part += fmt.Sprintf("), in force in: %s", strings.Join(r.zones, ", "))
		subjects = append(subjects, part)
	}
	if len(unreadable) > 0 {
		subjects = append(subjects, fmt.Sprintf("the rules of %s, which could not be resolved",
			strings.Join(unreadable, ", ")))
	}

	return fmt.Sprintf(
		"zone %q requires an adapter that honours %s, and %d candidate robot(s) could not be shown "+
			"to: %s. The action is HELD, not judged — this is a fact about today's fleet, not a "+
			"standing refusal, and a conforming adapter deployed tomorrow makes this same action "+
			"dispatchable. Withholding is a refusal to assign work; it does not prevent a robot "+
			"entering the zone by some other path, and the control plane does no path planning, so "+
			"a zone the robot merely crosses on the way is not a zone this filter can see",
		action.Spec.Zone, strings.Join(subjects, "; "), len(names), strings.Join(names, ", "))
}

// withheldByOperatingConstraint reports whether filter 12 eliminated any candidate.
func withheldByOperatingConstraint(withheld []dispatchExclusion) bool {
	for _, w := range withheld {
		if w.Filter == filterOperatingConstraint {
			return true
		}
	}
	return false
}
