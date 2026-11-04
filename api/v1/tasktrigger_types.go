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

// TaskTriggerSourceRef selects which events fire this trigger.
type TaskTriggerSourceRef struct {
	// SemanticEventSource names the source, in this namespace, to watch.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SemanticEventSource string `json:"semanticEventSource"`

	// EventType is the single event type that fires this trigger. One type per
	// trigger, deliberately: a trigger that matched a SET of types would have to
	// decide which template each produced, and the answer is a second trigger.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	EventType string `json:"eventType"`
}

// TaskTriggerSpec binds a semantic event to a FleetTask that a human may then run.
//
// # THE WHOLE POINT IS THAT IT DOES NOT DISPATCH
//
// A firing produces a DRAFT FleetTask — `spec.draft: true`, which the FleetTask
// controller holds at phase Draft, generating no child actions and dispatching
// nothing. A human activates it by clearing that flag. Until they do, no robot has
// been asked to do anything.
//
// This is not timidity about automation; it is where the authority sits. A semantic
// event is evidence that something happened, produced by a sensor, a bridge, or a
// model — none of which is accountable for what a robot then does in a shared physical
// space. Auto-dispatch would place a physical-world decision behind whatever produced
// the event, and the failure mode is not a wrong API call but a machine moving because
// a door sensor glitched. The draft keeps the machine still and puts a person between
// the observation and the motion, while still doing the tedious part — composing the
// task, grounded in the real schema — automatically.
//
// The control plane holds only this controller to drafts. Any other client that
// creates a FleetTask sets spec.draft itself, and RBAC alone cannot require it to.
type TaskTriggerSpec struct {
	// Source selects the events that fire this trigger.
	// +kubebuilder:validation:Required
	Source TaskTriggerSourceRef `json:"source"`

	// TaskTemplate is the FleetTask spec to draft when this trigger fires.
	//
	// It is drafted as written. This controller performs NO parameter substitution
	// from the event payload — see the type comment on TaskTriggerStatus for why that
	// is a deliberate omission rather than an oversight.
	// +kubebuilder:validation:Required
	TaskTemplate FleetTaskSpec `json:"taskTemplate"`

	// Suspend stops this trigger firing without deleting it. A suspended trigger
	// records nothing: an event it would have matched is accepted with the message
	// "accepted; no TaskTrigger matched", the same as when no trigger exists.
	// +optional
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// TaskTriggerStatus is the observed state of a TaskTrigger.
//
// # WHAT THIS TYPE DELIBERATELY DOES NOT CARRY
//
// It has no `spec.condition` — a CEL expression evaluated against a closed context,
// with which a trigger would scope itself to one vendor's robots in one zone — and the
// absence is deliberate: this tree vendors no CEL evaluator, and a condition field that
// nothing evaluates would be strictly worse than none — an operator would write a scope,
// the apply would succeed, and the trigger would fire on every event regardless. A field
// that is specified but inert is more dangerous here than usually, because the field's
// whole purpose is to NARROW what fires.
//
// The same reasoning covers payload substitution into the template: there is no
// `paramsFrom` mapping event payload fields into action parameters, because a half-done
// version that silently dropped unmapped fields would produce a task that looks
// parameterised and is not. A human reviewing the draft can fill values in; that is
// what the draft is for.
type TaskTriggerStatus struct {
	// LastFiredAt is when this trigger last drafted a task.
	// +optional
	LastFiredAt *metav1.Time `json:"lastFiredAt,omitempty"`

	// LastEventName is the SemanticEvent that last fired it.
	// +optional
	LastEventName string `json:"lastEventName,omitempty"`

	// DraftCount is how many drafts this trigger has produced.
	// +optional
	// +kubebuilder:validation:Minimum=0
	DraftCount int64 `json:"draftCount,omitempty"`

	// PendingDrafts names the drafted FleetTasks that are still awaiting a human.
	// This is the operator's work queue, and a growing list is the signal that
	// events are arriving faster than anyone is acting on them.
	// +optional
	// +listType=set
	PendingDrafts []string `json:"pendingDrafts,omitempty"`

	// Conditions is the standard conditions list.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tt
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.semanticEventSource`
// +kubebuilder:printcolumn:name="Event",type=string,JSONPath=`.spec.source.eventType`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Drafts",type=integer,JSONPath=`.status.draftCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TaskTrigger drafts a FleetTask when a semantic event arrives. It never dispatches.
type TaskTrigger struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TaskTriggerSpec   `json:"spec,omitempty"`
	Status TaskTriggerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TaskTriggerList contains a list of TaskTrigger.
type TaskTriggerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TaskTrigger `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TaskTrigger{}, &TaskTriggerList{})
}
