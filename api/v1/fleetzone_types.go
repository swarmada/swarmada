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
type FleetZoneSpec struct {
	// DisplayName is a human-readable label for this zone (e.g. "Receiving Dock A").
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// ParentZone references a parent FleetZone for hierarchical zones.
	// Leave empty for top-level zones. The FleetZone admission webhook rejects a
	// non-existent parent and any cycle A→B→A (§9.3.1).
	// +optional
	ParentZone string `json:"parentZone,omitempty"`

	// Waypoints is the list of named locations robots can navigate to within
	// this zone.  Each waypoint name must be unique within the zone.
	// +optional
	// +listType=map
	// +listMapKey=name
	Waypoints []Waypoint `json:"waypoints,omitempty"`

	// EdgeNode, when set, declares a Zone Controller edge node for this zone
	// (§9.2.10, §9.6.2.5). The API Server advertises its address in
	// RegisterAck.edge_endpoints to every adapter whose robots' zone chain
	// includes this zone; those adapters MUST open the EdgeStream.
	// +optional
	EdgeNode *EdgeNodeConfig `json:"edgeNode,omitempty"`

	// PhysicalBounds is the spatial extent of this zone. The Zone Controller uses
	// the polygon for point-in-polygon containment when deriving a robot's current
	// zone from position telemetry (§9.3.4).
	// +optional
	PhysicalBounds *PhysicalBounds `json:"physicalBounds,omitempty"`

	// MaxConcurrentRobots caps how many robots may occupy this zone at once. The
	// TDE enforces it; a robot attempting to enter a full zone is held at the
	// boundary (§9.4). 0 = no limit (default; typical for root zones that
	// aggregate children).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MaxConcurrentRobots int32 `json:"maxConcurrentRobots,omitempty"`

	// SharedResources are physical resources (lifts, corridors, docks) that robots
	// in this zone AND all descendant zones may reserve before use (§9.4).
	// Resources declared on a parent are visible to all child zones.
	// +optional
	SharedResources []SharedResource `json:"sharedResources,omitempty"`

	// EstopPolicy controls how an emergency stop on this zone propagates through
	// the zone tree (§9.6.2.5). Absent = defaults (propagate to children, not to
	// parent).
	// +optional
	EstopPolicy *ZoneEstopPolicy `json:"estopPolicy,omitempty"`

	// ProvisioningPolicy overrides SwarmadaConfig.spec.provisioning for robots in
	// this zone. Absent = inherit the namespace default (§9.1.5.1).
	// +optional
	ProvisioningPolicy *ZoneProvisioningPolicy `json:"provisioningPolicy,omitempty"`
}

// Point is a 2-D coordinate in the site coordinate frame (metres from the site
// origin, per SwarmadaConfig.spec.coordinateSystem). 0.0 is a valid coordinate.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// PhysicalBounds defines a zone's floor and boundary polygon (§9.1.5.1).
type PhysicalBounds struct {
	// Floor is the integer floor number (0 = ground), interpreted relative to
	// SwarmadaConfig.spec.coordinateSystem.groundFloor.
	// +kubebuilder:validation:Minimum=-10
	// +kubebuilder:validation:Maximum=200
	Floor int32 `json:"floor"`

	// Polygon is the closed zone boundary (the last vertex connects back to the
	// first). At least 3 vertices are required; a self-intersecting polygon is
	// rejected by the FleetZone admission webhook (§9.3.1).
	// +kubebuilder:validation:MinItems=3
	Polygon []Point `json:"polygon"`

	// MinAltitude and MaxAltitude bound the zone's vertical extent in metres for
	// geodetic (aerial) namespaces only — the drone altitude floor/ceiling.
	// Unset for ground (Local) zones.
	// +optional
	MinAltitude *float64 `json:"minAltitude,omitempty"`
	// +optional
	MaxAltitude *float64 `json:"maxAltitude,omitempty"`
}

// EdgeNodeConfig points at a facility-LAN Zone Controller edge node (§9.2.10).
type EdgeNodeConfig struct {
	// Address is the host:port the adapter dials on the facility LAN.
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
	// TLSSecretRef names the Secret holding the edge node's server-certificate
	// material, issued from the namespace CA tree (§9.5.2).
	// +optional
	TLSSecretRef string `json:"tlsSecretRef,omitempty"`
}

// SharedResourceType classifies a shared resource (§9.1.5.1).
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

// ReservationPolicy orders reservation grants for a shared resource (§9.1.5.1).
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

// DurationFallback governs PriorityWithDuration ordering when a request omits
// estimatedDurationSeconds (§9.1.5.1).
// +kubebuilder:validation:Enum=FIFO;Deprioritize
type DurationFallback string

// DurationFallback values.
const (
	// DurationFallbackFIFO treats a request without a duration as last in its band.
	DurationFallbackFIFO DurationFallback = "FIFO"
	// DurationFallbackDeprioritize treats a missing duration as MaxInt32 (goes last).
	DurationFallbackDeprioritize DurationFallback = "Deprioritize"
)

