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

package compute_test

// a Service load-balances per connection/request, so handing an
// adapter the Service VIP for a contract: Service run silently splits one
// robot's stream across replicas. These tests exercise the pinning mechanism
// against three REAL, distinct gRPC servers standing in for three Service
// replicas — not a mock that merely asserts a function was called with the
// "right" argument — so "which replica received the traffic" is an observed
// fact, not a claim.
//
// compute.v1 carries no frame RPC (frames are data-path, adapter → runtime
// direct, never through this control channel — design Principle 3), so
// RenewLease traffic stands in for "frames" here: it is the one RPC a
// long-lived Service-contract run repeats many times over its lifetime,
// which is exactly the property being tested.

import (
	"context"
	"net"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/swarmada/swarmada/internal/compute"
	"github.com/swarmada/swarmada/internal/computeclient"
	computev1 "github.com/swarmada/swarmada/proto/compute/v1"
)

// replicaStub is one Service replica. It records how many RenewLease calls it
// personally received, so the test can assert traffic landed on exactly one.
type replicaStub struct {
	computev1.UnimplementedComputeRuntimeServiceServer
	mu    sync.Mutex
	calls int
}

func (r *replicaStub) RenewLease(context.Context, *computev1.RenewLeaseRequest) (*computev1.RenewLeaseResponse, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return &computev1.RenewLeaseResponse{Renewed: true}, nil
}

func (r *replicaStub) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// startReplica binds a stub to an OS-assigned loopback port and returns its
// dialable "host:port" address. Real EndpointSlice entries behind one Service
// normally share ONE port and differ by pod IP; this sandbox cannot bind
// secondary loopback addresses (127.0.0.2,.3,...), so each replica here
// instead gets its own port on 127.0.0.1 and its own EndpointSlice object
// (endpointSliceFor) — the API shape a resolver must handle regardless of
// whether a Service happens to be split across multiple EndpointSlices.
func startReplica(t *testing.T) (stub *replicaStub, address string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stub = &replicaStub{}
	srv := grpc.NewServer()
	computev1.RegisterComputeRuntimeServiceServer(srv, stub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return stub, lis.Addr().String()
}

// endpointSliceFor builds one EndpointSlice carrying a single ready address,
// labeled for `service` — one object per replica, as ResolvePinnedEndpoint
// must aggregate across every EndpointSlice a Service owns, not just one.
func endpointSliceFor(name, namespace, service, address string) *discoveryv1.EndpointSlice {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		panic(err)
	}
	portNum, err := strconv.Atoi(portStr)
	if err != nil {
		panic(err)
	}
	port := int32(portNum)
	portName := "grpc"
	ready := true
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: service},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{host},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
	}
}

