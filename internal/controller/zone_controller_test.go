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
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/tde"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

var rectPts = []fleetv1.Point{{X: 0, Y: 0}, {X: 120, Y: 0}, {X: 120, Y: 80}, {X: 0, Y: 80}}

func newZoneController(t *testing.T, objs ...client.Object) (*ZoneController, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).
		Build()
	return &ZoneController{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(32)}, c
}

func zoneObj(name, parent string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetZoneSpec{ParentZone: parent}}
}

func robotAt(name, robotID, currentZone string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS, Annotations: map[string]string{RobotIDAnnotation: robotID}},
		Status:     fleetv1.RobotStatus{CurrentZone: currentZone},
	}
}

func reconcileZone(t *testing.T, z *ZoneController, name string) {
	t.Helper()
	if _, err := z.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: actionNS},
	}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

func getZone(t *testing.T, c client.Client, name string) *fleetv1.FleetZone {
	t.Helper()
	z := &fleetv1.FleetZone{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: actionNS}, z); err != nil {
		t.Fatalf("get zone %s: %v", name, err)
	}
	return z
}

func TestZoneReconcile_Topology(t *testing.T) {
	z, c := newZoneController(t,
		zoneObj("floor2", ""),
		zoneObj("left", "floor2"),
		zoneObj("right", "floor2"),
		robotAt("r1", "rid1", "left"), // in a descendant of floor2
	)

	reconcileZone(t, z, "floor2")
	parent := getZone(t, c, "floor2")
	if parent.Status.IsLeaf {
		t.Fatal("floor2 has children; isLeaf must be false")
	}
	if len(parent.Status.ChildZones) != 2 {
		t.Fatalf("childZones = %v, want 2 entries", parent.Status.ChildZones)
	}
	if parent.Status.RobotCount != 1 {
		t.Fatalf("robotCount = %d, want 1 (robot in descendant 'left')", parent.Status.RobotCount)
	}

	reconcileZone(t, z, "left")
	leaf := getZone(t, c, "left")
	if !leaf.Status.IsLeaf {
		t.Fatal("left has no children; isLeaf must be true")
	}
	if leaf.Status.RobotCount != 1 {
		t.Fatalf("leaf robotCount = %d, want 1", leaf.Status.RobotCount)
	}
}

func TestZoneObserver_DerivesAndSuppressesUnchanged(t *testing.T) {
	z, c := newZoneController(t, robotAt("r1", "rid1", ""))
	z.setZones(actionNS, []zone.Candidate{{Name: "z1", Floor: 0, Polygon: toZonePoints(rectPts), IsLeaf: true}})

	ctx := context.Background()
	frame := telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40, Floor: 0}}

	z.ObservePosition(ctx, frame)
	got := getRobot(t, c, "r1", actionNS)
	if got.Status.CurrentZone != "z1" {
		t.Fatalf("currentZone = %q, want z1", got.Status.CurrentZone)
	}
	rv := got.ResourceVersion

	// RA-1: an identical (position-only) frame must produce NO status write.
	z.ObservePosition(ctx, frame)
	if again := getRobot(t, c, "r1", actionNS); again.ResourceVersion != rv {
		t.Fatalf("unchanged frame caused a status write (rv %s → %s) — RA-1 violated", rv, again.ResourceVersion)
	}
}

func TestZoneObserver_UnmatchedClearsAndEvents(t *testing.T) {
	z, c := newZoneController(t, robotAt("r1", "rid1", "z1")) // starts in z1
	z.setZones(actionNS, []zone.Candidate{{Name: "z1", Floor: 0, Polygon: toZonePoints(rectPts), IsLeaf: true}})
	// seed the observer's cache as if z1 was the last written zone
	z.recordZone("rid1", "z1", "z1")

	// Position far outside every polygon.
	z.ObservePosition(context.Background(), telemetry.Frame{
		RobotID: "rid1", Position: &telemetry.Position{X: 999, Y: 999, Floor: 0},
	})

	got := getRobot(t, c, "r1", actionNS)
	if got.Status.CurrentZone != "" {
		t.Fatalf("currentZone = %q, want empty (unmatched)", got.Status.CurrentZone)
	}
	rec := z.Recorder.(*record.FakeRecorder)
	select {
	case e := <-rec.Events:
		if !contains(e, "ZonePositionUnmatched") {
			t.Fatalf("event = %q, want ZonePositionUnmatched", e)
		}
	default:
		t.Fatal("expected a ZonePositionUnmatched event")
	}
}

