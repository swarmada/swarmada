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

// FA-R7 on the wire (ADR-0059). The controller's own tests establish that containment
// is derived and aggregated correctly; these establish that what it derived reaches the
// message an adapter receives, and that the adapter's answer reaches the plane.

func uvRequired() *fav1.EffectiveOperatingConstraint {
	return &fav1.EffectiveOperatingConstraint{
		Subject:       "uv",
		Mode:          fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_REQUIRED,
		Reason:        "terminal clean; the lamp must be running",
		DeclaringZone: "terminal-clean",
	}
}

func TestPushZoneConstraints_CarriesTheSetAndTheZonesOnTheWire(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_ZoneConstraints{
				ZoneConstraints: &fav1.ZoneConstraintsAck{Acknowledged: true},
			},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushZoneConstraints(context.Background(), cmdNS, cmdRobotID, ZoneConstraints{
		ContainingZones:      []string{"bay-1", "ward-a"},
		OperatingConstraints: []*fav1.EffectiveOperatingConstraint{uvRequired()},
	})
	if err != nil {
		t.Fatalf("PushZoneConstraints: %v", err)
	}
	if !out.Acknowledged {
		t.Fatalf("outcome = %+v, want acknowledged", out)
	}

	zc := sender.sent[0].GetZoneConstraints()
	if zc == nil {
		t.Fatal("no zone_constraints on the wire")
	}
	// THE SET, not the arbitrated winner: a robot inside two overlapping zones is inside
	// both, and an adapter naming the zone a rule came from needs both names.
	if got := zc.GetContainingZones(); len(got) != 2 || got[0] != "bay-1" || got[1] != "ward-a" {
		t.Errorf("containing_zones = %v, want [bay-1 ward-a]", got)
	}
	rules := zc.GetOperatingConstraints()
	if len(rules) != 1 {
		t.Fatalf("operating_constraints = %v, want the uv requirement", rules)
	}
	// REQUIRED TRAVELS. A push that only ever forbade would implement half the enum:
	// entering a zone can oblige a subject to be ON.
	if rules[0].GetMode() != fav1.OperatingConstraintMode_OPERATING_CONSTRAINT_MODE_REQUIRED {
		t.Errorf("mode = %s, want REQUIRED", rules[0].GetMode())
	}
	if rules[0].GetDeclaringZone() != "terminal-clean" || rules[0].GetReason() == "" {
		t.Errorf("the rule arrived incomplete: %+v — an adapter refusing or logging needs the "+
			"zone an operator must edit and the words they wrote", rules[0])
	}
}

// AN EMPTY SET IS PERMISSION AND IS SENT AS ONE. It is how the plane says a robot has
// LEFT a restricted region; a message that omitted it would leave the adapter holding
// the last rule set it received and make every zone boundary one-way.
func TestPushZoneConstraints_AnEmptySetIsSentAndMeansPermission(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_ZoneConstraints{
				ZoneConstraints: &fav1.ZoneConstraintsAck{Acknowledged: true},
			},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	if _, err := d.PushZoneConstraints(context.Background(), cmdNS, cmdRobotID, ZoneConstraints{}); err != nil {
		t.Fatalf("PushZoneConstraints: %v", err)
	}
	zc := sender.sent[0].GetZoneConstraints()
	if zc == nil {
		t.Fatal("an empty set sent no message at all; leaving a restricted region is a fact the " +
			"adapter must be told, not silence")
	}
	if len(zc.GetOperatingConstraints()) != 0 {
		t.Errorf("an empty set carried %v; the message must not invent a rule",
			zc.GetOperatingConstraints())
	}
}

// FZ-R6 TRAVELS. A containing zone whose own chain did not resolve makes the set
// INCOMPLETE, and the adapter is told which zones rather than handed a partial answer
// that looks whole — an unresolvable restriction is not an absent restriction.
func TestPushZoneConstraints_UnresolvedZonesTravel(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_ZoneConstraints{
				ZoneConstraints: &fav1.ZoneConstraintsAck{Acknowledged: true},
			},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	if _, err := d.PushZoneConstraints(context.Background(), cmdNS, cmdRobotID, ZoneConstraints{
		ContainingZones: []string{"bay-1", "ward-a"},
		UnresolvedZones: []string{"ward-a"},
	}); err != nil {
		t.Fatalf("PushZoneConstraints: %v", err)
	}
	if got := sender.sent[0].GetZoneConstraints().GetUnresolvedZones(); len(got) != 1 || got[0] != "ward-a" {
		t.Fatalf("unresolved_zones = %v, want [ward-a]", got)
	}
}

// AN ADAPTER MAY DECLINE THE COMMAND. zone_constraints is OPTIONAL, on the same footing
// as zone_admission: an adapter built before it exists answers unsupported=true and is
// not in breach.
func TestPushZoneConstraints_AnUnsupportedDeclineIsNotAnError(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(), Unsupported: true,
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushZoneConstraints(context.Background(), cmdNS, cmdRobotID, ZoneConstraints{})
	if err != nil {
		t.Fatalf("an optional command an adapter declines must not be an error: %v", err)
	}
	if !out.Unsupported || out.Acknowledged {
		t.Fatalf("outcome = %+v, want unsupported and not acknowledged", out)
	}
}

// THE SUBJECTS AN ADAPTER CANNOT HONOUR COME BACK, and the plane can report them. Unlike
// the same list on AssignActionResult it prevents nothing: the robot is already in the
// zone, and there is no assignment to decline. Reportable, not preventable — which is
// the crossing lag, arriving as data rather than as a disclaimer.
func TestPushZoneConstraints_UnhonourableSubjectsComeBack(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		var cannot []string
		for _, c := range cmd.GetZoneConstraints().GetOperatingConstraints() {
			cannot = append(cannot, c.GetSubject())
		}
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_ZoneConstraints{
				ZoneConstraints: &fav1.ZoneConstraintsAck{
					Acknowledged: true, UnhonourableSubjects: cannot,
					Message: "this build cannot switch the uv lamp on",
				},
			},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	out, err := d.PushZoneConstraints(context.Background(), cmdNS, cmdRobotID, ZoneConstraints{
		OperatingConstraints: []*fav1.EffectiveOperatingConstraint{uvRequired()},
	})
	if err != nil {
		t.Fatalf("PushZoneConstraints: %v", err)
	}
	if len(out.UnhonourableSubjects) != 1 || out.UnhonourableSubjects[0] != "uv" {
		t.Fatalf("unhonourableSubjects = %v, want [uv]", out.UnhonourableSubjects)
	}
}
