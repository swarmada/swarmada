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

// Does deleting a Robot free the lift it holds, without a restart, and is the next robot told?
//
// A manager runs the Robot controller and the TDE recovery runnable as cmd/manager wires
// them, with the engine's grant notifier recording pushes. r1 holds a capacity-1 lift and r2
// waits behind it. The operator deletes r1's Robot. RFC-0001 #tde-normative-invariants
// (TDE-7) now says the hold is released and the promoted waiter told at once. A second case
// covers a Robot deleted while no manager runs: the recovery that follows promotes r2 and
// must tell it too.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/tde"
)

// grantRecorder records the reservation_granted pushes the engine makes.
type grantRecorder struct {
	mu     sync.Mutex
	grants []string // robot/resource
}

func (g *grantRecorder) notify(_ context.Context, _, robotID, resourceName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.grants = append(g.grants, robotID+"/"+resourceName)
}

func (g *grantRecorder) got() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.grants...)
}

// startHoldManager starts a manager scoped to ns running the Robot controller wired to the
// engine, as cmd/manager wires it, and the recovery runnable. It returns once the engine
// has recovered.
func startHoldManager(t *testing.T, ns string, grants *grantRecorder) *tdeManager {
	t.Helper()
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:  envScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	engine := tde.New(mgr.GetClient(), tde.DefaultConfig()).WithGrantNotifier(grants.notify)
	robots := &RobotReconciler{Client: mgr.GetClient(), Scheme: envScheme, Holds: engine}
	if err := ctrl.NewControllerManagedBy(mgr).For(&fleetv1.Robot{}).Named("robot-" + ns).Complete(robots); err != nil {
		t.Fatalf("register robot reconciler: %v", err)
	}
	if err := mgr.Add(&tde.RecoveryRunnable{
		Engine: engine, Client: mgr.GetClient(), Mode: tde.RecoverValidate,
		Ready: func() bool { return true }, ReadyTimeout: time.Second,
		ReadyPollInterval: 20 * time.Millisecond, Fallback: tde.RecoverReleaseAll,
		RetryInterval: 50 * time.Millisecond, Log: logr.Discard(),
	}); err != nil {
		t.Fatalf("add recovery runnable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the manager did not stop")
		}
	}
	t.Cleanup(stop)
	waitFor(t, "the TDE engine to recover", engine.Recovered)
	return &tdeManager{engine: engine, stop: stop}
}

// seedHeldLift creates the lift zone and robots r1 and r2, with r1 holding the lift and r2
// queued behind it, through m's engine.
func seedHeldLift(t *testing.T, ns string, m func() *tdeManager) *tdeManager {
	t.Helper()
	ctx := context.Background()
	zone := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "shaft", Namespace: ns},
		Spec: fleetv1.FleetZoneSpec{SharedResources: []fleetv1.SharedResource{{
			Name: "lift", Type: fleetv1.SharedResourceElevator, Capacity: 1,
			ReservationPolicy: fleetv1.ReservationFIFO,
		}}},
	}
	if err := envK8s.Create(ctx, zone); err != nil {
		t.Fatalf("create zone: %v", err)
	}
	f12SeedRobot(t, ns, "r1", "shaft")
	f12SeedRobot(t, ns, "r2", "shaft")
	mgr := m()
	if out, err := mgr.engine.ReserveResource(ctx, ns, "lift", "r1"); err != nil || out.State != tde.ResourceGranted {
		t.Fatalf("r1 reserve = %+v, %v; want Granted", out, err)
	}
	if out, _ := mgr.engine.ReserveResource(ctx, ns, "lift", "r2"); out.State != tde.ResourceQueued {
		t.Fatalf("r2 reserve = %s; want Queued behind r1", out.State)
	}
	waitFor(t, "r1 holding and r2 queued to be persisted", func() bool {
		h := liftHolders(t, ns)
		return len(h) == 1 && h[0] == "r1"
	})
	return mgr
}

func TestEnvtest_DeletingARobotReleasesItsHoldAndTellsTheWaiter(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	grants := &grantRecorder{}
	mgr := seedHeldLift(t, ns, func() *tdeManager { return startHoldManager(t, ns, grants) })

	if err := envK8s.Delete(ctx, &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "r1"}}); err != nil {
		t.Fatalf("delete r1: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h := liftHolders(t, ns); len(h) == 1 && h[0] == "r2" && len(grants.got()) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h := liftHolders(t, ns); len(h) != 1 || h[0] != "r2" {
		t.Errorf("lift holders after r1's Robot was deleted, with no restart = %v; want [r2]", h)
	}
	if out, _ := mgr.engine.ReserveResource(ctx, ns, "lift", "r2"); out.State != tde.ResourceGranted {
		t.Errorf("r2 after r1's Robot was deleted = %s; want Granted", out.State)
	}
	if got := grants.got(); len(got) != 1 || got[0] != "r2/lift" {
		t.Errorf("reservation_granted pushes = %v; want exactly [r2/lift]", got)
	}
}

func TestEnvtest_RecoveryTellsTheWaiterItPromotes(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	first := seedHeldLift(t, ns, func() *tdeManager { return startTDEManager(t, ns, func() bool { return true }, nil) })

	// r1's Robot is deleted while no manager runs, so nothing sees the deletion until the
	// next recovery.
	first.stop()
	if err := envK8s.Delete(ctx, &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "r1"}}); err != nil {
		t.Fatalf("delete r1: %v", err)
	}
	grants := &grantRecorder{}
	startHoldManager(t, ns, grants)
	if h := liftHolders(t, ns); len(h) != 1 || h[0] != "r2" {
		t.Errorf("lift holders after recovery = %v; want [r2]", h)
	}
	if got := grants.got(); len(got) != 1 || got[0] != "r2/lift" {
		t.Errorf("reservation_granted pushes after recovery promoted r2 = %v; want exactly [r2/lift]", got)
	}
}
