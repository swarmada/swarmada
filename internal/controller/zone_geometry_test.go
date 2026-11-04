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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// framedZone builds a zone with the standard rectangle and a declared frame.
func framedZone(name, frameID string, alignment fleetv1.FrameAlignment) *fleetv1.FleetZone {
	fz := leafZoneWithBounds(name)
	fz.Spec.PhysicalBounds.Frame = &fleetv1.CoordinateFrame{ID: frameID, Alignment: alignment}
	return fz
}

// RFC-0001 §9.1.6, FZ-G4: status.geometry.state is derived from spec.physicalBounds
// so that "has usable geometry" is decided in one place. Absent / FrameLocal /
// Containment are the three states, and frameId echoes spec.physicalBounds.frame.id.
func TestZoneGeometry_StateDerivation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		zone      *fleetv1.FleetZone
		wantState fleetv1.ZoneGeometryState
		wantFrame string
	}{
		{
			name:      "no physicalBounds is an identity-only zone",
			zone:      zoneObj("identity-only", ""),
			wantState: fleetv1.ZoneGeometryAbsent,
		},
		{
			name:      "site-aligned polygon participates in containment",
			zone:      framedZone("aligned", "site", fleetv1.FrameSiteAligned),
			wantState: fleetv1.ZoneGeometryContainment,
			wantFrame: "site",
		},
		{
			name:      "frame-local polygon is carried, never used for containment",
			zone:      framedZone("local", "amr-07/map", fleetv1.FrameFrameLocal),
			wantState: fleetv1.ZoneGeometryFrameLocal,
			wantFrame: "amr-07/map",
		},
		{
			// A polygon predating the frame model declares no frame. FZ-G2 requires
			// one, but the admission rule is not shipped, so the derivation must not
			// silently reclassify existing site-aligned geometry as unusable.
			name:      "polygon with no declared frame stays site-aligned",
			zone:      leafZoneWithBounds("unframed"),
			wantState: fleetv1.ZoneGeometryContainment,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, c := newZoneController(t, tc.zone)
			reconcileZone(t, z, tc.zone.Name)

			got := getZone(t, c, tc.zone.Name).Status.Geometry
			if got == nil {
				t.Fatalf("status.geometry is nil; want state %q", tc.wantState)
			}
			if got.State != tc.wantState {
				t.Errorf("status.geometry.state = %q, want %q", got.State, tc.wantState)
			}
			if got.FrameID != tc.wantFrame {
				t.Errorf("status.geometry.frameId = %q, want %q", got.FrameID, tc.wantFrame)
			}
		})
	}
}

// FZ-G7: geometry-derived behaviour degrades EXPLICITLY. The GeometryAvailable
// condition is the mechanism — while it is False the control plane must not claim
// boundary-breach detection for the zone.
func TestZoneCondition_GeometryAvailable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		zone       *fleetv1.FleetZone
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"identity-only", zoneObj("identity-only", ""), metav1.ConditionFalse, "IdentityOnly"},
		{"frame-local", framedZone("local", "amr-07/map", fleetv1.FrameFrameLocal), metav1.ConditionFalse, "FrameLocalGeometry"},
		{"site-aligned", framedZone("aligned", "site", fleetv1.FrameSiteAligned), metav1.ConditionTrue, "SiteAlignedPolygon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, c := newZoneController(t, tc.zone)
			reconcileZone(t, z, tc.zone.Name)

			cond := findCond(getZone(t, c, tc.zone.Name).Status.Conditions, "GeometryAvailable")
			if cond == nil {
				t.Fatalf("GeometryAvailable condition not published")
			}
			if cond.Status != tc.wantStatus || cond.Reason != tc.wantReason {
				t.Errorf("GeometryAvailable = %s/%s, want %s/%s",
					cond.Status, cond.Reason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// FZ-G3, and the reason it is a MUST NOT rather than a preference: the control plane
// never transforms between frames, so a FrameLocal polygon's vertices are not
// comparable with Robot.status.position. Using it for containment yields silently
// wrong containment — worse than supplying no geometry at all. The zone must be
// excluded from the derivation candidate set entirely: not treated as covering no
// space, and not as covering all space.
func TestZoneDerivation_FrameLocalPolygonNeverMatches(t *testing.T) {
	local := framedZone("local", "amr-07/map", fleetv1.FrameFrameLocal)
	z, c := newZoneController(t, local, robotAt("r1", "rid1", ""))

	reconcileZone(t, z, "local")
	// A position squarely inside the frame-local polygon's vertices.
	z.ObservePosition(context.Background(), telemetry.Frame{
		RobotID:  "rid1",
		Position: &telemetry.Position{X: 5, Y: 5, Floor: 0},
	})

	if got := getRobot(t, c, "r1", actionNS).Status.CurrentZone; got != "" {
		t.Errorf("currentZone = %q; a FrameLocal polygon must never be used for containment", got)
	}
}

// The site-aligned counterpart of the test above: excluding FrameLocal must not
// break the derivation for geometry that IS comparable.
func TestZoneDerivation_SiteAlignedPolygonStillMatches(t *testing.T) {
	aligned := framedZone("aligned", "site", fleetv1.FrameSiteAligned)
	z, c := newZoneController(t, aligned, robotAt("r1", "rid1", ""))

	reconcileZone(t, z, "aligned")
	z.ObservePosition(context.Background(), telemetry.Frame{
		RobotID:  "rid1",
		Position: &telemetry.Position{X: 5, Y: 5, Floor: 0},
	})

	if got := getRobot(t, c, "r1", actionNS).Status.CurrentZone; got != "aligned" {
		t.Errorf("currentZone = %q, want aligned", got)
	}
}

// RA-1 guard: a second reconcile with nothing changed must not rewrite status. The
// geometry block and its condition are derived from spec alone, so they must be
// byte-stable across reconciles.
func TestZoneGeometry_StableAcrossReconciles(t *testing.T) {
	z, c := newZoneController(t, framedZone("aligned", "site", fleetv1.FrameSiteAligned))

	reconcileZone(t, z, "aligned")
	rv := getZone(t, c, "aligned").ResourceVersion
	reconcileZone(t, z, "aligned")

	if got := getZone(t, c, "aligned").ResourceVersion; got != rv {
		t.Errorf("resourceVersion moved %s → %s on an unchanged reconcile", rv, got)
	}
}
