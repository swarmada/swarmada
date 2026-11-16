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
	"crypto/tls"
	"crypto/x509"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/robotid"
	"github.com/swarmada/swarmada/internal/streamauth"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A ROBOT ID RE-POINTED WHILE ITS STOP IS IN FLIGHT, AGAINST A REAL API SERVER.
//
// The estop goes out under the robot id the Robot carried when it was sent, and it is awaited
// under that id. The operator then re-points the Robot's swarmada.io/robot-id annotation. In
// the manager the SafetyStream authorizer reads Robots from the informer cache, so an EstopAck
// under the old id is authorized for as long as the cache still shows a Robot under it. Ruled
// (option (a)): that ack is the answer of the robot the estop was delivered to, on the stream it
// went down, under the estop's own id, so confirming the stop is correct. Once the cache has seen
// the re-point, no Robot carries the old id, the ack is refused, and the estop resolves Failed at
// the end of its window: escalated, never Stopped. An ack under the new id matches no estop.
//
// The dispatcher reads through a cache over the envtest API server, as the manager's does. The
// authorizer reads either that live cache, after it has seen the re-point, or a copy of what it
// held just before the re-point: the moment the cache has not caught up yet, made deterministic.

// switchReader is a client.Reader whose target a test can swap.
type switchReader struct {
	mu sync.Mutex
	r  client.Reader
}

func (s *switchReader) set(r client.Reader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.r = r
}

func (s *switchReader) get() client.Reader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r
}

func (s *switchReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return s.get().Get(ctx, key, obj, opts...)
}

func (s *switchReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return s.get().List(ctx, list, opts...)
}

// envAckFixture is ackFixture over the envtest API server: f.c reads and writes the API server
// directly; the dispatcher reads through live, a cache; the authorizer reads through authz.
type envAckFixture struct {
	*ackFixture
	live  client.Client
	authz *switchReader
}

func newEnvAckFixture(t *testing.T) *envAckFixture {
	t.Helper()
	requireSafetyEnvtest(t)
	ctx := context.Background()
	if err := safetyEnvK8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	for _, o := range []client.Object{
		&fleetv1.FleetAdapter{ObjectMeta: metav1.ObjectMeta{Name: "adapter-a", Namespace: ns},
			Spec: fleetv1.FleetAdapterSpec{ServesRobotClasses: []string{"amr"}}},
		&fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: ns},
			// The CRD requires a manufacturer, a model and a zone; the fake client in the unit tests does not.
			Spec: fleetv1.RobotSpec{RobotClass: "amr", Manufacturer: "acme", Model: "amr-x", Zone: "aisle-1",
				Adapter: fleetv1.AdapterRef{Name: "adapter-a", Version: "1.0.0"}}},
	} {
		if err := safetyEnvK8s.Create(ctx, o); err != nil {
			t.Fatalf("create %s: %v", o.GetName(), err)
		}
		o := o
		t.Cleanup(func() {
			_ = safetyEnvK8s.Delete(context.Background(), o)
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if err := safetyEnvK8s.Get(context.Background(), client.ObjectKeyFromObject(o), o); apierrors.IsNotFound(err) {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Errorf("%s was not deleted", o.GetName())
		})
	}

	cctx, cancel := context.WithCancel(ctx)
	ic, err := cache.New(safetyEnvCfg, cache.Options{Scheme: safetyEnvScheme,
		DefaultNamespaces: map[string]cache.Config{ns: {}}})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	stopped := make(chan struct{})
	go func() { _ = ic.Start(cctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
	sctx, scancelSync := context.WithTimeout(ctx, 10*time.Second)
	defer scancelSync()
	if !ic.WaitForCacheSync(sctx) {
		t.Fatal("the cache never synced")
	}
	live, err := client.New(safetyEnvCfg, client.Options{Scheme: safetyEnvScheme, Cache: &client.CacheOptions{Reader: ic}})
	if err != nil {
		t.Fatalf("cached client: %v", err)
	}
	// Start every informer the stop's path reads lazily (Robots, FleetAdapters, SwarmadaConfigs)
	// now, so none starts inside a stop's delivery window.
	for _, l := range []client.ObjectList{&fleetv1.RobotList{}, &fleetv1.FleetAdapterList{}, &fleetv1.SwarmadaConfigList{}} {
		if err := live.List(sctx, l, client.InNamespace(ns)); err != nil {
			t.Fatalf("warm the %T informer: %v", l, err)
		}
	}
	// Both reads go through the informer, so wait until it shows the seeded objects.
	waitForCache(t, live, "amr-1 in the cache", func() bool {
		r, err := robotid.Lookup(ctx, live, ns, "amr-1")
		return err == nil && r != nil
	})

	d := New(live, record.NewFakeRecorder(16))
	// As the manager wires it (cmd/manager/main.go): the outcome write reads the stored Robot past
	// the cache.
	d.APIReader = safetyEnvK8s
	d.deliveryTimeout = 750 * time.Millisecond
	d.confirmTimeout = 750 * time.Millisecond
	authz := &switchReader{r: live}
	srv := &controlstream.Server{Safety: d, Authorizer: &streamauth.Authorizer{Client: authz}}

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
	sctx, scancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- controlstream.NewGRPCRunnable(addr, srv, logr.Discard(), serverTLS).Start(sctx) }()
	t.Cleanup(func() { scancel(); <-done })
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
	f := &ackFixture{d: d, c: safetyEnvK8s, ca: ca, cli: map[string]fav1.FleetAdapterServiceClient{}}
	cert := ca.leaf(t, "adapter-a."+ns+".svc.cluster.local", x509.ExtKeyUsageClientAuth)
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: ca.pool, ServerName: "localhost", MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	})))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	f.cli["adapter-a"] = fav1.NewFleetAdapterServiceClient(conn)
	return &envAckFixture{ackFixture: f, live: live, authz: authz}
}

