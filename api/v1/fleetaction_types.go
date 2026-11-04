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

// ── Enumerations ──────────────────────────────────────────────────────────────

// ActionPhase is the lifecycle state of a FleetAction (RFC-0001 #fleetaction).
// +kubebuilder:validation:Enum=Pending;Assigned;InProgress;Succeeded;Failed;Cancelled;Paused;Revoking;Preempted
type ActionPhase string

// ActionPhase values.
const (
	ActionPhasePending ActionPhase = "Pending"
	// ActionPhaseAssigned: scheduled to a robot, not yet acknowledged/executing.
	ActionPhaseAssigned ActionPhase = "Assigned"
	// ActionPhaseInProgress: the robot is actively executing the action.
	ActionPhaseInProgress ActionPhase = "InProgress"
	ActionPhaseSucceeded  ActionPhase = "Succeeded"
	ActionPhaseFailed     ActionPhase = "Failed"
	ActionPhaseCancelled  ActionPhase = "Cancelled"
	// ActionPhasePaused: interrupted by estop; awaits an operator decision
	// (RFC-0001 #safety-fleetaction-behaviour-during-estop).
	ActionPhasePaused ActionPhase = "Paused"
	// ActionPhaseRevoking: assigned robot lost connectivity; awaiting lease expiry
	// before safe reassignment (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee).
	ActionPhaseRevoking ActionPhase = "Revoking"
	// ActionPhasePreempted: displaced by a higher-priority action at a shared resource
	// or zone (RFC-0001 #fleetaction-priority-semantics).
	ActionPhasePreempted ActionPhase = "Preempted"
)

// ActionPriority is the named scheduling band for a FleetAction. Bands are compared
// by rank (Critical > High > Normal > Low) by the scheduler and the Traffic
// Deconfliction Engine (RFC-0001 #tde); the preemptor bands (Critical and High,
// see CanPreempt) may displace a lower Normal/Low band.
// +kubebuilder:validation:Enum=Critical;High;Normal;Low
type ActionPriority string

// ActionPriority values.
const (
	ActionPriorityCritical ActionPriority = "Critical"
	ActionPriorityHigh     ActionPriority = "High"
	ActionPriorityNormal   ActionPriority = "Normal"
	ActionPriorityLow      ActionPriority = "Low"
)

// CanPreempt reports whether a action in this band may displace a lower-band
// (Normal/Low) reservation or assignment when no slot/robot is otherwise
// available. Both Critical and High are preemptor bands; the victim rule
// (Normal/Low only — see internal/tde.isPreemptibleBand) is independent, so a
// High preemptor never evicts another High or a Critical.
func (p ActionPriority) CanPreempt() bool {
	return p == ActionPriorityCritical || p == ActionPriorityHigh
}

// ActionType classifies the kind of work to be performed. External is dispatched
// to an executor endpoint (spec.executor) rather than to a robot.
//
// ADMISSION RULE FOR A NEW VALUE — an enum of verbs is a closed set meeting an open
// world, and this is the rule that keeps it from growing one device class at a time.
// A value enters this enum ONLY when the control plane's own behaviour depends on
// distinguishing it. Exactly one value meets that bar today: External, which selects
// the dispatch target and is enforced structurally by the CEL rule on FleetActionSpec.
// The robot verbs meet it in exactly one specified place — the same-type test that
// classifies a directive supersession as in-place rather than confirmed-cancel
// (RFC-0001 #fleetaction-directive-supersession). Nothing else in the tree branches on
// them: they reach the adapter as AssignAction.action_type, are matched against the
// adapter's advertised catalog at admission (internal/webhook/fleetaction_webhook.go),
// and are printed. They are grandfathered VOCABULARY, not a design.
//
// Clean is admitted as a vocabulary repair under that rule, not as an extension of it.
// The catalog pre-filter compares FleetAdapter.status.supportedActions[].actionType to
// spec.type by exact string equality, so while this enum lacked the verb an adapter
// whose primary job is cleaning could not advertise a catalog that described it — every
// clean was typed Custom and the adapter had to misdeclare itself to get its own work
// admitted. That is a defect in this enum, not a request for a seventh verb.
//
// A verb that does NOT meet the rule belongs on Custom + CustomType (below), which is
// the open-world extension point and is now legible on the printer columns.
// +kubebuilder:validation:Enum=Navigate;PickUp;DropOff;Patrol;Charge;Clean;Custom;External
type ActionType string

// ActionType values.
const (
	ActionTypeNavigate ActionType = "Navigate"
	ActionTypePickUp   ActionType = "PickUp"
	ActionTypeDropOff  ActionType = "DropOff"
	ActionTypePatrol   ActionType = "Patrol"
	ActionTypeCharge   ActionType = "Charge"
	// ActionTypeClean: surface/area cleaning. See the admission rule above — this is
	// the vocabulary gap that forced every clean to be typed Custom.
	ActionTypeClean  ActionType = "Clean"
	ActionTypeCustom ActionType = "Custom"
	// ActionTypeExternal: work is dispatched to spec.executor, not to a robot.
	// Presence of spec.executor and ActionTypeExternal imply each other (CEL, below).
	ActionTypeExternal ActionType = "External"
)

// OutcomeCertainty records whether the control plane could ESTABLISH what a terminal
// action achieved — a fact distinct from, and not derivable from, status.phase.
//
// WHY THIS IS A FIELD AND NOT A PHASE VALUE. The wire has carried this state since
// fleet_adapter.v1: ACTION_STATE_UNKNOWN and ROBOT_ACTION_PHASE_UNKNOWN both mean "this
// run ended and I cannot tell you what it achieved". The CRD had no counterpart, so an
// adapter that honestly could not tell had to pick a phase asserting something it never
// observed. status.phase cannot be that counterpart: it is a CLOSED enumeration by
// RFC-0001's phase-enum extension policy, and the policy is not ceremonial — every
// consumer in this tree defaults an unrecognised phase into a WRONG bucket, and three
// of those defaults are deadlocks (the FleetTask aggregator counts it non-terminal and
// the task hangs Running; the flow evaluator reports it InProgress and the node never
// settles; the TDE never releases its zone reservation). The policy names the shape a
// new observable fact must take instead — a condition and, where a value is needed, a
// dedicated status field — and this is that field.
//
// The precedent is on this very resource: CancellationReason exists because a
// supersession and an operator cancellation share the Cancelled phase and must be told
// apart by a field, never by the phase. This is the same move for the same reason.
//
// FAIL-CLOSED IS PRESERVED, NOT REPLACED. An unestablished outcome stays phase: Failed,
// so every policy that reads phase keeps behaving correctly with no change; and the CEL
// rule on FleetActionStatus pins Unestablished to Failed, which makes "an outcome nobody
// could establish landing in the success column" structurally impossible rather than
// something each reader has to get right. What the field adds is that Failed is no
// longer a false STATEMENT: the phase says what the control plane did, and this says how
// much it actually knew.
// +kubebuilder:validation:Enum=Established;Unestablished
type OutcomeCertainty string

