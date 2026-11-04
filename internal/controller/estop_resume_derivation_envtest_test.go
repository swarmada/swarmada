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
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// Release from Resuming in a real manager, with every reconciler that acts on a Robot
// running: it happens only on an operator's request, once per request, and never on
// telemetry (RFC-0001 #safety-estop-state-machine).
//
// The telemetry sink writes status.hardware, the Robot reconciler derives
// status.capabilities from it, and the robot-estop controller relays an operator's
// request to the dispatcher. The first two are queued by the same status event and
// nothing orders them, so the test holds the Robot reconciler back while a post-clear
// frame reporting the lidar Failed is processed and a request arrives — the
// interleaving in which the stored capability list is still the pre-stop one — and
// the request is refused on the reported hardware. A healthy frame then releases
// nothing by itself; a new request does.
func TestEnvtest_EstopResume_FullManagerReleasesOnlyOnAnOperatorRequest(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	key := types.NamespacedName{Namespace: ns, Name: "amr-1"}

	robot := envtestValidRobot(ns, "amr-1", "z-a")
	robot.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}}
	robot.Spec.Capabilities = []fleetv1.ClassCapability{{Name: "navigation", Type: fleetv1.CapabilityKindHardwareNative, RequiredHardware: []string{"lidar"}}}
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	seedDerivationFixture(t, ns, "amr-1")

	frames := resumeWitness()
	dispatcher := safety.New(envK8s, nil)
	dispatcher.Telemetry = frames
	wireResumeDerivation(dispatcher)
	calls := &confirmCounter{inner: dispatcher}
	held := newReconcileGate()

	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:  envScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	// The router is the Robot reconciler's frame clock, as in cmd/manager, so a robot
	// that is sending frames is not judged silent.
	robots := &RobotReconciler{Client: mgr.GetClient(), Scheme: envScheme, Frames: frames}
	if err := ctrl.NewControllerManagedBy(mgr).For(&fleetv1.Robot{}).Named("robot-derive-" + ns).
		Complete(held.wrap(robots)); err != nil {
		t.Fatalf("register robot reconciler: %v", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&fleetv1.Robot{}).Named("robotestop-" + ns).
		Complete(&RobotEstopReconciler{Client: mgr.GetClient(), Scheme: envScheme, Resumer: calls,
			APIReader: mgr.GetAPIReader()}); err != nil {
		t.Fatalf("register robot-estop reconciler: %v", err)
	}
	mctx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(mctx) }()
	t.Cleanup(func() {
		held.release()
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the manager did not stop")
		}
	})

	// Let the Robot reconciler settle the healthy projection before the stop, so the
	// stored capability list is a real derivation: navigation Active on a healthy lidar.
	waitFor(t, "navigation derived Active before the stop", func() bool {
		return capabilityStatus(t, key, "navigation") == fleetv1.CapabilityStatusActive
	})

	if st, err := dispatcher.ClearEstop(ctx, ns, "amr-1", "alice", safety.Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop = %s, %v; want Resuming", st, err)
	}

	// ── the Robot reconciler is held; the post-clear frame reports the lidar Failed,
	//    and an operator asks for the release ──
	held.hold()
	sendHardwareFrame(t, frames, ns, "amr-1", fleetv1.HardwareFailed)
	requestAt := time.Now()
	writeResumeRequest(t, key, "first")
	waitFor(t, "the estop controller to answer the first request", func() bool { return calls.after(requestAt) >= 1 })
	if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s after a request on a frame reporting the lidar Failed, want Resuming: the "+
			"stored capabilities (navigation %s) predate that frame", got, capabilityStatus(t, key, "navigation"))
	}

	// ── the Robot reconciler catches up; the lidar recovers; nothing is requested ──
	held.release()
	waitFor(t, "navigation re-derived Inactive", func() bool {
		return capabilityStatus(t, key, "navigation") == fleetv1.CapabilityStatusInactive
	})
	sendHardwareFrame(t, frames, ns, "amr-1", fleetv1.HardwareHealthy)
	waitFor(t, "navigation re-derived Active", func() bool {
		return capabilityStatus(t, key, "navigation") == fleetv1.CapabilityStatusActive
	})
	time.Sleep(2 * time.Second) // time for any controller that would release it on its own
	if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s on healthy post-clear telemetry with no new request, want Resuming", got)
	}

	// ── a new request releases it ──
	writeResumeRequest(t, key, "second")
	waitFor(t, "the robot to leave Resuming on the second request", func() bool {
		return readEstopState(t, ns, "amr-1") == fleetv1.RobotEstopNormal
	}, func() string { return robotSummary(t, key) })
	// The counter records a confirmation after it returns, and the release is written inside it,
	// so the robot can read Normal a moment before the second confirmation is counted.
	waitFor(t, "the second confirmation to be counted", func() bool { return calls.after(time.Time{}) >= 2 })
	if n := calls.after(time.Time{}); n != 2 {
		t.Fatalf("ConfirmResume called %d times, want 2: once per operator request and at no other time", n)
	}
}

