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

// RobotClass (RFC-0001 §5.2.1) is a namespace-scoped template capturing the
// shared hardware inventory, default inference models, base capabilities, and
// operational defaults common to every robot of one physical type. A Robot that
// sets spec.robotClass inherits these fields at admission time (the merge runs
// once and the resolved spec is persisted on the Robot; updating the class does
// NOT retroactively mutate existing robots — see §5.2.1.3).
//
// RobotClass is also the key the FleetAdapter admission gate (§5.2.12) matches
// on: a Robot referencing a class is admissible only if a Connected, conformant
// FleetAdapter serves that class.

// ── Enumerations ──────────────────────────────────────────────────────────────

// HardwareComponentType classifies a physical component in a hardware inventory.
// Custom covers vendor-specific components without a first-class type; such
// entries MUST also set customType.
// +kubebuilder:validation:Enum=Lidar;Camera;Gripper;LoadPlatform;Arm;Thermal;Microphone;Display;Custom
type HardwareComponentType string

// HardwareComponentType values.
const (
	HardwareTypeLidar        HardwareComponentType = "Lidar"
	HardwareTypeCamera       HardwareComponentType = "Camera"
	HardwareTypeGripper      HardwareComponentType = "Gripper"
	HardwareTypeLoadPlatform HardwareComponentType = "LoadPlatform"
	HardwareTypeArm          HardwareComponentType = "Arm"
	HardwareTypeThermal      HardwareComponentType = "Thermal"
	HardwareTypeMicrophone   HardwareComponentType = "Microphone"
	HardwareTypeDisplay      HardwareComponentType = "Display"
	HardwareTypeCustom       HardwareComponentType = "Custom"
)

// CapabilityKind describes how a capability is provided. A hardware-native
// capability is available whenever its required hardware is Healthy; a
// model-driven capability additionally requires its providing model to be
// Active; a manual capability is toggled by operator override.
// +kubebuilder:validation:Enum=hardware-native;model-driven;manual
type CapabilityKind string

// CapabilityKind values.
const (
	CapabilityKindHardwareNative CapabilityKind = "hardware-native"
	CapabilityKindModelDriven    CapabilityKind = "model-driven"
	CapabilityKindManual         CapabilityKind = "manual"
)

// ── Spec ──────────────────────────────────────────────────────────────────────

// RobotClassSpec is the shared template merged into a Robot's spec at admission
// (§5.2.1.2). Scalar fields: the Robot value wins if present, else the class
// value. List fields (hardware, defaultModels, baseCapabilities) merge
// union-by-name — a Robot entry with the same name fully replaces the class
// entry; there is no partial per-field merge within a list element.
type RobotClassSpec struct {
	// Manufacturer is the canonical manufacturer identifier (lowercase, no spaces).
	// +kubebuilder:validation:MinLength=1
	Manufacturer string `json:"manufacturer"`

	// Model is the manufacturer's model name; informational but indexed for
	// filtering (e.g. `swarmctl get robot --model acme-picker`).
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// BaseAdapter is the Fleet Adapter configuration inherited by every robot of
	// this class. A Robot may override adapter.version (e.g. during a staged
	// adapter rollout) but inherits the adapter name.
	BaseAdapter BaseAdapterRef `json:"baseAdapter"`

	// Hardware is the physical component inventory present on every robot of this
	// class. Keyed by name; a Robot overrides one entry by declaring a hardware
	// item with the same name. A Robot cannot remove a class entry — omission
	// means inheritance. See §5.2.3 for the authoritative per-type field list.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Hardware []HardwareComponent `json:"hardware"`

	// DefaultModels are the inference models installed on robots of this class at
	// admission unless the operator's admit command overrides them.
	// +optional
	// +listType=map
	// +listMapKey=name
	DefaultModels []ClassModel `json:"defaultModels,omitempty"`

	// BaseCapabilities are the capabilities every robot of this class advertises,
	// merged (union by name) into the admitted Robot's capability set.
	// +optional
	// +listType=map
	// +listMapKey=name
	BaseCapabilities []ClassCapability `json:"baseCapabilities,omitempty"`

	// DefaultConstraints are operational limits inherited by all robots of this
	// class; any field may be overridden per-robot.
	// +optional
	DefaultConstraints *ClassConstraints `json:"defaultConstraints,omitempty"`

	// DefaultChargingConfig is the charging policy inherited by all robots of this
	// class. dockName is intentionally NOT a class field — docks are per-unit and
	// assigned at admission.
	// +optional
	DefaultChargingConfig *ClassChargingConfig `json:"defaultChargingConfig,omitempty"`

	// DefaultTelemetry are the telemetry-cadence defaults inherited by all robots of
	// this class; any field may be overridden per-robot. Fields a robot leaves unset
	// are filled from here by the RobotDefaulter merge (§9.1.11).
	// +optional
	DefaultTelemetry *ClassTelemetryDefaults `json:"defaultTelemetry,omitempty"`
}

