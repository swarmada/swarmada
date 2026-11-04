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

// The control plane sends its response headers when it accepts a SafetyStream, and only
// then. A gRPC client's call to SafetyStream returns before the server has accepted
// anything, so the headers are the first thing an adapter can observe that says the
// stream is served: an estop pushed from that moment reaches it (RFC-0001
// #safety-emergency-stop-protocol). A stream the control plane does not serve sends none.

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// registeringSafetyDispatcher keeps the sender each stream registered, so a test can push
// an estop down it and can tell whether the stream was registered.
type registeringSafetyDispatcher struct {
	mu      sync.Mutex
	senders []SafetySender
}

func (d *registeringSafetyDispatcher) RegisterStream(_ TLSIdentity, s SafetySender) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.senders = append(d.senders, s)
	return func() {}
}

func (d *registeringSafetyDispatcher) RouteClearAck(TLSIdentity, SafetySender, string, *fav1.EstopClearAck) error {
	return nil
}

func (d *registeringSafetyDispatcher) RouteAck(TLSIdentity, SafetySender, string, *fav1.EstopAck) error {
	return nil
}

func (d *registeringSafetyDispatcher) registered() []SafetySender {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]SafetySender(nil), d.senders...)
}

type headerResult struct {
	md  metadata.MD
	err error
}

// awaitHeader runs stream.Header on its own goroutine, since it blocks until the server
// sends headers or the stream ends.
func awaitHeader(stream fav1.FleetAdapterService_SafetyStreamClient) <-chan headerResult {
	ch := make(chan headerResult, 1)
	go func() {
		md, err := stream.Header()
		ch <- headerResult{md, err}
	}()
	return ch
}

// An accepted SafetyStream sends its headers at once, before any message, and only once
// the stream is registered for estops: an estop pushed after the headers arrive reaches
// the adapter as the first message on the stream.
func TestSafetyStream_AcceptedStreamSendsHeadersBeforeAnyMessage(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	disp := &registeringSafetyDispatcher{}
	addr, stop := startMTLSRunnable(t, &Server{Safety: disp}, mtlsServerConfig(server, ca.pool))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := tlsClient(t, addr, clientConfig(ca.pool, &client)).SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open safety stream: %v", err)
	}

	// Nothing is pushed, so the only thing that can end this wait is the headers.
	var got headerResult
	select {
	case got = <-awaitHeader(stream):
	case <-time.After(5 * time.Second):
		t.Fatal("no headers 5s after the SafetyStream was accepted; an adapter cannot tell the stream is served")
	}
	if got.err != nil || got.md == nil {
		t.Fatalf("Header() = (%v, %v); want the headers of a live stream (the stream ended without any)", got.md, got.err)
	}
	senders := disp.registered()
	if len(senders) != 1 {
		t.Fatalf("headers arrived with %d streams registered for estops, want 1: headers must mean an estop can reach the adapter", len(senders))
	}

	if err := senders[0].Send(&fav1.ControlPlaneSafetyMessage{Payload: &fav1.ControlPlaneSafetyMessage_Estop{
		Estop: &fav1.Estop{EstopId: "e-1"},
	}}); err != nil {
		t.Fatalf("push estop: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv estop: %v", err)
	}
	if msg.GetEstop().GetEstopId() != "e-1" || msg.GetSeq() != 1 {
		t.Fatalf("first message = %v; want estop e-1 at seq 1", msg)
	}
	_ = stream.CloseSend()
}

// resendingSafetyDispatcher pushes an estop down each stream while registering it, as
// internal/safety.Dispatcher re-sends an estop still pending on the stream a new one replaces.
type resendingSafetyDispatcher struct {
	registeringSafetyDispatcher
}

func (d *resendingSafetyDispatcher) RegisterStream(id TLSIdentity, s SafetySender) func() {
	_ = s.Send(&fav1.ControlPlaneSafetyMessage{Payload: &fav1.ControlPlaneSafetyMessage_Estop{
		Estop: &fav1.Estop{EstopId: "e-resent"},
	}})
	return d.registeringSafetyDispatcher.RegisterStream(id, s)
}

