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

// Command demo plays a Fleet Adapter's EdgeStream role against a running cmd/edge
// node — the same messages the reference simulation adapter tees and confirms. It
// tees an in-zone position (no estop), then an out-of-zone position (a boundary
// breach), receives the edge node's headless Estop, and replies with a confirming
// EstopAck so the edge records a CONFIRMED stop. No control plane and no hardware.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func main() {
	edgeAddr := flag.String("edge-address", "localhost:9600", "cmd/edge EdgeStream address")
	robotID := flag.String("robot-id", "amr-1", "robot to tee positions for (must be in the edge config)")
	flag.Parse()

	conn, err := grpc.NewClient(*edgeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial edge %q: %v", *edgeAddr, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := runScenario(ctx, fav1.NewEdgeServiceClient(conn), *robotID, log.Printf); err != nil {
		fmt.Fprintf(os.Stderr, "demo FAILED: %v\n", err)
		os.Exit(1)
	}
	log.Printf("demo OK: headless estop issued on the boundary breach and CONFIRMED")
}

func posFrame(robotID string, x, y float64, floor int32) *fav1.AdapterEdgeMessage {
	return &fav1.AdapterEdgeMessage{Msg: &fav1.AdapterEdgeMessage_Position{Position: &fav1.PositionFrame{
		RobotId:  robotID,
		Position: &fav1.RobotPosition{X: &x, Y: &y, Floor: &floor},
		TsMs:     time.Now().UnixMilli(),
	}}}
}

func ackMsg(estopID string, state fav1.EstopState) *fav1.AdapterEdgeMessage {
	return &fav1.AdapterEdgeMessage{Msg: &fav1.AdapterEdgeMessage_EstopAck{
		EstopAck: &fav1.EstopAck{EstopId: estopID, State: state}}}
}

// runScenario drives the in-zone → breach → confirmed-estop path. It is the testable
// core of the demo (see main_test.go).
func runScenario(ctx context.Context, client fav1.EdgeServiceClient, robotID string,
	logf func(string, ...any)) error {
	stream, err := client.EdgeStream(ctx)
	if err != nil {
		return fmt.Errorf("open EdgeStream: %w", err)
	}

	// 1. In-zone position (10,10) inside the 20×20 zone → no breach, no estop.
	if err := stream.Send(posFrame(robotID, 10, 10, 0)); err != nil {
		return fmt.Errorf("tee in-zone frame: %w", err)
	}
	logf("teed in-zone position (10,10) for %s — inside the zone, no estop expected", robotID)
	time.Sleep(100 * time.Millisecond) // let the edge process it (it produces nothing)

	// 2. Out-of-zone position (50,50) → a confirmed boundary breach.
	if err := stream.Send(posFrame(robotID, 50, 50, 0)); err != nil {
		return fmt.Errorf("tee out-of-zone frame: %w", err)
	}
	logf("teed OUT-OF-ZONE position (50,50) — a boundary breach; expecting a headless estop")

	// 3. Receive the edge node's headless Estop.
	msg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("awaiting headless estop: %w", err)
	}
	est := msg.GetEstop()
	if est == nil {
		return fmt.Errorf("expected an Estop, got %T", msg.GetMsg())
	}
	logf("received headless Estop id=%q reason=%q issuedBy=%q",
		est.GetEstopId(), est.GetReason(), est.GetIssuedBy())

	// 4. Confirm STOPPED — only this makes the edge record a confirmed stop; a dropped
	//    ack would resolve to Failed, never a false Stopped.
	if err := stream.Send(ackMsg(est.GetEstopId(), fav1.EstopState_ESTOP_STATE_STOPPED)); err != nil {
		return fmt.Errorf("send confirming EstopAck: %w", err)
	}
	logf("sent EstopAck(STOPPED) — the edge node records a CONFIRMED headless stop")
	return nil
}
