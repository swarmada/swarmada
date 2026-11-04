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
	"context"
	"sync"
	"testing"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/telemetry"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// THE ADAPTER HOLDS THE RULE SET ONLY IF IT WAS SENT TO THIS ADAPTER, FOR THIS ROBOT.
//
// The push memo suppresses a rule set already sent (RA-1: an unchanged tick sends
// nothing). What it remembers must be what the adapter holds, and two events break that
// without a containment change: the Robot is deleted and recreated under the same name,
// and the adapter process reconnects with no rule set. Each must be sent the set again
// (FA-R7, ADR-0059).

// A Robot recreated under the same name is sent its rule set by its first containment
// event, even though the set is the one sent to the Robot it replaced.
func TestConstraintsResend_RecreatedRobotIsSentItsRuleSetAgain(t *testing.T) {
	push := &fakeConstraintPush{}
	ward := restrictedZone("ward", wardPts, denies("camera", "patient privacy"))
	z, _ := containmentFixture(t, push, ward)
	c := z.Client
	ctx := context.Background()
	inside := &telemetry.Position{X: 60, Y: 40, Floor: 0}

	old := getRobot(t, c, "amr-1", actionNS)
	old.UID = "uid-before"
	if err := c.Update(ctx, old); err != nil {
		t.Fatalf("stamp uid: %v", err)
	}
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: inside})
	if push.count() != 1 {
		t.Fatalf("%d pushes after the entry transition, want 1", push.count())
	}

	if err := c.Delete(ctx, getRobot(t, c, "amr-1", actionNS)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	recreated := robotAt("amr-1", "rid1", "")
	recreated.UID = "uid-after"
	if err := c.Create(ctx, recreated); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	// What RobotProjectionResetter does on a change of object identity.
	z.ForgetRobot(actionNS, "rid1")
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: inside})

	if push.count() != 2 {
		t.Fatalf("%d pushes after the recreated robot's entry, want 2: the recreated Robot was "+
			"never sent the rule set of the zone it stands in", push.count())
	}
	if got := push.last().OperatingConstraints; len(got) != 1 || got[0].GetSubject() != "camera" {
		t.Errorf("pushed %v, want the ward's camera denial", got)
	}
}

// zoneConstraintSender is one adapter ControlStream. It records every zone_constraints
// Command and acknowledges it.
type zoneConstraintSender struct {
	mu   sync.Mutex
	d    *command.Dispatcher
	sent []*fav1.Command
}

func (s *zoneConstraintSender) Send(msg *fav1.ControlPlaneMessage) error {
	cmd := msg.GetCommand()
	if cmd.GetZoneConstraints() == nil {
		return nil
	}
	s.mu.Lock()
	s.sent = append(s.sent, cmd)
	s.mu.Unlock()
	go func() {
		_ = s.d.RouteResult(s, &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_ZoneConstraints{ZoneConstraints: &fav1.ZoneConstraintsAck{Acknowledged: true}},
		})
	}()
	return nil
}

func (s *zoneConstraintSender) received() []*fav1.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fav1.Command(nil), s.sent...)
}

func (s *zoneConstraintSender) waitFor(t *testing.T, n int) []*fav1.Command {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := s.received()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wireConstraintResend connects the command dispatcher's stream registration to the Zone
// Controller, as the manager does.
func wireConstraintResend(d *command.Dispatcher, z *ZoneController) {
	d.OnStreamRegistered = func(id controlstream.TLSIdentity) {
		z.ResendConstraintsForAdapter(context.Background(), id.Namespace, id.AdapterName)
	}
}

// An adapter that reconnects is sent the rule set of a robot standing still in a zone,
// with no new containment event, through the real command dispatcher. The Command names
// the robot's wire robot_id.
func TestConstraintsResend_ReconnectedAdapterIsSentTheRuleSetAgain(t *testing.T) {
	ward := restrictedZone("ward", wardPts, denies("camera", "patient privacy"))
	z, _ := containmentFixture(t, nil, ward)
	c := z.Client
	ctx := context.Background()

	robot := getRobot(t, c, "amr-1", actionNS)
	robot.Spec.Adapter = fleetv1.AdapterRef{Name: "acme"}
	if err := c.Update(ctx, robot); err != nil {
		t.Fatalf("bind adapter: %v", err)
	}
	d := command.New(c)
	z.Constraints = d
	wireConstraintResend(d, z)
	id := controlstream.TLSIdentity{Verified: true, Namespace: actionNS, AdapterName: "acme"}

	first := &zoneConstraintSender{d: d}
	deregister := d.RegisterStream(id, first)
	z.ObservePosition(ctx, telemetry.Frame{RobotID: "rid1", Position: &telemetry.Position{X: 60, Y: 40, Floor: 0}})
	if got := first.waitFor(t, 1); len(got) != 1 {
		t.Fatalf("%d zone_constraints on the first stream after the entry transition, want 1", len(got))
	}
	if got := first.received()[0].GetRobotId(); got != "rid1" {
		t.Errorf("zone_constraints named robot_id %q, want the wire id rid1", got)
	}

	// The adapter process restarts: its stream goes away and a new one registers. The
	// robot has not moved, so no containment event follows.
	deregister()
	second := &zoneConstraintSender{d: d}
	d.RegisterStream(id, second)
	got := second.waitFor(t, 1)
	if len(got) != 1 {
		t.Fatalf("%d zone_constraints on the reconnected stream, want 1: the adapter restarted "+
			"with no rule set and the plane still believes it holds one", len(got))
	}
	if ocs := got[0].GetZoneConstraints().GetOperatingConstraints(); len(ocs) != 1 || ocs[0].GetSubject() != "camera" {
		t.Errorf("re-sent %v, want the ward's camera denial", ocs)
	}
	if got[0].GetRobotId() != "rid1" {
		t.Errorf("re-sent zone_constraints named robot_id %q, want rid1", got[0].GetRobotId())
	}

	// A robot of another adapter, or one standing in no zone, is not sent anything.
	other := robotAt("amr-2", "rid2", "")
	other.Spec.Adapter = fleetv1.AdapterRef{Name: "other"}
	idle := robotAt("amr-3", "rid3", "")
	idle.Spec.Adapter = fleetv1.AdapterRef{Name: "acme"}
	for _, r := range []*fleetv1.Robot{other, idle} {
		if err := c.Create(ctx, r); err != nil {
			t.Fatalf("create %s: %v", r.Name, err)
		}
	}
	other.Status.ContainingZones = []string{"ward"}
	if err := c.Status().Update(ctx, other); err != nil {
		t.Fatalf("place amr-2 in the ward: %v", err)
	}
	third := &zoneConstraintSender{d: d}
	d.RegisterStream(id, third)
	time.Sleep(200 * time.Millisecond)
	for _, cmd := range third.waitFor(t, 1) {
		if cmd.GetRobotId() != "rid1" {
			t.Errorf("reconnect re-sent zone_constraints to %q, which is not acme's robot in a zone", cmd.GetRobotId())
		}
	}
}
