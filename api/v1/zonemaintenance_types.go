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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ZoneMaintenanceMode controls how robots are paused.
// +kubebuilder:validation:Enum=Graceful;Immediate
type ZoneMaintenanceMode string

const (
	// ZoneMaintenanceModeGraceful — no new actions assigned; robots finish current actions then pause.
	ZoneMaintenanceModeGraceful ZoneMaintenanceMode = "Graceful"
	// ZoneMaintenanceModeImmediate — robots pause now; in-progress actions are requeued as Pending.
	ZoneMaintenanceModeImmediate ZoneMaintenanceMode = "Immediate"
)

// ZoneMaintenancePhase is the lifecycle phase of a ZoneMaintenance resource.
// +kubebuilder:validation:Enum=Scheduled;Active;Completed;Cancelled
type ZoneMaintenancePhase string

// ZoneMaintenancePhase values (§9.1.11): a window is Scheduled until its start, Active while robots
// are wound down and held, and Completed once the zone resumes.
const (
	ZoneMaintenancePhaseScheduled ZoneMaintenancePhase = "Scheduled"
	ZoneMaintenancePhaseActive    ZoneMaintenancePhase = "Active"
	ZoneMaintenancePhaseCompleted ZoneMaintenancePhase = "Completed"
	ZoneMaintenancePhaseCancelled ZoneMaintenancePhase = "Cancelled"
)

// MaintenanceScopeType selects what a ZoneMaintenance applies to (§9.1.10).
// +kubebuilder:validation:Enum=Zone;Namespace
type MaintenanceScopeType string

// MaintenanceScopeType values.
const (
	// MaintenanceScopeZone applies to a named FleetZone and its descendants.
	MaintenanceScopeZone MaintenanceScopeType = "Zone"
	// MaintenanceScopeNamespace applies to all robots in the namespace.
	MaintenanceScopeNamespace MaintenanceScopeType = "Namespace"
)

// ZoneMaintenanceScope defines the target of a maintenance event.
type ZoneMaintenanceScope struct {
	// Type selects Zone- or Namespace-scoped maintenance.
	// +kubebuilder:validation:Required
	Type MaintenanceScopeType `json:"type"`
	// ZoneName is the target FleetZone (leaf or parent). Required when Type is
	// Zone; ignored when Type is Namespace.
	// +optional
	ZoneName string `json:"zoneName,omitempty"`
}

// WindingDownRobot describes a robot still finishing its current action
// during Graceful mode wind-down.
type WindingDownRobot struct {
	Name                  string       `json:"name"`
	AssignedAction        string       `json:"assignedAction"`
	EstimatedCompletionAt *metav1.Time `json:"estimatedCompletionAt,omitempty"`
}

// PausedRobotEntry records when a robot entered Maintenance phase.
type PausedRobotEntry struct {
	Name string `json:"name"`
	// Namespace is the paused robot's namespace.
	// +optional
	Namespace string      `json:"namespace,omitempty"`
	PausedAt  metav1.Time `json:"pausedAt"`
}

// ContinuousCapabilityEntry records non-pauseable capabilities still running.
type ContinuousCapabilityEntry struct {
	RobotName    string   `json:"robotName"`
	Capabilities []string `json:"capabilities"`
}

