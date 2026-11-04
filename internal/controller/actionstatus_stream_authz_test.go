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

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/streamauth"
	"github.com/swarmada/swarmada/internal/tde"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// An ActionStatusUpdate names an action, not a robot, but ending an action frees a robot:
// one reconcile of the terminal branch sets the robot Idle and releases its zone slot. So
// a report is authorized against the robot the action is bound to, and a stream that does
// not serve that robot cannot end the action (RFC-0001
// #security-fleet-adapter-authentication-robot-identity).
//
// These tests drive the real controlstream.Server, the real ingestor, the real reconciler
// and the real TDE, and assert on the consequence rather than the phase alone: while the
// action holds its slot a second robot is Denied the zone, and only a release lets it in.
// The fixture (newSupersessionFixture) binds live action "x" to robot "r1" at assignment
// generation 4 in a zone with room for one robot.

// streamAuthorizer allows a fixed set of robots to every stream, whatever its identity.
// The identity half of the check is streamauth's and is exercised with a real certificate
// below; this one isolates the wiring.
type streamAuthorizer struct{ allow map[string]bool }

func (a streamAuthorizer) AuthorizeRobot(_ context.Context, _ controlstream.TLSIdentity, robotID string) error {
	if a.allow[robotID] {
		return nil
	}
	return fmt.Errorf("robot %q is not served by this stream", robotID)
}

func (a streamAuthorizer) AuthorizeAnnounce(ctx context.Context, id controlstream.TLSIdentity, robotID string) error {
	return a.AuthorizeRobot(ctx, id, robotID)
}

// lockedAudit is an audit.Recorder safe to read while a stream goroutine writes to it.
type lockedAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (l *lockedAudit) Record(e audit.Entry) (audit.Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	return e, nil
}

func (l *lockedAudit) denied(eventType, resource string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.EventType == eventType && e.Outcome == audit.OutcomeDenied && e.Resource.Name == resource {
			return true
		}
	}
	return false
}

func (l *lockedAudit) reason(eventType, resource string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.EventType == eventType && e.Resource.Name == resource {
			return e.Detail["reason"]
		}
	}
	return ""
}

// sendOverStream opens a ControlStream on cl, sends hello declaring namespace, then each
// message, and drains to EOF so every message has been dispatched when it returns.
func sendOverStream(t *testing.T, cl fav1.FleetAdapterServiceClient, namespace string, msgs ...*fav1.AdapterMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := cl.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Hello{Hello: &fav1.AdapterHello{
		Vendor: "acme", AdapterVersion: "1.0.0", ProtocolVersion: controlstream.ProtocolVersion,
		Namespace: namespace,
	}}}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if !ack.GetHelloAck().GetAccepted() {
		t.Fatalf("hello refused: %s", ack.GetHelloAck().GetMessage())
	}
	for _, m := range msgs {
		if err := stream.Send(m); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	for {
		if _, err := stream.Recv(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("stream torn down: %v", err)
		}
	}
}

// bufconnClient serves srv over an in-memory listener with no TLS, so every stream has
// an empty identity and the Authorizer alone decides.
func bufconnClient(t *testing.T, srv *controlstream.Server) fav1.FleetAdapterServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fav1.RegisterFleetAdapterServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(); gs.Stop() })
	return fav1.NewFleetAdapterServiceClient(conn)
}

func actionStatusMsg(u *fav1.ActionStatusUpdate) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_ActionStatus{ActionStatus: u}}
}

// terminalReports are the three reports that end a live action on the control plane's
// books. The SUPERSEDED claim names "y", a live action bound to the same robot, so it
// passes the supersession check on its own: only the stream authorization can refuse it.
func terminalReports() map[string]*fav1.ActionStatusUpdate {
	return map[string]*fav1.ActionStatusUpdate{
		"succeeded": {ActionId: "x", State: fav1.ActionState_ACTION_STATE_SUCCEEDED},
		"failed":    {ActionId: "x", State: fav1.ActionState_ACTION_STATE_FAILED, Message: "forged"},
		"superseded": {ActionId: "x", State: fav1.ActionState_ACTION_STATE_CANCELLED,
			EndCause: fav1.ActionEndCause_ACTION_END_CAUSE_SUPERSEDED, SupersedingActionId: "y"},
	}
}

// assertStillLiveAndHeld is the consequence check: x live, r1 bound, x's zone slot held,
// and no FleetAction status written.
func assertStillLiveAndHeld(t *testing.T, f *supersessionFixture) {
	t.Helper()
	if n := f.statusWrites.Load(); n != 0 {
		t.Errorf("a refused report wrote FleetAction status %d time(s), want 0", n)
	}
	reconcileAction(t, f.r, "x")
	if x := getAction(t, f.c, "x"); x.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Errorf("x phase = %s, want InProgress: a stream that does not serve r1 ended its action", x.Status.Phase)
	}
	if r1 := getRobot(t, f.c, "r1", actionNS); r1.Status.AssignedAction != "x" {
		t.Errorf("r1 = {phase %s, assignedAction %q}, want still bound to x", r1.Status.Phase, r1.Status.AssignedAction)
	}
	if got := f.secondRobotReservation(t); got == tde.Granted {
		t.Errorf("a second robot was Granted x's zone: r1's slot was released while r1 may still be moving")
	}
}

