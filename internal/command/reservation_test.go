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

// PushReservationGranted pushes a reservation_granted Command to the promoted
// robot's adapter and binds the ReservationGrantedAck (§5.4.5).
func TestPushReservationGranted_AcknowledgedRoundTrip(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{
			CommandId: cmd.GetCommandId(),
			RobotId:   cmd.GetRobotId(),
			Result: &fav1.CommandResult_ReservationGranted{
				ReservationGranted: &fav1.ReservationGrantedAck{Acknowledged: true},
			},
		}
	}}
	d.RegisterStream(verifiedIdentity(), sender)

	ack, err := d.PushReservationGranted(context.Background(), cmdNS, cmdRobotID, "lift")
	if err != nil {
		t.Fatalf("PushReservationGranted: %v", err)
	}
	if !ack {
		t.Fatal("want acknowledged")
	}
	rg := sender.sent[0].GetReservationGranted()
	if rg == nil || rg.GetResourceName() != "lift" {
		t.Errorf("pushed reservation_granted = %+v, want resource_name=lift", rg)
	}
	if sender.sent[0].GetRobotId() != cmdRobotID {
		t.Errorf("command robot_id = %q, want %q", sender.sent[0].GetRobotId(), cmdRobotID)
	}
}

// FAIL path: no live ControlStream to the robot's adapter → ErrUnreachable, so the
// caller knows the promoted robot was not told to proceed (it stays at the boundary).
func TestPushReservationGranted_NoStreamIsUnreachable(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	if _, err := d.PushReservationGranted(context.Background(), cmdNS, cmdRobotID, "lift"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}
