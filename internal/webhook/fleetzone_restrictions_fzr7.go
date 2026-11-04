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

package webhook

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// FZ-R7 — admission refuses a new hole. It never refuses a new wall.
//
// # The asymmetry, and why symmetric validation is the bug
//
// Two directions of change get deliberately different answers:
//
//   - A DESCENDANT weakening an ancestor — a raised max, a widened allow-list, a
//     Required under a Denied or the reverse — is REJECTED here, naming the ancestor
//     zone and the conflicting rule. Not silently ignored: silently ignoring produces
//     an object whose spec says one thing and whose behaviour is another, and the
//     entire point of this surface is that the API is where the rule is stated.
//   - An ANCESTOR tightening onto an existing descendant is ALWAYS ADMITTED (FZ-R8),
//     even when a descendant now contradicts it. The contradiction resolves in favour
//     of the restriction and the DESCENDANT publishes RestrictionsConsistent: False.
//
// That second half is enforced by CONSTRUCTION rather than by a check: this function
// is only ever given the zone being written and its ANCESTORS. It never lists
// children, never lists a subtree, and therefore has no way to refuse a write on
// account of a descendant. Writing it the other way — one symmetric "reject any
// disagreement" rule — is the natural thing to reach for and is wrong: it would make
// a boundary un-addable, let a leaf someone else owns veto a site policy, and force
// a whole-subtree list on every zone write.
//
// The direction of the asymmetry is what makes it safe: the unresolvable case is
// never the permissive one.
//
// # This is the DIAGNOSTIC, not the enforcement (FZ-R9)
//
// A webhook can be uninstalled, set to failurePolicy: Ignore, raced, or skipped by
// an etcd restore. So this check tells an operator NOW, and the resolver in
// internal/controller is what holds on an object admission never saw. An
// implementation that enforces FZ-R7 here and resolves nearest-ancestor-wins there
// has built the diagnostic and skipped the invariant.