// SharedResource declares a reservable physical resource (§9.1.5.1).
type SharedResource struct {
	// Name is unique within the zone tree; robots reserve by this name.
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
	// DurationFallback applies only under PriorityWithDuration.
	// +kubebuilder:default=FIFO
	DurationFallback DurationFallback `json:"durationFallback,omitempty"`
}

// ZoneEstopPolicy controls estop propagation through the zone tree (§9.6.2.5).
type ZoneEstopPolicy struct {
	// PropagateToChildren: an estop on this zone also triggers estop on all
	// descendant zones. Recommended true.
	// +kubebuilder:default=true
	PropagateToChildren bool `json:"propagateToChildren,omitempty"`
	// PropagateToParent: an estop on this zone notifies the parent (which does NOT
	// auto-trigger its own estop — it receives a ChildEstopTriggered event).
	// +kubebuilder:default=false
	PropagateToParent bool `json:"propagateToParent,omitempty"`
}

// ZoneProvisioningPolicy overrides SwarmadaConfig.spec.provisioning for a zone
// (§9.1.5.1). Reuses the namespace-level ProvisioningMode enum.
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
	// namespaces only); frame-exclusive with the zone Floor. Unset for Local.
	// +optional
	Altitude *float64 `json:"altitude,omitempty"`
}

// ZoneEstopStatus is a FleetZone's aggregate emergency-stop status (§9.1.5).
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

// FleetZoneStatus describes the observed state of a FleetZone.
type FleetZoneStatus struct {
	// IsLeaf is true when this zone has no child zones. Computed by the Zone
	// Controller and recomputed whenever a child FleetZone is created or deleted
	// (§9.3.4). A zone not yet reconciled reports false (safe default: not treated
	// as a leaf until confirmed — the Robot admission gate requires a leaf zone).
	// +optional
	IsLeaf bool `json:"isLeaf,omitempty"`

	// ChildZones is the list of direct child zone names, written by the Zone
	// Controller (§9.3.4).
	// +optional
	// +listType=set
	ChildZones []string `json:"childZones,omitempty"`

	// RobotCount is the number of robots whose currentZone is this zone or any
	// descendant zone. Written by the Zone Controller (§9.3.4).
	// +optional
	// +kubebuilder:validation:Minimum=0
	RobotCount int32 `json:"robotCount,omitempty"`

	// EstopStatus is the zone's aggregate emergency-stop status, written by the
	// zone-estop controller (§9.6.2.5). Empty is treated as Clear.
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
	// is non-empty. Empty when every expected feed is live (§9.2.10).
	// +optional
	// +listType=set
	EdgeFeedUnavailable []string `json:"edgeFeedUnavailable,omitempty"`

	// CurrentConcurrentRobots is the number of Occupied reservations (robots
	// physically confirmed in this zone). Written by the TDE (§9.4). MAY briefly
	// exceed spec.maxConcurrentRobots by 1 during Critical preemption (TDE-3).
	// +optional
	CurrentConcurrentRobots int32 `json:"currentConcurrentRobots,omitempty"`

	// Reservations is the per-action zone-capacity reservation state, written
	// exclusively by the Traffic Deconfliction Engine (§9.4). Operator edits are
	// rejected by the FleetZone admission webhook (TDE-4).
	// +optional
	// +listType=atomic
	Reservations []ZoneReservation `json:"reservations,omitempty"`

	// SharedResourceQueues is the per-resource reservation/queue state, one entry
	// per spec.sharedResources[]. Written exclusively by the TDE (§9.4).
	// +optional
	// +listType=atomic
	SharedResourceQueues []SharedResourceQueue `json:"sharedResourceQueues,omitempty"`
	// ActiveActions is the number of FleetActions currently running in this zone.
	// +optional
	ActiveActions int32 `json:"activeActions,omitempty"`

	// Conditions is the standard conditions list, written by the Zone Controller:
	// ZoneReady (serviceability) and CapacityAvailable (room for another robot).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ReservationState is the lifecycle state of a zone-capacity reservation (§9.4.2).
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

// ZoneReservation is one action's zone-capacity reservation (§9.4.2). TDE-owned.
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

// ResourceHolder is the current holder of a shared resource (§9.4.5).
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
// reservationPolicy (§9.4.5).
type WaitQueueEntry struct {
	RobotID  string         `json:"robotID"`
	ActionID string         `json:"actionID"`
	Priority ActionPriority `json:"priority"`
	// RequestedAt is the arrival time (FIFO tiebreak within band).
	RequestedAt metav1.Time `json:"requestedAt"`
	// EstimatedDurationSeconds drives PriorityWithDuration (SJF); nil → fallback.
	// +optional
	// +kubebuilder:validation:Minimum=0
	EstimatedDurationSeconds *int32 `json:"estimatedDurationSeconds,omitempty"`
}

// SharedResourceQueue is one shared resource's hold/queue state (§9.4.5). One
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
// +kubebuilder:printcolumn:name="ActiveActions",type=integer,JSONPath=".status.activeActions"
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
