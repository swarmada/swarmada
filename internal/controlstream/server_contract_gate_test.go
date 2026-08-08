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
	"strings"
	"testing"
	"time"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"

	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// ADR-0032, "Compatibility gate at the handshake": the contract version an adapter reports on
// AdapterHello is intersected with the range this control plane supports. In range → HelloAck
// carries negotiated_contract_version. Out of range, missing, or unparseable → the connection
// stands but the adapter may not bring a robot under management: Register and Discover are refused
// VERSION_MISMATCH, fail closed.
//
// Three properties this file exists to hold down, each with a reason:
//
//   - The refusal lands on REGISTRATION, not on the connection. A version-mismatched adapter stays
//     observable (telemetry, heartbeat) and stays STOPPABLE (estop) — refusing the connection would
//     make a mismatch a blind spot, which is strictly less safe than a refused registration.
//   - The gate sits BEFORE the Registrar, so a refused adapter cannot reach the code that writes a
//     Robot or DiscoveredRobot at all (RA-1: nothing is written on this path).
//   - Estop is version-INVARIANT: SafetyStream carries no hello, so the gate cannot reach it.

// helloWithContract builds the standard fixture hello with contract_version overridden — including
// to "" for the pre-ADR-0032 adapter that reports nothing at all.
func helloWithContract(v string) *fav1.AdapterMessage {
	h := hello()
	h.GetHello().ContractVersion = v
	return h
}

// gateStream opens a ControlStream, sends a hello reporting contractVersion, and returns the stream
// plus the HelloAck. Every case here needs those three steps.
func gateStream(t *testing.T, srv *Server, contractVersion string) (fav1.FleetAdapterService_ControlStreamClient, *fav1.HelloAck) {
	t.Helper()
	client, cleanup := newTestServer(t, srv)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(helloWithContract(contractVersion)); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	ack := reply.GetHelloAck()
	if ack == nil {
		t.Fatalf("first reply = %+v, want a HelloAck", reply.GetPayload())
	}
	return stream, ack
}

// sendRegister asks to bring robot-1 under management and returns the ack.
func sendRegister(t *testing.T, stream fav1.FleetAdapterService_ControlStreamClient) *fav1.RegisterAck {
	t.Helper()
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Register{
		Register: &fav1.RegisterRobot{RobotId: "robot-1", AdapterVersion: "1.2.3"},
	}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv register ack: %v", err)
	}
	ack := reply.GetRegisterAck()
	if ack == nil {
		t.Fatalf("reply = %+v, want a RegisterAck", reply.GetPayload())
	}
	return ack
}

// sendDiscover announces an unmanaged robot and returns the ack.
func sendDiscover(t *testing.T, stream fav1.FleetAdapterService_ControlStreamClient) *fav1.DiscoverAck {
	t.Helper()
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Discover{
		Discover: &fav1.DiscoverRobot{RobotId: "robot-1"},
	}}); err != nil {
		t.Fatalf("send discover: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv discover ack: %v", err)
	}
	ack := reply.GetDiscoverAck()
	if ack == nil {
		t.Fatalf("reply = %+v, want a DiscoverAck", reply.GetPayload())
	}
	return ack
}

// ── In range ──────────────────────────────────────────────────────────────────

// The implemented version negotiates and registers.
func TestContractGate_InRangeNegotiatesAndRegisters(t *testing.T) {
	reg := &fakeRegistrar{}
	stream, ack := gateStream(t, &Server{Registrar: reg}, contract.Version)

	if !ack.GetAccepted() {
		t.Fatalf("hello ack = %+v, want accepted", ack)
	}
	if got, want := ack.GetNegotiatedContractVersion(), contract.Version; got != want {
		t.Errorf("negotiated_contract_version = %q, want %q", got, want)
	}
	if ack := sendRegister(t, stream); !ack.GetAccepted() {
		t.Errorf("register ack = %+v, want accepted for an in-range adapter", ack)
	}
	if len(reg.registers) != 1 {
		t.Errorf("registrar saw %d registrations, want 1", len(reg.registers))
	}
}

