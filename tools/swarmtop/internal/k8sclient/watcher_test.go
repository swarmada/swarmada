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
	"testing"
	"time"
)

// startWithFake builds a reducer Store over a FakeWatcher and starts it.
func startWithFake(t *testing.T) (*FakeWatcher, Store) {
	t.Helper()
	f := NewFakeWatcher(64)
	s := NewStoreFromWatcher(f)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(f.Close)
	return f, s
}

// waitChanged blocks until the reducer signals a change (it applies the event
// then notifies), so a following Snapshot() is guaranteed to see it.
func waitChanged(t *testing.T, s Store) {
	t.Helper()
	select {
	case <-s.Changed():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a Changed nudge")
	}
}

func TestReducer_AddUpdateDelete(t *testing.T) {
	f, s := startWithFake(t)

	f.EmitRobot(EventAdded, RobotView{Name: "robot-1", Phase: "Idle", BatteryPercent: i32(80)})
	waitChanged(t, s)
	if got := s.Snapshot().Robots; len(got) != 1 || got[0].Phase != "Idle" {
		t.Fatalf("after add: %+v", got)
	}

	f.EmitRobot(EventUpdated, RobotView{Name: "robot-1", Phase: "InProgress", BatteryPercent: i32(60)})
	waitChanged(t, s)
	if got := s.Snapshot().Robots; len(got) != 1 || got[0].Phase != "InProgress" || *got[0].BatteryPercent != 60 {
		t.Fatalf("after update: %+v", got)
	}

	f.EmitRobot(EventDeleted, RobotView{Name: "robot-1"})
	waitChanged(t, s)
	if got := s.Snapshot().Robots; len(got) != 0 {
		t.Fatalf("after delete, expected empty, got %+v", got)
	}
}

func TestReducer_AllTypesAndSorting(t *testing.T) {
	f, s := startWithFake(t)

	// Emit out of order to prove Snapshot sorts by name.
	f.EmitRobot(EventAdded, RobotView{Name: "robot-3"})
	waitChanged(t, s)
	f.EmitRobot(EventAdded, RobotView{Name: "robot-1"})
	waitChanged(t, s)
	f.EmitAction(EventAdded, FleetActionView{Name: "haul-1", Phase: "InProgress"})
	waitChanged(t, s)
	f.EmitProbe(EventAdded, RobotProbeView{Name: "cam-probe", ProbeType: "capability", RobotCount: 3, FailingCount: 1})
	waitChanged(t, s)
	f.EmitAdapter(EventAdded, AdapterView{Name: "vda5050-a", Conformance: "Passed"})
	waitChanged(t, s)

	snap := s.Snapshot()
	if len(snap.Robots) != 2 || snap.Robots[0].Name != "robot-1" || snap.Robots[1].Name != "robot-3" {
		t.Fatalf("robots not sorted: %+v", snap.Robots)
	}
	if len(snap.Actions) != 1 || snap.Actions[0].Name != "haul-1" {
		t.Fatalf("actions: %+v", snap.Actions)
	}
	if len(snap.Probes) != 1 || snap.Probes[0].FailingCount != 1 {
		t.Fatalf("probes: %+v", snap.Probes)
	}
	if len(snap.Adapters) != 1 || snap.Adapters[0].Conformance != "Passed" {
		t.Fatalf("adapters: %+v", snap.Adapters)
	}
}

func TestReducer_EventsPokeSurfacesRobotEvents(t *testing.T) {
	f, s := startWithFake(t)

	f.SetRobotEvents(map[string][]EventView{
		"robot-1": {{Type: "Warning", Reason: "CameraFault"}},
	})
	waitChanged(t, s) // the EventsChanged poke fires a nudge

	got := s.Snapshot().EventsByRobot
	if evs := got["robot-1"]; len(evs) != 1 || evs[0].Reason != "CameraFault" {
		t.Fatalf("events not surfaced via poke: %+v", got)
	}
}

func TestReducer_SnapshotIsIndependent(t *testing.T) {
	f, s := startWithFake(t)
	f.EmitRobot(EventAdded, RobotView{Name: "robot-1", Phase: "Idle"})
	waitChanged(t, s)

	snap := s.Snapshot()
	snap.Robots[0].Phase = "MUTATED" // caller mutation must not leak back

	if got := s.Snapshot().Robots[0].Phase; got != "Idle" {
		t.Fatalf("snapshot shared underlying state, got %q", got)
	}
}

// TestReducer_ZonesFlowThroughAndSort covers the FleetZone data path added for the zones screen:
// events reach the reducer, land in their own map, and come out of Snapshot sorted by name like
// every other collection. Emitted out of order deliberately.
func TestReducer_ZonesFlowThroughAndSort(t *testing.T) {
	f, s := startWithFake(t)

	f.EmitZone(EventAdded, ZoneView{Name: "yard", EstopStatus: "Triggered", RobotCount: 5})
	waitChanged(t, s)
	f.EmitZone(EventAdded, ZoneView{Name: "dock-a", EstopStatus: "Clear", IsLeaf: true})
	waitChanged(t, s)

	snap := s.Snapshot()
	if len(snap.Zones) != 2 || snap.Zones[0].Name != "dock-a" || snap.Zones[1].Name != "yard" {
		t.Fatalf("zones not sorted: %+v", snap.Zones)
	}
	if snap.Zones[1].EstopStatus != "Triggered" || snap.Zones[1].RobotCount != 5 {
		t.Fatalf("zone fields lost in reduce: %+v", snap.Zones[1])
	}

	// A deleted zone must leave the snapshot — a stale zone still showing "Triggered" would be a
	// safety claim about something that no longer exists.
	f.EmitZone(EventDeleted, ZoneView{Name: "yard"})
	waitChanged(t, s)
	if snap := s.Snapshot(); len(snap.Zones) != 1 || snap.Zones[0].Name != "dock-a" {
		t.Fatalf("delete not reduced: %+v", snap.Zones)
	}
}
