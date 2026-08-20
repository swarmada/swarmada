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
// §9.6.3): it delivers a action to a robot with the fencing token / lease generation
// the adapter uses to reject a stale assignment. FencingToken and LeaseGeneration
// are the FleetAction's assignmentGeneration (the monotonic per-action token).
type AssignAction struct {
	ActionID        string
	ActionType      string
	FencingToken    uint64
	LeaseGeneration uint64
	LeaseDurationMs uint32
	Priority        int32
	// DeadlineMs is an absolute deadline in unix-millis; 0 means no deadline.
	DeadlineMs int64
	// Payload is the raw JSON of FleetAction.spec.payload, delivered on the
	// assignment as AssignAction.payload_json (§9.1.4.5). It is the ONLY channel
	// for an action's parameters: proto field 3 `destination` is deprecated
	// ("destination now travels in payload_json"), so an assignment without this
	// reaches the robot with nothing to act on — a Navigate with no destination.
	//
	// The adapter also sees the payload at validate_action, but §9.1.4.5 requires
	// it to read the payload from the ASSIGNMENT rather than relying on what it
	// retained at validation: validation is pure inspection that may have run
	// against a different robot, or not at all.
	//
	// nil means "no payload" and is sent as no bytes; it is never an empty
	// non-nil slice (docs/api-principles.md explicit presence).
	Payload []byte
}

// AssignActionOutcome is the adapter's assign_action acknowledgement.
type AssignActionOutcome struct {
	Accepted             bool
	Rejection            string
	AcceptedFencingToken uint64
	Message              string
}

// RenewLease is the payload for a server→adapter renew_lease Command (§9.6.3.5):
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

// CancelAction is the payload for a server→adapter cancel_action Command (§9.6.3): it
// asks the adapter to stop and cancel a action. FencingToken is the action's
// assignmentGeneration, so the adapter can reject a stale cancel.
type CancelAction struct {
	ActionID     string
	Reason       string
	FencingToken uint64
}

// CancelDisposition is how the robot handled a capability-loss cancel_action
// (mirrors fleet_adapter.v1.CancelDisposition). UNSPECIFIED/STOPPED_SAFELY both
// map to CancelStoppedSafely, so a legacy adapter behaves exactly as before.
type CancelDisposition int

// CancelDisposition values reported on CancelActionResult (§9.6.3.5): how far the robot got before
// the cancel took effect. The control plane uses this to decide whether the action can be requeued.
const (
	CancelStoppedSafely CancelDisposition = iota // safe stop (also UNSPECIFIED)
	CancelCompleted                              // robot finished the action
	CancelRecovered                              // recovered a mid-commitment robot; action should Fail
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
	default: // UNSPECIFIED or STOPPED_SAFELY
		return CancelStoppedSafely
	}
}

// ActionCommander pushes action-lifecycle Commands (assign/renew-lease/cancel) to a
// robot's adapter over ControlStream. [Dispatcher] satisfies it. These pushes are
// the control-plane→robot delivery of an assignment, its lease renewal, and its
// cancellation; the authoritative single-executor lease state stays server-side
// (§9.6.3.5) — the caller uses assign/renew best-effort, and treats a cancel ack
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
		PayloadJson:     a.Payload,
		FencingToken:    u64ptr(a.FencingToken),
		LeaseGeneration: u64ptr(a.LeaseGeneration),
		LeaseDurationMs: a.LeaseDurationMs,
		Priority:        a.Priority,
		// assignment_id (proto field 6, "UUID for audit correlation") is deliberately
		// left unset: nothing on either side of the wire reads it today — the safety
		// audit log has no assignment event to correlate against — so choosing an
		// identifier scheme for it is a design decision with no consumer to validate
		// it. Populating it is a separate change from carrying the payload.
	}
	if a.DeadlineMs != 0 {
		d := a.DeadlineMs
		msg.DeadlineMs = &d
	}
	res, err := d.roundTrip(ctx, namespace, robotID, &fav1.Command{Command: &fav1.Command_AssignAction{AssignAction: msg}})
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
