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

// mTLS enforcement for the ControlStream listener (RFC-0001 §9.2.7, ADR-0025).
// These tests run a real GRPCRunnable with a server tls.Config over a loopback
// port and dial it with real client certificates, so they exercise the actual TLS
// handshake — not the fabricated VerifiedChains of identity_test.go.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// --- certificate authority + leaf issuance -------------------------------------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// issue signs a leaf with the given SANs and validity. clientAuth selects the
// extended key usage (client vs server).
func (ca *testCA) issue(t *testing.T, sans []string, notAfter time.Time, clientAuth bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	eku := x509.ExtKeyUsageServerAuth
	if clientAuth {
		eku = x509.ExtKeyUsageClientAuth
	}
	cn := "leaf"
	if len(sans) > 0 {
		cn = sans[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		DNSNames:     sans,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("sign leaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func mtlsServerConfig(serverCert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

func clientConfig(rootCAs *x509.CertPool, clientCert *tls.Certificate) *tls.Config {
	cfg := &tls.Config{RootCAs: rootCAs, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	return cfg
}

// --- harness -------------------------------------------------------------------

func startMTLSRunnable(t *testing.T, srv *Server, serverTLS *tls.Config) (string, func()) {
	t.Helper()
	addr := freeAddr(t) // from runnable_test.go
	r := NewGRPCRunnable(addr, srv, logr.Discard(), serverTLS)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	waitTCP(t, addr)
	return addr, func() { cancel(); <-done }
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func tlsClient(t *testing.T, addr string, clientTLS *tls.Config) fav1.FleetAdapterServiceClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fav1.NewFleetAdapterServiceClient(conn)
}

// openHello opens a ControlStream, sends AdapterHello, and consumes the HelloAck.
// It fails the test if the handshake or hello is rejected (used by the accept path).
func openHello(t *testing.T, cl fav1.FleetAdapterServiceClient) fav1.FleetAdapterService_ControlStreamClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := cl.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(hello()); err != nil { // hello() from server_test.go
		t.Fatalf("send hello: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	return stream
}

func telemetryMsg(robotID string) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{
		Telemetry: &fav1.TelemetryPayload{RobotId: robotID, TimestampMs: 1},
	}}
}

// assertHandshakeRejected asserts the mTLS handshake fails, so no stream is usable.
func assertHandshakeRejected(t *testing.T, cl fav1.FleetAdapterServiceClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := cl.ControlStream(ctx)
	if err != nil {
		return // handshake already failed
	}
	if err := stream.Send(hello()); err != nil {
		return
	}
	if _, err := stream.Recv(); err != nil {
		return // handshake failure surfaces on first recv
	}
	t.Fatal("expected the mTLS handshake to be rejected, but a stream established")
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// --- test doubles --------------------------------------------------------------

type recordingPresence struct {
	mu        sync.Mutex
	connected []TLSIdentity
}

func (p *recordingPresence) AdapterConnected(_ context.Context, id TLSIdentity, _ Negotiation) {
	p.mu.Lock()
	p.connected = append(p.connected, id)
	p.mu.Unlock()
}
func (p *recordingPresence) AdapterHeartbeat(context.Context, TLSIdentity)    {}
func (p *recordingPresence) AdapterDisconnected(context.Context, TLSIdentity) {}
func (p *recordingPresence) snapshot() []TLSIdentity {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]TLSIdentity(nil), p.connected...)
}

// recordAuthorizer records the identity of every per-robot decision and, when
// failUnverified is set, fail-closes on an unverified identity (as the production
// streamauth.Authorizer does).
type recordAuthorizer struct {
	mu             sync.Mutex
	calls          []TLSIdentity
	failUnverified bool
}

func (a *recordAuthorizer) AuthorizeRobot(_ context.Context, id TLSIdentity, _ string) error {
	a.mu.Lock()
	a.calls = append(a.calls, id)
	a.mu.Unlock()
	if a.failUnverified && !id.Verified {
		return fmt.Errorf("unverified adapter identity (fail closed)")
	}
	return nil
}
func (a *recordAuthorizer) AuthorizeAnnounce(_ context.Context, id TLSIdentity, _ string) error {
	if a.failUnverified && !id.Verified {
		return fmt.Errorf("unverified adapter identity (fail closed)")
	}
	return nil
}
func (a *recordAuthorizer) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}
func (a *recordAuthorizer) last() TLSIdentity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[len(a.calls)-1]
}

const goodSAN = "acme-adapter.warehouse-a.svc.cluster.local"

// --- tests ---------------------------------------------------------------------

// A verified client cert with a well-formed SAN establishes the stream, yields the
// expected verified identity, drives presence (AdapterConnected), and its
// robot-scoped telemetry is authorized and ingested.
func TestControlStreamMTLS_VerifiedClientCert(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := ca.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)

	pres := &recordingPresence{}
	ing := &fakeIngestor{} // from server_test.go
	authz := &recordAuthorizer{failUnverified: true}
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: ing, Presence: pres, Authorizer: authz},
		mtlsServerConfig(server, ca.pool))
	defer stop()

	stream := openHello(t, tlsClient(t, addr, clientConfig(ca.pool, &client)))
	if err := stream.Send(telemetryMsg("robot-1")); err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	_ = stream.CloseSend() // half-close so the server stream ends and shutdown is prompt
	eventually(t, func() bool { return len(ing.snapshot()) == 1 && len(pres.snapshot()) == 1 })

	id := pres.snapshot()[0]
	if id.AdapterName != "acme-adapter" || id.Namespace != "warehouse-a" || !id.Verified {
		t.Fatalf("presence identity = %+v; want {acme-adapter warehouse-a true}", id)
	}
}

