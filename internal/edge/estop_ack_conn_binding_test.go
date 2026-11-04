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

package edge

import (
	"testing"
	"time"

	"github.com/swarmada/swarmada/internal/audit"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A headless estop is confirmed only by an EstopAck on the EdgeStream it was pushed down
// (RFC-0001 #safety-edge-headless-emergency-stop). The edge stream carries no identity to
// authorize an ack against, and an EstopAck names no robot, so the connection is the only
// thing that ties an ack to the adapter that was told to stop. An ack on any other stream
// is refused and recorded.

// waitEstopEntry returns the headless-estop record for robotID, skipping any other entry.
func waitEstopEntry(t *testing.T, sink *audit.MemorySink, robotID string) audit.Entry {
	t.Helper()
	for i := 0; i < 400; i++ {
		for _, e := range sink.ForNamespace(edgeNS) {
			if e.EventType == eventEdgeEstop && e.Detail["robot"] == robotID {
				return e
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no edge estop record")
	return audit.Entry{}
}

func TestEdge_AckOnAnotherStreamDoesNotConfirmTheEstop(t *testing.T) {
	n, sink := newNode(t)
	a, b := newFakeStream(), newFakeStream()
	defer close(a.done)
	defer close(b.done)
	go func() { _ = n.EdgeStream(a) }()
	go func() { _ = n.EdgeStream(b) }()

	a.in <- posMsg("amr-1", 50, 50, 0) // breach reported on stream A
	id := (<-a.out).GetEstop().GetEstopId()
	b.in <- ackMsg(id, fav1.EstopState_ESTOP_STATE_STOPPED) // the ack arrives on stream B only

	e := waitEstopEntry(t, sink, "amr-1")
	if e.Detail["state"] == "Stopped" || e.Detail["confirmed"] == "true" {
		t.Fatalf("SAFETY: an ack on stream B confirmed the estop pushed down stream A: %+v", e.Detail)
	}
	var refused bool
	for _, d := range sink.ForNamespace(edgeNS) {
		if d.EventType == audit.EventRobotAuthzDenied && d.Outcome == audit.OutcomeDenied && d.Resource.Name == "amr-1" {
			refused = true
		}
	}
	if !refused {
		t.Errorf("the ack on stream B was not recorded as refused")
	}
}

func TestEdge_AckOnTheSendingStreamConfirmsTheEstop(t *testing.T) {
	n, sink := newNode(t)
	a, b := newFakeStream(), newFakeStream()
	defer close(a.done)
	defer close(b.done)
	go func() { _ = n.EdgeStream(a) }()
	go func() { _ = n.EdgeStream(b) }()

	a.in <- posMsg("amr-1", 50, 50, 0)
	id := (<-a.out).GetEstop().GetEstopId()
	a.in <- ackMsg(id, fav1.EstopState_ESTOP_STATE_STOPPED)

	e := waitEstopEntry(t, sink, "amr-1")
	if e.Detail["state"] != "Stopped" || e.Detail["confirmed"] != "true" {
		t.Fatalf("an ack on the sending stream did not confirm the estop: %+v", e.Detail)
	}
}
