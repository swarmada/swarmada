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

// PushZoneAdmission pushes a zone_admission hold/admit Command and binds the
// ZoneAdmissionAck (§9.3.4).
func TestPushZoneAdmission_AcknowledgedRoundTrip(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(),
			RobotId:   cmd.GetRobotId(),
			Result: &fav1.CommandResult_ZoneAdmission{
				ZoneAdmission: &fav1.ZoneAdmissionAck{Acknowledged: true},
			},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	ack, err := d.PushZoneAdmission(context.Background(), cmdNS, cmdRobotID, "aisle-3", false)
	if err != nil {
		t.Fatalf("PushZoneAdmission: %v", err)
	}
	if !ack {
		t.Fatal("want acknowledged")
	}
	za := sender.sent[0].GetZoneAdmission()
	if za == nil || za.GetZoneName() != "aisle-3" || za.GetAdmit() {
		t.Errorf("pushed zone_admission = %+v, want zone=aisle-3 admit=false", za)
	}
	if sender.sent[0].GetRobotId() != cmdRobotID {
		t.Errorf("command robot_id = %q, want %q", sender.sent[0].GetRobotId(), cmdRobotID)
	}
}

// FAIL path: no live stream to the robot's adapter → ErrUnreachable (the boundary
// hold/admit could not be delivered).
func TestPushZoneAdmission_NoStreamIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	if _, err := d.PushZoneAdmission(context.Background(), cmdNS, cmdRobotID, "aisle-3", true); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}
