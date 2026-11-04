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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FleetZoneSpec defines the layout and properties of a zone.
//
// A zone carrying a non-empty MemberZones is a COMPOSITE zone (FZ-C1..C6,
// RFC-0001 #fleetzone-composite-zones-and-zone-expansion). The two invariants a composite can violate within its own
// spec are rejected here rather than left to a controller, per the cross-field
// rule in docs/api-principles.md. The remaining FZ-C rules (FZ-C1's "not both a
// containment parent and a composite", member existence, cycles, and the depth
// bound) span OTHER objects and cannot be expressed in CEL over one spec. At v0.4
// the admission webhook does not check them either, so a composite that breaks
// them is accepted.
//
// FZ-C4: composition edges are traversed for targeting only, and a composite
// contains no physical space, so it holds no physical resource. A sharedResources
// name is not resolved along either edge: the TDE looks it up across every FleetZone
// in the namespace, and the zone whose name sorts first among those declaring it owns
// it. An entry on a composite would make the composite the owner of a lift or
// corridor it does not contain.
// +kubebuilder:validation:XValidation:rule="!has(self.memberZones) || size(self.memberZones) == 0 || !has(self.sharedResources) || size(self.sharedResources) == 0",message="spec.sharedResources is not permitted on a composite zone: a composite contains no physical space, and a shared resource is looked up by name across the namespace, so the composite would own a resource it does not contain"
//
// FZ-C6: capacity is enforced where robots are, and a robot is never IN a
// composite (FZ-C2). status.robotCount does not sum a composite's members: it
// counts the containment subtree only, so on a composite it counts only robots
// placed in the composite itself, normally 0.
// +kubebuilder:validation:XValidation:rule="!has(self.memberZones) || size(self.memberZones) == 0 || !has(self.maxConcurrentRobots) || self.maxConcurrentRobots == 0",message="spec.maxConcurrentRobots must be 0 on a composite zone: capacity is enforced on the zones robots occupy"
type FleetZoneSpec struct {
	// DisplayName is a human-readable label for this zone (e.g. "Receiving Dock A").
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// ParentZone references a parent FleetZone for hierarchical zones.
	// Leave empty for top-level zones. The FleetZone admission webhook rejects a
	// non-existent parent, a cycle A→B→A, and a chain that would give this zone or any zone
	// below it more than 64 ancestors, the depth a zone's emergency stop reaches
	// (RFC-0001 #control-plane-swarmada-api-server, #safety-estop-scopes). Each edit is judged
	// against the zone tree as admission reads it, so two edits admitted at the same moment can
	// together form a cycle or exceed that depth; a zone's stop then stops every robot whose
	// zone chain it cannot walk. An edit that would take a zone below this one out of the reach
	// of a stop that covers it is refused.
	// +optional
	ParentZone string `json:"parentZone,omitempty"`

	// MemberZones lists the zones this zone GROUPS, by name, in this zone's own
	// namespace. A zone with a non-empty MemberZones is a composite zone: a named
	// group whose content is other zones, so a task can target "the whole floor"
	// once instead of every consumer enumerating sub-zones by hand and drifting out
	// of step as zones are added and removed (RFC-0001 #fleetzone-composite-zones-and-zone-expansion).
	//
	// COMPOSITION IS NOT CONTAINMENT. It is a second relation and it traverses
	// differently:
	//
	//   - ParentZone is bottom-up (the child names its parent), asserts that the
	//     child's physical space lies inside the parent's, forms a TREE, and is
	//     traversed for targeting AND estopPolicy propagation. A reservation does not
	//     walk it: the TDE looks a resource name up across the whole namespace (see
	//     sharedResources). Robot admission does: a Robot's spec.charging.dockName
	//     must name a ChargingDock declared in the robot's zone or one of its
	//     ancestors.
	//   - MemberZones is top-down (the composite names its members), asserts
	//     NOTHING physical, forms a DAG (a zone MAY belong to several composites),
	//     and is traversed for TARGETING ONLY.
	//
	// The top-down direction is what makes composition usable on zones this control
	// plane did not author: rooms arriving from an adapter as identity-only zones
	// can be grouped from outside, without editing the adapter-owned resources, so
	// the adapter can keep reconciling them.
	//
	// Because composition asserts nothing physical, an estop on a composite would
	// stop nothing — two zones grouped for scheduling convenience may be at opposite
	// ends of a building. A silent no-op stop is a false assurance of the most
	// dangerous kind, so the estop path REJECTS a composite target rather than
	// accepting it (FZ-C5); stop the containment zone that physically contains them,
	// each member, or the namespace.
	//
	// Bounds: entries are zone names in this namespace. Breadth is deliberately not
	// capped here — RFC-0001 caps the expansion GRAPH at 8 edges deep (FZ-C3), not the
	// member list. Self-reference, member existence, cycles and the depth bound are
	// specified as admission checks. At v0.4 the admission webhook does not check any
	// of them, so a self-referencing, cyclic, too-deep or dangling composite is
	// accepted. A ZoneMaintenance scoped to such a composite reports the member as
	// Cycle, DepthExceeded or NotFound when it expands the scope.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:MaxLength=253
	MemberZones []string `json:"memberZones,omitempty"`

	// Waypoints is the list of named locations robots can navigate to within
	// this zone.  Each waypoint name must be unique within the zone.
	// +optional
	// +listType=map
	// +listMapKey=name
	Waypoints []Waypoint `json:"waypoints,omitempty"`

	// EdgeNode, when set, declares a Zone Controller edge node for this zone
	// (RFC-0001 #fleet-adapter-protocol-the-edge-stream, RFC-0001 #safety-edge-headless-emergency-stop). The API Server advertises its address in
	// RegisterAck.edge_endpoints to every adapter whose robots' zone chain
	// includes this zone; those adapters MUST open the EdgeStream.
	// +optional
	EdgeNode *EdgeNodeConfig `json:"edgeNode,omitempty"`

	// PhysicalBounds is the spatial extent of this zone. The Zone Controller uses
	// the polygon for point-in-polygon containment when deriving a robot's current
	// zone from position telemetry (RFC-0001 #control-plane-zone-controller).
	// +optional
	PhysicalBounds *PhysicalBounds `json:"physicalBounds,omitempty"`

	// MaxConcurrentRobots caps how many robots may occupy this zone at once. The
	// TDE enforces it; a robot attempting to enter a full zone is held at the
	// boundary (RFC-0001 #tde-requestreservation-algorithm). 0 = no limit (default; typical for root zones that
	// aggregate children).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MaxConcurrentRobots int32 `json:"maxConcurrentRobots,omitempty"`

	// SharedResources are physical resources (lifts, corridors, docks) that robots
	// reserve before use (RFC-0001 #fleetzone-sharedresource-types-and-reservation-policy).
	// A reservation names a resource, and the name is looked up across the whole
	// namespace, not up this zone's tree: a robot in any zone of the namespace can
	// reserve a resource declared on any zone. Two zones that declare the same name
	// declare one resource, owned by the zone whose name sorts first; the other
	// declaration's capacity and reservationPolicy are not used. Give each resource a
	// name unique in the namespace.
	// +optional
	SharedResources []SharedResource `json:"sharedResources,omitempty"`

	// EstopPolicy controls how an emergency stop on this zone propagates through
	// the zone tree (RFC-0001 #safety-estop-scopes). Absent = defaults (propagate to children, not to
	// parent).
	// +optional
	EstopPolicy *ZoneEstopPolicy `json:"estopPolicy,omitempty"`

	// ProvisioningPolicy is specified as a per-zone override of
	// SwarmadaConfig.spec.provisioning (RFC-0001 #fleetzone-schema-root-zone). No
	// controller reads it at this version: it is validated and stored, and setting it
	// changes nothing. Provisioning, including auto-admit, is governed by the namespace
	// SwarmadaConfig alone, so a zone-level override does not suppress a namespace-wide
	// auto-admit.
	// +optional
	ProvisioningPolicy *ZoneProvisioningPolicy `json:"provisioningPolicy,omitempty"`

	// Restrictions are standing statements about what may happen in this zone:
	// whether a robot may be here at all, whether it may perform a given job here,
	// and what it may have RUNNING while it is here (ADR-0058).
	//
	// Every other field on this spec is about CAPACITY, GEOMETRY, TOPOLOGY or an
	// EMERGENCY. None of them can say "no robots here": maxConcurrentRobots: 0 means
	// the OPPOSITE (0 = no limit), estopPolicy models an emergency with a
	// clear-and-resume lifecycle, and ZoneMaintenance is a window with a Completed
	// phase. A permanent boundary modelled as any of those is indistinguishable from
	// one that was forgotten.
	//
	// ABSENT MEANS THE ZONE STATES NO RESTRICTION, WHICH PERMITS EVERYTHING. That is
	// what every FleetZone written before this field existed meant, and it is the
	// whole compatibility story: the field is additive and optional, no existing
	// object changes meaning, and no API version bump is required.
	//
	// At v0.4 the Zone Controller folds these rules and every ancestor's into
	// status.restrictions.effective. The Scheduler fails an action whose target
	// zone denies entry or a capability the action requires, and dispatches into a
	// zone with operating rules only a robot whose adapter declares it honours them.
	// The resolved operating rules are sent to the adapter on the assignment; the
	// control plane cannot check that the robot honours them. entry.occupantAction
	// moves no robot.
	// +optional
	Restrictions *ZoneRestrictions `json:"restrictions,omitempty"`
}

