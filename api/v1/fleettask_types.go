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
// +kubebuilder:validation:Enum=Draft;Pending;Running;Succeeded;Failed;Compensating;Compensated;Cancelled
type FleetTaskPhase string

// FleetTaskPhase values (§9.1.5). A composite task moves Pending → Running → a terminal state;
// Compensating/Compensated belong to the saga rollback path taken when failurePolicy is Compensate.
const (
	// FleetTaskPhaseDraft: composed but NOT dispatched. No child actions exist and
	// none will be generated while spec.draft is true. A human clears the flag to
	// activate it. This is the phase a TaskTrigger's output sits in.
	FleetTaskPhaseDraft        FleetTaskPhase = "Draft"
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
	// immediately. Multiple entries are an AND-join: eligible only
	// when EVERY predecessor has reached this action's StartCondition. Must be acyclic and name
	// existing members. Specified to be enforced at admission; at v0.4 this is validated by
	// validateGraph on every non-terminal reconcile of a task that is not a draft instead, not by
	// the admission webhook — a cyclic or dangling entry is currently admitted and fails on the
	// next reconcile. See crds/fleettask.md.
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
	// dormant until an authorized append/patch activates it. Specified to allow an existing entry's
	// Trigger to be flipped OnEvent -> Auto post-creation as the sole exception to Actions being
	// otherwise immutable once created; at v0.4 this is not implemented — ValidateUpdate's
	// append-only check rejects any change to an existing action, including a Trigger-only flip.
	// See crds/fleettask.md.
	// +kubebuilder:validation:Enum=Auto;OnEvent
	// +kubebuilder:default=Auto
	Trigger TriggerMode `json:"trigger,omitempty"`

	// Compensation is an OPTIONAL FleetActionSpec run to undo this action's effect if it reached
	// Succeeded but the task later fails under failurePolicy: Compensate.
	//
	// EXCLUDED for an external action: a member whose Action
	// carries spec.executor MUST NOT set this. Swarmada cannot compensate what
	// it never observed — an external executor writes to its sink on the data
	// path and the control plane never sees the row — so an unbacked rollback
	// promise is refused at admission rather than discovered at rollback time.
	//
	// A compensation carries no spec.executor either, for the same reason: an
	// external undo would report a rollback the control plane never observed.
	// +optional
	Compensation *FleetActionSpec `json:"compensation,omitempty"`
}

