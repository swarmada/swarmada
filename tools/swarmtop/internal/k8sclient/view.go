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

import "time"

// The view types are the leaf data model the UI renders. They are intentionally
// free of any k8s.io / client-go / controller-runtime imports: the mapping from
// the Swarmada CRDs happens in map.go and stops here, so internal/ui and
// internal/format never depend on the API machinery. This is also the seam that
// makes a later switch from the snapshot Store (option C) to an event channel
// (option B) cheap — only store.go changes; these types and the mappers do not.

// Fleet is a single consistent snapshot of everything swarmtop watches. The
// Store hands out an independent copy per Snapshot() call, so the UI can hold
// and range over it without locking.
type Fleet struct {
	// Robots, Actions, Probes, Adapters and Zones are all sorted by Name for
	// stable rendering.
	Robots   []RobotView
	Actions    []FleetActionView
	Probes   []RobotProbeView
	Adapters []AdapterView
	Zones    []ZoneView

	// EventsByRobot holds the recent core/v1 Events whose involvedObject is a
	// Robot, keyed by robot name and sorted newest-first. Same trust boundary
	// as `kubectl describe` — a pure read.
	EventsByRobot map[string][]EventView

	// SnapshotAt is when the Store materialized this copy (wall clock), used to
	// render relative "age" columns consistently against a single reference.
	SnapshotAt time.Time
}

// RobotView is the reduced, display-ready projection of one Robot. Every field
// is derived from status the controllers already populate; nothing here is
// live-pose or telemetry-cadence data (RA-1) — battery, position, and
// connectivity are the coarse, throttled status projections, shown with an
// explicit staleness age rather than as smooth live values.
type RobotView struct {
	Name string

	// Phase is status.phase (Idle, InProgress, Charging, Offline, ...).
	Phase string

	// Estop is status.estopState; empty string is normalized to "Normal".
	Estop string

	// BatteryPercent is status.batteryPercent (0–100); nil when unreported.
	BatteryPercent *int32

	// SpecZone is spec.zone (assigned leaf zone). CurrentZone is
	// status.currentZone (derived from telemetry); ZoneDrift is true when they
	// differ (status.specZoneMatchesCurrent == false).
	SpecZone    string
	CurrentZone string
	ZoneDrift   bool

	// Caps summarizes status.capabilities[] for the list view; Capabilities is
	// the full per-entry breakdown for the detail/split view.
	Caps         CapSummary
	Capabilities []CapabilityView

	// Hardware is the per-component health breakdown (detail/split view).
	Hardware []HardwareView

	// HasPosition distinguishes an unreported pose from a genuine origin pose.
	HasPosition bool
	Position    PositionView

	// AssignedAction is status.assignedAction ("" when none).
	AssignedAction string

	// --- extended detail fields (rendered in the scrollable detail view) ---

	// Conditions is status.conditions[] (standard controller conditions).
	Conditions []ConditionView

	// Health is the aggregate health summary (status.health).
	HealthStatus  string
	HealthMessage string

	// LatencyMs is status.connectivity.latencyMs (last gRPC ping); nil when
	// unreported.
	LatencyMs *int32

	// FirmwareVersion / PreviousFirmwareVersion are status firmware fields.
	FirmwareVersion         string
	PreviousFirmwareVersion string

	// InstalledModels is the robot agent's per-model runtime report
	// (status.installedModels[]). ModelGrantedCaps is
	// status.modelGrantedCapabilities[].
	InstalledModels  []InstalledModelView
	ModelGrantedCaps []ModelGrantedView

	// AdapterName is spec.adapterRef target; LastTelemetry is
	// status.connectivity.lastSeenAt. TelemetryUnknown is true when the robot
	// has never reported connectivity.
	AdapterName      string
	LastTelemetry    time.Time
	TelemetryUnknown bool
}

// CapSummary is the at-a-glance capability roll-up shown in the list. When all are
// active the list shows just the count (e.g. "3"); otherwise "2/3 cam_front" (Active
// count / total, plus the name of the first non-Active capability as the headline
// problem).
type CapSummary struct {
	Active int
	Total  int

	// FirstProblem is the name of the first capability whose Status is not
	// Active, or "" when all are Active. FirstProblemState is that capability's
	// status (Degraded, Failed, ...).
	FirstProblem      string
	FirstProblemState string
}

// CapabilityView is one row of the capability breakdown.
type CapabilityView struct {
	Name   string
	Status string // Active, Degraded, Paused, Inactive, Unavailable, Failed
	Paused bool
	Reason string
}

// HardwareView is one row of the hardware breakdown.
type HardwareView struct {
	Name   string
	Status string // Healthy, Degraded, Failed
	Reason string
}

// ConditionView is one status.conditions[] entry.
type ConditionView struct {
	Type           string
	Status         string // True, False, Unknown
	Reason         string
	Message        string
	LastTransition time.Time
}

// InstalledModelView is one status.installedModels[] runtime entry.
type InstalledModelView struct {
	Name           string
	Status         string
	RunningVersion string
	FailureReason  string
}

// ModelGrantedView is one status.modelGrantedCapabilities[] entry.
type ModelGrantedView struct {
	ModelName    string
	GrantedBy    string
	Capabilities []string
}

// EventView is a reduced core/v1 Event involving a Robot.
type EventView struct {
	Time    time.Time
	Type    string // Normal, Warning
	Reason  string
	Message string
	Count   int32
}

// PositionView is the coarse last-known pose (status.position). Floor is nil
// when the site is single-floor / unknown.
type PositionView struct {
	X     float64
	Y     float64
	Floor *int32
}

// FleetActionView is the reduced projection of one FleetAction (Phase 2 views, but
// the Store already watches the type so the data is populated now).
type FleetActionView struct {
	Name          string
	Phase         string
	AssignedRobot string
	Priority      string
	ProgressPct   int32
	RetryCount    int32
	Deadline      *time.Time
	Message       string
}

// RobotProbeView is the reduced projection of one RobotProbe — active health
// verification (hardware/capability/model) layered on top of passive telemetry.
type RobotProbeView struct {
	Name            string
	ProbeType       string // hardware, capability, model
	TargetComponent string
	LastResult      string // Healthy, Degraded, Failed, Pending, Unknown
	LastProbeTime   time.Time
	RobotCount      int // robots covered by the last cycle
	FailingCount    int // of those, how many were not Healthy
}

// ZoneView is the reduced projection of one FleetZone.
//
// Deliberately carries the SAFETY state (estop, degraded edge feeds) alongside the structural
// state (hierarchy, capacity). A zone screen that showed only the tree would omit the two things
// an operator most needs to notice: that a zone is stopped, and that its boundary-breach trigger
// is not receiving position frames for some robots — which is a safety guarantee silently not
// being met.
type ZoneView struct {
	Name        string
	DisplayName string
	ParentZone  string
	IsLeaf      bool
	ChildZones  []string

	RobotCount          int32
	CurrentConcurrent   int32
	MaxConcurrentRobots int32

	EstopStatus string
	LastEstopAt time.Time
	// LastEstopUnknown distinguishes "never stopped" from a zero timestamp, so the UI never
	// renders the epoch as if it were a real estop.
	LastEstopUnknown bool

	EdgeFeedUnavailable []string
	HasEdgeNode         bool
	Waypoints           int
}

// AdapterView is the reduced projection of one FleetAdapter (Phase 2 view).
type AdapterView struct {
	Name             string
	Phase            string
	Conformance      string
	ProtocolVersion  string
	ConnectedRobots  int32
	LastHeartbeat    time.Time
	HeartbeatUnknown bool
	Message          string
}
