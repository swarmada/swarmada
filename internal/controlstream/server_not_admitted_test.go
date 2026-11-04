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
	"strings"
	"testing"
	"time"

	"github.com/swarmada/swarmada/internal/audit"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Onboarding a robot that has no Robot object yet (adapters/CONFORMANCE.md C2.1):
// the adapter registers FIRST, is told NOT_ADMITTED, and falls through to
// DiscoverRobot. A robot that is merely not admitted is a provisioning step; a robot
// the adapter may not act on is a violation. The two must stay distinguishable to the
// server and indistinguishable to nobody but the adapter that legitimately serves it.

// onboardingAuthorizer admits "amr-ok", reports "amr-new" as not admitted (no Robot
// object), and denies everything else as a genuine violation.
type onboardingAuthorizer struct{}

func (onboardingAuthorizer) AuthorizeRobot(_ context.Context, _ TLSIdentity, robotID string) error {
	switch robotID {
	case "amr-ok":
		return nil
	case "amr-new":
		return fmt.Errorf("robot %q not found in namespace: %w", robotID, ErrRobotNotAdmitted)
	}
	return fmt.Errorf("robot %q is bound to another adapter", robotID)
}

func (onboardingAuthorizer) AuthorizeAnnounce(_ context.Context, _ TLSIdentity, robotID string) error {
	if robotID == "amr-bound-elsewhere" {
		return fmt.Errorf("robot %q is bound to another adapter", robotID)
	}
	return nil
}

func registerOverStream(t *testing.T, s *Server, robotID string) *fav1.RegisterAck {
	t.Helper()
	client, cleanup := newTestServer(t, s)
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
		Register: &fav1.RegisterRobot{RobotId: robotID},
	}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv register ack: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	return reply.GetRegisterAck()
}

// A Register for a robot with no Robot object is answered NOT_ADMITTED: the code the
// contract declares for "Register: no Robot CRD exists", which is what routes a
// conforming adapter into discovery. It never reaches the Registrar and is not
// sealed as an authorization denial.
func TestRegister_NotAdmittedRobotIsToldNotAdmitted(t *testing.T) {
	reg := &fakeRegistrar{}
	sink := &audit.MemorySink{}
	ack := registerOverStream(t, &Server{Registrar: reg, Authorizer: onboardingAuthorizer{}, Audit: audit.New(sink, "v0.1.0")}, "amr-new")

	if ack == nil || ack.GetAccepted() {
		t.Fatalf("register ack = %+v, want accepted:false", ack)
	}
	if got := ack.GetRejection(); got != fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED {
		t.Fatalf("rejection = %s, want NOT_ADMITTED: without it a conforming adapter has no basis "+
			"to fall through to DiscoverRobot, and the robot can never be admitted", got)
	}
	if !strings.Contains(ack.GetMessage(), "DiscoverRobot") {
		t.Errorf("message %q does not tell the adapter what to do next", ack.GetMessage())
	}
	if reg.registers != nil {
		t.Fatalf("a not-admitted Register reached the Registrar: %+v", reg.registers)
	}
	if n := len(sink.Entries()); n != 0 {
		t.Errorf("%d audit entries for an onboarding step, want 0: the chain records refusals, not provisioning", n)
	}
}

// A genuine violation is NOT answered NOT_ADMITTED. That code tells the adapter the
// robot is unclaimed, which a hostile adapter must not learn for a robot bound
// elsewhere. It stays opaque, and it is sealed.
func TestRegister_ViolationIsOpaqueAndAudited(t *testing.T) {
	reg := &fakeRegistrar{}
	sink := &audit.MemorySink{}
	ack := registerOverStream(t, &Server{Registrar: reg, Authorizer: onboardingAuthorizer{}, Audit: audit.New(sink, "v0.1.0")}, "amr-bound-elsewhere")

	if ack == nil || ack.GetAccepted() {
		t.Fatalf("register ack = %+v, want accepted:false", ack)
	}
	if got := ack.GetRejection(); got == fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED {
		t.Fatal("SECURITY: a violation was answered NOT_ADMITTED, telling the adapter the robot is unclaimed")
	}
	if !strings.Contains(ack.GetMessage(), "PERMISSION_DENIED") {
		t.Errorf("message = %q, want the opaque PERMISSION_DENIED", ack.GetMessage())
	}
	if reg.registers != nil {
		t.Fatalf("SECURITY: an unauthorized Register reached the Registrar: %+v", reg.registers)
	}
	entries := sink.Entries()
	if len(entries) != 1 || entries[0].EventType != audit.EventRobotAuthzDenied {
		t.Fatalf("audit entries = %+v, want one ROBOT_AUTHZ_DENIED", entries)
	}
}

