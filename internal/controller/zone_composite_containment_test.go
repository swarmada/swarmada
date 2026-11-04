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

// A composite's polygon is never used for containment (RFC-0001 FZ-C6): a robot's
// currentZone is always a terminal zone. Before, a composite carrying a site-aligned
// polygon classified as Containment like any other zone, so a robot inside it and inside
// a member could derive the composite as its currentZone. A zone estop on the member,
// which selects by currentZone, then did not reach it, and one on the composite is
// refused (FZ-C5).

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// squareZone is a zone with a site-aligned square polygon from (x0,y0) to (x1,y1).
func squareZone(name string, x0, y0, x1, y1 float64) *fleetv1.FleetZone {
	z := zoneObj(name, "")
	z.Spec.PhysicalBounds = &fleetv1.PhysicalBounds{
		Polygon: []fleetv1.Point{{X: x0, Y: y0}, {X: x1, Y: y0}, {X: x1, Y: y1}, {X: x0, Y: y1}},
		Frame:   &fleetv1.CoordinateFrame{ID: "site", Alignment: fleetv1.FrameSiteAligned},
	}
	return z
}

func TestZoneGeometry_ACompositeIsNeverContainment(t *testing.T) {
	ward := squareZone("ward", 0, 0, 20, 20)
	ward.Spec.MemberZones = []string{"room-a"}
	z, c := newZoneController(t, ward, squareZone("room-a", 9, 9, 19, 19))

	reconcileZone(t, z, "ward")
	got := getZone(t, c, "ward")
	if g := got.Status.Geometry; g == nil || g.State != fleetv1.ZoneGeometryAbsent || g.FrameID != "" {
		t.Fatalf("status.geometry = %+v; a composite with a site-aligned polygon must read Absent "+
			"(it takes no part in containment), not Containment", g)
	}
	cond := findCond(got.Status.Conditions, "GeometryAvailable")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "CompositeZone" {
		t.Fatalf("GeometryAvailable = %+v, want False/CompositeZone", cond)
	}

	// RA-1: written on the transition only. A second reconcile with nothing changed
	// writes nothing.
	rv := got.ResourceVersion
	reconcileZone(t, z, "ward")
	if again := getZone(t, c, "ward").ResourceVersion; again != rv {
		t.Fatalf("resourceVersion moved %s → %s on an unchanged reconcile", rv, again)
	}
}

func TestZoneDerivation_ARobotInACompositeAndAMemberIsInTheMember(t *testing.T) {
	// ward's centroid (10,10) is nearer the robot than room-a's (14,14), so while ward was
	// a containment candidate, overlap arbitration between the two leaves picked ward.
	ward := squareZone("ward", 0, 0, 20, 20)
	ward.Spec.MemberZones = []string{"room-a"}
	robot := robotAt("r1", "rid1", "")
	robot.Spec.Zone = "room-a"
	z, c := newZoneController(t, ward, squareZone("room-a", 9, 9, 19, 19), robot)

	reconcileZone(t, z, "ward")
	frame := telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 10, Y: 10, Floor: 0}}
	z.ObservePosition(context.Background(), frame)

	placed := getRobot(t, c, "r1", actionNS)
	if placed.Status.CurrentZone != "room-a" {
		t.Fatalf("currentZone = %q; a robot inside a composite's polygon and a member's is in the member "+
			"(room-a), never in the composite", placed.Status.CurrentZone)
	}

	// RA-1: an unchanged frame writes nothing.
	rv := placed.ResourceVersion
	z.ObservePosition(context.Background(), frame)
	if again := getRobot(t, c, "r1", actionNS).ResourceVersion; again != rv {
		t.Fatalf("an unchanged frame rewrote the robot's status (rv %s → %s)", rv, again)
	}

	// A zone estop on the member covers the robot.
	stopped := placed.DeepCopy()
	stopped.ResourceVersion = ""
	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil, zeZone("room-a", "", nil, "true"), zeZone("ward", "", nil, ""), stopped)
	reconcileZE(t, r, "room-a")
	if got := est.names(); len(got) != 1 || got[0] != "r1" {
		t.Fatalf("the zone estop on room-a stopped %v; want [r1], the robot inside it", got)
	}
}
