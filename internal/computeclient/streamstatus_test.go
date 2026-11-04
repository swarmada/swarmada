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

package computeclient_test

// StreamStatus is the one client method the Grpc binding was missing: the
// generated stubs existed and had zero Go callers, so nothing in the control
// plane could ever receive a semantic event.
//
// What these tests pin is the RESUMPTION CONTRACT, because that is the half a
// compiling method does not give you: since_seq must reach the executor
// unchanged, and it is the high-water mark, never the floor of the retained
// window.

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	computev1 "github.com/swarmada/swarmada/proto/compute/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// eventStub is a compute runtime holding a fixed event log. It serves exactly
// the events with seq > since_seq, which is the replay rule the RPC's own
// comment states.
type eventStub struct {
	computev1.UnimplementedComputeRuntimeServiceServer

	log []*fav1.TaskEvent

	mu       sync.Mutex
	asked    []uint64 // every since_seq the control plane asked with
	askedInv []string
}

func (s *eventStub) StreamStatus(req *computev1.StreamStatusRequest, srv computev1.ComputeRuntimeService_StreamStatusServer) error {
	s.mu.Lock()
	s.asked = append(s.asked, req.GetSinceSeq())
	s.askedInv = append(s.askedInv, req.GetInvocationId())
	s.mu.Unlock()

	for _, e := range s.log {
		if e.GetSeq() <= req.GetSinceSeq() {
			continue
		}
		if err := srv.Send(e); err != nil {
			return err
		}
	}
	return nil
}

func (s *eventStub) askedWith() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.asked...)
}

func logOf(seqs ...uint64) []*fav1.TaskEvent {
	out := make([]*fav1.TaskEvent, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, &fav1.TaskEvent{Type: "IntruderDetected", Seq: s})
	}
	return out
}

func drain(t *testing.T, st interface {
	Recv() (*fav1.TaskEvent, error)
}) []uint64 {
	t.Helper()
	var got []uint64
	for {
		e, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, e.GetSeq())
	}
}

// A first connection asks from 0 and receives the whole log. 0 is correct for a
// first connection precisely because seq is documented as monotonic from the
// run's beginning: an action that has accepted nothing wants everything.
func TestStreamStatus_ZeroMeansFromTheStart(t *testing.T) {
	stub := &eventStub{log: logOf(1, 2, 3)}
	c := dial(t, startStub(t, stub))

	st, err := c.StreamStatus(context.Background(), "ns/act/1/1", 0)
	if err != nil {
		t.Fatalf("StreamStatus: %v", err)
	}
	if got := drain(t, st); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("received %v, want the whole log 1,2,3", got)
	}
	if asked := stub.askedWith(); len(asked) != 1 || asked[0] != 0 {
		t.Fatalf("executor was asked with since_seq %v, want [0]", asked)
	}
}

// THE RESUMPTION CONTRACT. A reconnect asks from the HIGH-WATER MARK and gets
// only what it has never seen. Asking from the floor of the retained window
// instead would replay events already evicted, evict newer ones to make room,
// and ask for them again on the next reconnect — a resume that cannot converge,
// which is why status.events[] documents retention as a permanent loss.
func TestStreamStatus_SinceSeqResumesFromTheHighWaterMark(t *testing.T) {
	stub := &eventStub{log: logOf(1, 2, 3, 4, 5)}
	c := dial(t, startStub(t, stub))

	first, err := c.StreamStatus(context.Background(), "ns/act/1/1", 0)
	if err != nil {
		t.Fatalf("StreamStatus: %v", err)
	}
	seen := drain(t, first)
	high := seen[len(seen)-1]

	// Reconnect. The control plane asks from what it last ACCEPTED, not from
	// what it still retains.
	second, err := c.StreamStatus(context.Background(), "ns/act/1/1", high)
	if err != nil {
		t.Fatalf("StreamStatus (reconnect): %v", err)
	}
	if got := drain(t, second); len(got) != 0 {
		t.Fatalf("a reconnect at the high-water mark replayed %v; it must receive only what it has never seen", got)
	}

	// And a run that has moved on since delivers only the new tail.
	stub.log = logOf(1, 2, 3, 4, 5, 6, 7)
	third, err := c.StreamStatus(context.Background(), "ns/act/1/1", high)
	if err != nil {
		t.Fatalf("StreamStatus (tail): %v", err)
	}
	if got := drain(t, third); len(got) != 2 || got[0] != 6 || got[1] != 7 {
		t.Fatalf("received %v, want only the unseen tail 6,7", got)
	}

	if asked := stub.askedWith(); len(asked) != 3 || asked[0] != 0 || asked[1] != 5 || asked[2] != 5 {
		t.Fatalf("since_seq reached the executor as %v, want [0 5 5] — the value must not be rewritten in transit", asked)
	}
}

// Cancelling the caller's context ends the stream. There is no Close method
// because a server-stream's lifetime IS its context's, and a second way to end
// it would be a second thing to get wrong.
func TestStreamStatus_ContextCancellationEndsTheStream(t *testing.T) {
	stub := &eventStub{log: logOf(1, 2, 3)}
	c := dial(t, startStub(t, stub))

	ctx, cancel := context.WithCancel(context.Background())
	st, err := c.StreamStatus(ctx, "ns/act/1/1", 0)
	if err != nil {
		t.Fatalf("StreamStatus: %v", err)
	}
	cancel()
	for {
		_, err := st.Recv()
		if err != nil {
			return // any terminating error is acceptable; hanging is not
		}
	}
}
