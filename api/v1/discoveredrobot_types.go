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

// DiscoveredRobotPhase is the lifecycle phase of a DiscoveredRobot.
// +kubebuilder:validation:Enum=Discovered;Stale
type DiscoveredRobotPhase string

const (
	// DiscoveredRobotPhaseDiscovered — connected, awaiting operator admission decision.
	DiscoveredRobotPhaseDiscovered DiscoveredRobotPhase = "Discovered"
	// DiscoveredRobotPhaseStale — approaching TTL expiry; will be deleted if not admitted soon.
	DiscoveredRobotPhaseStale DiscoveredRobotPhase = "Stale"
)

// ModelStatus describes the health of an installed AI model at discovery time.
// Mirrors the fleet_adapter.v1 ModelStatus enum.
// +kubebuilder:validation:Enum=Active;Updating;Failed;Inactive
type ModelStatus string

// ModelStatus values.
const (
	ModelStatusActive   ModelStatus = "Active"
	ModelStatusUpdating ModelStatus = "Updating"
	ModelStatusFailed   ModelStatus = "Failed"
	ModelStatusInactive ModelStatus = "Inactive"
)

// DiscoveredHardwareComponent is a hardware component reported at discovery time.
//
// The attribute set mirrors Robot.spec.hardware[] so a DiscoveredRobot can round-trip
// its reported inventory into a Robot at admission without loss. NOTE (ADR-0022): the
// Discover handler populates the identity/health fields — Name, Type, Status, Model, and
// CustomType (the preserved subtype of an unrecognised Type) — and the physical-measurement
// attributes below, which the fleet_adapter.v1 HardwareComponent carries at tags 6-13.
//
// Each measurement is a POINTER because the wire field is proto3 `optional`: an attribute the
// adapter does not report stays nil, and nil is not 0. A defaulted 0 would read as a measured
// 0 kg payload ceiling or 0 m sensing range, and a defaulted false as "not depth-capable" —
// values a scheduler would act on.
type DiscoveredHardwareComponent struct {
	Name   string                `json:"name"`
	Type   HardwareComponentType `json:"type"`
	Status HardwareStatus        `json:"status"`
	// Model is the manufacturer model string (informational).
	// +optional
	Model string `json:"model,omitempty"`
	// CustomType is the operator-defined subtype when Type is Custom.
	// +optional
	CustomType string `json:"customType,omitempty"`
	// +optional
	MaxPayloadKg *float64 `json:"maxPayloadKg,omitempty"`
	// +optional
	ResolutionMp *float64 `json:"resolutionMp,omitempty"`
	// RangeM is the sensing range in metres (lidar/range sensors).
	// +optional
	RangeM *float64 `json:"rangeM,omitempty"`
	// HorizontalFovDeg is the horizontal field of view in degrees.
	// +optional
	HorizontalFovDeg *float64 `json:"horizontalFovDeg,omitempty"`
	// DepthCapable indicates a depth-capable camera.
	// +optional
	DepthCapable *bool `json:"depthCapable,omitempty"`
	// FrameRateFps is the sensor/camera frame rate in frames per second.
	// +optional
	FrameRateFps *float64 `json:"frameRateFps,omitempty"`
	// PlatformLengthMm is the load-platform length in millimetres.
	// +optional
	PlatformLengthMm *float64 `json:"platformLengthMm,omitempty"`
	// PlatformWidthMm is the load-platform width in millimetres.
	// +optional
	PlatformWidthMm *float64 `json:"platformWidthMm,omitempty"`
}

// DiscoveredModel is an AI model reported at discovery time.
type DiscoveredModel struct {
	Name               string      `json:"name"`
	Version            string      `json:"version"`
	Status             ModelStatus `json:"status"`
	GrantsCapabilities []string    `json:"grantsCapabilities,omitempty"`
}

