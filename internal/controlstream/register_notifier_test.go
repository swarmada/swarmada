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

package controlstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/swarmada/swarmada/internal/contract"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// notifyingCommands is a CommandDispatcher that records each processed register.
type notifyingCommands struct {
	mu         sync.Mutex
	registered []string
}

func (n *notifyingCommands) RegisterStream(TLSIdentity, CommandSender) func()     { return func() {} }
func (n *notifyingCommands) RouteResult(CommandSender, *fav1.CommandResult) error { return nil }
func (n *notifyingCommands) RobotRegistered(_ context.Context, _ CommandSender, namespace, robotID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.registered = append(n.registered, namespace+"/"+robotID)
}

func (n *notifyingCommands) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.registered...)
}

// The server tells the command dispatcher a robot's register was processed only after it has sent
// an accepting RegisterAck, so the fences of the stops in force go down the stream before any
// work (RFC-0001 #safety-robot-stop-table, Sending a stop again). A refused register is not one.
func TestServer_TellsTheCommandDispatcherOfAProcessedRegister(t *testing.T) {
	cmds := &notifyingCommands{}
	stream, _ := gateStream(t, &Server{Registrar: &fakeRegistrar{}, Commands: cmds}, contract.Version)
	if ack := sendRegister(t, stream); !ack.GetAccepted() {
		t.Fatalf("register ack = %+v, want accepted", ack)
	}
	// The hook runs after the ack is sent, so the ack can arrive first: wait for it, bounded.
	for end := time.Now().Add(2 * time.Second); len(cmds.got()) == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	if got := cmds.got(); len(got) != 1 {
		t.Fatalf("RobotRegistered calls = %v, want one for the accepted register", got)
	}

	refused := &notifyingCommands{}
	stream, _ = gateStream(t, &Server{Registrar: rejectAllRegistrar{}, Commands: refused}, contract.Version)
	if ack := sendRegister(t, stream); ack.GetAccepted() {
		t.Fatalf("register ack = %+v, want refused", ack)
	}
	time.Sleep(100 * time.Millisecond)
	if got := refused.got(); len(got) != 0 {
		t.Fatalf("RobotRegistered called for a refused register: %v", got)
	}
}
