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
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// `occupantAction: Report` — its one observable effect (ADR-0058 §10).
//
// # Why this exists at all
//
// `occupantAction` has three values. `Graceful` and `Immediate` are RESERVED and move no
// robot at v0.4; the admission webhook warns on both and tells the operator the zone
// "behaves as Report". Until this producer landed, **`Report` did nothing either** — so
// all three values had one behaviour, and that behaviour was nothing. A default that is
// indistinguishable from the two modes documented as inert makes the warning about them
// HARDER to read, not easier: it has to disclaim a difference that is not there.
//
// The three normative texts that describe this event were struck rather than satisfied,
// correctly, by the session that found the producer missing and could not add it. The
// strike was the honest move at the time. This is the other half: the event exists now,
// so those texts describe something real again — and none of the struck OVER-claims come
// back with it. Report still moves no robot. It reports.
//
// # WHAT THE EVENT CLAIMS, AND THE HARDER QUESTION OF WHAT IT MUST NOT
//
// It names the robots inside the zone at the moment the denial took effect, the zone
// that declared it, and the operator's own reason. That is all.
//
// It is NOT a statement that those robots left, were stopped, were wound down, or will
// be. `Report` is the default precisely because a control plane that starts MOVING
// ROBOTS in response to an edit of a `spec` field is a new autonomous motion path, and
// this surface is explicitly not a safety system. An event whose wording let an operator
// infer motion would reintroduce the exact expectation the field's documentation was
// struck for, one layer out — so the sentence says "were inside", never "have been".
//
// # RA-1: THE OCCUPANT LIST IS THE TRAP, AND THE ESCAPE IS TO READ PUBLISHED PROJECTIONS
//
// "Which robots are in this zone" is a question about POSITION, and position is exactly
// what may not drive a status write or an emit. ADR-0058 §10 is explicit that the
// occupant report must not become a status stream for this reason.
//
// So nothing here reads a position frame. It reads two fields that are each written ONLY
// on a material transition:
//
//   - `Robot.status.containingZones[]` — the containment set (ADR-0059), written by the
//     edge-triggered compare-and-set in ObservePosition and by nothing else;
//   - `Robot.status.currentZone` — the arbitrated single answer, written by the same
//     transition.
//
// Both are already-throttled projections, so consulting them costs a List on a
// reconcile — which FZ-R5 guarantees is a spec-or-topology event — and never scales with
// telemetry volume. A telemetry tick that changes containment writes those fields and
// does not reach this file; a tick that changes nothing writes nothing, so there is
// nothing for this file to be triggered by either.
//
// # BOTH fields, unioned, and the union is not belt-and-braces
//
// `containingZones` is polygon containment, so a zone with no site-aligned polygon
// (`ZoneGeometryAbsent`, `FrameLocal`) never appears in it — its occupants would be
// invisible. `currentZone` covers that case, because membership there can be declared
// rather than derived. Conversely `currentZone` is ONE name arbitrated from overlapping
// polygons by nearest centroid, so a robot standing in two zones is reported in one:
// reading it alone misses exactly the robot ADR-0059's containment ruling exists for.
//
// Neither field subsumes the other. The union is the honest occupant set.

// occupantReportKey identifies one zone's latch entry.
func occupantReportKey(zoneObj *fleetv1.FleetZone) string {
	return zoneObj.Namespace + "/" + zoneObj.Name
}

