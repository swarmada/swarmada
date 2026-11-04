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
	"strconv"
	"strings"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Zone restriction resolution — ADR-0058 §3, amendment FZ-R1 … FZ-R3.
//
// This file is a PURE FOLD over an ancestor chain: no client, no context, no
// status writes. The Zone Controller calls it and projects the result; nothing
// else may re-implement it, because FZ-R4's whole point is that "what is forbidden
// here" is decided in one place.
//
// # Why this is the enforcement, and admission is only the diagnostic
//
// FZ-R9 is explicit that the resolver — not the webhook — is what has to hold:
// admission tells an operator NOW, but the resolver has to stay correct on an
// object admission never saw. Restored from a backup, applied before the webhook
// was installed, written while failurePolicy was Ignore, or simply raced past the
// check. So this fold errs restrictive unconditionally, and never assumes any
// admission rule ran.
//
// # The one rule that generates all the others
//
// THE FOLD NEVER DROPS A CONSTRAINT IT READ. That single sentence is FZ-R2
// operationalised, and every case below follows from it:
//
//   - Denied absorbs everything, because Denied forbids the subject outright and
//     so is stronger than any bound that would otherwise be discarded with it.
//   - Between two Limited rules the tighter max wins and allow-lists INTERSECT —
//     never union, because an allow-list is a denial of everything unlisted and a
//     union would re-permit what an ancestor forbade.
//   - Required plus Denied is a contradiction resolving to Denied (FZ-R3).
//   - Required plus Limited keeps BOTH: see the note on foldRule.
//
// A nearest-ancestor-wins resolution does not conform (FZ-R1), and it is the
// intuitive implementation — which is why the test file sweeps a chain space
// rather than asserting a few examples.

// restrictionKey identifies one (axis, subject). Entry has no subject, so its key
// carries an empty one; that is deliberate rather than a placeholder, because
// entry is a statement about the zone and not about a named thing in it.
func restrictionKey(axis fleetv1.ZoneRestrictionAxis, subject string) string {
	return string(axis) + "/" + subject
}

// zoneRestrictionContradiction is a Denied/Required pair on one subject somewhere
// in one chain (FZ-R3). BOTH declaring zones are carried: an operator told only one
// of them cannot act, because they do not know which edit to make, whether they own
// the other zone, or whether the other rule is the one that matters.
type zoneRestrictionContradiction struct {
	Axis       fleetv1.ZoneRestrictionAxis
	Subject    string
	DeniedBy   string
	RequiredBy string
}

// String renders the contradiction for a condition message and an event.
func (c zoneRestrictionContradiction) String() string {
	subj := c.Subject
	if subj == "" {
		subj = strings.ToLower(string(c.Axis))
	}
	return fmt.Sprintf("%s %q is Denied by zone %q and Required by zone %q; resolved to Denied",
		strings.ToLower(string(c.Axis)), subj, c.DeniedBy, c.RequiredBy)
}

