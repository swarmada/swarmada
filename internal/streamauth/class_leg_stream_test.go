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

package streamauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/telemetry"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The class is a condition of the admission gate, and the gate MUST NOT block observation or
// stopping (RFC-0001 #fleetadapter-admission-gating). So once spec.servesRobotClasses no longer
// lists a robot's class, the robot's own adapter is still heard: its telemetry, a heartbeat
// naming the robot, an EstopAck and a LocalStopReport are taken. Every arm that is work is
// still refused (#security-fleet-adapter-authentication-robot-identity).
//
// These tests run the real controlstream.Server with this package's Authorizer over mTLS, so
// the identity is the client certificate's and the class is read from the FleetAdapter. Adapter
// "acme" presents the certificate. Robot amr-1 (class "amr") is bound to it; amr-x (class
// "amr") is bound to adapter "other", which serves "amr". Each test sets acme's list.

// classLegRecorder records what reached the consumers behind the stream gate.
type classLegRecorder struct {
	mu         sync.Mutex
	frames     []string // robot ids of ingested telemetry
	heartbeats []string // robot ids of routed per-robot heartbeats
	acks       []string // estop ids of routed EstopAcks
	clears     []string // estop ids of routed EstopClearAcks
	registers  int
	results    int
	reserves   int
	caps       int
	progress   int
	actionAuth []bool // what the stream's authorization said for amr-1, per ActionStatusUpdate
}

// classLegSeen is a copy of what the recorder saw, taken under its lock.
type classLegSeen struct {
	frames, heartbeats, acks, clears             []string
	registers, results, reserves, caps, progress int
	actionAuth                                   []bool
}

func (r *classLegRecorder) seen() classLegSeen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return classLegSeen{
		frames: append([]string(nil), r.frames...), heartbeats: append([]string(nil), r.heartbeats...),
		acks: append([]string(nil), r.acks...), clears: append([]string(nil), r.clears...),
		registers: r.registers, results: r.results, reserves: r.reserves, caps: r.caps, progress: r.progress,
		actionAuth: append([]bool(nil), r.actionAuth...),
	}
}

func (r *classLegRecorder) Ingest(_ context.Context, f telemetry.Frame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, f.RobotID)
	return nil
}

func (r *classLegRecorder) Register(context.Context, controlstream.AdapterIdentity, *fav1.RegisterRobot) *fav1.RegisterAck {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registers++
	return &fav1.RegisterAck{Accepted: true}
}

func (r *classLegRecorder) Discover(context.Context, controlstream.AdapterIdentity, controlstream.TLSIdentity,
	*fav1.DiscoverRobot) *fav1.DiscoverAck {
	return &fav1.DiscoverAck{Accepted: false}
}

func (r *classLegRecorder) IngestActionStatus(ctx context.Context, _ string, authorize controlstream.RobotAuthorization,
	_ *fav1.ActionStatusUpdate) error {
	ok := authorize(ctx, "amr-1")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actionAuth = append(r.actionAuth, ok)
	return nil
}

func (r *classLegRecorder) IngestUpdateProgress(context.Context, string, *fav1.UpdateProgress) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress++
	return nil
}

func (r *classLegRecorder) IngestCapabilities(context.Context, string, *fav1.CapabilitiesSnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caps++
	return nil
}

func (r *classLegRecorder) ReserveResource(context.Context, string, string, string) controlstream.ResourceDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reserves++
	return controlstream.ResourceDecision{State: controlstream.ResourceGranted}
}

func (r *classLegRecorder) ReleaseResource(context.Context, string, string, string) controlstream.ResourceDecision {
	return controlstream.ResourceDecision{}
}

// classLegCommands is the CommandDispatcher (and HeartbeatRouter) behind the stream.
type classLegCommands struct{ r *classLegRecorder }

func (c classLegCommands) RegisterStream(controlstream.TLSIdentity, controlstream.CommandSender) func() {
	return func() {}
}

func (c classLegCommands) RouteResult(controlstream.CommandSender, *fav1.CommandResult) error {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.results++
	return nil
}

func (c classLegCommands) RouteHeartbeat(_, robotID string) {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.heartbeats = append(c.r.heartbeats, robotID)
}

// classLegSafety is the SafetyDispatcher behind the stream.
type classLegSafety struct{ r *classLegRecorder }

func (s classLegSafety) RegisterStream(controlstream.TLSIdentity, controlstream.SafetySender) func() {
	return func() {}
}

func (s classLegSafety) RouteAck(_ controlstream.TLSIdentity, _ controlstream.SafetySender, _ string, ack *fav1.EstopAck) error {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	s.r.acks = append(s.r.acks, ack.GetEstopId())
	return nil
}