// Point is a 2-D coordinate in the site coordinate frame (metres from the site
// origin, per SwarmadaConfig.spec.coordinateSystem). 0.0 is a valid coordinate.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// FrameAlignment says whether a polygon's vertices are directly comparable with
// every other spatial value in the namespace (§9.1.6, FZ-G3).
// +kubebuilder:validation:Enum=SiteAligned;FrameLocal
type FrameAlignment string

// FrameAlignment values.
const (
	// FrameSiteAligned: the vertices are comparable with Robot.status.position and the
	// edge PositionFrame stream, in the frame SwarmadaConfig.spec.coordinateSystem
	// declares. The polygon participates in containment derivation, boundary-breach
	// detection and overlap arbitration.
	FrameSiteAligned FrameAlignment = "SiteAligned"
	// FrameFrameLocal: they are not. The polygon is carried for consumers that own the
	// transform (visualization, an import tool, the producing adapter) and MUST NOT be
	// used for containment. An adapter that cannot vouch for site alignment MUST
	// declare this: a wrong SiteAligned claim yields silently wrong containment, which
	// is worse than supplying no geometry at all.
	FrameFrameLocal FrameAlignment = "FrameLocal"
)

// CoordinateFrame declares the frame a zone's polygon vertices are expressed in
// (§9.1.6). Coordinates with no stated frame are not geometry; they are numbers.
type CoordinateFrame struct {
	// ID is meaningful to whoever produced the geometry: "site" for an
	// operator-drawn floor plan in the namespace coordinate system, or a robot/SLAM
	// frame name such as "map" or "amr-07/map" for a polygon exported from an
	// occupancy grid. Free-form; never resolved by the control plane.
	// +kubebuilder:validation:MaxLength=253
	ID string `json:"id,omitempty"`

	// Alignment decides whether this polygon participates in containment (FZ-G3).
	// The control plane NEVER transforms between frames.
	// +kubebuilder:default=SiteAligned
	Alignment FrameAlignment `json:"alignment,omitempty"`

	// Source is optional provenance, for operators tracing where a polygon came
	// from. Informational; read by no controller.
	// +optional
	Source string `json:"source,omitempty"`
}

