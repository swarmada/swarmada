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
	"errors"
	"testing"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func TestCancelDispositionOf(t *testing.T) {
	cases := map[fav1.CancelDisposition]CancelDisposition{
		fav1.CancelDisposition_CANCEL_DISPOSITION_UNSPECIFIED:    CancelStoppedSafely,
		fav1.CancelDisposition_CANCEL_DISPOSITION_STOPPED_SAFELY: CancelStoppedSafely,
		fav1.CancelDisposition_CANCEL_DISPOSITION_COMPLETED:      CancelCompleted,
		fav1.CancelDisposition_CANCEL_DISPOSITION_RECOVERED:      CancelRecovered,
	}
	for in, want := range cases {
		if got := cancelDispositionOf(in); got != want {
			t.Fatalf("cancelDispositionOf(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestPushAssignAction_AcceptedRoundTrip(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(),
			RobotId:   cmd.GetRobotId(),
			Result: &fav1.CommandResult_AssignAction{AssignAction: &fav1.AssignActionResult{
				Accepted: true, AcceptedFencingToken: u64ptr(7),
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{
		ActionID: "pick-1", ActionType: "PickUp", FencingToken: 7, LeaseGeneration: 7, LeaseDurationMs: 30000,
	})
	if err != nil {
		t.Fatalf("PushAssignAction: %v", err)
	}
	if !out.Accepted || out.AcceptedFencingToken != 7 {
		t.Fatalf("outcome = %+v", out)
	}
	// The fencing token / lease generation were delivered on the wire.
	at := sender.sent[0].GetAssignAction()
	if at == nil || at.GetFencingToken() != 7 || at.GetLeaseGeneration() != 7 || at.GetActionId() != "pick-1" {
		t.Errorf("pushed assign_action = %+v", at)
	}
}

// The payload must reach the wire on the ASSIGNMENT (§9.1.4.5), not only on
// validate_action. proto field 3 `destination` is deprecated in favour of
// payload_json, so this is the only field that can carry a Navigate's destination.
func TestPushAssignAction_CarriesPayloadOnTheWire(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(),
			RobotId:   cmd.GetRobotId(),
			Result: &fav1.CommandResult_AssignAction{AssignAction: &fav1.AssignActionResult{
				Accepted: true, AcceptedFencingToken: u64ptr(1),
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	payload := []byte(`{"destination":{"x":3,"y":4}}`)
	if _, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{
		ActionID: "nav-1", ActionType: "Navigate", FencingToken: 1, Payload: payload,
	}); err != nil {
		t.Fatalf("PushAssignAction: %v", err)
	}

	at := sender.sent[0].GetAssignAction()
	if at == nil {
		t.Fatal("no assign_action on the wire")
	}
	if got := string(at.GetPayloadJson()); got != string(payload) {
		t.Errorf("payload_json = %q, want %q", got, payload)
	}
}

// A nil payload sends no bytes rather than an empty non-nil slice, so an adapter can
// tell "no payload" from "empty payload" (docs/api-principles.md explicit presence).
func TestPushAssignAction_NilPayloadSendsNoBytes(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(),
			RobotId:   cmd.GetRobotId(),
			Result: &fav1.CommandResult_AssignAction{AssignAction: &fav1.AssignActionResult{
				Accepted: true, AcceptedFencingToken: u64ptr(1),
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	if _, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{
		ActionID: "nav-1", ActionType: "Navigate", FencingToken: 1,
	}); err != nil {
		t.Fatalf("PushAssignAction: %v", err)
	}

	if got := sender.sent[0].GetAssignAction().GetPayloadJson(); got != nil {
		t.Errorf("payload_json = %#v, want nil when no payload is set", got)
	}
}

func TestPushAssignAction_RejectedReportsRejection(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(),
			Result: &fav1.CommandResult_AssignAction{AssignAction: &fav1.AssignActionResult{
				Accepted: false, Rejection: fav1.AssignActionRejection_ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN,
			}}}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{ActionID: "t", FencingToken: 1})
	if err != nil {
		t.Fatalf("a rejection is a valid outcome, not an error: %v", err)
	}
	if out.Accepted || out.Rejection == "" {
		t.Errorf("outcome = %+v, want not-accepted with a rejection", out)
	}
}

func TestPushAssignAction_NoStreamIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	if _, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{ActionID: "t"}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestPushRenewLease_RoundTrip(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(),
			Result: &fav1.CommandResult_RenewLease{RenewLease: &fav1.RenewActionLeaseResult{
				Renewed: true, Running: true, CurrentGeneration: 4,
			}}}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushRenewLease(context.Background(), cmdNS, cmdRobotID, RenewLease{ActionID: "t", LeaseGeneration: 4, LeaseDurationMs: 30000})
	if err != nil {
		t.Fatalf("PushRenewLease: %v", err)
	}
	if !out.Renewed || !out.Running || out.CurrentGeneration != 4 {
		t.Fatalf("outcome = %+v", out)
	}
	rl := sender.sent[0].GetRenewLease()
	if rl == nil || rl.GetLeaseGeneration() != 4 || rl.GetActionId() != "t" {
		t.Errorf("pushed renew_lease = %+v", rl)
	}
}

func TestPushRenewLease_NoStreamIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	if _, err := d.PushRenewLease(context.Background(), cmdNS, cmdRobotID, RenewLease{ActionID: "t"}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestPushCancelAction_AcknowledgedRoundTrip(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(),
			Result: &fav1.CommandResult_CancelAction{CancelAction: &fav1.CancelActionResult{Acknowledged: true}}}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushCancelAction(context.Background(), cmdNS, cmdRobotID, CancelAction{ActionID: "t", Reason: "operator", FencingToken: 5})
	if err != nil {
		t.Fatalf("PushCancelAction: %v", err)
	}
	if !out.Acknowledged {
		t.Fatalf("outcome = %+v, want acknowledged", out)
	}
	ct := sender.sent[0].GetCancelAction()
	if ct == nil || ct.GetActionId() != "t" || ct.GetFencingToken() != 5 || ct.GetReason() != "operator" {
		t.Errorf("pushed cancel_action = %+v", ct)
	}
}

func TestPushCancelAction_NoStreamIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	if _, err := d.PushCancelAction(context.Background(), cmdNS, cmdRobotID, CancelAction{ActionID: "t"}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}