// TestPin_StreamingRunPinnedToOneReplicaForItsLifetime is the "Done
// when" test: three replicas behind one Service; a run pins to one and stays
// pinned across repeated calls AND across a second resolution (simulating the
// next reconcile), observed by which replica's counter moved.
func TestPin_StreamingRunPinnedToOneReplicaForItsLifetime(t *testing.T) {
	r1, addr1 := startReplica(t)
	r2, addr2 := startReplica(t)
	r3, addr3 := startReplica(t)
	stubOf := map[string]*replicaStub{addr1: r1, addr2: r2, addr3: r3}

	scheme := runtime.NewScheme()
	if err := discoveryv1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(
			endpointSliceFor("vehicle-detector-1", "yard-west", "vehicle-detector", addr1),
			endpointSliceFor("vehicle-detector-2", "yard-west", "vehicle-detector", addr2),
			endpointSliceFor("vehicle-detector-3", "yard-west", "vehicle-detector", addr3),
		).
		Build()

	ctx := context.Background()

	// Deterministic selection (lowest "host:port" string) — computed the same
	// way ResolvePinnedEndpoint computes it, so this test does not assume
	// which OS-assigned port happens to sort first.
	sorted := []string{addr1, addr2, addr3}
	sort.Strings(sorted)
	wantFirst := sorted[0]
	winner := stubOf[wantFirst]
	var loser1, loser2 *replicaStub
	for addr, stub := range stubOf {
		if addr == wantFirst {
			continue
		}
		if loser1 == nil {
			loser1 = stub
		} else {
			loser2 = stub
		}
	}

	addr, err := compute.ResolvePinnedEndpoint(ctx, fc, "yard-west", "vehicle-detector", "grpc")
	if err != nil {
		t.Fatalf("ResolvePinnedEndpoint: %v", err)
	}
	if addr != wantFirst {
		t.Fatalf("ResolvePinnedEndpoint() = %q, want %q (deterministic lowest-address selection)", addr, wantFirst)
	}

	// Dial the PINNED address specifically — never the Service. Drive several
	// calls, standing in for "frames" over the run's lifetime.
	c := dialAt(t, addr)
	for i := 0; i < 5; i++ {
		if _, _, err := c.RenewLease(ctx, "inv-1", nowPlus()); err != nil {
			t.Fatalf("RenewLease call %d: %v", i, err)
		}
	}

	// Next reconcile: Pin() is called again with the address already
	// resolved. It MUST return the SAME address unchanged — re-resolving
	// every reconcile would defeat "pinned... for the run's lifetime".
	again, changed, err := compute.Pin(ctx, fc, "yard-west", "vehicle-detector", "grpc", addr)
	if err != nil {
		t.Fatalf("Pin (re-check): %v", err)
	}
	if changed || again != addr {
		t.Fatalf("Pin() re-resolved on a live endpoint: got (%q, changed=%v), want (%q, changed=false)", again, changed, addr)
	}

	// More traffic after the "next reconcile" pin check — still the same replica.
	for i := 0; i < 3; i++ {
		if _, _, err := c.RenewLease(ctx, "inv-1", nowPlus()); err != nil {
			t.Fatalf("RenewLease (second batch) call %d: %v", i, err)
		}
	}

	if got := winner.count(); got != 8 {
		t.Errorf("pinned replica (%s) received %d calls, want 8 (all of them)", wantFirst, got)
	}
	if got := loser1.count(); got != 0 {
		t.Errorf("a non-pinned replica received %d calls, want 0 — the stream must not split across replicas", got)
	}
	if got := loser2.count(); got != 0 {
		t.Errorf("a non-pinned replica received %d calls, want 0 — the stream must not split across replicas", got)
	}
}

// TestPin_EndpointLossRePlaces is the other half of the "Done when":
// on losing the pinned endpoint, the run is re-placed onto a different ready
// replica rather than left dangling — `changed` is what a caller uses to
// raise the discontinuity event.
func TestPin_EndpointLossRePlaces(t *testing.T) {
	_, addr1 := startReplica(t)
	_, addr2 := startReplica(t)

	scheme := runtime.NewScheme()
	if err := discoveryv1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	// Only addr2's EndpointSlice exists here — addr1 "left" the Service,
	// simulating endpoint loss of the previously pinned replica.
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(endpointSliceFor("vehicle-detector-2", "yard-west", "vehicle-detector", addr2)).
		Build()

	next, changed, err := compute.Pin(context.Background(), fc, "yard-west", "vehicle-detector", "grpc", addr1)
	if err != nil {
		t.Fatalf("Pin (after loss): %v", err)
	}
	if !changed {
		t.Fatal("losing the pinned endpoint must re-place onto a different address (changed=true)")
	}
	if next == addr1 {
		t.Fatalf("re-placement returned the lost address %q", next)
	}
	// Never a Service VIP/DNS name — only a backend address drawn from the
	// EndpointSlice, and specifically the one remaining ready replica.
	if next != addr2 {
		t.Fatalf("Pin() re-placed to %q, want the one remaining ready address %q", next, addr2)
	}
}

// TestResolvePinnedEndpoint_NeverReturnsTheServiceItself makes the "never the
// VIP" invariant test-visible rather than merely implicit in what the fake
// object graph happens to contain: no Service object exists in this test's
// client at all, so ResolvePinnedEndpoint has nothing to fall back to and
// MUST fail rather than silently resolve to a name it was never given.
func TestResolvePinnedEndpoint_NeverReturnsTheServiceItself(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := discoveryv1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).Build() // no EndpointSlice at all

	_, err := compute.ResolvePinnedEndpoint(context.Background(), fc, "yard-west", "vehicle-detector", "grpc")
	if err == nil {
		t.Fatal("expected an error with no ready EndpointSlice, got nil")
	}
}

func nowPlus() time.Time { return time.Now().Add(time.Minute) }

func dialAt(t *testing.T, addr string) *computeclient.Client {
	t.Helper()
	c, err := computeclient.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Dial(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
