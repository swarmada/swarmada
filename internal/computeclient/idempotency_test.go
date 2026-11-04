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

// A step retried by Swarmada's own retryPolicy and by the transport
// (e.g. a resent RPC) must produce exactly ONE side effect at the callee. The
// design puts the deduplication responsibility on the
// EXECUTOR, keyed by Invocation.id — "an executor MUST return the same result
// for a repeated id rather than performing the work twice". This test proves
// the client carries a key an executor CAN dedup on, using a counting stub
// that implements exactly that contract.

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/swarmada/swarmada/internal/computeclient"
	computev1 "github.com/swarmada/swarmada/proto/compute/v1"
)

// countingStub is a fake compute runtime that performs its "side effect"
// (modelled here as an incrementing counter — in a real executor, a database
// write or a partner-API call) at most once per Invocation.id, exactly as the
// design requires of a conforming executor.
type countingStub struct {
	computev1.UnimplementedComputeRuntimeServiceServer

	mu          sync.Mutex
	seen        map[string]bool
	sideEffects int
}

func (s *countingStub) Activate(_ context.Context, req *computev1.ActivateRequest) (*computev1.ActivateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	id := req.GetInvocation().GetId()
	if !s.seen[id] {
		s.seen[id] = true
		s.sideEffects++
	}
	return &computev1.ActivateResponse{Accepted: true}, nil
}

func (s *countingStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sideEffects
}

func startStub(t *testing.T, stub computev1.ComputeRuntimeServiceServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	computev1.RegisterComputeRuntimeServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func dial(t *testing.T, addr string) *computeclient.Client {
	t.Helper()
	c, err := computeclient.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// THE CRITICAL ONE: a step retried by BOTH Swarmada
// (our own controller re-issuing Activate for the same attempt after, say, a
// transient reconcile error) AND the transport (an RPC resend) must land as
// exactly one side effect at the callee.
func TestActivate_SwarmadaAndTransportRetryProduceExactlyOneSideEffect(t *testing.T) {
	stub := &countingStub{}
	c := dial(t, startStub(t, stub))

	key := computeclient.IdempotencyKey("yard-west", "plate-read", 44, 1)
	inv := computeclient.Invocation{ID: key, RunGeneration: 44, Attempt: 1}

	// Three calls carrying the IDENTICAL Invocation.id: the first is the
	// original dispatch, the second stands in for Swarmada's own retry, the
	// third for the transport independently resending the request.
	for i, label := range []string{"original dispatch", "swarmada retry", "transport retry"} {
		accepted, _, err := c.Activate(context.Background(), inv, nil)
		if err != nil {
			t.Fatalf("call %d (%s): Activate: %v", i, label, err)
		}
		if !accepted {
			t.Fatalf("call %d (%s): not accepted", i, label)
		}
	}

	if got := stub.count(); got != 1 {
		t.Fatalf("expected exactly one side effect at the callee, got %d", got)
	}
}

// A genuinely NEW attempt (a real retry after a real change — e.g. the action
// was requeued and retryCount incremented) carries a DIFFERENT idempotency
// key and MUST produce its own side effect. Without this the dedup above
// would be indistinguishable from "the stub never does anything twice".
func TestActivate_ADifferentAttemptIsANewSideEffect(t *testing.T) {
	stub := &countingStub{}
	c := dial(t, startStub(t, stub))

	first := computeclient.Invocation{ID: computeclient.IdempotencyKey("yard-west", "plate-read", 44, 1), RunGeneration: 44, Attempt: 1}
	second := computeclient.Invocation{ID: computeclient.IdempotencyKey("yard-west", "plate-read", 44, 2), RunGeneration: 44, Attempt: 2}

	if _, _, err := c.Activate(context.Background(), first, nil); err != nil {
		t.Fatalf("Activate (attempt 1): %v", err)
	}
	if _, _, err := c.Activate(context.Background(), second, nil); err != nil {
		t.Fatalf("Activate (attempt 2): %v", err)
	}

	if got := stub.count(); got != 2 {
		t.Fatalf("two distinct attempts must produce two side effects, got %d", got)
	}
}

func TestIdempotencyKey_Shape(t *testing.T) {
	got := computeclient.IdempotencyKey("yard-west", "plate-read", 44, 1)
	want := "yard-west/plate-read/44/1"
	if got != want {
		t.Fatalf("IdempotencyKey( = %q, want %q (Http-binding envelope shape)", got, want)
	}
}
