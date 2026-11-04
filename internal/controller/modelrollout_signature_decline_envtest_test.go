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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/command"
)

// lockedPusher and sigAudit are the fake-client doubles made safe for a manager,
// which reconciles on its own goroutines.
type lockedPusher struct {
	mu      sync.Mutex
	message string
	pushes  int
}

func (p *lockedPusher) PushModelUpdate(_ context.Context, _, _ string, _ command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushes++
	return command.ModelUpdateOutcome{Acknowledged: false, Message: p.message}, nil
}

func (p *lockedPusher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pushes
}

type sigAudit struct {
	mu    sync.Mutex
	inner recordingAudit
}

func (a *sigAudit) Record(e audit.Entry) (audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inner.Record(e)
}

func (a *sigAudit) count(eventType string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.inner.ofType(eventType))
}

// rolloutsInNamespace is SetupWithManager's Robot → ModelRollout mapping, for a controller
// registered under a per-test name.
func rolloutsInNamespace(mgr manager.Manager, ns string) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list fleetv1.ModelRolloutList
		if err := mgr.GetClient().List(ctx, &list, client.InNamespace(ns)); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: ns, Name: list.Items[i].Name}})
		}
		return out
	})
}

// A signature refusal in a real manager, where Robot events re-queue the rollout on
// their own schedule: the robot is failed once, the rollout pauses, and nothing about
// the refusal repeats however often the rollout is reconciled afterwards.
func TestEnvtest_ModelRollout_SignatureDeclineIsTerminalInAManager(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	robot := envtestValidRobot(ns, "r1", "z-a")
	robot.Labels = map[string]string{"fleet": "pickers"}
	robot.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "cam-front", Type: fleetv1.HardwareTypeCamera}}
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	battery := int32(90)
	robot.Status = fleetv1.RobotStatus{
		Phase: fleetv1.RobotPhaseIdle, BatteryPercent: &battery,
		Hardware: []fleetv1.HardwareComponentStatus{{Name: "cam-front", Status: fleetv1.HardwareHealthy}},
	}
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed robot status: %v", err)
	}

	pusher := &lockedPusher{message: sigDecline}
	aud := &sigAudit{}
	events := record.NewFakeRecorder(1000)
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Scheme:  envScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).For(&fleetv1.ModelRollout{}).Named("modelrollout-"+ns).
		Watches(&fleetv1.Robot{}, rolloutsInNamespace(mgr, ns)).
		Complete(&ModelRolloutReconciler{Client: mgr.GetClient(), Scheme: envScheme,
			Pusher: pusher, Audit: aud, Recorder: events, APIReader: mgr.GetAPIReader()}); err != nil {
		t.Fatalf("register rollout reconciler: %v", err)
	}
	mctx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(mctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the manager did not stop")
		}
	})

	ro := pickerRollout()
	ro.Namespace = ns
	ro.Spec.ModelChecksum = testChecksum
	if err := envK8s.Create(ctx, ro); err != nil {
		t.Fatalf("create rollout: %v", err)
	}
	key := types.NamespacedName{Namespace: ns, Name: ro.Name}
	status := func() fleetv1.ModelRolloutStatus {
		var got fleetv1.ModelRollout
		if err := envK8s.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	waitFor(t, "the rollout to pause on the refused robot", func() bool {
		st := status()
		return st.Phase == fleetv1.RolloutPhasePaused && st.RobotsFailed == 1
	}, func() string { return fmt.Sprintf("status=%+v pushes=%d", status(), pusher.count()) })

	// Re-queue the rollout through its Robot watch, repeatedly, as telemetry would.
	for i := 0; i < 5; i++ {
		var r fleetv1.Robot
		if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "r1"}, &r); err != nil {
			t.Fatal(err)
		}
		b := int32(80 - i)
		r.Status.BatteryPercent = &b
		if err := envK8s.Status().Update(ctx, &r); err != nil {
			t.Fatalf("touch robot: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if n := pusher.count(); n != 1 {
		t.Errorf("the refused artifact was pushed %d times; want 1", n)
	}
	if n := aud.count(audit.EventModelSignatureFailed); n != 1 {
		t.Errorf("MODEL_SIGNATURE_FAILED sealed %d times; want 1", n)
	}
	if n := aud.count(audit.EventModelRolloutPaused); n != 1 {
		t.Errorf("MODEL_ROLLOUT_PAUSED sealed %d times; want 1", n)
	}
	if n := countEvents(events, "ModelRolloutPushDeclined"); n != 1 {
		t.Errorf("ModelRolloutPushDeclined emitted %d times; want 1", n)
	}
	if st := status(); st.Phase != fleetv1.RolloutPhasePaused || len(st.FailedRobots) != 1 {
		t.Errorf("want the rollout still Paused with the robot failed, got %+v", st)
	}
}