// resolveZoneRestrictions folds an ancestor chain into the effective restriction
// set for the zone at its end.
//
// chain is ROOT-DOWNWARD and INCLUDES the zone itself: chain[0] is the outermost
// ancestor, chain[len-1] is the zone being resolved. That order is not cosmetic —
// it is how ties are broken in favour of the outermost declaration, which is the
// one a local edit cannot remove and therefore the one an operator most needs
// named in inheritedFrom.
//
// The returned slice is sorted (axis, then subject) so an unchanged chain produces
// a byte-identical projection and the caller's DeepEqual guard suppresses the
// write. That is not tidiness: it is what keeps FZ-R5 true in practice.
func resolveZoneRestrictions(chain []fleetv1.FleetZone) ([]fleetv1.EffectiveZoneRestriction, []zoneRestrictionContradiction) {
	// acc[key] is the fold so far; declaredBy tracks the zones that named each mode,
	// which is what FZ-R3 needs and what inheritedFrom is chosen from.
	acc := map[string]*fleetv1.EffectiveZoneRestriction{}
	deniedBy := map[string]string{}
	requiredBy := map[string]string{}
	boundBy := map[string]string{}

	for i := range chain {
		z := &chain[i]
		if z.Spec.Restrictions == nil {
			continue
		}
		r := z.Spec.Restrictions

		if r.Entry != nil {
			foldRule(acc, deniedBy, requiredBy, boundBy, z.Name, fleetv1.EffectiveZoneRestriction{
				Axis: fleetv1.ZoneAxisEntry, Mode: r.Entry.Mode,
				Reason: r.Entry.Reason, InheritedFrom: z.Name,
			})
		}
		for _, c := range r.Capabilities {
			foldRule(acc, deniedBy, requiredBy, boundBy, z.Name, fleetv1.EffectiveZoneRestriction{
				Axis: fleetv1.ZoneAxisCapability, Subject: c.Name, Mode: c.Mode,
				Reason: c.Reason, InheritedFrom: z.Name,
			})
		}
		for _, o := range r.Operating {
			foldRule(acc, deniedBy, requiredBy, boundBy, z.Name, fleetv1.EffectiveZoneRestriction{
				Axis: fleetv1.ZoneAxisOperating, Subject: o.Subject, Mode: o.Mode,
				Max: o.Max, Allowed: append([]string(nil), o.Allowed...),
				Reason: o.Reason, InheritedFrom: z.Name,
			})
		}
	}

	out := make([]fleetv1.EffectiveZoneRestriction, 0, len(acc))
	var contradictions []zoneRestrictionContradiction
	for key, e := range acc {
		// An allow-list that intersected to EMPTY permits nothing, which is a denial
		// and must be emitted as one.
		//
		// This is not tidying. `allowed` carries `omitempty`, so an empty list
		// serialises away and reads back as "no allow-list declared" — which means
		// permit EVERYTHING, the exact inversion of what the chain said. The FZ-R2
		// sweep found this: two non-overlapping allow-lists on one subject produced a
		// Limited rule with no list, weaker than either input. Denied is the only
		// expressible answer that is not weaker than what was read.
		if e.Mode == fleetv1.ZoneRestrictionLimited && e.Allowed != nil && len(e.Allowed) == 0 {
			e.Mode = fleetv1.ZoneRestrictionDenied
			e.Max = ""
			e.Allowed = nil
			if b := boundBy[key]; b != "" {
				e.InheritedFrom = b
			}
			e.Reason = strings.TrimSpace(e.Reason + " (no value is permitted by every allow-list in the zone chain)")
			deniedBy[key] = e.InheritedFrom
		}
		if d, okD := deniedBy[key]; okD {
			if rq, okR := requiredBy[key]; okR {
				contradictions = append(contradictions, zoneRestrictionContradiction{
					Axis: e.Axis, Subject: e.Subject, DeniedBy: d, RequiredBy: rq,
				})
			}
		}
		out = append(out, *e)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Axis != out[j].Axis {
			return out[i].Axis < out[j].Axis
		}
		return out[i].Subject < out[j].Subject
	})
	sort.Slice(contradictions, func(i, j int) bool {
		if contradictions[i].Axis != contradictions[j].Axis {
			return contradictions[i].Axis < contradictions[j].Axis
		}
		return contradictions[i].Subject < contradictions[j].Subject
	})
	return out, contradictions
}

// foldRule merges one declared rule into the accumulator for its key.
//
// The mode lattice is Denied > Required > Limited > absent, and the ONLY place
// that ordering is written down is here.
//
//   - Denied wins outright and clears the parameters, because a forbidden subject
//     needs no bound; nothing is lost, which is why this is not a dropped constraint.
//   - Required beats Limited on the MODE. It does not discard the bound: a Required
//     rule that also carries a merged max or allow-list means "this must be running,
//     and within these limits". ADR-0058 defines the Denied/Required contradiction
//     and the Limited/Limited merge; keeping both on Required/Limited is the only
//     reading that cannot emit something weaker than an input, which is the rule
//     FZ-R2 actually states, and it is the answer ADR-0058 records for the pair.
//     Pinned by TestResolveZoneRestrictions_RequiredPlusLimitedCarriesBoth.
//   - Bounds always merge, whatever the mode: min of the maxes, intersection of the
//     allow-lists.
//
// Ties on the mode go to the incumbent, which — because the chain is walked
// root-downward — is the OUTERMOST zone that declared it.
func foldRule(
	acc map[string]*fleetv1.EffectiveZoneRestriction,
	deniedBy, requiredBy, boundBy map[string]string,
	zoneName string,
	in fleetv1.EffectiveZoneRestriction,
) {
	key := restrictionKey(in.Axis, in.Subject)
	switch in.Mode {
	case fleetv1.ZoneRestrictionDenied:
		if _, seen := deniedBy[key]; !seen {
			deniedBy[key] = zoneName
		}
	case fleetv1.ZoneRestrictionRequired:
		if _, seen := requiredBy[key]; !seen {
			requiredBy[key] = zoneName
		}
	}

	cur, ok := acc[key]
	if !ok {
		cp := in
		acc[key] = &cp
		return
	}

	// Bounds intersect regardless of which mode wins, so a bound is never lost by
	// losing a mode comparison. Whichever side actually NARROWS is recorded, because
	// that is the zone an operator must edit to change the limit that is in force —
	// naming the zone that declared a looser bound would send them to an edit that
	// changes nothing.
	if newMax := tighterMax(cur.Max, in.Max); newMax != cur.Max {
		cur.Max = newMax
		boundBy[key] = zoneName
	}
	if newAllowed := intersectAllowed(cur.Allowed, in.Allowed); !sameStrings(newAllowed, cur.Allowed) {
		cur.Allowed = newAllowed
		boundBy[key] = zoneName
	}

	if modeRank(in.Mode) > modeRank(cur.Mode) {
		cur.Mode = in.Mode
		cur.Reason = in.Reason
		cur.InheritedFrom = in.InheritedFrom
	} else if cur.Mode == fleetv1.ZoneRestrictionLimited && boundBy[key] == zoneName {
		// A Limited rule is identified by its bound, not by who first mentioned it.
		cur.Reason = in.Reason
		cur.InheritedFrom = in.InheritedFrom
	}
	if cur.Mode == fleetv1.ZoneRestrictionDenied {
		// Denied forbids the subject outright: a bound alongside it would be noise,
		// and would suggest the subject is permitted within it.
		cur.Max = ""
		cur.Allowed = nil
		// inheritedFrom must name the zone that DENIED, not merely the first zone
		// seen — the outermost denial is the one a local edit cannot remove.
		if d := deniedBy[key]; d != "" {
			cur.InheritedFrom = d
			if r := reasonFromZone(acc, key, d); r != "" {
				cur.Reason = r
			}
		}
	}
}

