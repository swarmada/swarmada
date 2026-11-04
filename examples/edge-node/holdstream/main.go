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

// Command holdstream plays a Fleet Adapter's EdgeStream role and HOLDS the stream
// open, which the local-safety-input path needs: cmd/edge fans a zone-wide headless
// estop to every *connected* EdgeStream, so something has to still be connected when
// the input trips. The sibling demo client exits as soon as its boundary-breach estop
// is confirmed, so it cannot show that path.
//
// It tees one in-zone position (so the edge node sees a live adapter), then blocks,
// logging every Estop it receives and replying with a confirming EstopAck — the same
// confirmed-only discipline the demo client uses. No control plane and no hardware.
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
	robotID := flag.String("robot-id", "amr-1", "robot to tee an in-zone position for")
	hold := flag.Duration("hold", 30*time.Second, "how long to hold the stream open")
	label := flag.String("label", "stream", "label for this stream in the log")
	ack := flag.Bool("ack", true, "reply with a confirming EstopAck(STOPPED)")
	flag.Parse()

	conn, err := grpc.NewClient(*edgeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial edge %q: %v", *edgeAddr, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *hold)
	defer cancel()

	if err := holdStream(ctx, fav1.NewEdgeServiceClient(conn), *robotID, *label, *ack, log.Printf); err != nil {
		fmt.Fprintf(os.Stderr, "holdstream FAILED: %v\n", err)
		os.Exit(1)
	}
}

// holdStream opens an EdgeStream, tees one in-zone position, then serves estops until
// ctx expires or the stream closes.
func holdStream(ctx context.Context, client fav1.EdgeServiceClient, robotID, label string,
	ack bool, logf func(string, ...any)) error {
	stream, err := client.EdgeStream(ctx)
	if err != nil {
		return fmt.Errorf("open EdgeStream: %w", err)
	}

	x, y, floor := 10.0, 10.0, int32(0)
	if err := stream.Send(&fav1.AdapterEdgeMessage{Msg: &fav1.AdapterEdgeMessage_Position{
		Position: &fav1.PositionFrame{
			RobotId:  robotID,
			Position: &fav1.RobotPosition{X: &x, Y: &y, Floor: &floor},
			TsMs:     time.Now().UnixMilli(),
		}}}); err != nil {
		return fmt.Errorf("tee in-zone frame: %w", err)
	}
	logf("%s: connected; teed in-zone position (10,10) for %s — holding the stream open", label, robotID)

	for {
		msg, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				logf("%s: hold elapsed; closing", label)
				return nil
			}
			return fmt.Errorf("stream closed: %w", err)
		}
		est := msg.GetEstop()
		if est == nil {
			logf("%s: ignoring non-estop message %T", label, msg.GetMsg())
			continue
		}
		logf("%s: received headless Estop id=%q reason=%q issuedBy=%q",
			label, est.GetEstopId(), est.GetReason(), est.GetIssuedBy())
		if !ack {
			continue
		}
		if err := stream.Send(&fav1.AdapterEdgeMessage{Msg: &fav1.AdapterEdgeMessage_EstopAck{
			EstopAck: &fav1.EstopAck{EstopId: est.GetEstopId(), State: fav1.EstopState_ESTOP_STATE_STOPPED},
		}}); err != nil {
			return fmt.Errorf("send confirming EstopAck: %w", err)
		}
		logf("%s: sent EstopAck(STOPPED) — the edge node records a CONFIRMED headless stop", label)
	}
}
