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

// Does a capacity-1 shared resource stay single-held across a manager restart?
//
// An adapter reserves a lift for its robot (ReservationRequest → Engine.ReserveResource)
// and is granted it. The manager then restarts: a new process, a new engine, and the
// leader-elected recovery runnable rebuilding state from FleetZone.status, exactly as
// cmd/manager wires it. The holder's adapter is told nothing, so its robot is still in
// the lift. A second robot then asks for the same lift. RFC-0001 #tde-normative-invariants
// (TDE-5) says it must not be granted while the first holds it.

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/tde"
)

// tdeManager is one manager process's worth of TDE: its own engine, recovered by the
// same runnable cmd/manager adds. Stop ends the process.
type tdeManager struct {
	engine *tde.Engine
	stop   func()
}

// startTDEManager starts a manager scoped to ns (so recovery touches no other test's
// zones) and waits until its engine has recovered and opened the grant gate. tune, when
// non-nil, adjusts the recovery runnable before it is added.
func startTDEManager(t *testing.T, ns string, ready func() bool, tune func(*tde.RecoveryRunnable)) *tdeManager {
	t.Helper()
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:  envScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	engine := tde.New(mgr.GetClient(), tde.DefaultConfig())
	runnable := &tde.RecoveryRunnable{
		Engine:            engine,
		Client:            mgr.GetClient(),
		Mode:              tde.RecoverValidate,
		Ready:             ready,
		ReadyTimeout:      time.Second,
		ReadyPollInterval: 20 * time.Millisecond,
		Fallback:          tde.RecoverReleaseAll,
		RetryInterval:     50 * time.Millisecond,
		Log:               logr.Discard(),
	}
	if tune != nil {
		tune(runnable)
	}
	if err := mgr.Add(runnable); err != nil {
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

// liftHolders reads the lift's persisted holders from FleetZone.status.
func liftHolders(t *testing.T, ns string) []string {
	t.Helper()
	var fz fleetv1.FleetZone
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "shaft"}, &fz); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	var ids []string
	for _, q := range fz.Status.SharedResourceQueues {
		if q.ResourceName != "lift" {
			continue
		}
		for _, h := range q.CurrentHolders {
			ids = append(ids, h.RobotID)
		}
	}
	return ids
}

func TestEnvtest_TDERestart_CapacityOneResourceIsNotGrantedTwice(t *testing.T) {
	requireEnvtest(t)
	for _, tc := range []struct {
		name  string
		ready func() bool // the Zone Controller's readiness, as the runnable sees it
	}{
		// The Zone Controller comes up: RecoverValidate, the production path.
		{"validate", func() bool { return true }},
		// It does not come up in time: the conservative fallback, ReleaseAll.
		{"fallback-release-all", func() bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ns := envtestNamespace(t)

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
			// Both robots are where the lift is: r1 because it is riding it, r2 waiting.
			f12SeedRobot(t, ns, "r1", "shaft")
			f12SeedRobot(t, ns, "r2", "shaft")

			first := startTDEManager(t, ns, tc.ready, nil)
			if out, err := first.engine.ReserveResource(ctx, ns, "lift", "r1"); err != nil || out.State != tde.ResourceGranted {
				t.Fatalf("r1 reserve before restart = %+v, %v; want Granted", out, err)
			}
			waitFor(t, "r1's hold to be persisted", func() bool {
				h := liftHolders(t, ns)
				return len(h) == 1 && h[0] == "r1"
			})
			if out, _ := first.engine.ReserveResource(ctx, ns, "lift", "r2"); out.State != tde.ResourceQueued {
				t.Fatalf("r2 reserve before restart = %s; want Queued behind r1", out.State)
			}
			// Take r2 back out of the queue so the restart is judged on the holder alone.
			if _, err := first.engine.ReleaseResource(ctx, ns, "lift", "r2"); err != nil {
				t.Fatalf("r2 leaves the queue: %v", err)
			}

			// The restart. r1's adapter is told nothing and r1 has not released the lift.
			first.stop()
			second := startTDEManager(t, ns, tc.ready, nil)

			t.Logf("persisted lift holders after recovery: %v", liftHolders(t, ns))
			out, err := second.engine.ReserveResource(ctx, ns, "lift", "r2")
			if err != nil {
				t.Fatalf("r2 reserve after restart: %v", err)
			}
			t.Logf("r2 reserve after restart: %s (queue position %d)", out.State, out.QueuePosition)
			if out.State == tde.ResourceGranted {
				t.Errorf("capacity-1 lift granted to r2 after a manager restart while r1 still holds it "+
					"(r1 was granted it before the restart and never released it); holders now %v",
					liftHolders(t, ns))
			}
			// r1 itself is still recognised as the holder by the new process.
			if again, _ := second.engine.ReserveResource(ctx, ns, "lift", "r1"); again.State != tde.ResourceGranted {
				t.Errorf("r1 re-reserve after restart = %s; the holder should still hold", again.State)
			}

			// r1 never comes back. The operator deletes its Robot, and the next recovery
			// releases r1's hold and promotes r2, which queued behind it above.
			if err := envK8s.Delete(ctx, &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "r1"}}); err != nil {
				t.Fatalf("delete r1: %v", err)
			}
			second.stop()
			third := startTDEManager(t, ns, tc.ready, nil)
			if h := liftHolders(t, ns); len(h) != 1 || h[0] != "r2" {
				t.Errorf("lift holders after r1's Robot was deleted and the manager restarted = %v; want [r2]", h)
			}
			if out, _ := third.engine.ReserveResource(ctx, ns, "lift", "r2"); out.State != tde.ResourceGranted {
				t.Errorf("r2 after r1's Robot was deleted = %s; want Granted", out.State)
			}
		})
	}
}