// An estop sent down the stream while it registers carries the headers itself. The stream
// stays up: the headers are not sent a second time, and the failure gRPC returns for that
// does not end it.
func TestSafetyStream_AnEstopSentWhileRegisteringKeepsTheStream(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	disp := &resendingSafetyDispatcher{}
	addr, stop := startMTLSRunnable(t, &Server{Safety: disp}, mtlsServerConfig(server, ca.pool))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := tlsClient(t, addr, clientConfig(ca.pool, &client)).SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open safety stream: %v", err)
	}
	if md, err := stream.Header(); err != nil || md == nil {
		t.Fatalf("Header() = (%v, %v); want the headers of a live stream", md, err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv the re-sent estop: %v", err)
	}
	if msg.GetEstop().GetEstopId() != "e-resent" || msg.GetSeq() != 1 {
		t.Fatalf("first message = %v; want estop e-resent at seq 1", msg)
	}

	// The stream is still served: a second estop reaches the adapter.
	senders := disp.registered()
	if len(senders) != 1 {
		t.Fatalf("%d streams registered, want 1", len(senders))
	}
	if err := senders[0].Send(&fav1.ControlPlaneSafetyMessage{Payload: &fav1.ControlPlaneSafetyMessage_Estop{
		Estop: &fav1.Estop{EstopId: "e-2"},
	}}); err != nil {
		t.Fatalf("push a second estop: the stream ended after its registration: %v", err)
	}
	msg, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv the second estop: the stream ended after its registration: %v", err)
	}
	if msg.GetEstop().GetEstopId() != "e-2" {
		t.Fatalf("second message = %v; want estop e-2", msg)
	}
	_ = stream.CloseSend()
}

// A SafetyStream the control plane refuses sends no headers: the call ends with its
// status alone, so an adapter waiting for headers never takes it for a served stream.
func TestSafetyStream_RefusedStreamSendsNoHeaders(t *testing.T) {
	t.Run("no safety dispatcher configured", func(t *testing.T) {
		client, cleanup := newTestServer(t, &Server{})
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := client.SafetyStream(ctx)
		if err != nil {
			t.Fatalf("open safety stream: %v", err)
		}
		if md, _ := stream.Header(); md != nil {
			t.Fatalf("a SafetyStream refused for configuration sent headers %v", md)
		}
		if _, err := stream.Recv(); status.Code(err) != codes.Unavailable {
			t.Fatalf("recv = %v; want Unavailable", err)
		}
	})

	t.Run("client certificate from an untrusted CA", func(t *testing.T) {
		ca := newTestCA(t)
		otherCA := newTestCA(t)
		server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
		client := otherCA.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
		disp := &registeringSafetyDispatcher{}
		addr, stop := startMTLSRunnable(t, &Server{Safety: disp}, mtlsServerConfig(server, ca.pool))
		defer stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := tlsClient(t, addr, clientConfig(ca.pool, &client)).SafetyStream(ctx)
		if err != nil {
			return // refused before a stream existed
		}
		if md, _ := stream.Header(); md != nil {
			t.Fatalf("a SafetyStream refused at the handshake sent headers %v", md)
		}
		if _, err := stream.Recv(); err == nil {
			t.Fatal("a SafetyStream from an untrusted certificate was served")
		}
		if n := len(disp.registered()); n != 0 {
			t.Fatalf("%d streams registered for estops from an untrusted certificate, want 0", n)
		}
	})

	// A trusted certificate whose SAN names no adapter passes the handshake, but its
	// identity is unverified and the safety dispatcher never registers its stream, so no
	// estop can reach it. The stream stays open, and its headers would claim a path that
	// does not exist. Closing the stream from the client side ends it with OK; had the
	// server sent headers before that, Header() would return them.
	t.Run("trusted certificate with an unverified identity", func(t *testing.T) {
		ca := newTestCA(t)
		server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
		badSAN := ca.issue(t, []string{"acme-adapter.warehouse-a.example.com"}, time.Now().Add(time.Hour), true)
		addr, stop := startMTLSRunnable(t, &Server{Safety: &registeringSafetyDispatcher{}}, mtlsServerConfig(server, ca.pool))
		defer stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := tlsClient(t, addr, clientConfig(ca.pool, &badSAN)).SafetyStream(ctx)
		if err != nil {
			t.Fatalf("open safety stream: %v", err)
		}
		// Let the server reach the point where an accepted stream sends its headers.
		time.Sleep(300 * time.Millisecond)
		if err := stream.CloseSend(); err != nil {
			t.Fatalf("close send: %v", err)
		}
		if md, _ := stream.Header(); md != nil {
			t.Fatalf("a SafetyStream under an unverified identity sent headers %v", md)
		}
	})
}
