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
	"io"
	"sync"
	"testing"
	"time"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// robot_id is REQUIRED and non-empty on every message arm that carries it (RFC-0001
// #fleet-adapter-protocol-transport-security, "Robot identity"). An empty one is not a
// message about no robot; it is a message the per-robot gate cannot see a robot in, and it
// must be authorized like any other value rather than skipped. Otherwise a stream with no
// verified identity reaches consumers that act on what it sent, the fail-closed rule for an
// unverified identity notwithstanding.

// armCounter counts what reached each downstream consumer.
type armCounter struct {
	mu                                                       sync.Mutex
	reserves, results, capabilities, progress, registrations int
}

func (c *armCounter) inc(p *int) { c.mu.Lock(); *p++; c.mu.Unlock() }
func (c *armCounter) get(p *int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *p
}

type countingReserver struct{ c *armCounter }

func (r countingReserver) ReserveResource(context.Context, string, string, string) ResourceDecision {
	r.c.inc(&r.c.reserves)
	return ResourceDecision{State: ResourceGranted}
}
func (r countingReserver) ReleaseResource(context.Context, string, string, string) ResourceDecision {
	return ResourceDecision{}
}

type countingCommands struct{ c *armCounter }

func (d countingCommands) RegisterStream(TLSIdentity, CommandSender) func() { return func() {} }
func (d countingCommands) RouteResult(CommandSender, *fav1.CommandResult) error {
	d.c.inc(&d.c.results)
	return nil
}

type countingCapabilities struct{ c *armCounter }

func (i countingCapabilities) IngestCapabilities(context.Context, string, *fav1.CapabilitiesSnapshot) error {
	i.c.inc(&i.c.capabilities)
	return nil
}

type countingProgress struct{ c *armCounter }

func (i countingProgress) IngestUpdateProgress(context.Context, string, *fav1.UpdateProgress) error {
	i.c.inc(&i.c.progress)
	return nil
}

type countingRegistrar struct{ c *armCounter }

func (r countingRegistrar) Register(context.Context, AdapterIdentity, *fav1.RegisterRobot) *fav1.RegisterAck {
	r.c.inc(&r.c.registrations)
	return &fav1.RegisterAck{Accepted: true}
}
func (r countingRegistrar) Discover(context.Context, AdapterIdentity, TLSIdentity, *fav1.DiscoverRobot) *fav1.DiscoverAck {
	return &fav1.DiscoverAck{Accepted: true}
}

func TestDispatch_EmptyRobotIDIsAuthorizedNotSkipped(t *testing.T) {
	cnt := &armCounter{}
	srv := &Server{
		Authorizer:     &fakeAuthorizer{allow: map[string]bool{"amr-1": true}}, // "" is not allowed
		Reservations:   countingReserver{cnt},
		Commands:       countingCommands{cnt},
		Capabilities:   countingCapabilities{cnt},
		UpdateProgress: countingProgress{cnt},
		Registrar:      countingRegistrar{cnt},
	}
	client, cleanup := newTestServer(t, srv)
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
	for _, m := range []*fav1.AdapterMessage{
		{Payload: &fav1.AdapterMessage_ResourceRequest{ResourceRequest: &fav1.ResourceRequest{
			RequestId: 1, Request: &fav1.ResourceRequest_Reserve{Reserve: &fav1.ReserveResource{ResourceName: "dock-1"}},
		}}},
		{Payload: &fav1.AdapterMessage_CommandResult{CommandResult: &fav1.CommandResult{CommandId: 1}}},
		{Payload: &fav1.AdapterMessage_Capabilities{Capabilities: &fav1.CapabilitiesSnapshot{}}},
		{Payload: &fav1.AdapterMessage_UpdateProgress{UpdateProgress: &fav1.UpdateProgress{Phase: "Downloading"}}},
		{Payload: &fav1.AdapterMessage_Register{Register: &fav1.RegisterRobot{}}},
	} {
		if err := stream.Send(m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	var registerAck *fav1.RegisterAck
	for {
		reply, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("an empty robot_id tore down the stream: %v", err)
		}
		if a := reply.GetRegisterAck(); a != nil {
			registerAck = a
		}
	}

	for name, n := range map[string]int{
		"ResourceRequest reached the reserver":          cnt.get(&cnt.reserves),
		"CommandResult was routed to a waiting command": cnt.get(&cnt.results),
		"CapabilitiesSnapshot reached its ingestor":     cnt.get(&cnt.capabilities),
		"UpdateProgress reached its ingestor":           cnt.get(&cnt.progress),
		"RegisterRobot reached the registrar":           cnt.get(&cnt.registrations),
	} {
		if n != 0 {
			t.Errorf("%s with an empty robot_id and no authorized identity (%d)", name, n)
		}
	}
	// Refused, and refused with the code the contract names for an empty robot_id.
	if registerAck == nil || registerAck.GetAccepted() ||
		registerAck.GetRejection() != fav1.RegistrationRejection_REGISTRATION_REJECTION_INVALID_ROBOT_ID {
		t.Errorf("RegisterAck = %v, want accepted=false with INVALID_ROBOT_ID", registerAck)
	}
}

// The one robot-free form the protocol defines on a robot arm: a Heartbeat with an empty
// robot_id is the adapter-scoped reply to the periodic probe (ADR-0054). It must keep
// reaching presence under an enforcing Authorizer.
func TestDispatch_AdapterScopedHeartbeatStillReachesPresence(t *testing.T) {
	fp := &fakePresence{}
	s := &Server{Presence: fp, Authorizer: &fakeAuthorizer{allow: map[string]bool{}}}
	tlsID := TLSIdentity{AdapterName: "acme", Namespace: "warehouse-a", Verified: true}
	hb := &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Heartbeat{Heartbeat: &fav1.HeartbeatResponse{}}}
	if err := s.dispatch(context.Background(), &streamWriter{}, AdapterIdentity{Namespace: "warehouse-a"}, tlsID, hb); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if fp.heartbeats != 1 {
		t.Fatalf("adapter-scoped heartbeat presence = %d, want 1", fp.heartbeats)
	}
}