// FleetTaskSpec defines the desired composite objective.
//
// +kubebuilder:validation:XValidation:rule="self.completionPolicy != 'Quorum' || has(self.quorum)",message="quorum is required when completionPolicy is Quorum"
// +kubebuilder:validation:XValidation:rule="!has(self.flow) || self.actions.all(a, !has(a.dependsOn) || size(a.dependsOn) == 0)",message="spec.flow and a member's dependsOn are mutually exclusive: with flow present, every action's dependsOn must be empty"
// +kubebuilder:validation:XValidation:rule="!has(self.flow) || self.completionPolicy == 'All'",message="spec.flow supplies the task's outcome (the root node's); admission rejects a task that sets both a non-default completionPolicy and a flow"
// +kubebuilder:validation:XValidation:rule="self.actions.all(a, !has(a.compensation) || !has(a.action.executor))",message="spec.compensation must not be set on a member whose action carries spec.executor — external actions are excluded from Compensate"
// +kubebuilder:validation:XValidation:rule="self.actions.all(a, !has(a.compensation) || !has(a.compensation.executor))",message="spec.compensation must not carry spec.executor — an external undo is excluded from Compensate"
type FleetTaskSpec struct {
	// CompletionPolicy decides when the task is Succeeded given member outcomes.
	// +kubebuilder:validation:Enum=All;Any;Quorum
	// +kubebuilder:default=All
	CompletionPolicy CompletionPolicy `json:"completionPolicy,omitempty"`

	// Quorum is REQUIRED when CompletionPolicy is Quorum, ignored otherwise (CEL-enforced).
	// The controller re-checks it on every non-terminal reconcile and fails the task
	// CLOSED (condition CompletionPolicyValid=False, reason QuorumUnset) if it is absent —
	// a nil threshold read as 0 made `succeeded >= 0` true on the first pass, reporting
	// Succeeded having run nothing.
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
	// re-writing the same value is idempotent (RFC-0001 §9.1.5, ADR-0047).
	// +kubebuilder:validation:Enum=Running;Paused;Returning;Cancelled
	// +kubebuilder:default=Running
	DesiredState DesiredState `json:"desiredState,omitempty"`

	// Draft holds this task as a proposal. While true the FleetTask controller
	// generates NO child actions and dispatches nothing; the task sits at phase
	// Draft. A human activates it by setting this false. Where the admission
	// webhooks are served, that write needs the activate verb on fleettasks, and
	// it is refused to every ServiceAccount whatever its roles hold.
	//
	// # WHY THIS IS NOT desiredState: Paused
	//
	// Paused already means something, and it is a different thing: a HOLD on work
	// under way, entered through an estop or spec.desiredState and, after an estop,
	// governed by rules about who may lift it. A draft has never run, holds no
	// robot, and has no lease. Overloading Paused would have put an approval gate and
	// an emergency stop on the same field, so an operator clearing one would be
	// working the same control as an operator clearing the other — and the estop
	// rules exist precisely to stop a hold being lifted casually.
	//
	// The default is false, so every FleetTask written before this field existed, and
	// every one an operator writes directly, behaves exactly as before. Only a
	// producer that sets it gets the gate: at v0.4 that is the TaskTrigger
	// controller; any other client that creates a FleetTask sets it itself.
	// +optional
	// +kubebuilder:default=false
	Draft bool `json:"draft,omitempty"`

	// Inputs declares the named, typed values supplied WITH this task, which a
	// member's parameter binding reads via `fromTaskInput` — source 2 of the
	// closed six a `ParameterBinding` admits.
	//
	// # IT EXISTS SO THAT A SUPPLIED VALUE IS NOT A LITERAL EDITED INTO THE FLOW
	//
	// "A person said go to the loading dock" needs somewhere to land that is not
	// a string spliced into a member's spec. That matters most where the task
	// template is fixed and only the inputs vary: with declared inputs, the
	// template is a reusable object and the variable part is data; without them,
	// every variation is a new object that differs from the last one somewhere in
	// its middle.
	//
	// # WITHOUT IT, `fromTaskInput` NAMED NOTHING
	//
	// `ParameterBinding.FromTaskInput` (api/v1/fleetaction_types.go) has said
	// "names a FleetTask-level input (spec.inputs) to read" since it was added,
	// and `spec.inputs` did not exist — a field in this API whose documentation
	// pointed at a field in this API that was not there. A FleetTask whose
	// binding names an undeclared input is refused at admission, on create and
	// on every update that changes spec.actions: an appended member whose
	// binding names an input the task does not declare is refused.
	// spec.inputs and spec.flow are immutable after a FleetTask is created, and
	// the API server refuses an update that changes either, so no update can
	// remove an input a binding still reads. A TaskTrigger's taskTemplate stays
	// editable, and is not checked when the trigger is
	// written; the check runs when the trigger creates the FleetTask. No
	// controller resolves a fromTaskInput binding at v0.4: it is copied onto
	// the child FleetAction as written, and no value in this list is read
	// outside admission.
	//
	// VALUES ARE STRING-ENCODED, like every other scalar in this grammar
	// (`ParameterBinding.Value`, `ComputeEndpointParameter.Default`). One
	// encoding for one concept: a number that is a string in one field and a
	// float in the next is a number two consumers will parse differently.
	// +optional
	// +listType=map
	// +listMapKey=name
	Inputs []TaskInput `json:"inputs,omitempty"`

	// Actions is the member set. APPEND-ONLY after creation: existing entries (spec and recorded
	// status) are immutable; new entries may be appended before the task is terminal via the
	// scoped `append` RBAC verb (enforced by the validating webhook). Keyed by name.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Actions []FleetTaskAction `json:"actions"`

	// Flow is an optional structured control-flow tree over Actions:
	// sequence, parallel+join, loop, select. spec.actions
	// keeps its meaning as the member INVENTORY; Flow supplies the SCHEDULE
	// over it. Mutually exclusive with any member's dependsOn (CEL, above).
	// When Flow is unset the controller's flow branch is not entered, and the
	// task is scheduled by the dependsOn/completionPolicy reconciliation path
	// alone; a nil Flow changes nothing on that path.
	// +optional
	Flow *FlowNode `json:"flow,omitempty"`
}

// TaskInput declares one named, typed value supplied with a FleetTask, which a
// member's `parameters` block reads by name via `fromTaskInput`.
//
// Deliberately the SAME shape as ComputeEndpointParameter
// (api/v1/computeendpoint_types.go) minus the endpoint-specific fields: same
// ValueKind vocabulary, same string-encoded Default, same Allowed set for an
// enum, same numeric bounds. A second, subtly different way to declare "a named
// scalar with a type and a default" is a second thing to learn and a second
// place for the two to disagree about what `number` means.
//
// +kubebuilder:validation:XValidation:rule="self.kind != 'enum' || size(self.allowed) > 0",message="allowed must list at least one value when kind is enum"
// +kubebuilder:validation:XValidation:rule="!has(self.allowed) || size(self.allowed) == 0 || self.kind == 'enum'",message="allowed is meaningful only when kind is enum"
type TaskInput struct {
	// Name is the input name a `fromTaskInput` binding reads.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Kind is the input's scalar type.
	Kind ValueKind `json:"kind"`

	// Allowed enumerates permitted values when Kind is enum.
	// +optional
	// +listType=set
	Allowed []string `json:"allowed,omitempty"`

	// Required marks whether this input must be supplied. An input that is
	// Required with no Default is one a task cannot run without, and a binding
	// onto it resolves only when the value is actually present.
	// +optional
	Required bool `json:"required,omitempty"`

	// Default is the string-encoded value specified to be used when none is
	// supplied. Absent is NOT the same as empty: an input with no default that
	// is not supplied is UNRESOLVED, not "" — writing "" for it would silently
	// turn "nobody said" into "somebody said nothing". At v0.4 no controller
	// resolves a fromTaskInput binding, so this default is stored and not
	// applied.
	// +optional
	Default *string `json:"default,omitempty"`

	// Min is the minimum permitted value when Kind is number.
	// +optional
	Min *float64 `json:"min,omitempty"`

	// Max is the maximum permitted value when Kind is number.
	// +optional
	Max *float64 `json:"max,omitempty"`
}

