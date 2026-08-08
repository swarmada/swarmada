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

// RobotIDAnnotation carries the stable, globally-unique robot_id (serial / MAC)
// the Fleet Adapter announces, mapping a telemetry frame's robot_id to its Robot
// (RFC-0001 §9.3.1: robot_id ↔ swarmada.io/robot-id). Set on the Robot at admission
// (auto-admit stamps it; the RobotDefaulter backfills operator-created Robots) so the
// telemetry status projection and zone routing can resolve robot_id → Robot.
const RobotIDAnnotation = "swarmada.io/robot-id"

// AutoAdmittedAnnotation marks a Robot created by auto-admit (ADR-0014). Only Robots
// carrying it are eligible for opt-in offline auto-removal (ADR-0030); operator-created
// robots never are, so a warehouse fleet is unaffected by the removal policy.
const AutoAdmittedAnnotation = "swarmada.io/auto-admitted"

// ── Enumerations ──────────────────────────────────────────────────────────────

// RobotPhase is the high-level lifecycle state of a Robot. The value set mirrors
// the fleet_adapter.v1 RobotPhase wire enum (RFC-0001 §5.2.3), the authoritative
// schema per §5.3.
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
// (RFC-0001 §9.6.2.3). A robot may be in any Phase and Stopped simultaneously.
// +kubebuilder:validation:Enum=Normal;Stopping;Stopped;Resuming;Failed
type RobotEstopState string

// RobotEstopState values (§9.6.2.3).
const (
	// RobotEstopNormal: no active estop.
	RobotEstopNormal RobotEstopState = "Normal"
	// RobotEstopStopping: the Fleet Adapter has issued the stop to hardware.
	RobotEstopStopping RobotEstopState = "Stopping"
	// RobotEstopStopped: CONFIRMED at rest; awaiting explicit operator clear.
	RobotEstopStopped RobotEstopState = "Stopped"
	// RobotEstopResuming: operator cleared; motion re-enabling, capabilities restoring.
	RobotEstopResuming RobotEstopState = "Resuming"
	// RobotEstopFailed: the control plane could not obtain a CONFIRMED stop within
	// the SLA/watchdog (dropped estop, silence, or STOPPING with no STOPPED). It is
	// NOT "stopped" — it is an escalation signal (§9.6.2.3): a stop was commanded
	// but never confirmed, so the robot must be escalated (edge/manual), never
	// treated as at-rest.
	RobotEstopFailed RobotEstopState = "Failed"
)

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

	// Version is used by the OTA/Model Update Manager to validate compatibility
	// before pushing firmware; admission rejects mismatched major versions.
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

	// MinBatteryPctToCharge is the battery level below which the robot
	// initiates autonomous docking.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MinBatteryPctToCharge *int32 `json:"minBatteryPctToCharge,omitempty"`

	// TargetBatteryPct is the level at which the robot leaves the dock; it
	// MUST be greater than MinBatteryPctToCharge.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	TargetBatteryPct *int32 `json:"targetBatteryPct,omitempty"`
}

// MaintenanceWindow is a recurring planned-maintenance window. When the
// current time falls within the window AND a ZoneMaintenance resource exists
// for the robot's zone, the robot transitions to the Maintenance phase;
// outside the window, a targeting ZoneMaintenance is queued, not applied.
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
// (a same-named entry fully overrides the class's); Constraints and Charging
// are whole-value overrides. See RobotClassSpec for the merge semantics.
//
// Both rules are guarded with !has(self.capabilities) since capabilities is
// optional — a Robot that omits it entirely (the common case; capabilities
// are frequently supplied only via RobotClass/ModelRollout) must not fail CEL
// evaluation with "no such key: capabilities".
// +kubebuilder:validation:XValidation:rule="!has(self.capabilities) || self.capabilities.all(c, !has(c.requiredHardware) || c.requiredHardware.all(h, self.hardware.exists(hw, hw.name == h)))",message="a capability's requiredHardware must reference a component declared in spec.hardware"
// +kubebuilder:validation:XValidation:rule="!has(self.capabilities) || self.capabilities.all(c, !has(c.providingModel) || self.installedModels.exists(m, m.name == c.providingModel))",message="a capability's providingModel must reference a model declared in spec.installedModels"
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
	// exclusively in status.installedModels[], written by the Robot Agent —
	// mirroring spec.containers[] / status.containerStatuses[].
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

	// Constraints are operational limits the Scheduler uses as filter
	// predicates (e.g. a action requiring more than MaxPayloadKg is never
	// assigned here).
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
	// interval between TelemetryPayload frames and the basis for Offline detection
	// (3x this value with no heartbeat). Lower values increase control-plane load;
	// 5s is the recommended production default. Use 1s only for development.
	//
	// No schema default: unset here it is inherited from the robot's RobotClass
	// (spec.defaultTelemetry.telemetryIntervalSeconds) by the RobotDefaulter merge —
	// the same inheritance model as adapter/constraints/charging.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	TelemetryIntervalSeconds *int32 `json:"telemetryIntervalSeconds,omitempty"`

	// MotionThresholdMeters is the minimum position change (in the units declared by
	// SwarmadaConfig.spec.coordinateSystem) before the Fleet Adapter sends a new
	// position frame. A stationary robot may skip position frames down to the ceiling
	// imposed by MaxIdleIntervalSeconds. Offline detection relies on heartbeat, not
	// position frames, and is unaffected. Unset here it is inherited from the class's
	// defaultTelemetry.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MotionThresholdMeters *float64 `json:"motionThresholdMeters,omitempty"`

	// MaxIdleIntervalSeconds is the maximum time a stationary robot may go without
	// sending a position frame even if MotionThresholdMeters has not been exceeded,
	// bounding staleness of last-known position. Unset here it is inherited from the
	// class's defaultTelemetry.
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
	// component. Absent when the component has never reported Healthy.
	// +optional
	LastHealthyAt *metav1.Time `json:"lastHealthyAt,omitempty"`

	// DegradationReason is a human-readable reason from the Fleet Adapter;
	// empty when Status is Healthy.
	// +optional
	DegradationReason string `json:"degradationReason,omitempty"`

	// DegradedMetrics carries type-specific numeric readings populated when
	// Status is not Healthy (e.g. a Camera's depthFrameRateFps). Numeric-only
	// so the field stays typed rather than free-form interface{} — see
	// api-principles.md "Opaque and free-form data".
	// +optional
	DegradedMetrics map[string]float64 `json:"degradedMetrics,omitempty"`
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