// ZoneMaintenanceSpec defines the desired state of a ZoneMaintenance resource.
type ZoneMaintenanceSpec struct {
	// Scope identifies the target zone or namespace.
	// +kubebuilder:validation:Required
	Scope ZoneMaintenanceScope `json:"scope"`

	// Mode controls how robots are paused.
	// +kubebuilder:default=Graceful
	Mode ZoneMaintenanceMode `json:"mode,omitempty"`

	// Reason is a human-readable description of why maintenance is occurring.
	// Included in operator notifications and audit logs.
	// +optional
	Reason string `json:"reason,omitempty"`

	// ScheduledStart is the time at which this maintenance should activate.
	// Empty means activate immediately on resource creation.
	// +optional
	ScheduledStart *metav1.Time `json:"scheduledStart,omitempty"`

	// AutoResumeAfterMinutes: if non-zero, the maintenance automatically
	// deactivates this many minutes after activation (ZoneMaintenance deleted).
	// 0 means the operator must manually resume.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1440
	// +kubebuilder:default=0
	AutoResumeAfterMinutes int32 `json:"autoResumeAfterMinutes,omitempty"`

	// RequireEstopClearBeforeResume, when true, holds a paused robot's
	// administrative resume (Maintenance→Idle) until its observed estop is Clear.
	// This is an OPERATIONAL gate on the phase transition only — it never stops a
	// robot and is separate from the hardware estop path. Defaults from
	// SwarmadaConfig.spec.maintenance.requireEstopClearBeforeResume. Deletion-driven
	// resume is never gated.
	// +optional
	// +kubebuilder:default=true
	RequireEstopClearBeforeResume *bool `json:"requireEstopClearBeforeResume,omitempty"`
}

// ZoneMaintenanceStatus defines the observed state of a ZoneMaintenance resource.
type ZoneMaintenanceStatus struct {
	Phase       ZoneMaintenancePhase `json:"phase,omitempty"`
	ActivatedAt *metav1.Time         `json:"activatedAt,omitempty"`
	ActivatedBy string               `json:"activatedBy,omitempty"`

	// CompletedAt is when the window closed, stamped once on the transition into
	// Completed and never rewritten.
	//
	// It is the window's own record of when robots came back into service. The
	// Completed condition's lastTransitionTime is close but not equivalent: conditions
	// are rewritten by later reconciles and are subject to their own retention, so a
	// duration computed from them silently drifts. Empty while Scheduled or Active.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	AutoResumeAt *metav1.Time `json:"autoResumeAt,omitempty"`

	// PausedRobotsCount is a print-column convenience count of PausedRobots.
	// +optional
	PausedRobotsCount int32 `json:"pausedRobotsCount,omitempty"`

	// WindingDownRobotsCount is a print-column convenience count of WindingDownRobots.
	// +optional
	WindingDownRobotsCount int32 `json:"windingDownRobotsCount,omitempty"`

	// PausedRobots lists robots that have entered Maintenance phase.
	// +optional
	PausedRobots []PausedRobotEntry `json:"pausedRobots,omitempty"`

	// WindingDownRobots lists robots still finishing actions (Graceful mode only).
	// +optional
	WindingDownRobots []WindingDownRobot `json:"windingDownRobots,omitempty"`

	// ContinuousCapabilities lists non-pauseable capabilities still running
	// on paused robots. These capabilities are NOT suspended by this maintenance.
	// +optional
	ContinuousCapabilities []ContinuousCapabilityEntry `json:"continuousCapabilities,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ZoneMaintenance is the Schema for the zonemaintenances API.
//
// ZoneMaintenance represents a planned maintenance event for a zone or namespace.
// Creating this resource pauses robot activity in the targeted area.
// Deleting it (or running swarmctl deactivate maintenance) resumes all robots.
//
// ZoneMaintenance is distinct from estop:
//   - Estop: safety-critical, immediate, requires per-robot acknowledgment.
//   - ZoneMaintenance: planned operational maintenance, graceful or immediate,
//     bulk pause/resume, operator-initiated.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=zm
// +kubebuilder:printcolumn:name="Scope",type="string",JSONPath=".spec.scope.zoneName"
// +kubebuilder:printcolumn:name="Mode",type="string",JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ZoneMaintenance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZoneMaintenanceSpec   `json:"spec,omitempty"`
	Status ZoneMaintenanceStatus `json:"status,omitempty"`
}

// ZoneMaintenanceList contains a list of ZoneMaintenance resources.
// +kubebuilder:object:root=true
type ZoneMaintenanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZoneMaintenance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZoneMaintenance{}, &ZoneMaintenanceList{})
}
