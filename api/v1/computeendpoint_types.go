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

// ValueKind is the scalar type of a declared parameter or result value.
// +kubebuilder:validation:Enum=string;number;bool;enum
type ValueKind string

// ValueKind values.
const (
	ValueKindString ValueKind = "string"
	ValueKindNumber ValueKind = "number"
	ValueKindBool   ValueKind = "bool"
	ValueKindEnum   ValueKind = "enum"
)

// ComputeEndpointInputKind classifies a non-scalar input a ComputeEndpoint
// accepts. The closed set is declared for forward compatibility. No code reads
// a ComputeEndpoint's spec.inputs at v0.4 and nothing delivers either kind to an
// endpoint, so spec.inputs is descriptive only.
// +kubebuilder:validation:Enum=SensorStream;Artifact
type ComputeEndpointInputKind string

// ComputeEndpointInputKind values.
const (
	ComputeEndpointInputSensorStream ComputeEndpointInputKind = "SensorStream"
	ComputeEndpointInputArtifact     ComputeEndpointInputKind = "Artifact"
)

// ── Spec ──────────────────────────────────────────────────────────────────────

// ComputeEndpointSpec is an administrator-installed reference to an addressable
// executor: a binding, a provisioning mode, and a contract. A task
// never describes an executor inline — it names a ComputeEndpoint by reference
// (FleetAction.spec.executor.endpointRef).
//
// +kubebuilder:validation:XValidation:rule="self.contract != 'Service' || self.binding == 'Grpc'",message="contract: Service requires binding: Grpc — it is the only binding that carries a sensor stream; this also rejects binding: Mcp with contract: Service"
// +kubebuilder:validation:XValidation:rule="self.contract != 'Service' || (has(self.streamPolicy) && self.streamPolicy.pinEndpoint)",message="contract: Service requires streamPolicy.pinEndpoint: true — splitting a stream across replicas silently degrades tracking, motion estimates and pre-roll"
type ComputeEndpointSpec struct {
	// Binding is the wire contract this endpoint speaks.
	// There is no dispatch path at v0.4. Nothing in the reference control plane
	// resolves an endpoint address, activates a run, or calls the compute runtime service.
	Binding ExecutorBinding `json:"binding"`

	// Contract is the execution shape this endpoint implements. Besides the
	// CEL rules on this type, the FleetTask admission webhook reads it when a task
	// is created and when an update changes the task's spec.flow, spec.inputs or
	// spec.actions: a binding that reads an event from an App producer, or a
	// result from a Service producer, is refused with reason ExternalContractMismatch.
	Contract ExecutorContract `json:"contract"`

	// Provision is how this endpoint's address is obtained. The v0.4 schema
	// declares `existing` and `workload` only; `address` and `onRobot` are not
	// in it. No code reads this field at v0.4: there is no dispatch path.
	Provision ComputeEndpointProvision `json:"provision"`

	// Parameters are the scalar parameters this endpoint accepts, keyed by name
	// in an action's spec.executor.parameters.
	// +optional
	// +listType=map
	// +listMapKey=name
	Parameters []ComputeEndpointParameter `json:"parameters,omitempty"`

	// Results are the scalar fields this endpoint's ActionResult.values may carry.
	// Each declares a name and a kind only: a result declares no value domain (no
	// allowed values, bounds or unit), so a result of kind enum names no members and
	// nothing constrains the value an endpoint returns. Only the field names are
	// checked, at admission, against the ResultSinks an action names.
	// +optional
	// +listType=map
	// +listMapKey=name
	Results []ComputeEndpointResultField `json:"results,omitempty"`

	// Inputs are the non-scalar inputs this endpoint accepts (a sensor stream or
	// an artifact). Descriptive only at v0.4 — see ComputeEndpointInputKind.
	// +optional
	// +listType=map
	// +listMapKey=name
	Inputs []ComputeEndpointInput `json:"inputs,omitempty"`

	// EmitsEvents lists the semantic event types this endpoint may raise. The
	// FleetTask admission webhook reads it on the same writes as Contract: an
	// event binding naming a type not listed here is refused with reason
	// UndeclaredEvent, including when the list is empty. Nothing checks a raised
	// event against it.
	// +optional
	// +listType=set
	EmitsEvents []string `json:"emitsEvents,omitempty"`

	// Probe is specified as an active liveness check for an endpoint whose
	// readiness is not otherwise observable (an Http or Mcp endpoint), modelled on
	// RobotProbeSpec. No code reads it at v0.4: nothing probes the endpoint, and
	// this kind has no status and no Ready condition.
	// +optional
	Probe *ComputeEndpointProbeSpec `json:"probe,omitempty"`

	// StreamPolicy governs a contract: Service endpoint's stream placement.
	// REQUIRED (with PinEndpoint: true) when Contract is Service — see the
	// struct-level CEL rule above. Meaningless, and left unset, for an App
	// contract.
	// +optional
	StreamPolicy *StreamPolicy `json:"streamPolicy,omitempty"`
}

