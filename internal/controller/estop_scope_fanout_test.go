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
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

// The §9.3.8 `scope` label and the fan-out histogram (ADR-0042).
//
// TWO DEFECTS, one visible and one structural. Every estop metric emit site hard-coded
// scope="robot", so a zone or namespace estop was indistinguishable from a single-robot one.
// And the per-robot latency metric is stamped just before THAT robot's send, so sequential
// fan-out delays the send rather than the round trip: in a 50-robot zone estop every robot
// reports healthy sub-SLA latency, the violation counter stays at zero, and the last robot is
// commanded tens of seconds after the operator hit the trigger. The gauge is green while the
// zone is unsafe. The scope label alone does not fix that — it only relabels healthy
// observations — which is why the fan-out histogram exists.

// fanoutCount reads how many EPISODES the fan-out histogram has observed for a scope.
// Deltas are taken around each reconcile rather than assuming a zero baseline: the metric is
// process-global and other tests in this package fan out in the same namespace.
func fanoutCount(t *testing.T, ns string, scope metrics.EstopScope) uint64 {
	t.Helper()
	obs, err := metrics.EstopFanoutDurationSeconds.GetMetricWithLabelValues(ns, string(scope))
	if err != nil {
		t.Fatalf("fan-out metric: %v", err)
	}
	h, ok := obs.(prometheus.Histogram)
	if !ok {
		t.Fatalf("fan-out metric is %T, not a Histogram", obs)
	}
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatalf("writing fan-out metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestZoneEstop_CarriesZoneScope(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", &fleetv1.ZoneEstopPolicy{PropagateToChildren: true}, "e1"),
		zeRobot("r-a", "floor-1"), zeRobot("r-b", "floor-1"),
	)

	reconcileZE(t, r, "floor-1")

	if len(est.scopes) != 2 {
		t.Fatalf("scopes = %v, want one per estopped robot", est.scopes)
	}
	for i, got := range est.scopes {
		if got != metrics.ScopeZone {
			t.Errorf("scope[%d] = %q, want %q — a zone fan-out must not report as a robot estop",
				i, got, metrics.ScopeZone)
		}
	}
}

func TestNamespaceEstop_CarriesNamespaceScope(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newNamespaceEstop(t, est, nsConfig("evacuate", ""),
		zeRobot("amr-1", "z1"), zeRobot("amr-2", "z2"))

	reconcileNsEstop(t, r)

	if len(est.scopes) != 2 {
		t.Fatalf("scopes = %v, want one per estopped robot", est.scopes)
	}
	for i, got := range est.scopes {
		if got != metrics.ScopeNamespace {
			t.Errorf("scope[%d] = %q, want %q", i, got, metrics.ScopeNamespace)
		}
	}
}

func TestRobotEstop_CarriesRobotScope(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newRobotEstop(t, est, robotWithEstop("amr-1", "person detected", ""))

	reconcileRobotEstop(t, r, "amr-1")

	if len(est.scopes) != 1 || est.scopes[0] != metrics.ScopeRobot {
		t.Fatalf("scopes = %v, want [%s]", est.scopes, metrics.ScopeRobot)
	}
}

// The fan-out histogram observes ONCE per episode, not once per robot. Per-robot would make
// a wide fan-out look like many fast episodes — the same averaging-away that hid the gap.
func TestZoneEstop_ObservesFanoutOncePerEpisode(t *testing.T) {
	ns := zeNS
	before := fanoutCount(t, ns, metrics.ScopeZone)

	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", &fleetv1.ZoneEstopPolicy{PropagateToChildren: true}, "e1"),
		zeRobot("r-a", "floor-1"), zeRobot("r-b", "floor-1"), zeRobot("r-c", "floor-1"),
	)
	reconcileZE(t, r, "floor-1")

	if got := fanoutCount(t, ns, metrics.ScopeZone) - before; got != 1 {
		t.Fatalf("fan-out observations = %d for a 3-robot episode, want exactly 1", got)
	}
}

