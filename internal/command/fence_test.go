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
	"fmt"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The fence and its order with AssignActions (RFC-0001 #safety-action-table, Rules for every A
// row; #fleet-adapter-protocol-the-stop-latch): an AssignAction is sent only if no stop holds the
// robot, and the check and the fences share the stream's order. On a new ControlStream the fences
// of the stops in force go first, before any AssignAction, and no AssignAction is sent before the
// robot's register is processed on that stream.

func acceptAll(d *Dispatcher) *fakeSender {
	return &fakeSender{dispatch: d, respond: func(cmd *fav1.Command) *fav1.CommandResult {
		if cmd.GetAssignAction() == nil {
			return nil
		}
		return &fav1.CommandResult{CommandId: cmd.GetCommandId(), Result: &fav1.CommandResult_AssignAction{
			AssignAction: &fav1.AssignActionResult{Accepted: true, AcceptedFencingToken: u64ptr(7)}}}
	}}
}

func assignOnce(d *Dispatcher) error {
	_, err := d.PushAssignAction(context.Background(), cmdNS, cmdRobotID, AssignAction{
		ActionID: "pick-1", ActionType: "PickUp", FencingToken: 7, LeaseGeneration: 7, LeaseDurationMs: 30000})
	return err
}

func cmdRobot(t *testing.T, c client.Client) *fleetv1.Robot {
	t.Helper()
	r := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: cmdNS, Name: cmdRobotID}, r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Run under -race: whatever the interleaving, an AssignAction sent is on the wire ahead of the
// fence, or it is not sent.
func TestFence_AnAssignActionIsAheadOfTheFenceOrNotSent(t *testing.T) {
	for i := 0; i < 2000; i++ {
		d, c := newDispatcher(t, cmdAdapter)
		s := acceptAll(d)
		d.RegisterStream(verifiedIdentity(), s)
		robot := cmdRobot(t, c)
		var wg sync.WaitGroup
		var assignErr error
		wg.Add(2)
		go func() { defer wg.Done(); assignErr = assignOnce(d) }()
		go func() {
			defer wg.Done()
			_ = d.SendFence(robot, fmt.Sprintf("stop-%d", i), &fav1.Estop{Reason: "hazard"})
		}()
		wg.Wait()
		s.mu.Lock()
		fenced := false
		for _, cmd := range s.sent {
			switch {
			case cmd.GetEstop() != nil:
				fenced = true
			case cmd.GetAssignAction() != nil && fenced:
				s.mu.Unlock()
				t.Fatalf("iteration %d: an AssignAction went on the wire after the fence", i)
			}
		}
		sentAssign := false
		for _, cmd := range s.sent {
			sentAssign = sentAssign || cmd.GetAssignAction() != nil
		}
		s.mu.Unlock()
		if !sentAssign && !errors.Is(assignErr, ErrHeldByStop) {
			t.Fatalf("iteration %d: the AssignAction was not sent, and the error is %v, not ErrHeldByStop", i, assignErr)
		}
	}
}

// The fence is the stop's Estop as Command.estop, under the stop's ID, stamped with the leader
// term; once the stop is in force no AssignAction is sent, and a clear lets one through again.
func TestFence_IsTheStopsEstopAndHoldsWorkUntilTheClear(t *testing.T) {
	d, c := newDispatcher(t, cmdAdapter)
	d.Term = func() (uint64, bool) { return 5, true }
	s := acceptAll(d)
	d.RegisterStream(verifiedIdentity(), s)
	if err := d.SendFence(cmdRobot(t, c), "6f1c2b1e-9a43-4f3e-8d55-0f3f1c2d9e10", &fav1.Estop{Reason: "hazard", IssuedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	if len(s.sent) != 1 || s.sent[0].GetEstop().GetEstopId() != "6f1c2b1e-9a43-4f3e-8d55-0f3f1c2d9e10" ||
		s.sent[0].GetLeaderTerm() != 5 || s.sent[0].GetRobotId() != cmdRobotID {
		t.Fatalf("sent %v; want one Command.estop under the stop's ID, term 5, for the robot", s.sent)
	}
	if err := assignOnce(d); !errors.Is(err, ErrHeldByStop) || len(s.sent) != 1 {
		t.Fatalf("assign under a stop in force: err %v, %d commands on the wire; want ErrHeldByStop and nothing sent", err, len(s.sent))
	}
	d.ClearHeld(cmdNS, cmdRobotID)
	if err := assignOnce(d); err != nil || len(s.sent) != 2 {
		t.Fatalf("assign after the clear: err %v, %d commands; want it sent", err, len(s.sent))
	}
}

// With RequireRegister, no AssignAction reaches a robot before its register is processed on the
// stream; once it is, the fences of the stops it records go down that stream first.
func TestFence_ANewStreamCarriesTheFencesBeforeAnyWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		estop   fleetv1.RobotEstopState
		stops   []fleetv1.RobotStop
		fences  int
		assigns bool
	}{
		{"no-stop", fleetv1.RobotEstopNormal, nil, 0, true},
		{"recorded-stop-in-force", fleetv1.RobotEstopStopped,
			[]fleetv1.RobotStop{{ID: "s1", Scope: fleetv1.StopScopeRobot, SentAt: metav1.Now()}}, 1, false},
		{"cleared-robot-resuming", fleetv1.RobotEstopResuming,
			[]fleetv1.RobotStop{{ID: "s1", Scope: fleetv1.StopScopeRobot, SentAt: metav1.Now()}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, c := newDispatcher(t, cmdAdapter)
			d.RequireRegister = true
			robot := cmdRobot(t, c)
			robot.Status.EstopState, robot.Status.Stops = tc.estop, tc.stops
			if err := c.Update(context.Background(), robot); err != nil {
				t.Fatal(err)
			}
			s := acceptAll(d)
			d.RegisterStream(verifiedIdentity(), s)
			if err := assignOnce(d); !errors.Is(err, ErrNotRegistered) || len(s.sent) != 0 {
				t.Fatalf("before the register: err %v, %d commands; want ErrNotRegistered and nothing sent", err, len(s.sent))
			}
			d.RobotRegistered(context.Background(), s, cmdNS, cmdRobotID)
			err := assignOnce(d)
			fences, firstAssign := 0, -1
			for i, cmd := range s.sent {
				if cmd.GetEstop() != nil {
					fences++
					if firstAssign >= 0 {
						t.Fatalf("a fence went after an AssignAction: %v", s.sent)
					}
				}
				if cmd.GetAssignAction() != nil && firstAssign < 0 {
					firstAssign = i
				}
			}
			if fences != tc.fences || (firstAssign >= 0) != tc.assigns {
				t.Fatalf("%d fences, assign sent=%v (err %v); want %d fences, assign sent=%v", fences, firstAssign >= 0, err, tc.fences, tc.assigns)
			}
		})
	}
}
