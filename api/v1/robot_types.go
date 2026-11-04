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

// RobotIDAnnotation carries the stable robot_id the Fleet Adapter announces, mapping a
// telemetry frame's robot_id to its Robot. The id is typically taken from the robot's
// serial number or MAC, which are unique as a fact about the hardware; the control plane
// does not enforce robot_id uniqueness across namespaces and does not rely on it
// (RFC-0001 #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3). Within one
// namespace it identifies at most one Robot
// (RFC-0001 #control-plane-swarmada-api-server: robot_id ↔ swarmada.io/robot-id). Set on the Robot at admission
// (auto-admit stamps it; the RobotDefaulter backfills operator-created Robots) so the
// telemetry status projection and zone routing can resolve robot_id → Robot.
const RobotIDAnnotation = "swarmada.io/robot-id"

// EstopResumeAnnotation is an operator's request to release a robot from the
// post-clear estop window (Robot.status.estopState Resuming), carrying the operator's
// reason. It is the only way a robot leaves Resuming: nothing releases it
// automatically (RFC-0001 #safety-estop-state-machine). Where the admission webhooks
// are installed, a robots/status write of the estop state from any identity other than
// the control plane's is refused; without them it is not. Writing it is authorized as
// the estop-clear custom verb on the Robot; the robot-estop controller acts on a NEW
// value once and records it in EstopResumeProcessedAnnotation.
const EstopResumeAnnotation = "swarmada.io/estop-resume"

// EstopResumeProcessedAnnotation records the EstopResumeAnnotation value the control
// plane has already acted on, so a request is evaluated exactly once. A request left
// on the Robot after it was refused, or written while the robot was not Resuming,
// therefore never releases a later window. It is written after the request's outcome:
// a reader that finds a request here reads the estopState that answered it.
const EstopResumeProcessedAnnotation = "swarmada.io/estop-resume-processed"

// EstopResumeClaimedAnnotation records a resume request the control plane has claimed
// and not yet answered. The request is claimed before it is evaluated, so a refused
// request is never evaluated a second time; once the answer (the estopState, an Event
// and an audit entry) is recorded, the annotation is removed in the same write that
// records the request in EstopResumeProcessedAnnotation. One that stands when the
// control plane next reads the Robot was closed without an answer, which the control
// plane reports as an EstopResumeUnanswered Event: re-issue the request.
const EstopResumeClaimedAnnotation = "swarmada.io/estop-resume-claimed"

// AutoAdmittedAnnotation marks a Robot created by auto-admit (ADR-0014). Only Robots
// carrying it are eligible for opt-in offline auto-removal (ADR-0030); operator-created
// robots never are, so a warehouse fleet is unaffected by the removal policy.
const AutoAdmittedAnnotation = "swarmada.io/auto-admitted"

// ── Enumerations ──────────────────────────────────────────────────────────────

// RobotPhase is the high-level lifecycle state of a Robot. The value set mirrors
// the fleet_adapter.v1 RobotPhase wire enum (RFC-0001 #robot), the authoritative
// schema per RFC-0001 #fleet-adapter-protocol.
// +kubebuilder:validation:Enum=Discovered;Idle;Assigned;InProgress;Charging;Error;Offline;Maintenance
type RobotPhase string

// RobotPhase values.
const (
	// RobotPhaseDiscovered: registered, awaiting capability verification.
	RobotPhaseDiscovered RobotPhase = "Discovered"
	RobotPhaseIdle       RobotPhase = "Idle"
	// RobotPhaseAssigned: a action is assigned but the robot is not yet executing.
	RobotPhaseAssigned RobotPhase = "Assigned"
	// RobotPhaseInProgress: actively executing a action.
	RobotPhaseInProgress RobotPhase = "InProgress"
	RobotPhaseCharging   RobotPhase = "Charging"
	// RobotPhaseError: non-recoverable fault; requires operator attention.
	RobotPhaseError   RobotPhase = "Error"
	RobotPhaseOffline RobotPhase = "Offline"
	// RobotPhaseMaintenance: inside a ZoneMaintenance window.
	RobotPhaseMaintenance RobotPhase = "Maintenance"
)

// RobotEstopState is a robot's emergency-stop status, independent of Phase
// (RFC-0001 #safety-estop-state-machine). A robot may be in any Phase and Stopped simultaneously.
// +kubebuilder:validation:Enum=Normal;Stopping;Stopped;Resuming;Failed
type RobotEstopState string

// RobotEstopState values (RFC-0001 #safety-estop-state-machine).
const (
	// RobotEstopNormal: no active estop.
	RobotEstopNormal RobotEstopState = "Normal"
	// RobotEstopStopping: the Fleet Adapter has issued the stop to hardware.
	RobotEstopStopping RobotEstopState = "Stopping"
	// RobotEstopStopped: CONFIRMED at rest; awaiting explicit operator clear.
	RobotEstopStopped RobotEstopState = "Stopped"
	// RobotEstopResuming: the last stop holding the robot was cleared; awaiting the operator's
	// release, capabilities restoring. Nothing is sent to the adapter.
	// Left only when an operator releases the robot (EstopResumeAnnotation); enforced at
	// admission where the webhooks are installed.
	RobotEstopResuming RobotEstopState = "Resuming"
	// RobotEstopFailed: the control plane could not obtain a CONFIRMED stop within
	// the SLA/watchdog (dropped estop, silence, or STOPPING with no STOPPED). It is
	// NOT "stopped" — it is an escalation signal (RFC-0001 #safety-estop-state-machine): a stop was commanded
	// but never confirmed, so the robot must be escalated (edge/manual), never
	// treated as at-rest.
	RobotEstopFailed RobotEstopState = "Failed"
)

// EstopBaseline is what the estop dispatcher recorded about a robot when its current
// emergency stop was taken (RFC-0001 #safety-estop-state-machine). The resume
// confirmation reads it: a robot is released from Resuming only when every capability
// that was Active at the stop is Active again.
type EstopBaseline struct {
	// ActiveCapabilities names the capabilities that were Active when the stop was
	// taken, on the stored status.capabilities or on the capabilities derived from the
	// reported hardware at that moment. A capability that was not Active then (never
	// reported, Inactive, or Paused by a maintenance window) is not listed, and does
	// not gate the resume.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	ActiveCapabilities []string `json:"activeCapabilities,omitempty"`

	// RecordedAt is when the stop that took this record was written.
	RecordedAt metav1.Time `json:"recordedAt"`
}

// EstopHoldKind names the carrier of a zone- or namespace-scope stop recorded on a robot.
// +kubebuilder:validation:Enum=FleetZone;SwarmadaConfig
type EstopHoldKind string

