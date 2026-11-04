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

package streamauth

import (
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The FleetAdapter's existence is a condition of the admission gate, and the gate MUST NOT block
// observation or stopping (RFC-0001 #fleetadapter-admission-gating). So once the FleetAdapter a
// robot names is deleted, the robot's own adapter is still heard: its telemetry, a heartbeat
// naming the robot, an EstopAck and a LocalStopReport are taken. Every arm that is work is still
// refused (#security-fleet-adapter-authentication-robot-identity).
//
// These run the real controlstream.Server with this package's Authorizer over mTLS, on the
// class-leg fixture (class_leg_stream_test.go). Adapter "acme" presents the certificate and has
// no FleetAdapter object. Robot amr-1 is bound to acme; amr-x is bound to adapter "other", which
// has no FleetAdapter object either.

// newAdapterDeletedFixture serves acme with acme's FleetAdapter absent.
func newAdapterDeletedFixture(t *testing.T) *classLegFixture {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(classLegScheme(t)).WithObjects(
		[]client.Object{robot("amr-1", "acme", "amr"), robot("amr-x", "other", "amr")}...).Build()
	rec := &classLegRecorder{}
	sink := &audit.MemorySink{}
	srv := &controlstream.Server{
		Registrar:      rec,
		Ingestor:       rec,
		ActionStatus:   rec,
		UpdateProgress: rec,
		Capabilities:   rec,
		Reservations:   rec,
		Commands:       classLegCommands{rec},
		Safety:         classLegSafety{rec},
		Authorizer:     &Authorizer{Client: c},
		Audit:          audit.New(sink, "test"),
	}
	return &classLegFixture{rec: rec, sink: sink, cli: serveClassLeg(t, srv)}
}

// The robot's FleetAdapter is gone, and its adapter's telemetry for it is still ingested, with no
// denial sealed.
func TestAdapterDeleted_TelemetryIsStillIngested(t *testing.T) {
	f := newAdapterDeletedFixture(t)
	f.control(t, clTelemetry("amr-1"))
	if got := f.rec.seen().frames; len(got) != 1 || got[0] != "amr-1" {
		t.Errorf("ingested %v, want amr-1's frame: observation of a robot whose FleetAdapter was deleted is blocked", got)
	}
	if d := f.denials("amr-1"); len(d) != 0 {
		t.Errorf("telemetry sealed as denied: %v", d)
	}
}

// A heartbeat naming the robot still answers its per-robot confirming exchange.
func TestAdapterDeleted_RobotHeartbeatStillReachesTheConfirmingExchange(t *testing.T) {
	f := newAdapterDeletedFixture(t)
	f.control(t, clHeartbeat("amr-1"))
	if got := f.rec.seen().heartbeats; len(got) != 1 || got[0] != "amr-1" {
		t.Errorf("routed heartbeats %v, want amr-1's", got)
	}
}

// An EstopAck from the robot's own adapter still reaches the dispatcher, so the stop can be
// confirmed.
func TestAdapterDeleted_EstopAckIsStillRouted(t *testing.T) {
	f := newAdapterDeletedFixture(t)
	f.safety(t, clEstopAck("amr-1", "e-1"))
	if got := f.rec.seen().acks; len(got) != 1 || got[0] != "e-1" {
		t.Errorf("routed acks %v, want e-1: a stop sent to the robot could never be confirmed", got)
	}
	if d := f.denials("amr-1"); len(d) != 0 {
		t.Errorf("EstopAck sealed as denied: %v", d)
	}
}

// A LocalStopReport is a report about stopping, and is not sealed as a denial.
func TestAdapterDeleted_LocalStopReportIsNotADenial(t *testing.T) {
	f := newAdapterDeletedFixture(t)
	f.safety(t, clLocalStop("amr-1"))
	if d := f.denials("amr-1"); len(d) != 0 {
		t.Errorf("LocalStopReport sealed as denied: %v", d)
	}
}

// Every arm that is work is still refused with the FleetAdapter gone, and the refusal is sealed.
// An EstopClearAck is one: a clear begins the robot's return to work.
func TestAdapterDeleted_WorkIsStillRefused(t *testing.T) {
	work := []struct {
		name    string
		control *fav1.AdapterMessage
		safety  *fav1.AdapterSafetyMessage
		reached func(r classLegSeen) bool
	}{
		{name: "register", control: clRegister("amr-1"),
			reached: func(r classLegSeen) bool { return r.registers != 0 }},
		{name: "command_result", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_CommandResult{
			CommandResult: &fav1.CommandResult{CommandId: 1, RobotId: "amr-1"}}},
			reached: func(r classLegSeen) bool { return r.results != 0 }},
		{name: "resource_request", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_ResourceRequest{
			ResourceRequest: &fav1.ResourceRequest{RequestId: 1, RobotId: "amr-1", Request: &fav1.ResourceRequest_Reserve{
				Reserve: &fav1.ReserveResource{ResourceName: "door-1"}}}}},
			reached: func(r classLegSeen) bool { return r.reserves != 0 }},
		{name: "capabilities", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Capabilities{
			Capabilities: &fav1.CapabilitiesSnapshot{RobotId: "amr-1"}}},
			reached: func(r classLegSeen) bool { return r.caps != 0 }},
		{name: "update_progress", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_UpdateProgress{
			UpdateProgress: &fav1.UpdateProgress{RobotId: "amr-1"}}},
			reached: func(r classLegSeen) bool { return r.progress != 0 }},
		{name: "action_status", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_ActionStatus{
			ActionStatus: &fav1.ActionStatusUpdate{ActionId: "a-1"}}},
			reached: func(r classLegSeen) bool { return len(r.actionAuth) != 1 || r.actionAuth[0] }},
		{name: "estop_clear_ack", safety: &fav1.AdapterSafetyMessage{RobotId: "amr-1",
			Payload: &fav1.AdapterSafetyMessage_EstopClearAck{EstopClearAck: &fav1.EstopClearAck{EstopId: "e-1"}}},
			reached: func(r classLegSeen) bool { return len(r.clears) != 0 }},
	}
	for _, w := range work {
		t.Run(w.name, func(t *testing.T) {
			f := newAdapterDeletedFixture(t)
			var replies []*fav1.ControlPlaneMessage
			if w.control != nil {
				replies = f.control(t, w.control)
			} else {
				f.safety(t, w.safety)
			}
			if w.reached(f.rec.seen()) {
				t.Errorf("%s for a robot whose FleetAdapter was deleted was taken as work", w.name)
			}
			if w.name == "register" {
				if len(replies) != 1 || replies[0].GetRegisterAck() == nil || replies[0].GetRegisterAck().GetAccepted() {
					t.Errorf("register replies %v, want one RegisterAck accepted: false", replies)
				}
			}
			if d := f.denials("amr-1"); len(d) != 1 || !strings.Contains(d[0], `"acme"`) || !strings.Contains(d[0], "not found") {
				t.Errorf("sealed denials %v, want one naming the missing FleetAdapter", d)
			}
		})
	}
}

// Only the adapter's existence is relaxed. A robot bound to another adapter is not acme's to
// observe or to confirm stopped, even when that adapter's FleetAdapter is gone as well.
func TestAdapterDeleted_ObservationStillNeedsTheBinding(t *testing.T) {
	f := newAdapterDeletedFixture(t)
	f.control(t, clTelemetry("amr-x"), clHeartbeat("amr-x"))
	f.safety(t, clEstopAck("amr-x", "e-x"), clLocalStop("amr-x"))
	if got := f.rec.seen(); len(got.frames) != 0 || len(got.heartbeats) != 0 || len(got.acks) != 0 {
		t.Errorf("acme was heard for amr-x, which is bound to another adapter: frames %v heartbeats %v acks %v",
			got.frames, got.heartbeats, got.acks)
	}
	if d := f.denials("amr-x"); len(d) != 4 {
		t.Errorf("sealed denials %v, want 4 (one per message)", d)
	}
}
