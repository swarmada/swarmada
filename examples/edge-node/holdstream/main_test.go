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

package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/edge"
	"github.com/swarmada/swarmada/internal/zone"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// recorder collects a client's log lines for assertions.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, format)
	_ = args
}

func (r *recorder) count(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

func newNode(t *testing.T) (*edge.Node, *audit.MemorySink, string) {
	t.Helper()
	sink := &audit.MemorySink{}
	node := edge.New(edge.Config{
		Namespace: "warehouse-a",
		Zones: []edge.ZonePolygon{{Name: "warehouse-a", Floor: 0, Polygon: []zone.Point{
			{X: 0, Y: 0}, {X: 20, Y: 0}, {X: 20, Y: 20}, {X: 0, Y: 20}}}},
		RobotZone: map[string]string{"amr-1": "warehouse-a"},
	}, audit.New(sink, "test"))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	fav1.RegisterEdgeServiceServer(gs, node)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return node, sink, lis.Addr().String()
}

func dial(t *testing.T, addr string) fav1.EdgeServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fav1.NewEdgeServiceClient(conn)
}

// TestHoldStream_ConfirmsALocalSafetyEstop is the property the safety-input beat of the
// UC-4.1 take depends on: the client stays connected after teeing its position, so a
// local-input estop reaches it, and it answers with a confirming EstopAck.
func TestHoldStream_ConfirmsALocalSafetyEstop(t *testing.T) {
	node, sink, addr := newNode(t)
	rec := &recorder{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- holdStream(ctx, dial(t, addr), "amr-1", "adapter-1", true, rec.logf) }()

	// Retry the trigger until the stream is registered on the node.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && rec.count("received headless Estop") == 0 {
		node.TriggerLocalEstop(context.Background(), `safety input "test" asserted`)
		time.Sleep(20 * time.Millisecond)
	}
	if rec.count("received headless Estop") == 0 {
		t.Fatal("the held-open stream never received the local-input estop")
	}

	var confirmed bool
	for i := 0; i < 200 && !confirmed; i++ {
		for _, e := range sink.ForNamespace("warehouse-a") {
			if e.Detail["state"] == "Stopped" && e.Detail["confirmed"] == "true" {
				confirmed = true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !confirmed {
		t.Fatal("expected a CONFIRMED headless stop in the edge audit log")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("holdStream returned an error on a clean hold expiry: %v", err)
	}
}

// TestHoldStream_EveryConnectedStreamIsEstopped covers the claim the narration makes:
// a local input stops the zone, not just the stream that teed last. One connected
// client cannot tell "zone-wide" from "sent to me"; two can.
func TestHoldStream_EveryConnectedStreamIsEstopped(t *testing.T) {
	node, _, addr := newNode(t)
	recA, recB := &recorder{}, &recorder{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = holdStream(ctx, dial(t, addr), "amr-1", "adapter-1", true, recA.logf) }()
	go func() { _ = holdStream(ctx, dial(t, addr), "amr-1", "adapter-2", true, recB.logf) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recA.count("received headless Estop") > 0 && recB.count("received headless Estop") > 0 {
			return
		}
		node.TriggerLocalEstop(context.Background(), `safety input "test" asserted`)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected both streams to be estopped; adapter-1 got %d, adapter-2 got %d",
		recA.count("received headless Estop"), recB.count("received headless Estop"))
}