// EstopHoldKind values.
const (
	// EstopHoldFleetZone: the stop of a FleetZone, the robot's own zone or an ancestor of it.
	EstopHoldFleetZone EstopHoldKind = "FleetZone"
	// EstopHoldSwarmadaConfig: the namespace stop, carried by the namespace's SwarmadaConfig.
	EstopHoldSwarmadaConfig EstopHoldKind = "SwarmadaConfig"
)

// EstopHold records that a zone's or the namespace's emergency stop holds this robot
// (RFC-0001 #safety-estop-scopes). The control plane writes it when that stop is issued to
// the robot. It removes it on the clear of a stop on a carrier of the same kind and name: the
// stop that wrote it, or the stop of a carrier deleted and created again under that name. An
// entry whose carrier no longer exists, or exists under another UID, is also removed by a
// robot-scope stop and clear on the robot. The hold does not depend on where the zone tree,
// the robot's zone or its reported position stand afterwards.
type EstopHold struct {
	// Kind is the carrier's kind.
	Kind EstopHoldKind `json:"kind"`
	// Name is the carrier's name in the robot's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// UID is the carrier's UID when the stop was recorded. A carrier recreated under the same
	// name has another UID.
	// +optional
	UID string `json:"uid,omitempty"`
	// Since is when the hold was recorded.
	Since metav1.Time `json:"since"`
}

// StopScope is the scope of a recorded stop (RFC-0001 #safety-robot-stop-table, the term
// recorded stop): the robot's own stop, a zone's, the namespace's, a stop an adapter reported
// that the control plane did not send, or an end-state stop.
// +kubebuilder:validation:Enum=Robot;Zone;Namespace;Reported;EndState
type StopScope string

// StopScope values.
const (
	StopScopeRobot     StopScope = "Robot"
	StopScopeZone      StopScope = "Zone"
	StopScopeNamespace StopScope = "Namespace"
	StopScopeReported  StopScope = "Reported"
	StopScopeEndState  StopScope = "EndState"
)

// StopCarrierKind is the kind of the object that carries a stop: the robot itself, a FleetZone
// or the namespace's SwarmadaConfig. It is a separate type from EstopHoldKind, which names only a
// zone or namespace carrier: estopHeldBy never names the robot.
// +kubebuilder:validation:Enum=Robot;FleetZone;SwarmadaConfig
type StopCarrierKind string

// StopCarrierKind values. FleetZone and SwarmadaConfig equal the EstopHoldKind values.
const (
	StopCarrierRobot          StopCarrierKind = "Robot"
	StopCarrierFleetZone      StopCarrierKind = "FleetZone"
	StopCarrierSwarmadaConfig StopCarrierKind = "SwarmadaConfig"
)

// StopEvent is the abnormal event an end-state stop records.
// +kubebuilder:validation:Enum=Register;LocalStop;ReportedStop
type StopEvent string

// StopEvent values.
const (
	StopEventRegister     StopEvent = "Register"
	StopEventLocalStop    StopEvent = "LocalStop"
	StopEventReportedStop StopEvent = "ReportedStop"
)

// StopCarrier names the object whose stop annotation issued a stop.
type StopCarrier struct {
	// Kind is the carrier's kind.
	Kind StopCarrierKind `json:"kind"`
	// Name is the carrier's name in the robot's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// UID is the carrier's UID when the stop was issued.
	// +optional
	UID string `json:"uid,omitempty"`
}

// RobotStop is one stop recorded on the robot (RFC-0001 #safety-robot-stop-table, the term
// recorded stop): its stop ID, scope and carrier, when it was sent, acknowledged and cleared.
// +kubebuilder:validation:XValidation:rule="self.scope in ['Reported', 'EndState'] ? !has(self.carrier) : has(self.carrier)",message="a Reported or EndState stop has no carrier; every other stop names its carrier"
// +kubebuilder:validation:XValidation:rule="!has(self.event) || self.scope == 'EndState'",message="event is set only on an EndState stop"
type RobotStop struct {
	// ID is the stop ID: the estop_id every copy and re-send of the stop carries. A stop the
	// control plane issues has a random ID of at least 128 bits; a Reported entry keeps the ID
	// the adapter reported, which an older control plane may have minted in another form.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=320
	ID string `json:"id"`
	// Scope is the stop's scope.
	Scope StopScope `json:"scope"`
	// Carrier is the object whose stop this is; none for a Reported or EndState stop.
	// +optional
	Carrier *StopCarrier `json:"carrier,omitempty"`
	// SentAt is when the stop was sent; for a Reported stop, when the report was received.
	SentAt metav1.Time `json:"sentAt"`
	// AcknowledgedAt is when the first STOPPING answer for the stop arrived (row S2).
	// +optional
	AcknowledgedAt *metav1.Time `json:"acknowledgedAt,omitempty"`
	// ClearedAt is when the clear that ended the stop was written (row S5).
	// +optional
	ClearedAt *metav1.Time `json:"clearedAt,omitempty"`
	// Event is the abnormal event an EndState stop records. Written by no controller at v0.4.
	// +optional
	Event StopEvent `json:"event,omitempty"`
	// LocalSet is the set ID of the local latch an EndState stop records. Written by no
	// controller at v0.4.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	LocalSet string `json:"localSet,omitempty"`
	// AttestedAt is when an operator attested the check at the robot that ends an EndState
	// stop. Written by no controller at v0.4.
	// +optional
	AttestedAt *metav1.Time `json:"attestedAt,omitempty"`
	// AttestedBy is the identity that attested it. Written by no controller at v0.4.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	AttestedBy string `json:"attestedBy,omitempty"`
}

// HardwareStatus describes the health of a single hardware component. Mirrors the
// fleet_adapter.v1 HardwareStatus wire enum (HEALTHY;DEGRADED;FAILED;DISABLED).
// +kubebuilder:validation:Enum=Healthy;Degraded;Failed;Disabled
type HardwareStatus string

// HardwareStatus values.
const (
	HardwareHealthy  HardwareStatus = "Healthy"
	HardwareDegraded HardwareStatus = "Degraded"
	HardwareFailed   HardwareStatus = "Failed"
	// HardwareDisabled: the component is intentionally not in service (an operator/edge
	// toggle turned it off). Distinct from Failed (fault) — benign, reversible, and NOT
	// critical, so it does not bypass the status-write throttle (ADR-0031).
	HardwareDisabled HardwareStatus = "Disabled"
)

// DayOfWeek names a day for a MaintenanceWindow.
// +kubebuilder:validation:Enum=Monday;Tuesday;Wednesday;Thursday;Friday;Saturday;Sunday
type DayOfWeek string

// DayOfWeek values.
const (
	Monday    DayOfWeek = "Monday"
	Tuesday   DayOfWeek = "Tuesday"
	Wednesday DayOfWeek = "Wednesday"
	Thursday  DayOfWeek = "Thursday"
	Friday    DayOfWeek = "Friday"
	Saturday  DayOfWeek = "Saturday"
	Sunday    DayOfWeek = "Sunday"
)

