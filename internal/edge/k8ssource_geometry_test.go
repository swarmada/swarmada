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

package edge

import (
	"context"
	"sort"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

const geoNS = "warehouse-g"

// zoneInFrame is a zone whose polygon (the 0-10 square) declares the given alignment.
func zoneInFrame(name string, alignment fleetv1.FrameAlignment) *fleetv1.FleetZone {
	fz := fzWithBounds(geoNS, name, 0, squarePoints())
	fz.Spec.PhysicalBounds.Frame = &fleetv1.CoordinateFrame{ID: name + "-frame", Alignment: alignment}
	return fz
}

// compositeWithPolygon is a composite zone that also carries a site-aligned polygon.
func compositeWithPolygon(name string, members ...string) *fleetv1.FleetZone {
	fz := zoneInFrame(name, fleetv1.FrameSiteAligned)
	fz.Spec.MemberZones = members
	return fz
}

func loadGeo(t *testing.T, objs ...client.Object) Config {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(edgeScheme(t)).WithObjects(objs...).Build()
	cfg, err := NewKubeConfigSource(c, geoNS).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func zoneNames(cfg Config) []string {
	out := make([]string, 0, len(cfg.Zones))
	for _, z := range cfg.Zones {
		out = append(out, z.Name)
	}
	sort.Strings(out)
	return out
}

// The edge geofences exactly the polygons the control plane uses for containment. A
// FrameLocal polygon's vertices are in a frame the control plane never transforms
// (FZ-G3), and a composite's polygon is a rendering envelope (FZ-C6): both zones read
// GeometryAvailable False, and an edge node MUST NOT report such a zone as geofenced
// (FZ-G7). A robot that declares one of them is left out of the robot map, as a robot
// whose zone has no polygon already is.
func TestKubeConfigSource_LoadsOnlyPolygonsUsedForContainment(t *testing.T) {
	cfg := loadGeo(t,
		zoneInFrame("lab", fleetv1.FrameFrameLocal),
		compositeWithPolygon("ward", "room-a"),
		zoneInFrame("bay", fleetv1.FrameSiteAligned),
		fzWithBounds(geoNS, "dock", 0, squarePoints()), // no frame: read as site-aligned
		robotInZone(geoNS, "r-lab", "lab"),
		robotInZone(geoNS, "r-ward", "ward"),
		robotInZone(geoNS, "r-bay", "bay"),
		robotInZone(geoNS, "r-dock", "dock"),
	)

	if got := zoneNames(cfg); len(got) != 2 || got[0] != "bay" || got[1] != "dock" {
		t.Fatalf("geofenced zones = %v; want [bay dock]: a FrameLocal or composite polygon "+
			"is never used for containment, so the edge must not geofence it", got)
	}
	for _, r := range []string{"r-lab", "r-ward"} {
		if z, ok := cfg.RobotZone[r]; ok {
			t.Errorf("%s is guarded against zone %q, whose polygon the control plane never uses", r, z)
		}
	}
	if cfg.RobotZone["r-bay"] != "bay" || cfg.RobotZone["r-dock"] != "dock" {
		t.Errorf("robot map = %v; want r-bay→bay and r-dock→dock", cfg.RobotZone)
	}
}

// nodeFrom builds an edge node over a Config loaded from the given objects.
func nodeFrom(t *testing.T, objs ...client.Object) *Node {
	t.Helper()
	n := New(loadGeo(t, objs...), audit.New(&audit.MemorySink{}, "v0.1.0"))
	n.deliveryTimeout = 60 * time.Millisecond
	n.confirmTimeout = 60 * time.Millisecond
	return n
}

// sendPosition streams one position for robotID and reports whether the edge issued
// an estop in response.
func sendPosition(t *testing.T, n *Node, robotID string, x, y float64) bool {
	t.Helper()
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()
	fs.in <- posMsg(robotID, x, y, 0)
	select {
	case m := <-fs.out:
		return m.GetEstop() != nil
	case <-time.After(150 * time.Millisecond):
		return false
	}
}

// A robot whose site-frame position lies outside a FrameLocal zone's vertices is not
// estopped by the edge: those vertices are not comparable with the position.
func TestEdge_NoBreachAgainstAFrameLocalPolygon(t *testing.T) {
	n := nodeFrom(t, zoneInFrame("lab", fleetv1.FrameFrameLocal), robotInZone(geoNS, "r-lab", "lab"))
	if sendPosition(t, n, "r-lab", 50, 50) {
		t.Fatal("the edge estopped a robot for leaving a FrameLocal polygon it must not geofence")
	}
}

// A robot outside its site-aligned terminal zone's polygon is still estopped.
func TestEdge_BreachAgainstASiteAlignedPolygonStillStops(t *testing.T) {
	n := nodeFrom(t, zoneInFrame("bay", fleetv1.FrameSiteAligned), robotInZone(geoNS, "r-bay", "bay"))
	if !sendPosition(t, n, "r-bay", 50, 50) {
		t.Fatal("no estop for a robot outside its site-aligned zone")
	}
}

// The edge evaluates the zone a robot is ASSIGNED (spec.zone), which is what the
// boundary-breach trigger is defined against (RFC-0001 #safety-edge-headless-emergency-stop).
// The control plane places a robot in a Containment zone by position
// (status.currentZone), so for a robot that has left its assigned zone the two name
// different zones: the control plane reports it in the zone it is now in, and the edge
// stops it for having left the one it was assigned. Evaluating the placed zone instead
// would compare a position with the zone derived from that same position, and no breach
// could ever be seen.
func TestEdge_GuardsTheAssignedZoneNotThePlacedOne(t *testing.T) {
	aisle := zoneInFrame("aisle", fleetv1.FrameSiteAligned)
	aisle.Spec.PhysicalBounds.Polygon = []fleetv1.Point{{X: 20, Y: 0}, {X: 30, Y: 0}, {X: 30, Y: 10}, {X: 20, Y: 10}}
	r := robotInZone(geoNS, "r-1", "bay")
	r.Status.CurrentZone = "aisle" // where the control plane placed it by position

	objs := []client.Object{zoneInFrame("bay", fleetv1.FrameSiteAligned), aisle, r}
	if cfg := loadGeo(t, objs...); cfg.RobotZone["r-1"] != "bay" {
		t.Fatalf("robot map = %v; want r-1 guarded against its assigned zone bay", cfg.RobotZone)
	}
	if !sendPosition(t, nodeFrom(t, objs...), "r-1", 25, 5) {
		t.Fatal("no estop for a robot inside aisle, outside its assigned zone bay")
	}
}
