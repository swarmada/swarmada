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
	"strings"
	"testing"
	"time"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The namespace a ControlStream is served in is the one its verified client
// certificate binds. AdapterHello.namespace may repeat it or omit it; it may not
// name another. Without a verified certificate there is nothing to check it against.

func TestBindHelloNamespace(t *testing.T) {
	verified := TLSIdentity{AdapterName: "acme-adapter", Namespace: "warehouse-a", Verified: true}
	for _, tc := range []struct {
		name, declared string
		identity       TLSIdentity
		wantNS         string
		wantRefused    bool
	}{
		{"certificate, same namespace", "warehouse-a", verified, "warehouse-a", false},
		{"certificate, none declared", "", verified, "warehouse-a", false},
		{"certificate, other namespace", "warehouse-b", verified, "", true},
		{"no certificate, declared", "warehouse-b", TLSIdentity{}, "warehouse-b", false},
		{"no certificate, none declared", "", TLSIdentity{}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, refusal := bindHelloNamespace(tc.declared, tc.identity)
			if (refusal != "") != tc.wantRefused {
				t.Fatalf("refusal = %q, want refused=%v", refusal, tc.wantRefused)
			}
			if ns != tc.wantNS {
				t.Fatalf("namespace = %q, want %q", ns, tc.wantNS)
			}
		})
	}
}

// helloOverMTLS opens a ControlStream with goodSAN's certificate (namespace
// warehouse-a), sends a hello declaring the given namespace, and returns the ack.
func helloOverMTLS(t *testing.T, srv *Server, declared string) (fav1.FleetAdapterService_ControlStreamClient, *fav1.HelloAck) {
	t.Helper()
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	addr, stop := startMTLSRunnable(t, srv, mtlsServerConfig(server, ca.pool))
	t.Cleanup(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := tlsClient(t, addr, clientConfig(ca.pool, &client)).ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	h := hello()
	h.GetHello().Namespace = declared
	if err := stream.Send(h); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	return stream, reply.GetHelloAck()
}

// A certificate for warehouse-a and a hello declaring warehouse-b: refused at the
// handshake, with a message naming both, and nothing the adapter sends is ingested.
func TestControlStream_HelloNamespaceDifferentFromCertificateIsRefused(t *testing.T) {
	ing := &fakeIngestor{}
	stream, ack := helloOverMTLS(t, &Server{Ingestor: ing, Authorizer: &recordAuthorizer{failUnverified: true}}, "warehouse-b")
	if ack.GetAccepted() {
		t.Fatalf("hello declaring warehouse-b over a warehouse-a certificate was accepted: %+v", ack)
	}
	for _, want := range []string{`"warehouse-b"`, `"warehouse-a"`, "client certificate"} {
		if !strings.Contains(ack.GetMessage(), want) {
			t.Fatalf("refusal %q does not name %s", ack.GetMessage(), want)
		}
	}
	// The server closed the stream after refusing; nothing more is read from it.
	_ = stream.Send(telemetryMsg("robot-1"))
	if _, err := stream.Recv(); err == nil {
		t.Fatal("stream still open after a refused hello")
	}
	if n := len(ing.snapshot()); n != 0 {
		t.Fatalf("%d frames ingested from a refused stream", n)
	}
}

// A certificate for warehouse-a and a hello declaring no namespace: accepted, and
// its telemetry arrives carrying the certificate's namespace.
func TestControlStream_HelloWithoutNamespaceIsServedInTheCertificateNamespace(t *testing.T) {
	ing := &fakeIngestor{}
	stream, ack := helloOverMTLS(t, &Server{Ingestor: ing, Authorizer: &recordAuthorizer{failUnverified: true}}, "")
	if !ack.GetAccepted() {
		t.Fatalf("hello with no namespace over a valid certificate refused: %s", ack.GetMessage())
	}
	if err := stream.Send(telemetryMsg("robot-1")); err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	_ = stream.CloseSend()
	if fr := waitFrames(t, ing, 1)[0]; fr.Namespace != "warehouse-a" {
		t.Fatalf("frame namespace = %q, want warehouse-a (the certificate's)", fr.Namespace)
	}
}

// Telemetry reaches the ingestor with no namespace only when the stream has no
// verified certificate AND per-robot authorization is disabled AND the hello
// declared none. With an Authorizer and no certificate the frame is denied before
// ingestion; with a certificate it carries the certificate's namespace.
func TestControlStream_AFrameWithNoNamespaceNeedsNoCertificateAndNoAuthorizer(t *testing.T) {
	send := func(t *testing.T, srv *Server) {
		t.Helper()
		client, cleanup := newTestServer(t, srv) // plaintext: no client certificate
		t.Cleanup(cleanup)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		t.Cleanup(cancel)
		stream, err := client.ControlStream(ctx)
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		h := hello()
		h.GetHello().Namespace = ""
		if err := stream.Send(h); err != nil {
			t.Fatalf("send hello: %v", err)
		}
		if reply, err := stream.Recv(); err != nil || !reply.GetHelloAck().GetAccepted() {
			t.Fatalf("hello: %v %+v", err, reply)
		}
		if err := stream.Send(telemetryMsg("robot-1")); err != nil {
			t.Fatalf("send telemetry: %v", err)
		}
		_ = stream.CloseSend()
	}

	t.Run("no certificate, authorization on: denied before ingestion", func(t *testing.T) {
		ing, authz := &fakeIngestor{}, &recordAuthorizer{failUnverified: true}
		send(t, &Server{Ingestor: ing, Authorizer: authz})
		eventually(t, func() bool { return authz.count() > 0 })
		time.Sleep(50 * time.Millisecond)
		if n := len(ing.snapshot()); n != 0 {
			t.Fatalf("%d frames ingested from an unverified stream with authorization on", n)
		}
	})
	t.Run("no certificate, authorization off: the one path with no namespace", func(t *testing.T) {
		ing := &fakeIngestor{}
		send(t, &Server{Ingestor: ing})
		if fr := waitFrames(t, ing, 1)[0]; fr.Namespace != "" {
			t.Fatalf("frame namespace = %q, want empty", fr.Namespace)
		}
	})
}
