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
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/swarmada/swarmada/internal/audit"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The per-message gate authorizes a message only when robotIDOf can see a robot in it,
// so robotIDOf is the authorization boundary. It names every arm of the oneof, and an
// arm it does not name is refused. This test walks the oneof as the generated code
// describes it, so an arm added to the proto fails here until someone decides how it is
// authorized — rather than being passed unchecked by a default that returns nothing.
func TestRobotIDOf_EveryAdapterMessageArmIsClassified(t *testing.T) {
	desc := (&fav1.AdapterMessage{}).ProtoReflect().Descriptor()
	oneof := desc.Oneofs().ByName("payload")
	if oneof == nil {
		t.Fatal("AdapterMessage has no payload oneof")
	}
	if oneof.Fields().Len() == 0 {
		t.Fatal("payload oneof has no arms; the walk below would prove nothing")
	}
	for i := 0; i < oneof.Fields().Len(); i++ {
		fd := oneof.Fields().Get(i)
		msg := &fav1.AdapterMessage{}
		m := msg.ProtoReflect()
		m.Set(fd, protoreflect.ValueOfMessage(m.NewField(fd).Message()))
		if _, scope := robotIDOf(msg); scope == scopeUnclassified {
			t.Errorf("AdapterMessage arm %q is not classified in robotIDOf: decide whether it "+
				"names a robot, an action, or none, and say why", fd.Name())
		}
	}
	if _, scope := robotIDOf(&fav1.AdapterMessage{}); scope != scopeAdapter {
		t.Errorf("empty oneof scope = %v, want scopeAdapter", scope)
	}
}

// An EstopAck with an empty envelope robot_id names no robot to authorize, so it is
// refused and sealed, never routed — even with no Authorizer configured, since the
// dispatcher could not match it to an estop anyway.
func TestSafetyStream_EmptyRobotIDAckIsRefusedAndRecorded(t *testing.T) {
	disp := &fakeSafetyDispatcher{}
	spy := &authzAuditSpy{}
	client, cleanup := newTestServer(t, &Server{Safety: disp, Audit: spy})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open safety stream: %v", err)
	}
	if err := stream.Send(&fav1.AdapterSafetyMessage{
		Payload: &fav1.AdapterSafetyMessage_EstopAck{EstopAck: &fav1.EstopAck{
			EstopId: "estop-amr-1-1", State: fav1.EstopState_ESTOP_STATE_STOPPED,
		}},
	}); err != nil {
		t.Fatalf("send ack: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	for {
		if _, err := stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("an empty robot_id tore down the SafetyStream: %v", err)
		}
	}
	if n := disp.count(); n != 0 {
		t.Errorf("routed %d ack(s) with an empty robot_id, want 0", n)
	}
	if n := spy.count(audit.EventRobotAuthzDenied); n != 1 {
		t.Errorf("%s entries = %d, want 1: the refusal must be recorded", audit.EventRobotAuthzDenied, n)
	}
}

type authzAuditSpy struct{ entries []audit.Entry }

func (s *authzAuditSpy) Record(e audit.Entry) (audit.Entry, error) {
	s.entries = append(s.entries, e)
	return e, nil
}

func (s *authzAuditSpy) count(eventType string) int {
	n := 0
	for _, e := range s.entries {
		if e.EventType == eventType {
			n++
		}
	}
	return n
}
