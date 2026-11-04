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

package controller

// The consumer end to end: a real StreamStatus server-stream, the real client
// method, and the real FleetAction status it lands on.
//
// The unit tests beside this one prove the edge-trigger; this one proves the
// piece a unit test cannot — that the since_seq handed to the executor comes
// from the PERSISTED high-water mark, so a reconnect (or a control-plane
// restart) resumes where the last durable write left off.

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/swarmada/swarmada/internal/computeclient"
	computev1 "github.com/swarmada/swarmada/proto/compute/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

type pumpStub struct {
	computev1.UnimplementedComputeRuntimeServiceServer

	mu    sync.Mutex
	log   []*fav1.TaskEvent
	asked []uint64
}

func (s *pumpStub) StreamStatus(req *computev1.StreamStatusRequest, srv computev1.ComputeRuntimeService_StreamStatusServer) error {
	s.mu.Lock()
	s.asked = append(s.asked, req.GetSinceSeq())
	log := append([]*fav1.TaskEvent(nil), s.log...)
	s.mu.Unlock()

	for _, e := range log {
		if e.GetSeq() <= req.GetSinceSeq() {
			continue
		}
		if err := srv.Send(e); err != nil {
			return err
		}
	}
	return nil // clean end of stream
}

func (s *pumpStub) setLog(evs ...*fav1.TaskEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = evs
}

func (s *pumpStub) askedWith() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.asked...)
}

func startPumpStub(t *testing.T, stub computev1.ComputeRuntimeServiceServer) *computeclient.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	computev1.RegisterComputeRuntimeServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := computeclient.Dial(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func pumpEvent(seq uint64, evType string) *fav1.TaskEvent {
	return &fav1.TaskEvent{Type: evType, Seq: seq}
}

// The pump ingests what the executor streams, and the SECOND connection asks
// from the persisted high-water mark — not from 0, and not from the oldest
// retained entry.
func TestPumpStatusStream_ResumesFromThePersistedHighWaterMark(t *testing.T) {
	stub := &pumpStub{}
	stub.setLog(pumpEvent(1, "IntruderDetected"), pumpEvent(2, "IntruderDetected"), pumpEvent(3, "IntruderDetected"))

	i, c, _ := newComputeEventIngestor(t, serviceAction("watch"))
	cc := startPumpStub(t, stub)
	ctx := context.Background()

	if err := i.PumpStatusStream(ctx, cc, evNS, "watch", "ns/watch/1/1"); err != nil {
		t.Fatalf("pump: %v", err)
	}
	got := getEvAction(t, c, "watch")
	if len(got.Status.Events) != 3 || got.Status.EventSeqHigh != 3 {
		t.Fatalf("after the first stream: events=%d seqHigh=%d, want 3 and 3", len(got.Status.Events), got.Status.EventSeqHigh)
	}
	rv := got.ResourceVersion

	// The executor raises more; the control plane reconnects.
	stub.setLog(pumpEvent(1, "IntruderDetected"), pumpEvent(2, "IntruderDetected"),
		pumpEvent(3, "IntruderDetected"), pumpEvent(4, "IntruderDetected"))
	if err := i.PumpStatusStream(ctx, cc, evNS, "watch", "ns/watch/1/1"); err != nil {
		t.Fatalf("pump (reconnect): %v", err)
	}

	asked := stub.askedWith()
	if len(asked) != 2 || asked[0] != 0 || asked[1] != 3 {
		t.Fatalf("since_seq per connection = %v, want [0 3] — the resume point is the persisted high-water mark", asked)
	}
	final := getEvAction(t, c, "watch")
	if len(final.Status.Events) != 4 || final.Status.EventSeqHigh != 4 {
		t.Fatalf("after the reconnect: events=%d seqHigh=%d, want 4 and 4", len(final.Status.Events), final.Status.EventSeqHigh)
	}
	if final.ResourceVersion == rv {
		t.Fatal("the reconnect delivered a new event but wrote nothing")
	}
}

// A reconnect that replays ONLY events already accepted writes nothing at all —
// the resume path meets the edge-trigger, and the combination is what keeps a
// reconnect off the status-write path.
func TestPumpStatusStream_AReplayOfSeenEventsWritesNothing(t *testing.T) {
	stub := &pumpStub{}
	stub.setLog(pumpEvent(1, "IntruderDetected"), pumpEvent(2, "IntruderDetected"))

	i, c, rec := newComputeEventIngestor(t, serviceAction("watch"))
	cc := startPumpStub(t, stub)
	ctx := context.Background()

	if err := i.PumpStatusStream(ctx, cc, evNS, "watch", "ns/watch/1/1"); err != nil {
		t.Fatalf("pump: %v", err)
	}
	rv := getEvAction(t, c, "watch").ResourceVersion
	drainRecorder(rec)

	// An executor that ignores since_seq and replays from the start anyway —
	// which a conforming one must not do, and which must cost nothing when one does.
	for k := 0; k < 8; k++ {
		if err := i.PumpStatusStream(ctx, cc, evNS, "watch", "ns/watch/1/1"); err != nil {
			t.Fatalf("pump %d: %v", k, err)
		}
	}
	got := getEvAction(t, c, "watch")
	if got.ResourceVersion != rv {
		t.Fatalf("resourceVersion moved from %s to %s across 8 replaying reconnects", rv, got.ResourceVersion)
	}
	if evs := drainRecorder(rec); len(evs) != 0 {
		t.Fatalf("replaying reconnects emitted %d Events: %v", len(evs), evs)
	}
}
