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

// ProbeType identifies the target of an active health check.
// +kubebuilder:validation:Enum=hardware;capability;model
type ProbeType string

// ProbeType values.
const (
	ProbeTypeHardware   ProbeType = "hardware"
	ProbeTypeCapability ProbeType = "capability"
	ProbeTypeModel      ProbeType = "model"
)

// RobotProbeSpec defines an active health check targeting a set of robots.
// The Zone Controller calls the VerifyHardware Fleet Adapter RPC at
// spec.intervalSeconds to actively confirm hardware, capability, or model
// health, rather than relying on passive telemetry alone.
type RobotProbeSpec struct {
	// RobotSelector selects the robots this probe verifies.
	RobotSelector metav1.LabelSelector `json:"robotSelector"`

	// ProbeType is the target class of the verification (hardware/capability/model).
	ProbeType ProbeType `json:"probeType"`

	// TargetComponent names the specific component/capability/model to verify.
	// +optional
	TargetComponent string `json:"targetComponent,omitempty"`

	// TargetCapability names the capability to verify when ProbeType is capability.
	// +optional
	TargetCapability string `json:"targetCapability,omitempty"`

	// TargetModel names the model to verify when ProbeType is model.
	// +optional
	TargetModel string `json:"targetModel,omitempty"`

	// SyntheticInput is an optional test input for a model probe (VerifyModel),
	// carried to the adapter as VerifyModel.synthetic_input.
	// +optional
	SyntheticInput []byte `json:"syntheticInput,omitempty"`

	// IntervalSeconds is how often the probe runs.
	// +optional
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=60
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the per-probe RPC timeout.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=60
	// +kubebuilder:default=10
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is the consecutive failing cycles before the debounced
	// status.lastProbeResult flips to Degraded/Failed. Optional: unset (nil) means
	// fall back to SwarmadaConfig.spec.health.defaultProbeFailureThreshold, else the
	// built-in default of 3 (ADR-0012).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`

	// RecoveryThreshold is the consecutive Healthy cycles before the debounced
	// status.lastProbeResult returns to Healthy. Optional: unset (nil) means fall
	// back to SwarmadaConfig.spec.health.defaultProbeRecoveryThreshold, else the
	// built-in default of 2 (ADR-0012).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	RecoveryThreshold *int32 `json:"recoveryThreshold,omitempty"`

	// ExpectedMetrics are threshold key/value pairs the VerifyHardware result is
	// checked against (e.g. {"min_frame_rate_pct": "80"}).
	// +optional
	ExpectedMetrics map[string]string `json:"expectedMetrics,omitempty"`
}

// ProbeResult is a probe outcome value (§9.1.6).
// +kubebuilder:validation:Enum=Healthy;Degraded;Failed;Pending;Unknown
type ProbeResult string

// ProbeResult values.
const (
	// ProbeResultHealthy: expected metrics met and recovery threshold satisfied.
	ProbeResultHealthy ProbeResult = "Healthy"
	// ProbeResultDegraded: failing threshold reached; component marked Degraded.
	ProbeResultDegraded ProbeResult = "Degraded"
	// ProbeResultFailed: the Fleet Adapter returned an explicit probe failure.
	ProbeResultFailed ProbeResult = "Failed"
	// ProbeResultPending: probe not yet issued since the robot matched the selector.
	ProbeResultPending ProbeResult = "Pending"
	// ProbeResultUnknown: the last probe timed out or was unsupported.
	ProbeResultUnknown ProbeResult = "Unknown"
)

// RobotProbeRobotResult is the per-robot outcome of a probe cycle.
type RobotProbeRobotResult struct {
	// RobotName is the Robot this result is for.
	RobotName string `json:"robotName"`
	// Namespace is the Robot's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// ProbeStatus is the last observed probe status for this robot.
	ProbeStatus ProbeResult `json:"probeStatus"`
	// ConsecutiveFailures is this robot's consecutive failing-cycle count, reset to
	// 0 on a Healthy cycle and clamped at the effective failureThreshold.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`
	// ConsecutiveSuccesses is this robot's consecutive Healthy-cycle count, reset to
	// 0 on a failing cycle and clamped at the effective recoveryThreshold.
	// +optional
	ConsecutiveSuccesses int32 `json:"consecutiveSuccesses,omitempty"`
	// FailedAt is when this robot's current failure run began (nil when not failing).
	// +optional
	FailedAt *metav1.Time `json:"failedAt,omitempty"`
	// LastProbeTime is when this robot was last probed.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// Message is a human-readable summary, especially on failure.
	// +optional
	Message string `json:"message,omitempty"`
	// ActualMetrics are the metric values the Fleet Adapter returned on the last probe.
	// +optional
	ActualMetrics map[string]float64 `json:"actualMetrics,omitempty"`
}

// RobotProbeStatus defines the observed state of a RobotProbe.
type RobotProbeStatus struct {
	// LastProbeTime is when the probe last ran.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// LastProbeResult is the aggregate result of the most recent cycle.
	// +optional
	LastProbeResult ProbeResult `json:"lastProbeResult,omitempty"`
	// ConsecutiveFailures is the current consecutive failing-cycle count, clamped at
	// the effective failureThreshold (ADR-0012) so a steady failing probe does not
	// churn status every cycle.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`
	// ConsecutiveSuccesses is the current consecutive Healthy-cycle count, clamped at
	// the effective recoveryThreshold (ADR-0012). Together with ConsecutiveFailures
	// it debounces lastProbeResult.
	// +optional
	ConsecutiveSuccesses int32 `json:"consecutiveSuccesses,omitempty"`
	// RobotResults is the per-robot outcome of the most recent cycle.
	// +optional
	RobotResults []RobotProbeRobotResult `json:"robotResults,omitempty"`
}

// RobotProbe is the Schema for the robotprobes API.
//
// RobotProbes add active verification on top of passive telemetry. The Zone
// Controller calls the VerifyHardware Fleet Adapter RPC at spec.intervalSeconds
// to confirm the targeted hardware, capability, or model is functioning within
// spec.expectedMetrics thresholds. This is distinct from Kubernetes-style
// liveness probing: it verifies robot subsystems via the Fleet Adapter, not an
// HTTP endpoint.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rp
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.probeType"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.targetComponent"
// +kubebuilder:printcolumn:name="Result",type="string",JSONPath=".status.lastProbeResult"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type RobotProbe struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RobotProbeSpec   `json:"spec,omitempty"`
	Status            RobotProbeStatus `json:"status,omitempty"`
}

// RobotProbeList contains a list of RobotProbe resources.
// +kubebuilder:object:root=true
type RobotProbeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RobotProbe `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RobotProbe{}, &RobotProbeList{})
}