func TestActionStatus_StreamNotServingTheRobotCannotEndTheAction(t *testing.T) {
	for name, report := range terminalReports() {
		t.Run(name, func(t *testing.T) {
			f := newSupersessionFixture(t, liveAction("y", actionNS, "r1"))
			if got := f.secondRobotReservation(t); got == tde.Granted {
				t.Fatalf("precondition: a second robot was granted the zone while x holds it")
			}
			streamAudit := &lockedAudit{}
			srv := &controlstream.Server{
				ActionStatus: f.ing,
				Authorizer:   streamAuthorizer{allow: map[string]bool{"r2": true}}, // not r1
				Audit:        streamAudit,
			}
			// No fencing token: the forger needs nothing but the action's name. The token
			// is optional and an absent one is read as current.
			sendOverStream(t, bufconnClient(t, srv), actionNS, actionStatusMsg(report))

			assertStillLiveAndHeld(t, f)
			if !streamAudit.denied(audit.EventRobotAuthzDenied, "r1") {
				t.Errorf("no Denied %s entry naming r1: the refusal must be recorded, not silent",
					audit.EventRobotAuthzDenied)
			}
		})
	}
}

// The control: the same report from a stream that DOES serve r1 ends x, and one reconcile
// frees r1 and its slot. Without this the test above could pass by the observable being
// dead rather than by the refusal.
func TestActionStatus_StreamServingTheRobotEndsTheActionAndFreesTheSlot(t *testing.T) {
	f := newSupersessionFixture(t)
	srv := &controlstream.Server{ActionStatus: f.ing, Authorizer: streamAuthorizer{allow: map[string]bool{"r1": true}}}
	token := uint64(getAction(t, f.c, "x").Status.AssignmentGeneration)
	sendOverStream(t, bufconnClient(t, srv), actionNS,
		actionStatusMsg(&fav1.ActionStatusUpdate{ActionId: "x", State: fav1.ActionState_ACTION_STATE_SUCCEEDED, FencingToken: &token}))

	if x := getAction(t, f.c, "x"); x.Status.Phase != fleetv1.ActionPhaseSucceeded {
		t.Fatalf("x phase = %s, want Succeeded from the robot's own adapter", x.Status.Phase)
	}
	reconcileAction(t, f.r, "x")
	if got := f.secondRobotReservation(t); got != tde.Granted {
		t.Errorf("second robot = %s after x ended, want Granted", got)
	}
}

// RA-1: a refusal writes no status, however often it arrives. A report stream at
// telemetry cadence, forged or fenced out, must not become a status write per tick. The
// API server does not bump resourceVersion for an identical patch, so the patch calls
// themselves are counted.
func TestActionStatus_RefusedReportsWriteNoStatus(t *testing.T) {
	f := newSupersessionFixture(t)
	stale := uint64(3) // x is at generation 4
	var msgs []*fav1.AdapterMessage
	for i := 0; i < 25; i++ {
		msgs = append(msgs,
			actionStatusMsg(&fav1.ActionStatusUpdate{ActionId: "x", State: fav1.ActionState_ACTION_STATE_RUNNING,
				ProgressPct: int32(i)}),
			actionStatusMsg(&fav1.ActionStatusUpdate{ActionId: "x", State: fav1.ActionState_ACTION_STATE_SUCCEEDED}))
	}
	forger := &controlstream.Server{ActionStatus: f.ing, Authorizer: streamAuthorizer{allow: map[string]bool{"r2": true}}}
	sendOverStream(t, bufconnClient(t, forger), actionNS, msgs...)

	var staleMsgs []*fav1.AdapterMessage
	for i := 0; i < 25; i++ {
		staleMsgs = append(staleMsgs,
			actionStatusMsg(&fav1.ActionStatusUpdate{ActionId: "x", State: fav1.ActionState_ACTION_STATE_RUNNING,
				FencingToken: &stale, ProgressPct: int32(i)}))
	}
	owner := &controlstream.Server{ActionStatus: f.ing, Authorizer: streamAuthorizer{allow: map[string]bool{"r1": true}}}
	sendOverStream(t, bufconnClient(t, owner), actionNS, staleMsgs...)

	if n := f.statusWrites.Load(); n != 0 {
		t.Fatalf("100 refused reports wrote FleetAction status %d time(s), want 0 (RA-1)", n)
	}
}