func (s classLegSafety) RouteClearAck(_ controlstream.TLSIdentity, _ controlstream.SafetySender, _ string,
	ack *fav1.EstopClearAck) error {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	s.r.clears = append(s.r.clears, ack.GetEstopId())
	return nil
}

type classLegFixture struct {
	rec  *classLegRecorder
	sink *audit.MemorySink
	cli  fav1.FleetAdapterServiceClient // presents acme's certificate
}

// newClassLegFixture serves acme with served as its spec.servesRobotClasses. With
// failAdapterRead, every read of a FleetAdapter fails as an API outage would.
func newClassLegFixture(t *testing.T, served []string, failAdapterRead bool) *classLegFixture {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(classLegScheme(t)).WithObjects(
		robot("amr-1", "acme", "amr"), robot("amr-x", "other", "amr"),
		adapter("acme", served...), adapter("other", "amr"))
	if failAdapterRead {
		b = b.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*fleetv1.FleetAdapter); ok {
					return errors.New("simulated API outage")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	}
	rec := &classLegRecorder{}
	sink := &audit.MemorySink{}
	srv := &controlstream.Server{
		Registrar:      rec,
		Ingestor:       rec,
		ActionStatus:   rec,
		UpdateProgress: rec,
		Capabilities:   rec,
		Reservations:   rec,
		Commands:       classLegCommands{rec},
		Safety:         classLegSafety{rec},
		Authorizer:     &Authorizer{Client: b.Build()},
		Audit:          audit.New(sink, "test"),
	}
	return &classLegFixture{rec: rec, sink: sink, cli: serveClassLeg(t, srv)}
}

func classLegScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

// serveClassLeg serves srv over mTLS on a loopback port and returns a client presenting acme's
// certificate.
func serveClassLeg(t *testing.T, srv *controlstream.Server) fav1.FleetAdapterServiceClient {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "namespace-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	leaf := func(san string, eku x509.ExtKeyUsage) tls.Certificate {
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
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("sign leaf: %v", err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{leaf("localhost", x509.ExtKeyUsageServerAuth)},
		ClientCAs:    pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controlstream.NewGRPCRunnable(addr, srv, logr.Discard(), serverTLS).Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{leaf("acme."+ns+".svc.cluster.local", x509.ExtKeyUsageClientAuth)},
	})))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fav1.NewFleetAdapterServiceClient(conn)
}

// control sends msgs on a ControlStream after the handshake, closes it, and returns every
// reply after the HelloAck. It returns once the server has handled every message.
func (f *classLegFixture) control(t *testing.T, msgs ...*fav1.AdapterMessage) []*fav1.ControlPlaneMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := f.cli.ControlStream(ctx)
	if err != nil {
		t.Fatalf("open control stream: %v", err)
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Hello{Hello: &fav1.AdapterHello{
		Vendor: "acme", AdapterVersion: "1.0.0", ProtocolVersion: controlstream.ProtocolVersion,
		ContractVersion: contract.Version, Namespace: ns,
	}}}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	if !ack.GetHelloAck().GetAccepted() {
		t.Fatalf("hello refused: %v", ack)
	}
	for _, m := range msgs {
		if err := stream.Send(m); err != nil {
			t.Fatalf("send %T: %v", m.GetPayload(), err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	var replies []*fav1.ControlPlaneMessage
	for {
		m, err := stream.Recv()
		if err == io.EOF {
			return replies
		}
		if err != nil {
			t.Fatalf("control stream torn down: %v", err)
		}
		replies = append(replies, m)
	}
}

// safety sends msgs on a SafetyStream, closes it, and returns once the server has handled them.
func (f *classLegFixture) safety(t *testing.T, msgs ...*fav1.AdapterSafetyMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := f.cli.SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open safety stream: %v", err)
	}
	for _, m := range msgs {
		if err := stream.Send(m); err != nil {
			t.Fatalf("send %T: %v", m.GetPayload(), err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	for {
		if _, err := stream.Recv(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("safety stream torn down: %v", err)
		}
	}
}

// denials returns the ROBOT_AUTHZ_DENIED reasons sealed for robotID.
func (f *classLegFixture) denials(robotID string) []string {
	var out []string
	for _, e := range f.sink.Entries() {
		if e.EventType == audit.EventRobotAuthzDenied && e.Resource.Name == robotID {
			out = append(out, e.Detail["reason"])
		}
	}
	return out
}

func clTelemetry(robotID string) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Telemetry{Telemetry: &fav1.TelemetryPayload{
		RobotId: robotID, TimestampMs: 1_700_000_000_000, Phase: fav1.RobotPhase_ROBOT_PHASE_IDLE,
	}}}
}

func clHeartbeat(robotID string) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Heartbeat{Heartbeat: &fav1.HeartbeatResponse{RobotId: robotID}}}
}

