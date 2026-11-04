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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/leadership"
	"github.com/swarmada/swarmada/internal/probe"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// "The control plane MUST set the sending leader's term on every Command" (RFC-0001
// #fleet-adapter-protocol-the-stop-latch, The leader term), and a process that does not hold
// leadership sends nothing (RFC-0001 #safety-robot-stop-table). roundTripTo is the one send
// site, so a probe exercises the rule for every Command type.

func healthy(cmd *fav1.Command) *fav1.CommandResult {
	return &fav1.CommandResult{CommandId: cmd.GetCommandId(),
		Result: &fav1.CommandResult_Verify{Verify: &fav1.VerifyResult{Status: fav1.ProbeStatus_PROBE_STATUS_HEALTHY}}}
}

func verify(d *Dispatcher) error {
	_, err := d.Verify(context.Background(), cmdNS, cmdRobotID,
		probe.VerifyRequest{ProbeType: fleetv1.ProbeTypeHardware, Target: "t"})
	return err
}

func TestSend_EveryCommandCarriesTheLeaderTerm(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	state := &leadership.State{}
	if !state.Acquire(12) {
		t.Fatal("Acquire refused")
	}
	d.Term = state.Term
	sender := &fakeSender{dispatch: d, respond: healthy}
	d.RegisterStream(verifiedIdentity(), sender)

	if err := verify(d); err != nil {
		t.Fatalf("Verify on the leader: %v", err)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.sent) != 1 || sender.sent[0].GetLeaderTerm() != 12 {
		t.Fatalf("sent %v; want one Command carrying leader_term 12", sender.sent)
	}
}

func TestSend_ANonLeaderSendsNoCommand(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	state := &leadership.State{}
	if !state.Acquire(3) {
		t.Fatal("Acquire refused")
	}
	state.Release()
	d.Term = state.Term
	sender := &fakeSender{dispatch: d, respond: healthy}
	d.RegisterStream(verifiedIdentity(), sender)

	err := verify(d)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Verify after leadership was lost = %v, want ErrUnreachable", err)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.sent) != 0 {
		t.Fatalf("a process that does not hold leadership sent %d Commands", len(sender.sent))
	}
}

func TestSend_ATermIsStampedOnTheTermHeldAtTheSend(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	state := &leadership.State{}
	d.Term = state.Term
	sender := &fakeSender{dispatch: d, respond: healthy}
	d.RegisterStream(verifiedIdentity(), sender)
	for _, term := range []uint64{1, 2, 9} {
		if !state.Acquire(term) {
			t.Fatalf("Acquire(%d) refused", term)
		}
		if err := verify(d); err != nil {
			t.Fatalf("Verify under term %d: %v", term, err)
		}
		state.Release()
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.sent) != 3 {
		t.Fatalf("sent %d, want 3", len(sender.sent))
	}
	for i, want := range []uint64{1, 2, 9} {
		if got := sender.sent[i].GetLeaderTerm(); got != want {
			t.Fatalf("command %d carried term %d, want %d", i, got, want)
		}
	}
}

func TestSend_WithoutLeadershipWiredTheTermIsZero(t *testing.T) {
	d, _ := newDispatcher(t, cmdAdapter)
	sender := &fakeSender{dispatch: d, respond: healthy}
	d.RegisterStream(verifiedIdentity(), sender)
	if err := verify(d); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.sent[0].GetLeaderTerm() != 0 {
		t.Fatalf("term %d with no leadership wired; want 0 (no term sent)", sender.sent[0].GetLeaderTerm())
	}
}
