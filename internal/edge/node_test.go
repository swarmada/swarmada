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
	"context"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/zone"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

const edgeNS = "warehouse-a"

// fakeEdgeStream is an in-process EdgeService.EdgeStream for deterministic tests.
type fakeEdgeStream struct {
	grpc.ServerStream
	ctx  context.Context
	in   chan *fav1.AdapterEdgeMessage
	out  chan *fav1.EdgeControlMessage
	done chan struct{}
}

func newFakeStream() *fakeEdgeStream {
	return &fakeEdgeStream{
		ctx:  context.Background(),
		in:   make(chan *fav1.AdapterEdgeMessage, 8),
		out:  make(chan *fav1.EdgeControlMessage, 8),
		done: make(chan struct{}),
	}
}

func (f *fakeEdgeStream) Recv() (*fav1.AdapterEdgeMessage, error) {
	select {
	case m := <-f.in:
		return m, nil
	case <-f.done:
		return nil, io.EOF
	}
}
func (f *fakeEdgeStream) Send(m *fav1.EdgeControlMessage) error {
	select {
	case f.out <- m:
		return nil
	case <-f.done:
		return io.EOF
	}
}
func (f *fakeEdgeStream) Context() context.Context { return f.ctx }

func fp(f float64) *float64 { return &f }
func ip(i int32) *int32     { return &i }

func posMsg(robotID string, x, y float64, floor int32) *fav1.AdapterEdgeMessage {
	return &fav1.AdapterEdgeMessage{Msg: &fav1.AdapterEdgeMessage_Position{Position: &fav1.PositionFrame{
		RobotId: robotID, Position: &fav1.RobotPosition{X: fp(x), Y: fp(y), Floor: ip(floor)}, TsMs: 1,
	}}}
}

func ackMsg(id string, st fav1.EstopState) *fav1.AdapterEdgeMessage {
	return &fav1.AdapterEdgeMessage{Msg: &fav1.AdapterEdgeMessage_EstopAck{EstopAck: &fav1.EstopAck{EstopId: id, State: st}}}
}

func newNode(t *testing.T) (*Node, *audit.MemorySink) {
	t.Helper()
	sink := &audit.MemorySink{}
	square := []zone.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}
	n := New(Config{
		Namespace: edgeNS,
		Zones:     []ZonePolygon{{Name: "z", Floor: 0, Polygon: square}},
		RobotZone: map[string]string{"amr-1": "z"},
	}, audit.New(sink, "v0.1.0"))
	n.deliveryTimeout = 60 * time.Millisecond
	n.confirmTimeout = 60 * time.Millisecond
	return n, sink
}

func waitEdgeEntry(t *testing.T, sink *audit.MemorySink) audit.Entry {
	t.Helper()
	for i := 0; i < 200; i++ {
		if es := sink.ForNamespace(edgeNS); len(es) > 0 {
			return es[len(es)-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no edge audit entry recorded")
	return audit.Entry{}
}

// A confirmed out-of-zone position triggers an estop, confirmed STOPPED by the ack.
func TestEdge_BreachConfirmedStopped(t *testing.T) {
	n, sink := newNode(t)
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()

	fs.in <- posMsg("amr-1", 50, 50, 0) // outside the square → breach
	est := <-fs.out
	id := est.GetEstop().GetEstopId()
	if id == "" {
		t.Fatal("no estop issued on breach")
	}
	fs.in <- ackMsg(id, fav1.EstopState_ESTOP_STATE_STOPPED)

	e := waitEdgeEntry(t, sink)
	if e.EventType != eventEdgeEstop || e.Detail["state"] != "Stopped" || e.Detail["confirmed"] != "true" {
		t.Fatalf("audit = %+v, want confirmed Stopped edge estop", e)
	}
}

// A dropped estop (no ack) is NEVER recorded as Stopped — it resolves to Failed.
func TestEdge_DroppedNeverStopped(t *testing.T) {
	n, sink := newNode(t)
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()

	fs.in <- posMsg("amr-1", 50, 50, 0)
	<-fs.out // the estop; the adapter never acks

	e := waitEdgeEntry(t, sink)
	if e.Detail["state"] == "Stopped" || e.Detail["confirmed"] == "true" {
		t.Fatalf("SAFETY: dropped edge estop recorded as stopped: %+v", e)
	}
	if e.Detail["state"] != "Failed" {
		t.Fatalf("audit state = %q, want Failed", e.Detail["state"])
	}
}

// Staleness / an in-zone position never triggers an estop (absence ≠ breach).
func TestEdge_InZoneNoEstop(t *testing.T) {
	n, _ := newNode(t)
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()

	fs.in <- posMsg("amr-1", 5, 5, 0) // inside the square
	select {
	case <-fs.out:
		t.Fatal("estop issued for an in-zone robot")
	case <-time.After(80 * time.Millisecond):
	}
}

// A PositionFrame for a robot not in the cached set is ignored — the edge cannot
// be made to estop an unknown/forged robot.
func TestEdge_UnknownRobotIgnored(t *testing.T) {
	n, _ := newNode(t)
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()

	fs.in <- posMsg("ghost-bot", 50, 50, 0) // out of zone, but unknown robot
	select {
	case <-fs.out:
		t.Fatal("estop issued for an unknown robot_id")
	case <-time.After(80 * time.Millisecond):
	}
}

// A wrong-floor position is out of zone → breach.
func TestEdge_WrongFloorIsBreach(t *testing.T) {
	n, sink := newNode(t)
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()

	fs.in <- posMsg("amr-1", 5, 5, 3) // inside X/Y but wrong floor
	est := <-fs.out
	fs.in <- ackMsg(est.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)
	if e := waitEdgeEntry(t, sink); e.Detail["confirmed"] != "true" {
		t.Fatalf("wrong-floor breach not confirmed-stopped: %+v", e)
	}
}

// The local (GPIO) safety input estops every connected stream.
func TestEdge_LocalEstopFansOut(t *testing.T) {
	n, _ := newNode(t)
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()
	// Let the stream register.
	time.Sleep(20 * time.Millisecond)

	n.TriggerLocalEstop(context.Background(), "e-stop button pressed")
	select {
	case est := <-fs.out:
		if est.GetEstop() == nil {
			t.Fatal("local estop did not carry an Estop")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("local estop did not reach the connected stream")
	}
}

// The local edge audit log is tamper-evident (the §F-4 chain), so a modified
// edge estop record is detectable.
func TestEdge_AuditLogTamperEvident(t *testing.T) {
	n, sink := newNode(t)
	fs := newFakeStream()
	defer close(fs.done)
	go func() { _ = n.EdgeStream(fs) }()

	fs.in <- posMsg("amr-1", 50, 50, 0)
	est := <-fs.out
	fs.in <- ackMsg(est.GetEstop().GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)
	waitEdgeEntry(t, sink)

	entries := sink.ForNamespace(edgeNS)
	if res := audit.Verify(entries); !res.OK {
		t.Fatalf("clean edge log failed verification: %+v", res)
	}
	entries[0].Detail["state"] = "tampered"
	if res := audit.Verify(entries); res.OK {
		t.Fatal("tampered edge estop record not detected")
	}
}
