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

// FleetAdapter declares a vendor Fleet Adapter — the per-manufacturer translation
// layer (RFC-0001 #fleet-adapter-protocol) that the control plane connects to over gRPC
// (fleet_adapter.v1.FleetAdapterService). It is the admission keystone for
// third-party robots: a Robot may only be admitted when a Connected, conformant
// FleetAdapter serves its RobotClass, so an unverified or absent adapter cannot
// silently drive physical hardware.
//
// NOTE (RFC-0001 #fleetadapter): the admission-gating webhook that consumes this type
// is implemented (internal/webhook/robot_webhook.go, RobotAdmissionGate). The
// FleetAdapter status controller that sets status.phase/status.conformance is also
// implemented (internal/controller/fleetadapter_controller.go: digest- and
// signature-verified conformance report consumption per RFC-0001 #fleetadapter-status-fields-and-phase-machine).
// The RFC-0001 #fleet-adapter-protocol conformance suite (adapters/conformance/) is an
// executable harness that drives checks C0 through C19 (adapters/CONFORMANCE.md).

// ── Enumerations ──────────────────────────────────────────────────────────────

// FleetAdapterPhase is the connection/health state of a FleetAdapter.
// +kubebuilder:validation:Enum=Pending;Connected;Degraded;Disconnected;Rejected
type FleetAdapterPhase string

// FleetAdapterPhase values.
const (
	// FleetAdapterPhasePending means the control plane has not yet established a
	// session with the adapter.
	FleetAdapterPhasePending FleetAdapterPhase = "Pending"
	// FleetAdapterPhaseConnected means the adapter has an established session (an
	// accepted, verified handshake) and a current heartbeat. It says nothing about
	// conformance, which status.conformance carries: Robot admission requires both
	// Connected and conformance Passed.
	FleetAdapterPhaseConnected FleetAdapterPhase = "Connected"
	// FleetAdapterPhaseDegraded means the adapter is reachable but failing
	// liveness probes or driving only a subset of its robots.
	FleetAdapterPhaseDegraded FleetAdapterPhase = "Degraded"
	// FleetAdapterPhaseDisconnected means the adapter is currently unreachable.
	FleetAdapterPhaseDisconnected FleetAdapterPhase = "Disconnected"
	// FleetAdapterPhaseRejected means the adapter may not drive robots: conformance failed, the
	// protocol version could not be negotiated, or the negotiated CONTRACT version is outside the
	// range this control plane supports (ADR-0032).
	//
	// It bars WORK, not observation: a Rejected adapter always receives estop. One rejected on its
	// contract version or on a failed conformance report keeps its session and keeps streaming
	// telemetry and heartbeats; one whose ControlStream is refused on its package version sends
	// neither, and its SafetyStream stays open (RFC-0001 #fleetadapter-admission-gating). Because
	// it is a negotiation verdict rather than a liveness one, a heartbeat does not clear it — only a
	// fresh, compatible handshake does.
	FleetAdapterPhaseRejected FleetAdapterPhase = "Rejected"
)

// ConformanceState is the result of the last Fleet Adapter conformance check
// (RFC-0001 #fleet-adapter-protocol).
// +kubebuilder:validation:Enum=Unknown;Passed;Failed
type ConformanceState string

// ConformanceState values.
const (
	ConformanceStateUnknown ConformanceState = "Unknown"
	ConformanceStatePassed  ConformanceState = "Passed"
	ConformanceStateFailed  ConformanceState = "Failed"
)

// ── Spec ──────────────────────────────────────────────────────────────────────

