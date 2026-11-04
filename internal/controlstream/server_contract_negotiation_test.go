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
	"sync"
	"testing"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The contract version is negotiated per connection (RFC-0001
// #fleet-adapter-protocol-package-and-versioning): "A compatible connection negotiates 1.4.0 when
// the adapter reported a 1.4 version, and the version the adapter reported otherwise, so a 1.3.x
// adapter's connection negotiates 1.3.x and is held to it." Before this, every compatible
// connection was told 1.4.0, so a 1.3.0 adapter was answered with a contract it does not implement.

// Each compatible report is answered with the version agreed for that connection; an incompatible
// one with none. A 1.3.x connection is still compatible: it registers.
func TestContractNegotiation_HelloAckAnswersTheVersionAgreedForTheConnection(t *testing.T) {
	for _, tc := range []struct{ reported, want string }{
		{"1.3.0", "1.3.0"},
		{"1.3.2", "1.3.2"},
		{"1.4.0", "1.4.0"},
		{"1.4.7", "1.4.0"},
		{"1.5.0", ""},
		{"", ""},
	} {
		t.Run(tc.reported, func(t *testing.T) {
			reg := &fakeRegistrar{}
			stream, ack := gateStream(t, &Server{Registrar: reg}, tc.reported)
			if !ack.GetAccepted() {
				t.Fatalf("hello ack = %+v, want accepted: the connection stands whatever the version", ack)
			}
			if got := ack.GetNegotiatedContractVersion(); got != tc.want {
				t.Fatalf("negotiated_contract_version = %q for a reported %q, want %q", got, tc.reported, tc.want)
			}
			if tc.want == "" {
				return
			}
			if rack := sendRegister(t, stream); !rack.GetAccepted() {
				t.Errorf("register ack = %+v, want accepted: a %s connection is compatible", rack, tc.want)
			}
		})
	}
}

// Below 1.4.0 the control plane sends no ActionOutcomeAck (RFC-0001, Current version: the arm is a
// 1.4.0 addition). The outcome is still read; it is only the answer that a 1.3.x adapter does not
// implement that is not sent.
func TestContractNegotiation_NoActionOutcomeAckBelow140(t *testing.T) {
	for _, tc := range []struct {
		reported string
		wantAck  bool
	}{{"1.3.0", false}, {"1.4.0", true}} {
		t.Run(tc.reported, func(t *testing.T) {
			ing := &retainedIngestor{ack: true}
			stream, _ := gateStream(t, &Server{Registrar: &fakeRegistrar{}, ActionStatus: ing}, tc.reported)
			if err := stream.Send(retainedReport("a1", 7, true)); err != nil {
				t.Fatalf("send: %v", err)
			}
			got := nextAfter(t, stream)
			acked := len(got) == 1 && got[0].GetOutcomeAck() != nil
			if acked != tc.wantAck || (!tc.wantAck && len(got) != 0) {
				t.Fatalf("messages before the RegisterAck on a %s connection = %v; want an ActionOutcomeAck: %v",
					tc.reported, got, tc.wantAck)
			}
			ing.mu.Lock()
			defer ing.mu.Unlock()
			if len(ing.retained) != 1 {
				t.Fatalf("retained outcome read %d times, want 1: the version decides the answer, not the read",
					len(ing.retained))
			}
		})
	}
}

// contractCommands is a CommandDispatcher that records the version each stream was told.
type contractCommands struct {
	mu   sync.Mutex
	told []string
}

func (c *contractCommands) RegisterStream(TLSIdentity, CommandSender) func()     { return func() {} }
func (c *contractCommands) RouteResult(CommandSender, *fav1.CommandResult) error { return nil }
func (c *contractCommands) StreamNegotiated(_ CommandSender, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.told = append(c.told, version)
}

// The command dispatcher is told the version agreed for each ControlStream before the stream's
// first message is read, so row A15's send check never meets a stream whose version it lacks.
func TestContractNegotiation_TheCommandDispatcherIsToldEachStreamsVersion(t *testing.T) {
	for _, tc := range []struct{ reported, want string }{{"1.3.0", "1.3.0"}, {"1.4.7", "1.4.0"}, {"9.0.0", ""}} {
		t.Run(tc.reported, func(t *testing.T) {
			cmds := &contractCommands{}
			stream, _ := gateStream(t, &Server{Registrar: &fakeRegistrar{}, Commands: cmds}, tc.reported)
			sendRegister(t, stream) // answered in order: the stream's messages are being read
			cmds.mu.Lock()
			defer cmds.mu.Unlock()
			if len(cmds.told) != 1 || cmds.told[0] != tc.want {
				t.Fatalf("StreamNegotiated calls = %q, want one with %q", cmds.told, tc.want)
			}
		})
	}
}
