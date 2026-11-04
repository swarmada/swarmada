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

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// FA-R7 — the operating rules in force WHERE THE ROBOT NOW IS (ADR-0059).
//
// # It rides an existing shape, and that was a decision
//
// ControlPlaneMessage.Command already carries ZoneAdmission: a control-plane-initiated,
// zone-scoped push on ControlStream, emitted on a containment transition and on no
// other event. That is this message's precedent in every respect that matters — the
// trigger, the direction, the stream and the best-effort delivery discipline — so this
// is a new Command variant beside it rather than a new stream or a new RPC. A second
// transport would have doubled the handshake, the fencing story and the conformance
// surface to carry one more message shape.
//
// # Why the plane evaluates and the adapter does not
//
// The alternative was to ship geometry down and let each adapter decide containment
// locally. That is how real geofencing works and it has no crossing lag — but it puts a
// SECOND EVALUATOR of the same rules in every vendor's build, which is the divergence
// FZ-R4 exists to prevent, and it makes every adapter author implement point-in-polygon
// correctly. ADR-0059 section 4 records the choice and what the rejected architecture
// would be for.

// ZoneConstraints is the payload for a server→adapter zone_constraints Command: the
// resolved, unioned operating rule set in force across every zone containing the robot.
type ZoneConstraints struct {
	// ContainingZones is every zone whose polygon contains the robot, sorted. It is a
	// SET: a robot inside two overlapping zones is inside both and is bound by both.
	ContainingZones []string
	// OperatingConstraints is the union of those zones' already-resolved projections,
	// folded most-restrictive. The same wire type AssignAction carries, so an adapter
	// implements one representation of an operating rule rather than two.
	//
	// nil means NO RULE IS IN FORCE, WHICH IS PERMISSION — never "everything is
	// denied". A push carrying none is how the plane says a robot has left a restricted
	// region, and an adapter reading it as a lockout would strand every robot that
	// walked out of a ward.
	OperatingConstraints []*fav1.EffectiveOperatingConstraint
	// UnresolvedZones names containing zones whose own restriction set could not be
	// resolved (FZ-R6). The set above is then INCOMPLETE and the adapter is told so
	// rather than handed a partial answer that looks whole: an unresolvable restriction
	// is not an absent restriction.
	UnresolvedZones []string
}

// ZoneConstraintsOutcome is the adapter's zone_constraints acknowledgement.
//
// Acknowledged is a RECEIPT AND AN ATTESTATION, NOT A MEASUREMENT. The adapter has said
// it applied the set; nothing in a control plane can verify that a camera is off
// (ADR-0058 §12). The plane records the claim and cannot check it, and an operator
// surface that renders this as "enforced" is claiming a property this contract does not
// provide.
type ZoneConstraintsOutcome struct {
	Acknowledged bool
	// UnhonourableSubjects are the subjects the adapter cannot honour where the robot
	// now is. Unlike the same list on AssignActionResult, naming them here does not
	// prevent anything: the robot is already in the zone, and there is no assignment to
	// decline. It is reportable and not preventable, and that difference is the whole
	// of the crossing lag ADR-0059 §6 states.
	UnhonourableSubjects []string
	Message              string
	// Unsupported is true when the adapter declined the command as one it does not
	// implement. zone_constraints is OPTIONAL, on the same footing as zone_admission:
	// an adapter built before it exists answers unsupported=true and is not in breach.
	Unsupported bool
}

// ZoneConstraintsPusher pushes the in-force operating set to a robot's adapter.
// [Dispatcher] satisfies it.
type ZoneConstraintsPusher interface {
	PushZoneConstraints(ctx context.Context, namespace, robotID string, c ZoneConstraints) (ZoneConstraintsOutcome, error)
}

var _ ZoneConstraintsPusher = (*Dispatcher)(nil)

// PushZoneConstraints pushes a zone_constraints Command and binds the
// ZoneConstraintsAck. A missing stream / send failure / timeout returns ErrUnreachable
// (wrapped) — the set was not delivered, and the robot is operating under whatever it
// last received.
func (d *Dispatcher) PushZoneConstraints(
	ctx context.Context, namespace, robotID string, c ZoneConstraints,
) (ZoneConstraintsOutcome, error) {
	cmd := &fav1.Command{Command: &fav1.Command_ZoneConstraints{
		ZoneConstraints: &fav1.ZoneConstraints{
			ContainingZones: c.ContainingZones,
			// Passed through untouched: these are already the resolved effective rules,
			// folded once by the Zone Controller and unioned once by the caller. Nothing
			// here filters, sorts or re-resolves them, because a second opinion at this
			// layer is a second answer.
			OperatingConstraints: c.OperatingConstraints,
			UnresolvedZones:      c.UnresolvedZones,
		},
	}}
	res, err := d.roundTrip(ctx, namespace, robotID, cmd)
	if err != nil {
		return ZoneConstraintsOutcome{}, err
	}
	if res.GetUnsupported() {
		return ZoneConstraintsOutcome{Unsupported: true}, nil
	}
	ack := res.GetZoneConstraints()
	return ZoneConstraintsOutcome{
		Acknowledged:         ack.GetAcknowledged(),
		UnhonourableSubjects: ack.GetUnhonourableSubjects(),
		Message:              ack.GetMessage(),
	}, nil
}

// NotifyZoneConstraints satisfies controller.ZoneConstraintsNotifier: an async,
// best-effort push detached from the caller's ctx.
//
// DETACHED FOR THE SAME TWO REASONS NotifyZoneAdmission IS. The Zone Controller is
// standing in a position-telemetry callback, and it must not block that callback on one
// adapter's ack — a slow adapter would otherwise stall zone derivation for every robot
// behind it. And the observer's ctx closes with the telemetry stream, which is a
// different stream from the one this push travels on; letting it cancel a push to a
// robot's own adapter would drop the message for a reason that has nothing to do with
// that robot.
//
// A DELIVERY FAILURE IS DROPPED, AND THE NEXT TRANSITION RE-PUSHES. That is the same
// discipline zone_admission follows and it has the same consequence, which is stated
// rather than hidden: a robot whose push was lost operates under the set it last
// received until its containment changes again. The plane does not retry, because a
// retry loop against an unreachable adapter is a write amplifier on exactly the path
// RA-1 protects, and because the message it would redeliver may already be stale.
func (d *Dispatcher) NotifyZoneConstraints(_ context.Context, namespace, robotID string, c ZoneConstraints) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		defer cancel()
		_, _ = d.PushZoneConstraints(ctx, namespace, robotID, c)
	}()
}
