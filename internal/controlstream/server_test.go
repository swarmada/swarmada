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
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/telemetry"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// fakeIngestor is a concurrency-safe TelemetryIngestor recording every frame.
type fakeIngestor struct {
	mu     sync.Mutex
	frames []telemetry.Frame
}

func (f *fakeIngestor) Ingest(_ context.Context, fr telemetry.Frame) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames = append(f.frames, fr)
	return nil
}

func (f *fakeIngestor) snapshot() []telemetry.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telemetry.Frame(nil), f.frames...)
}

// fakeRegistrar is a concurrency-safe Registrar capturing what it was asked.
type fakeRegistrar struct {
	mu        sync.Mutex
	registers []*fav1.RegisterRobot
	discovers []*fav1.DiscoverRobot
	ids       []AdapterIdentity
}

func (r *fakeRegistrar) Register(_ context.Context, id AdapterIdentity, msg *fav1.RegisterRobot) *fav1.RegisterAck {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registers = append(r.registers, msg)
	r.ids = append(r.ids, id)
	return &fav1.RegisterAck{Accepted: true, TelemetryIntervalSeconds: 10, Message: "ok"}
}

func (r *fakeRegistrar) Discover(_ context.Context, id AdapterIdentity, _ TLSIdentity, msg *fav1.DiscoverRobot) *fav1.DiscoverAck {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discovers = append(r.discovers, msg)
	r.ids = append(r.ids, id)
	return &fav1.DiscoverAck{Accepted: true, DiscoveredRobotName: "dr-" + msg.GetRobotId()}
}

// newTestServer starts srv on an in-memory bufconn listener and returns a
// connected client plus a cleanup func.
func newTestServer(t *testing.T, srv *Server) (fav1.FleetAdapterServiceClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fav1.RegisterFleetAdapterServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		gs.Stop()
	}
	return fav1.NewFleetAdapterServiceClient(conn), cleanup
}

func hello() *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Hello{Hello: &fav1.AdapterHello{
		Vendor:          "acme",
		AdapterVersion:  "1.2.3",
		ProtocolVersion: ProtocolVersion,
		// ADR-0032: a hello must report a compatible contract_version or registration is refused
		// (fail closed). The default fixture is compatible; the version gate's own tests build
		// deliberately incompatible hellos, see server_contract_gate_test.go.
		ContractVersion: contract.Version,
		Namespace:       "warehouse-east",
	}}}
}

func TestControlStream_Handshake(t *testing.T) {
	client, cleanup := newTestServer(t, &Server{})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	ack := reply.GetHelloAck()
	if ack == nil || !ack.GetAccepted() {
		t.Fatalf("hello ack = %+v, want accepted", ack)
	}
	if ack.GetNegotiatedProtocolVersion() != ProtocolVersion {
		t.Fatalf("negotiated version = %q, want %q", ack.GetNegotiatedProtocolVersion(), ProtocolVersion)
	}
}

func TestControlStream_UnsupportedProtocolRefused(t *testing.T) {
	client, cleanup := newTestServer(t, &Server{})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	h := hello()
	h.GetHello().ProtocolVersion = "fleet_adapter.v2"
	if err := stream.Send(h); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if ack := reply.GetHelloAck(); ack == nil || ack.GetAccepted() {
		t.Fatalf("hello ack = %+v, want refused", ack)
	}
	// Server closes the stream after refusing.
	if _, err := stream.Recv(); err == nil {
		t.Fatal("expected stream close after refused handshake")
	}
}

func TestControlStream_FirstMessageMustBeHello(t *testing.T) {
	client, cleanup := newTestServer(t, &Server{})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	// Open with telemetry instead of hello.
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{
		Telemetry: &fav1.TelemetryPayload{RobotId: "r1"},
	}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v (code %v), want FailedPrecondition", err, status.Code(err))
	}
}

func TestControlStream_RegisterAndDiscover(t *testing.T) {
	reg := &fakeRegistrar{}
	client, cleanup := newTestServer(t, &Server{Registrar: reg})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}

	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Register{
		Register: &fav1.RegisterRobot{RobotId: "robot-1", AdapterVersion: "1.2.3"},
	}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	rreply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv register ack: %v", err)
	}
	if ack := rreply.GetRegisterAck(); ack == nil || !ack.GetAccepted() {
		t.Fatalf("register ack = %+v, want accepted", ack)
	}

	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Discover{
		Discover: &fav1.DiscoverRobot{RobotId: "robot-2", Manufacturer: "acme", Model: "m1"},
	}}); err != nil {
		t.Fatalf("send discover: %v", err)
	}
	dreply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv discover ack: %v", err)
	}
	if ack := dreply.GetDiscoverAck(); ack == nil || ack.GetDiscoveredRobotName() != "dr-robot-2" {
		t.Fatalf("discover ack = %+v", ack)
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.registers) != 1 || reg.registers[0].GetRobotId() != "robot-1" {
		t.Fatalf("registers = %+v", reg.registers)
	}
	if len(reg.discovers) != 1 || reg.discovers[0].GetRobotId() != "robot-2" {
		t.Fatalf("discovers = %+v", reg.discovers)
	}
	if len(reg.ids) == 0 || reg.ids[0].Namespace != "warehouse-east" || reg.ids[0].Vendor != "acme" {
		t.Fatalf("identity not propagated: %+v", reg.ids)
	}
}