// A robot-scoped estop has no fan-out. Observing it would pollute the histogram's population
// with near-zero episodes and drag every quantile down — the metric would then under-report
// exactly the thing it exists to expose.
func TestRobotEstop_DoesNotObserveFanout(t *testing.T) {
	ns := zeNS
	before := fanoutCount(t, ns, metrics.ScopeRobot)

	est := &fakeEstopper{}
	r, _ := newRobotEstop(t, est, robotWithEstop("amr-1", "person detected", ""))
	reconcileRobotEstop(t, r, "amr-1")

	if got := fanoutCount(t, ns, metrics.ScopeRobot) - before; got != 0 {
		t.Fatalf("robot-scoped estop observed the fan-out histogram %d time(s), want 0", got)
	}
}

// An episode that confirms NOTHING is still an episode: a zone whose robots all fail to stop
// is exactly the one an operator needs timed, so it must not be silently unobserved.
func TestZoneEstop_ObservesFanoutEvenWhenNothingConfirms(t *testing.T) {
	ns := zeNS
	before := fanoutCount(t, ns, metrics.ScopeZone)

	est := &fakeEstopper{state: fleetv1.RobotEstopFailed}
	r, _ := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", &fleetv1.ZoneEstopPolicy{PropagateToChildren: true}, "e1"),
		zeRobot("r-a", "floor-1"),
	)
	reconcileZE(t, r, "floor-1")

	if got := fanoutCount(t, ns, metrics.ScopeZone) - before; got != 1 {
		t.Fatalf("fan-out observations = %d for an all-failed episode, want 1", got)
	}
}

// swarmada_robots_in_estop must expose Failed. metrics_sweeper already counted it into its
// per-namespace map, but SetRobotsInEstop iterated EstopStates — which omitted Failed — so the
// count was computed every sweep and thrown away. A robot withheld from dispatch for an estop
// the gauge does not count is invisible to the operator debugging it.
func TestSetRobotsInEstop_ExposesFailed(t *testing.T) {
	ns := "estop-states-ns"
	metrics.SetRobotsInEstop(ns, map[string]int{"Stopping": 1, "Stopped": 2, "Failed": 3})

	for state, want := range map[string]float64{"Stopping": 1, "Stopped": 2, "Failed": 3} {
		got := testutil.ToFloat64(metrics.RobotsInEstop.WithLabelValues(ns, state))
		if got != want {
			t.Errorf("robots_in_estop{estop_state=%q} = %v, want %v", state, got, want)
		}
	}
}

// ── Parallel fan-out (§9.6.2.1) ────────────────────────────────────────────────

// blockingEstopper holds every TriggerEstop call until all of them have arrived. If dispatch
// is sequential this deadlocks — robot 2's send never happens because robot 1's call has not
// returned — so the test times out rather than passing slowly. That is the point: it asserts
// CONCURRENCY, not merely that the refactor preserved the outcome.
type blockingEstopper struct {
	arrived chan struct{} // one token per call
	release chan struct{} // closed once every call has arrived

	mu     sync.Mutex
	robots []string
}

func (b *blockingEstopper) TriggerEstop(_ context.Context, _, robotID, _, _ string,
	_ metrics.EstopScope) (safety.Result, error) {
	b.mu.Lock()
	b.robots = append(b.robots, robotID)
	b.mu.Unlock()

	b.arrived <- struct{}{}
	<-b.release // every robot's send must have happened before any call returns
	return safety.Result{State: fleetv1.RobotEstopStopped, Confirmed: true, Delivered: true}, nil
}

func (b *blockingEstopper) ClearEstop(_ context.Context, _, _, _ string) (fleetv1.RobotEstopState, error) {
	return fleetv1.RobotEstopNormal, nil
}

