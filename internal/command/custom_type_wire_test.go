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
	"testing"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A Custom action's verb reaches the adapter on the assignment itself
// (AssignAction.custom_type, contract 1.4.0). Before it, the adapter received
// `action_type: Custom` and a payload, and an adapter serving two Custom verbs could
// tell them apart only by a payload convention nothing checked against spec.customType.
func TestPushAssignAction_CarriesTheCustomVerbOnTheWire(t *testing.T) {
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
		ActionID: "explore-1", ActionType: "Custom", CustomType: "explore-floor", FencingToken: 1,
	}); err != nil {
		t.Fatalf("PushAssignAction: %v", err)
	}
	if got := sender.sent[0].GetAssignAction().GetCustomType(); got != "explore-floor" {
		t.Errorf("assign_action.custom_type = %q, want %q", got, "explore-floor")
	}
}

// validate_action is the same question asked before commitment, so it carries the same
// verb: an adapter that judged `Custom` without it would answer for a verb it was never
// told, and the assignment that follows would carry one.
func TestValidateAction_CarriesTheCustomVerbOnTheWire(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(),
			RobotId:   cmd.GetRobotId(),
			Result: &fav1.CommandResult_ValidateAction{ValidateAction: &fav1.ValidateActionResult{
				Servable: true,
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	if _, err := d.ValidateAction(context.Background(), cmdNS, cmdRobotID,
		"Custom", "explore-floor", []byte(`{}`)); err != nil {
		t.Fatalf("ValidateAction: %v", err)
	}
	if got := sender.sent[0].GetValidateAction().GetCustomType(); got != "explore-floor" {
		t.Errorf("validate_action.custom_type = %q, want %q", got, "explore-floor")
	}
}