// ── Spec ──────────────────────────────────────────────────────────────────────

// AdapterRef binds the Fleet Adapter that manages this robot (RFC-0001 §Robot
// Schema). Replaces the earlier flat fleetAdapterEndpoint placeholder — the
// adapter's transport identity now comes from the Fleet Adapter's own mTLS
// client certificate (Security Model), not an operator-declared address.
type AdapterRef struct {
	// Name must match an installed FleetAdapter resource in this namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the adapter BUILD version. It is specified to be compared with
	// the adapter's reported adapter_version at reconnect and checked before a
	// firmware push; at v0.4 neither happens, no rollout controller reads it, and
	// admission does not compare it. It is filled from the RobotClass
	// baseAdapter when unset and is informational.
	// +kubebuilder:validation:Pattern=`^\d+\.\d+\.\d+$`
	Version string `json:"version"`
}

// RobotChargingConfig is this robot's charging policy. Unlike
// ClassChargingConfig (RobotClass), it carries DockName — a per-unit dock
// assignment that is never inherited from a class.
// +kubebuilder:validation:XValidation:rule="!has(self.minBatteryPctToCharge) || !has(self.targetBatteryPct) || self.targetBatteryPct > self.minBatteryPctToCharge",message="targetBatteryPct must be greater than minBatteryPctToCharge"
type RobotChargingConfig struct {
	// DockName names a ChargingDock shared resource declared in this robot's
	// FleetZone or any ancestor zone's sharedResources[]. Cross-resource, so
	// validated by the admission webhook, not by CEL.
	// +optional
	DockName string `json:"dockName,omitempty"`

	// MinBatteryPctToCharge is the battery level below which the robot is
	// specified to initiate autonomous docking. Not implemented at v0.4: no
	// controller acts on it and no Fleet Adapter protocol message carries it.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MinBatteryPctToCharge *int32 `json:"minBatteryPctToCharge,omitempty"`

	// TargetBatteryPct is the level at which the robot is specified to leave
	// the dock; it MUST be greater than MinBatteryPctToCharge (CEL, enforced).
	// Not acted on at v0.4, like MinBatteryPctToCharge.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	TargetBatteryPct *int32 `json:"targetBatteryPct,omitempty"`
}

// MaintenanceWindow is a recurring planned-maintenance window, in UTC. At v0.4
// it is read only by the FirmwareRollout and ModelRollout
// safetyConstraints.maintenanceWindowOnly gate, which admits a robot to a
// rollout batch only inside its window. ZoneMaintenance does not read it: a
// ZoneMaintenance pauses robots in scope when it activates, whatever the window.
type MaintenanceWindow struct {
	// DayOfWeek the window recurs on.
	DayOfWeek DayOfWeek `json:"dayOfWeek"`

	// StartHour is the hour (24h UTC) at which the window opens.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=23
	StartHour int32 `json:"startHour"`

	// DurationMinutes is how long the window stays open.
	// +kubebuilder:validation:Minimum=15
	// +kubebuilder:validation:Maximum=480
	DurationMinutes int32 `json:"durationMinutes"`
}

