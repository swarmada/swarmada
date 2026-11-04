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

package command

import (
	"context"
	"fmt"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// AssignAction is the payload for a server→adapter assign_action Command (RFC-0001
// RFC-0001 #safety-connectivity-loss-behaviour): it delivers a action to a robot with the fencing token / lease generation
// the adapter uses to reject a stale assignment. FencingToken and LeaseGeneration
// are the FleetAction's assignmentGeneration (the monotonic per-action token).
type AssignAction struct {
	ActionID   string
	ActionType string
	// CustomType is FleetAction.spec.customType, carried as AssignAction.custom_type
	// (contract 1.4.0): the verb a Custom action performs. Empty for every other type, and
	// for a Custom action that names none; the adapter then reads the payload's
	// `operation`, as it did before the field existed.
	CustomType      string
	FencingToken    uint64
	LeaseGeneration uint64
	LeaseDurationMs uint32
	Priority        int32
	// DeadlineMs is an absolute deadline in unix-millis; 0 means no deadline.
	DeadlineMs int64
	// Payload is the raw JSON of FleetAction.spec.payload, delivered on the
	// assignment as AssignAction.payload_json (RFC-0001 #fleetaction-the-payload-field). It is the ONLY channel
	// for an action's parameters: proto field 3 `destination` is deprecated
	// ("destination now travels in payload_json"), so an assignment without this
	// reaches the robot with nothing to act on — a Navigate with no destination.
	//
	// The adapter also sees the payload at validate_action, but RFC-0001 #fleetaction-the-payload-field requires
	// it to read the payload from the ASSIGNMENT rather than relying on what it
	// retained at validation: validation is pure inspection that may have run
	// against a different robot, or not at all.
	//
	// nil means "no payload" and is sent as no bytes; it is never an empty
	// non-nil slice (docs/api-principles.md explicit presence).
	Payload []byte

	// OperatingConstraints is FA-R2's carry: the RESOLVED EFFECTIVE operating rules
	// in force where this robot is being sent, delivered on the assignment as
	// AssignAction.operating_constraints. The adapter is never asked to walk a zone
	// tree, and never receives a rule that resolution has already superseded.
	//
	// IT IS THE PROTO TYPE AND NOT A SECOND GO MODEL OF IT, deliberately. The caller
	// builds these through the ONE converter that exists
	// (controller.operatingConstraintsForWire), whose input type is the resolved
	// projection []EffectiveZoneRestriction and which has no overload taking the spec
	// block — filling the wire from spec.restrictions would be the second resolver
	// FZ-R4 forbids. Re-modelling the message here would create a second place where
	// the axis filter and the mode mapping live, and the one that drifted would be
	// the one on the wire.
	//
	// nil means NO RULE IS IN FORCE, WHICH IS PERMISSION — never "everything is
	// denied". Absence of a rule is permission at every axis (ADR-0058 §2), and the
	// converter returns nil rather than an empty slice for exactly that reason.
	OperatingConstraints []*fav1.EffectiveOperatingConstraint
}

// AssignActionOutcome is the adapter's assign_action acknowledgement.
type AssignActionOutcome struct {
	Accepted             bool
	Rejection            string
	AcceptedFencingToken uint64
	Message              string
	// UnhonourableSubjects are the operating subjects the adapter could NOT honour,
	// carried alongside rejection = OPERATING_CONSTRAINT_UNMET (FA-R5).
	//
	// Declare, honour and attest are claims a control plane cannot verify; DECLINE is
	// an observable message on a wire, and this list is what makes it observable. A
	// decline that does not name the subject is not testable and does not satisfy
	// FA-R5, so the plane surfaces the names rather than collapsing them into the
	// generic rejection string — an operator reading "the adapter refused" cannot tell
	// which zone rule to look at, and the subject is the only thing that tells them.
	UnhonourableSubjects []string
}

// RenewLease is the payload for a server→adapter renew_lease Command (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee):
// it refreshes the robot's self-stop deadline for a action it is executing.
type RenewLease struct {
	ActionID        string
	LeaseGeneration uint64
	LeaseDurationMs uint32
}

// RenewLeaseOutcome is the adapter's renew_lease result. Renewed is false when the
// adapter rejects the renewal (e.g. a stale generation); Running reports whether
// the robot is still executing the action.
type RenewLeaseOutcome struct {
	Renewed           bool
	Running           bool
	CurrentGeneration uint64
	Message           string
}

// CancelAction is the payload for a server→adapter cancel_action Command (RFC-0001 #safety-connectivity-loss-behaviour): it
// asks the adapter to stop and cancel a action. FencingToken is the action's
// assignmentGeneration, so the adapter can reject a stale cancel.
type CancelAction struct {
	ActionID     string
	Reason       string
	FencingToken uint64
	// Cause is the MACHINE-READABLE reason, distinct from Reason's free text. It is
	// what lets an adapter report the matching ActionEndCause on the terminal
	// ActionStatusUpdate, which is in turn what makes a supersession distinguishable
	// from an operator cancellation on the object (RFC-0001
	// #fleetaction-directive-supersession). Zero value is CancelCauseUnspecified,
	// which states no cause — the pre-existing behaviour for any caller that does
	// not set it.
	Cause CancelCause
}

// CancelCause is the machine-readable cause on a cancel_action (mirrors
// fleet_adapter.v1.CancelCause). Free text in Reason is for humans and is not
// parseable; this is what the adapter branches on and echoes back.
type CancelCause int

// CancelCause values.
const (
	// CancelCauseUnspecified states no cause. A legacy caller that sets nothing
	// sends this, and the adapter is entitled to treat it as an untyped cancel.
	CancelCauseUnspecified CancelCause = iota
	// CancelCauseOperator: a person stopped it, via swarmada.io/cancel-requested
	// under the `cancel` verb.
	CancelCauseOperator
	// CancelCauseSuperseded: a newer directive for this robot replaces this action
	// (the D2 cancel-then-dispatch path).
	CancelCauseSuperseded
	// CancelCauseCapabilityLoss: a required capability degraded.
	CancelCauseCapabilityLoss
	// CancelCauseRevocation: lease revocation after connectivity loss.
	CancelCauseRevocation
)

// wireCause maps the command-layer cause onto the wire enum.
func (c CancelCause) wireCause() fav1.CancelCause {
	switch c {
	case CancelCauseOperator:
		return fav1.CancelCause_CANCEL_CAUSE_OPERATOR
	case CancelCauseSuperseded:
		return fav1.CancelCause_CANCEL_CAUSE_SUPERSEDED
	case CancelCauseCapabilityLoss:
		return fav1.CancelCause_CANCEL_CAUSE_CAPABILITY_LOSS
	case CancelCauseRevocation:
		return fav1.CancelCause_CANCEL_CAUSE_REVOCATION
	default:
		return fav1.CancelCause_CANCEL_CAUSE_UNSPECIFIED
	}
}

// CancelDisposition is how the robot handled a capability-loss cancel_action
// (mirrors fleet_adapter.v1.CancelDisposition). UNSPECIFIED/STOPPED_SAFELY both
// map to CancelStoppedSafely, so a legacy adapter behaves exactly as before.
type CancelDisposition int

// CancelDisposition values reported on CancelActionResult (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee): how far the robot got before
// the cancel took effect. The control plane uses this to decide whether the action can be requeued.
const (
	CancelStoppedSafely CancelDisposition = iota // safe stop (also UNSPECIFIED)
	CancelCompleted                              // robot finished the action
	CancelRecovered                              // recovered a mid-commitment robot; action should Fail
	// CancelStopUnconfirmed: the stop was commanded and the robot's rest could not be
	// witnessed (CANCEL_DISPOSITION_STOP_UNCONFIRMED). An acknowledgement carrying it does
	// NOT prove the lease dead: the caller treats the stop as not yet confirmed until the
	// lease has provably expired (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee).
	CancelStopUnconfirmed
)

// CancelActionOutcome is the adapter's cancel_action acknowledgement. Acknowledged
// means the adapter CONFIRMS the action is stopped/cancelled — the control plane
// treats that as a provable stop. Stale means the fencing token is behind the
// adapter's view (a newer assignment exists). Disposition refines a confirmed
// capability-loss cancel (safe stop / completed / recovered).
type CancelActionOutcome struct {
	Acknowledged bool
	Stale        bool
	Message      string
	Disposition  CancelDisposition
}

// cancelDispositionOf maps the wire enum to the command-layer enum, defaulting an
// unset/unspecified value to CancelStoppedSafely.
func cancelDispositionOf(d fav1.CancelDisposition) CancelDisposition {
	switch d {
	case fav1.CancelDisposition_CANCEL_DISPOSITION_COMPLETED:
		return CancelCompleted
	case fav1.CancelDisposition_CANCEL_DISPOSITION_RECOVERED:
		return CancelRecovered
	case fav1.CancelDisposition_CANCEL_DISPOSITION_STOP_UNCONFIRMED:
		return CancelStopUnconfirmed
	default: // UNSPECIFIED, STOPPED_SAFELY, or a value this build does not know
		return CancelStoppedSafely
	}
}

// ActionCommander pushes action-lifecycle Commands (assign/renew-lease/cancel) to a
// robot's adapter over ControlStream. [Dispatcher] satisfies it. These pushes are
// the control-plane→robot delivery of an assignment, its lease renewal, and its
// cancellation; the authoritative single-executor lease state stays server-side
// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee) — the caller uses assign/renew best-effort, and treats a cancel ack
// as a confirmed stop (never freeing a robot on an unconfirmed cancel).
type ActionCommander interface {
	PushAssignAction(ctx context.Context, namespace, robotID string, a AssignAction) (AssignActionOutcome, error)
	PushRenewLease(ctx context.Context, namespace, robotID string, r RenewLease) (RenewLeaseOutcome, error)
	PushCancelAction(ctx context.Context, namespace, robotID string, c CancelAction) (CancelActionOutcome, error)
}

var _ ActionCommander = (*Dispatcher)(nil)

// PushAssignAction pushes an assign_action Command and binds the AssignActionResult. A
// missing stream / send failure / timeout returns ErrUnreachable (wrapped).
func (d *Dispatcher) PushAssignAction(ctx context.Context, namespace, robotID string, a AssignAction) (AssignActionOutcome, error) {
	msg := &fav1.AssignAction{
		ActionId:        a.ActionID,
		ActionType:      a.ActionType,
		CustomType:      a.CustomType,
		PayloadJson:     a.Payload,
		FencingToken:    u64ptr(a.FencingToken),
		LeaseGeneration: u64ptr(a.LeaseGeneration),
		LeaseDurationMs: a.LeaseDurationMs,
		Priority:        a.Priority,
		// FA-R2 — carry. Passed through untouched: these are already the resolved
		// effective rules, folded once by the Zone Controller and converted once by
		// the caller. Nothing here filters, sorts or re-resolves them, because a
		// second opinion at this layer is a second answer.
		OperatingConstraints: a.OperatingConstraints,
		// assignment_id (proto field 6, "UUID for audit correlation") is deliberately
		// left unset. The control plane reads it nowhere — the safety audit log has no
		// assignment event to correlate against — but adapters do: the Python SDK's
		// FenceGuard.check, and the template, simulation and example-noop adapters,
		// compare it with the assignment accepted at the current fencing token to tell
		// an identical re-delivery (C3.4) from a stale one. Unset, it is "" on every
		// command, so at an equal token every re-delivery compares as the same
		// assignment. Choosing an identifier scheme is a separate change from carrying
		// the payload.
	}
	if a.DeadlineMs != 0 {
		d := a.DeadlineMs
		msg.DeadlineMs = &d
	}
	res, err := d.roundTripAssign(ctx, namespace, robotID, &fav1.Command{Command: &fav1.Command_AssignAction{AssignAction: msg}})
	if err != nil {
		return AssignActionOutcome{}, err
	}
	if res.GetUnsupported() {
		return AssignActionOutcome{}, fmt.Errorf("%w: adapter does not support assign_action", ErrUnreachable)
	}
	ar := res.GetAssignAction()
	if ar == nil {
		return AssignActionOutcome{}, fmt.Errorf("%w: CommandResult missing assign_action payload", ErrUnreachable)
	}
	out := AssignActionOutcome{
		Accepted:             ar.GetAccepted(),
		AcceptedFencingToken: ar.GetAcceptedFencingToken(),
		Message:              ar.GetMessage(),
	}
	if !ar.GetAccepted() {
		out.Rejection = ar.GetRejection().String()
		out.UnhonourableSubjects = ar.GetUnhonourableSubjects()
	}
	return out, nil
}

// PushRenewLease pushes a renew_lease Command and binds the RenewActionLeaseResult.
// A missing stream / send failure / timeout returns ErrUnreachable (wrapped).
func (d *Dispatcher) PushRenewLease(ctx context.Context, namespace, robotID string, r RenewLease) (RenewLeaseOutcome, error) {
	res, err := d.roundTrip(ctx, namespace, robotID, &fav1.Command{Command: &fav1.Command_RenewLease{RenewLease: &fav1.RenewActionLease{
		ActionId:        r.ActionID,
		LeaseGeneration: r.LeaseGeneration,
		LeaseDurationMs: r.LeaseDurationMs,
	}}})
	if err != nil {
		return RenewLeaseOutcome{}, err
	}
	if res.GetUnsupported() {
		return RenewLeaseOutcome{}, fmt.Errorf("%w: adapter does not support renew_lease", ErrUnreachable)
	}
	rr := res.GetRenewLease()
	if rr == nil {
		return RenewLeaseOutcome{}, fmt.Errorf("%w: CommandResult missing renew_lease payload", ErrUnreachable)
	}
	return RenewLeaseOutcome{
		Renewed:           rr.GetRenewed(),
		Running:           rr.GetRunning(),
		CurrentGeneration: rr.GetCurrentGeneration(),
		Message:           rr.GetMessage(),
	}, nil
}

// PushCancelAction pushes a cancel_action Command and binds the CancelActionResult. A
// missing stream / send failure / timeout returns ErrUnreachable (wrapped) — the
// caller must NOT treat that as a confirmed stop.
func (d *Dispatcher) PushCancelAction(ctx context.Context, namespace, robotID string, c CancelAction) (CancelActionOutcome, error) {
	res, err := d.roundTrip(ctx, namespace, robotID, &fav1.Command{Command: &fav1.Command_CancelAction{CancelAction: &fav1.CancelAction{
		ActionId:     c.ActionID,
		Reason:       c.Reason,
		FencingToken: u64ptr(c.FencingToken),
		Cause:        c.Cause.wireCause(),
	}}})
	if err != nil {
		return CancelActionOutcome{}, err
	}
	if res.GetUnsupported() {
		return CancelActionOutcome{}, fmt.Errorf("%w: adapter does not support cancel_action", ErrUnreachable)
	}
	cr := res.GetCancelAction()
	if cr == nil {
		return CancelActionOutcome{}, fmt.Errorf("%w: CommandResult missing cancel_action payload", ErrUnreachable)
	}
	return CancelActionOutcome{
		Acknowledged: cr.GetAcknowledged(),
		Stale:        cr.GetStale(),
		Message:      cr.GetMessage(),
		Disposition:  cancelDispositionOf(cr.GetDisposition()),
	}, nil
}

func u64ptr(v uint64) *uint64 { return &v }
