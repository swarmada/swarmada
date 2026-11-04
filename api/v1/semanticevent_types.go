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

// SemanticEventSourceSpec declares an event source that is NOT a fleet adapter.
//
// # WHY THIS KIND EXISTS
//
// Three classes of semantic-event source raise events during execution (a robot
// capability, a resident executor, a request/response executor), and all three are
// EXECUTION-BOUND:
// each raises its events against a FleetAction that is already running, and each needs
// a vendor adapter or an executor endpoint behind it. None of them can raise an event
// when nothing is running — which is exactly the case a trigger is most useful for
// ("a door opened", "a shift started", "a work order arrived").
//
// A SemanticEventSource is the adapter-free, K8s-native ingress for those. Anything
// that can reach the API server — a curl in a CI job, an operator with kubectl, a
// webhook receiver, a building-management bridge — creates a SemanticEvent naming this
// source, and the TaskTrigger controller treats it exactly like any other event.
//
// # THIS IS NOT THE WIRE `TaskEvent`
//
// The name is deliberately different. `TaskEvent` is a protobuf message defined in
// fleet_adapter.v1, attributed to an execution and sequenced by its producer; no
// fleet-adapter message carries it yet (RFC-0001 #fleettask-features-outside-this-chapter). A
// SemanticEvent is a Kubernetes object with no execution behind it and no wire
// sequence. Calling both `TaskEvent` would have made two different things share one
// name in one system, and the idempotency rules that apply to one do not apply to the
// other.
type SemanticEventSourceSpec struct {
	// DisplayName is a human-readable label for this source.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// EventTypes is the CLOSED set of event types this source may raise.
	//
	// Declaring them is not bookkeeping: a SemanticEvent whose type its source does not
	// declare is REJECTED and fires no trigger, for the same reason an
	// undeclared robot capability cannot. Without the declaration a typo in an event
	// type is indistinguishable from a type nobody has written a trigger for yet —
	// both simply do nothing — and a silent no-op is the worst outcome for something
	// whose whole job is to notice that something happened.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	EventTypes []string `json:"eventTypes"`
}

// SemanticEventSourceStatus is the observed state of a SemanticEventSource.
type SemanticEventSourceStatus struct {
	// AcceptedCount is how many SemanticEvents naming this source have been admitted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	AcceptedCount int64 `json:"acceptedCount,omitempty"`

	// RejectedCount is how many were refused for naming an undeclared type (E10.1).
	// A non-zero value is the signal that something upstream is emitting a type this
	// source never declared — usually a typo, occasionally a version skew.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RejectedCount int64 `json:"rejectedCount,omitempty"`

	// Conditions is the standard conditions list.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ses
// +kubebuilder:printcolumn:name="Types",type=string,JSONPath=`.spec.eventTypes`
// +kubebuilder:printcolumn:name="Accepted",type=integer,JSONPath=`.status.acceptedCount`
// +kubebuilder:printcolumn:name="Rejected",type=integer,JSONPath=`.status.rejectedCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SemanticEventSource declares a non-adapter source of semantic events.
type SemanticEventSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SemanticEventSourceSpec   `json:"spec,omitempty"`
	Status SemanticEventSourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SemanticEventSourceList contains a list of SemanticEventSource.
type SemanticEventSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SemanticEventSource `json:"items"`
}

// SemanticEventSpec is one occurrence: this source says this happened.
type SemanticEventSpec struct {
	// Source names the SemanticEventSource in this namespace that raised it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Source string `json:"source"`

	// Type is the semantic event type, which MUST be one the source declared.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Type string `json:"type"`

	// Payload is the event's data, as flat string key/values.
	//
	// SCALARS ONLY AND BOUNDED, for the same reason ActionResult.values is: a payload
	// is something a trigger reads and an operator scans, not a data channel. Nested
	// structure invites a caller to put a point cloud in it, and this object lives in
	// etcd.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	Payload map[string]string `json:"payload,omitempty"`

	// OccurredAt is when the thing happened, as distinct from when the object was
	// created. A bridge replaying a backlog sets it; nothing defaults it, because a
	// defaulted timestamp would assert a time nobody observed.
	// +optional
	OccurredAt *metav1.Time `json:"occurredAt,omitempty"`
}

// SemanticEventPhase is how far an event got.
// +kubebuilder:validation:Enum=Pending;Accepted;Rejected
type SemanticEventPhase string

// SemanticEventPhase values.
const (
	// SemanticEventPending: not yet evaluated.
	SemanticEventPending SemanticEventPhase = "Pending"
	// SemanticEventAccepted: the type was declared by its source and triggers were
	// evaluated against it.
	SemanticEventAccepted SemanticEventPhase = "Accepted"
	// SemanticEventRejected: the source does not exist, or never declared this type
	// (E10.1). A rejected event fires nothing.
	SemanticEventRejected SemanticEventPhase = "Rejected"
)

// SemanticEventStatus is the observed state of a SemanticEvent.
type SemanticEventStatus struct {
	// Phase is how far this event got.
	// +optional
	Phase SemanticEventPhase `json:"phase,omitempty"`

	// Message says why, and is the whole diagnostic surface for a rejected event.
	// +optional
	Message string `json:"message,omitempty"`

	// DraftedTasks names the FleetTasks drafted for this event (see FleetTaskSpec.Draft).
	// It is written once, when the event is accepted, and is not updated afterwards: a
	// listed task may since have been activated, dispatched or deleted. A name is listed
	// also when a FleetTask of that name already existed, which may be another trigger's.
	// Whether a listed task still waits for a person is on the FleetTask: spec.draft and
	// status.phase.
	// +optional
	// +listType=set
	DraftedTasks []string `json:"draftedTasks,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sev
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Drafted",type=string,JSONPath=`.status.draftedTasks`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SemanticEvent is one semantic event raised by a SemanticEventSource.
type SemanticEvent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SemanticEventSpec   `json:"spec,omitempty"`
	Status SemanticEventStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SemanticEventList contains a list of SemanticEvent.
type SemanticEventList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SemanticEvent `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&SemanticEventSource{}, &SemanticEventSourceList{},
		&SemanticEvent{}, &SemanticEventList{},
	)
}