// THE GAP THIS CLOSES. §9.6.2.1 requires every in-scope robot's estop to be issued in
// parallel "so that a slow acknowledgement from one robot does not delay the estop signal to
// others". The reference implementation dispatched sequentially: on a 50-robot zone the last
// robot was commanded tens of seconds after the trigger, while every per-robot round trip
// still measured healthy.
func TestZoneEstop_FansOutInParallel(t *testing.T) {
	const n = 3
	est := &blockingEstopper{arrived: make(chan struct{}, n), release: make(chan struct{})}

	objs := []client.Object{zeZone("floor-1", "", &fleetv1.ZoneEstopPolicy{PropagateToChildren: true}, "e1")}
	for _, name := range []string{"r-a", "r-b", "r-c"} {
		objs = append(objs, zeRobot(name, "floor-1"))
	}
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).WithObjects(objs...).Build()
	r := &ZoneEstopReconciler{Client: c, Estopper: est}

	done := make(chan error, 1)
	go func() {
		_, err := r.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: types.NamespacedName{Namespace: zeNS, Name: "floor-1"}})
		done <- err
	}()

	// Every robot's send must be in flight at once. Sequential dispatch cannot reach the
	// third arrival, because the first call is still blocked.
	for i := 0; i < n; i++ {
		select {
		case <-est.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d estops were in flight; the fan-out is still sequential", i, n)
		}
	}
	close(est.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile did not complete after the fan-out was released")
	}

	est.mu.Lock()
	got := sortedCopy(est.robots)
	est.mu.Unlock()
	if len(got) != n {
		t.Fatalf("estopped = %v, want all %d robots", got, n)
	}
}

// The same property for the widest scope, where sequential dispatch cost the most.
func TestNamespaceEstop_FansOutInParallel(t *testing.T) {
	const n = 3
	est := &blockingEstopper{arrived: make(chan struct{}, n), release: make(chan struct{})}
	r, _ := newNamespaceEstop(t, nil, nsConfig("evacuate", ""),
		zeRobot("amr-1", "z1"), zeRobot("amr-2", "z2"), zeRobot("amr-3", "z3"))
	r.Estopper = est

	done := make(chan error, 1)
	go func() {
		_, err := r.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: types.NamespacedName{Namespace: zeNS, Name: "swarmada"}})
		done <- err
	}()

	for i := 0; i < n; i++ {
		select {
		case <-est.arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d estops were in flight; the fan-out is still sequential", i, n)
		}
	}
	close(est.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile did not complete after the fan-out was released")
	}
}

// Aggregation must not depend on completion order: the counts, the worst latency and the
// audit entry's robot list are all derived after the join, in robot order.
func TestEstopFanout_ResultsAreOrderedByRobotNotByCompletion(t *testing.T) {
	// r-a is slow, r-c is fast — so completion order is the reverse of robot order.
	est := &staggeredEstopper{delays: map[string]time.Duration{
		"r-a": 60 * time.Millisecond,
		"r-b": 30 * time.Millisecond,
		"r-c": 0,
	}}
	robots := []fleetv1.Robot{*zeRobot("r-a", "z"), *zeRobot("r-b", "z"), *zeRobot("r-c", "z")}

	out, dur := estopFanout(context.Background(), est, zeNS, robots, "why", "who", metrics.ScopeZone)

	want := []string{"r-a", "r-b", "r-c"}
	for i := range want {
		if out[i].robot != want[i] {
			t.Fatalf("outcome[%d] = %q, want %q — results must be positional, not arrival-ordered",
				i, out[i].robot, want[i])
		}
	}
	// Parallel: the episode takes about as long as the SLOWEST robot, not the sum (90ms).
	if dur >= 90*time.Millisecond {
		t.Errorf("fan-out took %v, which is at least the sequential sum; it is not parallel", dur)
	}
}

// staggeredEstopper resolves each robot after a per-robot delay.
type staggeredEstopper struct{ delays map[string]time.Duration }

func (s *staggeredEstopper) TriggerEstop(_ context.Context, _, robotID, _, _ string,
	_ metrics.EstopScope) (safety.Result, error) {
	time.Sleep(s.delays[robotID])
	return safety.Result{State: fleetv1.RobotEstopStopped, Confirmed: true, Delivered: true}, nil
}

func (s *staggeredEstopper) ClearEstop(_ context.Context, _, _, _ string) (fleetv1.RobotEstopState, error) {
	return fleetv1.RobotEstopNormal, nil
}
