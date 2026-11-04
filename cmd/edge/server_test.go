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
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/edge"
	"github.com/swarmada/swarmada/internal/zone"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// startEdge serves an edge Node (with cfg) over mTLS on an ephemeral port; returns addr.
func startEdge(t *testing.T, tlsCfg *tls.Config, cfg edge.Config) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	node := edge.New(cfg, audit.New(&audit.MemorySink{}, "test"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan struct{})
	go func() {
		_ = serveEdgeOnListener(ctx, lis, tlsCfg, node, logr.Discard(), func() { close(ready) })
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not become ready")
	}
	return lis.Addr().String()
}

func clientCreds(t *testing.T, p *pki, certFile, keyFile string) credentials.TransportCredentials {
	t.Helper()
	caPEM, err := os.ReadFile(p.caFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	cfg := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(cfg)
}

func dial(t *testing.T, addr string, creds credentials.TransportCredentials) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// oneZoneConfig guards a small square zone with amr-1 assigned to it.
func oneZoneConfig() edge.Config {
	return edge.Config{
		Namespace: "warehouse-a",
		Zones: []edge.ZonePolygon{{Name: "zone-a", Floor: 0, Polygon: []zone.Point{
			{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}}},
		RobotZone: map[string]string{"amr-1": "zone-a"},
	}
}

func f64(v float64) *float64 { return &v }

func TestEdgeStreamAcceptsNamespaceSignedClientAndActs(t *testing.T) {
	p := newPKI(t)
	tlsCfg, err := serverTLSConfig(p.serverCert, p.serverKey, p.caFile)
	if err != nil {
		t.Fatal(err)
	}
	addr := startEdge(t, tlsCfg, oneZoneConfig())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dial(t, addr, clientCreds(t, p, p.clientCert, p.clientKey))
	stream, err := fav1.NewEdgeServiceClient(conn).EdgeStream(ctx)
	if err != nil {
		t.Fatalf("authorized client could not open EdgeStream: %v", err)
	}
	// A pose well outside zone-a is a confirmed breach → the node pushes an Estop back
	// down this stream. Receiving it proves the mTLS handshake, authorization, and the
	// full EdgeStream round trip all work.
	if err := stream.Send(&fav1.AdapterEdgeMessage{Msg: &fav1.AdapterEdgeMessage_Position{
		Position: &fav1.PositionFrame{RobotId: "amr-1", Position: &fav1.RobotPosition{X: f64(99), Y: f64(99)}}}}); err != nil {
		t.Fatalf("send position: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("expected an Estop from the edge node, got error: %v", err)
	}
	if msg.GetEstop() == nil {
		t.Fatalf("expected an Estop control message, got %T", msg.GetMsg())
	}
}

func TestEdgeStreamRejectsClientWithoutCert(t *testing.T) {
	p := newPKI(t)
	tlsCfg, err := serverTLSConfig(p.serverCert, p.serverKey, p.caFile)
	if err != nil {
		t.Fatal(err)
	}
	addr := startEdge(t, tlsCfg, edge.Config{Namespace: "warehouse-a"})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dial(t, addr, clientCreds(t, p, "", "")) // no client cert
	stream, err := fav1.NewEdgeServiceClient(conn).EdgeStream(ctx)
	if err == nil {
		_ = stream.Send(&fav1.AdapterEdgeMessage{})
		_, err = stream.Recv()
	}
	if err == nil {
		t.Fatal("expected the server to reject a client presenting no certificate")
	}
}

func TestEdgeStreamRejectsForeignCAClient(t *testing.T) {
	p := newPKI(t)
	tlsCfg, err := serverTLSConfig(p.serverCert, p.serverKey, p.caFile)
	if err != nil {
		t.Fatal(err)
	}
	addr := startEdge(t, tlsCfg, edge.Config{Namespace: "warehouse-a"})

	rogueCert, rogueKey := p.signWithForeignCA(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn := dial(t, addr, clientCreds(t, p, rogueCert, rogueKey))
	stream, err := fav1.NewEdgeServiceClient(conn).EdgeStream(ctx)
	if err == nil {
		_ = stream.Send(&fav1.AdapterEdgeMessage{})
		_, err = stream.Recv()
	}
	if err == nil {
		t.Fatal("expected the server to reject a client cert from an untrusted CA")
	}
}
