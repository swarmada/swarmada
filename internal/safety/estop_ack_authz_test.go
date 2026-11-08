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

package safety

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/streamauth"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A confirmed stop is the one state the safety model treats as proof a robot is at rest
// (RFC-0001 #safety-emergency-stop-protocol). It may be confirmed only by the adapter the
// estop was sent to, for the robot it was sent for. These tests run two adapters with
// real client certificates against the real SafetyStream, the real streamauth.Authorizer
// and the real Dispatcher: adapter-a serves amr-1 and is sent the estop; adapter-b serves
// amr-b and tries to confirm amr-1's stop.

type ackCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newAckCA(t *testing.T) *ackCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "namespace-ca"},
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
	return &ackCA{cert: cert, key: key, pool: pool}
}

func (ca *ackCA) leaf(t *testing.T, san string, eku x509.ExtKeyUsage) tls.Certificate {
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

type ackFixture struct {
	d *Dispatcher
	// c is the dispatcher's own client: every write takes the lock the dispatcher's writes take
	// (atomicFake). It is the only client a test gets. Write through update, which retries a
	// conflict from a fresh read.
	c   client.Client
	ca  *ackCA
	cli map[string]fav1.FleetAdapterServiceClient // adapter name → client presenting its cert
}

func newAckFixture(t *testing.T) *ackFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	robot := func(name, adapter string) *fleetv1.Robot {
		return &fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: fleetv1.RobotSpec{RobotClass: "amr",
				Adapter: fleetv1.AdapterRef{Name: adapter, Version: "1.0.0"}},
		}
	}
	adapter := func(name string) *fleetv1.FleetAdapter {
		return &fleetv1.FleetAdapter{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       fleetv1.FleetAdapterSpec{ServesRobotClasses: []string{"amr"}},
		}
	}
	// The fake applies an update by a read and a store that are not one step, so a test's write
	// beside the dispatcher's own (the stop's record, written in the background after the send) can
	// be undone by it, where an API server would refuse the stale one. The test's writes take the
	// dispatcher's lock, and update retries the refused one.
	c := atomicFake(fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(robot("amr-1", "adapter-a"), robot("amr-b", "adapter-b"), adapter("adapter-a"), adapter("adapter-b")).
		WithStatusSubresource(&fleetv1.Robot{}).Build())
	d := New(c, record.NewFakeRecorder(16))
	d.deliveryTimeout = 750 * time.Millisecond
	d.confirmTimeout = 750 * time.Millisecond

	srv := &controlstream.Server{Safety: d, Authorizer: &streamauth.Authorizer{Client: c}}
	ca := newAckCA(t)
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
	f := &ackFixture{d: d, c: c, ca: ca, cli: map[string]fav1.FleetAdapterServiceClient{}}
	for _, name := range []string{"adapter-a", "adapter-b"} {
		cert := ca.leaf(t, name+"."+ns+".svc.cluster.local", x509.ExtKeyUsageClientAuth)
		conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs: ca.pool, ServerName: "localhost", MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
		})))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		f.cli[name] = fav1.NewFleetAdapterServiceClient(conn)
	}
	return f
}

// openSafety opens adapter's SafetyStream and waits until the dispatcher has it registered.
func (f *ackFixture) openSafety(t *testing.T, adapter string) fav1.FleetAdapterService_SafetyStreamClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := f.cli[adapter].SafetyStream(ctx)
	if err != nil {
		t.Fatalf("open %s safety stream: %v", adapter, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.d.mu.Lock()
		_, ok := f.d.streams[adapterKey(ns, adapter)]
		f.d.mu.Unlock()
		if ok {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s SafetyStream never registered with the dispatcher", adapter)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// update applies change to the object at key, read fresh into obj, and writes it. A write that
// conflicts with one of the dispatcher's is retried from a new read, as a client of an API server
// does.
func (f *ackFixture) update(t *testing.T, key client.ObjectKey, obj client.Object, change func()) {
	t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := f.c.Get(context.Background(), key, obj); err != nil {
			return err
		}
		change()
		return f.c.Update(context.Background(), obj)
	}); err != nil {
		t.Fatalf("update %s: %v", key.Name, err)
	}
}

func (f *ackFixture) robotEstopState(t *testing.T, name string) fleetv1.RobotEstopState {
	t.Helper()
	r := &fleetv1.Robot{}
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, r); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return r.Status.EstopState
}