// FleetAdapterSpec defines the desired adapter connection.
type FleetAdapterSpec struct {
	// Vendor is the robot manufacturer or adapter provider (e.g. "acme-robotics").
	Vendor string `json:"vendor"`

	// Endpoint is the gRPC address (host:port) the adapter is expected to be
	// reachable at, for operator reference. Informational only: the control plane
	// never dials it — the ControlStream is adapter-initiated (RFC-0001 §9.2), so
	// nothing in the reconciler reads this field.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ProtocolVersion is the Fleet Adapter protocol version this adapter
	// implements. Not consulted by any controller at v0.4: the package check runs
	// on the version the adapter advertises in AdapterHello, where an empty value
	// or "fleet_adapter.v1" is accepted and any other value is refused and the
	// stream closed, without moving this FleetAdapter to Rejected.
	// +kubebuilder:default="fleet_adapter.v1"
	ProtocolVersion string `json:"protocolVersion,omitempty"`

	// ServesRobotClasses lists the RobotClass names this adapter can drive. A
	// Robot referencing one of these classes is admitted only if this adapter is
	// Connected and conformant. Empty means the adapter serves any class —
	// discouraged outside development.
	// +optional
	ServesRobotClasses []string `json:"servesRobotClasses,omitempty"`

	// TLSSecretRef names a Secret in this namespace specified to pin the adapter's
	// expected client-certificate identity. Read by no controller at v0.4: adapter
	// identity is resolved from the client certificate's SAN alone, so this field
	// pins nothing (RFC-0001 #fleetadapter-spec-fields).
	// +optional
	TLSSecretRef string `json:"tlsSecretRef,omitempty"`

	// ConformanceReport references the signed result proving this adapter passes
	// the RFC-0001 #fleet-adapter-protocol conformance suite. Its verification sets
	// status.conformance, which admission and dispatch require to be Passed. It does
	// not gate status.phase at v0.4: an adapter with no report, or a failed one,
	// still reaches Connected.
	// +optional
	ConformanceReport *ConformanceReportRef `json:"conformanceReport,omitempty"`

	// HeartbeatIntervalSeconds is the adapter's liveness staleness budget: an adapter
	// silent for this long is Degraded, and silent for this long times the missed-probe
	// threshold is Disconnected. The control plane pushes an adapter-scoped
	// HeartbeatRequest at HALF this period so a live adapter is never judged on its own
	// boundary (ADR-0054); it is the floor under ADR-0026's liveness projection, not
	// only a timeout.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	HeartbeatIntervalSeconds int32 `json:"heartbeatIntervalSeconds,omitempty"`

	// Simulated declares that this adapter drives simulated robots rather than
	// physical hardware (ADR-0038). Purely informational: it never gates admission
	// or dispatch, and RobotAdmissionGate treats a simulated adapter exactly as it
	// treats a physical one. The Robot defaulting webhook reads it to stamp
	// swarmada.io/robot-source onto each Robot bound to this adapter, so a
	// conformance report earned against a simulated binding is distinguishable from
	// one earned against hardware.
	//
	// false (the default) means physical: an adapter that does not declare itself is
	// assumed physical, which is exactly what an undeclared adapter already means.
	// +optional
	// +kubebuilder:default=false
	Simulated bool `json:"simulated,omitempty"`
}

// FleetAdapter status condition types and reasons.
const (
	// ConditionSpecFieldsIgnored is True when the FleetAdapter sets a spec field that no
	// controller acts on in this release: spec.tlsSecretRef, or a spec.protocolVersion
	// other than the protocol the control plane speaks. The message names each one.
	// Implementing a field removes it from this condition.
	ConditionSpecFieldsIgnored = "SpecFieldsIgnored"
	// ReasonIgnoredFieldsSet is the reason when at least one such field is set.
	ReasonIgnoredFieldsSet = "IgnoredFieldsSet"
	// ReasonNoIgnoredFields is the reason when none is set.
	ReasonNoIgnoredFields = "NoIgnoredFields"
)

// RobotSource is the provenance of the robots an adapter drives, stamped onto a
// Robot as the swarmada.io/robot-source annotation by the Robot defaulting webhook
// (ADR-0038). An ABSENT annotation means unknown — the bound FleetAdapter could not
// be resolved — and MUST NOT be read as Physical.
type RobotSource string