// Review-pass concurrency test: many concurrent position frames (same and
// different robots) must not race and must converge to the correct currentZone
// with no double-write. Run under -race.
func TestZoneObserver_ConcurrentFrames(t *testing.T) {
	const robots = 6
	const framesPerRobot = 40

	objs := make([]client.Object, 0, robots)
	for i := 0; i < robots; i++ {
		objs = append(objs, robotAt(fmt.Sprintf("r%d", i), fmt.Sprintf("rid%d", i), ""))
	}
	z, c := newZoneController(t, objs...)
	z.setZones(actionNS, []zone.Candidate{{Name: "z1", Floor: 0, Polygon: toZonePoints(rectPts), IsLeaf: true}})

	var wg sync.WaitGroup
	for i := 0; i < robots; i++ {
		for f := 0; f < framesPerRobot; f++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				z.ObservePosition(context.Background(), telemetry.Frame{
					RobotID:  fmt.Sprintf("rid%d", i),
					Position: &telemetry.Position{X: 60, Y: 40, Floor: 0},
				})
			}(i)
		}
	}
	wg.Wait()

	for i := 0; i < robots; i++ {
		got := getRobot(t, c, fmt.Sprintf("r%d", i), actionNS)
		if got.Status.CurrentZone != "z1" {
			t.Fatalf("robot r%d currentZone = %q, want z1", i, got.Status.CurrentZone)
		}
	}
}

// Integration: the ZoneController's currentZone transition drives the SHARED TDE
// engine's Reserved→Occupied (§9.4.2), off the existing position signal — no
// second position path.
func TestZoneObserver_EntrySignalsTDEOccupied(t *testing.T) {
	robot := robotAt("r1", "rid1", "") // currentZone empty; will enter "z"
	zoneZ := zoneObj("z", "")          // FleetZone the engine reads (maxConcurrentRobots=0)
	z, c := newZoneController(t, robot, zoneZ)

	engine := tde.New(c, tde.DefaultConfig())
	engine.SetRecovered(true) // model a running (recovered) engine; the grant gate is open
	z.TDE = engine
	z.setZones(actionNS, []zone.Candidate{{Name: "z", Floor: 0, Polygon: toZonePoints(rectPts), IsLeaf: true}})

	ctx := context.Background()
	// The FleetAction gate reserved a slot for this robot (RobotID = robot name "r1").
	if res, err := engine.RequestReservation(ctx, tde.ReservationRequest{
		ActionID: "t1", RobotID: "r1", Namespace: actionNS, TargetZone: "z", Priority: fleetv1.ActionPriorityNormal,
	}); err != nil || res.Status != tde.Granted {
		t.Fatalf("reserve: status=%s err=%v", res.Status, err)
	}

	// Robot's position resolves into zone z → currentZone transition → OnRobotEnteredZone.
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40, Floor: 0}})

	fz := &fleetv1.FleetZone{}
	if err := c.Get(ctx, types.NamespacedName{Name: "z", Namespace: actionNS}, fz); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	if len(fz.Status.Reservations) != 1 || fz.Status.Reservations[0].State != fleetv1.ReservationOccupied {
		t.Fatalf("reservation state = %+v, want a single Occupied entry (zone entry did not signal the TDE)", fz.Status.Reservations)
	}
}

func toZonePoints(pts []fleetv1.Point) []zone.Point {
	out := make([]zone.Point, len(pts))
	for i, p := range pts {
		out[i] = zone.Point{X: p.X, Y: p.Y}
	}
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