// triggerAndAck estops amr-1, waits for the estop to reach adapter-a (which never acks
// it), and then has ackFrom send ack for that estop_id. It returns the trigger's result.
func (f *ackFixture) triggerAndAck(t *testing.T, ackFrom string, envelopeRobot string) Result {
	t.Helper()
	a := f.openSafety(t, "adapter-a")
	b := a
	if ackFrom != "adapter-a" {
		b = f.openSafety(t, ackFrom)
	}
	type out struct {
		res Result
		err error
	}
	resCh := make(chan out, 1)
	go func() {
		res, err := f.d.TriggerEstop(context.Background(), ns, "amr-1", "test", "tester", metrics.ScopeRobot, Carrier{})
		resCh <- out{res, err}
	}()
	pushed, err := a.Recv()
	if err != nil {
		t.Fatalf("adapter-a never received the estop: %v", err)
	}
	estopID := pushed.GetEstop().GetEstopId()
	if estopID == "" {
		t.Fatalf("adapter-a received %v, want an estop", pushed)
	}
	if err := b.Send(&fav1.AdapterSafetyMessage{
		RobotId: envelopeRobot,
		Payload: &fav1.AdapterSafetyMessage_EstopAck{EstopAck: &fav1.EstopAck{
			EstopId: estopID, State: fav1.EstopState_ESTOP_STATE_STOPPED,
		}},
	}); err != nil {
		t.Fatalf("send ack: %v", err)
	}
	select {
	case o := <-resCh:
		return o.res
	case <-time.After(10 * time.Second):
		t.Fatalf("TriggerEstop never returned")
	}
	return Result{}
}

// An EstopAck with an empty envelope robot_id skipped authorization and was routed by
// estop_id alone, so any adapter with a client certificate could confirm a stop for a
// robot it does not serve.
func TestEstopAck_FromAnotherAdapterWithEmptyRobotIDDoesNotConfirm(t *testing.T) {
	f := newAckFixture(t)
	res := f.triggerAndAck(t, "adapter-b", "")
	if res.Confirmed || res.State == fleetv1.RobotEstopStopped {
		t.Errorf("result = %+v: adapter-b confirmed amr-1's stop with an empty robot_id", res)
	}
	if got := f.robotEstopState(t, "amr-1"); got == fleetv1.RobotEstopStopped {
		t.Errorf("amr-1 estopState = Stopped on adapter-b's word; the robot's own adapter never confirmed")
	}
}

// The same forgery with the envelope naming a robot adapter-b DOES serve: the envelope
// passes authorization, and a lookup by estop_id alone resolves amr-1's waiter anyway.
func TestEstopAck_FromAnotherAdapterNamingItsOwnRobotDoesNotConfirm(t *testing.T) {
	f := newAckFixture(t)
	res := f.triggerAndAck(t, "adapter-b", "amr-b")
	if res.Confirmed || res.State == fleetv1.RobotEstopStopped {
		t.Errorf("result = %+v: adapter-b confirmed amr-1's stop under its own robot's name", res)
	}
	if got := f.robotEstopState(t, "amr-1"); got == fleetv1.RobotEstopStopped {
		t.Errorf("amr-1 estopState = Stopped on adapter-b's word")
	}
}

// The control: the robot's own adapter, naming the robot, confirms the stop.
func TestEstopAck_FromTheRobotsOwnAdapterConfirms(t *testing.T) {
	f := newAckFixture(t)
	res := f.triggerAndAck(t, "adapter-a", "amr-1")
	if !res.Confirmed || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("result = %+v, want a confirmed stop from amr-1's own adapter", res)
	}
	if got := f.robotEstopState(t, "amr-1"); got != fleetv1.RobotEstopStopped {
		t.Errorf("amr-1 estopState = %s, want Stopped", got)
	}
}