// RobotSpec defines the desired state of a Robot (RFC-0001 §Robot Schema).
// When RobotClass is set, Hardware / InstalledModels / Capabilities entries
// declared here are merged union-by-name over the class's inherited entries
// (a same-named entry fully overrides the class's). Constraints and Charging
// inherit per field: each sub-field the Robot leaves unset is filled from the
// class's defaultConstraints / defaultChargingConfig, so setting one sub-field
// keeps the class's value for the others. See RobotClassSpec for the merge semantics.
//
// The cross-reference rules below are guarded on BOTH sides. The
// referencing side is optional — a Robot that omits capabilities entirely (the
// common case; capabilities are frequently supplied only via
// RobotClass/ModelRollout) must not fail CEL evaluation with "no such key:
// capabilities". So is the referenced side: hardware and installedModels are
// omitempty too, and an unguarded self.hardware / self.installedModels raises
// "no such key: hardware" — an evaluation ERROR reported against spec as a
// whole, instead of the validation message written three lines below it. The
// object is rejected either way; what the guard buys is that the operator reads
// the reason rather than API-server-internal CEL text.
//
// The absent-referenced-side case gets its own rule and its own message,
// because it is a different mistake from a typo: the first two rules say "you
// declared no hardware / no models at all", the second two say "the name you
// referenced is not among the ones you declared".
//
// These rules live on RobotSpec, not on capabilities[], because a CEL rule
// attached to a nested field cannot see a sibling field — self is scoped to
// what the rule is attached to. That is why the failure is reported against
// spec rather than spec.capabilities[i].requiredHardware, and it is not
// fixable here.
// +kubebuilder:validation:XValidation:rule="!has(self.capabilities) || self.capabilities.all(c, !has(c.requiredHardware) || size(c.requiredHardware) == 0 || has(self.hardware))",message="spec.capabilities[].requiredHardware is set but spec.hardware declares no components; declare the component in spec.hardware"
// +kubebuilder:validation:XValidation:rule="!has(self.capabilities) || self.capabilities.all(c, !has(c.providingModel) || has(self.installedModels))",message="spec.capabilities[].providingModel is set but spec.installedModels declares no models; declare the model in spec.installedModels"
// +kubebuilder:validation:XValidation:rule="!has(self.capabilities) || self.capabilities.all(c, !has(c.requiredHardware) || !has(self.hardware) || c.requiredHardware.all(h, self.hardware.exists(hw, hw.name == h)))",message="a capability's requiredHardware must reference a component declared in spec.hardware"
// +kubebuilder:validation:XValidation:rule="!has(self.capabilities) || self.capabilities.all(c, !has(c.providingModel) || !has(self.installedModels) || self.installedModels.exists(m, m.name == c.providingModel))",message="a capability's providingModel must reference a model declared in spec.installedModels"
type RobotSpec struct {
	// Manufacturer is the robot vendor (e.g. "acme-robotics", "borealis").
	// +kubebuilder:validation:MinLength=1
	Manufacturer string `json:"manufacturer"`

	// Model is the hardware model identifier (e.g. "acme-picker", "borealis-250").
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// RobotClass is the name of a RobotClass in this namespace whose hardware
	// inventory, default models, base capabilities, and operational defaults
	// are merged into this Robot's spec at admission. When set, the admission
	// gate additionally requires a Connected, conformance-passed FleetAdapter
	// that serves this class. Optional: a Robot may instead describe its
	// hardware and capabilities inline.
	// +optional
	RobotClass string `json:"robotClass,omitempty"`

	// Adapter is the Fleet Adapter binding for this robot.
	Adapter AdapterRef `json:"adapter"`

	// Hardware is the physical component inventory declared by the operator —
	// NOT runtime state. Presence here does not imply the component is
	// healthy; health lives in status.hardware[].
	// MaxItems bounds the CEL cost of this spec's cross-field
	// x-kubernetes-validations rules (capabilities[].requiredHardware /
	// providingModel cross-references) — without it the API server's CEL
	// cost estimator assumes an unbounded array and rejects the CRD at
	// apply time ("estimated rule cost exceeds budget"), since those rules
	// are quadratic in hardware/capabilities/installedModels size.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Hardware []HardwareComponent `json:"hardware,omitempty"`

	// InstalledModels declares inference model packages deployed to this
	// robot's onboard compute, overriding any RobotClass defaultModels[] of
	// the same name. Runtime status (Active/Updating/Failed/Inactive) lives
	// in status.installedModels[], written by the control plane: the ModelRollout
	// controller marks a model Updating, and the install-state projection records
	// what the adapter reports — mirroring spec.containers[] /
	// status.containerStatuses[].
	// MaxItems bounds CEL cost — see the Hardware field comment above.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	InstalledModels []ClassModel `json:"installedModels,omitempty"`

	// Capabilities are operator-owned exclusively; no controller writes to
	// this field. Capabilities granted by a ModelRollout are never added
	// here — they are recorded in status.modelGrantedCapabilities[] and
	// folded into the computed status.capabilities[] union by the Capability
	// Controller.
	// MaxItems bounds CEL cost — see the Hardware field comment above.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Capabilities []ClassCapability `json:"capabilities,omitempty"`

	// Constraints are operational limits. Only MinBatteryPctForAction is read
	// on the dispatch path (candidate filter 4). No controller or dispatch filter
	// acts on MaxPayloadKg or MaxSpeedMs, and neither is sent to an adapter: an action's
	// spec.constraints.maxPayloadKg is checked against a capability parameter
	// resolved from spec.hardware[] (candidate filter 5), not against this
	// field.
	// +optional
	Constraints *ClassConstraints `json:"constraints,omitempty"`

	// Charging is this robot's charging dock assignment and thresholds.
	// +optional
	Charging *RobotChargingConfig `json:"charging,omitempty"`

	// Zone is the leaf FleetZone this robot is assigned to operate in. MUST
	// reference a FleetZone with no children; admission rejects a non-leaf
	// reference (cross-resource, enforced by the admission webhook). The Zone
	// Controller derives status.currentZone from telemetry and may differ
	// from this field if the robot has physically moved.
	// +kubebuilder:validation:MinLength=1
	Zone string `json:"zone"`

	// TelemetryIntervalSeconds is the telemetry reporting interval — the MAXIMUM
	// interval between TelemetryPayload frames — sent to the adapter as
	// RegisterAck.telemetry_interval_seconds. It sets the robot-silence threshold
	// for Offline detection: a robot that has streamed and then sends nothing for
	// the longer of three intervals and
	// SwarmadaConfig.spec.health.connectivityOfflineThresholdSeconds goes Offline,
	// even while its adapter is live. Adapter presence is judged separately, on
	// status.connectivity.lastSeenAt against that same namespace threshold.
	// Lower values increase control-plane load; 5s is the recommended production
	// default. Use 1s only for development.
	//
	// No schema default: unset here it is inherited from the robot's RobotClass
	// (spec.defaultTelemetry.telemetryIntervalSeconds) by the RobotDefaulter merge —
	// the same inheritance model as adapter/constraints/charging.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	TelemetryIntervalSeconds *int32 `json:"telemetryIntervalSeconds,omitempty"`

	// MotionThresholdMeters is specified as the minimum position change (in the
	// units declared by SwarmadaConfig.spec.coordinateSystem) before the Fleet
	// Adapter sends a new position frame. Not sent: no Fleet Adapter protocol
	// message carries it, so no adapter can apply a per-robot value.
	// Offline detection relies on adapter presence, not position frames, and is
	// unaffected. Unset here it is inherited from the class's defaultTelemetry.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MotionThresholdMeters *float64 `json:"motionThresholdMeters,omitempty"`

	// MaxIdleIntervalSeconds is specified as the maximum time a stationary robot
	// may go without sending a position frame even if MotionThresholdMeters has
	// not been exceeded. Not sent, like MotionThresholdMeters. Unset here
	// it is inherited from the class's defaultTelemetry.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	MaxIdleIntervalSeconds *int32 `json:"maxIdleIntervalSeconds,omitempty"`

	// MaintenanceWindow is this robot's planned recurring maintenance window.
	// +optional
	MaintenanceWindow *MaintenanceWindow `json:"maintenanceWindow,omitempty"`
}

// ── Status enumerations ────────────────────────────────────────────────────────

// CapabilityStatus is the computed runtime status of one capability entry,
// per the RFC-0001 §Robot Capability Derivation Truth Table. Paused and
// Unavailable are override states applied after truth-table evaluation (by
// ZoneMaintenance and an in-progress ModelRollout respectively), not
// truth-table outputs themselves.
// +kubebuilder:validation:Enum=Active;Degraded;Paused;Inactive;Unavailable;Failed
type CapabilityStatus string

// CapabilityStatus values.
const (
	CapabilityStatusActive      CapabilityStatus = "Active"
	CapabilityStatusDegraded    CapabilityStatus = "Degraded"
	CapabilityStatusPaused      CapabilityStatus = "Paused"
	CapabilityStatusInactive    CapabilityStatus = "Inactive"
	CapabilityStatusUnavailable CapabilityStatus = "Unavailable"
	CapabilityStatusFailed      CapabilityStatus = "Failed"
)

// Note: installed-model runtime status reuses the existing ModelStatus type
// (declared in discoveredrobot_types.go — Active/Updating/Failed/Inactive,
// mirroring fleet_adapter.v1 ModelStatus) rather than a second identical enum.

// HealthState is the aggregate health summary for a Robot.
// +kubebuilder:validation:Enum=Healthy;Degraded;Critical
type HealthState string

// HealthState values.
const (
	// HealthStateHealthy: all non-pauseable capabilities Active; no hardware Failed.
	HealthStateHealthy HealthState = "Healthy"
	// HealthStateDegraded: at least one component Degraded but no Failed; robot operational.
	HealthStateDegraded HealthState = "Degraded"
	// HealthStateCritical: at least one component Failed OR a non-pauseable capability lost.
	HealthStateCritical HealthState = "Critical"
)

// ── Status ────────────────────────────────────────────────────────────────────

