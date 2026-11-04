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
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// FA-R7 — telling the adapter what may be running WHERE THE ROBOT NOW IS (ADR-0059).
//
// # This is the missing half of FA-R3, not a new obligation
//
// fleet_adapter.proto already binds an adapter to honour an operating rule "for the
// whole time the robot is in the zone, including while only passing through". FA-R2 only
// ever sent it the rules for the ASSIGNMENT TARGET's chain, once, at assignment. So an
// adapter that entered any other zone — a corridor it transits, an overlapping bay, a
// zone whose rule was written after dispatch — had no rule set for where it now was and
// could not comply with an obligation this contract already placed on it. Nothing here
// asks more of an adapter; it makes what was already asked possible.
//
// # RA-1 IS THE WHOLE DIFFICULTY, AND THE PATTERN IS ALREADY IN THIS PACKAGE
//
// Containment is derived from POSITION TELEMETRY, which is precisely where a per-tick
// status write is prohibited. ObservePosition already solves this shape for currentZone:
// a compare-and-set against both the last write and the persisted status, so a frame
// that changes nothing returns before touching anything. The containment set and the
// push ride that same gate rather than a new one.
//
// TWO GATES, NOT ONE, and the difference is deliberate:
//
//   - THE STATUS WRITE fires when the containment SET changes (or currentZone does),
//     because status.containingZones must report what is true.
//   - THE PUSH fires when the RESOLVED RULE SET changes, which is a strictly smaller
//     event. A robot crossing from one unrestricted zone into another changes its
//     containment and changes nothing an adapter has to do; re-sending an identical
//     rule set would be a command per boundary crossing, on a fleet where a robot may
//     cross several in a minute.
//
// Both comparisons are on SORTED values, and that is load-bearing rather than tidy:
// zone.DeriveContainingZones sorts, unionEffectiveRestrictions sorts, and a
// map-iteration reordering in either would make an unchanged fact look like a
// transition and emit on every tick.

// ZoneConstraintsNotifier pushes the in-force operating set to a robot's adapter
// (FA-R7). The command Dispatcher satisfies it structurally. Nil disables the push, and
// the containment derivation is unaffected — the set is still written to status, so an
// operator and the API still see what binds a robot even where no adapter is listening.
type ZoneConstraintsNotifier interface {
	NotifyZoneConstraints(ctx context.Context, namespace, robotID string, c command.ZoneConstraints)
}

// containmentFingerprint renders a resolved set as one comparable string.
//
// It carries every field that changes what an adapter must DO — subject, mode, bound,
// allow-list — and also inheritedFrom and reason, which change only what a refusal
// SAYS. Including them is the conservative direction: an operator who edits a reason to
// correct it wants the corrected words to reach the adapter that will quote them, and
// the cost of being slightly eager is one command on an edit that is already a material
// transition. Excluding them would have been an optimisation whose failure mode is a
// robot quoting a reason its operator has deleted.
//
// The unresolved list is part of the fingerprint because FZ-R6 makes it part of the
// answer: a set that was complete and is now partial is a change the adapter must hear
// about, even when the rules that DID resolve are identical.
func containmentFingerprint(rules []fleetv1.EffectiveZoneRestriction, unresolved []string) string {
	var b strings.Builder
	for i := range rules {
		e := &rules[i]
		b.WriteString(string(e.Axis))
		b.WriteString("/")
		b.WriteString(e.Subject)
		b.WriteString("/")
		b.WriteString(string(e.Mode))
		b.WriteString("/")
		b.WriteString(e.Max)
		b.WriteString("/")
		b.WriteString(strings.Join(e.Allowed, ","))
		b.WriteString("/")
		b.WriteString(e.InheritedFrom)
		b.WriteString("/")
		b.WriteString(e.Reason)
		b.WriteString("|")
	}
	b.WriteString("!")
	b.WriteString(strings.Join(unresolved, ","))
	return b.String()
}