func clEstopAck(robotID, estopID string) *fav1.AdapterSafetyMessage {
	return &fav1.AdapterSafetyMessage{RobotId: robotID, Payload: &fav1.AdapterSafetyMessage_EstopAck{
		EstopAck: &fav1.EstopAck{EstopId: estopID, State: fav1.EstopState_ESTOP_STATE_STOPPED},
	}}
}

func clLocalStop(robotID string) *fav1.AdapterSafetyMessage {
	return &fav1.AdapterSafetyMessage{RobotId: robotID, Payload: &fav1.AdapterSafetyMessage_LocalStop{
		LocalStop: &fav1.LocalStopReport{Latched: true, Source: "robot"},
	}}
}

func clRegister(robotID string) *fav1.AdapterMessage {
	return &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Register{Register: &fav1.RegisterRobot{RobotId: robotID}}}
}

// The robot's class has left acme's list, and acme's telemetry for it is still ingested, with
// no denial sealed.
func TestClassLeft_TelemetryIsStillIngested(t *testing.T) {
	f := newClassLegFixture(t, []string{"forklift"}, false)
	f.control(t, clTelemetry("amr-1"))
	if got := f.rec.seen().frames; len(got) != 1 || got[0] != "amr-1" {
		t.Errorf("ingested %v, want amr-1's frame: observation of a robot outside the class list is blocked", got)
	}
	if d := f.denials("amr-1"); len(d) != 0 {
		t.Errorf("telemetry sealed as denied: %v", d)
	}
}

// A heartbeat naming the robot still answers its per-robot confirming exchange.
func TestClassLeft_RobotHeartbeatStillReachesTheConfirmingExchange(t *testing.T) {
	f := newClassLegFixture(t, []string{"forklift"}, false)
	f.control(t, clHeartbeat("amr-1"))
	if got := f.rec.seen().heartbeats; len(got) != 1 || got[0] != "amr-1" {
		t.Errorf("routed heartbeats %v, want amr-1's", got)
	}
}

// An EstopAck from the robot's own adapter still reaches the dispatcher, so the stop can be
// confirmed.
func TestClassLeft_EstopAckIsStillRouted(t *testing.T) {
	f := newClassLegFixture(t, []string{"forklift"}, false)
	f.safety(t, clEstopAck("amr-1", "e-1"))
	if got := f.rec.seen().acks; len(got) != 1 || got[0] != "e-1" {
		t.Errorf("routed acks %v, want e-1: a stop sent to the robot could never be confirmed", got)
	}
	if d := f.denials("amr-1"); len(d) != 0 {
		t.Errorf("EstopAck sealed as denied: %v", d)
	}
}

// A LocalStopReport is a report about stopping, and is not sealed as a denial.
func TestClassLeft_LocalStopReportIsNotADenial(t *testing.T) {
	f := newClassLegFixture(t, []string{"forklift"}, false)
	f.safety(t, clLocalStop("amr-1"))
	if d := f.denials("amr-1"); len(d) != 0 {
		t.Errorf("LocalStopReport sealed as denied: %v", d)
	}
}