// HardwareComponentStatus is the observed runtime health of one component
// declared in spec.hardware[]. Written by the Health Monitor from Fleet
// Adapter telemetry.
type HardwareComponentStatus struct {
	// Name matches a spec.hardware[*].name entry.
	Name string `json:"name"`

	// Status is the component's current health.
	Status HardwareStatus `json:"status"`

	// LastHealthyAt is the timestamp of the last Healthy report for this
	// component. At v0.4 written only by the RobotProbe controller; absent for a
	// component no hardware probe has reported on.
	// +optional
	LastHealthyAt *metav1.Time `json:"lastHealthyAt,omitempty"`

	// DegradationReason is a human-readable reason; empty when Status is
	// Healthy. At v0.4 written only by the RobotProbe controller, from the probe
	// response message or, when the adapter gave none, a summary of the failure
	// run. The reason an adapter sends in telemetry is dropped. A non-empty reason
	// on a Degraded or Failed component marks a probe verdict, which telemetry
	// does not lower while that probe still reports on the robot.
	// +optional
	DegradationReason string `json:"degradationReason,omitempty"`

	// DegradedMetrics carries type-specific numeric readings specified to be
	// populated when Status is not Healthy (e.g. a Camera's depthFrameRateFps).
	// Written by nothing at v0.4. Numeric-only
	// so the field stays typed rather than free-form interface{} — see
	// api-principles.md "Opaque and free-form data".
	// +optional
	DegradedMetrics map[string]float64 `json:"degradedMetrics,omitempty"`
}

// ActivationCost classifies whether ActivateStream has a real power/bandwidth
// cost for a sensor stream (OnDemand — the sensor-stream duty cycle exists for
// this case) or the sensor is already powered regardless of activation
// (AlwaysOn, e.g. a mic array that is never switched off). Advisory: it
// changes no admission or scheduling decision.
// +kubebuilder:validation:Enum=AlwaysOn;OnDemand
type ActivationCost string

// ActivationCost values.
const (
	ActivationCostAlwaysOn ActivationCost = "AlwaysOn"
	ActivationCostOnDemand ActivationCost = "OnDemand"
)

// SensorStreamEntry is the read-only projection of one adapter-declared
// CapabilitiesSnapshot.sensor_streams entry, written
// only by CapabilitiesIngestor — never by a task or an operator. A task
// author discovers what a robot can see here before writing a task, and the
// scheduler's SensorStreamNotOffered gate reads it to refuse
// scheduling before any robot is committed, mirroring how
// status.supportedActions (FleetAdapter) and status.hardware already work as
// discovery/health projections from the same CapabilitiesSnapshot channel.
type SensorStreamEntry struct {
	// StreamID is the adapter-defined, stable identifier (e.g. "front-rgb").
	StreamID string `json:"streamId"`

	// MediaType is the IANA media type of the stream (e.g. "image/jpeg").
	// +optional
	MediaType string `json:"mediaType,omitempty"`

	// NativeRateHz is the sensor's native capture rate.
	// +optional
	NativeRateHz float64 `json:"nativeRateHz,omitempty"`

	// RequiredCapabilities are robot capabilities that must be Active for this
	// stream to serve.
	// +optional
	// +listType=set
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty"`

	// ActivationCost classifies the stream's power/bandwidth cost.
	// +optional
	ActivationCost ActivationCost `json:"activationCost,omitempty"`

	// SupportsClip reports whether the stream can serve a pre/post-roll window.
	// +optional
	SupportsClip bool `json:"supportsClip,omitempty"`

	// ClipPreSecondsMax bounds the pre-roll window when SupportsClip is true.
	// +optional
	ClipPreSecondsMax int32 `json:"clipPreSecondsMax,omitempty"`
}

// CapabilityStatusEntry is the computed runtime status of one capability,
// from either spec.capabilities[] or a model-granted synthetic entry (see the
// Capability Derivation Truth Table).
type CapabilityStatusEntry struct {
	// Name matches a spec.capabilities[*].name or a
	// status.modelGrantedCapabilities[*].capabilities[] entry.
	Name string `json:"name"`

	// Status is the computed truth-table result (or an override state).
	Status CapabilityStatus `json:"status"`

	// Paused is true only when a ZoneMaintenance override has suspended this
	// (pauseable) capability; pauseable:false capabilities are never Paused.
	Paused bool `json:"paused"`

	// Reason explains a non-Active status; empty when Status is Active.
	// +optional
	Reason string `json:"reason,omitempty"`

	// ResolvedParameters carries the current numeric values for a parametric
	// hardware-native capability's spec.capabilities[*].parameters, resolved
	// from the sourced hardware spec fields.
	// +optional
	ResolvedParameters map[string]float64 `json:"resolvedParameters,omitempty"`

	// DegradedSchedulable reflects the capability's degradedPolicy: when true, the
	// Scheduler may satisfy parametric constraints with this capability while it is
	// Degraded, using ResolvedParameters. Derived from the capability definition's
	// degradedPolicy.schedulable; written by the capability derivation, never by
	// operators.
	// +optional
	DegradedSchedulable bool `json:"degradedSchedulable,omitempty"`

	// DegradedSince is when Status last transitioned away from Active.
	// +optional
	DegradedSince *metav1.Time `json:"degradedSince,omitempty"`
}

// ModelGrantedCapabilityEntry records the capabilities a completed
// ModelRollout has granted for one installed model. Written exclusively by
// the OTA/Model Update Manager; operators MUST NOT write this field.
type ModelGrantedCapabilityEntry struct {
	// ModelName matches a spec.installedModels[*].name entry.
	ModelName string `json:"modelName"`

	// GrantedBy is the name of the ModelRollout resource that last wrote this entry.
	GrantedBy string `json:"grantedBy"`

	// Capabilities are the names currently granted by this model version.
	// Revoked names are absent; the Capability Controller drops them from the
	// computed status.capabilities[] on its next reconciliation.
	// +optional
	// +listType=set
	Capabilities []string `json:"capabilities,omitempty"`
}

// InstalledModelStatusEntry is the runtime state of one installed model,
// parallel to spec.installedModels[] — mirroring the Kubernetes
// spec.containers[] / status.containerStatuses[] pattern. It is written by the
// control plane from the adapter's reports, not by a component on the robot.
type InstalledModelStatusEntry struct {
	// Name matches a spec.installedModels[*].name entry.
	Name string `json:"name"`

	// Status is the model's current runtime state.
	Status ModelStatus `json:"status"`

	// RunningVersion is the version currently loaded on the robot; it may lag
	// spec during a rollout.
	// +optional
	RunningVersion string `json:"runningVersion,omitempty"`

	// ActiveAt is when the model last transitioned to Active.
	// +optional
	ActiveAt *metav1.Time `json:"activeAt,omitempty"`

	// FailureReason is empty when Status is Active.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`
}