// reasonFromZone is a narrow helper: when Denied wins a fold the reason must come
// from the denying zone, and the accumulator may currently hold a different zone's
// reason. It returns "" when the current entry already belongs to that zone.
func reasonFromZone(acc map[string]*fleetv1.EffectiveZoneRestriction, key, zoneName string) string {
	if e, ok := acc[key]; ok && e.InheritedFrom == zoneName {
		return e.Reason
	}
	return ""
}

// modeRank orders the modes by restrictiveness. Absence is rank 0 and is never
// stored, so it never appears here.
func modeRank(m fleetv1.ZoneRestrictionMode) int {
	switch m {
	case fleetv1.ZoneRestrictionDenied:
		return 3
	case fleetv1.ZoneRestrictionRequired:
		return 2
	case fleetv1.ZoneRestrictionLimited:
		return 1
	default:
		return 0
	}
}

// tighterMax returns the smaller of two decimal bounds. An empty string means "no
// bound declared", which loses to any bound — never the other way round, or a zone
// declaring nothing would erase an ancestor's limit. An unparseable bound is
// treated as no bound at all on that side ONLY: the schema pattern makes it
// unreachable through admission, and dropping the other side's real bound to
// honour a malformed one would be the weakening FZ-R2 forbids.
func tighterMax(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	av, aerr := strconv.ParseFloat(a, 64)
	bv, berr := strconv.ParseFloat(b, 64)
	switch {
	case aerr != nil && berr != nil:
		return a
	case aerr != nil:
		return b
	case berr != nil:
		return a
	case bv < av:
		return b
	default:
		return a
	}
}