// ClassTelemetryDefaults are per-class telemetry-cadence defaults inherited by a
// Robot's spec.{telemetryIntervalSeconds,motionThresholdMeters,maxIdleIntervalSeconds}
// when the Robot leaves them unset.
type ClassTelemetryDefaults struct {
	// TelemetryIntervalSeconds is the default telemetry reporting interval (the
	// ceiling between frames; basis for Offline detection).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	TelemetryIntervalSeconds *int32 `json:"telemetryIntervalSeconds,omitempty"`

	// MotionThresholdMeters is the default minimum position change before a new
	// position frame is sent.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MotionThresholdMeters *float64 `json:"motionThresholdMeters,omitempty"`

	// MaxIdleIntervalSeconds is the default ceiling on how long a stationary robot may
	// go without sending a position frame.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	MaxIdleIntervalSeconds *int32 `json:"maxIdleIntervalSeconds,omitempty"`
}

// BaseAdapterRef pins the Fleet Adapter, and its default version, that drives
// robots of a class.
type BaseAdapterRef struct {
	// Name is the FleetAdapter resource name that manages this class by default.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the default adapter version applied to newly admitted robots.
	// +kubebuilder:validation:Pattern=^\d+\.\d+\.\d+$
	Version string `json:"version"`
}

// HardwareComponent declares one physical component. Type-specific attributes are
// optional and interpreted according to Type. They are pointers so that "unset"
// is distinguishable from a real zero (a LoadPlatform with maxPayloadKg unset is
// "unknown", not "0 kg") — the same explicit-presence discipline applied to the
// Fleet Adapter telemetry proto.
// +kubebuilder:validation:XValidation:rule="self.type != 'Custom' || (has(self.customType) && size(self.customType) > 0)",message="customType is required when type is Custom"
type HardwareComponent struct {
	// Name uniquely identifies this component within the inventory (merge key).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// Type classifies the component.
	Type HardwareComponentType `json:"type"`

	// Model is the component's manufacturer/model string (e.g. "SICK TIM551").
	// +optional
	Model string `json:"model,omitempty"`

	// CustomType names the component kind when Type is Custom (e.g. "SafetyBus").
	// +optional
	CustomType string `json:"customType,omitempty"`

	// RangeM is the sensing range in metres (Lidar / depth Camera).
	// +optional
	RangeM *float64 `json:"rangeM,omitempty"`

	// HorizontalFovDeg is the horizontal field of view in degrees.
	// +optional
	HorizontalFovDeg *float64 `json:"horizontalFovDeg,omitempty"`

	// ResolutionMp is the sensor resolution in megapixels (Camera).
	// +optional
	ResolutionMp *float64 `json:"resolutionMp,omitempty"`

	// DepthCapable reports whether a Camera provides a depth channel.
	// +optional
	DepthCapable *bool `json:"depthCapable,omitempty"`

	// FrameRateFps is the sensor frame rate (Camera).
	// +optional
	FrameRateFps *int32 `json:"frameRateFps,omitempty"`

	// MaxPayloadKg is the rated payload for a LoadPlatform / Arm in kilograms.
	// +optional
	MaxPayloadKg *float64 `json:"maxPayloadKg,omitempty"`

	// PlatformLengthMm is the load platform length in millimetres.
	// +optional
	PlatformLengthMm *int32 `json:"platformLengthMm,omitempty"`

	// PlatformWidthMm is the load platform width in millimetres.
	// +optional
	PlatformWidthMm *int32 `json:"platformWidthMm,omitempty"`

	// MaxGripForceN is the maximum grip force in newtons (Gripper).
	// +optional
	MaxGripForceN *float64 `json:"maxGripForceN,omitempty"`

	// StrokeMm is the maximum jaw opening in millimetres (Gripper).
	// +optional
	StrokeMm *int32 `json:"strokeMm,omitempty"`

	// ReachMm is the maximum reach from base in millimetres (Arm).
	// +optional
	ReachMm *int32 `json:"reachMm,omitempty"`

	// DegreesOfFreedom is the number of articulated joints (Arm).
	// +optional
	DegreesOfFreedom *int32 `json:"degreesOfFreedom,omitempty"`

	// ResolutionH is the horizontal resolution in pixels (Thermal / Display).
	// +optional
	ResolutionH *int32 `json:"resolutionH,omitempty"`

	// ResolutionV is the vertical resolution in pixels (Thermal / Display).
	// +optional
	ResolutionV *int32 `json:"resolutionV,omitempty"`

	// TempRangeMinC is the minimum sensing temperature in Celsius (Thermal).
	// +optional
	TempRangeMinC *int32 `json:"tempRangeMinC,omitempty"`

	// TempRangeMaxC is the maximum sensing temperature in Celsius (Thermal).
	// +optional
	TempRangeMaxC *int32 `json:"tempRangeMaxC,omitempty"`

	// Channels is the number of audio input channels (Microphone).
	// +optional
	Channels *int32 `json:"channels,omitempty"`

	// SampleRateHz is the audio sample rate in Hz (Microphone).
	// +optional
	SampleRateHz *int32 `json:"sampleRateHz,omitempty"`

	// TouchCapable reports whether a Display accepts touch input.
	// +optional
	TouchCapable *bool `json:"touchCapable,omitempty"`
}

