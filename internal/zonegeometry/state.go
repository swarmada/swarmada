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

// Package zonegeometry classifies a FleetZone's polygon: whether it participates in
// containment (RFC-0001 FZ-G3, FZ-G4, FZ-C6). It is shared by the Zone Controller,
// which derives placement and status.geometry from it, and by the edge node, which
// geofences only what it classifies as containment (FZ-G7), so the two cannot
// disagree about which polygons are used. It reads the spec only; package zone stays
// free of any Kubernetes type.
package zonegeometry

import fleetv1 "github.com/swarmada/swarmada/api/v1"

// StateOf derives a zone's geometry state and frame id from its spec
// (RFC-0001 §9.1.6, FZ-G3/FZ-G4). It is the single place the question "does this
// polygon participate in containment" is answered: status.geometry echoes it for
// consumers, the GeometryAvailable condition reports it, the derivation candidate
// set is filtered on it, and the edge node geofences only on it — so none of them
// can disagree about one zone.
//
// The control plane never transforms between frames. A FrameLocal polygon's vertices
// are therefore not comparable with Robot.status.position, and using it for
// containment would yield silently wrong containment — worse than supplying no
// geometry at all, which is why FZ-G3 makes it a MUST NOT rather than a preference.
//
// A composite (a zone with members) is never Containment, whatever polygon it carries:
// its polygon is a declared envelope for rendering, and a robot's currentZone is always
// a terminal zone (FZ-C6). It reads Absent, the state that takes no part in containment.
// Deciding it here excludes a composite from the derivation candidate set by
// construction, as every containment consumer reads this helper.
func StateOf(spec *fleetv1.FleetZoneSpec) (fleetv1.ZoneGeometryState, string) {
	pb := spec.PhysicalBounds
	if pb == nil || len(pb.Polygon) == 0 || IsComposite(spec) {
		return fleetv1.ZoneGeometryAbsent, ""
	}
	if pb.Frame == nil {
		// A polygon written before the frame model existed declares no frame. FZ-G2
		// specifies one as required here and its admission rejection is not shipped,
		// so the derivation reads an absent frame the way every such polygon already
		// meant it — site-aligned. Reclassifying these as unusable would withdraw
		// containment from geometry that has it.
		return fleetv1.ZoneGeometryContainment, ""
	}
	if pb.Frame.Alignment == fleetv1.FrameFrameLocal {
		return fleetv1.ZoneGeometryFrameLocal, pb.Frame.ID
	}
	return fleetv1.ZoneGeometryContainment, pb.Frame.ID
}

// IsComposite reports whether a zone is a composite: it declares members (FZ-C2).
func IsComposite(spec *fleetv1.FleetZoneSpec) bool { return len(spec.MemberZones) > 0 }
