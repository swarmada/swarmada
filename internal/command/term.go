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
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ErrStaleLeaderTerm means the adapter rejected the Command because it has seen a higher leader
// term than the one the Command carried: a newer leader exists, and the Command was not acted on
// (RFC-0001 #fleet-adapter-protocol-the-stop-latch, The leader term). It wraps ErrUnreachable, so
// every caller keeps its fail-safe path, and errors.Is tells a deposed leader from a lost adapter.
// It is never a refusal: the robot did not refuse the Command, so it is no proof for a row that
// reads a refusal (A24; RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee).
var ErrStaleLeaderTerm = fmt.Errorf("%w: the adapter has seen a higher leader term", ErrUnreachable)

// LeaderTerm reports the term of the leadership this process holds, and whether it holds it
// (leadership.State.Term). Dispatcher.Term is set to it in the manager.
type LeaderTerm func() (term uint64, leading bool)

// send stamps cmd with the sending leader's term and sends it down sender. Every Command the
// control plane sends leaves through here (roundTripTo), so this is where the rule "The
// control plane MUST set the sending leader's term on every Command" is kept (RFC-0001
// #fleet-adapter-protocol-the-stop-latch, The leader term). A stop is stamped too; the adapter
// accepts a stop whatever its term.
//
// A process that does not hold leadership sends nothing: a control-plane process sends on an
// adapter's streams only while it holds leadership (RFC-0001 #safety-robot-stop-table). With
// d.Term nil (no leadership wired, as in the package's own tests) the Command goes with term
// 0, which no adapter rejects.
func (d *Dispatcher) send(sender CommandSender, cmd *fav1.Command) error {
	if d.Term != nil {
		term, leading := d.Term()
		if !leading {
			return fmt.Errorf("%w: this control-plane process does not hold leadership, so it sends no command", ErrUnreachable)
		}
		cmd.LeaderTerm = term
	}
	if err := sender.Send(&fav1.ControlPlaneMessage{
		Payload: &fav1.ControlPlaneMessage_Command{Command: cmd},
	}); err != nil {
		return fmt.Errorf("%w: send: %v", ErrUnreachable, err)
	}
	return nil
}

// termChecked reads the CommandResult of cmd for the leader-term rejection. A result that sets
// stale_leader_term is the adapter's word that it did not act on cmd: it is logged and returned
// as ErrStaleLeaderTerm, never as the result, so no caller reads its empty arm as an answer. The
// process does not give up leadership on it: the Lease decides leadership, and the term only
// protects the robot.
func termChecked(ctx context.Context, robot *fleetv1.Robot, cmd *fav1.Command, res *fav1.CommandResult) (*fav1.CommandResult, error) {
	if !res.GetStaleLeaderTerm() {
		return res, nil
	}
	log.FromContext(ctx).Info("command rejected: the adapter has seen a higher leader term",
		"adapter", robot.Spec.Adapter.Name, "robot", robot.Name, "commandID", cmd.GetCommandId(),
		"leaderTerm", cmd.GetLeaderTerm())
	return nil, fmt.Errorf("%w (command %d sent with term %d)", ErrStaleLeaderTerm, cmd.GetCommandId(), cmd.GetLeaderTerm())
}