// restrictionWeakeningErrors returns one error per rule in z that is more
// permissive than a rule declared by an ancestor. ancestors may be in any order and
// need not be complete; a partial chain yields a partial check, which is the correct
// degradation for a diagnostic.
func restrictionWeakeningErrors(z *fleetv1.FleetZone, ancestors []fleetv1.FleetZone) field.ErrorList {
	if z.Spec.Restrictions == nil || len(ancestors) == 0 {
		return nil
	}

	var errs field.ErrorList
	base := field.NewPath("spec").Child("restrictions")

	for _, anc := range ancestors {
		if anc.Spec.Restrictions == nil {
			continue
		}
		for _, a := range declaredZoneRules(&anc) {
			own, ok := declaredRuleFor(z, a.axis, a.subject)
			if !ok {
				continue // declaring nothing is not a weakening: the ancestor's rule still applies
			}
			if why := weakensAncestor(own, a); why != "" {
				errs = append(errs, field.Invalid(a.fieldPath(base), own.describe(),
					fmt.Sprintf("%s is more permissive than the rule declared by ancestor zone %q (%s); "+
						"a descendant may not re-permit what an ancestor restricts",
						why, anc.Name, a.describe())))
			}
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errs
}

// zoneRule is one declared rule, flattened across the three axes so the comparison
// is written once rather than three times. ADR-0058 §1 is explicit that inheritance,
// precedence and admission are defined ONCE OVER RULES and never restated per axis;
// an admission check that covered only the axis it was written for would be the
// "three schemas" mistake the model refuses.
type zoneRule struct {
	axis    fleetv1.ZoneRestrictionAxis
	subject string
	mode    fleetv1.ZoneRestrictionMode
	max     string
	allowed []string
}

func (r zoneRule) describe() string {
	var b strings.Builder
	b.WriteString(string(r.mode))
	if r.max != "" {
		fmt.Fprintf(&b, " max=%s", r.max)
	}
	if len(r.allowed) > 0 {
		fmt.Fprintf(&b, " allowed=%v", r.allowed)
	}
	return b.String()
}

// fieldPath points at the field on the zone being written that must change, not at
// the ancestor's — the operator can only edit their own object.
func (r zoneRule) fieldPath(base *field.Path) *field.Path {
	switch r.axis {
	case fleetv1.ZoneAxisEntry:
		return base.Child("entry")
	case fleetv1.ZoneAxisCapability:
		return base.Child("capabilities").Key(r.subject)
	default:
		return base.Child("operating").Key(r.subject)
	}
}

func declaredZoneRules(z *fleetv1.FleetZone) []zoneRule {
	r := z.Spec.Restrictions
	if r == nil {
		return nil
	}
	var out []zoneRule
	if r.Entry != nil {
		out = append(out, zoneRule{axis: fleetv1.ZoneAxisEntry, mode: r.Entry.Mode})
	}
	for _, c := range r.Capabilities {
		out = append(out, zoneRule{axis: fleetv1.ZoneAxisCapability, subject: c.Name, mode: c.Mode})
	}
	for _, o := range r.Operating {
		out = append(out, zoneRule{
			axis: fleetv1.ZoneAxisOperating, subject: o.Subject,
			mode: o.Mode, max: o.Max, allowed: o.Allowed,
		})
	}
	return out
}

func declaredRuleFor(z *fleetv1.FleetZone, axis fleetv1.ZoneRestrictionAxis, subject string) (zoneRule, bool) {
	for _, r := range declaredZoneRules(z) {
		if r.axis == axis && r.subject == subject {
			return r, true
		}
	}
	return zoneRule{}, false
}

// weakensAncestor returns a non-empty explanation when own permits something anc
// forbade. It is the admission-side twin of the resolver's FZ-R2 check, and the two
// must agree about what "weaker" means — which is why both are written as one
// predicate over the flattened rule rather than per axis.
func weakensAncestor(own, anc zoneRule) string {
	// Denied forbids the subject outright; nothing else is as strong.
	if anc.mode == fleetv1.ZoneRestrictionDenied && own.mode != fleetv1.ZoneRestrictionDenied {
		return fmt.Sprintf("mode %s under an ancestor's Denied", own.mode)
	}
	// Required mandates the subject be running. Denied under it is the mirror case
	// FZ-R7 names explicitly, and it is a conflict rather than a tightening: the two
	// rules cannot both be honoured, and the descendant is the one introducing it.
	if anc.mode == fleetv1.ZoneRestrictionRequired && own.mode == fleetv1.ZoneRestrictionDenied {
		return "mode Denied under an ancestor's Required"
	}
	if anc.mode == fleetv1.ZoneRestrictionRequired && own.mode == fleetv1.ZoneRestrictionLimited {
		return "mode Limited under an ancestor's Required, dropping the requirement"
	}
	if own.mode == fleetv1.ZoneRestrictionDenied {
		return "" // strictly stronger than any bound
	}
	if anc.max != "" && own.max != "" {
		ancV, err1 := strconv.ParseFloat(anc.max, 64)
		ownV, err2 := strconv.ParseFloat(own.max, 64)
		if err1 == nil && err2 == nil && ownV > ancV {
			return fmt.Sprintf("max %s exceeds the ancestor's %s", own.max, anc.max)
		}
	}
	if anc.max != "" && own.max == "" && own.mode == fleetv1.ZoneRestrictionLimited {
		return fmt.Sprintf("a Limited rule with no max under an ancestor's max %s", anc.max)
	}
	if len(anc.allowed) > 0 {
		permitted := make(map[string]bool, len(anc.allowed))
		for _, a := range anc.allowed {
			permitted[a] = true
		}
		var added []string
		for _, a := range own.allowed {
			if !permitted[a] {
				added = append(added, a)
			}
		}
		if len(added) > 0 {
			sort.Strings(added)
			return fmt.Sprintf("allowed adds %v, which the ancestor's list does not permit", added)
		}
		if len(own.allowed) == 0 && own.mode == fleetv1.ZoneRestrictionLimited {
			return fmt.Sprintf("a Limited rule with no allow-list under an ancestor's %v", anc.allowed)
		}
	}
	return ""
}
