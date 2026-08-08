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
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/swarmada/swarmada/internal/audit"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// fakeSafetyDispatcher records routed EstopAcks so the server's authorize-before-
// route gate can be observed.
type fakeSafetyDispatcher struct {
	mu     sync.Mutex
	routed []*fav1.EstopAck
}

func (f *fakeSafetyDispatcher) RegisterStream(TLSIdentity, SafetySender) func() { return func() {} }
func (f *fakeSafetyDispatcher) RouteAck(ack *fav1.EstopAck) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routed = append(f.routed, ack)
}
func (f *fakeSafetyDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.routed)
}

// fakePresence counts adapter connectivity events.
type fakePresence struct {
	connected, heartbeats, disconnected int
}

func (f *fakePresence) AdapterConnected(context.Context, TLSIdentity, Negotiation) { f.connected++ }
func (f *fakePresence) AdapterHeartbeat(context.Context, TLSIdentity)              { f.heartbeats++ }
func (f *fakePresence) AdapterDisconnected(context.Context, TLSIdentity)           { f.disconnected++ }

// RA-1 distinction: an adapter Heartbeat drives presence (liveness); a telemetry
// frame NEVER does (it is a per-robot data tick, not adapter liveness).
func TestDispatch_HeartbeatIsPresenceTelemetryIsNot(t *testing.T) {
	fp := &fakePresence{}
	s := &Server{Presence: fp} // no Authorizer → dev-mode authz bypass
	id := AdapterIdentity{Namespace: "warehouse-a"}
	tlsID := TLSIdentity{AdapterName: "acme", Namespace: "warehouse-a", Verified: true}
	w := &streamWriter{}

	// A liveness Heartbeat → presence.
	hb := &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Heartbeat{Heartbeat: &fav1.HeartbeatResponse{RobotId: "amr-1"}}}
	if err := s.dispatch(context.Background(), w, id, tlsID, hb); err != nil {
		t.Fatalf("dispatch heartbeat: %v", err)
	}
	if fp.heartbeats != 1 {
		t.Fatalf("heartbeat presence = %d, want 1", fp.heartbeats)
	}

	// A telemetry frame → NO presence event (RA-1).
	if err := s.dispatch(context.Background(), w, id, tlsID, telemetryFor("amr-1")); err != nil {
		t.Fatalf("dispatch telemetry: %v", err)
	}
	if fp.heartbeats != 1 || fp.connected != 0 {
		t.Fatalf("telemetry touched presence (heartbeats=%d connected=%d) — RA-1 violation", fp.heartbeats, fp.connected)
	}
}

// An EstopAck for a robot the adapter is not authorized for is dropped, never
// routed to the dispatcher — a compromised adapter cannot forge a STOPPED for
// another adapter's robot. The stream survives.
func TestSafetyStream_UnauthorizedAckDropped(t *testing.T) {
	disp := &fakeSafetyDispatcher{}
	auth := &fakeAuthorizer{allow: map[string]bool{"amr-1": true}} // amr-forged denied
	client, cleanup := newTestServer(t, &Server{Safety: disp, Authorizer: auth})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open safety stream: %v", err)
	}
	send := func(robotID string) {
		if err := stream.Send(&fav1.AdapterSafetyMessage{
			RobotId: robotID,
			Payload: &fav1.AdapterSafetyMessage_EstopAck{EstopAck: &fav1.EstopAck{
				EstopId: "e-" + robotID, State: fav1.EstopState_ESTOP_STATE_STOPPED,
			}},
		}); err != nil {
			t.Fatalf("send ack: %v", err)
		}
	}
	send("amr-forged") // denied → dropped
	send("amr-1")      // authorized → routed
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("forged ack tore down the SafetyStream: %v", err)
		}
	}

	if n := disp.count(); n != 1 {
		t.Fatalf("routed %d acks, want 1 (forged dropped, authorized routed)", n)
	}
	if disp.routed[0].GetEstopId() != "e-amr-1" {
		t.Fatalf("routed the wrong ack: %q", disp.routed[0].GetEstopId())
	}
}

