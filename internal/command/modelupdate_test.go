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

func modelUpdatePayload() ModelUpdate {
	return ModelUpdate{
		ModelName:  "item-recognition",
		OldVersion: "3.2.0",
		NewVersion: "3.2.1",
		ModelURI:   "oci://reg/models/item:3.2.1",
	}
}

func TestPushModelUpdate_AcknowledgedRoundTrip(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(),
			RobotId:   cmd.GetRobotId(),
			Result: &fav1.CommandResult_ModelUpdate{ModelUpdate: &fav1.ModelUpdateResult{
				Acknowledged: true, VerifiedSigner: "ci-signer", Message: "accepted",
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushModelUpdate(context.Background(), cmdNS, cmdRobotID, modelUpdatePayload())
	if err != nil {
		t.Fatalf("PushModelUpdate: %v", err)
	}
	if !out.Acknowledged || out.VerifiedSigner != "ci-signer" {
		t.Fatalf("outcome = %+v", out)
	}
	// The right payload was pushed.
	mu := sender.sent[0].GetModelUpdate()
	if mu == nil || mu.GetNewVersion() != "3.2.1" || mu.GetModelUri() != "oci://reg/models/item:3.2.1" ||
		mu.GetOldVersion() != "3.2.0" || mu.GetModelName() != "item-recognition" {
		t.Errorf("pushed model_update = %+v", mu)
	}
	if sender.sent[0].GetRobotId() != cmdRobotID {
		t.Errorf("command robot_id = %q", sender.sent[0].GetRobotId())
	}
}

func TestPushModelUpdate_NotAcknowledged(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(),
			Result: &fav1.CommandResult_ModelUpdate{ModelUpdate: &fav1.ModelUpdateResult{Acknowledged: false, Message: "busy"}}}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushModelUpdate(context.Background(), cmdNS, cmdRobotID, modelUpdatePayload())
	if err != nil {
		t.Fatalf("a declined update is a valid outcome, not an error: %v", err)
	}
	if out.Acknowledged {
		t.Error("outcome acknowledged = true, want false")
	}
}

func TestPushModelUpdate_NoStreamIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter) // robot exists, no stream registered
	_, err := d.PushModelUpdate(context.Background(), cmdNS, cmdRobotID, modelUpdatePayload())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestPushModelUpdate_UnsupportedIsError(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(), Unsupported: true}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	// An adapter that cannot apply model updates must not be treated as updating.
	if _, err := d.PushModelUpdate(context.Background(), cmdNS, cmdRobotID, modelUpdatePayload()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable for unsupported model_update", err)
	}
}