// A Heartbeat naming a not-admitted robot proves the ADAPTER is alive, so the adapter
// does not decay Connected → Degraded → Disconnected while the robot it serves waits
// for admission. Only that half runs: no per-robot confirming exchange is answered.
// A Heartbeat naming a robot the adapter may not act on drives nothing.
func TestDispatch_NotAdmittedHeartbeatIsAdapterPresenceOnly(t *testing.T) {
	id := AdapterIdentity{Namespace: "warehouse-a"}
	tlsID := TLSIdentity{AdapterName: "acme", Namespace: "warehouse-a", Verified: true}
	beat := func(robotID string) *fav1.AdapterMessage {
		return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Heartbeat{
			Heartbeat: &fav1.HeartbeatResponse{RobotId: robotID},
		}}
	}

	t.Run("not admitted: presence, no robot exchange", func(t *testing.T) {
		fp, router := &fakePresence{}, &beatRouter{}
		s := &Server{Presence: fp, Commands: router, Authorizer: onboardingAuthorizer{}}
		if err := s.dispatch(context.Background(), &streamWriter{}, id, tlsID, beat("amr-new")); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if fp.heartbeats != 1 {
			t.Fatalf("adapter presence heartbeats = %d, want 1: an adapter whose only robot awaits "+
				"admission could not prove liveness, which is the onboarding deadlock", fp.heartbeats)
		}
		if len(router.routedRobots) != 0 {
			t.Fatalf("a not-admitted heartbeat answered a per-robot exchange for %v", router.routedRobots)
		}
	})

	t.Run("violation: nothing", func(t *testing.T) {
		fp, router := &fakePresence{}, &beatRouter{}
		s := &Server{Presence: fp, Commands: router, Authorizer: onboardingAuthorizer{}}
		if err := s.dispatch(context.Background(), &streamWriter{}, id, tlsID, beat("amr-bound-elsewhere")); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if fp.heartbeats != 0 || len(router.routedRobots) != 0 {
			t.Fatalf("a heartbeat for a robot the adapter may not act on drove presence=%d routed=%v",
				fp.heartbeats, router.routedRobots)
		}
	})

	t.Run("unverified identity: no presence even when not admitted", func(t *testing.T) {
		fp := &fakePresence{}
		s := &Server{Presence: fp, Commands: &beatRouter{}, Authorizer: onboardingAuthorizer{}}
		unverified := tlsID
		unverified.Verified = false
		if err := s.dispatch(context.Background(), &streamWriter{}, id, unverified, beat("amr-new")); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if fp.heartbeats != 0 {
			t.Fatalf("an unverified identity drove adapter presence (%d)", fp.heartbeats)
		}
	})
}

// Every other robot-scoped arm for a not-admitted robot is dropped: telemetry from a
// robot with no Robot object reaches no ingestor.
func TestDispatch_NotAdmittedTelemetryIsDropped(t *testing.T) {
	ing := &fakeIngestor{}
	s := &Server{Ingestor: ing, Authorizer: onboardingAuthorizer{}}
	id := AdapterIdentity{Namespace: "warehouse-a"}
	tlsID := TLSIdentity{AdapterName: "acme", Namespace: "warehouse-a", Verified: true}
	if err := s.dispatch(context.Background(), &streamWriter{}, id, tlsID, telemetryFor("amr-new")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if n := len(ing.snapshot()); n != 0 {
		t.Fatalf("telemetry for a not-admitted robot was ingested (%d frames)", n)
	}
}
