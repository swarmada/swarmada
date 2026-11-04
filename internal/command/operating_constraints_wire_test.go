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

// FA-R2 — carry, ON THE WIRE, and FA-R5's refusal coming back along it.
//
// The controller's own tests establish that the dispatch path FILLS the constraint set
// from the resolved projection. These establish the next link: that the transport puts
// them on the actual AssignAction message an adapter receives, and that the subjects an
// adapter names in its refusal reach the plane rather than being flattened into the
// generic rejection string. A decline that does not name the subject is not testable and
// does not satisfy FA-R5; a decline the plane receives named and discards is no better to
// the operator reading the object.

func wardCameraRule() *fav1.EffectiveOperatingConstraint {
	return &fav1.EffectiveOperatingConstraint{
		Subject:       "camera",
		Mode:          fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_DENIED,
		Reason:        "patient privacy",
		DeclaringZone: "ward-icu",
	}
}

// The constraint set reaches the ADAPTER'S message, resolved and whole.
func TestPushAssignAction_CarriesTheOperatingConstraintsOnTheWire(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_AssignAction{AssignAction: &fav1.AssignActionResult{
				Accepted: true, AcceptedFencingToken: u64ptr(1),
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	if _, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{
		ActionID: "nav-1", ActionType: "Navigate", FencingToken: 1,
		OperatingConstraints: []*fav1.EffectiveOperatingConstraint{wardCameraRule()},
	}); err != nil {
		t.Fatalf("PushAssignAction: %v", err)
	}

	at := sender.sent[0].GetAssignAction()
	if at == nil {
		t.Fatal("no assign_action on the wire")
	}
	got := at.GetOperatingConstraints()
	if len(got) != 1 {
		t.Fatalf("operating_constraints on the wire = %v, want one camera rule. FA-R2: the "+
			"control plane MUST send the resolved effective operating rules on AssignAction; "+
			"an adapter cannot decline a rule it was never sent", got)
	}
	// RESOLVED, NEVER RAW: every field an adapter needs to refuse usefully travels with
	// the rule, including the DECLARING zone — very often not the target, and the object
	// an operator must actually edit.
	if got[0].GetSubject() != "camera" ||
		got[0].GetMode() != fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_DENIED ||
		got[0].GetDeclaringZone() != "ward-icu" || got[0].GetReason() != "patient privacy" {
		t.Errorf("the rule arrived incomplete: %+v", got[0])
	}
}

// No rule in force sends NO constraint, not an empty-but-present one that an adapter
// might read as "everything is denied". Absence of a rule is permission (ADR-0058 §2).
func TestPushAssignAction_NoRuleInForceSendsNoConstraint(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
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
	if got := sender.sent[0].GetAssignAction().GetOperatingConstraints(); len(got) != 0 {
		t.Errorf("an unrestricted dispatch carried %v; empty means no rule is in force, "+
			"which is PERMISSION — the message must not invent one", got)
	}
}

// FA-R5's refusal, coming back. The subjects the adapter named must reach the plane:
// naming them is the whole of what makes the obligation observable, and the plane is
// where an operator reads them.
func TestPushAssignAction_ADeclineCarriesTheUnhonourableSubjectsBack(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		// L3's own predicate: decline every subject on the assignment this adapter did
		// not declare it honours. This adapter declares nothing.
		var unhonourable []string
		for _, c := range cmd.GetAssignAction().GetOperatingConstraints() {
			unhonourable = append(unhonourable, c.GetSubject())
		}
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_AssignAction{AssignAction: &fav1.AssignActionResult{
				Accepted:             false,
				Rejection:            fav1.AssignActionRejection_ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET,
				UnhonourableSubjects: unhonourable,
				Message:              "this build cannot switch the camera off",
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{
		ActionID: "nav-1", ActionType: "Navigate", FencingToken: 1,
		OperatingConstraints: []*fav1.EffectiveOperatingConstraint{wardCameraRule()},
	})
	if err != nil {
		t.Fatalf("PushAssignAction: %v", err)
	}
	if out.Accepted {
		t.Fatal("the adapter declined and the outcome says accepted")
	}
	if out.Rejection != "ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET" {
		t.Errorf("rejection = %q; the five refusal shapes are what separate 'cannot honour' "+
			"from 'busy', and only this one means another candidate should be tried",
			out.Rejection)
	}
	if len(out.UnhonourableSubjects) != 1 || out.UnhonourableSubjects[0] != "camera" {
		t.Fatalf("unhonourableSubjects = %v, want [camera]. A decline that does not name the "+
			"subject is not testable and does not satisfy FA-R5 — and a plane that receives "+
			"the name and drops it leaves the operator exactly as unable to act",
			out.UnhonourableSubjects)
	}
}

// An ACCEPTED assignment carries no subjects back. unhonourable_subjects is meaningful
// only alongside the decline, and reading it on an acceptance would invent a refusal.
func TestPushAssignAction_AnAcceptanceNamesNoUnhonourableSubject(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_AssignAction{AssignAction: &fav1.AssignActionResult{
				Accepted: true, AcceptedFencingToken: u64ptr(1),
				UnhonourableSubjects: []string{"camera"}, // a confused adapter
			}},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{
		ActionID: "nav-1", ActionType: "Navigate", FencingToken: 1,
		OperatingConstraints: []*fav1.EffectiveOperatingConstraint{wardCameraRule()},
	})
	if err != nil {
		t.Fatalf("PushAssignAction: %v", err)
	}
	if len(out.UnhonourableSubjects) != 0 {
		t.Errorf("an ACCEPTED assignment reported unhonourable subjects %v. The field is "+
			"meaningful only alongside the decline; carrying it up from an acceptance would "+
			"make the plane withhold a robot that took the work", out.UnhonourableSubjects)
	}
}