// OutcomeCertainty values.
const (
	// OutcomeCertaintyEstablished: the reported terminal outcome is an observation.
	// The robot (or executor) told the control plane what it achieved and the control
	// plane recorded that. This is the ordinary case for every terminal phase.
	OutcomeCertaintyEstablished OutcomeCertainty = "Established"
	// OutcomeCertaintyUnestablished: the run ENDED and whether it did the job CANNOT be
	// established. Not "it failed", and not "it was cancelled" — those are claims, and
	// no one is in a position to make either. The action is held at Failed because
	// fail-closed is the only safe disposition for an outcome nobody can vouch for, and
	// this value is the record that the disposition is a policy and not an observation.
	// status.failureReason carries the detail (e.g. which transport could not
	// distinguish completion from refusal).
	OutcomeCertaintyUnestablished OutcomeCertainty = "Unestablished"
)

// ExecutorBinding is the wire contract a ComputeEndpoint speaks.
// Grpc is the only binding that carries a sensor stream.
// There is no dispatch path at v0.4. Nothing in the reference control plane
// resolves an endpoint address, activates a run, or calls the compute runtime service.
// +kubebuilder:validation:Enum=Grpc;Http;Mcp
type ExecutorBinding string

// ExecutorBinding values.
const (
	ExecutorBindingGrpc ExecutorBinding = "Grpc"
	ExecutorBindingHttp ExecutorBinding = "Http"
	ExecutorBindingMcp  ExecutorBinding = "Mcp"
)

// ExecutorContract is the execution shape a ComputeEndpoint implements. App runs
// to completion and reports one terminal result; Service stays resident and is
// cancelled. Only the App contract is exercised at v0.4.
// +kubebuilder:validation:Enum=App;Service
type ExecutorContract string

// ExecutorContract values.
const (
	ExecutorContractApp     ExecutorContract = "App"
	ExecutorContractService ExecutorContract = "Service"
)

// CompletionMode is how an executor run's lifecycle is observed: RunToCompletion
// for an App contract, Continuous for a Service contract. Specified to be
// defaulted from the referenced ComputeEndpoint's contract; not defaulted at v0.4.
// +kubebuilder:validation:Enum=RunToCompletion;Continuous
type CompletionMode string

// CompletionMode values.
const (
	CompletionModeRunToCompletion CompletionMode = "RunToCompletion"
	CompletionModeContinuous      CompletionMode = "Continuous"
)

// UnresolvedParameterPolicy names the consequence specified for a parameter
// binding that cannot be resolved at activation. At v0.4 no controller applies
// any of the three values; see ActionExecutor.OnUnresolvedParameter for what
// happens to an unresolved binding instead.
// +kubebuilder:validation:Enum=Fail;UseDefault;Omit
type UnresolvedParameterPolicy string

// UnresolvedParameterPolicy values.
const (
	UnresolvedParameterFail       UnresolvedParameterPolicy = "Fail"
	UnresolvedParameterUseDefault UnresolvedParameterPolicy = "UseDefault"
	UnresolvedParameterOmit       UnresolvedParameterPolicy = "Omit"
)

// DesiredState is the declarative, level-triggered control intent for a FleetAction (and, at the
// composite level, a FleetTask). A write persists and re-converges after a disconnect; re-writing
// the same value is idempotent — a dropped edge is never a lost command (RFC-0001, ADR-0047). The
// FleetAction controller is the single place that honors it, mapping onto the existing
// safe-hold / confirmed-cancel / recover paths.
// +kubebuilder:validation:Enum=Running;Paused;Returning;Cancelled
type DesiredState string

const (
	// DesiredStateRunning — execute / continue (default).
	DesiredStateRunning DesiredState = "Running"
	// DesiredStatePaused — safe-hold via the estop-pause path.
	DesiredStatePaused DesiredState = "Paused"
	// DesiredStateReturning — return-to-base / recover, then hold.
	DesiredStateReturning DesiredState = "Returning"
	// DesiredStateCancelled — lease-safe confirmed cancellation.
	DesiredStateCancelled DesiredState = "Cancelled"
)

// ── Spec ──────────────────────────────────────────────────────────────────────

