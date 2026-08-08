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
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// freeAddr reserves and releases an ephemeral port, returning its address for a
// runnable to re-bind. The tiny reuse window is tolerated by the dial retry.
func freeAddr(t *testing.T) string {
	t.Helper()
	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

func dialWhenUp(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	dialer := net.Dialer{Timeout: 200 * time.Millisecond}
	for {
		c, err := dialer.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func TestGRPCRunnable_ServesThenStopsGracefully(t *testing.T) {
	ing := &fakeIngestor{} // defined in server_test.go (same package)
	addr := freeAddr(t)
	r := NewGRPCRunnable(addr, &Server{Ingestor: ing}, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	conn := dialWhenUp(t, addr)
	defer func() { _ = conn.Close() }()
	client := fav1.NewFleetAdapterServiceClient(conn)

	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	stream, err := client.ControlStream(sctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil { // hello() from server_test.go
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{
		Telemetry: &fav1.TelemetryPayload{RobotId: "robot-1", TimestampMs: 1},
	}}); err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	_ = stream.CloseSend()

	waitFrames(t, ing, 1) // from server_test.go

	// Cancelling the manager context must gracefully stop the server and return.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}
}

func TestGRPCRunnable_ListenErrorSurfaces(t *testing.T) {
	// A port far outside the valid range cannot be bound; Start must return the
	// listen error rather than block.
	r := NewGRPCRunnable("127.0.0.1:999999", &Server{}, logr.Discard(), nil)
	if err := r.Start(context.Background()); err == nil {
		t.Fatal("expected a listen error for an invalid port")
	}
}

func TestGRPCRunnable_NotLeaderElected(t *testing.T) {
	// The stream server must run on every replica, not just the leader.
	if NewGRPCRunnable(":0", &Server{}, logr.Discard(), nil).NeedLeaderElection() {
		t.Fatal("ControlStream server must not be gated on leader election")
	}
}