// intersectAllowed returns the INTERSECTION of two allow-lists, and this is the
// function most likely to be "fixed" into a union by someone reading it as a
// permission list. It is not one: `allowed` denies everything it does not name, so
// the union of two lists re-permits what each list separately forbade.
//
// A nil list means "no allow-list declared on this side" and is the identity. An
// EMPTY (non-nil) intersection means the chain permits nothing, which is a real
// outcome and is preserved as an empty non-nil slice so it is distinguishable from
// "no list".
func intersectAllowed(a, b []string) []string {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	inB := make(map[string]bool, len(b))
	for _, v := range b {
		inB[v] = true
	}
	out := []string{}
	for _, v := range a {
		if inB[v] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// sameStrings reports whether two string slices are element-wise equal, treating
// nil and empty as DIFFERENT — the distinction between "no allow-list" and "an
// allow-list that permits nothing" is exactly the one this file must not lose.
func sameStrings(a, b []string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── The containment union (ADR-0059) ─────────────────────────────────────────
//
// A robot standing inside several overlapping zones is bound by ALL of them. This is
// the second half of resolution: resolveZoneRestrictions folds ONE zone's ancestor
// chain, and this folds the already-resolved projections of every zone containing the
// robot.
//
// IT IS A UNION OF ANSWERS, NEVER A SECOND WALK OF THE TREE. Each input is a zone's
// status.restrictions.effective[], which has already folded that zone's own ancestors
// (FZ-R1/FZ-R4). Re-resolving here would be a second implementation of the fold, which
// FZ-R4 forbids, and FZ-R9 names the specific way it goes wrong: it drifts to
// nearest-ancestor-wins. Feeding already-resolved values into the SAME foldRule is what
// makes that structurally impossible — there is no second resolver to drift, because
// there is no second resolver.
//
// The fold is the same one for a reason beyond economy. Its single governing sentence —
// THE FOLD NEVER DROPS A CONSTRAINT IT READ — is exactly what the union needs: denial
// absorbs across containing zones as it does along a chain, two bounds intersect rather
// than one overriding, and Denied plus Required is the same contradiction resolved the
// same way. Writing a second, union-specific merge would have been a second place for
// those four rules to be stated, and the one that drifted would have been the one
// deciding what a robot may run.

// unionEffectiveRestrictions folds the effective sets of every named zone into the set
// in force for a robot contained by all of them.
//
// containing is the containment set (zone.DeriveContainingZones). A zone named here but
// absent from zones, or carrying no projection, contributes nothing — and the caller
// that needs to know which those were uses unionEffectiveRestrictionsWithUnresolved,
// because FZ-R6 says an unresolvable restriction is not an absent one.
//
// Returns nil, never an empty slice, when nothing is in force: absence of a rule is
// permission at every axis (ADR-0058 §2), and a caller must be able to tell "no rule"
// from "a rule that permits nothing" without inspecting length.
func unionEffectiveRestrictions(zones []fleetv1.FleetZone, containing []string) []fleetv1.EffectiveZoneRestriction {
	out, _ := unionEffectiveRestrictionsWithUnresolved(zones, containing)
	return out
}

// unionEffectiveRestrictionsWithUnresolved is unionEffectiveRestrictions plus the names
// of the containing zones whose own resolution did not succeed.
//
// FZ-R6 IS WHY THE SECOND RETURN EXISTS. A zone whose ancestor chain could not be read
// publishes RestrictionsResolved: False, and its projection is either stale or absent.
// Silently omitting it from the union would publish a set weaker than the truth without
// saying so — an unresolvable restriction is not an absent restriction. The caller fails
// closed on a non-empty second return; this function does not decide that for it,
// because the right response differs between a dispatch decision and a push.
//
// A zone with a nil projection counts as unresolved for the same reason
// unresolvedReason treats it so: the Zone Controller has not reconciled it at all, which
// is UNKNOWN rather than empty, and reading it as unrestricted would make a brand-new
// denied zone permissive for exactly as long as the controller takes to catch up.
func unionEffectiveRestrictionsWithUnresolved(
	zones []fleetv1.FleetZone, containing []string,
) ([]fleetv1.EffectiveZoneRestriction, []string) {
	if len(containing) == 0 {
		return nil, nil
	}
	byName := make(map[string]*fleetv1.FleetZone, len(zones))
	for i := range zones {
		byName[zones[i].Name] = &zones[i]
	}

	acc := map[string]*fleetv1.EffectiveZoneRestriction{}
	deniedBy := map[string]string{}
	requiredBy := map[string]string{}
	boundBy := map[string]string{}
	var unresolved []string

	// Sorted so the fold visits the containing zones in one order whatever order the
	// caller derived them in. foldRule breaks mode ties in favour of the INCUMBENT, so
	// visit order decides which zone inheritedFrom names on a tie — and a tie broken
	// differently on two consecutive frames would look like a transition (RA-1).
	names := append([]string(nil), containing...)
	sort.Strings(names)

	for _, name := range names {
		z, known := byName[name]
		if !known {
			unresolved = append(unresolved, name)
			continue
		}
		if why := unresolvedReason(z); why != "" {
			unresolved = append(unresolved, name)
			continue
		}
		for _, e := range z.Status.Restrictions.Effective {
			// zoneName is the DECLARING zone, not the containing one. inheritedFrom is the
			// single most useful field in the projection — it is where an operator must go to
			// edit the rule — and a union that rewrote it to name whichever containing zone
			// happened to carry the rule would send them to an object with nothing in it.
			in := e
			in.Allowed = append([]string(nil), e.Allowed...)
			foldRule(acc, deniedBy, requiredBy, boundBy, e.InheritedFrom, in)
		}
	}

	out := make([]fleetv1.EffectiveZoneRestriction, 0, len(acc))
	for key, e := range acc {
		// An allow-list that intersected to EMPTY permits nothing, which is a denial and
		// must be emitted as one — the same inversion resolveZoneRestrictions guards, for
		// the same reason: `allowed` carries omitempty, so an empty list serialises away
		// and reads back as "no allow-list", meaning permit EVERYTHING.
		if e.Mode == fleetv1.ZoneRestrictionLimited && e.Allowed != nil && len(e.Allowed) == 0 {
			e.Mode = fleetv1.ZoneRestrictionDenied
			e.Max = ""
			e.Allowed = nil
			if b := boundBy[key]; b != "" {
				e.InheritedFrom = b
			}
			e.Reason = strings.TrimSpace(e.Reason + " (no value is permitted by every allow-list in force where this robot stands)")
		}
		out = append(out, *e)
	}
	if len(out) == 0 {
		return nil, unresolved
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Axis != out[j].Axis {
			return out[i].Axis < out[j].Axis
		}
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return out[i].InheritedFrom < out[j].InheritedFrom
	})
	return out, unresolved
}
