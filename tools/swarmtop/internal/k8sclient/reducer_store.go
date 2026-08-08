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
	"sort"
	"sync"
	"time"

	"k8s.io/client-go/rest"
)

// reducerStore is a snapshot Store built as a reducer over a FleetWatcher. It
// keeps name-keyed maps per resource type, applies the watcher's add/update/
// delete stream to them, and materializes a Fleet on Snapshot(). This is the
// "C on B" arrangement from the design notes: the option-C pull API the UI
// wants, implemented over the option-B event stream.
type reducerStore struct {
	watcher FleetWatcher

	mu       sync.RWMutex
	robots   map[string]RobotView
	actions    map[string]FleetActionView
	probes   map[string]RobotProbeView
	adapters map[string]AdapterView
	zones    map[string]ZoneView

	changed chan struct{}
}

// NewStore builds the production Store: a cache-backed FleetWatcher scoped to
// namespace ("" = all), reduced into a snapshot store.
func NewStore(cfg *rest.Config, namespace string) (Store, error) {
	w, err := NewCacheWatcher(cfg, namespace)
	if err != nil {
		return nil, err
	}
	return NewStoreFromWatcher(w), nil
}

// NewStoreFromWatcher wraps any FleetWatcher (the cache-backed one in
// production, a FakeWatcher in tests) in a snapshot Store.
func NewStoreFromWatcher(w FleetWatcher) Store {
	return &reducerStore{
		watcher:  w,
		robots:   map[string]RobotView{},
		actions:    map[string]FleetActionView{},
		probes:   map[string]RobotProbeView{},
		adapters: map[string]AdapterView{},
		zones:    map[string]ZoneView{},
		changed:  make(chan struct{}, 1),
	}
}

// Start begins draining the watcher's change stream, then starts the watcher
// (whose initial Added events populate the maps). The drain loop runs until the
// stream closes.
func (s *reducerStore) Start(ctx context.Context) error {
	go s.drain()
	return s.watcher.Start(ctx)
}

func (s *reducerStore) drain() {
	for ev := range s.watcher.Changes() {
		s.apply(ev)
		s.notify()
	}
}

// apply folds one event into the maps. EventsChanged pokes carry no resource —
// they just trigger a nudge so Snapshot() re-reads RobotEvents().
func (s *reducerStore) apply(ev FleetEvent) {
	if ev.EventsChanged {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case ev.Robot != nil:
		applyOne(s.robots, ev.Kind, ev.Robot.Name, *ev.Robot)
	case ev.Action != nil:
		applyOne(s.actions, ev.Kind, ev.Action.Name, *ev.Action)
	case ev.Probe != nil:
		applyOne(s.probes, ev.Kind, ev.Probe.Name, *ev.Probe)
	case ev.Zone != nil:
		applyOne(s.zones, ev.Kind, ev.Zone.Name, *ev.Zone)
	case ev.Adapter != nil:
		applyOne(s.adapters, ev.Kind, ev.Adapter.Name, *ev.Adapter)
	}
}

// applyOne upserts on Added/Updated and evicts on Deleted.
func applyOne[T any](m map[string]T, kind EventKind, name string, v T) {
	if kind == EventDeleted {
		delete(m, name)
		return
	}
	m[name] = v
}

func (s *reducerStore) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Changed returns the coalesced change-nudge channel.
func (s *reducerStore) Changed() <-chan struct{} { return s.changed }

// Snapshot materializes a sorted, independent Fleet from the current maps and
// pulls the latest Robot-bucketed Events from the watcher.
func (s *reducerStore) Snapshot() Fleet {
	s.mu.RLock()
	f := Fleet{
		SnapshotAt: time.Now(),
		Robots:     make([]RobotView, 0, len(s.robots)),
		Actions:      make([]FleetActionView, 0, len(s.actions)),
		Probes:     make([]RobotProbeView, 0, len(s.probes)),
		Adapters:   make([]AdapterView, 0, len(s.adapters)),
		Zones:      make([]ZoneView, 0, len(s.zones)),
	}
	for _, v := range s.robots {
		f.Robots = append(f.Robots, v)
	}
	for _, v := range s.actions {
		f.Actions = append(f.Actions, v)
	}
	for _, v := range s.probes {
		f.Probes = append(f.Probes, v)
	}
	for _, v := range s.adapters {
		f.Adapters = append(f.Adapters, v)
	}
	for _, v := range s.zones {
		f.Zones = append(f.Zones, v)
	}
	s.mu.RUnlock()

	sort.Slice(f.Robots, func(a, b int) bool { return f.Robots[a].Name < f.Robots[b].Name })
	sort.Slice(f.Actions, func(a, b int) bool { return f.Actions[a].Name < f.Actions[b].Name })
	sort.Slice(f.Probes, func(a, b int) bool { return f.Probes[a].Name < f.Probes[b].Name })
	sort.Slice(f.Adapters, func(a, b int) bool { return f.Adapters[a].Name < f.Adapters[b].Name })
	sort.Slice(f.Zones, func(a, b int) bool { return f.Zones[a].Name < f.Zones[b].Name })

	f.EventsByRobot = s.watcher.RobotEvents()
	return f
}
