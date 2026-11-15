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

// Does a robot that becomes free wake the action waiting for it, at once?
//
// A manager runs the FleetAction controller as cmd/manager wires it (SetupWithManager), and
// nothing else: no FleetAdapter writes, so no heartbeat re-enqueues the action, and its own
// recheck (pendingRecheckInterval, 15 s) is far off. An action waits Pending because its only
// robot is not free. The robot becomes free (each way a robot does), and the action must be
// assigned well before its recheck: only the robot's event can have woken it.

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
)

// wakeAssignWithin is how soon a woken action must be assigned: far under the 15 s recheck, and
// above the 0.3 s target only by what a loaded runner needs. The latency itself is logged.
const wakeAssignWithin = time.Second

func startActionManager(t *testing.T, ns string) {
	t.Helper()
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:  envScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	// The TDE engine and its recovery, as cmd/manager wires them: until it has recovered, the
	// engine admits nothing and every assignment waits on "zone capacity".
	engine := tde.New(mgr.GetClient(), tde.DefaultConfig())
	if err := mgr.Add(&tde.RecoveryRunnable{
		Engine: engine, Client: mgr.GetClient(), Mode: tde.RecoverValidate,
		Ready: func() bool { return true }, ReadyTimeout: time.Second,
		ReadyPollInterval: 20 * time.Millisecond, Fallback: tde.RecoverReleaseAll,
		RetryInterval: 50 * time.Millisecond, Log: logr.Discard(),
	}); err != nil {
		t.Fatalf("add the TDE recovery runnable: %v", err)
	}
	r := &FleetActionReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), TDE: engine, Commander: &fakeCommander{},
		Recorder: record.NewFakeRecorder(256)}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("register the FleetAction controller: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the manager did not stop")
		}
	})
	waitFor(t, "the TDE engine to recover", engine.Recovered)
}

func TestEnvtest_ARobotThatBecomesFreeWakesTheActionWaitingForIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		notFree func(*fleetv1.Robot) // the robot's state while the action waits
	}{
		{"its action is done and it goes Idle", func(r *fleetv1.Robot) {
			r.Status.Phase, r.Status.AssignedAction = fleetv1.RobotPhaseAssigned, "pick-0"
		}},
		{"its stop clears through to Normal", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopResuming }},
		{"it comes back from Offline after an adapter fault", func(r *fleetv1.Robot) { r.Status.Phase = fleetv1.RobotPhaseOffline }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireEnvtest(t)
			ctx := context.Background()
			ns := envtestNamespace(t)
			f12SeedAdapter(t, ns)
			zrSeedZone(t, ns, "aisle-1", "", nil, nil, true)
			f12SeedRobot(t, ns, "amr-1", "aisle-1")
			robot := &fleetv1.Robot{}
			key := client.ObjectKey{Namespace: ns, Name: "amr-1"}
			if err := envK8s.Get(ctx, key, robot); err != nil {
				t.Fatal(err)
			}
			tc.notFree(robot)
			if err := envK8s.Status().Update(ctx, robot); err != nil {
				t.Fatalf("make the robot not free: %v", err)
			}

			startActionManager(t, ns)
			req := zrSeedAction(t, ns, "pick-1", "aisle-1")
			// It waits: the controller has seen it and found no robot (its message says so).
			waitFor(t, "the action to wait for a robot", func() bool {
				a := zrGetAction(t, req)
				return (a.Status.Phase == "" || a.Status.Phase == fleetv1.ActionPhasePending) && a.Status.Message != ""
			})
			if a := zrGetAction(t, req); a.Status.AssignedRobot != "" {
				t.Fatalf("the action was assigned to %q before its robot was free; the fixture is wrong", a.Status.AssignedRobot)
			}

			if err := envK8s.Get(ctx, key, robot); err != nil {
				t.Fatal(err)
			}
			robot.Status.Phase, robot.Status.AssignedAction = fleetv1.RobotPhaseIdle, ""
			robot.Status.EstopState = fleetv1.RobotEstopNormal
			freed := time.Now()
			if err := envK8s.Status().Update(ctx, robot); err != nil {
				t.Fatalf("free the robot: %v", err)
			}
			requireAssignedSoon(t, req, "amr-1", freed, "its robot becoming free")
		})
	}
}

// requireAssignedSoon fails unless the action is assigned to robot within wakeAssignWithin of since.
func requireAssignedSoon(t *testing.T, req ctrl.Request, robot string, since time.Time, what string) {
	t.Helper()
	deadline := since.Add(wakeAssignWithin)
	for {
		a := zrGetAction(t, req)
		if a.Status.AssignedRobot == robot {
			t.Logf("assigned %v after %s", time.Since(since).Round(time.Millisecond), what)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the action was not assigned within %v of %s (phase %q, message %q); "+
				"nothing but its own %v recheck would reach it", wakeAssignWithin, what, a.Status.Phase, a.Status.Message,
				pendingRecheckInterval)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A free robot moving into an action's zone wakes the action waiting there.
func TestEnvtest_AFreeRobotMovingIntoTheZoneWakesTheActionWaitingThere(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "aisle-1", "", nil, nil, true)
	zrSeedZone(t, ns, "aisle-2", "", nil, nil, true)
	f12SeedRobot(t, ns, "amr-1", "aisle-2") // free, but in the other zone

	startActionManager(t, ns)
	req := zrSeedAction(t, ns, "pick-1", "aisle-1")
	waitFor(t, "the action to wait for a robot in its zone", func() bool {
		a := zrGetAction(t, req)
		return (a.Status.Phase == "" || a.Status.Phase == fleetv1.ActionPhasePending) && a.Status.Message != ""
	})
	if a := zrGetAction(t, req); a.Status.AssignedRobot != "" {
		t.Fatalf("the action was assigned to %q while its robot was in another zone; the fixture is wrong", a.Status.AssignedRobot)
	}

	robot := &fleetv1.Robot{}
	if err := envK8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "amr-1"}, robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.CurrentZone = "aisle-1"
	moved := time.Now()
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("move the robot: %v", err)
	}
	requireAssignedSoon(t, req, "amr-1", moved, "its robot moving into the zone")
}