// StreamPolicy governs how a contract: Service endpoint's stream is placed
// across the backing Service's replicas.
type StreamPolicy struct {
	// PinEndpoint, when true, requires the control plane to resolve ONE
	// EndpointSlice address at activation and pin the stream to it for the
	// run's lifetime, rather than handing an adapter the Service VIP — which
	// would load-balance per connection/request and silently split the stream
	// across replicas. REQUIRED true for contract: Service (CEL, above); the
	// field exists (rather than being implied) so a reader sees the invariant
	// stated on the object, not only enforced by it.
	PinEndpoint bool `json:"pinEndpoint"`

	// OnEndpointLoss selects what happens when the pinned endpoint is lost.
	// Replace re-places the run onto a newly resolved endpoint and raises a
	// semantic event marking the temporal discontinuity — a
	// consumer sees the discontinuity rather than inferring it. Fail ends the
	// run instead of re-placing it.
	// +kubebuilder:default=Replace
	// +optional
	OnEndpointLoss StreamEndpointLossPolicy `json:"onEndpointLoss,omitempty"`
}

// StreamEndpointLossPolicy selects what happens when a pinned stream endpoint
// is lost.
// +kubebuilder:validation:Enum=Replace;Fail
type StreamEndpointLossPolicy string

// StreamEndpointLossPolicy values.
const (
	StreamEndpointLossReplace StreamEndpointLossPolicy = "Replace"
	StreamEndpointLossFail    StreamEndpointLossPolicy = "Fail"
)

// ComputeEndpointProvision is how a ComputeEndpoint's address is obtained.
// Exactly one mode is populated. The schema declares Existing and Workload
// and no other mode: there is no Address or OnRobot mode. Nothing reads
// either mode at v0.4.
//
// +kubebuilder:validation:XValidation:rule="has(self.existing) != has(self.workload)",message="provision must set exactly one of existing or workload; there is no other mode"
type ComputeEndpointProvision struct {
	// Existing references a pre-created Kubernetes Service in a permitted
	// namespace. Swarmada creates nothing for this mode.
	// +optional
	Existing *ExistingServiceProvision `json:"existing,omitempty"`

	// Workload specifies that the controller creates a Job or Deployment+Service
	// per run, from an admin-installed ComputeWorkloadTemplate, owned by
	// ownerReferences so garbage collection is the duty cycle. Specified, not
	// implemented at v0.4: no controller creates a workload. This is the
	// Extended conformance profile: a
	// deployment that grants no workload-create RBAC and never sets this field
	// still conforms fully at Core.
	// +optional
	Workload *WorkloadProvision `json:"workload,omitempty"`
}

// WorkloadProvision names an admin-installed ComputeWorkloadTemplate. There is
// no field here for a pod spec, an image, or a command — only a reference by
// name to an object an administrator created — the first containment rule.
type WorkloadProvision struct {
	// TemplateRef names a ComputeWorkloadTemplate in this namespace.
	// +kubebuilder:validation:MinLength=1
	TemplateRef string `json:"templateRef"`

	// Mode selects whether the controller creates a Job (RunToCompletion — App
	// contract) or a Deployment+Service (Continuous — Service contract).
	Mode WorkloadTemplateMode `json:"mode"`
}

// WorkloadTemplateMode selects the Kubernetes object provision: workload
// creates for one run.
// +kubebuilder:validation:Enum=Job;Deployment
type WorkloadTemplateMode string

// WorkloadTemplateMode values.
const (
	WorkloadTemplateModeJob        WorkloadTemplateMode = "Job"
	WorkloadTemplateModeDeployment WorkloadTemplateMode = "Deployment"
)