// PhysicalBounds defines a zone's floor and boundary polygon (RFC-0001 #fleetzone-schema-root-zone).
type PhysicalBounds struct {
	// Frame is the coordinate frame the Polygon vertices are expressed in (§9.1.6).
	// Specified as required whenever Polygon is set (FZ-G2). An absent Frame on a
	// zone that carries a polygon is read as SiteAligned with an empty id: that is
	// what every polygon written before this field existed meant, and reclassifying
	// such a zone as unusable would withdraw containment from geometry that has it.
	// +optional
	Frame *CoordinateFrame `json:"frame,omitempty"`

	// Floor is the integer floor number (0 = ground), interpreted relative to
	// SwarmadaConfig.spec.coordinateSystem.groundFloor.
	// +kubebuilder:validation:Minimum=-10
	// +kubebuilder:validation:Maximum=200
	Floor int32 `json:"floor"`

	// Polygon is the closed zone boundary (the last vertex connects back to the
	// first). At least 3 vertices are required; a self-intersecting polygon is
	// rejected by the FleetZone admission webhook (RFC-0001 #control-plane-swarmada-api-server).
	// +kubebuilder:validation:MinItems=3
	Polygon []Point `json:"polygon"`

	// MinAltitude and MaxAltitude are specified as the zone's vertical extent in
	// metres for geodetic (aerial) namespaces only, and are unset for ground (Local)
	// zones. No controller reads them at this version and they are not sent to any
	// adapter or edge node, so no altitude floor or ceiling is enforced from them.
	// +optional
	MinAltitude *float64 `json:"minAltitude,omitempty"`
	// +optional
	MaxAltitude *float64 `json:"maxAltitude,omitempty"`
}