// ── Status ──────────────────────────────────────────────────────────────────────

// FleetTaskActionStatus projects one member's current state. The composite controller reads child
// FleetAction status and writes one field of it only: status.parameterResolution, once, when the
// child carries resolved parameter bindings and the list is still empty (RFC-0001
// #fleettask-ownership-status-aggregation-and-ra-1).
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

	// Attempt is specified to count RETRIES within one execution: one more each time this
	// member is generated again as a new child FleetAction. No controller writes it at v0.4,
	// so it stays 0 however many times the member is generated; read the child FleetActions
	// for the retries. Not to be confused with Iteration,
	// which counts EXECUTIONS — "this leg has run 40 times" and "this leg has
	// failed 40 times" are the difference between a working patrol and an
	// incident, so the two counters are never merged.
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// CompensationPhase tracks a compensation run under failurePolicy: Compensate.
	// +optional
	// +kubebuilder:validation:Enum=None;Pending;InProgress;Succeeded;Failed
	CompensationPhase string `json:"compensationPhase,omitempty"`

	// Iteration is this member's current loop iteration when it is scheduled
	// under a flow loop node; zero/absent for a member that is
	// not under a loop, or before the loop's first iteration.
	// +optional
	Iteration int32 `json:"iteration,omitempty"`

	// IterationsCompleted is the total count of this member's completed
	// (terminal) loop iterations. A complete scalar — never truncated,
	// unlike RecentIterations.
	// +optional
	IterationsCompleted int32 `json:"iterationsCompleted,omitempty"`

	// IterationsFailed is the total count of this member's Failed loop
	// iterations, out of IterationsCompleted. A complete scalar.
	// +optional
	IterationsFailed int32 `json:"iterationsFailed,omitempty"`

	// LoopStartedAt is when this member's owning loop began iteration 1,
	// for a Forever loop's maxDurationSeconds bound. Set once and never
	// updated, so it survives RecentIterations retention trimming.
	// +optional
	LoopStartedAt *metav1.Time `json:"loopStartedAt,omitempty"`

	// RecentIterations is a BOUNDED window of the most recent iteration
	// outcomes — capped at the owning loop's
	// effective flow.retention.keepIterationRecords, so an indefinitely
	// repeating member cannot grow FleetTask.status without bound. Full
	// history survives only as the IterationsCompleted/IterationsFailed
	// scalars above.
	// +optional
	// +listType=map
	// +listMapKey=iteration
	RecentIterations []IterationRecord `json:"recentIterations,omitempty"`
}

// IterationRecord is one bounded entry in
// FleetTaskActionStatus.recentIterations.
type IterationRecord struct {
	// Iteration is this record's 1-based iteration number.
	Iteration int32 `json:"iteration"`

	// ActionRef is the iteration's owned child FleetAction name.
	// +optional
	ActionRef string `json:"actionRef,omitempty"`

	// Phase is the iteration's terminal phase.
	// +optional
	Phase ActionPhase `json:"phase,omitempty"`

	// CompletedAt is when the iteration reached its terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
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

	// Conditions is the standard conditions list (e.g. DependencyGraphValid, ChildrenAdmitted,
	// CompletionPolicyValid).
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

	// The two immutability rules sit on this field, not on FleetTaskSpec, because
	// TaskTrigger.spec.taskTemplate is a FleetTaskSpec too, and a trigger's
	// template stays editable.

	// Spec is the desired composite objective. spec.inputs and spec.flow are
	// immutable after creation: a child copies what it consumes when it is
	// created, and an edit to the task would never reach it, so the task and its
	// children would name different values.
	// +kubebuilder:validation:XValidation:rule="has(self.inputs) == has(oldSelf.inputs) && (!has(self.inputs) || self.inputs == oldSelf.inputs)",message="spec.inputs is immutable after creation; create a new FleetTask to supply different inputs"
	// +kubebuilder:validation:XValidation:rule="has(self.flow) == has(oldSelf.flow) && (!has(self.flow) || self.flow == oldSelf.flow)",message="spec.flow is immutable after creation; create a new FleetTask to run a different flow"
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
