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

package controller

import (
	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// Directive supersession (RFC-0001 #fleetaction-directive-supersession).
//
// A DIRECTIVE is a FleetAction the control plane has selected for a specific robot.
// SUPERSESSION is the case where that robot is already executing: the control plane
// commits an assignment of B to robot R while R holds a live assignment lease for A.
//
// # THIS IS NOT PRIORITY PREEMPTION, AND THE DIFFERENCE IS THE VICTIM'S FATE
//
// Priority preemption and the TDE's reservation preemption decide WHETHER a preemptor
// may take a robot or a zone slot, and their victim is REQUEUED — Preempted → Pending,
// retryCount unchanged — because it is work the fleet still owes. Supersession decides
// HOW a handover reaches the robot once some decision has already selected B for R, and
// its victim is TERMINAL: A is replaced, not requeued.
//
// The two must not be conflated, and the code keeps them apart physically:
// markVictimPreempted is the priority path and writes Preempted; nothing in this file
// is reachable from it. A priority preemption that dispatches its preemptor to the
// evicted robot executes that dispatch through the disposition below, but the evicted
// action still returns to Pending and MUST NOT be marked superseded.

// SupersessionDisposition selects how a handover reaches the robot. The choice is made
// on spec.type ALONE — not on priority, not on adapter, not on payload.
type SupersessionDisposition int

const (
	// SupersedeInPlace (D1): B.spec.type == A.spec.type. One assign_action for B at a
	// strictly higher fencing token and a fresh lease generation; NO cancel_action, and
	// no waiting for A to reach a terminal phase. The adapter picks the safe stopping
	// point and reports A terminal with ACTION_END_CAUSE_SUPERSEDED.
	//
	// The robot MUST NOT return to Idle between the two actions — that is the observable
	// a conformance test reads to tell D1 from D2.
	SupersedeInPlace SupersessionDisposition = iota

	// SupersedeCancelThenDispatch (D2): B.spec.type != A.spec.type. The control plane
	// MUST NOT push assign_action for B until A is terminal. cancel_action for A
	// carrying CANCEL_CAUSE_SUPERSEDED → CancelActionResult → assign_action for B.
	//
	// A type change means a different executor, and handing a committed robot to one
	// without a confirmed stop is the dual-execution hazard the single-executor
	// guarantee exists to exclude.
	SupersedeCancelThenDispatch
)

// String renders the disposition as the condition reason RFC-0001 specifies for it,
// so the reason on the object and the branch in the code cannot drift apart.
func (d SupersessionDisposition) String() string {
	if d == SupersedeInPlace {
		return ConditionReasonInPlacePreemption
	}
	return ConditionReasonCancelThenDispatch
}

// Condition type and reasons for supersession (RFC-0001 #fleetaction-directive-supersession).
const (
	// ConditionSuperseded is True on an action a newer directive replaced. Its absence
	// or False is what distinguishes an operator cancellation, which never sets it.
	ConditionSuperseded = "Superseded"

	// ConditionReasonInPlacePreemption is D1: handed over with no cancel_action.
	ConditionReasonInPlacePreemption = "InPlacePreemption"
	// ConditionReasonCancelThenDispatch is D2: confirmed cancel, then dispatch.
	ConditionReasonCancelThenDispatch = "CancelThenDispatch"
)

// Kubernetes Event reasons. These are the distinguishable events: phase alone is
// `Cancelled` for both causes and MUST NOT be used to tell them apart, so an operator
// watching events sees which of the two actually happened.
const (
	// EventActionSupersededInPlace — D1: the fleet replaced this work, no cancel sent.
	EventActionSupersededInPlace = "ActionSupersededInPlace"
	// EventActionSupersededAfterCancel — D2: the fleet replaced this work after a
	// confirmed cancel.
	EventActionSupersededAfterCancel = "ActionSupersededAfterCancel"
	// EventActionCancelledByOperator — a person stopped it. Never a supersession.
	EventActionCancelledByOperator = "ActionCancelledByOperator"
)

// DispositionFor selects the supersession disposition for replacing a with b.
//
// The rule is exactly "same type in place, different type cancel first", and it is a
// pure function of the two types so it can be unit-tested without a cluster, a robot,
// or a wire. Prior art: Nav2's action-server convention splits the same way — a new
// goal of the same action type replaces the running goal in place, while a different
// action type means a different server and an explicit cancel. The reasoning carries a
// layer up: a same-type directive reaches the same executor and the robot's physical
// commitment carries over.
func DispositionFor(a, b *fleetv1.FleetAction) SupersessionDisposition {
	if a == nil || b == nil {
		return SupersedeCancelThenDispatch
	}
	return dispositionForTypes(a.Spec.Type, b.Spec.Type)
}

// dispositionForTypes is the rule itself, on the two type values.
func dispositionForTypes(aType, bType fleetv1.ActionType) SupersessionDisposition {
	if aType == bType {
		return SupersedeInPlace
	}
	return SupersedeCancelThenDispatch
}

// MaySupersede reports whether action a may be superseded at all, independent of which
// disposition would apply.
//
// # E-STOP IS NOT A PREEMPTION (RFC-0001 #safety-estop-is-not-a-preemption)
//
// An action paused by an estop or by a ZoneMaintenance window is OPERATOR-GATED. Neither
// disposition is available while A is Paused: the control plane MUST leave it at Paused
// and MUST NOT dispatch B to R until the pause resolves through its own path.
//
// Superseding a Paused action would resolve — automatically and silently — an action
// whose resolution the pause deliberately handed to a human. That is the whole reason
// the rule exists, and it is why this is a guard rather than a scheduling preference:
// the failure it prevents is a fleet quietly restarting work a person stopped.
//
// A terminal action is likewise not supersedable; there is nothing left to replace.
func MaySupersede(a *fleetv1.FleetAction) bool {
	if a == nil {
		return false
	}
	switch a.Status.Phase {
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress:
		return true
	default:
		// Paused (estop / maintenance), Pending, Preempted, Revoking and every
		// terminal phase are all excluded, each for its own reason: Paused is
		// operator-gated, Pending is not executing so there is nothing to hand over,
		// and a terminal action is already settled.
		return false
	}
}

// cancelIntent carries WHY a cancel is happening, all the way from the decision that
// made it to the object, the wire, the Kubernetes Event and the audit chain.
//
// It exists because the four used to be decided in four places from one free-text
// `reason` string, and free text is not a cause: RFC-0001 requires that a supersession
// and an operator cancellation stay distinguishable "on the object, on the wire, and in
// the audit trail", and a string that happened to read "cancelled by operator" carried
// none of that. Threading one value through is what keeps the four consistent by
// construction rather than by four authors agreeing.
type cancelIntent struct {
	// reason is the free text shown to humans in status.message and the audit detail.
	reason string
	// cause is the machine-readable cause sent on cancel_action.
	cause command.CancelCause
	// cancellationReason is written to status.cancellationReason. Empty is legitimate
	// and means NO CONTROLLER STATED A CAUSE — a reader must not read it as evidence
	// of either cause. It is deliberately left empty rather than guessed.
	cancellationReason fleetv1.CancellationReason
	// supersededBy names the superseding action. REQUIRED when cancellationReason is
	// SupersededByDirective, and MUST be empty otherwise — this is what makes the
	// distinction structural: an operator cancellation can never populate it.
	supersededBy string
	// conditionReason, when non-empty, sets the Superseded condition True with it.
	conditionReason string
	// eventReason is the Kubernetes Event reason. Empty emits no event.
	eventReason string
	// supersededAudit selects FLEETACTION_SUPERSEDED over FLEETACTION_CANCELLED.
	supersededAudit bool
}

// operatorCancelIntent is a person stopping the work, via the SAR-gated
// swarmada.io/cancel-requested annotation. That intake is definitionally a human —
// it is gated on the `cancel` verb — which is why this is the one path that may
// claim Operator.
func operatorCancelIntent(reason string) cancelIntent {
	return cancelIntent{
		reason:             reason,
		cause:              command.CancelCauseOperator,
		cancellationReason: fleetv1.CancellationReasonOperator,
		eventReason:        EventActionCancelledByOperator,
	}
}

// supersededCancelIntent is the D2 cancel-then-dispatch cancel: the fleet is replacing
// this work with byAction, of a different spec.type.
func supersededCancelIntent(byAction string) cancelIntent {
	return cancelIntent{
		reason:             "superseded by directive " + byAction,
		cause:              command.CancelCauseSuperseded,
		cancellationReason: fleetv1.CancellationReasonSupersededByDirective,
		supersededBy:       byAction,
		conditionReason:    ConditionReasonCancelThenDispatch,
		eventReason:        EventActionSupersededAfterCancel,
		supersededAudit:    true,
	}
}

// unattributedCancelIntent is a cancel whose cause is NOT one of the stated enum
// values — a composite cancelling its child through spec.desiredState, or an
// onDisconnect policy expiring a revoked action.
//
// It deliberately leaves cancellationReason EMPTY rather than labelling such a cancel
// `Operator`. spec.desiredState: Cancelled can be written by a person OR by the
// FleetTask controller's FailFast/Compensate path, and nothing at this point can tell
// which. Guessing `Operator` would put a false actor in the audit trail for every
// composite-driven cancel — precisely the confusion RFC-0001 #fleetaction-directive-supersession
// forbids — and the RFC provides for this case explicitly: empty means no controller
// stated a cause, and a reader MUST NOT infer one.
func unattributedCancelIntent(reason string, cause command.CancelCause) cancelIntent {
	return cancelIntent{reason: reason, cause: cause}
}