// writeResumeRequest writes an operator's release request on the Robot, as
// `swarmctl estop resume` does.
func writeResumeRequest(t *testing.T, key types.NamespacedName, value string) {
	t.Helper()
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), key, &robot); err != nil {
		t.Fatal(err)
	}
	base := robot.DeepCopy()
	if robot.Annotations == nil {
		robot.Annotations = map[string]string{}
	}
	robot.Annotations[resumeRequestKey] = value
	if err := envK8s.Patch(context.Background(), &robot, client.MergeFrom(base)); err != nil {
		t.Fatalf("write resume request: %v", err)
	}
}

func robotSummary(t *testing.T, key types.NamespacedName) string {
	t.Helper()
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), key, &robot); err != nil {
		return err.Error()
	}
	return fmt.Sprintf("estopState=%s phase=%s capabilities=%+v hardware=%+v",
		robot.Status.EstopState, robot.Status.Phase, robot.Status.Capabilities, robot.Status.Hardware)
}

// seedDerivationFixture writes the pre-stop projection the Robot reconciler derives
// from: Stopped, Idle, located, heard from, and a healthy lidar.
func seedDerivationFixture(t *testing.T, ns, name string) {
	t.Helper()
	seedResumeStatus(t, ns, name, fleetv1.RobotEstopStopped, time.Now())
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.Hardware = []fleetv1.HardwareComponentStatus{{Name: "lidar", Status: fleetv1.HardwareHealthy}}
	if err := envK8s.Status().Update(context.Background(), &robot); err != nil {
		t.Fatalf("seed hardware: %v", err)
	}
}

// sendHardwareFrame delivers one frame of the robot's own telemetry carrying a
// hardware report, through the real telemetry plane and status sink.
func sendHardwareFrame(t *testing.T, r *telemetry.Router, ns, robotID string, lidar fleetv1.HardwareStatus) {
	t.Helper()
	pct := int32(80)
	if err := r.Ingest(context.Background(), telemetry.Frame{
		RobotID: robotID, Namespace: ns, Timestamp: time.Now(), BatteryPct: &pct,
		Hardware: map[string]fleetv1.HardwareStatus{"lidar": lidar},
	}); err != nil {
		t.Fatalf("ingest frame: %v", err)
	}
}

func capabilityStatus(t *testing.T, key types.NamespacedName, name string) fleetv1.CapabilityStatus {
	t.Helper()
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), key, &robot); err != nil {
		t.Fatal(err)
	}
	for _, c := range robot.Status.Capabilities {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

func waitFor(t *testing.T, what string, ok func() bool, state ...func() string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, st := range state {
		what += "; " + st()
	}
	t.Fatalf("timed out waiting for %s", what)
}

// confirmCounter records when each confirmation returned, so the test knows the
// estop controller has answered a request before it asserts on the outcome, and can
// count that it was asked once per request.
type confirmCounter struct {
	inner ResumeConfirmer
	mu    sync.Mutex
	done  []time.Time
}

func (c *confirmCounter) ConfirmResume(ctx context.Context, ns, id, by string) (fleetv1.RobotEstopState, string, error) {
	st, why, err := c.inner.ConfirmResume(ctx, ns, id, by)
	c.mu.Lock()
	c.done = append(c.done, time.Now())
	c.mu.Unlock()
	return st, why, err
}

func (c *confirmCounter) after(t0 time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, at := range c.done {
		if at.After(t0) {
			n++
		}
	}
	return n
}

// reconcileGate holds a reconciler's work while closed, the way a slow or busy
// reconciler would, without changing what it does once it runs.
type reconcileGate struct {
	mu   sync.Mutex
	open chan struct{}
}

func newReconcileGate() *reconcileGate {
	g := &reconcileGate{open: make(chan struct{})}
	close(g.open)
	return g
}

func (g *reconcileGate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
		g.open = make(chan struct{})
	default:
	}
}

func (g *reconcileGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
	default:
		close(g.open)
	}
}

func (g *reconcileGate) wrap(r reconcile.Reconciler) reconcile.Reconciler {
	return reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
		g.mu.Lock()
		open := g.open
		g.mu.Unlock()
		<-open
		return r.Reconcile(ctx, req)
	})
}

// wireResumeDerivation gives the dispatcher the derivation cmd/manager gives it.
func wireResumeDerivation(d *safety.Dispatcher) { d.DeriveCapabilities = DeriveCapabilities }
