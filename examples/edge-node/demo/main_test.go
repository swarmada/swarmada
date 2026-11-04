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
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/edge"
	"github.com/swarmada/swarmada/internal/zone"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// TestRunScenario_ConfirmedEdgeEstop runs the demo scenario against an in-process edge
// Node and asserts the full path completes: in-zone → breach → headless estop →
// confirmed. This is the same path the run-demo.sh script exercises against cmd/edge.
func TestRunScenario_ConfirmedEdgeEstop(t *testing.T) {
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
	defer gs.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runScenario(ctx, fav1.NewEdgeServiceClient(conn), "amr-1", t.Logf); err != nil {
		t.Fatalf("demo scenario failed: %v", err)
	}

	// The edge node should have recorded a CONFIRMED headless stop.
	var confirmed bool
	for i := 0; i < 200; i++ {
		for _, e := range sink.ForNamespace("warehouse-a") {
			if e.Detail["state"] == "Stopped" && e.Detail["confirmed"] == "true" {
				confirmed = true
			}
		}
		if confirmed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !confirmed {
		t.Fatal("expected a CONFIRMED headless estop in the edge audit log")
	}
}

// TestRunScenario_UnknownRobotNoEstop confirms the demo cannot fabricate an estop for
// a robot the edge node does not guard (absence of a matching config → no estop).
func TestRunScenario_UnknownRobotNoEstop(t *testing.T) {
	node := edge.New(edge.Config{Namespace: "warehouse-a"}, audit.New(&audit.MemorySink{}, "test"))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	fav1.RegisterEdgeServiceServer(gs, node)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// No zones/robots configured → the breach frame is ignored → Recv blocks → the
	// scenario returns a deadline error rather than a fabricated estop.
	if err := runScenario(ctx, fav1.NewEdgeServiceClient(conn), "ghost", t.Logf); err == nil {
		t.Fatal("expected no estop for an unguarded robot, but the scenario reported success")
	}
}