// RobotSource values.
const (
	// RobotSourcePhysical: the bound FleetAdapter drives physical hardware.
	RobotSourcePhysical RobotSource = "Physical"
	// RobotSourceSimulated: the bound FleetAdapter declares spec.simulated.
	RobotSourceSimulated RobotSource = "Simulated"
)

// RobotSourceAnnotation records the provenance of a Robot's adapter binding
// (ADR-0038). Informational; never a safety signal and never an admission input.
const RobotSourceAnnotation = "swarmada.io/robot-source"

// ConformanceReportRef points at a signed conformance test result.
type ConformanceReportRef struct {
	// SuiteVersion is the version of the conformance suite that was run.
	SuiteVersion string `json:"suiteVersion"`

	// ConfigMapRef names a ConfigMap in this namespace holding the signed report.
	// +optional
	ConfigMapRef string `json:"configMapRef,omitempty"`

	// Digest is the sha256 digest of the signed report, for integrity checking.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ── Status ────────────────────────────────────────────────────────────────────

// SupportedAction is one entry in an adapter's discoverable action catalog
// (RFC-0001 §9.2): an action type the adapter can serve, the capabilities a robot
// needs, and coarse parameter descriptors. Numeric min/max travel on the wire but
// are not projected to status in v0.1 (the projection is deliberately coarse).
type SupportedAction struct {
	// ActionType matches FleetAction.spec.type.
	ActionType string `json:"actionType"`
	// RequiredCapabilities a robot must have Active to be eligible.
	// +optional
	// +listType=atomic
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty"`
	// Params are coarse payload parameter descriptors — what the action TAKES.
	// +optional
	// +listType=atomic
	Params []ActionParam `json:"params,omitempty"`

	// Results are coarse descriptors for what the action EMITS, projected from
	// fleet_adapter.v1 SupportedAction.results.
	//
	// Params declares inputs; until Results, nothing declared outputs, so a
	// binding feeding one step's output into a later step's input could be checked
	// at the consuming end only — the producing end was not checkable at all,
	// because no descriptor said what a robot action emits. The binding validator
	// reads THIS field, not the proto, so the proto field alone would change
	// nothing: this mirror is the half that makes a static parameter-binding check
	// possible for a binding onto a robot action.
	//
	// ADDITIVE, and empty means UNKNOWN. An adapter that declares no results is
	// making no statement about its outputs, which is what every adapter built
	// before this field meant. A checker MUST NOT read empty as "produces
	// nothing" — that would reject every binding onto every adapter predating the
	// field.
	// +optional
	// +listType=atomic
	Results []ActionParam `json:"results,omitempty"`

	// +optional
	Description string `json:"description,omitempty"`

	// CustomType is the one Custom verb this entry declares, projected from
	// fleet_adapter.v1 SupportedAction.custom_type, and matched against
	// FleetAction.spec.customType. Set only on an entry whose actionType is
	// Custom; the control plane drops it from any other entry, which then reads
	// as the type-only entry it always was.
	//
	// EMPTY ON A CUSTOM ENTRY MEANS EVERY VERB NO KEYED ENTRY NAMES — the meaning
	// every Custom entry had before this field. For a verb V a reader uses the
	// entry keyed V when there is one, and the unkeyed Custom entry otherwise.
	//
	// No validation beyond the bound spec.customType carries: a rule that refused
	// one malformed entry would refuse the whole status write, and cost the
	// adapter every valid entry beside it. The control plane drops an entry whose
	// verb exceeds the bound (no FleetAction can name it) before it writes.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	CustomType string `json:"customType,omitempty"`
}

// ActionParam is a coarse parameter descriptor for a SupportedAction.
type ActionParam struct {
	Name string `json:"name"`
	// +optional
	Unit string `json:"unit,omitempty"`
	// Kind is one of number|string|bool|enum.
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	// +listType=atomic
	Allowed []string `json:"allowed,omitempty"`
	// +optional
	Required bool `json:"required,omitempty"`
}

// FleetAdapterStatus describes the observed state of a FleetAdapter: its live connectivity and
// health phase, the versions negotiated at the handshake, the conformance result the control plane
// gates dispatch on, and the robots it currently drives. Control-plane-owned — an adapter never
// writes it.
type FleetAdapterStatus struct {
	// Phase is the current connection/health state.
	// +optional
	Phase FleetAdapterPhase `json:"phase,omitempty"`

	// Conformance is the result of the last conformance verification. Robots are
	// only admitted against an adapter whose Conformance is Passed.
	// +optional
	Conformance ConformanceState `json:"conformance,omitempty"`

	// ConformanceContractVersion is the fleet-adapter CONTRACT version (semver, e.g. "1.0.0") that
	// the verified conformance report was earned against — the `contract_version` the harness
	// stamped into the report whose digest this controller checked (ADR-0032, "Version-bound
	// conformance"). It binds the result to a version instead of treating a Passed earned against
	// one revision of the contract as valid against any.
	//
	// It is only set from a report whose digest verified: an unverifiable report clears it, so a
	// stale version never sits beside a Failed result. Empty means the attestation carries no
	// contract version (a report from a pre-versioning harness) — recorded as unknown, never
	// inferred, and never treated as an implicit pass.
	//
	// Conformance remains SELF-CERTIFIED (ADR-0007): the harness produces this value, the registry
	// pull request attests it, and the control plane only consumes it. Nothing here issues a result.
	// +optional
	ConformanceContractVersion string `json:"conformanceContractVersion,omitempty"`

	// NegotiatedProtocolVersion is the protocol version actually agreed with the
	// adapter at connect time; it may differ from spec.protocolVersion if the
	// adapter reports an older version.
	// +optional
	NegotiatedProtocolVersion string `json:"negotiatedProtocolVersion,omitempty"`

	// NegotiatedContractVersion is the fleet-adapter CONTRACT version agreed at the last handshake
	// (ADR-0032) — the semver over the proto surface, the SupportedAction schema and the conformance
	// suite. Distinct from NegotiatedProtocolVersion, which is a package identity ("fleet_adapter.v1")
	// and so cannot express compatibility.
	//
	// Set only when the reported version is inside the range this control plane supports; an adapter
	// that reports an out-of-range, unparseable, or absent version leaves this EMPTY and lands in
	// phase Rejected, which bars its robots from admission. Empty therefore means "no compatible
	// contract was agreed", never "compatible but unrecorded".
	// +optional
	NegotiatedContractVersion string `json:"negotiatedContractVersion,omitempty"`

	// ConnectedRobots is the number of Robots currently driven through this adapter.
	// +optional
	ConnectedRobots int32 `json:"connectedRobots,omitempty"`

	// SupportedActions is the adapter's advertised action catalog, projected from
	// the CapabilitiesSnapshot supported_actions (RFC-0001 §9.2). Read-only; written
	// on change (RA-1). Admission uses it to pre-filter unserviceable FleetAction
	// types before scheduling.
	// +optional
	// +listType=atomic
	SupportedActions []SupportedAction `json:"supportedActions,omitempty"`

	// LastHeartbeat is when the control plane last confirmed adapter liveness.
	// +optional
	LastHeartbeat *metav1.Time `json:"lastHeartbeat,omitempty"`

	// Message is a human-readable summary, especially on Degraded or Rejected.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions is the standard conditions list.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// FleetAdapter is the Schema for the fleetadapters API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fadapt
// +kubebuilder:printcolumn:name="Vendor",type=string,JSONPath=".spec.vendor"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Conformance",type=string,JSONPath=".status.conformance"
// +kubebuilder:printcolumn:name="Robots",type=integer,JSONPath=".status.connectedRobots"
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=".spec.endpoint",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type FleetAdapter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetAdapterSpec   `json:"spec,omitempty"`
	Status FleetAdapterStatus `json:"status,omitempty"`
}

// FleetAdapterList contains a list of FleetAdapter.
// +kubebuilder:object:root=true
type FleetAdapterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetAdapter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FleetAdapter{}, &FleetAdapterList{})
}
