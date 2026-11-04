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
	"testing"
	"time"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A robot_id is unique per namespace only, so a telemetry frame must carry the
// namespace its stream declared, or a Robot cannot be resolved from it.
//
// The second case establishes that the namespace CAN be empty on this path: the
// handshake accepts an AdapterHello with no namespace, and the frame then reaches the
// ingestor with none. That is why the resolvers keep a defined behaviour for an empty
// namespace instead of assuming one.
func TestControlStream_TelemetryFrameCarriesTheStreamNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, declared string
	}{
		{"declared", "warehouse-east"},
		{"none declared", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ing := &fakeIngestor{}
			client, cleanup := newTestServer(t, &Server{Ingestor: ing})
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.ControlStream(ctx)
			if err != nil {
				t.Fatalf("open stream: %v", err)
			}
			h := hello()
			h.GetHello().Namespace = tc.declared
			if err := stream.Send(h); err != nil {
				t.Fatalf("send hello: %v", err)
			}
			reply, err := stream.Recv()
			if err != nil {
				t.Fatalf("recv hello ack: %v", err)
			}
			if !reply.GetHelloAck().GetAccepted() {
				t.Fatalf("hello with namespace %q refused: %+v", tc.declared, reply.GetHelloAck())
			}
			if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{
				Telemetry: &fav1.TelemetryPayload{RobotId: "robot-1", Battery: &fav1.BatteryStatus{Percent: i32(50)}},
			}}); err != nil {
				t.Fatalf("send telemetry: %v", err)
			}
			if err := stream.CloseSend(); err != nil {
				t.Fatalf("close send: %v", err)
			}

			fr := waitFrames(t, ing, 1)[0]
			if fr.Namespace != tc.declared {
				t.Fatalf("frame namespace = %q, want %q (the namespace the stream declared)", fr.Namespace, tc.declared)
			}
		})
	}
}