// EdgeNodeConfig points at a facility-LAN Zone Controller edge node (RFC-0001 #fleet-adapter-protocol-the-edge-stream).
type EdgeNodeConfig struct {
	// Address is the host:port the adapter dials on the facility LAN.
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
	// TLSSecretRef is specified to name the Secret holding the edge node's
	// server-certificate material, issued by a CA the operator provisions
	// (RFC-0001 #security-certificate-management). No controller reads it at this
	// version: the edge node's server certificate is not configured from this Secret.
	// +optional
	TLSSecretRef string `json:"tlsSecretRef,omitempty"`
}

// SharedResourceType classifies a shared resource (RFC-0001 #fleetzone-sharedresource-types-and-reservation-policy).
// +kubebuilder:validation:Enum=ChargingDock;Corridor;Elevator;Entrance;Custom
type SharedResourceType string

// SharedResourceType values.
const (
	// SharedResourceChargingDock is a charging dock.
	SharedResourceChargingDock SharedResourceType = "ChargingDock"
	// SharedResourceCorridor is a shared corridor.
	SharedResourceCorridor SharedResourceType = "Corridor"
	// SharedResourceElevator is a lift/elevator.
	SharedResourceElevator SharedResourceType = "Elevator"
	// SharedResourceEntrance is a shared entrance (e.g. with personnel).
	SharedResourceEntrance SharedResourceType = "Entrance"
	// SharedResourceCustom is a site-specific resource.
	SharedResourceCustom SharedResourceType = "Custom"
)

// ReservationPolicy orders reservation grants for a shared resource (RFC-0001 #tde-shared-resource-queue-management).
// +kubebuilder:validation:Enum=FIFO;Priority;PriorityWithDuration;Manual
type ReservationPolicy string

// ReservationPolicy values.
const (
	// ReservationFIFO grants in arrival order.
	ReservationFIFO ReservationPolicy = "FIFO"
	// ReservationPriority orders by priority band DESC, then requestedAt ASC.
	ReservationPriority ReservationPolicy = "Priority"
	// ReservationPriorityWithDuration orders by band DESC, then shortest job first.
	ReservationPriorityWithDuration ReservationPolicy = "PriorityWithDuration"
	// ReservationManual disables automatic reservation; an operator manages access.
	ReservationManual ReservationPolicy = "Manual"
)

// DurationFallback is specified to govern PriorityWithDuration ordering when a
// request omits estimatedDurationSeconds (RFC-0001 #tde-shared-resource-queue-management).
// No code reads it at this version: the reference TDE orders a request without a
// duration last in its priority band, then by arrival, whichever value is set.
// +kubebuilder:validation:Enum=FIFO;Deprioritize
type DurationFallback string

// DurationFallback values.
const (
	// DurationFallbackFIFO treats a request without a duration as last in its band.
	DurationFallbackFIFO DurationFallback = "FIFO"
	// DurationFallbackDeprioritize treats a missing duration as MaxInt32 (goes last).
	DurationFallbackDeprioritize DurationFallback = "Deprioritize"
)

// SharedResource declares a reservable physical resource (RFC-0001 #fleetzone-sharedresource-types-and-reservation-policy).
type SharedResource struct {
	// Name is the name robots reserve by. It is looked up across the whole
	// namespace, so give each resource a name no other zone in the namespace
	// declares (see FleetZoneSpec.sharedResources).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Type classifies the resource.
	Type SharedResourceType `json:"type"`
	// DisplayName is a human-readable label.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Capacity is the maximum number of simultaneous reservation holders (usually
	// 1 for a physical resource).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Capacity int32 `json:"capacity,omitempty"`
	// ReservationPolicy orders grants for this resource.
	// +optional
	ReservationPolicy ReservationPolicy `json:"reservationPolicy,omitempty"`
	// DurationFallback is specified to apply only under PriorityWithDuration. No
	// code reads it at this version, so FIFO and Deprioritize behave alike: a request
	// without a duration is ordered last in its priority band, then by arrival.
	// +kubebuilder:default=FIFO
	DurationFallback DurationFallback `json:"durationFallback,omitempty"`
}

// ZoneEstopPolicy controls estop propagation through the zone tree (RFC-0001 #safety-estop-scopes).
type ZoneEstopPolicy struct {
	// PropagateToChildren: an estop on this zone also triggers estop on all
	// descendant zones. Recommended true; unset means true.
	// +kubebuilder:default=true
	// +optional
	PropagateToChildren *bool `json:"propagateToChildren,omitempty"`
	// PropagateToParent: an estop on this zone notifies the parent (which does NOT
	// auto-trigger its own estop — it receives a ChildEstopTriggered event).
	// +kubebuilder:default=false
	PropagateToParent bool `json:"propagateToParent,omitempty"`
}