// reportOccupantsOnEntryDenial emits ONE ZoneEntryDeniedWithOccupants Warning per
// transition into the denied-with-occupants state.
//
// # The latch is warnOnContradiction's, not a second mechanism
//
// Same map shape, same mutex (`restrictMu`), same "was it already true" test, and the
// same reason: an event stream that repeats an unchanged fact is one an operator stops
// reading, which costs them the next real one. Two latches with the same job would be two
// things to keep in step, and the one that drifted would be the one nobody was watching.
//
// The latch records a STATE, not a fact that has happened once for ever. An operator who
// lifts a denial and re-applies it gets a second event, because that is a second
// transition and they are entitled to know who was inside the second time.
//
// # It is called AFTER the status patch, on the same gate as the constraint re-push
//
// The caller gates it on the restriction projection having MOVED, which FZ-R5 guarantees
// is a spec or topology change and never a telemetry tick. A resync, an unrelated spec
// edit, or a robotCount change reaches this function not at all.
func (z *ZoneController) reportOccupantsOnEntryDenial(ctx context.Context, zoneObj *fleetv1.FleetZone) {
	if z.Recorder == nil {
		return
	}

	// The denial is read from the RESOLVED projection, never from spec (FZ-R4): a zone
	// whose parent denies entry is denied without declaring anything itself, and its
	// occupants are as much shut in as the declaring zone's.
	denial := findEffective(zoneObj, fleetv1.ZoneAxisEntry, "")
	denied := denial != nil && denial.Mode == fleetv1.ZoneRestrictionDenied

	// LATCH FIRST, AND UNCONDITIONALLY, so the state advances whether or not an event is
	// emitted. Latching only on the emit path would leave the flag stale for a zone whose
	// denial was lifted, and the NEXT denial would then be swallowed as "already warned".
	key := occupantReportKey(zoneObj)
	z.restrictMu.Lock()
	if z.occupantAlerted == nil {
		z.occupantAlerted = map[string]bool{}
	}
	already := z.occupantAlerted[key]
	z.occupantAlerted[key] = denied
	z.restrictMu.Unlock()

	if !denied || already {
		return
	}

	occupants := z.occupantsOf(ctx, zoneObj)
	if len(occupants) == 0 {
		// Nothing to report. An event reading "0 robots were inside" is noise on every
		// zone an operator closes ahead of time, which is the common and correct case.
		return
	}

	because := ""
	if denial.Reason != "" {
		because = " — " + denial.Reason
	}
	via := ""
	if denial.InheritedFrom != "" && denial.InheritedFrom != zoneObj.Name {
		via = fmt.Sprintf(", declared by ancestor zone %q", denial.InheritedFrom)
	}

	// THE WORDING IS LOAD-BEARING. Past tense about presence ("were inside"), and an
	// explicit statement that nothing was done to them — because the whole reason this
	// field defaults to Report is that the plane does not move robots on a spec edit, and
	// an event a reader could take as "they have been dealt with" would undo that.
	z.Recorder.Event(zoneObj, corev1.EventTypeWarning, zoneReasonEntryDeniedWithOccupants,
		fmt.Sprintf("entry to zone %q is now denied%s%s, and %d robot(s) were inside when it "+
			"took effect: %s. NO ROBOT HAS BEEN MOVED, wound down or requeued — occupantAction "+
			"reports and does not act, and this event is not a statement that they have left. "+
			"New work is withheld from the zone by the denial itself. This is a one-shot report "+
			"at the transition; it is not a live occupancy list",
			zoneObj.Name, because, via, len(occupants), strings.Join(occupants, ", ")))
}

// occupantsOf returns the names of robots published as being inside zoneObj, sorted.
//
// # It reads two PUBLISHED projections and no position frame
//
// See this file's header for why that is the whole design. `containingZones[]` and
// `currentZone` are each written only on a containment transition, so this is a read of
// throttled state rather than a derivation from telemetry.
//
// # The subtree, because an entry denial is inherited
//
// A denial applies to the zone AND every descendant (FZ-R1), so a robot in a child of a
// denied zone is inside the denial. `currentZone` is matched against the whole subtree,
// which is the same set `countSubtreeRobots` counts for `status.robotCount` — the
// standing occupancy view this event deliberately does not become.
//
// `containingZones[]` is matched against the zone itself only, and that is not an
// inconsistency: it is polygon containment, and a descendant's polygon is not required
// to lie inside its parent's. A robot in a child zone is picked up by the `currentZone`
// arm; a robot physically inside this zone's own polygon is picked up by this one.
//
// Sorted so the event text is stable for a stable fleet. An event whose wording varied
// with map iteration would be a different string every reconcile, which matters here
// because the latch is the only thing stopping repeats and a changed string would look
// like new information to anyone reading the stream.
func (z *ZoneController) occupantsOf(ctx context.Context, zoneObj *fleetv1.FleetZone) []string {
	var zones fleetv1.FleetZoneList
	if err := z.List(ctx, &zones, client.InNamespace(zoneObj.Namespace)); err != nil {
		// NOT fail-closed, and the asymmetry with candidate filters 11 and 12 is
		// deliberate. Those refuse to DISPATCH on an unreadable zone list, because
		// dispatching into a possibly-denied zone is an action the plane takes. This
		// only reports. An unreadable list means the subtree is unknown, and naming a
		// partial occupant set as though it were complete would be worse than the
		// silence — an operator who reads "1 robot was inside" and acts on it has been
		// told something false about a zone they just closed.
		log.FromContext(ctx).V(1).Info("occupant report skipped: listing FleetZones",
			"zone", zoneObj.Name, "err", err)
		return nil
	}
	childrenOf, _ := buildTree(zones.Items)

	subtree := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if subtree[name] {
			return // cycle guard; admission rejects cycles, this survives one anyway
		}
		subtree[name] = true
		for _, c := range childrenOf[name] {
			walk(c)
		}
	}
	walk(zoneObj.Name)

	var robots fleetv1.RobotList
	if err := z.List(ctx, &robots, client.InNamespace(zoneObj.Namespace)); err != nil {
		log.FromContext(ctx).V(1).Info("occupant report skipped: listing Robots",
			"zone", zoneObj.Name, "err", err)
		return nil
	}

	var out []string
	for i := range robots.Items {
		r := &robots.Items[i]
		inside := subtree[r.Status.CurrentZone]
		if !inside {
			for _, name := range r.Status.ContainingZones {
				if name == zoneObj.Name {
					inside = true
					break
				}
			}
		}
		if inside {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}
