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

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// An EstopClearAck is checked as an EstopAck is: one with an empty robot_id, or for a robot this
// adapter may not act on, is dropped and the stream stays up; an authorized one reaches the
// dispatcher. A LocalStopReport is read and dropped, and reaches neither route.
func TestSafetyStream_EstopClearAckIsAuthorizedThenRouted(t *testing.T) {
	disp := &fakeSafetyDispatcher{}
	auth := &fakeAuthorizer{allow: map[string]bool{"amr-1": true}}
	client, cleanup := newTestServer(t, &Server{Safety: disp, Authorizer: auth})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open safety stream: %v", err)
	}
	for _, m := range []*fav1.AdapterSafetyMessage{
		{RobotId: "", Payload: &fav1.AdapterSafetyMessage_EstopClearAck{EstopClearAck: &fav1.EstopClearAck{EstopId: "s0"}}},
		{RobotId: "amr-forged", Payload: &fav1.AdapterSafetyMessage_EstopClearAck{EstopClearAck: &fav1.EstopClearAck{EstopId: "s1"}}},
		{RobotId: "amr-1", Payload: &fav1.AdapterSafetyMessage_LocalStop{LocalStop: &fav1.LocalStopReport{Latched: true}}},
		{RobotId: "amr-1", Payload: &fav1.AdapterSafetyMessage_EstopClearAck{EstopClearAck: &fav1.EstopClearAck{EstopId: "s2"}}},
	} {
		if err := stream.Send(m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("a refused EstopClearAck tore down the SafetyStream: %v", err)
		}
	}
	if n := disp.clearCount(); n != 1 || disp.cleared[0].GetEstopId() != "s2" || disp.count() != 0 {
		t.Fatalf("routed %d EstopClearAcks (%v) and %d EstopAcks; want only s2, and no EstopAck", n, disp.cleared, disp.count())
	}
}