// ClassModel is a default inference model installed on robots of a class.
type ClassModel struct {
	// Name is the logical model name; it must match
	// Robot.spec.installedModels[*].name after a ModelRollout completes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// Version is the model version (semver).
	// +kubebuilder:validation:Pattern=^\d+\.\d+\.\d+$
	Version string `json:"version"`

	// ModelURI is where the Robot Agent fetches the model artifact
	// (e.g. "oci://registry.swarmada.io/models/item-recognition:3.2.1").
	// Scheme-constrained to match ModelRollout.spec.modelUri so a model URI is
	// validated identically wherever it is declared (RFC-0001 D3).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(oci|s3|https)://`
	ModelURI string `json:"modelUri"`

	// RequiredHardware lists component names that must be Healthy for the model to
	// run; a degraded dependency moves the model — and the capabilities it grants
	// — Inactive. MaxItems bounds CEL cost (see Robot.spec.hardware's comment).
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	RequiredHardware []string `json:"requiredHardware,omitempty"`

	// GrantsCapabilities lists capabilities unlocked when the model is Active.
	// MaxItems bounds CEL cost (see Robot.spec.hardware's comment).
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	GrantsCapabilities []string `json:"grantsCapabilities,omitempty"`
}

// ClassCapability is a capability advertised by every robot of a class.
type ClassCapability struct {
	// Name is the capability identifier (e.g. "navigation.2d", "estop.receive").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name"`

	// Type is how the capability is provided.
	Type CapabilityKind `json:"type"`

	// Pauseable reports whether the scheduler may suspend this capability. Safety
	// and monitoring capabilities (e.g. estop.receive, health.heartbeat) MUST be
	// false and are never paused.
	// +kubebuilder:default=true
	Pauseable bool `json:"pauseable"`

	// RequiredHardware lists component names that must be Healthy for this
	// capability to be Active. An empty list means no hardware dependency.
	// MaxItems bounds CEL cost (see Robot.spec.hardware's comment).
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	RequiredHardware []string `json:"requiredHardware,omitempty"`

	// ProvidingModel names the ClassModel backing a model-driven capability.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	ProvidingModel string `json:"providingModel,omitempty"`

	// Parameters carries capability-specific parameters. A parameter is resolved
	// either dynamically from a hardware field (sourceField) — so a per-robot
	// hardware override is reflected automatically — or as a static literal.
	// +optional
	Parameters map[string]CapabilityParameter `json:"parameters,omitempty"`

	// DegradedPolicy governs whether this capability may still be scheduled while
	// in Degraded status. When absent (or Schedulable=false) a Degraded instance is
	// treated as Inactive for scheduling. When Schedulable=true, the Scheduler may
	// satisfy a action's parametric constraints using the capability's currently
	// resolved (reduced) parameters — so a degraded capability serves lower-
	// constraint actions but is excluded from those it can no longer meet.
	// +optional
	DegradedPolicy *CapabilityDegradedPolicy `json:"degradedPolicy,omitempty"`
}