// No client certificate: the server requires and verifies one, so the handshake is
// rejected and no stream is usable.
func TestControlStreamMTLS_NoClientCert_Rejected(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: &fakeIngestor{}}, mtlsServerConfig(server, ca.pool))
	defer stop()
	assertHandshakeRejected(t, tlsClient(t, addr, clientConfig(ca.pool, nil)))
}

// A client cert signed by a CA the server does not trust is rejected at the
// handshake.
func TestControlStreamMTLS_UntrustedCA_Rejected(t *testing.T) {
	ca := newTestCA(t)
	otherCA := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	client := otherCA.issue(t, []string{goodSAN}, time.Now().Add(time.Hour), true)
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: &fakeIngestor{}}, mtlsServerConfig(server, ca.pool))
	defer stop()
	assertHandshakeRejected(t, tlsClient(t, addr, clientConfig(ca.pool, &client)))
}

// An expired client cert is rejected at the handshake.
func TestControlStreamMTLS_ExpiredClientCert_Rejected(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	expired := ca.issue(t, []string{goodSAN}, time.Now().Add(-time.Minute), true)
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: &fakeIngestor{}}, mtlsServerConfig(server, ca.pool))
	defer stop()
	assertHandshakeRejected(t, tlsClient(t, addr, clientConfig(ca.pool, &expired)))
}

// A trusted client cert whose SAN is NOT the <adapter>.<namespace>.svc.cluster.local
// form: the TLS handshake succeeds (the cert is trusted), but the identity is
// unverified, so presence never fires and per-robot messages fail closed.
func TestControlStreamMTLS_TrustedCertBadSAN_FailsClosed(t *testing.T) {
	ca := newTestCA(t)
	server := ca.issue(t, []string{"localhost"}, time.Now().Add(time.Hour), false)
	// Trusted, but a bad suffix -> parseAdapterSAN fails -> unverified identity.
	badSAN := ca.issue(t, []string{"acme-adapter.warehouse-a.example.com"}, time.Now().Add(time.Hour), true)

	pres := &recordingPresence{}
	ing := &fakeIngestor{}
	authz := &recordAuthorizer{failUnverified: true}
	addr, stop := startMTLSRunnable(t, &Server{Ingestor: ing, Presence: pres, Authorizer: authz},
		mtlsServerConfig(server, ca.pool))
	defer stop()

	// Handshake + hello succeed (openHello would fail the test otherwise).
	stream := openHello(t, tlsClient(t, addr, clientConfig(ca.pool, &badSAN)))
	if err := stream.Send(telemetryMsg("robot-1")); err != nil {
		t.Fatalf("send telemetry: %v", err)
	}
	_ = stream.CloseSend() // half-close so the server stream ends and shutdown is prompt
	// The per-robot authorizer is consulted during telemetry dispatch, after the
	// presence decision — so once it is called, the presence outcome is settled.
	eventually(t, func() bool { return authz.count() > 0 })

	if got := authz.last(); got.Verified || got.AdapterName != "" {
		t.Fatalf("authorizer saw identity %+v; want an unverified (empty) identity", got)
	}
	if n := len(ing.snapshot()); n != 0 {
		t.Fatalf("telemetry from an unverified identity must be denied (fail closed); ingested %d frames", n)
	}
	if n := len(pres.snapshot()); n != 0 {
		t.Fatalf("AdapterConnected must not fire for an unverified identity; got %d", n)
	}
}
