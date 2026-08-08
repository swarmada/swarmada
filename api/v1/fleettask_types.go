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

// FleetTask is the composite, upstream-facing unit of work (RFC-0001 §9.1.5): one logical
// objective accomplished by scheduling one or more atomic FleetActions. It never crosses the
// fleet-adapter boundary; the control plane decomposes it into child FleetActions, each dispatched
// to one robot. It supersedes the design formerly drafted as a separate multi-robot resource.

// ── Enumerations ──────────────────────────────────────────────────────────────

// CompletionPolicy decides when a FleetTask is Succeeded, given its actions' outcomes.
// +kubebuilder:validation:Enum=All;Any;Quorum
type CompletionPolicy string

const (
	// CompletionPolicyAll — every action must reach Succeeded (default).
	CompletionPolicyAll CompletionPolicy = "All"
	// CompletionPolicyAny — the task succeeds as soon as one action reaches Succeeded.
	CompletionPolicyAny CompletionPolicy = "Any"
	// CompletionPolicyQuorum — at least spec.quorum actions must reach Succeeded.
	CompletionPolicyQuorum CompletionPolicy = "Quorum"
)

// FailurePolicy decides the task's response to an action that reaches a terminal Failed phase
// its completionPolicy cannot absorb (per-action RetryPolicy is exhausted first).
// +kubebuilder:validation:Enum=FailFast;ContinueOthers;Compensate
type FailurePolicy string

const (
	// FailurePolicyFailFast — cancel all non-terminal actions; task → Failed.
	FailurePolicyFailFast FailurePolicy = "FailFast"
	// FailurePolicyContinueOthers — independent actions run on; task → Failed only once
	// completionPolicy can no longer be met.
	FailurePolicyContinueOthers FailurePolicy = "ContinueOthers"
	// FailurePolicyCompensate — run each Succeeded action's compensation in reverse dependency
	// order (saga); task → Compensated.
	FailurePolicyCompensate FailurePolicy = "Compensate"
)

// TriggerMode gates when an action becomes eligible.
// +kubebuilder:validation:Enum=Auto;OnEvent
type TriggerMode string

const (
	// TriggerModeAuto — eligible as soon as dependsOn is satisfied (default).
	TriggerModeAuto TriggerMode = "Auto"
	// TriggerModeOnEvent — dormant; becomes eligible only when an authorized append/patch
	// activates it. (The event-binding surface itself is specified in a later RFC.)
	TriggerModeOnEvent TriggerMode = "OnEvent"
)

// FleetTaskPhase is the aggregate lifecycle state of a composite FleetTask. It is a pure function
// of the member actions' phases and the task's policies (RFC-0001 §9.1.5), recomputed on any
// member phase transition and never on a telemetry tick (RA-1); it is monotonic toward a terminal
// value.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Compensating;Compensated;Cancelled
type FleetTaskPhase string

// FleetTaskPhase values (§9.1.5). A composite task moves Pending → Running → a terminal state;
// Compensating/Compensated belong to the saga rollback path taken when failurePolicy is Compensate.
const (
	FleetTaskPhasePending      FleetTaskPhase = "Pending"
	FleetTaskPhaseRunning      FleetTaskPhase = "Running"
	FleetTaskPhaseSucceeded    FleetTaskPhase = "Succeeded"
	FleetTaskPhaseFailed       FleetTaskPhase = "Failed"
	FleetTaskPhaseCompensating FleetTaskPhase = "Compensating"
	FleetTaskPhaseCompensated  FleetTaskPhase = "Compensated"
	FleetTaskPhaseCancelled    FleetTaskPhase = "Cancelled"
)

// ── Spec ────────────────────────────────────────────────────────────────────────

// FleetTaskAction is one member action of a composite FleetTask.
type FleetTaskAction struct {
	// Name is unique within the task and stable for the life of the task.
	Name string `json:"name"`

	// Action is a full FleetActionSpec, embedded verbatim and copied unchanged onto the child
	// FleetAction the controller generates for this member.
	Action FleetActionSpec `json:"action"`

	// DependsOn lists member names that gate this action's start. Empty = a root action, eligible
	// immediately (subject to the start barrier). Multiple entries are an AND-join: eligible only
	// when EVERY predecessor has reached this action's StartCondition. Must be acyclic and name
	// existing members (enforced at admission).
	// +optional
	// +listType=set
	DependsOn []string `json:"dependsOn,omitempty"`

	// StartCondition is the predecessor phase that satisfies each dependency, in the FleetAction
	// phase vocabulary. Succeeded (default) is a strict pipeline; InProgress is a relay; Assigned
	// is "as soon as a robot commits."
	// +kubebuilder:validation:Enum=Assigned;InProgress;Succeeded
	// +kubebuilder:default=Succeeded
	StartCondition ActionPhase `json:"startCondition,omitempty"`

	// Trigger controls eligibility. Auto (default): eligible when DependsOn is satisfied. OnEvent:
	// dormant until an authorized append/patch activates it.
	// +kubebuilder:validation:Enum=Auto;OnEvent
	// +kubebuilder:default=Auto
	Trigger TriggerMode `json:"trigger,omitempty"`

	// Compensation is an OPTIONAL FleetActionSpec run to undo this action's effect if it reached
	// Succeeded but the task later fails under failurePolicy: Compensate.
	// +optional
	Compensation *FleetActionSpec `json:"compensation,omitempty"`
}

