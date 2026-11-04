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
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// ProjectionForgetter drops a robot's recorded telemetry-projection state.
// [github.com/swarmada/swarmada/internal/telemetry.Router] satisfies it, for the
// projector and the last-frame clock, and [ZoneController] does, for the zone it last
// derived from the robot's position.
type ProjectionForgetter interface {
	// ForgetRobot discards whatever the projector believes it last wrote to the
	// status of the Robot in namespace with this wire robot_id, so the next frame
	// re-establishes it. It reports whether anything was actually held.
	ForgetRobot(namespace, robotID string) bool
}

// RobotProjectionResetter keeps the telemetry projector's per-robot cache honest
// about WHICH Robot object it describes.
//
// The projector (internal/telemetry) remembers, per (namespace, wire robot_id), the
// material state it last handed to [RobotStatusSink]. That memory is what makes the
// RA-1 gate work: an unchanged frame produces no status write. But it lives for the
// manager process's lifetime, while the
// Robot object it describes can be deleted and recreated underneath it — a
// namespace rebuild, a re-admission through DiscoveredRobot, an operator deleting
// and re-applying a manifest. The recreated object's status starts empty; the
// projector still believes it holds the old object's battery bucket and hardware
// map; an unchanging telemetry stream matches that stale belief and emits nothing.
// The robot then reports no batteryPercent and no hardware for as long as the
// manager keeps running.
//
// The Robot reconciler already sees every generation of every Robot and holds its
// UID. Observe records the (namespace, robot_id) -> UID binding and, when the UID
// behind it changes, tells the projector to forget the old generation. Whichever of
// the reconcile and the first post-recreation frame arrives second does the right
// thing: if the reconcile is first the very next frame re-establishes status; if a
// frame is first it is suppressed once and the following one re-establishes.
//
// It fixes RECREATION, which is the case observed. A Robot whose status is lost
// WITHOUT the object changing identity — an etcd restore rolling status back under
// a live manager — keeps its UID and is not covered here.
//
// Why the binding is keyed by namespace as well. The control plane does not enforce
// robot_id uniqueness across namespaces (RFC-0001
// #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3), so two live Robots
// may carry the same robot_id with different UIDs. Keyed by robot_id alone, every
// reconcile of one of them read as a change of identity and dropped both robots'
// projector state; with the periodic requeue that is a status patch per robot per
// reconcile, and a robot whose frames move inside a battery bucket turned each patch
// into a watch event, a reconcile and the next drop — a status write per frame.
//
// RA-1. Nothing in this type writes status, but a forget CAUSES a write: the
// robot's next frame is first sight to the projector and goes through the normal
// Project -> ApplyMaterialUpdate path as the one establishing write. That is the
// intended effect after a recreation, and the reason a forget must never be issued
// when the object behind the binding has not changed. Writes per robot generation
// are then one, on the first frame, never per tick.
//
// The seen map is deliberately in-memory, for the same reason RobotReconciler.beats
// is: it mirrors process-local projector state, so persisting it would outlive the
// thing it describes and reintroduce the defect on the other side. Losing it on
// restart costs nothing — the projector is empty then too.
//
// The zero value is usable and inert until [RobotProjectionResetter.SetForgetter]
// installs a target; a manager running without the ControlStream has no projector
// to reset. Safe for concurrent use.
type RobotProjectionResetter struct {
	mu         sync.Mutex
	forgetters []ProjectionForgetter
	seen       map[robotBinding]types.UID
}

// robotBinding is what a UID is recorded against: a wire robot_id is unique per
// namespace only.
type robotBinding struct {
	namespace string
	robotID   string
}

// SetForgetter installs the per-robot state to reset: the telemetry Router, and the
// Zone Controller whose position memo the Router feeds. It exists because the Router
// is built after the controllers in cmd/manager: the resetter is handed to the Robot
// reconciler first and pointed at its targets once they exist. Nil entries are
// skipped, and no targets leaves the resetter inert.
func (r *RobotProjectionResetter) SetForgetter(fs ...ProjectionForgetter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgetters = r.forgetters[:0]
	for _, f := range fs {
		if f != nil {
			r.forgetters = append(r.forgetters, f)
		}
	}
}

// Observe records that wire robot_id robotID in namespace currently maps to the
// Robot object with this UID, and forgets the projector's recorded state for that
// robot when the binding has changed since the last observation. An empty robotID
// (a Robot with no swarmada.io/robot-id annotation) projects no telemetry in the
// first place and is ignored. It reports whether stale projector state was
// dropped.
//
// It performs no status write, and it causes one only when it reports true: the
// robot's next frame then re-establishes status (see the RA-1 note on the type).
func (r *RobotProjectionResetter) Observe(namespace, robotID string, uid types.UID) bool {
	if r == nil || robotID == "" || uid == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[robotBinding]types.UID)
	}
	key := robotBinding{namespace: namespace, robotID: robotID}
	prev, known := r.seen[key]
	r.seen[key] = uid
	// First sight of this binding in this process: the projector has no entry for
	// it either (both are process-local and start empty together), so there is
	// nothing stale to drop.
	if !known || prev == uid {
		return false
	}
	forgotten := false
	for _, f := range r.forgetters {
		if f.ForgetRobot(namespace, robotID) {
			forgotten = true
		}
	}
	return forgotten
}