// fakeAuthorizer authorizes by an allow-set of robot ids, so server-level
// enforcement wiring can be tested independent of TLS extraction (covered in
// identity_test) and the real lookup logic (covered in streamauth).
type fakeAuthorizer struct{ allow map[string]bool }

func (f *fakeAuthorizer) AuthorizeRobot(_ context.Context, _ TLSIdentity, robotID string) error {
	if f.allow[robotID] {
		return nil
	}
	return fmt.Errorf("denied: %s", robotID)
}

func (f *fakeAuthorizer) AuthorizeAnnounce(_ context.Context, _ TLSIdentity, robotID string) error {
	if f.allow[robotID] {
		return nil
	}
	return fmt.Errorf("denied announce: %s", robotID)
}

func telemetryFor(robotID string) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{Telemetry: &fav1.TelemetryPayload{
		RobotId: robotID, TimestampMs: 1_700_000_000_000, Phase: fav1.RobotPhase_ROBOT_PHASE_IDLE,
	}}}
}

// A forged/unauthorized robot_id is dropped, the stream is NOT torn down, and a
// legitimate robot on the SAME stream still ingests (§9.2.7 fail-closed).
func TestControlStream_ForgedRobotIDDropped_StreamSurvives(t *testing.T) {
	ing := &fakeIngestor{}
	auth := &fakeAuthorizer{allow: map[string]bool{"amr-1": true}} // amr-forged is NOT allowed
	client, cleanup := newTestServer(t, &Server{Ingestor: ing, Authorizer: auth})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if err := stream.Send(telemetryFor("amr-forged")); err != nil {
		t.Fatalf("send forged: %v", err)
	}
	if err := stream.Send(telemetryFor("amr-1")); err != nil {
		t.Fatalf("send legit: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	// Drain to EOF — the stream must close cleanly (a stray robot_id is not a fault).
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream was torn down by a forged robot_id: %v", err)
		}
	}

	frames := ing.snapshot()
	if len(frames) != 1 || frames[0].RobotID != "amr-1" {
		t.Fatalf("ingested %+v; want exactly the authorized robot amr-1 (forged dropped)", frames)
	}
}

// A denied authorization is recorded to the safety audit log (§9.5.4) — a denied
// action is never silently dropped.
func TestControlStream_AuthzDeniedIsAudited(t *testing.T) {
	sink := &audit.MemorySink{}
	auth := &fakeAuthorizer{allow: map[string]bool{}} // deny everything
	client, cleanup := newTestServer(t, &Server{
		Ingestor:   &fakeIngestor{},
		Authorizer: auth,
		Audit:      audit.New(sink, "v0.1.0"),
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if err := stream.Send(telemetryFor("amr-forged")); err != nil {
		t.Fatalf("send forged: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	for {
		if _, err := stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("stream error: %v", err)
		}
	}

	entries := sink.Entries()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 ROBOT_AUTHZ_DENIED", len(entries))
	}
	e := entries[0]
	if e.EventType != audit.EventRobotAuthzDenied || e.Outcome != audit.OutcomeDenied || e.Resource.Name != "amr-forged" {
		t.Fatalf("audit entry = %+v", e)
	}
}

// An unauthorized Register is refused in-band (accepted:false) and NEVER reaches
// the Registrar — the business logic runs only after authorization passes.
func TestControlStream_UnauthorizedRegisterRefusedInBand(t *testing.T) {
	reg := &fakeRegistrar{}
	auth := &fakeAuthorizer{allow: map[string]bool{}} // deny everything
	client, cleanup := newTestServer(t, &Server{Registrar: reg, Authorizer: auth})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Register{
		Register: &fav1.RegisterRobot{RobotId: "amr-x"},
	}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv register ack: %v", err)
	}
	ack := reply.GetRegisterAck()
	if ack == nil || ack.GetAccepted() {
		t.Fatalf("register ack = %+v, want accepted:false", ack)
	}
	if reg.registers != nil {
		t.Fatalf("SECURITY: an unauthorized Register reached the Registrar: %+v", reg.registers)
	}
}
