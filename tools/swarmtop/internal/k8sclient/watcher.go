// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sclient

import "context"

// FleetWatcher is the low-level informer seam: a stream of typed add/update/
// delete events for the four Swarmada workload CRDs (Robot, FleetAction,
// RobotProbe, FleetAdapter), plus the Kubernetes Events those robots emit. It
// exists so nothing above it — the snapshot Store, and therefore internal/ui —
// imports client-go or controller-runtime: they speak only view types and this
// interface, which is what makes the UI testable against an in-memory fake with
// no cluster (see FakeWatcher).
//
// The production implementation is cache-backed (controller-runtime informers,
// the same watch mechanism internal/controller uses). The Store is a reducer
// over this stream; a consumer that wanted raw events (option B in the design
// notes) can read Changes() directly instead.
type FleetWatcher interface {
	// Start registers the informers and blocks until their caches have synced
	// (or ctx is cancelled), then returns. Existing objects arrive as Added
	// events on Changes(). The informers keep running until ctx is done.
	Start(ctx context.Context) error

	// Changes streams add/update/delete events. It is unbuffered-safe: senders
	// must not assume a reader, so callers should drain it promptly. Closed when
	// the watch ends.
	Changes() <-chan FleetEvent

	// RobotEvents returns the current Kubernetes Events whose involvedObject is a
	// Robot, bucketed by robot name, newest-first. Pulled on demand (Events are
	// not part of the Changes() add/update/delete stream); a change to Events
	// arrives on Changes() as an EventsChanged poke so consumers know to re-read.
	RobotEvents() map[string][]EventView

	// Err reports why the watch stopped, if it stopped abnormally.
	Err() error
}

// EventKind is the kind of change carried by a FleetEvent.
type EventKind int

// EventKind values.
const (
	EventAdded EventKind = iota
	EventUpdated
	EventDeleted
)

// FleetEvent is one change on the watch stream. Exactly one of the resource
// pointers is non-nil, OR EventsChanged is true (a poke that some Robot's
// Kubernetes Events changed and RobotEvents() should be re-read). For a
// Deleted event the pointed-to view carries at least Name so a reducer can
// evict it.
type FleetEvent struct {
	Kind EventKind

	Robot   *RobotView
	Action    *FleetActionView
	Probe   *RobotProbeView
	Adapter *AdapterView
	Zone    *ZoneView

	// EventsChanged is a content-free poke: a core/v1 Event involving a Robot
	// was added/updated/deleted. Kind is not meaningful when this is set.
	EventsChanged bool
}
