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

// Package estopscope answers one question: which emergency stops hold a robot. A clear of
// one scope is not the clear of another (RFC-0001 #safety-estop-scopes), so a robot that
// its own robot-scope trigger, a zone's stop or its namespace's stop still holds is not
// released by the clear of a different scope.
//
// A stop holds a robot from the moment it is issued to it until that stop's own clear. The
// robot-scope stop is recorded by the robot's own trigger annotation. A zone's or the
// namespace's stop is recorded on the robot, in Robot.status.estopHeldBy, when it is issued
// to the robot, and the record is removed by the clear of a stop on a carrier of that kind and
// name, or, once its carrier is gone, by a robot-scope stop and clear. So the answer does not
// depend on where the zone tree, the robot's zone or its reported position stand when it is
// asked.
//
// The controllers (which perform the clears, the resume and the late-arrival stops), the
// dispatch choke point, the held-action pause and swarmctl read the answer from here so
// they cannot disagree.
package estopscope

import (
	"fmt"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ProcessedAnnotation is the marker the controllers write once a carrier's stop has been
// issued, and remove once its clear has been applied. A carrier holds the robots it
// stopped while it carries the trigger or the marker.
const ProcessedAnnotation = "swarmada.io/estop-processed"

// TriggerAnnotation is the estop trigger on every carrier: a Robot, a FleetZone, or the
// namespace's SwarmadaConfig. Its presence is the trigger, whatever its value.
const TriggerAnnotation = "swarmada.io/estop-triggered"

// walkCap bounds the parentZone ancestor walk, as the zone-estop controller's does: a zone and
// walkCap ancestors above it, the farthest the fan-out reaches.
const walkCap = 64

// Kind names the scope of a stop.
type Kind string

// The three estop scopes.
const (
	KindRobot     Kind = "robot"
	KindZone      Kind = "zone"
	KindNamespace Kind = "namespace"
)

// Holder is a stop that holds a robot.
type Holder struct {
	Kind Kind
	// Name is the carrier: the robot, the FleetZone or the SwarmadaConfig.
	Name string
	// Value is the trigger annotation's value, which may be empty.
	Value string
	// Recorded is set when the hold is read from the robot's status.estopHeldBy rather
	// than from the zone tree as it stands.
	Recorded bool
	// Clearing is set on a recorded hold whose carrier no longer carries the trigger but still
	// carries the processed marker: its clear was requested and has not been applied yet.
	Clearing bool
	// Orphaned is set when the carrier a recorded hold names no longer exists, or exists
	// under another UID. Its own clear can no longer run, so an admin releases it with a
	// robot-scope clear on the robot (see OrphanedRelease).
	Orphaned bool
	// Pending is set when the stop is issued and not yet applied: the carrier holds the
	// trigger and no processed marker (the stop guard, coverage.go).
	Pending bool
}

// String describes the stop as the operator-facing messages name it.
func (h Holder) String() string {
	if h.Orphaned {
		what := "zone " + h.Name
		if h.Kind == KindNamespace {
			what = "SwarmadaConfig " + h.Name
		}
		return fmt.Sprintf("the estop of %s, which no longer exists (recorded in status.estopHeldBy)", what)
	}
	if h.Clearing {
		if h.Kind == KindNamespace {
			return fmt.Sprintf("the namespace estop (SwarmadaConfig %s), whose clear is being applied", h.Name)
		}
		return fmt.Sprintf("the estop on zone %s, whose clear is being applied", h.Name)
	}
	switch h.Kind {
	case KindRobot:
		return fmt.Sprintf("its own robot-scope estop (%s=%q)", TriggerAnnotation, h.Value)
	case KindZone:
		return fmt.Sprintf("the estop on zone %s (%s=%q)", h.Name, TriggerAnnotation, h.Value)
	default:
		return fmt.Sprintf("the namespace estop (SwarmadaConfig %s, %s=%q)", h.Name, TriggerAnnotation, h.Value)
	}
}

// OrphanedRelease is how an admin releases a robot held by an orphaned record: a
// robot-scope stop and its clear on the robot, whose clear drops every record of a stop
// whose carrier no longer exists, then a resume.
func OrphanedRelease(robot, namespace string) string {
	return fmt.Sprintf("an admin releases it with a robot-scope stop and clear: kubectl annotate robot %s %s=\"<reason>\" -n %s; "+
		"once the Robot carries %s, kubectl annotate robot %s %s- -n %s; then swarmctl estop resume %s --reason \"<text>\" -n %s",
		robot, TriggerAnnotation, namespace, ProcessedAnnotation, robot, TriggerAnnotation, namespace, robot, namespace)
}

// HoldKind maps a recorded hold's kind to the scope it names.
func HoldKind(k fleetv1.EstopHoldKind) Kind {
	if k == fleetv1.EstopHoldSwarmadaConfig {
		return KindNamespace
	}
	return KindZone
}

// HeldOnRecord returns a stop that holds robot as the robot itself records it, and whether
// one does: its own trigger first, then the first status.estopHeldBy entry. It reads
// nothing but the Robot, so it can decide where no zone list is at hand: the held-action
// pause, dispatch filter 10 and the delete guard. Every entry counts here, whatever its
// carrier now carries; an entry is removed only by its carrier's clear, or, when its
// carrier no longer exists, by a robot-scope clear.
func HeldOnRecord(robot *fleetv1.Robot) (Holder, bool) {
	if v, ok := robot.Annotations[TriggerAnnotation]; ok {
		return Holder{Kind: KindRobot, Name: robot.Name, Value: v}, true
	}
	if len(robot.Status.EstopHeldBy) > 0 {
		e := robot.Status.EstopHeldBy[0]
		return Holder{Kind: HoldKind(e.Kind), Name: e.Name, Recorded: true}, true
	}
	return Holder{}, false
}

// StandingStop returns the first stop Holders names, and whether there is one. Each clear
// asks it after removing its own trigger and its own record from the robot, so the stop
// being cleared is never the answer.
func StandingStop(robot *fleetv1.Robot, zones []fleetv1.FleetZone, configs []fleetv1.SwarmadaConfig) (Holder, bool) {
	hs := Holders(robot, zones, configs)
	if len(hs) == 0 {
		return Holder{}, false
	}
	return hs[0], true
}

// Holders returns every stop that holds robot, in this order: the robot's own trigger; each
// status.estopHeldBy entry whose carrier still holds its stop; each zone stop the zone tree
// as it stands reaches; the namespace's stop; and last, each entry whose carrier no longer
// exists (Orphaned). zones and configs are the FleetZones and SwarmadaConfigs of the
// robot's namespace.
//
// A recorded entry holds while its carrier carries the trigger or the processed marker,
// whatever the tree, the robot's zone or its position say now. An entry whose carrier
// exists under the same UID and carries neither has been cleared, and does not hold. An
// entry whose carrier is gone, or was recreated under another UID, still holds: nothing
// else would end it, so it holds until its release (OrphanedRelease), or until the clear of
// a carrier of that name.
//
// The tree adds the stops a robot has come under and that have not been recorded on it yet
// (the instant before the record is written): a zone covers the robot when it is the
// robot's zone, or an ancestor of it whose estopPolicy propagates to children; the robot's
// zone is status.currentZone, or spec.zone while that is empty, as the zone fan-out reads
// it. A carrier holds the robot while it carries the trigger annotation; the scope being
// cleared has had its annotation removed, so it is never the answer from the tree.
func Holders(robot *fleetv1.Robot, zones []fleetv1.FleetZone, configs []fleetv1.SwarmadaConfig) []Holder {
	var out []Holder
	seen := map[Holder]bool{}
	add := func(h Holder) {
		key := Holder{Kind: h.Kind, Name: h.Name}
		if !seen[key] {
			seen[key] = true
			out = append(out, h)
		}
	}
	if v, ok := robot.Annotations[TriggerAnnotation]; ok {
		add(Holder{Kind: KindRobot, Name: robot.Name, Value: v})
	}
	byName := make(map[string]*fleetv1.FleetZone, len(zones))
	for i := range zones {
		byName[zones[i].Name] = &zones[i]
	}
	cfgByName := make(map[string]*fleetv1.SwarmadaConfig, len(configs))
	for i := range configs {
		cfgByName[configs[i].Name] = &configs[i]
	}
	var orphaned []Holder
	for _, e := range robot.Status.EstopHeldBy {
		var meta *carrierMeta
		switch e.Kind {
		case fleetv1.EstopHoldSwarmadaConfig:
			if c, ok := cfgByName[e.Name]; ok {
				meta = &carrierMeta{uid: string(c.UID), annotations: c.Annotations}
			}
		default:
			if z, ok := byName[e.Name]; ok {
				meta = &carrierMeta{uid: string(z.UID), annotations: z.Annotations}
			}
		}
		h := Holder{Kind: HoldKind(e.Kind), Name: e.Name, Recorded: true}
		switch {
		case meta == nil || (e.UID != "" && meta.uid != "" && e.UID != meta.uid):
			h.Orphaned = true
			orphaned = append(orphaned, h)
		case meta.inForce():
			v, triggered := meta.annotations[TriggerAnnotation]
			h.Value, h.Clearing = v, !triggered
			add(h)
		}
	}
	name := robot.Status.CurrentZone
	if name == "" {
		name = robot.Spec.Zone
	}
	for depth := 0; name != "" && depth <= walkCap; depth++ {
		z, ok := byName[name]
		if !ok {
			break
		}
		if v, triggered := z.Annotations[TriggerAnnotation]; triggered && (depth == 0 || propagatesDown(z)) {
			add(Holder{Kind: KindZone, Name: z.Name, Value: v})
		}
		name = z.Spec.ParentZone
	}
	for i := range configs {
		if v, ok := configs[i].Annotations[TriggerAnnotation]; ok {
			add(Holder{Kind: KindNamespace, Name: configs[i].Name, Value: v})
		}
	}
	for _, h := range orphaned {
		add(h)
	}
	return out
}

// carrierMeta is what Holders reads from a recorded entry's carrier.
type carrierMeta struct {
	uid         string
	annotations map[string]string
}

// inForce reports whether the carrier still holds its stop: it carries the trigger, or the
// processed marker (its clear has not been applied yet).
func (m *carrierMeta) inForce() bool {
	if _, ok := m.annotations[TriggerAnnotation]; ok {
		return true
	}
	_, ok := m.annotations[ProcessedAnnotation]
	return ok
}

// propagatesDown is estopPolicy.propagateToChildren with its default (true) applied.
func propagatesDown(z *fleetv1.FleetZone) bool {
	return z.Spec.EstopPolicy == nil || z.Spec.EstopPolicy.PropagatesToChildren()
}

// StopFinalizer is the finalizer the estop controllers hold on a carrier (a Robot, a FleetZone
// or the namespace's SwarmadaConfig) from the moment its trigger lands until its clear has
// been applied. The same finalizer is held on a Robot that records an action while a stop holds
// it, and on that FleetAction, so the record survives any delete path while the work is held. A carrier deleted while no manager replica serves the delete check (the two
// delete guards with failurePolicy Ignore) then stays until the manager returns, fans the stop
// out and applies the clear, instead of disappearing with a stop that reached no robot. An
// admin who removes the finalizer by hand ends the stop unapplied.
const StopFinalizer = "swarmada.io/estop-in-force"
