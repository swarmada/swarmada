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

// staleTermSender answers every Command with stale_leader_term set and no result arm: the
// adapter has seen a higher leader term and did not act on it (RFC-0001
// #fleet-adapter-protocol-the-stop-latch, The leader term).
func staleTermSender(d *Dispatcher) *fakeSender {
	return &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(), StaleLeaderTerm: true}
	}}
}

// A Command rejected for its leader term is not acted on, and the caller is told so: the error
// is ErrStaleLeaderTerm and ErrUnreachable, never an outcome, and never a refusal.
func TestStaleLeaderTerm_IsReadAsNotActedOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		push func(d *Dispatcher) error
	}{
		{"assign", func(d *Dispatcher) error {
			out, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{
				ActionID: "pick-1", ActionType: "PickUp", FencingToken: 7, LeaseGeneration: 7, LeaseDurationMs: 30000,
			})
			if err == nil && !out.Accepted {
				return errors.New("a stale-term rejection was read as the robot's refusal (accepted = false)")
			}
			return err
		}},
		{"renew", func(d *Dispatcher) error {
			_, err := d.PushRenewLease(context.Background(), cmdNS, cmdRobotID, RenewLease{ActionID: "pick-1", LeaseGeneration: 7, LeaseDurationMs: 30000})
			return err
		}},
		{"cancel", func(d *Dispatcher) error {
			_, err := d.PushCancelAction(context.Background(), cmdNS, cmdRobotID, CancelAction{ActionID: "pick-1"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newDispatcher(t, cmdAdapter)
			d.Term = func() (uint64, bool) { return 3, true }
			d.RegisterStream(verifiedIdentity(), staleTermSender(d))
			err := tc.push(d)
			if !errors.Is(err, ErrStaleLeaderTerm) || !errors.Is(err, ErrUnreachable) {
				t.Fatalf("err = %v; want ErrStaleLeaderTerm, which is ErrUnreachable", err)
			}
		})
	}
}