// FleetActionSpec defines the desired work to be performed.
//
// Executor and ActionTypeExternal imply each other, and the eight
// robot-scheduling fields plus Payload are conditionally FORBIDDEN — not merely
// inert — when Executor is set, so a field a caller cannot set is self-documenting
// at apply time rather than silently doing nothing.
// +kubebuilder:validation:XValidation:rule="(has(self.executor) == (self.type == 'External')) && (!has(self.executor) || (!has(self.requiredCapabilities) && !has(self.constraints) && !has(self.acceptDegradedCapabilities) && !has(self.estimatedDurationSeconds) && !has(self.robotSelector) && !has(self.minBatteryPct) && !has(self.preferredRobot) && !has(self.preferredManufacturer) && !has(self.payload)))",message="spec.executor and spec.type=External require each other, and requiredCapabilities/constraints/acceptDegradedCapabilities/estimatedDurationSeconds/robotSelector/minBatteryPct/preferredRobot/preferredManufacturer/payload must be unset when spec.executor is set"
// +kubebuilder:validation:XValidation:rule="!has(self.customType) || self.type == 'Custom'",message="spec.customType names the verb a Custom action performs and is accepted only when spec.type is Custom"
type FleetActionSpec struct {
	// Type classifies the action.
	Type ActionType `json:"type"`

	// CustomType names the verb a Custom action actually performs (e.g.
	// "disinfect_area", "shelf_audit"). Accepted ONLY when Type is Custom (CEL, above);
	// setting it alongside a named verb is rejected rather than ignored, because this
	// field is a printer column and a Navigate action advertising customType: clean
	// would print a claim the type contradicts.
	//
	// THIS IS THE ANSWER TO "what stops the next device class". ActionType is a closed
	// set meeting an open world; Custom plus RequiredCapabilities is the intended
	// extension point, and the reason it kept losing to requests for a new enum value
	// was not that it failed to express the work — requiredCapabilities: [clean_area]
	// expresses it exactly — but that it was OPAQUE ON THE OPERATOR SURFACE. The verb
	// survived in requiredCapabilities and payload, and neither is a printer column, so
	// every Custom action read as the same undifferentiated "Custom" in kubectl get.
	// The request that arrives as "please add our verb to the enum" is, every time, a
	// request to see the verb in a column. This field is that column.
	//
	// DELIBERATELY OPTIONAL, NOT REQUIRED-WHEN-CUSTOM. HardwareComponent's
	// type/customType pair (api/v1/robotclass_types.go) requires it, and that is the
	// shape this field otherwise copies — but that field was born with its type, and
	// Custom has already shipped. Requiring customType on Custom would tighten
	// validation on a published CRD and reject, on their next update, every Custom
	// action already in the field (docs/api-principles.md, Versioning and
	// compatibility). Forbidding it on a non-Custom type cannot break anything: no
	// stored object can carry a field that did not exist. The field stays optional when
	// type is Custom; it is not made required.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	CustomType string `json:"customType,omitempty"`

	// Executor directs this action to a ComputeEndpoint instead of to a robot.
	// There is no dispatch path at v0.4. Nothing in the reference control plane
	// resolves an endpoint address, activates a run, or calls the compute runtime service.
	// Its presence is the discriminator: spec.type MUST be External, and the
	// eight robot-scheduling fields plus Payload MUST be unset (CEL, above).
	// Zone keeps its meaning when Executor is set: it
	// still names the edge zone context for endpoint resolution, not a robot
	// scheduling constraint.
	// +optional
	Executor *ActionExecutor `json:"executor,omitempty"`

	// Zone constrains which zone the action must be executed in.
	// If empty, the scheduler may assign any robot regardless of zone.
	// +optional
	Zone string `json:"zone,omitempty"`

	// RequiredCapabilities is the list of capabilities a robot must have
	// for this action to be assigned to it (e.g. ["navigation", "arm"]).
	// +optional
	RequiredCapabilities []string `json:"requiredCapabilities,omitempty"`

	// Constraints are parametric filter predicates (§6.10.3): each entry maps a
	// capability parameter name to the MINIMUM value a candidate robot must resolve.
	// A robot matches only if it has an Active capability whose
	// status.capabilities[].resolvedParameters[<name>] is >= the constraint (e.g.
	// constraints.maxPayloadKg: 30 requires an Active transport.payload resolving
	// maxPayloadKg >= 30). A parameter no Active capability resolves is treated as
	// unsatisfied (fail-closed), so the robot is excluded.
	// +optional
	Constraints map[string]float64 `json:"constraints,omitempty"`

	// Priority is the named scheduling band for this action (Critical/High/Normal/Low).
	// The scheduler and Traffic Deconfliction Engine compare bands by rank; Critical
	// and High may preempt a Normal or Low action on the only eligible robot
	// (RFC-0001 #fleetaction-priority-semantics) and a Normal or Low reservation at
	// a zone at capacity (RFC-0001 #tde-preemption-algorithm).
	// +kubebuilder:default=Normal
	Priority ActionPriority `json:"priority,omitempty"`

	// AcceptDegradedCapabilities, when true, lets the Scheduler satisfy this action's
	// requiredCapabilities with entries in Degraded status, not only Active. When
	// nil (unset), the namespace default
	// SwarmadaConfig.spec.scheduling.defaultAcceptDegradedCapabilities applies, and
	// a per-action value overrides that default — so this is a pointer with no static
	// CRD default (a plain defaulted bool could not distinguish unset from false).
	// This relaxes only the capability-status requirement; degraded parametric
	// constraints additionally require a capability-level degradedPolicy.
	// +optional
	AcceptDegradedCapabilities *bool `json:"acceptDegradedCapabilities,omitempty"`

	// EstimatedDurationSeconds is an optional hint specified for the Traffic
	// Deconfliction Engine's PriorityWithDuration policy (shortest-job-first within a
	// band, RFC-0001 #tde-shared-resource-queue-management). No code reads it at this
	// version: no assignment path passes it to the TDE, so it changes neither
	// shared-resource queue order nor preemption.
	// +optional
	// +kubebuilder:validation:Minimum=0
	EstimatedDurationSeconds *int32 `json:"estimatedDurationSeconds,omitempty"`

	// Payload carries action-type-specific parameters as free-form JSON.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Payload *ActionPayload `json:"payload,omitempty"`

	// Deadline is the latest time by which this action must be assigned. A Pending
	// action whose deadline passes is moved to Failed with failureReason "deadline
	// exceeded before scheduling". An action that is already Assigned is not failed
	// on its deadline, even if it never reaches InProgress. The deadline is sent to
	// the adapter on the assignment.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// TimeoutSeconds is the maximum duration, measured from status.startedAt, in
	// which this action must complete. An InProgress action whose window has passed
	// is failed once its robot is proven off it, and not while a stop holds the robot,
	// Resuming included (RFC-0001 #fleetaction-state-machine). For held work, which a
	// stop held, only its adapter's acknowledgement of the cancel proves it off.
	// It is also consulted by
	// SwarmadaConfig.actionCancellation.onDisconnect=WhenActionExpired (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect): a
	// Revoking (disconnected) action whose completion window has passed is
	// auto-cancelled rather than reassigned, since expiry renders completion moot,
	// unless it is held work (RFC-0001 #safety-action-table, Terms).
	// A action with no TimeoutSeconds, or one that never started, cannot expire.
	// +optional
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`

	// RobotSelector pins the action to a specific robot by name.
	// When set, the scheduler skips capability/zone matching.
	// +optional
	RobotSelector string `json:"robotSelector,omitempty"`

	// MinBatteryPct requires a candidate robot's status.batteryPercent to be at or
	// above this value (scheduler hard filter). Unset imposes no battery floor.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MinBatteryPct *int32 `json:"minBatteryPct,omitempty"`

	// PreferredRobot is a soft scheduling preference naming ONE robot: among
	// otherwise-eligible robots, that robot is ranked first. Not a hard filter, and
	// deliberately not RobotSelector.
	//
	// The distinction is the whole point. RobotSelector PINS: the named robot or
	// nothing. PreferredRobot expresses "this one if it can, otherwise whoever can" —
	// which is what a person means when they start a task on their own device. Because
	// it ranks rather than filters, every eligibility filter still applies to the
	// preferred robot: it wins only if it is genuinely able to do the work, and the
	// fleet silently takes over when it is busy, out of zone, or lacks the capability.
	//
	// Honoured only when the namespace sets
	// SwarmadaConfig.spec.scheduling.honorPreferredRobot (default true). Naming a robot
	// that does not exist, or one that is not eligible, is not an error — the hint
	// simply has no effect.
	// +optional
	PreferredRobot string `json:"preferredRobot,omitempty"`

	// PreferredManufacturer is a soft scheduling preference: among otherwise-eligible
	// robots, those whose manufacturer matches are ranked first. Not a hard filter.
	// +optional
	PreferredManufacturer string `json:"preferredManufacturer,omitempty"`

	// OnFailure selects what happens when the action reaches the Failed phase.
	// Requeue resets the action to Pending and applies RetryPolicy; Alert leaves it
	// Failed and emits a FleetActionFailed event for operator review; Abandon leaves
	// it Failed silently. An exhausted Requeue falls through to Alert behaviour.
	// +optional
	// +kubebuilder:validation:Enum=Requeue;Alert;Abandon
	// +kubebuilder:default=Requeue
	OnFailure ActionFailurePolicy `json:"onFailure,omitempty"`

	// RetryPolicy governs requeue-on-failure and is only consulted when OnFailure
	// is Requeue. When omitted, the controller applies the field defaults
	// (maxRetries=3, backoffSeconds=30).
	// +optional
	RetryPolicy *ActionRetryPolicy `json:"retryPolicy,omitempty"`

	// DesiredState is the declarative control intent (Running/Paused/Returning/Cancelled),
	// reconciled to the safe-hold / cancel / recover paths. Operator- and trigger-settable on a
	// standalone action; for an action owned by a FleetTask the composite is authoritative and
	// reconciles it to the action's desiredState. Paused on an Assigned or InProgress action
	// sends the robot cancel_action so that it stops where it is, with the adapter's answer
	// in status.message. The exits from Paused, and the proof a requeue waits for, are rows
	// A2 to A6 of RFC-0001 #safety-action-table.
	//
	// LEVEL-TRIGGERED IN ONE DIRECTION ONLY, and this asymmetry is deliberate (ADR-0045).
	// Paused and Returning ENTER a hold; writing Running back does NOT lift one. A held
	// action leaves Paused only by the rows that table lists (a requeue or cancel through
	// the verb-gated intake, or the move of unstarted work off an unconfirmed stop),
	// because Paused has one set of exits whatever produced it —
	// an estop or this field — and an operator reading phase: Paused must never have to
	// ask which path produced it before deciding whether it will move on its own.
	// Cancelled is the one value that is level-triggered in both directions, because
	// cancellation is terminal and there is nothing to lift.
	// +kubebuilder:validation:Enum=Running;Paused;Returning;Cancelled
	// +kubebuilder:default=Running
	// +optional
	DesiredState DesiredState `json:"desiredState,omitempty"`
}

// ActionPayload carries action-type-specific parameters (RFC-0001
// #fleetaction-the-payload-field). Raw holds the bytes delivered to the adapter as
// payload_json.
//
// It is encoded by hand (fleetaction_payload.go), not by the field tag: the object an
// author writes under spec.payload IS the payload, so its keys survive a typed read and
// every controller copy. The one exception is the RFC-0001 spelling, an object whose only
// key is `raw` holding a base64 string, which is read as that string's decoded bytes and
// written back in the same form. The field tag remains so the generated schema keeps
// declaring `raw`, which that spelling needs.
type ActionPayload struct {
	// Raw is the raw JSON bytes of the payload.
	Raw []byte `json:"raw,omitempty"`

	// rawForm records that the payload was written as {"raw": <base64>}, so it is
	// written back that way rather than rewritten into its decoded keys.
	rawForm bool `json:"-"`
}

// ActionExecutor directs this action to a ComputeEndpoint instead of to a
// robot. Its presence is the discriminator on FleetActionSpec (CEL, above).
// There is no dispatch path at v0.4. Nothing in the reference control plane
// resolves an endpoint address, activates a run, or calls the compute runtime service.
type ActionExecutor struct {
	// EndpointRef names a ComputeEndpoint in this namespace. The endpoint is
	// pre-created by an administrator; a task never describes an executor inline.
	// +kubebuilder:validation:MinLength=1
	EndpointRef string `json:"endpointRef"`

	// Parameters binds each declared ComputeEndpoint parameter to exactly one
	// source. Keyed by the parameter name the endpoint declares.
	// +optional
	Parameters map[string]ParameterBinding `json:"parameters,omitempty"`

	// Inputs are the non-scalar inputs this run supplies: a sensor stream, or
	// an artifact reference.
	// +optional
	// +listType=map
	// +listMapKey=name
	Inputs []ExecutorInput `json:"inputs,omitempty"`

	// Sinks names ResultSinks this run may write to. Each MUST
	// exist in this namespace and accept the endpoint's declared results —
	// admission checks this (G1). There is no field ANYWHERE in a FleetAction
	// carrying a connection string or a credential. The control plane is specified
	// to resolve a named Sink's credentialRef and pass a short-lived, sink-scoped,
	// lease-bounded grant to the executor, which writes the row itself. At v0.4
	// no controller starts an executor run, and no grant is issued.
	// +optional
	// +listType=set
	Sinks []string `json:"sinks,omitempty"`

	// CompletionMode is RunToCompletion for an App contract and Continuous for a
	// Service contract. Specified to be defaulted from the referenced
	// ComputeEndpoint's contract, with an explicit value that contradicts it
	// rejected. Neither is implemented at v0.4: no webhook or controller reads
	// this field or the endpoint's contract for it, so an omitted value stays
	// absent and any value in the enum is accepted, whatever the endpoint's
	// contract.
	// +optional
	CompletionMode *CompletionMode `json:"completionMode,omitempty"`

	// OnUnresolvedParameter is specified to select the consequence when a
	// binding cannot be resolved at activation: Fail, UseDefault or Omit, and
	// when unset, Fail for a required parameter, UseDefault for an optional one
	// with a default, Omit otherwise. At v0.4 the value is checked against that
	// enum and stored, and no controller reads it: no controller activates an
	// executor run, so none of the three is applied, whatever the field is set
	// to. What happens to an unresolved binding at v0.4 does not depend on this
	// field. On a member that a FleetTask spec.flow starts, the FleetTask
	// controller creates the child FleetAction anyway, leaves each unresolved
	// fromMember binding in the child's spec.executor.parameters as written,
	// and records the miss and its reason in the child's
	// status.parameterResolution (resolved: false). A fromMember binding on any
	// other action, and every fromTaskInput binding, is resolved by no
	// controller and gets no status.parameterResolution entry; it stays in the
	// spec as written.
	// +optional
	OnUnresolvedParameter *UnresolvedParameterPolicy `json:"onUnresolvedParameter,omitempty"`
}

// ExecutorInput is one non-scalar input bound to a run.
//
// +kubebuilder:validation:XValidation:rule="has(self.sensorStream)",message="inputs[].sensorStream is the only input kind implemented; an input without sensorStream, an artifact input included, is refused"
type ExecutorInput struct {
	// Name matches a name the referenced ComputeEndpoint declares in
	// spec.inputs[].
	Name string `json:"name"`

	// SensorStream binds this input to a named robot sensor stream. It is the
	// only input kind at v0.4, and a CEL rule on ExecutorInput requires it on
	// every entry.
	// +optional
	SensorStream *SensorStreamInput `json:"sensorStream,omitempty"`
}

// SensorStreamInput names one robot sensor stream and how much of it to take.
type SensorStreamInput struct {
	// RobotRef addresses which robot's stream to consume.
	RobotRef SensorStreamRobotRef `json:"robotRef"`

	// StreamID names the stream on that robot (Robot.status.sensorStreams[*].streamId).
	// +kubebuilder:validation:MinLength=1
	StreamID string `json:"streamId"`

	// RateHz requests an activation rate. Unset defers to the stream's
	// native_rate_hz. Shared activation is specified to take the MAX rate_hz any
	// live consumer of the same (robot, stream) requests. At v0.4 no controller
	// reads this field or activates a stream for an executor.
	// +optional
	RateHz *float64 `json:"rateHz,omitempty"`

	// Window selects continuous / snapshot / clip framing. Unset defaults to
	// Continuous.
	// +optional
	Window *ClipWindowSpec `json:"window,omitempty"`
}

// SensorStreamRobotRef addresses which robot's stream to consume. Closed and
// deliberately NOT a template expression — no `{{ }}` interpolation anywhere
// (docs/api-principles.md "Cross-field validation" prefers CEL-checkable
// structure over free text; a CRD that evaluates strings at reconcile time is
// a CRD whose admission cannot tell you whether it is valid). Exactly one of
// FromMember or Name is set (CEL, below). `{selector: ...}` fan-out is
// deferred because it multiplies the result model.
//
// +kubebuilder:validation:XValidation:rule="has(self.fromMember) != has(self.name)",message="exactly one of fromMember or name must be set"
type SensorStreamRobotRef struct {
	// FromMember names a sibling FleetTask member; the robot assigned to that
	// member is the stream's source. The common case: "the robot doing the
	// patrolling".
	// +optional
	FromMember *string `json:"fromMember,omitempty"`

	// Name pins a specific robot by name.
	// +optional
	Name *string `json:"name,omitempty"`
}

// ClipWindowMode selects a sensor stream's framing.
// +kubebuilder:validation:Enum=Continuous;Snapshot;Clip
type ClipWindowMode string

// ClipWindowMode values.
const (
	ClipWindowModeContinuous ClipWindowMode = "Continuous"
	ClipWindowModeSnapshot   ClipWindowMode = "Snapshot"
	ClipWindowModeClip       ClipWindowMode = "Clip"
)

// ClipWindowSpec selects continuous / snapshot / clip framing for a sensor
// stream input.
//
// +kubebuilder:validation:XValidation:rule="self.mode == 'Clip' || (!has(self.preSeconds) && !has(self.postSeconds))",message="preSeconds/postSeconds are meaningful only when mode is Clip"
type ClipWindowSpec struct {
	// Mode selects the framing.
	// +kubebuilder:default=Continuous
	// +optional
	Mode ClipWindowMode `json:"mode,omitempty"`

	// PreSeconds is the pre-roll window. Meaningful only for Clip; bounded by
	// the source stream's declared clipPreSecondsMax at activation.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PreSeconds *int32 `json:"preSeconds,omitempty"`

	// PostSeconds is the post-roll window. Meaningful only for Clip.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PostSeconds *int32 `json:"postSeconds,omitempty"`
}

// ParameterBinding is a discriminated union: exactly one source is set. The
// exactly-one-of constraint is specified, not enforced in CEL at v0.4; it is
// deferred to the same release that adds the parameter-resolution webhook checks.
type ParameterBinding struct {
	// Value is a literal, taken as-is.
	// +optional
	Value *string `json:"value,omitempty"`

	// FromTaskInput names a FleetTask-level input (spec.inputs) to read.
	// +optional
	FromTaskInput *string `json:"fromTaskInput,omitempty"`

	// FromMember reads a value a sibling FleetTask member produced.
	// +optional
	FromMember *MemberBinding `json:"fromMember,omitempty"`

	// FromContext reads a closed set of ambient values.
	// +optional
	// +kubebuilder:validation:Enum=assignedRobot;zone;taskName;memberName;iteration;activatedAt
	FromContext *string `json:"fromContext,omitempty"`
}

// MemberBinding reads a value a sibling FleetTask member produced. Exactly one of
// Result, Event or Artifact is set (Result is valid only for an
// App-contract producer, Event only for a Service-contract producer).
type MemberBinding struct {
	// Name is the sibling member's name (FleetTaskAction.Name).
	Name string `json:"name"`

	// Result names a field in the producer's declared result (App contract).
	// +optional
	Result *string `json:"result,omitempty"`

	// Event names a semantic event type raised by the producer (Service contract).
	// +optional
	Event *string `json:"event,omitempty"`

	// Field names a field within the named Event's payload. Required with Event.
	// +optional
	Field *string `json:"field,omitempty"`

	// Artifact names an artifact the producer's result referenced.
	// +optional
	Artifact *string `json:"artifact,omitempty"`

	// Iteration disambiguates a producer inside a loop read from outside it.
	// +optional
	// +kubebuilder:validation:Enum=Latest;First;All
	Iteration *string `json:"iteration,omitempty"`
}

// ActionFailurePolicy selects the behaviour applied when a action reaches Failed.
// +kubebuilder:validation:Enum=Requeue;Alert;Abandon
type ActionFailurePolicy string

const (
	// ActionFailureRequeue resets a failed action to Pending and applies RetryPolicy.
	ActionFailureRequeue ActionFailurePolicy = "Requeue"
	// ActionFailureAlert leaves a failed action in Failed and emits an operator alert.
	ActionFailureAlert ActionFailurePolicy = "Alert"
	// ActionFailureAbandon leaves a failed action in Failed with no retry and no alert.
	ActionFailureAbandon ActionFailurePolicy = "Abandon"
)

// ActionRetryPolicy bounds requeue-on-failure retries for onFailure: Requeue.
type ActionRetryPolicy struct {
	// MaxRetries is the maximum number of times the action may be requeued after a
	// failure. When status.retryCount reaches it, the action remains Failed and
	// Alert behaviour is applied.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3
	MaxRetries int32 `json:"maxRetries,omitempty"`

	// BackoffSeconds is how long to wait after a failure before requeueing, giving
	// the robot time to recover or another robot time to become available.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=30
	BackoffSeconds int32 `json:"backoffSeconds,omitempty"`
}

// ── Status ────────────────────────────────────────────────────────────────────

// CancellationReason is why a FleetAction reached Cancelled (RFC-0001 #fleetaction-superseded-is-not-cancelled-by-an-operator). A closed
// enum: a supersession and an operator cancellation share the Cancelled phase and are
// told apart here, never by the phase.
// +kubebuilder:validation:Enum=Operator;SupersededByDirective;CapabilityLoss
type CancellationReason string

// CancellationReason values.
const (
	// CancellationReasonOperator: a person stopped the work, through
	// swarmada.io/cancel-requested under the `cancel` verb. status.supersededBy MUST
	// be empty.
	CancellationReasonOperator CancellationReason = "Operator"
	// CancellationReasonSupersededByDirective: the fleet replaced the work — a newer
	// directive for this action's robot displaced it. status.supersededBy is REQUIRED.
	CancellationReasonSupersededByDirective CancellationReason = "SupersededByDirective"
	// CancellationReasonCapabilityLoss: a capability the action required degraded.
	CancellationReasonCapabilityLoss CancellationReason = "CapabilityLoss"
)

// FleetActionStatus describes the observed state of a FleetAction.
//
// The OutcomeCertainty rule is NORMATIVE, not advisory, and is enforced here rather
// than in a controller for the same reason ActionResult's bounds are: a CRD's CEL rules
// apply to the whole object regardless of which subresource wrote it, so this holds
// against a raw status patch and a downgraded controller as well as against the
// reconciler. Unestablished is pinned to Failed — the settled fail-closed
// disposition, made structural. An unestablished outcome therefore CANNOT be
// recorded on Succeeded by any writer, which is the single failure mode that matters
// here: a consumer that reads an outcome nobody could establish as a success.
// +kubebuilder:validation:XValidation:rule="!has(self.outcomeCertainty) || self.outcomeCertainty != 'Unestablished' || self.phase == 'Failed'",message="status.outcomeCertainty: Unestablished is accepted only with status.phase: Failed — an outcome that could not be established fails closed and must never be recorded on a Succeeded, Cancelled or non-terminal action"
type FleetActionStatus struct {
	// Phase is the current lifecycle state.
	// +optional
	Phase ActionPhase `json:"phase,omitempty"`

	// OutcomeCertainty reports whether the terminal outcome in Phase is an OBSERVATION
	// or a fail-closed disposition applied because no observation was available. Set
	// only on a terminal phase; empty otherwise.
	//
	// Empty means NO CONTROLLER STATED A CERTAINTY. It is not evidence of either value
	// — the same discipline CancellationReason follows, and the same the proto follows
	// for ACTION_END_CAUSE_UNSPECIFIED ("no cause stated — NOT a claim of any cause").
	// A reader MUST NOT read an empty value as Established: every action written before
	// this field existed carries an empty one.
	//
	// Unestablished ALWAYS warrants operator attention, and is the one terminal
	// disposition of which that is unconditionally true. Succeeded needs none by
	// definition; a Failed the robot observed and reported is handled by onFailure and
	// the retry budget; but an outcome nobody can establish leaves a physical question
	// open that no policy in this API can close — the run may well have done the job.
	//
	// AT v0.4 NO CONTROLLER WRITES THIS FIELD, so it is always empty, and there is no
	// phase for an outcome the robot cannot establish. An ACTION_STATE_UNKNOWN report
	// is dropped and changes nothing: while its robot is reachable the action stays
	// Assigned or InProgress until an operator cancels it or, once InProgress,
	// spec.timeoutSeconds elapses and it is Failed. An adapter that cannot
	// establish an outcome reports ACTION_STATE_FAILED instead, and its message in
	// failureReason is the only record that the outcome is unknown. For such work,
	// set timeoutSeconds, set onFailure: Alert where a repeat run is unsafe, and read
	// failureReason before treating a Failed action as a failure.
	// +optional
	OutcomeCertainty OutcomeCertainty `json:"outcomeCertainty,omitempty"`

	// AssignedRobot is the name of the Robot this action was scheduled to. Never
	// set when spec.executor is present — an external action is never scheduled
	// to a robot (internal/scheduler early return).
	// +optional
	AssignedRobot string `json:"assignedRobot,omitempty"`

	// ExecutorRef is the resolved endpoint this run is bound to — the external
	// analogue of AssignedRobot, specified to be set only when spec.executor is
	// present. No controller writes it at v0.4: no dispatch path resolves an
	// endpoint, so it is always empty.
	// +optional
	ExecutorRef string `json:"executorRef,omitempty"`

	// AssignmentGeneration is the lease generation, drawn from the namespace's one
	// generation counter, so it is strictly monotonic across every action in the
	// namespace (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee). The controller mints a new value from that PERSISTED
	// high-water mark on every new assignment (read-before-issue), so it is
	// monotonic across a control-plane restart/failover and is never reused; a
	// reassignment increments it, never resets it. Mirrors the proto uint64;
	// stored as int64 per Kubernetes API convention (always ≥ 0).
	// +optional
	AssignmentGeneration int64 `json:"assignmentGeneration,omitempty"`

	// LeaseExpiresAt is the stamp the lease horizon is reckoned from: the moment the
	// controller committed the assignment or decided the last renewal (before it is
	// sent) plus the namespace's spec.scheduling.leaseDurationSeconds, the same
	// duration the robot is sent. The lease horizon, which reassignment is gated on,
	// is defined once, in the terms of RFC-0001
	// #safety-action-table. While the assigned robot is reachable
	// the controller renews it; while a Paused action's robot has confirmed its stop
	// and is connected it is advanced with no wire renewal; otherwise it is FROZEN.
	// Unreachability alone never releases the action.
	// +optional
	LeaseExpiresAt *metav1.Time `json:"leaseExpiresAt,omitempty"`

	// StartTime is superseded by StartedAt and is written by no controller; nothing
	// reads it. It is kept for compatibility and is not removed; read StartedAt for
	// when the action started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// PendingSince is when the action entered the Pending phase. It is set at the
	// first scheduling reconcile that finds the action Pending, and again when the
	// controller returns the action to Pending (a requeue, or an assignment it could not
	// commit), and cleared on the Assigned transition. It anchors the
	// swarmada_scheduler_assignment_latency_seconds metric (RFC-0001
	// #control-plane-observability-prometheus-metrics-contract, time-to-Assigned). It is
	// also a scheduling input: when the lease horizon of a robot that may still run the
	// action is not known, the wait before the action is given to another robot, and
	// before that robot may be deleted, counts from the latest of this time, the
	// action's creation, its completion and its failure (RFC-0001
	// #safety-assignment-lease-and-the-single-executor-guarantee). A Paused action whose
	// hold released the horizon has no such time: its robot's delete waits for the adapter.
	// +optional
	PendingSince *metav1.Time `json:"pendingSince,omitempty"`

	// DisconnectedAt is when the assigned robot was lost and the action entered
	// Revoking due to a disconnect. It is set on that entry under every
	// SwarmadaConfig.actionCancellation.onDisconnect value, including the default
	// Never, so a set value does not mean the action will be reassigned. Under
	// AfterTimeout it is the wall-clock anchor for the ceiling
	// (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect): a Revoking action auto-reassigns only once
	// now ≥ DisconnectedAt + disconnectTimeoutSeconds AND the lease is provably
	// dead, and never if it is held work (RFC-0001 #safety-action-table). Under Never it is
	// informational: the action holds Revoking until an
	// operator cancels. Cleared on re-adoption (robot returned on a live lease),
	// on reassignment, and when WhenActionExpired cancels the action.
	// +optional
	DisconnectedAt *metav1.Time `json:"disconnectedAt,omitempty"`

	// CompletionTime is when the action reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Message provides a human-readable summary of the current state,
	// especially on failure.
	// +optional
	Message string `json:"message,omitempty"`

	// ProgressPct is the robot's reported action progress (0–100), surfaced from the
	// adapter's ActionStatusUpdate.progress_pct. It is advisory telemetry, clamped to
	// [0,100]; 100 on Succeeded. Absent/0 when the adapter reports no progress. Never
	// set when spec.executor is present.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	ProgressPct int32 `json:"progressPct,omitempty"`

	// Result is the typed outcome of an external action. Written once,
	// on the transition to a terminal phase (RA-1). Unset for a robot action.
	// +optional
	Result *ActionResult `json:"result,omitempty"`

	// Events is the bounded, seq-keyed record of the semantic observations a
	// Service-contract executor raised during this run, ingested from
	// compute.v1.ComputeRuntimeService.StreamStatus. Unset for a robot action
	// and for an App-contract run, which raises no events and reports its
	// outcome once, in Result.
	//
	// AT v0.4 NOTHING SHIPPED FILLS IT. The consumer that writes this list
	// (ComputeEventIngestor, internal/controller) is not started by the
	// manager, so the list, EventSeqHigh and EventsDropped stay empty on every
	// FleetAction. The FleetTask flow evaluator does read it (a select subject,
	// a loop.until event condition, a fromMember event binding), so each of
	// those sees no event: a select on an event waits while its producer runs
	// and settles Failed once the producer settles, and an event-only
	// loop.until never fires.
	//
	// APPEND IS EDGE-TRIGGERED ON SEQ, NOT ON ARRIVAL. An event whose Seq is
	// not strictly greater than EventSeqHigh is discarded without a status
	// write and without an Event. A recognition service emits continuously, so
	// an append per received frame would be a status write per telemetry tick —
	// the defect RA-1 exists to prevent, at the highest rate anything in this
	// system produces.
	//
	// WHAT A CONSUMER IS GUARANTEED, AND WHAT IT IS NOT. This list is a bounded
	// WINDOW on the most recent 32 events, not a log and not a replay buffer:
	//
	//   - GUARANTEED: the 32 most recently accepted events, in ascending Seq
	//     order, with no duplicates and no gaps among those retained. A
	//     consumer — a select subject, a loop.until condition, a fromMember
	//     event binding — that observes the action at least once every 32
	//     events sees every event. A `terminal` event is never evicted, because
	//     nothing is raised after it.
	//   - NOT GUARANTEED: that an event a consumer is waiting for is still
	//     here. If a producer raises 32 further events between two reconciles,
	//     the earlier ones are GONE, and a select waiting on one of them will
	//     never see it. This is a real, reachable loss, stated here rather than
	//     hidden behind a bound chosen to make it unlikely.
	//   - EventsDropped is how a reader TELLS the two apart. An event that was
	//     never raised and an event that was evicted are different facts, and a
	//     consumer that waits forever deserves to know which one it is waiting
	//     on. Absence alone does not distinguish them; this counter does.
	//   - NOT RECOVERABLE BY RESUMPTION. StreamStatus.since_seq is set from
	//     EventSeqHigh — the highest Seq ever accepted — never from the oldest
	//     retained entry. Asking the executor to replay from the window's floor
	//     would re-append events already evicted, evict newer ones to make room,
	//     and re-ask for them on the next reconnect: a resume that cannot
	//     converge. Retention therefore drops events permanently, by
	//     construction, and since_seq is what makes that a bounded loss rather
	//     than an oscillation.
	//
	// An author who cannot accept the loss must bound the producer, not this
	// list: a Service member gated by a select is activated only for the time it
	// needs to function, and a run that raises 32 unread events has outlived the
	// question it was started to answer.
	// +optional
	// +listType=map
	// +listMapKey=seq
	// +kubebuilder:validation:MaxItems=32
	Events []ActionEvent `json:"events,omitempty"`

	// EventSeqHigh is the highest ActionEvent.Seq ever accepted for this action,
	// across all its runs and invocations — the high-water mark, which NEVER
	// truncates, never decreases even as Events is trimmed, and is not reset by
	// a later run. It is both halves of the idempotency rule: the
	// admission test for an incoming event (accept iff Seq > EventSeqHigh) and
	// the resume point handed to StreamStatus.since_seq on reconnect. Mirrors
	// the proto uint64; stored as int64 per Kubernetes API convention.
	// +optional
	// +kubebuilder:validation:Minimum=0
	EventSeqHigh int64 `json:"eventSeqHigh,omitempty"`

	// EventsDropped counts the events evicted from Events by the retention
	// bound. It never decreases. Without it, an event that was never raised and
	// an event that was raised and then evicted look identical to a consumer —
	// and a select that waits forever with nothing anywhere saying why is the
	// failure the flow evaluator's event arm was written to avoid, arriving by
	// a different route.
	// +optional
	// +kubebuilder:validation:Minimum=0
	EventsDropped int64 `json:"eventsDropped,omitempty"`

	// ParameterResolution records what each of spec.executor.parameters resolved
	// to, or why it did not. Bounded by the referenced
	// ComputeEndpoint's declared parameter count. Unset for a robot action.
	// At v0.4 its only writer is the FleetTask controller, which writes it
	// once, when it creates a child for a member a spec.flow starts, with one
	// entry per fromMember binding (source: member). Bindings of any other
	// source get no entry, and a FleetAction that is not a flow child keeps it
	// empty.
	// +optional
	// +listType=map
	// +listMapKey=name
	ParameterResolution []ParameterResolutionEntry `json:"parameterResolution,omitempty"`

	// ScheduledAt is when the Scheduler assigned a robot (Pending → Assigned).
	// +optional
	ScheduledAt *metav1.Time `json:"scheduledAt,omitempty"`
	// StartedAt is when the robot reported execution began (Assigned → InProgress).
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the action reached Succeeded.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// FailedAt is when the action reached Failed.
	// +optional
	FailedAt *metav1.Time `json:"failedAt,omitempty"`

	// FailureReason is a human-readable reason, set when the phase transitions to
	// Failed and empty otherwise, except ZoneOperatingConstraintUnmet, which is set
	// while the action waits in Pending. It reflects the current attempt, not history.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// CancellationReason is why this action reached Cancelled: a CLOSED enum, empty
	// when the phase is not Cancelled (RFC-0001 #fleetaction-superseded-is-not-cancelled-by-an-operator). status.phase: Cancelled alone does
	// NOT identify the cause and a reader MUST NOT infer one from it — an operator
	// stopping work and the fleet replacing it are different facts an auditor must be
	// able to separate. Empty means no controller stated a cause; it is not evidence
	// of either one.
	// +optional
	CancellationReason CancellationReason `json:"cancellationReason,omitempty"`

	// SupersededBy names the FleetAction that displaced this one, as a plain name in
	// this action's own namespace. Required when CancellationReason is
	// SupersededByDirective; empty for every other cause and for every non-Cancelled
	// phase. This is what makes a supersession STRUCTURALLY distinguishable from an
	// operator cancellation rather than only differently labelled: an operator
	// cancellation has no superseding action and can never populate it (§9.1.4.4.2).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	SupersededBy string `json:"supersededBy,omitempty"`

	// RetryCount is the number of times this action has been requeued after failure
	// (onFailure: Requeue). It never exceeds spec.retryPolicy.maxRetries.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RetryCount int32 `json:"retryCount,omitempty"`

	// Conditions is the standard conditions list.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ActionResult is a small, typed outcome. It is what a select node
// branches on and what an operator reads in `kubectl get` — it is not a data
// channel, so Values and Artifacts are bounded, and the bounds are NORMATIVE
// (enforced at admission — including a status-subresource write, since a CRD's
// CEL rules apply to the whole object regardless of which subresource wrote
// it), not advisory:
//   - values: scalars only, <=16 entries, <=4 KiB total. Nested structure
//     invites a bounding-box list, and then a point cloud.
//   - artifacts[]: references only, never inline bytes, <=8 entries.
//
// Written once, on the transition to a terminal phase (RA-1) — never per
// frame, never per tick.
//
// +kubebuilder:validation:XValidation:rule="!has(self.values) || self.values.all(k, size(self.values[k]) <= 256)",message="each result value is bounded to 256 bytes — combined with the 16-entry cap this is how the 4 KiB total is enforced without a fragile whole-map size computation in CEL"
type ActionResult struct {
	// SchemaRef names the ComputeEndpoint whose declared results[] this conforms to.
	// +optional
	SchemaRef string `json:"schemaRef,omitempty"`

	// Values are scalars only (string-encoded regardless of underlying kind, per
	// ComputeEndpointParameter.Kind), at most 16 entries, each at most 256
	// bytes (CEL, above) — 16*256=4096 bytes, the design's 4 KiB total bound.
	// Never nested — a result is not a place for a bounding-box list or a
	// point cloud.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	Values map[string]string `json:"values,omitempty"`

	// Artifacts are references — never inline bytes — at most 8 entries. Bytes
	// live in a declared ArtifactStore, written by the executor
	// on the data path; the control plane records only that they exist and
	// where.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Artifacts []ActionResultArtifact `json:"artifacts,omitempty"`

	// ProducedBy identifies the implementation — and, where one applies, the
	// model — that produced this result. Subsumes the narrower
	// "adapter-supplied detail" field once asked for separately: this IS that
	// field, generalised. Answering "which detections came from model version
	// X" depends on it.
	// +optional
	ProducedBy *ResultProducer `json:"producedBy,omitempty"`

	// ProducedAt is when the executor produced this result.
	// +optional
	ProducedAt *metav1.Time `json:"producedAt,omitempty"`
}

// ResultProducer identifies the implementation and, where one applies, the
// model that produced an ActionResult. All fields
// are advisory-populate (an App-contract producer with no model concept
// leaves the Model* fields empty) but ImplementationName/Version SHOULD
// always be set — this is the entire audit trail for "which build produced
// this row", and there is no other field it could live in.
type ResultProducer struct {
	// ImplementationName identifies the executor implementation (e.g. a
	// container image name, distinct from ComputeEndpoint.spec.endpointRef
	// which names the deployment, not the code running behind it).
	// +optional
	ImplementationName string `json:"implementationName,omitempty"`

	// ImplementationVersion is the implementation's own build/semver.
	// +optional
	ImplementationVersion string `json:"implementationVersion,omitempty"`

	// ModelName identifies the model, when the result was produced by one.
	// +optional
	ModelName string `json:"modelName,omitempty"`

	// ModelVersion is the model's version.
	// +optional
	ModelVersion string `json:"modelVersion,omitempty"`

	// ModelDigest is a content digest identifying the exact model artifact
	// (the same uri+digest discipline rollout_types.go already uses for
	// ModelChecksum/FirmwareChecksum).
	// +optional
	ModelDigest string `json:"modelDigest,omitempty"`
}

// ActionResultArtifact is a reference to executor output held externally. It
// is a REFERENCE, never a raw path: Store names a declared ArtifactStore —
// there is no field here in which a task or an executor
// could write a hostPath, a node name, or a bare filesystem path. A bare
// `file://…` is not a reference: it is a path with no filesystem attached,
// and two consumers mounting different volumes would each "resolve" it to
// different bytes or none.
type ActionResultArtifact struct {
	// Name identifies this artifact within the result (e.g. "frame").
	Name string `json:"name"`

	// Store names the ArtifactStore this artifact's bytes live in. Required:
	// this is what makes Path resolvable and locality declared, rather than
	// implicit in whatever produced the artifact.
	// +kubebuilder:validation:MinLength=1
	Store string `json:"store"`

	// Path is the artifact's path within Store.
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// URI is OPTIONAL, rendered for humans (e.g. "s3://…") — never the
	// authoritative reference; Store+Path is.
	// +optional
	URI string `json:"uri,omitempty"`

	// MediaType is the artifact's IANA media type (e.g. "image/jpeg").
	// +optional
	MediaType string `json:"mediaType,omitempty"`

	// Digest is a content digest (e.g. "sha256:…"), REQUIRED whatever the
	// backing store: it is what makes a reference verifiable, and the only
	// part that survives the artifact being copied from an edge directory to
	// a bucket — the normal lifecycle for anything kept as evidence.
	// +kubebuilder:validation:MinLength=1
	Digest string `json:"digest"`

	// CapturedAt is when the underlying observation was captured, distinct from
	// ProducedAt (when the executor finished processing it).
	// +optional
	CapturedAt *metav1.Time `json:"capturedAt,omitempty"`

	// Pose is the robot pose at capture time, when applicable.
	// +optional
	Pose *ArtifactPose `json:"pose,omitempty"`
}

// ArtifactPose is a 2D pose in a named frame, carried on an artifact so a
// downstream consumer does not have to correlate position separately.
type ArtifactPose struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Theta float64 `json:"theta"`
	// Frame names the reference frame (e.g. "map").
	// +optional
	Frame string `json:"frame,omitempty"`
}