// sameZoneSet reports whether two sorted containment sets are equal.
//
// Both sides are produced sorted — zone.DeriveContainingZones sorts its result, and the
// status side was written from one — so this is a slice comparison and not a set
// comparison. That is the whole reason the sort exists; without it this would have to
// allocate a map per frame, on the hottest path in the controller.
func sameZoneSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// recordContainment compare-and-sets the last-derived containment set for a robot,
// seeding from the persisted status on first sight so a post-restart unchanged frame
// produces no write. Returns true iff the set MOVED. key is zoneMemoKey(namespace,
// robot_id), the same key recordZone uses.
//
// Serialised under the same lock as recordZone, and for the same reason: two concurrent
// frames for one robot must not both conclude they saw the transition.
func (z *ZoneController) recordContainment(key string, newSet, statusSet []string) bool {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	if z.lastContaining == nil {
		z.lastContaining = make(map[string][]string)
	}
	prev, seen := z.lastContaining[key]
	if !seen {
		prev = statusSet // seed from persisted status
	}
	if sameZoneSet(prev, newSet) {
		z.lastContaining[key] = newSet
		return false
	}
	z.lastContaining[key] = newSet
	return true
}

// recordPushedConstraints compare-and-sets the last rule set PUSHED to a robot.
// Returns true iff the set moved and a push is therefore warranted.
//
// This memo has no status to seed from, and that asymmetry is deliberate rather than an
// oversight. The containment set is published, so a restart can recover what was last
// TRUE; what was last SENT is not published anywhere, because publishing it would be a
// per-robot status field written from the same telemetry path RA-1 protects. So after a
// restart the first containment event re-pushes a set the adapter may already hold. That
// costs one redundant command per robot per restart, and it is the safe direction: the
// alternative is an adapter that reconnects holding a set the plane believes it has and
// has not.
//
// It is keyed by the Robot's namespace and name. A name is unique per namespace only, so
// two same-named robots in different namespaces keyed by name alone would share one
// entry: an equal fingerprint would suppress the second robot's push entirely, and
// unequal ones would re-push to both on every containment event.
//
// The entry also records the Robot's UID, and a different UID is a push. A Robot deleted
// and recreated under the same name is a new object whose adapter-side state nothing
// vouches for, so the first containment event of the recreated Robot sends its rule set
// even when the set is the one its predecessor was sent. The other way an adapter loses
// its set without the plane seeing it, a reconnect, is handled where the stream is
// registered (ResendConstraintsForAdapter).
func (z *ZoneController) recordPushedConstraints(robot *fleetv1.Robot, fingerprint string) bool {
	key := types.NamespacedName{Namespace: robot.Namespace, Name: robot.Name}
	now := pushedConstraints{uid: robot.UID, fingerprint: fingerprint}
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	if z.lastPushed == nil {
		z.lastPushed = make(map[types.NamespacedName]pushedConstraints)
	}
	if prev, ok := z.lastPushed[key]; ok && prev == now {
		return false
	}
	z.lastPushed[key] = now
	return true
}

// pushedConstraints is what recordPushedConstraints remembers for one Robot name: the
// object it was pushed to and the fingerprint of the rule set.
type pushedConstraints struct {
	uid         types.UID
	fingerprint string
}

// forgetPushedConstraints drops the memo for one Robot, so its next push is sent.
func (z *ZoneController) forgetPushedConstraints(robot types.NamespacedName) {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	delete(z.lastPushed, robot)
}

// ResendConstraintsForAdapter sends the rule set in force to every robot in namespace that
// adapter serves and that stands in at least one zone, whether or not the memo says it
// was sent. It is called when the adapter's ControlStream is registered: an adapter
// process that connects, or reconnects, holds no rule set, whatever this controller sent
// to the process before it, and a robot standing still produces no containment event
// that would send one.
//
// It writes no status (RA-1). It sends at most one command per robot per stream
// registration, which a telemetry tick cannot cause.
func (z *ZoneController) ResendConstraintsForAdapter(ctx context.Context, namespace, adapter string) {
	if z.Constraints == nil {
		return
	}
	var robots fleetv1.RobotList
	if err := z.List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).V(1).Info("zone constraints not re-sent on adapter connect: listing robots",
			"adapter", adapter, "namespace", namespace, "err", err)
		return
	}
	for i := range robots.Items {
		r := &robots.Items[i]
		if r.Spec.Adapter.Name != adapter || len(r.Status.ContainingZones) == 0 {
			continue
		}
		z.forgetPushedConstraints(types.NamespacedName{Namespace: r.Namespace, Name: r.Name})
		z.pushZoneConstraints(ctx, r, r.Status.ContainingZones)
	}
}