// Every arm that is work is still refused on the class, and the refusal is sealed. An
// EstopClearAck is one: a clear begins the robot's return to work.
func TestClassLeft_WorkIsStillRefused(t *testing.T) {
	work := []struct {
		name    string
		control *fav1.AdapterMessage
		safety  *fav1.AdapterSafetyMessage
		reached func(r classLegSeen) bool
	}{
		{name: "register", control: clRegister("amr-1"),
			reached: func(r classLegSeen) bool { return r.registers != 0 }},
		{name: "command_result", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_CommandResult{
			CommandResult: &fav1.CommandResult{CommandId: 1, RobotId: "amr-1"}}},
			reached: func(r classLegSeen) bool { return r.results != 0 }},
		{name: "resource_request", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_ResourceRequest{
			ResourceRequest: &fav1.ResourceRequest{RequestId: 1, RobotId: "amr-1", Request: &fav1.ResourceRequest_Reserve{
				Reserve: &fav1.ReserveResource{ResourceName: "door-1"}}}}},
			reached: func(r classLegSeen) bool { return r.reserves != 0 }},
		{name: "capabilities", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Capabilities{
			Capabilities: &fav1.CapabilitiesSnapshot{RobotId: "amr-1"}}},
			reached: func(r classLegSeen) bool { return r.caps != 0 }},
		{name: "update_progress", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_UpdateProgress{
			UpdateProgress: &fav1.UpdateProgress{RobotId: "amr-1"}}},
			reached: func(r classLegSeen) bool { return r.progress != 0 }},
		{name: "action_status", control: &fav1.AdapterMessage{Payload: &fav1.AdapterMessage_ActionStatus{
			ActionStatus: &fav1.ActionStatusUpdate{ActionId: "a-1"}}},
			reached: func(r classLegSeen) bool { return len(r.actionAuth) != 1 || r.actionAuth[0] }},
		{name: "estop_clear_ack", safety: &fav1.AdapterSafetyMessage{RobotId: "amr-1",
			Payload: &fav1.AdapterSafetyMessage_EstopClearAck{EstopClearAck: &fav1.EstopClearAck{EstopId: "e-1"}}},
			reached: func(r classLegSeen) bool { return len(r.clears) != 0 }},
	}
	for _, w := range work {
		t.Run(w.name, func(t *testing.T) {
			f := newClassLegFixture(t, []string{"forklift"}, false)
			var replies []*fav1.ControlPlaneMessage
			if w.control != nil {
				replies = f.control(t, w.control)
			} else {
				f.safety(t, w.safety)
			}
			if w.reached(f.rec.seen()) {
				t.Errorf("%s from a robot outside acme's class list was taken as work", w.name)
			}
			if w.name == "register" {
				if len(replies) != 1 || replies[0].GetRegisterAck() == nil || replies[0].GetRegisterAck().GetAccepted() {
					t.Errorf("register replies %v, want one RegisterAck accepted: false", replies)
				}
			}
			if d := f.denials("amr-1"); len(d) != 1 || !strings.Contains(d[0], `does not serve robot class "amr"`) {
				t.Errorf("sealed denials %v, want one naming the unserved class", d)
			}
		})
	}
}

// An empty list is not a class that left the list: it serves every class at admission and at
// dispatch (candidate filter 9), so it fails no gate condition and its robots are given work. The
// stream refuses that work (security.md discloses it), and taking the robot's telemetry would
// keep it Idle and selected for work it cannot report on. So the exemption stops at a list that
// names other classes, and an empty list's telemetry is refused as before.
func TestClassLeft_EmptyListIsNotAClassThatLeft(t *testing.T) {
	f := newClassLegFixture(t, nil, false)
	f.control(t, clTelemetry("amr-1"))
	if got := f.rec.seen().frames; len(got) != 0 {
		t.Errorf("ingested %v from an adapter with an empty list, whose robots dispatch still serves", got)
	}
	if d := f.denials("amr-1"); len(d) != 1 {
		t.Errorf("sealed denials %v, want 1", d)
	}
}

// Only the class is relaxed. A robot bound to another adapter is not acme's to observe or to
// confirm stopped, whatever acme's list holds.
func TestClassLeft_ObservationStillNeedsTheBinding(t *testing.T) {
	f := newClassLegFixture(t, []string{"forklift"}, false)
	f.control(t, clTelemetry("amr-x"), clHeartbeat("amr-x"))
	f.safety(t, clEstopAck("amr-x", "e-x"), clLocalStop("amr-x"))
	if got := f.rec.seen(); len(got.frames) != 0 || len(got.heartbeats) != 0 || len(got.acks) != 0 {
		t.Errorf("acme was heard for amr-x, which is bound to another adapter: frames %v heartbeats %v acks %v",
			got.frames, got.heartbeats, got.acks)
	}
	if d := f.denials("amr-x"); len(d) != 4 {
		t.Errorf("sealed denials %v, want 4 (one per message)", d)
	}
}

// An unreadable FleetAdapter is not a class that left the list: the stream cannot tell what it
// serves, and fails closed.
func TestClassLeft_UnreadableAdapterStillFailsClosed(t *testing.T) {
	f := newClassLegFixture(t, []string{"amr"}, true)
	f.control(t, clTelemetry("amr-1"))
	f.safety(t, clEstopAck("amr-1", "e-1"))
	if got := f.rec.seen(); len(got.frames) != 0 || len(got.acks) != 0 {
		t.Errorf("taken on an unreadable FleetAdapter: frames %v acks %v", got.frames, got.acks)
	}
	if d := f.denials("amr-1"); len(d) != 2 {
		t.Errorf("sealed denials %v, want 2", d)
	}
}