// ActionEvent is one semantic observation a Service-contract executor raised
// during this run: the control-plane record of a fleet_adapter.v1.TaskEvent
// received over compute.v1.ComputeRuntimeService.StreamStatus.
//
// It carries no subject kind. Every event stored here was raised BY THIS
// ACTION, which is the only value EventSubjectKind has and is what keeps Seq's
// documented scope ("monotonic per (subject_kind, subject id)") intact: the
// subject is the object the list is stored on. A later execution kind is an
// additive enum value on the wire and a separate decision here, not a
// reinterpretation of this list.
//
// Payload is flattened to scalars for the same reason ActionResult.Values is:
// this is what a select branches on and what a parameter binding reads, not a
// data channel. A TaskEvent payload entry whose value is an object or an array
// is DROPPED on ingest rather than JSON-encoded into a string — a consumer that
// reads a field must get a value it can compare, or nothing it can mistake for
// one.
//
// +kubebuilder:validation:XValidation:rule="!has(self.payload) || self.payload.all(k, size(self.payload[k]) <= 256)",message="each event payload value is bounded to 256 bytes — combined with the 16-entry cap this is how the 4 KiB total is enforced without a fragile whole-map size computation in CEL"
type ActionEvent struct {
	// Type is the semantic event type the executor raised (TaskEvent.type),
	// e.g. "IntruderDetected". Opaque to the control plane; it MUST appear in
	// the producing ComputeEndpoint's spec.emitsEvents, which admission checks
	// for a select subject that names it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Type string `json:"type"`

	// Seq is the executor's monotonic sequence number for this run
	// (TaskEvent.seq) and the idempotency key: an event whose Seq is not
	// strictly greater than status.eventSeqHigh is DISCARDED, never appended
	// and never rewritten. This is what makes the ingest edge-triggered rather
	// than a status write per received frame (RA-1). Mirrors the proto uint64;
	// stored as int64 per Kubernetes API convention (always >= 0).
	// +kubebuilder:validation:Minimum=0
	Seq int64 `json:"seq"`

	// TimeMs is the executor's own timestamp for the observation
	// (TaskEvent.time_ms, Unix milliseconds). It is the EXECUTOR's clock, not
	// the control plane's, and is never used to order events — Seq is.
	// +optional
	TimeMs int64 `json:"timeMs,omitempty"`

	// Payload is the event's scalar payload, bounded exactly as
	// ActionResult.Values is: at most 16 entries, each at most 256 bytes
	// (CEL, below) — the design's 4 KiB total. Never nested.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	Payload map[string]string `json:"payload,omitempty"`

	// Terminal marks the event that ends a Continuous execution
	// (TaskEvent.terminal). At most one such event exists per run, and it
	// cannot be evicted by retention: nothing is raised after it.
	// +optional
	Terminal bool `json:"terminal,omitempty"`
}