// pushZoneConstraints resolves what is in force across a robot's containment set and
// sends it — but only when that answer has changed since the last push.
//
// It is called from the containment transition in ObservePosition and from the
// restriction-projection change in the FleetZone reconcile, and from nowhere else. Those
// are the two events that can change the answer: the robot moved, or an operator edited
// a rule. A telemetry tick that does neither reaches this function and returns before
// sending anything.
func (z *ZoneController) pushZoneConstraints(ctx context.Context, robot *fleetv1.Robot, containing []string) {
	if z.Constraints == nil {
		return
	}
	var zones fleetv1.FleetZoneList
	if err := z.List(ctx, &zones, client.InNamespace(robot.Namespace)); err != nil {
		// NOT fail-closed, and the asymmetry with candidate filters 11 and 12 is
		// deliberate. Those refuse to DISPATCH on an unreadable zone list, because
		// dispatching into a possibly-denied zone is an action the plane takes. This
		// pushes nothing instead: the robot is already where it is, the adapter keeps the
		// set it last received, and the next containment event re-derives. Sending a
		// fail-closed "everything is denied" set would be the plane inventing a
		// restriction nobody wrote, from a read that failed.
		log.FromContext(ctx).V(1).Info("zone constraints not pushed: listing FleetZones",
			"robot", robot.Name, "err", err)
		return
	}

	rules, unresolved := unionEffectiveRestrictionsWithUnresolved(zones.Items, containing)
	fp := containmentFingerprint(rules, unresolved)
	if !z.recordPushedConstraints(robot, fp) {
		return // the answer has not moved — send nothing (RA-1)
	}

	z.Constraints.NotifyZoneConstraints(ctx, robot.Namespace, robot.Name, command.ZoneConstraints{
		ContainingZones: containing,
		// Only the OPERATING axis travels. Entry and capability rules are decided by
		// candidate filter 11 before any robot is chosen and are terminal for the action;
		// an adapter receiving one could only re-decide a question already answered, at
		// the one point in the flow where a second opinion cannot help. This is the same
		// split operatingConstraintsForWire already applies on AssignAction, through the
		// same converter, so the two messages cannot disagree about what an operating
		// rule looks like on the wire.
		OperatingConstraints: operatingConstraintsForWire(rules),
		UnresolvedZones:      unresolved,
	})
}

// robotsContaining lists the robots whose published containment set includes zoneName.
//
// IT READS status.containingZones RATHER THAN RE-DERIVING FROM POSITION, and that is
// what keeps it off the telemetry path. The set is already published by the transition
// that established it, so answering "who is inside this zone" is a List and a slice
// scan — no geometry, no position frames, and nothing that scales with telemetry volume.
//
// Called only from the restriction-projection change, which is a spec or topology edit
// (FZ-R5) and therefore already a material transition. It is NOT an occupancy status
// field: nothing is written to FleetZone.status, which is the line ADR-0058 §10 draws
// and the reason the occupant report is a one-shot event rather than a list.
func (z *ZoneController) robotsContaining(ctx context.Context, namespace, zoneName string) []fleetv1.Robot {
	var robots fleetv1.RobotList
	if err := z.List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).V(1).Info("listing robots for a restriction re-push", "err", err)
		return nil
	}
	var out []fleetv1.Robot
	for i := range robots.Items {
		for _, name := range robots.Items[i].Status.ContainingZones {
			if name == zoneName {
				out = append(out, robots.Items[i])
				break
			}
		}
	}
	return out
}

// repushOnRestrictionChange re-sends the in-force set to every robot standing inside a
// zone whose restriction projection has just changed.
//
// THIS IS THE TEMPORAL HALF OF THE HOLE, AND IT IS WHY THE MODEL IS NOT ADMISSION-ONLY
// (ADR-0059 §3).
// Candidate filters 11 and 12 gate DISPATCH; before this, nothing re-evaluated a robot
// once it was moving, so a camera denial written at T1 reached a robot admitted at T0
// through no path whatsoever. Now it reaches the adapter the moment the projection
// moves.
//
// IT DOES NOT CANCEL ANYTHING, and that is a decision rather than an omission. The robot
// is TOLD WHAT IT MAY RUN; its work is not taken away. A control plane that cancels
// in-flight work in response to a spec edit is a much larger behavioural commitment —
// ADR-0058 §10 already refused the analogous move for entry denials and made Report the
// default for exactly this reason — and an adapter that honours the pushed set has
// stopped the camera without anyone stopping the robot.
func (z *ZoneController) repushOnRestrictionChange(ctx context.Context, zoneObj *fleetv1.FleetZone) {
	if z.Constraints == nil {
		return
	}
	for _, robot := range z.robotsContaining(ctx, zoneObj.Namespace, zoneObj.Name) {
		r := robot
		z.pushZoneConstraints(ctx, &r, r.Status.ContainingZones)
	}
}