// The negotiated version is echoed on the identity the Registrar is handed, so a downstream writer
// can record what the robot was admitted under without re-parsing the hello.
func TestContractGate_IdentityCarriesTheNegotiatedVersion(t *testing.T) {
	reg := &fakeRegistrar{}
	stream, _ := gateStream(t, &Server{Registrar: reg}, contract.Version)
	sendRegister(t, stream)

	if len(reg.ids) != 1 {
		t.Fatalf("registrar saw %d identities, want 1", len(reg.ids))
	}
	id := reg.ids[0]
	if got, want := id.ContractVersion, contract.Version; got != want {
		t.Errorf("identity ContractVersion = %q, want %q", got, want)
	}
	if !id.ContractCompatible {
		t.Error("identity ContractCompatible = false for an in-range adapter")
	}
}

// ── Out of range, missing, unparseable ────────────────────────────────────────

// Every incompatible input refuses BOTH registration paths with VERSION_MISMATCH, and never reaches
// the Registrar. Missing ("") is in this table deliberately: ADR-0032 treats absent version data as
// incompatible, never as an implicit pass.
func TestContractGate_IncompatibleVersionsRefuseRegistration(t *testing.T) {
	for _, tc := range []struct{ name, version string }{
		{"missing", ""},                    // an adapter predating contract versioning
		{"newer minor", "1.1.0"},           // a contract this build has not implemented
		{"older major", "0.9.0"},           // a major bump is breaking in either direction
		{"newer major", "2.0.0"},           //
		{"major only", "1"},                // not a semver
		{"major minor", "1.0"},             //
		{"non numeric", "x.y.z"},           //
		{"prerelease", "1.0.0-rc1"},        // never a released contract
		{"build metadata", "1.0.0+build7"}, //
		{"padded", " 1.0.0"},               // whitespace is not silently trimmed into a pass
		{"four parts", "1.0.0.1"},          //
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := &fakeRegistrar{}
			stream, ack := gateStream(t, &Server{Registrar: reg}, tc.version)

			// Design: the CONNECTION stands (so the adapter stays observable and stoppable) but
			// carries no negotiated version, and says so.
			if !ack.GetAccepted() {
				t.Fatalf("hello ack = %+v, want the connection accepted", ack)
			}
			if got := ack.GetNegotiatedContractVersion(); got != "" {
				t.Errorf("negotiated_contract_version = %q, want empty for an incompatible adapter", got)
			}
			if !strings.Contains(ack.GetMessage(), "refused") {
				t.Errorf("hello ack message = %q, want it to say registration is refused", ack.GetMessage())
			}

			rack := sendRegister(t, stream)
			if rack.GetAccepted() {
				t.Errorf("register ack = %+v, want refused", rack)
			}
			if got, want := rack.GetRejection(), fav1.RegistrationRejection_REGISTRATION_REJECTION_VERSION_MISMATCH; got != want {
				t.Errorf("register rejection = %v, want %v", got, want)
			}

			dack := sendDiscover(t, stream)
			if dack.GetAccepted() {
				t.Errorf("discover ack = %+v, want refused", dack)
			}
			if got, want := dack.GetRejection(), fav1.RegistrationRejection_REGISTRATION_REJECTION_VERSION_MISMATCH; got != want {
				t.Errorf("discover rejection = %v, want %v", got, want)
			}

			// The gate sits before the Registrar: the code that would create a Robot or a
			// DiscoveredRobot is never entered (RA-1 — nothing on this path writes).
			if len(reg.registers) != 0 || len(reg.discovers) != 0 {
				t.Errorf("registrar was reached: %d registers, %d discovers; want 0 and 0",
					len(reg.registers), len(reg.discovers))
			}
		})
	}
}

