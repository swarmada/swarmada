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
	"reflect"
	"sync"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/tde"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// The reservation lifecycle and zone admission hang off a change of currentZone, and
// only off a change. ObservePosition also writes when only the containment set moved;
// that write must not be read as leaving the zone and re-entering it.

// recordingReservations records the zone signals the controller sends the TDE.
type recordingReservations struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingReservations) OnRobotEnteredZone(_ context.Context, _, zoneName, robot string) error {
	r.record("enter " + zoneName + " " + robot)
	return nil
}

func (r *recordingReservations) OnRobotExitedZone(_ context.Context, _, zoneName, robot string) error {
	r.record("exit " + zoneName + " " + robot)
	return nil
}

func (r *recordingReservations) record(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recordingReservations) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// A frame that moves the robot into a second, non-leaf zone while its arbitrated
// zone stays the same leaf keeps its Occupied reservation there, so the zone stays
// full to a third robot, and the robot held at that zone's boundary is not admitted.
func TestZoneObserver_ContainmentOnlyFrameKeepsReservationAndAdmission(t *testing.T) {
	ctx := context.Background()
	full := zoneWithCapacity("z", 1)
	z, c := newZoneController(t, full, robotAt("r1", "rid1", ""), robotAt("r2", "rid2", ""))
	engine := tde.New(c, tde.DefaultConfig())
	engine.SetRecovered(true)
	z.TDE = engine
	notifier := &fakeZoneAdmit{}
	z.Admission = notifier
	// "z" is the leaf the robot reports. "hall" overlaps its left half and is not a
	// leaf, so a robot inside both still reports "z" and only its containment set grows.
	hall := []fleetv1.Point{{X: 0, Y: 0}, {X: 60, Y: 0}, {X: 60, Y: 80}, {X: 0, Y: 80}}
	z.setZones(actionNS, []zone.Candidate{
		{Name: "z", Floor: 0, Polygon: toZonePoints(rectPts), IsLeaf: true},
		{Name: "hall", Floor: 0, Polygon: toZonePoints(hall), IsLeaf: false},
	})

	if res, err := engine.RequestReservation(ctx, tde.ReservationRequest{
		ActionID: "t1", RobotID: "r1", Namespace: actionNS, TargetZone: "z", Priority: fleetv1.ActionPriorityNormal,
	}); err != nil || res.Status != tde.Granted {
		t.Fatalf("reserve r1: status=%s err=%v", res.Status, err)
	}
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 100, Y: 40}})
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid2", Position: &telemetry.Position{X: 100, Y: 40}})
	if !notifier.has(admitCall{"r2", "z", false}) {
		t.Fatal("setup: r2 must be held at the boundary of a full zone")
	}

	// The containment-only frame.
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 30, Y: 40}})
	r1 := getRobot(t, c, "r1", actionNS)
	if r1.Status.CurrentZone != "z" || !reflect.DeepEqual(r1.Status.ContainingZones, []string{"hall", "z"}) {
		t.Fatalf("setup: r1 currentZone %q containingZones %v; want z and [hall z], a containment-only move",
			r1.Status.CurrentZone, r1.Status.ContainingZones)
	}

	zs := getZone(t, c, "z")
	if len(zs.Status.Reservations) != 1 || zs.Status.Reservations[0].RobotID != "r1" ||
		zs.Status.Reservations[0].State != fleetv1.ReservationOccupied {
		t.Fatalf("reservations = %+v, want r1 still Occupied: the robot never left the zone", zs.Status.Reservations)
	}
	res, err := engine.RequestReservation(ctx, tde.ReservationRequest{
		ActionID: "t3", RobotID: "r3", Namespace: actionNS, TargetZone: "z", Priority: fleetv1.ActionPriorityNormal,
	})
	if err != nil || res.Status != tde.Denied {
		t.Fatalf("reserve r3 in a zone r1 still occupies: status=%s err=%v, want Denied (maxConcurrentRobots=1)",
			res.Status, err)
	}
	if notifier.has(admitCall{"r2", "z", true}) {
		t.Fatal("r2 was admitted into a zone that is still full")
	}
	z.admitMu.Lock()
	held := z.held[actionNS+"/r2"]
	z.admitMu.Unlock()
	if !held {
		t.Fatal("r2's hold marker was cleared by a frame that did not change any robot's zone")
	}
}

// A genuine zone change still exits the old zone and enters the new one.
func TestZoneObserver_ZoneChangeStillExitsAndEnters(t *testing.T) {
	ctx := context.Background()
	z, _ := newZoneController(t, robotAt("r1", "rid1", ""))
	rec := &recordingReservations{}
	z.TDE = rec
	left := []fleetv1.Point{{X: 0, Y: 0}, {X: 50, Y: 0}, {X: 50, Y: 80}, {X: 0, Y: 80}}
	right := []fleetv1.Point{{X: 60, Y: 0}, {X: 120, Y: 0}, {X: 120, Y: 80}, {X: 60, Y: 80}}
	z.setZones(actionNS, []zone.Candidate{
		{Name: "left", Floor: 0, Polygon: toZonePoints(left), IsLeaf: true},
		{Name: "right", Floor: 0, Polygon: toZonePoints(right), IsLeaf: true},
	})

	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 20, Y: 40}})
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 90, Y: 40}})

	want := []string{"enter left r1", "exit left r1", "enter right r1"}
	if got := rec.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TDE signals = %v, want %v", got, want)
	}
}

// A robot placed by declaration, in a zone with no polygon, reaches the TDE and
// zone admission the same way a robot placed by position does (FZ-G6).
func TestZoneDeclaredMembership_PlacementReachesTDEAndAdmission(t *testing.T) {
	dock := zoneWithCapacity("dock", 1)
	z, _ := newZoneController(t, dock,
		declaredRobot("r1", "rid1", "dock", ""),
		declaredRobot("r2", "rid2", "dock", ""),
	)
	rec := &recordingReservations{}
	z.TDE = rec
	notifier := &fakeZoneAdmit{}
	z.Admission = notifier

	reconcileZone(t, z, "dock")

	got := rec.snapshot()
	for _, want := range []string{"enter dock r1", "enter dock r2"} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Fatalf("TDE signals = %v, missing %q", got, want)
		}
	}
	if !notifier.has(admitCall{"r2", "dock", false}) {
		t.Fatalf("admission pushes = %v, want r2 held at a full declared zone", notifier.calls)
	}
}
