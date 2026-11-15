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
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Two actions wait for one robot: an older Low one and a newer Normal one. When the robot becomes
// free (here, its stop clears), the Normal one is assigned it, and the Low one never records it,
// not even for a moment.
//
// The wake enqueues the Normal action first, and with one worker it is reconciled first and
// commits the robot. The Low action is reconciled a moment later, usually before the cache has
// seen that commit; it re-reads the robot past the cache (currentCandidate), finds it taken, and
// stays Pending. Without that read it would write itself Assigned, lose the robot commit and
// revert, and a watch on it sees the Assigned version.
func TestEnvtest_AFreedRobotGoesToTheMoreUrgentActionAndTheOtherNeverRecordsIt(t *testing.T) {
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
	// Held by a stop while the actions wait, so each waits with the message "under an active
	// emergency stop"; after the robot is freed and given to the Normal action, the Low action's
	// next reconcile must say "not Idle", which shows it ran after the robot was taken.
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("stop the robot: %v", err)
	}

	startActionManager(t, ns)
	low := seedActionOfPriority(t, ns, "low-old", "aisle-1", fleetv1.ActionPriorityLow)
	time.Sleep(1100 * time.Millisecond) // creationTimestamp has one-second resolution
	normal := zrSeedAction(t, ns, "normal-new", "aisle-1")
	for _, req := range []struct {
		name string
		get  func() *fleetv1.FleetAction
	}{{"low-old", func() *fleetv1.FleetAction { return zrGetAction(t, low) }},
		{"normal-new", func() *fleetv1.FleetAction { return zrGetAction(t, normal) }}} {
		waitFor(t, req.name+" to wait for a robot", func() bool {
			a := req.get()
			return (a.Status.Phase == "" || a.Status.Phase == fleetv1.ActionPhasePending) &&
				strings.Contains(a.Status.Message, "emergency stop")
		})
	}
	if a, b := zrGetAction(t, low), zrGetAction(t, normal); a.Status.AssignedRobot != "" || b.Status.AssignedRobot != "" {
		t.Fatalf("an action was assigned before the robot was free; the fixture is wrong")
	}
	// Each action's status write enqueues it once more. An action still being reconciled when the
	// robot is freed read the robot before the wake; the wake's Add only marks it to run again,
	// after the actions the wake queued (see pendingActionsFor). The order under test is the
	// wake's, so the robot is freed once the controller is idle.
	waitForIdleController(t, "fleetaction")

	// Every version of the Low action from here on.
	direct, err := client.NewWithWatch(envCfg, client.Options{Scheme: envScheme})
	if err != nil {
		t.Fatal(err)
	}
	w, err := direct.Watch(ctx, &fleetv1.FleetActionList{}, client.InNamespace(ns),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("metadata.name", "low-old")})
	if err != nil {
		t.Fatalf("watch the Low action: %v", err)
	}
	defer w.Stop()
	var mu sync.Mutex
	var lowRecorded []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range w.ResultChan() {
			if ev.Type != watch.Modified {
				continue
			}
			if a, ok := ev.Object.(*fleetv1.FleetAction); ok && a.Status.AssignedRobot != "" {
				mu.Lock()
				lowRecorded = append(lowRecorded, a.Status.AssignedRobot+" ("+string(a.Status.Phase)+")")
				mu.Unlock()
			}
		}
	}()

	if err := envK8s.Get(ctx, key, robot); err != nil {
		t.Fatal(err)
	}
	robot.Status.EstopState = fleetv1.RobotEstopNormal
	freed := time.Now()
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("free the robot: %v", err)
	}
	requireAssignedSoon(t, normal, "amr-1", freed, "its robot becoming free")
	// The Low action's reconcile follows at once; it has run once its message says the robot is
	// no longer Idle (any assignment and revert it made would come before that).
	waitFor(t, "the Low action to be reconciled after the robot was taken", func() bool {
		return strings.Contains(zrGetAction(t, low).Status.Message, "not Idle")
	})
	w.Stop()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(lowRecorded) > 0 {
		t.Fatalf("the Low action recorded the robot the Normal one was given: %v", lowRecorded)
	}
	if a := zrGetAction(t, low); a.Status.AssignedRobot != "" || a.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("the Low action is %s on %q, want Pending on no robot", a.Status.Phase, a.Status.AssignedRobot)
	}
}

// waitForIdleController waits until the controller's workqueue is empty and no worker is
// reconciling, and has stayed so for 200 ms.
func waitForIdleController(t *testing.T, name string) {
	t.Helper()
	gauge := func(metric, label string) float64 {
		mfs, err := ctrlmetrics.Registry.Gather()
		if err != nil {
			t.Fatalf("gather metrics: %v", err)
		}
		for _, mf := range mfs {
			if mf.GetName() != metric {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == label && lp.GetValue() == name {
						return m.GetGauge().GetValue()
					}
				}
			}
		}
		return 0
	}
	var quietSince time.Time
	waitFor(t, "the "+name+" controller to be idle", func() bool {
		if gauge("workqueue_depth", "name") != 0 || gauge("controller_runtime_active_workers", "controller") != 0 {
			quietSince = time.Time{}
			return false
		}
		if quietSince.IsZero() {
			quietSince = time.Now()
		}
		return time.Since(quietSince) >= 200*time.Millisecond
	})
}

func seedActionOfPriority(t *testing.T, ns, name, zone string, p fleetv1.ActionPriority) ctrl.Request {
	t.Helper()
	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: zone, Priority: p},
	}
	if err := envK8s.Create(context.Background(), a); err != nil {
		t.Fatalf("create action %s: %v", name, err)
	}
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
}