// The refusal must name the range, or an operator reading only the ack cannot tell what to build
// against. A bare "VERSION_MISMATCH" would send them to the source.
func TestContractGate_RefusalNamesTheSupportedRange(t *testing.T) {
	stream, _ := gateStream(t, &Server{Registrar: &fakeRegistrar{}}, "1.1.0")
	msg := sendRegister(t, stream).GetMessage()
	if !strings.Contains(msg, contract.SupportedRange()) {
		t.Errorf("register refusal = %q, want it to name the supported range %q", msg, contract.SupportedRange())
	}
	if !strings.Contains(msg, "1.1.0") {
		t.Errorf("register refusal = %q, want it to quote the version the adapter reported", msg)
	}
}

// ── What the gate must NOT touch ──────────────────────────────────────────────

// An incompatible adapter stays OBSERVABLE. Refusing its telemetry would turn a version mismatch
// into a blind spot — the operator would lose the very signal that shows what is connected.
func TestContractGate_TelemetryAndHeartbeatStillFlowWhenIncompatible(t *testing.T) {
	ing := &fakeIngestor{}
	stream, ack := gateStream(t, &Server{Registrar: &fakeRegistrar{}, Ingestor: ing}, "1.1.0")
	if ack.GetNegotiatedContractVersion() != "" {
		t.Fatalf("expected an incompatible handshake, got negotiated %q", ack.GetNegotiatedContractVersion())
	}

	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{
		Telemetry: &fav1.TelemetryPayload{
			RobotId:     "robot-1",
			TimestampMs: 1_700_000_000_000,
			Phase:       fav1.RobotPhase_ROBOT_PHASE_IDLE,
		},
	}}); err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Heartbeat{
		Heartbeat: &fav1.HeartbeatResponse{RobotId: "robot-1"},
	}}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	// Neither is an ack-bearing message, so drive one round-trip to prove the stream is still live
	// and the server did not close it over the version mismatch.
	if ack := sendRegister(t, stream); ack.GetAccepted() {
		t.Fatalf("register ack = %+v, want still refused", ack)
	}

	var frames []telemetry.Frame
	// The ingest is asynchronous relative to the send; poll briefly rather than sleep a fixed span.
	for i := 0; i < 50; i++ {
		if frames = ing.snapshot(); len(frames) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(frames) == 0 {
		t.Fatal("no telemetry ingested from an incompatible adapter; a version mismatch must not blind the operator")
	}
	if got := frames[0].RobotID; got != "robot-1" {
		t.Errorf("ingested frame robot = %q, want robot-1", got)
	}
}

// Estop is version-INVARIANT (ADR-0032). The structural reason is stronger than any assertion about
// the gate's logic: SafetyStream carries no AdapterHello, so there is no contract version to
// intersect and the gate cannot reach it. This test pins that structure — if a hello were ever added
// to the safety path, it fails.
func TestContractGate_EstopUnaffectedByTheVersionGate(t *testing.T) {
	disp := &fakeSafetyDispatcher{}
	client, cleanup := newTestServer(t, &Server{Safety: disp, Registrar: &fakeRegistrar{}})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open safety stream: %v", err)
	}
	// No hello, no contract_version — an estop ack is routed on its own merits.
	if err := stream.Send(&fav1.AdapterSafetyMessage{
		RobotId: "robot-1",
		Payload: &fav1.AdapterSafetyMessage_EstopAck{EstopAck: &fav1.EstopAck{
			EstopId: "e-1", State: fav1.EstopState_ESTOP_STATE_STOPPED,
		}},
	}); err != nil {
		t.Fatalf("send estop ack: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	// The route is asynchronous relative to the send; poll rather than sleep a fixed span.
	for i := 0; i < 50 && disp.count() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := disp.count(); got != 1 {
		t.Fatalf("safety dispatcher routed %d acks, want 1 — estop must be version-invariant", got)
	}
	disp.mu.Lock()
	state := disp.routed[0].GetState()
	disp.mu.Unlock()
	if state != fav1.EstopState_ESTOP_STATE_STOPPED {
		t.Errorf("routed ack state = %v, want STOPPED", state)
	}
}