// InstalledModelStatusEntry is the Robot Agent's runtime report for one
// installed model, parallel to spec.installedModels[] — mirroring the
// Kubernetes spec.containers[] / status.containerStatuses[] pattern.
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
	// LastSeenAt is the timestamp of the last received heartbeat from this robot.
	// +optional
	LastSeenAt *metav1.Time `json:"lastSeenAt,omitempty"`

	// LatencyMs is the round-trip latency of the last gRPC ping.
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
	// anchors the swarmada_robot_offline_duration_seconds metric (§9.3.8) — the
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

	// InstalledModels is the Robot Agent's per-model runtime report, parallel
	// to spec.installedModels[].
	// +optional
	// +listType=map
	// +listMapKey=name
	InstalledModels []InstalledModelStatusEntry `json:"installedModels,omitempty"`

	// BatteryPercent is the last reported battery charge (0–100). It is a COARSE,
	// throttled projection, updated only when the charge crosses a configured
	// bucket boundary — never on every telemetry tick (RA-1). For continuous
	// battery history, query the telemetry TSDB, not this field.
	// +optional
	BatteryPercent *int32 `json:"batteryPercent,omitempty"`

	// Position is a COARSE, heavily-throttled last-known pose retained for
	// at-a-glance display only. Live pose is served from the telemetry TSDB and is
	// deliberately NOT written to etcd at telemetry cadence (RA-1; see §5.4 and the
	// Drawbacks section). Do not build motion or deconfliction logic on this field.
	// +optional
	Position *RobotPosition `json:"position,omitempty"`

	// CurrentZone is derived by the Zone Controller from position telemetry;
	// it may differ from spec.zone if the robot has physically crossed a zone
	// boundary.
	// +optional
	CurrentZone string `json:"currentZone,omitempty"`

	// SpecZoneMatchesCurrent is true when CurrentZone == spec.zone. The
	// Scheduler uses this to detect zone drift before action assignment.
	// +optional
	SpecZoneMatchesCurrent *bool `json:"specZoneMatchesCurrent,omitempty"`

	// AssignedAction is the name of the FleetAction currently assigned to this
	// robot, if any.
	// +optional
	AssignedAction string `json:"assignedAction,omitempty"`

	// FirmwareVersion is the firmware the robot is currently running. Written by
	// the FirmwareRollout controller after a confirmed successful update (§9.1.7.2);
	// operators MUST NOT write it.
	// +optional
	FirmwareVersion string `json:"firmwareVersion,omitempty"`

	// PreviousFirmwareVersion is the firmware version prior to the last successful
	// update, retained for rollback (§9.3.6).
	// +optional
	PreviousFirmwareVersion string `json:"previousFirmwareVersion,omitempty"`

	// FirmwareInstall is the robot's OWN report of its firmware install state
	// (ADR-0033), distinct from FirmwareVersion above, which is the control plane's
	// record of what it pushed. The two disagreeing is the signal that matters: a
	// failed install leaves the robot running something other than the target, and
	// before this field the control plane had no way to learn that had happened.
	//
	// Adapter-reported and projected on change only (RA-1); absent for an adapter
	// that does not implement push_firmware.
	// +optional
	FirmwareInstall *FirmwareInstallState `json:"firmwareInstall,omitempty"`

	// EstopState is the robot's emergency-stop status (RFC-0001 §9.6.2.3),
	// independent of Phase. Active states (Stopping/Stopped) pause the robot's
	// assigned FleetAction (§9.6.2.4). Empty is treated as Normal (no active estop).
	// Set by the SafetyStream estop path (§9.6.2, not yet built) from a CONFIRMED
	// EstopAck — never inferred from telemetry or a timeout; consumed by the
	// FleetAction controller. Estop is never fenced: a safe stop is always honoured.
	// +optional
	EstopState RobotEstopState `json:"estopState,omitempty"`

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
	// Frame-exclusive with Floor: set under a Geodetic namespace, unset for Local.
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