// FirmwareInstallState is a robot's reported firmware install state (ADR-0033).
type FirmwareInstallState struct {
	// Status is the reported install state. Unset means the adapter reports none.
	// +optional
	Status FirmwareInstallStatus `json:"status,omitempty"`

	// RunningVersion is what the robot says it is running now. On a failed install
	// this is NOT AttemptedVersion — it is whatever the robot fell back to, which is
	// the fact an operator needs and cannot infer.
	// +optional
	RunningVersion string `json:"runningVersion,omitempty"`

	// AttemptedVersion is the version a failed or in-flight install targeted.
	// +optional
	AttemptedVersion string `json:"attemptedVersion,omitempty"`

	// FailureReason is set only when Status is Failed.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// ReportedAt is when the adapter last reported this state.
	// +optional
	ReportedAt *metav1.Time `json:"reportedAt,omitempty"`
}

// FirmwareInstallStatus is a robot's reported firmware install state.
// +kubebuilder:validation:Enum=Running;Updating;Failed
type FirmwareInstallStatus string

// FirmwareInstallStatus values. There is deliberately no "Unknown": an adapter that
// reports nothing leaves Robot.status.firmwareInstall absent, so "not reported" and
// "reported as something" stay distinguishable (explicit presence).
const (
	// FirmwareInstallRunning — no install in flight.
	FirmwareInstallRunning FirmwareInstallStatus = "Running"
	// FirmwareInstallUpdating — an install is in progress.
	FirmwareInstallUpdating FirmwareInstallStatus = "Updating"
	// FirmwareInstallFailed — the last install did not complete; the robot is running
	// RunningVersion, which is not AttemptedVersion.
	FirmwareInstallFailed FirmwareInstallStatus = "Failed"
)

// ConnectivityStatus is Fleet Adapter connectivity telemetry.
type ConnectivityStatus struct {
	// LastSeenAt is stamped by the FleetAdapter controller
	// (FleetAdapterReconciler.stampLastSeen, via projectLiveness) on every
	// handshake and heartbeat from the robot's ADAPTER, for every Robot bound to
	// that adapter, throttled to once per half the offline threshold. No
	// TelemetryPayload refreshes it, so it measures adapter presence, not this
	// robot's. A robot that has streamed telemetry and then goes silent behind a
	// heartbeating adapter is still taken Offline, by the Robot controller's separate
	// per-robot silence clock (Ready=False, reason RobotSilent), once it has sent
	// nothing for the longer of the namespace Offline threshold and three
	// spec.telemetryIntervalSeconds; that clock is not recorded in this field. A robot
	// whose adapter never sends telemetry for it is judged by adapter presence alone.
	// +optional
	LastSeenAt *metav1.Time `json:"lastSeenAt,omitempty"`

	// LatencyMs is specified as the round-trip latency of the last gRPC ping.
	// No code writes it at v0.4.
	// +optional
	LatencyMs *int32 `json:"latencyMs,omitempty"`
}

// RobotHealth is the aggregate health summary computed from hardware and
// capability status.
type RobotHealth struct {
	// Status is the aggregate health state.
	Status HealthState `json:"status"`

	// Message is a human-readable summary.
	// +optional
	Message string `json:"message,omitempty"`
}