// FleetTaskSpec defines the desired composite objective.
//
// +kubebuilder:validation:XValidation:rule="self.completionPolicy != 'Quorum' || has(self.quorum)",message="quorum is required when completionPolicy is Quorum"
type FleetTaskSpec struct {
	// CompletionPolicy decides when the task is Succeeded given member outcomes.
	// +kubebuilder:validation:Enum=All;Any;Quorum
	// +kubebuilder:default=All
	CompletionPolicy CompletionPolicy `json:"completionPolicy,omitempty"`

	// Quorum is REQUIRED when CompletionPolicy is Quorum, ignored otherwise (CEL-enforced).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Quorum *int32 `json:"quorum,omitempty"`

	// FailurePolicy decides the response to a terminally-failed action completionPolicy cannot
	// absorb. Immutable after creation (a graph's failure semantics must not shift mid-run).
	// +kubebuilder:validation:Enum=FailFast;ContinueOthers;Compensate
	// +kubebuilder:default=FailFast
	FailurePolicy FailurePolicy `json:"failurePolicy,omitempty"`

	// DesiredState is the declarative, level-triggered intent for the whole task, reconciled onto
	// its non-terminal child actions. A write persists and re-converges after a disconnect;
	// re-writing the same value is idempotent (RFC-0001 §9.1.5, ADR-0007).
	// +kubebuilder:validation:Enum=Running;Paused;Returning;Cancelled
	// +kubebuilder:default=Running
	DesiredState DesiredState `json:"desiredState,omitempty"`

	// Actions is the member set. APPEND-ONLY after creation: existing entries (spec and recorded
	// status) are immutable; new entries may be appended before the task is terminal via the
	// scoped `append` RBAC verb (enforced by the validating webhook). Keyed by name.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Actions []FleetTaskAction `json:"actions"`
}

// ── Status ──────────────────────────────────────────────────────────────────────

// FleetTaskActionStatus projects one member's current state. The composite controller reads child
// FleetAction status and never writes it (RA-1 + the RFC-0001 ownership discipline).
type FleetTaskActionStatus struct {
	// Name matches the spec.actions[].name.
	Name string `json:"name"`

	// ActionRef is the owned child FleetAction's name; empty until the action is generated.
	// +optional
	ActionRef string `json:"actionRef,omitempty"`

	// Phase is the child FleetAction's current phase (its own lifecycle).
	// +optional
	Phase ActionPhase `json:"phase,omitempty"`

	// AssignedRobot is the robot the child FleetAction is scheduled to, if any.
	// +optional
	AssignedRobot string `json:"assignedRobot,omitempty"`

	// DependenciesMet reports whether every DependsOn predecessor has reached StartCondition.
	DependenciesMet bool `json:"dependenciesMet"`

	// Attempt is incremented each time this member is (re)generated as a new child FleetAction.
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// CompensationPhase tracks a compensation run under failurePolicy: Compensate.
	// +optional
	// +kubebuilder:validation:Enum=None;Pending;InProgress;Succeeded;Failed
	CompensationPhase string `json:"compensationPhase,omitempty"`
}

// FleetTaskStatus describes the observed state of a FleetTask.
type FleetTaskStatus struct {
	// Phase is the aggregate lifecycle state (RA-1: transition-driven, never per telemetry tick).
	// +optional
	Phase FleetTaskPhase `json:"phase,omitempty"`

	// ActionSummary is a short "N/M Succeeded" projection for print columns and tooling.
	// +optional
	ActionSummary string `json:"actionSummary,omitempty"`

	// Actions projects each member's current state.
	// +optional
	// +listType=map
	// +listMapKey=name
	Actions []FleetTaskActionStatus `json:"actions,omitempty"`

	// StartedAt is when the first member entered execution.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletionTime is when the task reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions is the standard conditions list (e.g. Ready, DependencyGraphValid, BarrierSatisfied).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// FleetTask is the Schema for the fleettasks API — the composite objective composing FleetActions.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ft
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Actions",type=string,JSONPath=`.status.actionSummary`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.desiredState`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type FleetTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetTaskSpec   `json:"spec,omitempty"`
	Status FleetTaskStatus `json:"status,omitempty"`
}

// FleetTaskList contains a list of FleetTask.
// +kubebuilder:object:root=true
type FleetTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FleetTask{}, &FleetTaskList{})
}