func TestControlStream_RejectAllRegistrarByDefault(t *testing.T) {
	client, cleanup := newTestServer(t, &Server{}) // no Registrar configured
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Register{
		Register: &fav1.RegisterRobot{RobotId: "robot-1"},
	}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv register ack: %v", err)
	}
	if ack := reply.GetRegisterAck(); ack == nil || ack.GetAccepted() {
		t.Fatalf("register ack = %+v, want refused by default", ack)
	}
}

func TestControlStream_TelemetryIngested(t *testing.T) {
	ing := &fakeIngestor{}
	client, cleanup := newTestServer(t, &Server{Ingestor: ing})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{
		Telemetry: &fav1.TelemetryPayload{
			RobotId:     "robot-1",
			TimestampMs: 1_700_000_000_000,
			Phase:       fav1.RobotPhase_ROBOT_PHASE_IN_PROGRESS,
			Battery:     &fav1.BatteryStatus{Percent: i32(0)}, // critical, presence-trapped
		},
	}}); err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	frames := waitFrames(t, ing, 1)
	fr := frames[0]
	if fr.RobotID != "robot-1" {
		t.Errorf("robot id = %q", fr.RobotID)
	}
	if fr.BatteryPct == nil || *fr.BatteryPct != 0 {
		t.Errorf("battery presence lost through the stream: %v", fr.BatteryPct)
	}
	if fr.Phase == "" {
		t.Errorf("phase not translated")
	}
}

func TestControlStream_TelemetryWithoutIngestorIsDropped(t *testing.T) {
	// No Ingestor: telemetry must be accepted and dropped (RA-1: no status write
	// path exists), and the stream must stay healthy for subsequent messages.
	reg := &fakeRegistrar{}
	client, cleanup := newTestServer(t, &Server{Registrar: reg})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{
		Telemetry: &fav1.TelemetryPayload{RobotId: "robot-1"},
	}}); err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	// The stream is still usable: a following register still gets an ack.
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Register{
		Register: &fav1.RegisterRobot{RobotId: "robot-1"},
	}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("stream unhealthy after dropped telemetry: %v", err)
	}
}

// TestControlStream_ConcurrentStreams exercises the shared Server, Registrar, and
// Ingestor across many simultaneous adapter streams — meaningful under -race.
func TestControlStream_ConcurrentStreams(t *testing.T) {
	ing := &fakeIngestor{}
	client, cleanup := newTestServer(t, &Server{Registrar: &fakeRegistrar{}, Ingestor: ing})
	defer cleanup()

	const streams = 8
	const framesPerStream = 25

	var wg sync.WaitGroup
	for s := 0; s < streams; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := client.ControlStream(ctx)
			if err != nil {
				t.Errorf("stream %d open: %v", s, err)
				return
			}
			if err := stream.Send(hello()); err != nil {
				t.Errorf("stream %d hello: %v", s, err)
				return
			}
			if _, err := stream.Recv(); err != nil {
				t.Errorf("stream %d hello ack: %v", s, err)
				return
			}
			for f := 0; f < framesPerStream; f++ {
				if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{
					Telemetry: &fav1.TelemetryPayload{RobotId: "robot", TimestampMs: int64(f)},
				}}); err != nil {
					t.Errorf("stream %d telemetry %d: %v", s, f, err)
					return
				}
			}
			if err := stream.CloseSend(); err != nil {
				t.Errorf("stream %d close: %v", s, err)
				return
			}
			// Block until the server has drained the stream and returned (EOF),
			// so the deferred context cancel cannot abort in-flight telemetry.
			if _, err := stream.Recv(); err != nil && err != io.EOF {
				t.Errorf("stream %d drain: %v", s, err)
			}
		}(s)
	}
	wg.Wait()

	waitFrames(t, ing, streams*framesPerStream)
}

// waitFrames polls the ingestor until it has recorded at least n frames.
func waitFrames(t *testing.T, ing *fakeIngestor, n int) []telemetry.Frame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		frames := ing.snapshot()
		if len(frames) >= n {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d frames; got %d", n, len(frames))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
