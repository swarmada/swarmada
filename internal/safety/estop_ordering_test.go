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
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"

	"github.com/swarmada/swarmada/internal/metrics"
)

// holdGetKey marks the one context whose Get gatedGetClient holds open.
type holdGetKey struct{}

// gatedGetClient holds a Get made on a context carrying holdGetKey until release is
// closed. It opens the window a concurrent estop actually races in: the clear has
// been ISSUED (the operator pressed clear) but has not yet read state or written,
// and a trigger resolves entirely inside that window. Only the marked context is
// held, so the trigger's own reads run unimpeded.
type gatedGetClient struct {
	client.Client
	entered chan struct{} // closed once the held Get is reached
	release chan struct{} // closed to let the held Get proceed
	once    sync.Once
}

func (g *gatedGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if held, _ := ctx.Value(holdGetKey{}).(bool); held {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.Client.Get(ctx, key, obj, opts...)
}

// stoppedSender confirms every estop immediately with a STOPPED ack.
func stoppedSender(d *Dispatcher) *scriptedSender {
	return &scriptedSender{d: d, reply: func(id string) []*fav1.EstopAck {
		return []*fav1.EstopAck{ackState(id, fav1.EstopState_ESTOP_STATE_STOPPED)}
	}}
}

// A clear issued BEFORE a trigger must not release the stop, however
// late its write reaches the API server. Losing this returns the robot to the
// scheduler's candidate set (internal/controller/adapter_readiness.go) with a live
// hazard outstanding.
func TestEstopOrdering_ClearIssuedBeforeTriggerDoesNotReleaseTheStop(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	setState(t, c, fleetv1.RobotEstopStopped) // an earlier estop the operator is clearing
	gated := &gatedGetClient{Client: c, entered: make(chan struct{}), release: make(chan struct{})}
	d.Client = gated
	d.RegisterStream(identity("acme"), stoppedSender(d))

	// The clear is issued first, then held before it reads state.
	done := make(chan struct{})
	go func() {
		defer close(done)
		heldCtx := context.WithValue(context.Background(), holdGetKey{}, true)
		if _, err := d.ClearEstop(heldCtx, ns, "amr-1", "admin", Ends{}); err != nil {
			t.Errorf("ClearEstop: %v", err)
		}
	}()
	<-gated.entered

	// A fresh hazard is raised while the clear is in flight, and resolves fully.
	res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{})
	if err != nil || res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}

	// Only now does the clear's write reach the API server.
	close(gated.release)
	<-done

	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s, want Stopped: a clear issued before the trigger released a live estop", got)
	}
}

// The reverse interleaving still works: a clear issued AFTER the trigger resolved
// is the later event and releases the stop. A fix that simply refused every clear
// while a stop was recorded would pass the test above and fail this one.
//
// "Releases the stop" means Resuming, not Normal: the robot leaves Stopped, which
// is what the ordering gate governs, and enters the post-clear window
// (RFC-0001 #safety-estop-state-machine). What it does NOT do is go straight back
// into the scheduler's candidate set.
func TestEstopOrdering_ClearIssuedAfterTriggerReleasesTheStop(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.RegisterStream(identity("acme"), stoppedSender(d))

	if res, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil ||
		res.State != fleetv1.RobotEstopStopped {
		t.Fatalf("TriggerEstop res=%+v err=%v; want Stopped", res, err)
	}
	state, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin", Ends{})
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if state != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop returned %s, want Resuming", state)
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming after a clear issued after the trigger", got)
	}
}

// Estop stays unfenced. A stop write carrying an epoch older than the robot's
// current one is still applied: the ordering epoch gates the RELEASE direction
// only, so a stale token can never refuse or delay a stop (RFC-0001 #safety-emergency-stop-protocol, ADR-0046).
func TestEstopOrdering_StaleEpochNeverBlocksAStop(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	key := robotEstopKey(ns, "amr-1")
	// Two later triggers have already been issued for this robot.
	d.issueEstopEpoch(key, true)
	stale := d.issueEstopEpoch(key, true) - 1

	robot := &fleetv1.Robot{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "amr-1", Namespace: ns}, robot); err != nil {
		t.Fatal(err)
	}
	if !d.setEstopState(context.Background(), robot, fleetv1.RobotEstopStopped, "confirmed stopped", stale) {
		t.Fatal("a stop write with a stale epoch was refused; estop must never be fenced")
	}
	if got := estopState(t, c); got != fleetv1.RobotEstopStopped {
		t.Fatalf("estopState = %s, want Stopped", got)
	}
}

// A clear that is genuinely the latest event is not dropped by the epoch gate even
// when a trigger for the same robot came earlier in the same episode.
func TestEstopOrdering_LatestClearIsNotDroppedByTheGate(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	d.RegisterStream(identity("acme"), stoppedSender(d))

	for i := 0; i < 3; i++ {
		if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil {
			t.Fatalf("TriggerEstop: %v", err)
		}
		if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin", Ends{}); err != nil {
			t.Fatalf("ClearEstop: %v", err)
		}
		if got := estopState(t, c); got != fleetv1.RobotEstopResuming {
			t.Fatalf("round %d: estopState = %s, want Resuming", i, got)
		}
	}
}

// A trigger and a clear started together resolve to one of the two orderings and
// never to a torn result — and whichever wins, the run does not deadlock. Run with
// -race, this is the interleaving the item describes.
func TestEstopOrdering_ConcurrentTriggerAndClearResolveCleanly(t *testing.T) {
	d, c, _ := newDispatcher(t, "acme")
	setState(t, c, fleetv1.RobotEstopStopped)
	d.RegisterStream(identity("acme"), stoppedSender(d))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := d.TriggerEstop(context.Background(), ns, "amr-1", "hazard", "operator", metrics.ScopeRobot, Carrier{}); err != nil {
			t.Errorf("TriggerEstop: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := d.ClearEstop(context.Background(), ns, "amr-1", "admin", Ends{}); err != nil {
			t.Errorf("ClearEstop: %v", err)
		}
	}()
	waitTimeout(t, &wg, 5*time.Second)

	// Stopped (the trigger won) or Resuming (the clear won). Never Normal: nothing
	// in this test confirms a resume, and the release path must not reach Normal
	// without one.
	switch got := estopState(t, c); got {
	case fleetv1.RobotEstopStopped, fleetv1.RobotEstopResuming:
	default:
		t.Fatalf("estopState = %s, want Stopped or Resuming", got)
	}
}

func waitTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out waiting for the concurrent estop writes to settle")
	}
}
