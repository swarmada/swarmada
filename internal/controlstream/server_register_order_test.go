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

	"github.com/swarmada/swarmada/internal/audit"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// One RegisterAck for each RegisterRobot on a stream, in the order the registers arrived,
// refusals included (conformance/CONTROL-PLANE.md CP9.1, advisory until RFC-0001 §9.2.7 states
// the order). A RegisterAck names no robot, so an adapter that serves several robots on one
// ControlStream can tell which robot an ack answers only by this order. An ack skipped for a
// refusal, or sent out of order, gives one robot another robot's answer: an admission it was
// not given, or another robot's authoritative action state and fencing token.
func TestRegister_EveryRegisterIsAnsweredInTheOrderItArrived(t *testing.T) {
	reg := &fakeRegistrar{}
	srv := &Server{Registrar: reg, Authorizer: onboardingAuthorizer{}, Audit: audit.New(&audit.MemorySink{}, "v0.1.0")}
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

	// Sent back to back, before any ack is read: an adapter registers every robot it serves
	// at once. The violation goes last, so that nothing the server does about it can hide an
	// ack that should have come before it.
	robots := []string{"amr-new", "amr-ok", "amr-new", "amr-bound-elsewhere"}
	for _, id := range robots {
		if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Register{
			Register: &fav1.RegisterRobot{RobotId: id},
		}}); err != nil {
			t.Fatalf("send register %s: %v", id, err)
		}
	}

	var acks []*fav1.RegisterAck
	for len(acks) < len(robots) {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("after %d of %d register acks: %v", len(acks), len(robots), err)
		}
		if ack := msg.GetRegisterAck(); ack != nil {
			acks = append(acks, ack)
		}
	}

	notAdmitted := fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED
	want := []struct {
		accepted  bool
		rejection fav1.RegistrationRejection
		message   string
	}{
		{false, notAdmitted, "DiscoverRobot"},
		{true, fav1.RegistrationRejection_REGISTRATION_REJECTION_UNSPECIFIED, "ok"},
		{false, notAdmitted, "DiscoverRobot"},
		{false, fav1.RegistrationRejection_REGISTRATION_REJECTION_UNSPECIFIED, "PERMISSION_DENIED"},
	}
	for i, w := range want {
		got := acks[i]
		if got.GetAccepted() != w.accepted || got.GetRejection() != w.rejection ||
			!strings.Contains(got.GetMessage(), w.message) {
			t.Errorf("ack %d (register for %s) = accepted:%v rejection:%s message:%q, want "+
				"accepted:%v rejection:%s message containing %q: the acks are not one per "+
				"register in order, so an adapter serving several robots pairs them wrongly",
				i, robots[i], got.GetAccepted(), got.GetRejection(), got.GetMessage(),
				w.accepted, w.rejection, w.message)
		}
	}
}