// RobotStatus describes the observed state of a Robot (RFC-0001 §Robot
// Schema). Written exclusively by controllers via the /status subresource;
// operators MUST NOT write to status fields directly.
type RobotStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase RobotPhase `json:"phase,omitempty"`

	// OfflineSince is the time the robot entered the Offline phase, set by the
	// Robot controller on the Offline transition and cleared on reconnect. It
	// anchors the swarmada_robot_offline_duration_seconds metric (RFC-0001 #control-plane-observability-prometheus-metrics-contract) — the
	// completed offline span observed at reconnect — and is not otherwise
	// consumed. Nil whenever the robot is not (and was not) Offline.
	// +optional
	OfflineSince *metav1.Time `json:"offlineSince,omitempty"`

	// Hardware is per-component runtime health, one entry per
	// spec.hardware[]. Written by the Health Monitor from Fleet Adapter
	// telemetry.
	// +optional
	// +listType=map
	// +listMapKey=name
	Hardware []HardwareComponentStatus `json:"hardware,omitempty"`

	// SensorStreams is the read-only discovery projection of every sensor
	// stream this robot's adapter declared. Written
	// only by CapabilitiesIngestor from CapabilitiesSnapshot.sensor_streams.
	// +optional
	// +listType=map
	// +listMapKey=streamId
	SensorStreams []SensorStreamEntry `json:"sensorStreams,omitempty"`

	// HonouredOperatingConstraints is the read-only projection of the operating
	// SUBJECTS this robot's adapter declared it can honour while the robot is in a
	// zone that restricts one. Written only by CapabilitiesIngestor from
	// CapabilitiesSnapshot.honoured_operating_constraints — never by a task or an
	// operator — on exactly the path SensorStreams above already takes (FA-R1).
	//
	// THIS IS NOT THE CAPABILITY VOCABULARY (ADR-0058 §7). Status.Capabilities is
	// never consulted to resolve a subject, and a subject that happens to collide
	// with a capability name means nothing. A capability answers "can this robot be
	// asked to do X"; a subject answers "is X happening right now", which is true or
	// false of a robot that was asked to do nothing at all.
	//
	// ABSENT OR EMPTY SATISFIES NO SUBJECT. An adapter that declares nothing is not
	// in breach of FA-R1 and is not marked non-conformant — and it is not treated as
	// participating either: its robots are withheld from every zone restricting a
	// subject, on the same footing as a robot whose adapter declares other subjects
	// and not this one (ADR-0058 Amendment 1 §A1.2). The alternative reading, in
	// which declaring nothing means "not participating, so dispatch", is the
	// permissive resolution of an unresolvable case, and it would dispatch precisely
	// the robot ADR-0058 §1 exists for: the one transiting a ward with a live
	// microphone under an adapter that models no operating subjects at all.
	//
	// Candidate filter 12 (RFC-0001 #control-plane-scheduler) reads this field: a
	// robot is withheld from an action while the operating rules in force at the zone
	// it occupies (status.currentZone, whose rules fold in every zone above it), or in
	// any zone of its status.containingZones, restrict a subject this list does not
	// name (ADR-0058 Amendment 2).
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self.all(s, s.contains('/') || s in ['camera','microphone','water','chemical','uv','speed','noise'])",message="status.honouredOperatingConstraints[]: each subject must be one of the normative core seven (camera, microphone, water, chemical, uv, speed, noise), or a namespaced extension subject such as 'example.com/lidar'"
	HonouredOperatingConstraints []string `json:"honouredOperatingConstraints,omitempty"`

	// Capabilities is the computed union of spec-declared and model-granted
	// capabilities (see the Capability Derivation Truth Table) — the single
	// authoritative set the Scheduler reads. No controller ever writes
	// spec.capabilities[]; this field is the only one the Scheduler consults.
	// +optional
	// +listType=map
	// +listMapKey=name
	Capabilities []CapabilityStatusEntry `json:"capabilities,omitempty"`

	// ModelGrantedCapabilities is written exclusively by the OTA/Model Update
	// Manager after a completed ModelRollout. Operators MUST NOT write here.
	// +optional
	// +listType=map
	// +listMapKey=modelName
	ModelGrantedCapabilities []ModelGrantedCapabilityEntry `json:"modelGrantedCapabilities,omitempty"`

	// InstalledModels is the per-model runtime state, parallel to
	// spec.installedModels[]. Writers: the ModelRollout controller
	// (upsertModelStatus, which sets Updating at batch entry and on a rollback
	// push) and the install-state projection (projectModelState), which upserts
	// each model an adapter reports in an UpdateProgress outcome or a
	// CapabilitiesSnapshot.
	// +optional
	// +listType=map
	// +listMapKey=name
	InstalledModels []InstalledModelStatusEntry `json:"installedModels,omitempty"`

	// BatteryPercent is the last reported battery charge (0–100). It is a COARSE,
	// throttled projection, updated only when the charge crosses a configured
	// bucket boundary — never on every telemetry tick (RA-1). For continuous
	// battery history, query the telemetry TSDB, not this field.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	BatteryPercent *int32 `json:"batteryPercent,omitempty"`

	// Position is specified as a COARSE, heavily-throttled last-known pose for
	// at-a-glance display only. No code writes it at v0.4: pose reaches the
	// telemetry TSDB and the Zone Controller's zone derivation, never this field
	// (RA-1; see RFC-0001 #architecture-telemetry-architecture and the Drawbacks
	// section). Do not build motion or deconfliction logic on this field.
	// +optional
	Position *RobotPosition `json:"position,omitempty"`

	// CurrentZone is derived by the Zone Controller from position telemetry;
	// it may differ from spec.zone if the robot has physically crossed a zone
	// boundary.
	// +optional
	CurrentZone string `json:"currentZone,omitempty"`

	// ContainingZones is EVERY zone whose polygon contains the robot, sorted by name
	// (ADR-0059). It is the containment SET, of which CurrentZone is one arbitrated
	// member.
	//
	// THE TWO ANSWER DIFFERENT QUESTIONS AND MUST NOT BE CONFLATED. CurrentZone answers
	// "where should this robot report it is" — a single name, because candidate
	// filter 2, the reservation lifecycle, TDE admission and SpecZoneMatchesCurrent all
	// read it and all want one answer, and it is arbitrated from overlapping polygons by
	// nearest centroid. This answers "what binds it". Overlapping zones are a legitimate
	// modelling choice — a charging bay inside a warehouse floor, a sterile corridor
	// crossing two wards — and a robot inside two of them is inside BOTH. Resolving
	// restrictions from the arbitrated winner alone discarded the loser's rules, which is
	// the permissive direction, where FZ-R2 (denial is absorbing) and FZ-R6 (fail closed)
	// both point at a union.
	//
	// EMPTY MEANS NO ZONE POLYGON CONTAINS THE ROBOT, which is the same fact an empty
	// CurrentZone reports and is accompanied by the same ZonePositionUnmatched warning.
	// It does NOT mean containment is unknown: a zone that declares no polygon
	// (ZoneGeometryAbsent) or whose polygon is FrameLocal never appears here at all,
	// because containment cannot be derived for a region whose shape in the site frame
	// the plane does not know. Robots in those zones are governed by the assignment path
	// alone; ADR-0059 §6 states that limit and what would remove it.
	//
	// WRITTEN ONLY WHEN THE SET CHANGES (RA-1). It is derived from position telemetry,
	// which is precisely where per-tick status writes are prohibited, so it is written on
	// the same edge-triggered transition that writes CurrentZone and never on a tick that
	// changes nothing. Sorted so that set equality is a slice comparison: an order that
	// varied would make an unchanged containment look like a transition.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	ContainingZones []string `json:"containingZones,omitempty"`

	// SpecZoneMatchesCurrent is true when CurrentZone == spec.zone. It is
	// informational: the Scheduler does not read it, and candidate filter 2 reads
	// CurrentZone.
	// +optional
	SpecZoneMatchesCurrent *bool `json:"specZoneMatchesCurrent,omitempty"`

	// AssignedAction is the name of the FleetAction currently assigned to this
	// robot, if any. The control plane writes it when it gives the robot an action and
	// clears it only once the robot is provably off that action: its adapter acknowledged a
	// cancel for it or reported it ended, or the action's lease is provably dead. It is what
	// keeps a second robot from being given work this robot may still be running; a write of
	// it by any other identity is refused.
	// +optional
	AssignedAction string `json:"assignedAction,omitempty"`

	// FirmwareVersion is the firmware the robot is currently running. Written by
	// the install-state projection (projectFirmwareState), which promotes
	// status.firmwareInstall.runningVersion into this field when the robot's own
	// report says Running (RFC-0001 #firmwarerollout-robot-status-firmwareversion-field).
	// The FirmwareRollout controller reads it and does not write it. Unwritten
	// for a robot whose adapter reports no firmware install state. Operators MUST
	// NOT write it.
	// +optional
	FirmwareVersion string `json:"firmwareVersion,omitempty"`

	// PreviousFirmwareVersion is the firmware version prior to the last successful
	// update, retained for rollback (RFC-0001 #control-plane-ota-model-update-manager).
	// Written by the install-state projection on the same transition that changes
	// firmwareVersion.
	// +optional
	PreviousFirmwareVersion string `json:"previousFirmwareVersion,omitempty"`

	// FirmwareInstall is the robot's OWN report of its firmware install state
	// (ADR-0033). FirmwareVersion above is derived from it: it is promoted from
	// runningVersion only when this report says Running, so an Updating or Failed
	// report leaves FirmwareVersion on the last version the robot confirmed. A
	// failed install is visible here, with the version it attempted, while
	// FirmwareVersion is unchanged.
	//
	// Adapter-reported, from an UpdateProgress outcome or a CapabilitiesSnapshot,
	// and projected on change only (RA-1); absent for an adapter that reports
	// neither.
	// +optional
	FirmwareInstall *FirmwareInstallState `json:"firmwareInstall,omitempty"`

	// EstopState is the robot's emergency-stop status (RFC-0001 #safety-estop-state-machine),
	// independent of Phase. Empty is treated as Normal (no active estop). Its single
	// writer is the control plane's estop dispatcher, over the SafetyStream estop path
	// (RFC-0001 #safety-emergency-stop-protocol):
	//
	// Rows S1 to S7 of RFC-0001 #safety-robot-stop-table state when
	// each value is entered and left; this list says what each value means.
	//
	//   - Stopping: the adapter acknowledged the stop STOPPING: the hardware stop is
	//     commanded and rest is not confirmed.
	//   - Stopped: rest confirmed by an EstopAck STOPPED, never inferred from
	//     telemetry or a timeout.
	//   - Failed: no confirmed stop was obtained, or the adapter reported the stop
	//     FAILED. Failed is not "stopped"; it is an escalation.
	//   - Resuming: the last stop in force was cleared and no operator has released
	//     the robot yet (swarmctl estop resume writes the swarmada.io/estop-resume
	//     annotation); nothing releases it automatically.
	//     Where the admission webhooks are installed, a robots/status write that changes
	//     this field is refused unless it comes from the control plane's own service
	//     account; on an install without them it is not.
	//   - Normal: no stop; after a stop, only an operator's release of a Resuming
	//     robot whose readiness checks pass sets it.
	//
	// Stopping, Stopped and Failed pause the robot's assigned FleetAction
	// (RFC-0001 #safety-fleetaction-behaviour-during-estop) and exclude the robot
	// from dispatch.
	// Resuming withholds the robot from new dispatch but does not pause an action it
	// already holds. Estop is never fenced: a safe stop is always honoured.
	//
	// The state lives only here, so a robot deleted and created again would come back
	// Normal. Where the admission webhooks are installed, a Robot whose estopState is set
	// and not Normal, whose phase is Error or that has a Failed component is not deleted,
	// by any identity, and does not lose its last finalizer except to the control plane,
	// unless its namespace is being deleted; offline auto-removal leaves such a robot in
	// place. The delete check alone fails open: while no manager replica serves the
	// webhooks, such a robot can be deleted. The swarmada.io/robot-id annotation, which
	// routes the physical robot's reports and commands to this Robot, cannot change after
	// create, so the stop cannot be moved off it either. A robot held by its estop or by a
	// Failed component that never reports again stays held: the resume check refuses an
	// Offline robot, and only the robot's own report clears a Failed component. No command
	// decommissions such a robot on its own, and no delete of the Robot is admitted: it is
	// removed only with its namespace. A hold by phase Error alone ends differently: Error
	// is the robot's own report, and a robot that falls silent is marked Offline, which
	// ends that hold. An Offline robot is not dispatched, and one that still has the fault
	// reports Error again when it is heard from.
	// +optional
	EstopState RobotEstopState `json:"estopState,omitempty"`

	// EstopBaseline is the record the estop dispatcher takes when a stop begins: the
	// capabilities that were Active at that moment. It is written in the same patch as
	// the first stop state of an episode (Stopping, Stopped or Failed from Normal), kept
	// while the robot is stopped or Resuming, and removed in the patch that writes
	// Normal. Its writer is the estop dispatcher alone, and it is written on those two
	// transitions only (RA-1). The resume confirmation reads it: a capability flagged
	// Paused (a ZoneMaintenance window paused it) never holds the release; with this
	// record, only a capability that was Active at the stop and is not Active now
	// holds it, and one the robot no longer declares does not. Absent on a robot that
	// is not under a stop, and on one whose stop was taken before the control plane
	// recorded it; the resume confirmation then requires every capability that is
	// not Paused to be Active.
	// +optional
	EstopBaseline *EstopBaseline `json:"estopBaseline,omitempty"`

	// EstopHeldBy names each zone or namespace emergency stop that holds the robot
	// (RFC-0001 #safety-estop-scopes). An entry is written when that stop is issued to the
	// robot. It names its carrier by kind and name, and is removed by the clear of a stop on a
	// carrier of that kind and name: the stop that wrote it, or, when that zone or
	// SwarmadaConfig was deleted and created again under the same name, the new carrier's
	// stop, whose clear removes the older entry too. An entry whose carrier no longer exists,
	// or exists under another UID, is also removed by a robot-scope stop and clear on the
	// robot. A move out of the zone, a change of the zone tree or of estopPolicy, or a new
	// position report does not remove it. While any
	// entry stands, or the robot carries its own swarmada.io/estop-triggered annotation, the
	// robot is withheld from dispatch, an action it holds is paused, and its release is
	// refused. Its writer is the control plane; where the admission webhooks are installed a
	// robots/status write that changes it from any other identity is refused.
	// +optional
	// +listType=map
	// +listMapKey=kind
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=128
	EstopHeldBy []EstopHold `json:"estopHeldBy,omitempty"`

	// Stops records each stop the control plane sent the robot, by its stop ID (RFC-0001
	// #safety-robot-stop-table, the term recorded stop). The estop dispatcher on the leader
	// appends an entry after it has sent the stop (row S1), and records acknowledgedAt with the
	// first STOPPING (row S2). A Robot, Zone or Namespace entry gates nothing: a stop is sent
	// first. An EndState entry is its stop's trigger and holds the robot. The list keeps
	// every stop since the robot was created whose clear was never acknowledged, up to 256; at the
	// cap a stop is still sent, and a Warning Event StopRecordFull names the robot. Its writer is
	// the control plane; where the admission webhooks are installed a robots/status write that
	// changes it from any other identity is refused.
	// +optional
	// +listType=map
	// +listMapKey=id
	// +kubebuilder:validation:MaxItems=256
	Stops []RobotStop `json:"stops,omitempty"`

	// Connectivity is Fleet Adapter connectivity telemetry.
	// +optional
	Connectivity *ConnectivityStatus `json:"connectivity,omitempty"`

	// Health is the aggregate health summary.
	// +optional
	Health *RobotHealth `json:"health,omitempty"`

	// Conditions is the standard set of controller conditions.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// RobotPosition holds the robot's last known pose in the zone's coordinate frame.
type RobotPosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`

	// Floor is the integer floor number (0 = ground). Optional so "unknown"
	// (single-floor sites) is distinguishable from floor 0.
	// +optional
	Floor *int32 `json:"floor,omitempty"`

	// Altitude is the vertical position in metres for geodetic (aerial) namespaces,
	// per SwarmadaConfig coordinateSystem.geodetic.altitudeReference (AGL/MSL).
	// Set under a Geodetic namespace, unset for Local. It is reported alongside Floor,
	// not instead of it: zone containment compares Floor in every frame.
	// +optional
	Altitude *float64 `json:"altitude,omitempty"`

	Yaw float64 `json:"yaw"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// Robot is the Schema for the robots API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rob
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=".spec.zone"
// +kubebuilder:printcolumn:name="Battery",type=integer,JSONPath=".status.batteryPercent"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=".status.assignedAction"
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=".spec.robotClass",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type Robot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RobotSpec   `json:"spec,omitempty"`
	Status RobotStatus `json:"status,omitempty"`
}

// RobotList contains a list of Robot.
// +kubebuilder:object:root=true
type RobotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Robot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Robot{}, &RobotList{})
}