// ParameterResolutionEntry records the resolution outcome for one declared
// spec.executor.parameters entry.
type ParameterResolutionEntry struct {
	// Name is the parameter name.
	Name string `json:"name"`

	// Source is the ParameterBinding source that was attempted.
	// +kubebuilder:validation:Enum=value;taskInput;member;context
	Source string `json:"source"`

	// Resolved reports whether a value was obtained.
	Resolved bool `json:"resolved"`

	// Value is the resolved value (string-encoded), set only when Resolved is true.
	// +optional
	Value *string `json:"value,omitempty"`

	// Reason explains why resolution failed, set only when Resolved is false.
	// +optional
	Reason string `json:"reason,omitempty"`

	// FromMember names the sibling FleetTask member the value was read from,
	// set only for Source: member. WHICH MEMBER RAISED THE OBSERVATION IS PART
	// OF THE OBSERVATION: an action started because something was recognised
	// must be able to say what recognised it, and the resolved value alone
	// cannot — two members can raise the same event type with the same payload
	// field and mean different things.
	// +optional
	FromMember string `json:"fromMember,omitempty"`

	// FromEvent names the semantic event type the value was read from, set only
	// for an event binding (Source: member with fromMember.event).
	// +optional
	FromEvent string `json:"fromEvent,omitempty"`

	// FromEventSeq is the Seq of the specific ActionEvent the value was read
	// from, set only alongside FromEvent. It pins the resolution to ONE
	// observation: the producer keeps raising events after this action starts,
	// and without the seq an auditor cannot tell which frame the parameter came
	// from. Always >= 0; 0 is a real sequence number, so read it only when
	// FromEvent is non-empty.
	// +optional
	// +kubebuilder:validation:Minimum=0
	FromEventSeq int64 `json:"fromEventSeq,omitempty"`
}

// ── CRD root object ───────────────────────────────────────────────────────────

// FleetAction is the Schema for the fleetactions API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fact
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Verb",type=string,JSONPath=".spec.customType"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Outcome",type=string,JSONPath=".status.outcomeCertainty"
// +kubebuilder:printcolumn:name="Priority",type=string,JSONPath=".spec.priority"
// +kubebuilder:printcolumn:name="Robot",type=string,JSONPath=".status.assignedRobot"
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=".spec.zone"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type FleetAction struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetActionSpec   `json:"spec,omitempty"`
	Status FleetActionStatus `json:"status,omitempty"`
}

// FleetActionList contains a list of FleetAction.
// +kubebuilder:object:root=true
type FleetActionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetAction `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FleetAction{}, &FleetActionList{})
}