// ZoneProvisioningPolicy overrides SwarmadaConfig.spec.provisioning for a zone
// (RFC-0001 #fleetzone-schema-root-zone). Reuses the namespace-level ProvisioningMode enum.
type ZoneProvisioningPolicy struct {
	// Mode overrides the namespace provisioning mode for this zone.
	// +optional
	Mode ProvisioningMode `json:"mode,omitempty"`
	// AutoAdmitRobotClass overrides the namespace auto-admit class for this zone.
	// +optional
	AutoAdmitRobotClass string `json:"autoAdmitRobotClass,omitempty"`
	// AutoAdmitZone overrides the auto-admit target zone.
	// +optional
	AutoAdmitZone string `json:"autoAdmitZone,omitempty"`
}

// Waypoint is a named pose within a FleetZone.
type Waypoint struct {
	// Name is the logical name used in FleetAction destinations.
	// +kubebuilder:validation:MinLength=1
	Name string  `json:"name"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	// Yaw is the preferred heading in radians (0 = east, π/2 = north).
	// +optional
	Yaw float64 `json:"yaw,omitempty"`
	// Altitude is the waypoint's vertical position in metres (geodetic/aerial
	// namespaces only). It does not replace the zone Floor, which is required in
	// every frame. Unset for Local.
	// +optional
	Altitude *float64 `json:"altitude,omitempty"`
}

// ZoneEstopStatus is a FleetZone's aggregate emergency-stop status (RFC-0001 #fleetzone-schema-root-zone).
// +kubebuilder:validation:Enum=Clear;Triggered;ChildTriggered
type ZoneEstopStatus string

// ZoneEstopStatus values.
const (
	// ZoneEstopClear: no active estop in this zone or any descendant.
	ZoneEstopClear ZoneEstopStatus = "Clear"
	// ZoneEstopTriggered: this zone's own estop is active.
	ZoneEstopTriggered ZoneEstopStatus = "Triggered"
	// ZoneEstopChildTriggered: a descendant zone's estop is active.
	ZoneEstopChildTriggered ZoneEstopStatus = "ChildTriggered"
)

// ZoneGeometryState is the Zone Controller's one-place answer to "does this zone
// have usable geometry" (§9.1.6). Consumers read it rather than inspecting spec.
// +kubebuilder:validation:Enum=Absent;FrameLocal;Containment
type ZoneGeometryState string

// ZoneGeometryState values.
const (
	// ZoneGeometryAbsent: no polygon. An identity-only zone; membership in it is
	// declared, not derived (FZ-G5). It covers UNKNOWN space — not no space, which
	// would leave every robot in it unmatched, and not all space, which would match
	// every robot in the namespace.
	ZoneGeometryAbsent ZoneGeometryState = "Absent"
	// ZoneGeometryFrameLocal: a polygon in a frame not aligned to the site frame.
	// Carried for consumers; never used for containment (FZ-G3).
	ZoneGeometryFrameLocal ZoneGeometryState = "FrameLocal"
	// ZoneGeometryContainment: a site-aligned polygon. Participates in derivation,
	// boundary-breach detection and overlap arbitration.
	ZoneGeometryContainment ZoneGeometryState = "Containment"
)

// ZoneGeometryStatus is the derived geometry state of a FleetZone (§9.1.6).
type ZoneGeometryStatus struct {
	// State is the derived usability of this zone's geometry.
	State ZoneGeometryState `json:"state,omitempty"`

	// FrameID echoes spec.physicalBounds.frame.id; empty when State is Absent.
	// +optional
	FrameID string `json:"frameId,omitempty"`
}

// MemberResolutionReason says why a declared member zone did not resolve during
// expansion (RFC-0001 #fleetzone-composite-zones-and-zone-expansion). A member can disappear after admission accepted
// it — an operator deletes a room, an adapter stops reconciling a zone it no
// longer sees, or a member is created after the composite that names it — so the
// reason is recorded rather than the member silently vanishing from the group.
// +kubebuilder:validation:Enum=NotFound;Cycle;DepthExceeded
type MemberResolutionReason string

// MemberResolutionReason values.
const (
	// MemberNotFound: the named zone does not exist in this namespace. It was
	// deleted, or has not been created yet. Deleting a zone that is a member of a
	// composite is PERMITTED by design — blocking it would let any group hold a
	// zone hostage — so this is an expected state, not a corruption.
	MemberNotFound MemberResolutionReason = "NotFound"
	// MemberCycle: expansion revisited a zone it had already entered. At v0.4
	// admission does not check composition cycles, so expansion is the only place
	// one is caught (FZ-C3).
	MemberCycle MemberResolutionReason = "Cycle"
	// MemberDepthExceeded: expansion passed the 8-edge depth bound (FZ-C3).
	MemberDepthExceeded MemberResolutionReason = "DepthExceeded"
)

// UnresolvedMember names a declared member zone that expansion could not resolve,
// and why.
type UnresolvedMember struct {
	// Name is the member zone name as declared in spec.memberZones.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Reason is why it did not resolve.
	// +kubebuilder:validation:Required
	Reason MemberResolutionReason `json:"reason"`
}

// ZoneComposition is specified as the Zone Controller's published answer to "what
// does this zone actually denote", so the state is inspectable BEFORE anything is
// targeted at it (RFC-0001 #fleetzone-composite-zones-and-zone-expansion). No
// controller writes it at this version; the rules below are the specified contract.
//
// PARTIAL RESOLUTION IS NOT ONE BEHAVIOUR. Consumers of this block fall into two
// classes that MUST treat UnresolvedMembers differently, and the difference is a
// safety property, not a preference:
//
//   - TARGETING (scheduler candidate filter, TDE reservation admission,
//     FleetAction, listings, dashboards) proceeds against ExpandedZones. An action
//     targeting a nine-room floor where one room was deleted stays schedulable into
//     the other eight; refusing all work because one member vanished converts a
//     bookkeeping error into a fleet-wide outage. An EMPTY ExpandedZones is denied
//     rather than silently matching nothing.
//   - SAFETY AND OPERATIONAL HOLDS (ZoneMaintenance scope, graceful drain, any
//     pause or withhold) are ALL-OR-NOTHING. A window whose scope does not fully
//     resolve MUST NOT activate. A pause that silently misses a room is a
//     technician walking into a zone they were told was drained — partial
//     assurance is worse than none here, because it is believed.
type ZoneComposition struct {
	// IsComposite is true when spec.memberZones is non-empty. Consumers read this
	// rather than inspecting spec, so "is this a composite" is decided in one
	// place — the same discipline status.geometry applies to geometry.
	// +optional
	IsComposite bool `json:"isComposite,omitempty"`

	// ExpandedZones is the resolved set of TERMINAL zones this zone denotes — the
	// zones a robot can actually occupy. Expansion is a set union, so a zone
	// reachable by two paths appears once, a robot in it counts once against
	// capacity and holds one reservation, and the result is order-independent.
	// +optional
	// +listType=set
	ExpandedZones []string `json:"expandedZones,omitempty"`

	// UnresolvedMembers names the declared members expansion could not resolve,
	// with a reason each. The CompositionResolved condition is specified to be True
	// only when this list is empty; no controller publishes that condition (or this
	// block) at this version. Read the type comment before consuming it: whether a
	// non-empty list may be proceeded past depends on which consumer class you are.
	// +optional
	// +listType=map
	// +listMapKey=name
	UnresolvedMembers []UnresolvedMember `json:"unresolvedMembers,omitempty"`
}

// FleetZoneStatus describes the observed state of a FleetZone.
type FleetZoneStatus struct {
	// IsLeaf is true when this zone has no child zones. Computed by the Zone
	// Controller and recomputed whenever a child FleetZone is created or deleted
	// (RFC-0001 #control-plane-zone-controller). A zone not yet reconciled reports false (safe default: not treated
	// as a leaf until confirmed — the Robot admission gate requires a leaf zone).
	// +optional
	IsLeaf bool `json:"isLeaf,omitempty"`

	// ChildZones is the list of direct child zone names, written by the Zone
	// Controller (RFC-0001 #control-plane-zone-controller).
	// +optional
	// +listType=set
	ChildZones []string `json:"childZones,omitempty"`

	// Geometry is this zone's derived geometry state, written by the Zone Controller
	// from spec.physicalBounds (§9.1.6). Consumers read this rather than inspecting
	// spec, so that "has usable geometry" is decided in one place.
	// +optional
	Geometry *ZoneGeometryStatus `json:"geometry,omitempty"`

	// Composition is specified as this zone's derived composition state, computed
	// from spec.memberZones, on every zone (a terminal zone would report IsComposite
	// false with ExpandedZones naming itself). No controller writes it at this
	// version: it is absent on every zone, and a client MUST read its absence as "not
	// computed", never as "not a composite" or "expands to nothing". Only
	// ZoneMaintenance scope resolution expands a composite zone, from spec.memberZones
	// directly.
	// +optional
	Composition *ZoneComposition `json:"composition,omitempty"`

	// RobotCount is the number of robots whose currentZone is this zone or any
	// zone below it in the parentZone tree. Composite members (spec.memberZones)
	// are not counted. Written by the Zone Controller (RFC-0001 #control-plane-zone-controller).
	// +optional
	// +kubebuilder:validation:Minimum=0
	RobotCount int32 `json:"robotCount,omitempty"`

	// EstopStatus is the zone's aggregate emergency-stop status, written by the
	// zone-estop controller (RFC-0001 #control-plane-zone-controller). Empty is treated as Clear.
	// +optional
	EstopStatus ZoneEstopStatus `json:"estopStatus,omitempty"`

	// LastEstopAt is the time of the most recent estop trigger in this zone tree.
	// Empty when EstopStatus is Clear.
	// +optional
	LastEstopAt *metav1.Time `json:"lastEstopAt,omitempty"`

	// EdgeFeedUnavailable lists robots assigned to this edge-node zone from which
	// the edge node has received no EdgeStream PositionFrame within its grace
	// window — i.e. an adapter serving them never established its EdgeStream, so the
	// zone-boundary-breach trigger is degraded for those robots. Written by the edge
	// node; the Zone Controller emits an EdgeFeedUnavailable Warning event while this
	// is non-empty. Empty when every expected feed is live (RFC-0001 #fleet-adapter-protocol-the-edge-stream).
	// +optional
	// +listType=set
	EdgeFeedUnavailable []string `json:"edgeFeedUnavailable,omitempty"`

	// CurrentConcurrentRobots is the number of Occupied reservations (robots
	// physically confirmed in this zone). Written by the TDE (RFC-0001 #tde-normative-invariants).
	// At v0.4 it does not exceed spec.maxConcurrentRobots. A Critical or High preemption
	// of an Occupied reservation removes the evicted entry before it grants (TDE-3), so
	// while the evicted robot is still in the zone, the zone holds one robot more than
	// this count shows.
	// +optional
	CurrentConcurrentRobots int32 `json:"currentConcurrentRobots,omitempty"`

	// Reservations is the per-action zone-capacity reservation state, written by the
	// Traffic Deconfliction Engine (RFC-0001 #tde-normative-invariants). At v0.4 no
	// admission webhook checks a write to it: the FleetZone webhook does not see the
	// status subresource. The shipped swarmada:viewer, operator, fleet-manager and admin
	// roles cannot write FleetZone status, so they cannot edit it. An identity that can
	// (system:masters or another cluster-admin, the manager's and the edge node's
	// service accounts) is not refused: its edit is stored and stays until the TDE's next
	// write of this zone's status replaces it in part.
	// +optional
	// +listType=atomic
	Reservations []ZoneReservation `json:"reservations,omitempty"`

	// SharedResourceQueues is the per-resource reservation/queue state, one entry
	// per spec.sharedResources[]. Written exclusively by the TDE (RFC-0001 #tde-normative-invariants).
	// +optional
	// +listType=atomic
	SharedResourceQueues []SharedResourceQueue `json:"sharedResourceQueues,omitempty"`
	// ActiveActions is specified as the number of FleetActions running in this
	// zone. At v0.4 no controller writes it, so it always reads 0.
	// +optional
	ActiveActions int32 `json:"activeActions,omitempty"`

	// Restrictions is the RESOLVED restriction set in force at this zone: its own
	// spec.restrictions folded together with every ancestor's, one entry per
	// (axis, subject), each naming the zone it came from (FZ-R4).
	//
	// CONSUMERS READ THIS, NEVER THE TREE — the Scheduler's candidate filters
	// included. "What is forbidden here" is decided in one place for the same reason
	// status.geometry exists rather than leaving readers to interpret
	// spec.physicalBounds: a second implementation of the fold is a second answer.
	//
	// Nil means the Zone Controller has not yet reconciled this zone. That is NOT the
	// same as "nothing is restricted", which is a non-nil value with an empty
	// effective list — and the difference is load-bearing, because a consumer that
	// reads nil as unrestricted fails in the permissive direction. Read the
	// RestrictionsResolved condition, which is published either way.
	// +optional
	Restrictions *ZoneRestrictionsStatus `json:"restrictions,omitempty"`

	// Conditions is the standard conditions list, written by the Zone Controller:
	// ZoneReady (serviceability), CapacityAvailable (room for another robot),
	// GeometryAvailable (whether geometry-derived behaviour applies, FZ-G7), and the
	// two restriction conditions RestrictionsResolved and RestrictionsConsistent
	// (FZ-R3/FZ-R6).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ReservationState is the lifecycle state of a zone-capacity reservation (RFC-0001 #tde-reservation-state).
// +kubebuilder:validation:Enum=Reserved;Occupied
type ReservationState string

// ReservationState values.
const (
	// ReservationReserved: slot granted; robot not yet confirmed in the zone.
	// Has an expiresAt (grantedAt + reservationTTLSeconds).
	ReservationReserved ReservationState = "Reserved"
	// ReservationOccupied: Zone Controller confirmed the robot entered the zone.
	// No expiry while physically present.
	ReservationOccupied ReservationState = "Occupied"
)

// ZoneReservation is one action's zone-capacity reservation (RFC-0001 #tde-reservation-state). TDE-owned.
type ZoneReservation struct {
	// RobotID holds or awaits this slot.
	RobotID string `json:"robotID"`
	// ActionID is the FleetAction owning this reservation.
	ActionID string `json:"actionID"`
	// Priority is the owning action's band.
	Priority ActionPriority `json:"priority"`
	// State is Reserved or Occupied.
	State ReservationState `json:"state"`
	// GrantedAt is when the reservation was granted.
	GrantedAt metav1.Time `json:"grantedAt"`
	// ExpiresAt is when a Reserved slot expires; null while Occupied.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// EnteredAt is when the robot entered the zone (Reserved→Occupied).
	// +optional
	EnteredAt *metav1.Time `json:"enteredAt,omitempty"`
}

// ResourceHolder is the current holder of a shared resource (RFC-0001 #tde-shared-resource-queue-management).
type ResourceHolder struct {
	RobotID  string         `json:"robotID"`
	ActionID string         `json:"actionID"`
	Priority ActionPriority `json:"priority"`
	// HeldSince is when the hold was granted.
	HeldSince metav1.Time `json:"heldSince"`
	// EstimatedReleaseAt is set when an estimated duration was provided.
	// +optional
	EstimatedReleaseAt *metav1.Time `json:"estimatedReleaseAt,omitempty"`
}

// WaitQueueEntry is a pending shared-resource request, ordered by the resource's
// reservationPolicy (RFC-0001 #tde-shared-resource-queue-management).
type WaitQueueEntry struct {
	RobotID  string         `json:"robotID"`
	ActionID string         `json:"actionID"`
	Priority ActionPriority `json:"priority"`
	// RequestedAt is the arrival time (FIFO tiebreak within band).
	RequestedAt metav1.Time `json:"requestedAt"`
	// EstimatedDurationSeconds orders this entry under PriorityWithDuration
	// (shortest first within a band); nil orders it last in its band, whatever
	// durationFallback says. No FleetAction supplies it at this version, so it is nil.
	// +optional
	// +kubebuilder:validation:Minimum=0
	EstimatedDurationSeconds *int32 `json:"estimatedDurationSeconds,omitempty"`
}

// SharedResourceQueue is one shared resource's hold/queue state (RFC-0001 #tde-shared-resource-queue-management). One
// entry per spec.sharedResources[]. TDE-owned.
type SharedResourceQueue struct {
	// ResourceName matches a spec.sharedResources[].name.
	ResourceName string `json:"resourceName"`
	// CurrentHolders are the robots currently holding the resource, up to the
	// resource's capacity (TDE-5). A capacity-1 resource degenerates to one holder.
	// +optional
	// +listType=atomic
	CurrentHolders []ResourceHolder `json:"currentHolders,omitempty"`
	// WaitQueue is the ordered list of pending requests.
	// +optional
	// +listType=atomic
	WaitQueue []WaitQueueEntry `json:"waitQueue,omitempty"`
}

// FleetZone is the Schema for the fleetzones API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fz
// +kubebuilder:printcolumn:name="Robots",type=integer,JSONPath=".status.robotCount"
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=".spec.maxConcurrentRobots"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type FleetZone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetZoneSpec   `json:"spec,omitempty"`
	Status FleetZoneStatus `json:"status,omitempty"`
}

// FleetZoneList contains a list of FleetZone.
// +kubebuilder:object:root=true
type FleetZoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetZone `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FleetZone{}, &FleetZoneList{})
}

// PropagatesToChildren reports whether an estop on this zone reaches descendant zones.
// nil resolves to TRUE, matching the schema default and RFC-0001's stated default for
// downward estop propagation.
func (p ZoneEstopPolicy) PropagatesToChildren() bool {
	return p.PropagateToChildren == nil || *p.PropagateToChildren
}