// CapabilityDegradedPolicy declares how a capability behaves for scheduling while
// in Degraded status (RFC-0001 §6.10 capability state machine).
type CapabilityDegradedPolicy struct {
	// Schedulable allows the Scheduler to satisfy parametric constraints with this
	// capability while it is Degraded, using its current (reduced) resolved
	// parameters. Default false: a Degraded capability is not schedulable.
	// +kubebuilder:default=false
	Schedulable bool `json:"schedulable"`
}

// CapabilityParameter is one parameter of a capability. Set exactly one of
// SourceField (dynamic) or Value (static literal).
type CapabilityParameter struct {
	// SourceField is a dotted path into the resolved spec, e.g.
	// "hardware[load-platform].spec.maxPayloadKg".
	// +optional
	SourceField string `json:"sourceField,omitempty"`

	// Value is a static literal value for the parameter.
	// +optional
	Value string `json:"value,omitempty"`
}

// ClassConstraints are operational limits inherited by robots of a class.
type ClassConstraints struct {
	// MaxPayloadKg caps the load the robot will accept, in kilograms.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxPayloadKg *float64 `json:"maxPayloadKg,omitempty"`

	// MinBatteryPctForAction is the battery floor below which the robot refuses new
	// action assignments.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MinBatteryPctForAction *int32 `json:"minBatteryPctForAction,omitempty"`

	// MaxSpeedMs is the maximum commanded speed in metres per second; the Fleet
	// Adapter enforces this limit.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxSpeedMs *float64 `json:"maxSpeedMs,omitempty"`
}

// ClassChargingConfig is the charging policy inherited by robots of a class.
// +kubebuilder:validation:XValidation:rule="!has(self.minBatteryPctToCharge) || !has(self.targetBatteryPct) || self.targetBatteryPct > self.minBatteryPctToCharge",message="targetBatteryPct must be greater than minBatteryPctToCharge"
type ClassChargingConfig struct {
	// MinBatteryPctToCharge is the battery level below which the robot initiates
	// autonomous docking.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MinBatteryPctToCharge *int32 `json:"minBatteryPctToCharge,omitempty"`

	// TargetBatteryPct is the level at which the robot leaves the dock; it MUST be
	// greater than MinBatteryPctToCharge.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	TargetBatteryPct *int32 `json:"targetBatteryPct,omitempty"`
}

// ── Status ────────────────────────────────────────────────────────────────────

// RobotClassStatus is the observed state of a RobotClass. A RobotClass is a
// template and carries little runtime state; status is used to surface
// validation problems (e.g. an unreachable model URI), an observed generation,
// and how many Robots currently inherit from this class.
type RobotClassStatus struct {
	// Conditions is the standard conditions list.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed from.
	// It is the RobotClass generation the admission-time merge (§5.2.1.2) draws from —
	// the "last merge generation" a newly-admitted Robot of this class inherits.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReferencingRobots is the number of Robots in this class's namespace whose
	// spec.robotClass names it. Read/aggregate only — it drives no behavior.
	// +optional
	ReferencingRobots int32 `json:"referencingRobots,omitempty"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// RobotClass is the Schema for the robotclasses API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rc
// +kubebuilder:printcolumn:name="Manufacturer",type=string,JSONPath=".spec.manufacturer"
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=".spec.model"
// +kubebuilder:printcolumn:name="Adapter",type=string,JSONPath=".spec.baseAdapter.name"
// +kubebuilder:printcolumn:name="Robots",type=integer,JSONPath=".status.referencingRobots"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type RobotClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RobotClassSpec   `json:"spec,omitempty"`
	Status RobotClassStatus `json:"status,omitempty"`
}

// RobotClassList contains a list of RobotClass.
// +kubebuilder:object:root=true
type RobotClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RobotClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RobotClass{}, &RobotClassList{})
}