// DiscoveredRobotStatus defines the observed state of a DiscoveredRobot.
// DiscoveredRobot.spec is always empty — this resource is fully read-only.
type DiscoveredRobotStatus struct {
	// Phase is the current phase of the discovered robot.
	Phase DiscoveredRobotPhase `json:"phase"`

	// RobotID is the stable robot identifier the Fleet Adapter announced
	// (DiscoverRobot.robot_id); the identity key for admission, never empty.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	RobotID string `json:"robotId"`

	// ConnectedAt is the time the Fleet Adapter called Discover.
	ConnectedAt metav1.Time `json:"connectedAt"`

	// AdapterAddress is the gRPC address of the Fleet Adapter.
	AdapterAddress string `json:"adapterAddress"`

	// AdapterVersion is the Fleet Adapter version string.
	AdapterVersion string `json:"adapterVersion"`

	// Manufacturer as reported by the Fleet Adapter.
	Manufacturer string `json:"manufacturer"`

	// Model as reported by the Fleet Adapter.
	Model string `json:"model"`

	// FirmwareVersion as reported by the Fleet Adapter.
	// +optional
	FirmwareVersion string `json:"firmwareVersion,omitempty"`

	// MacAddress is the robot's hardware MAC address, when reported at discovery
	// (the projection of DiscoverRobot.mac). Empty when the robot exposes none.
	// +optional
	MacAddress string `json:"macAddress,omitempty"`

	// RobotClass suggested by the Fleet Adapter (may be empty).
	// +optional
	SuggestedRobotClass string `json:"suggestedRobotClass,omitempty"`

	// ReportedHardware is the hardware inventory reported at discovery time.
	// +optional
	ReportedHardware []DiscoveredHardwareComponent `json:"reportedHardware,omitempty"`

	// ReportedModels is the installed model list reported at discovery time.
	// +optional
	ReportedModels []DiscoveredModel `json:"reportedModels,omitempty"`

	// ReportedCapabilities is the initial capability set reported at discovery.
	// +optional
	ReportedCapabilities []string `json:"reportedCapabilities,omitempty"`

	// LastAnnouncedAt is the most recent time the Fleet Adapter re-announced this
	// robot (the discovery-TTL liveness refresh); TTLExpiresAt is derived from it.
	// +optional
	LastAnnouncedAt *metav1.Time `json:"lastAnnouncedAt,omitempty"`

	// TTLExpiresAt is the time at which this resource will be auto-deleted
	// if the robot has not been admitted or rejected.
	// +optional
	TTLExpiresAt *metav1.Time `json:"ttlExpiresAt,omitempty"`
}

// DiscoveredRobotSpec is intentionally empty. DiscoveredRobot is a read-only,
// status-only resource: the control plane creates it on Discover, and operators
// admit it via `swarmctl admit`, never by editing spec.
type DiscoveredRobotSpec struct{}

// DiscoveredRobot is the Schema for the discoveredrobots API.
//
// A DiscoveredRobot is created automatically when a Fleet Adapter calls the
// Discover RPC for a robot with no existing Robot CRD in the namespace.
// Operators review DiscoveredRobots and admit them with:
//
//	swarmctl admit robot <name> --zone <leaf-zone>
//
// DiscoveredRobot.spec is always empty — this is a read-only resource.
// Operators CANNOT edit it; admission is exclusively via swarmctl admit.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=dr
// +kubebuilder:printcolumn:name="Manufacturer",type="string",JSONPath=".status.manufacturer"
// +kubebuilder:printcolumn:name="Model",type="string",JSONPath=".status.model"
// +kubebuilder:printcolumn:name="Firmware",type="string",JSONPath=".status.firmwareVersion"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Connected",type="date",JSONPath=".status.connectedAt"
// +kubebuilder:printcolumn:name="TTL",type="date",JSONPath=".status.ttlExpiresAt"
type DiscoveredRobot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DiscoveredRobotSpec   `json:"spec,omitempty"`
	Status DiscoveredRobotStatus `json:"status,omitempty"`
}

// DiscoveredRobotList contains a list of DiscoveredRobot resources.
// +kubebuilder:object:root=true
type DiscoveredRobotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DiscoveredRobot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DiscoveredRobot{}, &DiscoveredRobotList{})
}