// waitForCache polls ok (which reads the cache with its own short calls) for up to 10 s.
func waitForCache(t *testing.T, c client.Reader, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// freezeAuthorizer points the authorizer at a copy of the Robots and FleetAdapters the live cache
// holds now: a cache that has not caught up with what is written next.
func (f *envAckFixture) freezeAuthorizer(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var robots fleetv1.RobotList
	var adapters fleetv1.FleetAdapterList
	if err := f.live.List(ctx, &robots, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if err := f.live.List(ctx, &adapters, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	var objs []client.Object
	for i := range robots.Items {
		objs = append(objs, robots.Items[i].DeepCopy())
	}
	for i := range adapters.Items {
		objs = append(objs, adapters.Items[i].DeepCopy())
	}
	f.authz.set(fake.NewClientBuilder().WithScheme(safetyEnvScheme).WithObjects(objs...).Build())
}

// The authorizer's cache still shows a Robot under the old id: the ack under it is the answer of
// the robot the estop went to, and it confirms the stop, written Stopped at the API server.
func TestEnvtest_ARepointedRobotIDStillCachedConfirmsTheDeliveredStop(t *testing.T) {
	f := newEnvAckFixture(t)
	// This case resolves on the ack, so a longer window costs nothing and keeps the test's own
	// steps between the send and the ack from racing the window on a loaded machine.
	f.d.deliveryTimeout = 5 * time.Second
	f.d.confirmTimeout = 5 * time.Second
	f.setWireID(t, "wire-7")
	waitForCache(t, f.live, "wire-7 in the cache", func() bool {
		r, err := robotid.Lookup(context.Background(), f.live, ns, "wire-7")
		return err == nil && r != nil
	})
	sentAs, res := f.estopAndAnswer(t, "wire-7", func() {
		f.freezeAuthorizer(t)
		f.setWireID(t, "wire-8")
	})
	if sentAs != "wire-7" {
		t.Fatalf("the estop envelope named %q, want the id at send time (wire-7)", sentAs)
	}
	if !res.Confirmed || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("result = %+v: the ack of the robot the estop was delivered to did not confirm it", res)
	}
	if got := f.robotEstopState(t, "amr-1"); got != fleetv1.RobotEstopStopped {
		t.Fatalf("amr-1 estopState at the API server = %s, want Stopped", got)
	}
	var r fleetv1.Robot
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "amr-1"}, &r); err != nil {
		t.Fatal(err)
	}
	if got := r.Annotations[fleetv1.RobotIDAnnotation]; got != "wire-8" {
		t.Errorf("amr-1's robot id after the outcome write = %q; the write must not undo the re-point", got)
	}
	f.requireNoWaiter(t)
}

// requireStillWaiting fails unless the stop's waiter is still registered: the ack about to be sent
// reaches a live stop, not one whose window already ran out.
func (f *envAckFixture) requireStillWaiting(t *testing.T) {
	t.Helper()
	f.d.mu.Lock()
	n := len(f.d.pending)
	f.d.mu.Unlock()
	if n == 0 {
		t.Fatal("the stop's window ran out before the ack was sent; this case would test nothing")
	}
}

// requireNoWaiter fails if a waiter is left after the trigger returned.
func (f *envAckFixture) requireNoWaiter(t *testing.T) {
	t.Helper()
	f.d.mu.Lock()
	n := len(f.d.pending)
	f.d.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d waiter(s) left after the trigger returned", n)
	}
}

// The authorizer's cache has seen the re-point: no Robot carries the old id, so an ack under it is
// refused, and one under the new id matches no estop. Either way the estop resolves Failed at the
// end of its window: escalated, never Stopped, and never stranded.
func TestEnvtest_ARepointedRobotIDTheCacheHasSeenResolvesFailed(t *testing.T) {
	for _, ack := range []string{"wire-7", "wire-8"} {
		t.Run("ack names "+ack, func(t *testing.T) {
			f := newEnvAckFixture(t)
			// This case resolves when its delivery window runs out. 3 s leaves the test's own steps
			// between the send and the ack (the re-point, the wait for the cache) room to finish
			// inside it, which requireStillWaiting checks.
			f.d.deliveryTimeout = 3 * time.Second
			f.setWireID(t, "wire-7")
			waitForCache(t, f.live, "wire-7 in the cache", func() bool {
				r, err := robotid.Lookup(context.Background(), f.live, ns, "wire-7")
				return err == nil && r != nil
			})
			start := time.Now()
			sentAs, res := f.estopAndAnswer(t, ack, func() {
				f.setWireID(t, "wire-8")
				waitForCache(t, f.live, "the re-point in the cache", func() bool {
					r, err := robotid.Lookup(context.Background(), f.live, ns, "wire-7")
					return err == nil && r == nil
				})
				f.requireStillWaiting(t)
			})
			if sentAs != "wire-7" {
				t.Fatalf("the estop envelope named %q, want the id at send time (wire-7)", sentAs)
			}
			if res.Confirmed || res.State != fleetv1.RobotEstopFailed {
				t.Fatalf("result = %+v, want Failed (escalated) once the cache has seen the re-point", res)
			}
			if got := f.robotEstopState(t, "amr-1"); got != fleetv1.RobotEstopFailed {
				t.Fatalf("amr-1 estopState at the API server = %s, want Failed (escalated)", got)
			}
			f.requireNoWaiter(t)
			if waited := time.Since(start); waited > 6*time.Second {
				t.Errorf("the trigger took %s to resolve; its delivery window is 3 s", waited)
			}
		})
	}
}
