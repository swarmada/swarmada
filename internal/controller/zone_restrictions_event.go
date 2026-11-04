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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// zoneReasonRestrictionsChanged is the audit Event a zone emits when the restrictions
// in force at it change (ADR-0058 §11; RFC-0001 zone-restrictions amendment, "Who may
// change a restriction").
const zoneReasonRestrictionsChanged = "ZoneRestrictionsChanged"

// announceRestrictionsChanged emits one ZoneRestrictionsChanged Normal Event naming the
// restrictions in force at the zone before and after, when they differ.
//
// It reads the PROJECTION (status.restrictions.effective), not spec.restrictions, for
// two reasons. The projection is persisted, so a restarted manager compares against
// what was published rather than against an empty memory and announces nothing on its
// first pass. And a descendant whose rules change because an ancestor was edited is
// told, which is the zone an operator looks at when a robot stops going somewhere.
//
// An unchanged pass emits nothing (RA-1): the caller gates on the projection having
// moved, and this compares the rule lists themselves, so a zone gaining an empty
// projection on its first reconcile is not a change.
func (z *ZoneController) announceRestrictionsChanged(zoneObj *fleetv1.FleetZone, before, after *fleetv1.ZoneRestrictionsStatus) {
	if z.Recorder == nil {
		return
	}
	was, now := effectiveOf(before), effectiveOf(after)
	if len(was) == 0 && len(now) == 0 {
		return
	}
	if equality.Semantic.DeepEqual(was, now) {
		return
	}
	z.Recorder.Event(zoneObj, corev1.EventTypeNormal, zoneReasonRestrictionsChanged,
		fmt.Sprintf("zone %q: restrictions in force changed from %s to %s",
			zoneObj.Name, restrictionsText(was), restrictionsText(now)))
}

func effectiveOf(s *fleetv1.ZoneRestrictionsStatus) []fleetv1.EffectiveZoneRestriction {
	if s == nil {
		return nil
	}
	return s.Effective
}

// restrictionsText renders a projection in its own (sorted) order, one rule per clause.
func restrictionsText(eff []fleetv1.EffectiveZoneRestriction) string {
	if len(eff) == 0 {
		return "[none]"
	}
	parts := make([]string, 0, len(eff))
	for _, e := range eff {
		s := string(e.Axis)
		if e.Subject != "" {
			s += "/" + e.Subject
		}
		s += " " + string(e.Mode)
		if e.Max != "" {
			s += " max=" + e.Max
		}
		if len(e.Allowed) > 0 {
			s += " allowed=" + strings.Join(e.Allowed, ",")
		}
		s += " from " + e.InheritedFrom
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, "; ") + "]"
}