// ExistingServiceProvision names a pre-created Kubernetes Service.
type ExistingServiceProvision struct {
	// Service is the Service name.
	// +kubebuilder:validation:MinLength=1
	Service string `json:"service"`

	// Namespace is the Service's namespace. Empty means the ComputeEndpoint's own
	// namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Port is the named port on the Service to dial.
	// +kubebuilder:validation:MinLength=1
	Port string `json:"port"`
}

// ComputeEndpointParameter declares one scalar parameter a ComputeEndpoint accepts.
type ComputeEndpointParameter struct {
	// Name is the parameter name.
	Name string `json:"name"`

	// Kind is the parameter's scalar type.
	Kind ValueKind `json:"kind"`

	// Allowed enumerates permitted values when Kind is enum.
	// +optional
	// +listType=set
	Allowed []string `json:"allowed,omitempty"`

	// Required marks whether a binding (or Default) must resolve this parameter.
	// +optional
	Required bool `json:"required,omitempty"`

	// Default is a string-encoded default value. It is stored only: no controller
	// applies it at v0.4, and spec.executor.onUnresolvedParameter, which names the
	// case it is for, is itself read by no controller.
	// +optional
	Default *string `json:"default,omitempty"`

	// Min is the minimum permitted value when Kind is number.
	// +optional
	Min *float64 `json:"min,omitempty"`

	// Max is the maximum permitted value when Kind is number.
	// +optional
	Max *float64 `json:"max,omitempty"`

	// Unit is a free-form, non-normative unit label: warn-only until a unit
	// vocabulary exists.
	// +optional
	Unit string `json:"unit,omitempty"`
}

// ComputeEndpointResultField declares one scalar field a ComputeEndpoint's
// ActionResult.values may carry: its name and kind, and no value domain.
type ComputeEndpointResultField struct {
	// Name is the result field name.
	Name string `json:"name"`

	// Kind is the field's scalar type. For enum it names no members: nothing
	// constrains the value returned.
	Kind ValueKind `json:"kind"`
}

// ComputeEndpointInput declares one non-scalar input a ComputeEndpoint accepts.
type ComputeEndpointInput struct {
	// Name is the input name.
	Name string `json:"name"`

	// Kind classifies the input.
	Kind ComputeEndpointInputKind `json:"kind"`

	// MediaTypes lists acceptable IANA media types (e.g. "video/mp4").
	// +optional
	// +listType=set
	MediaTypes []string `json:"mediaTypes,omitempty"`

	// Required marks whether an action invoking this endpoint must supply this input.
	// +optional
	Required bool `json:"required,omitempty"`
}

// ComputeEndpointProbeSpec is specified as an active liveness check, modelled on
// RobotProbeSpec. No code reads it at v0.4.
type ComputeEndpointProbeSpec struct {
	// Path is the HTTP path the probe is specified to check (e.g. "/healthz").
	// For an Mcp binding the probe is specified as a tools/list call and Path is
	// ignored.
	// +optional
	Path string `json:"path,omitempty"`

	// IntervalSeconds is how often the probe is specified to run.
	// +optional
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=30
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// FailureThreshold is the number of consecutive failing cycles specified
	// before the endpoint is reported not ready.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=3
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// ComputeEndpoint is the Schema for the computeendpoints API.
//
// NO STATUS SUBRESOURCE, DELIBERATELY. ComputeEndpoint is reference-only
// configuration, in the same shape as StorageClass or IngressClass: an administrator
// installs it and a FleetAction names it in spec.executor.endpointRef. No controller
// reconciles it, so there is nothing it could truthfully report.
//
// There is no dispatch path at this version: nothing resolves the endpoint's
// address, starts a run on it, or observes the executor behind it, and that
// executor's readiness is reported nowhere, on this object or on a FleetAction. The
// control plane reads it at admission only. The FleetAction admission webhook
// refuses a FleetAction whose spec.executor.endpointRef names no ComputeEndpoint in
// its namespace, or whose sinks do not accept the result fields this endpoint
// declares. The FleetTask admission webhook checks bindings against spec.contract
// and spec.emitsEvents when a task is created or its bindings change.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=cep
// +kubebuilder:printcolumn:name="Binding",type=string,JSONPath=".spec.binding"
// +kubebuilder:printcolumn:name="Contract",type=string,JSONPath=".spec.contract"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type ComputeEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ComputeEndpointSpec `json:"spec,omitempty"`
}

// ComputeEndpointList contains a list of ComputeEndpoint.
// +kubebuilder:object:root=true
type ComputeEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ComputeEndpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ComputeEndpoint{}, &ComputeEndpointList{})
}
