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

import (
	"context"
	"sync"
)

// FakeWatcher is an in-memory FleetWatcher for tests: no cluster, no client-go.
// Tests drive it with the Emit* helpers to simulate informer add/update/delete
// events and assert how a consumer (the reducer Store, the UI) reacts. It plays
// the role a generated client-go fake clientset would if one existed — this
// repo has none, so the seam is faked at the FleetWatcher boundary instead.
type FakeWatcher struct {
	changes chan FleetEvent

	mu     sync.Mutex
	events map[string][]EventView
}

// NewFakeWatcher returns a FakeWatcher with the given channel buffer (large
// enough that Emit* never blocks in a test).
func NewFakeWatcher(buffer int) *FakeWatcher {
	if buffer < 1 {
		buffer = 64
	}
	return &FakeWatcher{
		changes: make(chan FleetEvent, buffer),
		events:  map[string][]EventView{},
	}
}

// Start is a no-op — a fake is always "synced".
func (f *FakeWatcher) Start(context.Context) error { return nil }

// Changes returns the event stream tests push to via the Emit* helpers.
func (f *FakeWatcher) Changes() <-chan FleetEvent { return f.changes }

// Err always returns nil; a fake never fails.
func (f *FakeWatcher) Err() error { return nil }

// RobotEvents returns whatever SetRobotEvents last set.
func (f *FakeWatcher) RobotEvents() map[string][]EventView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events
}

// SetRobotEvents replaces the RobotEvents map and fires an EventsChanged poke.
func (f *FakeWatcher) SetRobotEvents(m map[string][]EventView) {
	f.mu.Lock()
	f.events = m
	f.mu.Unlock()
	f.changes <- FleetEvent{EventsChanged: true}
}

// EmitRobot pushes a Robot add/update/delete event.
func (f *FakeWatcher) EmitRobot(kind EventKind, v RobotView) {
	f.changes <- FleetEvent{Kind: kind, Robot: &v}
}

// EmitAction pushes a FleetAction add/update/delete event.
func (f *FakeWatcher) EmitAction(kind EventKind, v FleetActionView) {
	f.changes <- FleetEvent{Kind: kind, Action: &v}
}

// EmitProbe pushes a RobotProbe add/update/delete event.
func (f *FakeWatcher) EmitProbe(kind EventKind, v RobotProbeView) {
	f.changes <- FleetEvent{Kind: kind, Probe: &v}
}

// EmitZone pushes a FleetZone add/update/delete event.
func (f *FakeWatcher) EmitZone(kind EventKind, v ZoneView) {
	f.changes <- FleetEvent{Kind: kind, Zone: &v}
}

// EmitAdapter pushes a FleetAdapter add/update/delete event.
func (f *FakeWatcher) EmitAdapter(kind EventKind, v AdapterView) {
	f.changes <- FleetEvent{Kind: kind, Adapter: &v}
}

// EmitTask pushes a FleetTask add/update/delete event.
func (f *FakeWatcher) EmitTask(kind EventKind, v FleetTaskView) {
	f.changes <- FleetEvent{Kind: kind, Task: &v}
}

// Close ends the Changes() stream.
func (f *FakeWatcher) Close() { close(f.changes) }