// A terminal report under a stale fencing token was dropped with no log line and no
// record. It is now refused visibly: a Warning Event on the action says so. The token is
// a staleness check, not a credential, so this is visibility, not authorization.
func TestActionStatus_StaleTerminalReportIsRefusedVisibly(t *testing.T) {
	f := newSupersessionFixture(t)
	stale := uint64(3)
	srv := &controlstream.Server{ActionStatus: f.ing, Authorizer: streamAuthorizer{allow: map[string]bool{"r1": true}}}
	sendOverStream(t, bufconnClient(t, srv), actionNS, actionStatusMsg(&fav1.ActionStatusUpdate{
		ActionId: "x", State: fav1.ActionState_ACTION_STATE_SUCCEEDED, FencingToken: &stale}))

	if x := getAction(t, f.c, "x"); x.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("x phase = %s, want InProgress: a stale report must not end the action", x.Status.Phase)
	}
	if n := f.statusWrites.Load(); n != 0 {
		t.Errorf("stale report wrote status %d time(s), want 0", n)
	}
	saw := false
	for len(f.events.Events) > 0 {
		if e := <-f.events.Events; strings.HasPrefix(e, "Warning ") && strings.Contains(e, "fencing token 3") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("no Warning Event naming the stale fencing token: the refusal is still silent")
	}
}

// ── A certificate the client CA verified, whose SAN is not an adapter SAN ─────────────
//
// TLS verifies the chain, but the identity parser finds no <adapter>.<namespace>.svc.
// cluster.local SAN and returns an empty identity, so the hello's declared namespace is
// used as declared. Every robot-scoped message on such a stream fails closed. This test is
// the ActionStatus path on that stream, measured with a real handshake and the real
// streamauth.Authorizer.

type stubCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newStubCA(t *testing.T) *stubCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
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
	return &stubCA{cert: cert, key: key, pool: pool}
}

func (ca *stubCA) leaf(t *testing.T, san string, eku x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: san},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku},
		DNSNames: []string{san},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("sign leaf: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// mtlsClient serves srv with the production client-auth mode and returns a client that
// presents clientCert.
func mtlsClient(t *testing.T, srv *controlstream.Server, ca *stubCA, clientCert tls.Certificate) fav1.FleetAdapterServiceClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{ca.leaf(t, "localhost", x509.ExtKeyUsageServerAuth)},
		ClientCAs:    ca.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controlstream.NewGRPCRunnable(addr, srv, logr.Discard(), serverTLS).Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: ca.pool, ServerName: "localhost", MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{clientCert},
	})))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fav1.NewFleetAdapterServiceClient(conn)
}

func TestActionStatus_VerifiedCertWithNoAdapterSANCannotEndActionsInADeclaredNamespace(t *testing.T) {
	for name, report := range terminalReports() {
		t.Run(name, func(t *testing.T) {
			f := newSupersessionFixture(t, liveAction("y", actionNS, "r1"))
			// r1 is a real, admitted robot of the fixture's adapter; the certificate below
			// has no claim to it or to its namespace.
			r1 := getRobot(t, f.c, "r1", actionNS)
			r1.Spec.Adapter = boundAdapter()
			if err := f.c.Update(context.Background(), r1); err != nil {
				t.Fatalf("bind r1: %v", err)
			}
			streamAudit := &lockedAudit{}
			srv := &controlstream.Server{
				ActionStatus: f.ing,
				Authorizer:   &streamauth.Authorizer{Client: f.c},
				Audit:        streamAudit,
			}
			ca := newStubCA(t)
			// Signed by the client CA, client-auth usage, but not an adapter SAN.
			cert := ca.leaf(t, "metrics-scraper.monitoring.svc", x509.ExtKeyUsageClientAuth)

			// The hello declares a namespace the certificate says nothing about, and is
			// accepted: with no parsed identity there is nothing to bind it to.
			sendOverStream(t, mtlsClient(t, srv, ca, cert), actionNS, actionStatusMsg(report))

			assertStillLiveAndHeld(t, f)
			if !streamAudit.denied(audit.EventRobotAuthzDenied, "r1") {
				t.Errorf("no Denied %s entry naming r1", audit.EventRobotAuthzDenied)
			}
			// The denial is the empty identity's: the chain verified, the SAN did not parse.
			if reason := streamAudit.reason(audit.EventRobotAuthzDenied, "r1"); !strings.Contains(reason, "unauthenticated") {
				t.Errorf("denial reason = %q, want the unverified-identity refusal", reason)
			}
		})
	}
}

// allowAnyRobot is the authorization of a stream that serves every robot: the ingestor
// tests that are about lifecycle and fencing, not about who reported, run under it.
func allowAnyRobot(context.Context, string) bool { return true }
