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
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/leadership"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The leader term and stream ownership on a real API server and real leader election: a
// control-plane process serves an adapter's streams only while it holds leadership and closes
// them when it loses it (RFC-0001 #safety-robot-stop-table), and the term it stamps never
// decreases, even when the election Lease is deleted (RFC-0001
// #fleet-adapter-protocol-the-stop-latch, The leader term).

func leaseClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(envCfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnvtest_LeaderTermFloor_NeverFallsWhenTheElectionLeaseIsDeleted(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	c := leaseClient(t)
	n := int32(5)
	election := &coordinationv1.Lease{}
	election.Namespace, election.Name = ns, "swarmada-leader"
	election.Spec.LeaseTransitions = &n
	if err := c.Create(ctx, election); err != nil {
		t.Fatal(err)
	}
	f := &leadership.Floor{Reader: c, Writer: c, Namespace: ns, ElectionLease: "swarmada-leader"}

	first, err := f.Next(ctx)
	if err != nil || first != 6 {
		t.Fatalf("first term = %d, %v; want 6 (5 transitions, no floor)", first, err)
	}
	if err := c.Delete(ctx, election); err != nil {
		t.Fatal(err)
	}
	second, err := f.Next(ctx)
	if err != nil || second <= first {
		t.Fatalf("term after the election Lease was deleted = %d, %v; want above %d", second, err, first)
	}
	// The next election creates the Lease again, its count back at 0.
	zero := int32(0)
	again := &coordinationv1.Lease{}
	again.Namespace, again.Name = ns, "swarmada-leader"
	again.Spec.LeaseTransitions = &zero
	if err := c.Create(ctx, again); err != nil {
		t.Fatal(err)
	}
	third, err := f.Next(ctx)
	if err != nil || third <= second {
		t.Fatalf("term after the Lease was created again = %d, %v; want above %d", third, err, second)
	}
}

func TestEnvtest_LeaderTermFloor_ConcurrentProcessesNeverIssueOneTerm(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	c := leaseClient(t)
	const n = 12
	terms := make(chan uint64, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := &leadership.Floor{Reader: c, Writer: c, Namespace: ns}
			for attempt := 0; attempt < 20; attempt++ {
				term, err := f.Next(ctx)
				if err == nil {
					terms <- term
					return
				}
				if attempt == 19 {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(terms)
	close(errs)
	for err := range errs {
		t.Fatalf("a process issued no term: %v", err)
	}
	seen := map[uint64]bool{}
	for term := range terms {
		if seen[term] {
			t.Fatalf("term %d issued to two processes: the floor's optimistic write did not hold", term)
		}
		seen[term] = true
	}
	if len(seen) != n {
		t.Fatalf("%d distinct terms, want %d", len(seen), n)
	}
}

// replica is one control-plane process: a manager running real leader election, the
// leadership runnable, and a ControlStream server gated on its State, served over bufconn.
type replica struct {
	state  *leadership.State
	cancel context.CancelFunc
	done   chan error
	client fav1.FleetAdapterServiceClient
	stop   func()
}

func startReplica(t *testing.T, ns, electionID string) *replica {
	t.Helper()
	leaseDur, renew, retry := 3*time.Second, 2*time.Second, 200*time.Millisecond
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:                        envScheme,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                true,
		LeaderElectionID:              electionID,
		LeaderElectionNamespace:       ns,
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &leaseDur,
		RenewDeadline:                 &renew,
		RetryPeriod:                   &retry,
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	r := &replica{state: &leadership.State{}, done: make(chan error, 1)}
	lc := leaseClient(t)
	if err := mgr.Add(&leadership.Runnable{
		State: r.state,
		Terms: &leadership.Floor{Reader: lc, Writer: lc, Namespace: ns, ElectionLease: electionID},
		Log:   logr.Discard(),
		Retry: 100 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	fav1.RegisterFleetAdapterServiceServer(gs, &controlstream.Server{
		Registrar:  acceptingRegistrar{},
		Leadership: r.state,
	})
	go func() { _ = gs.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	r.client = fav1.NewFleetAdapterServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- mgr.Start(ctx) }()
	var once sync.Once
	r.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-r.done:
			case <-time.After(15 * time.Second):
				t.Error("manager did not stop")
			}
			_ = conn.Close()
			gs.Stop()
		})
	}
	t.Cleanup(r.stop)
	return r
}

// acceptingRegistrar is the stream server's registrar for a replica: it accepts every robot.
type acceptingRegistrar struct{}

func (acceptingRegistrar) Register(context.Context, controlstream.AdapterIdentity, *fav1.RegisterRobot) *fav1.RegisterAck {
	return &fav1.RegisterAck{Accepted: true}
}

func (acceptingRegistrar) Discover(context.Context, controlstream.AdapterIdentity, controlstream.TLSIdentity, *fav1.DiscoverRobot) *fav1.DiscoverAck {
	return &fav1.DiscoverAck{Accepted: true}
}

func (r *replica) leads(within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, ok := r.state.Leading(); ok {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// open opens a ControlStream on the replica and runs its handshake; it returns the stream and
// the error the handshake ended with, if any.
func (r *replica) open(t *testing.T, ctx context.Context) (fav1.FleetAdapterService_ControlStreamClient, error) {
	t.Helper()
	stream, err := r.client.ControlStream(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&fav1.AdapterMessage{Payload: &fav1.AdapterMessage_Hello{Hello: &fav1.AdapterHello{
		Vendor: "acme", ProtocolVersion: controlstream.ProtocolVersion, ContractVersion: "1.4.0", Namespace: "n",
	}}}); err != nil && !errors.Is(err, io.EOF) {
		// io.EOF: the server has already ended the stream, and Recv returns its status.
		return nil, err
	}
	msg, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	if !msg.GetHelloAck().GetAccepted() {
		t.Fatalf("handshake refused: %v", msg)
	}
	return stream, nil
}

func unavailableWith(err error, want string) bool {
	st, _ := status.FromError(err)
	return st.Code() == codes.Unavailable && strings.Contains(st.Message(), want)
}

func TestEnvtest_StreamOwnership_FollowsTheElectionAndTheTermRisesAcrossLeaders(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	const electionID = "swarmada-leader-test"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a := startReplica(t, ns, electionID)
	if !a.leads(20 * time.Second) {
		t.Fatal("the first replica never took leadership")
	}
	b := startReplica(t, ns, electionID)
	if b.leads(2 * time.Second) {
		t.Fatal("two replicas lead at once")
	}
	termA, _ := a.state.Term()

	// The non-leader refuses a stream; the leader serves it.
	if _, err := b.open(t, ctx); !unavailableWith(err, "not the leader") {
		t.Fatalf("stream on the non-leader: %v; want Unavailable \"not the leader\"", err)
	}
	stream, err := a.open(t, ctx)
	if err != nil {
		t.Fatalf("stream on the leader: %v", err)
	}

	// The leader stops (the manager cancels the leadership runnable, as it does on a lost
	// election): its open stream is closed, and the other replica takes over under a higher term.
	a.cancel()
	if _, err := stream.Recv(); !unavailableWith(err, "leadership lost") {
		t.Fatalf("the old leader's stream after it stopped: %v; want Unavailable \"leadership lost\"", err)
	}
	if !b.leads(20 * time.Second) {
		t.Fatal("the second replica never took leadership after the first released it")
	}
	termB, _ := b.state.Term()
	if termB <= termA {
		t.Fatalf("term fell or repeated across leaders: %d then %d", termA, termB)
	}
	if _, err := b.open(t, ctx); err != nil {
		t.Fatalf("stream on the new leader: %v", err)
	}

	// The election Lease is deleted while no replica runs; the next leader's term still rises.
	b.stop()
	lc := leaseClient(t)
	election := &coordinationv1.Lease{}
	if err := lc.Get(ctx, client.ObjectKey{Namespace: ns, Name: electionID}, election); err != nil {
		t.Fatalf("reading the election Lease: %v", err)
	}
	if err := lc.Delete(ctx, election); err != nil {
		t.Fatal(err)
	}
	c := startReplica(t, ns, electionID)
	if !c.leads(20 * time.Second) {
		t.Fatal("no replica took leadership after the election Lease was deleted")
	}
	if termC, _ := c.state.Term(); termC <= termB {
		t.Fatalf("term fell or repeated after the election Lease was deleted: %d then %d", termB, termC)
	}
}
