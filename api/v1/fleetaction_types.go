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

// ActionPhase is the lifecycle state of a FleetAction (RFC-0001 §5.2.4).
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
	// ActionPhasePaused: interrupted by estop; awaits an operator decision (§5.7.2.4).
	ActionPhasePaused ActionPhase = "Paused"
	// ActionPhaseRevoking: assigned robot lost connectivity; awaiting lease expiry
	// before safe reassignment (§5.7.3.5).
	ActionPhaseRevoking ActionPhase = "Revoking"
	// ActionPhasePreempted: displaced by a higher-priority action at a shared resource
	// or zone (§5.5.6).
	ActionPhasePreempted ActionPhase = "Preempted"
)

// ActionPriority is the named scheduling band for a FleetAction. Bands are compared
// by rank (Critical > High > Normal > Low) by the scheduler and the Traffic
// Deconfliction Engine (RFC-0001 §5.5.6); the preemptor bands (Critical and High,
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

// ActionType classifies the kind of work the robot should perform.
// +kubebuilder:validation:Enum=Navigate;PickUp;DropOff;Patrol;Charge;Custom
type ActionType string

// ActionType values.
const (
	ActionTypeNavigate ActionType = "Navigate"
	ActionTypePickUp   ActionType = "PickUp"
	ActionTypeDropOff  ActionType = "DropOff"
	ActionTypePatrol   ActionType = "Patrol"
	ActionTypeCharge   ActionType = "Charge"
	ActionTypeCustom   ActionType = "Custom"
)

// DesiredState is the declarative, level-triggered control intent for a FleetAction (and, at the
// composite level, a FleetTask). A write persists and re-converges after a disconnect; re-writing
// the same value is idempotent — a dropped edge is never a lost command (RFC-0001, ADR-0007). The
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
type FleetActionSpec struct {
	// Type classifies the action.
	Type ActionType `json:"type"`

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
	// may preempt a lower band at a zone or shared resource (§5.5.6).
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

	// EstimatedDurationSeconds is an optional hint used by the Traffic Deconfliction
	// Engine's PriorityWithDuration policy (shortest-job-first within a band, §5.5.5).
	// +optional
	// +kubebuilder:validation:Minimum=0
	EstimatedDurationSeconds *int32 `json:"estimatedDurationSeconds,omitempty"`

	// Payload carries action-type-specific parameters as free-form JSON.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Payload *ActionPayload `json:"payload,omitempty"`

	// Deadline is the latest time by which this action must start.
	// Actions that cannot start before their deadline are moved to Failed.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// TimeoutSeconds is the maximum duration, measured from status.StartTime, in
	// which this action must complete. It is consulted by
	// SwarmadaConfig.actionCancellation.onDisconnect=WhenActionExpired (§9.1.11.9): a
	// Revoking (disconnected) action whose completion window has passed is
	// auto-cancelled rather than reassigned, since expiry renders completion moot.
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
	// reconciles it to the action's desiredState.
	//
	// LEVEL-TRIGGERED IN ONE DIRECTION ONLY, and this asymmetry is deliberate (ADR-0045).
	// Paused and Returning ENTER a hold; writing Running back does NOT lift one. A held
	// action stays Paused until an operator resumes, requeues, or cancels it through the
	// verb-gated intake, because Paused has a single resume rule whatever produced it —
	// an estop or this field — and an operator reading phase: Paused must never have to
	// ask which path produced it before deciding whether it will move on its own.
	// Cancelled is the one value that is level-triggered in both directions, because
	// cancellation is terminal and there is nothing to lift.
	// +kubebuilder:validation:Enum=Running;Paused;Returning;Cancelled
	// +kubebuilder:default=Running
	// +optional
	DesiredState DesiredState `json:"desiredState,omitempty"`
}

// ActionPayload carries action-type-specific parameters.
type ActionPayload struct {
	// Raw is the raw JSON bytes of the payload.
	Raw []byte `json:"raw,omitempty"`
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

// FleetActionStatus describes the observed state of a FleetAction.
type FleetActionStatus struct {
	// Phase is the current lifecycle state.
	// +optional
	Phase ActionPhase `json:"phase,omitempty"`

	// AssignedRobot is the name of the Robot this action was scheduled to.
	// +optional
	AssignedRobot string `json:"assignedRobot,omitempty"`

	// AssignmentGeneration is the per-action strictly-monotonic lease generation
	// (RFC-0001 §9.6.3.5). The controller mints a new value from this PERSISTED
	// high-water mark on every new assignment (read-before-issue), so it is
	// monotonic across a control-plane restart/failover and is never reused; a
	// reassignment increments it, never resets it. Mirrors the proto uint64;
	// stored as int64 per Kubernetes API convention (always ≥ 0).
	// +optional
	AssignmentGeneration int64 `json:"assignmentGeneration,omitempty"`

	// LeaseExpiresAt is the control-plane estimate of when the current
	// assignment's lease expires (last renewal + leaseDuration + skew). While the
	// assigned robot is reachable the controller renews it; when the robot is lost
	// the value is FROZEN and reassignment is gated on it (§9.6.3.5): a Revoking
	// action returns to Pending only once now ≥ LeaseExpiresAt, never on
	// unreachability alone (RA-4).
	// +optional
	LeaseExpiresAt *metav1.Time `json:"leaseExpiresAt,omitempty"`

	// StartTime is when the robot acknowledged the action.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// PendingSince is when the action entered (or re-entered from Revoking) the
	// Pending phase — the anchor for the swarmada_scheduler_assignment_latency_seconds
	// metric (§9.3.8, time-to-Assigned). Set at the first scheduling reconcile while
	// Pending and cleared on the Assigned transition; not otherwise consumed.
	// +optional
	PendingSince *metav1.Time `json:"pendingSince,omitempty"`

	// DisconnectedAt is when the assigned robot was lost and the action entered
	// Revoking due to a disconnect. It is the wall-clock anchor for the
	// SwarmadaConfig.actionCancellation.onDisconnect=AfterTimeout ceiling
	// (§9.1.11.9): a Revoking action auto-reassigns only once
	// now ≥ DisconnectedAt + disconnectTimeoutSeconds AND the lease is provably
	// dead. Cleared on re-adoption (robot returned on a live lease) and on
	// reassignment. Unset under onDisconnect=Never, which holds Revoking until
	// an operator cancels.
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
	// [0,100]; 100 on Succeeded. Absent/0 when the adapter reports no progress.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	ProgressPct int32 `json:"progressPct,omitempty"`

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
	// Failed and empty otherwise. It reflects the current attempt, not history.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

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

// ── CRD root object ───────────────────────────────────────────────────────────

// FleetAction is the Schema for the fleetactions API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fact
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
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
