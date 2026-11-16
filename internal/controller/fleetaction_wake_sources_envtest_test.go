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

// The FleetAdapter heartbeat no longer re-enqueues the waiting actions, and each change it used to
// cover wakes them where it happens. Each test runs the FleetAction controller as cmd/manager wires
// it (startActionManager) with nothing else writing: an action waits on one gate, the gate lifts,
// and the action must be assigned within wakeAssignWithin, far inside its own recheck (15 s; the
// TDE's 10-120 s).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

const wakeAdapter = "acme-adapter"

func editRobotStatus(t *testing.T, ns, name string, edit func(*fleetv1.Robot)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		r := &fleetv1.Robot{}
		if err := envK8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, r); err != nil {
			return err
		}
		edit(r)
		return envK8s.Status().Update(context.Background(), r)
	})
	if err != nil {
		t.Fatalf("edit robot %s status: %v", name, err)
	}
}

func editAdapterStatus(t *testing.T, ns string, edit func(*fleetv1.FleetAdapter)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a := &fleetv1.FleetAdapter{}
		if err := envK8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: wakeAdapter}, a); err != nil {
			return err
		}
		edit(a)
		return envK8s.Status().Update(context.Background(), a)
	})
	if err != nil {
		t.Fatalf("edit adapter status: %v", err)
	}
}

func editObject(t *testing.T, obj client.Object, edit func()) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := envK8s.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			return err
		}
		edit()
		return envK8s.Update(context.Background(), obj)
	})
	if err != nil {
		t.Fatalf("edit %s: %v", obj.GetName(), err)
	}
}

func seedWaitingAction(t *testing.T, ns, name, zone string, edit func(*fleetv1.FleetAction)) ctrl.Request {
	t.Helper()
	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: zone, Priority: fleetv1.ActionPriorityNormal},
	}
	if edit != nil {
		edit(a)
	}
	if err := envK8s.Create(context.Background(), a); err != nil {
		t.Fatalf("create action %s: %v", name, err)
	}
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

// waitWaiting waits until the action has been reconciled and left waiting, with a message that
// contains want (any message when want is empty).
func waitWaiting(t *testing.T, req ctrl.Request, want string) {
	t.Helper()
	waitFor(t, req.Name+" to wait ("+want+")", func() bool {
		a := zrGetAction(t, req)
		return (a.Status.Phase == "" || a.Status.Phase == fleetv1.ActionPhasePending) && a.Status.Message != "" &&
			strings.Contains(a.Status.Message, want)
	}, func() string { return zrGetAction(t, req).Status.Message })
	if a := zrGetAction(t, req); a.Status.AssignedRobot != "" {
		t.Fatalf("%s was assigned to %q while it should wait; the fixture is wrong", req.Name, a.Status.AssignedRobot)
	}
	// The action's own status write enqueues it once more; the gate is lifted only once that
	// reconcile has run too, so that nothing but the wake under test can reach it.
	waitForIdleController(t, "fleetaction")
}

func fleetActionReconciles(t *testing.T) float64 {
	t.Helper()
	mfs, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() != "controller_runtime_reconcile_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "controller" && lp.GetValue() == "fleetaction" {
					n += m.GetCounter().GetValue()
				}
			}
		}
	}
	return n
}

// seedWakeFleet seeds the adapter, zone aisle-1 and the named robots in it.
func seedWakeFleet(t *testing.T, ns string, robots ...string) {
	t.Helper()
	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "aisle-1", "", nil, nil, true)
	for _, r := range robots {
		f12SeedRobot(t, ns, r, "aisle-1")
	}
}

// 1. Heartbeats alone reconcile no waiting action.
func TestEnvtest_AdapterHeartbeatsReconcileNoWaitingAction(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	// Offline, so the action waits, and recording no action (a record of one that does not exist
	// would be reconciled on its own, as an orphan).
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { r.Status.Phase = fleetv1.RobotPhaseOffline })
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", nil)
	waitWaiting(t, req, "")
	before := fleetActionReconciles(t)
	for range 10 {
		editAdapterStatus(t, ns, func(a *fleetv1.FleetAdapter) { a.Status.LastHeartbeat = &metav1.Time{Time: time.Now()} })
		time.Sleep(200 * time.Millisecond)
	}
	waitForIdleController(t, "fleetaction")
	if d := fleetActionReconciles(t) - before; d > 1 {
		t.Fatalf("10 heartbeats caused %.0f FleetAction reconciles, want at most 1", d)
	}
}

// 2. An adapter that recovers gets the action waiting for its robot assigned.
func TestEnvtest_AnAdapterRecoveringWakesTheActionWaitingForIt(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	editAdapterStatus(t, ns, func(a *fleetv1.FleetAdapter) { a.Status.Phase = fleetv1.FleetAdapterPhaseDisconnected })
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", nil)
	waitWaiting(t, req, "")
	since := time.Now()
	editAdapterStatus(t, ns, func(a *fleetv1.FleetAdapter) {
		a.Status.Phase = fleetv1.FleetAdapterPhaseConnected
		a.Status.LastHeartbeat = &metav1.Time{Time: time.Now()}
	})
	requireAssignedSoon(t, req, "amr-1", since, "its adapter reconnecting")
}

// 3. A free robot's battery rising past the action's floor.
func TestEnvtest_ABatteryRisingPastTheFloorWakesTheAction(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { v := int32(30); r.Status.BatteryPercent = &v })
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", func(a *fleetv1.FleetAction) { v := int32(60); a.Spec.MinBatteryPct = &v })
	waitWaiting(t, req, "battery")
	since := time.Now()
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { v := int32(70); r.Status.BatteryPercent = &v })
	requireAssignedSoon(t, req, "amr-1", since, "its robot's battery rising past the floor")
}

// 4. A free robot's capability going Degraded → Active, for an action that refuses Degraded.
func TestEnvtest_ACapabilityGoingActiveWakesTheAction(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) {
		r.Status.Capabilities = []fleetv1.CapabilityStatusEntry{{Name: "lift", Status: fleetv1.CapabilityStatusDegraded}}
	})
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", func(a *fleetv1.FleetAction) { a.Spec.RequiredCapabilities = []string{"lift"} })
	waitWaiting(t, req, "capability")
	since := time.Now()
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { r.Status.Capabilities[0].Status = fleetv1.CapabilityStatusActive })
	requireAssignedSoon(t, req, "amr-1", since, "its robot's capability going Active")
}

// 5. A free robot's model update ending, for an action needing the capability the model grants.
func TestEnvtest_AModelUpdateEndingWakesTheAction(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) {
		r.Status.Capabilities = []fleetv1.CapabilityStatusEntry{{Name: "scan", Status: fleetv1.CapabilityStatusActive}}
		r.Status.ModelGrantedCapabilities = []fleetv1.ModelGrantedCapabilityEntry{
			{ModelName: "scanner", GrantedBy: "rollout-1", Capabilities: []string{"scan"}}}
		r.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{Name: "scanner", Status: fleetv1.ModelStatusUpdating}}
	})
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", func(a *fleetv1.FleetAction) { a.Spec.RequiredCapabilities = []string{"scan"} })
	waitWaiting(t, req, "model")
	since := time.Now()
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { r.Status.InstalledModels[0].Status = fleetv1.ModelStatusActive })
	requireAssignedSoon(t, req, "amr-1", since, "its robot's model update ending")
}

// 6. A zone's stop held through its processed marker, then cleared: the action whose destination is
// there (row A15), on the free robot there that holds no record of the stop.
func TestEnvtest_AZoneStopClearedThroughItsMarkerWakesTheAction(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "aisle-1"}}
	editObject(t, zone, func() { zone.Annotations = map[string]string{estopscope.ProcessedAnnotation: "stop-1"} })
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", nil)
	// Row A15 holds it with the condition ZoneStopped (it writes no message).
	waitFor(t, "pick-1 to be held for its destination", func() bool {
		a := zrGetAction(t, req)
		return a.Status.AssignedRobot == "" && apimeta.IsStatusConditionTrue(a.Status.Conditions, conditionZoneStopped)
	})
	waitForIdleController(t, "fleetaction")
	since := time.Now()
	editObject(t, zone, func() { delete(zone.Annotations, estopscope.ProcessedAnnotation) })
	requireAssignedSoon(t, req, "amr-1", since, "its zone's stop clearing")
}

// 7. A maintenance window ending that kept its robot Idle throughout.
func TestEnvtest_AMaintenanceWindowEndingWakesTheAction(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	zm := &fleetv1.ZoneMaintenance{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "mw"},
		Spec: fleetv1.ZoneMaintenanceSpec{Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "aisle-1"}}}
	if err := envK8s.Create(ctx, zm); err != nil {
		t.Fatalf("create window: %v", err)
	}
	zm.Status.Phase = fleetv1.ZoneMaintenancePhaseActive
	zm.Status.PausedRobots = []fleetv1.PausedRobotEntry{{Name: "amr-1", Namespace: ns, PausedAt: metav1.Now()}}
	if err := envK8s.Status().Update(ctx, zm); err != nil {
		t.Fatalf("activate window: %v", err)
	}
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", nil)
	waitWaiting(t, req, "maintenance")
	since := time.Now()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := envK8s.Get(ctx, client.ObjectKeyFromObject(zm), zm); err != nil {
			return err
		}
		zm.Status.Phase = fleetv1.ZoneMaintenancePhaseCompleted
		zm.Status.PausedRobots = nil
		return envK8s.Status().Update(ctx, zm)
	}); err != nil {
		t.Fatalf("complete window: %v", err)
	}
	requireAssignedSoon(t, req, "amr-1", since, "its window ending")
}

// 8. A capped zone's slot freed by the action holding it ending: the action awaiting capacity.
func TestEnvtest_AZoneSlotFreedWakesTheActionAwaitingCapacity(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1", "amr-2")
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "aisle-1"}}
	editObject(t, zone, func() { zone.Spec.MaxConcurrentRobots = 1 })
	startActionManager(t, ns)
	// Each action is pinned to its own robot, so the first one's robot coming free cannot wake the
	// second: only the freed slot can.
	first := seedWaitingAction(t, ns, "first-1", "aisle-1", func(a *fleetv1.FleetAction) { a.Spec.RobotSelector = "amr-1" })
	waitFor(t, "the first action to take the zone's slot", func() bool { return zrGetAction(t, first).Status.AssignedRobot != "" })
	second := seedWaitingAction(t, ns, "second-2", "aisle-1", func(a *fleetv1.FleetAction) { a.Spec.RobotSelector = "amr-2" })
	waitWaiting(t, second, awaitingZoneCapacity)
	since := time.Now()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a := zrGetAction(t, first)
		a.Status.Phase = fleetv1.ActionPhaseSucceeded
		now := metav1.Now()
		a.Status.CompletionTime = &now
		return envK8s.Status().Update(ctx, a)
	}); err != nil {
		t.Fatalf("end the first action: %v", err)
	}
	waitFor(t, "the second action to be assigned", func() bool { return zrGetAction(t, second).Status.AssignedRobot != "" })
	if d := time.Since(since); d > wakeAssignWithin {
		t.Fatalf("assigned %v after the slot was freed, want within %v (the TDE's own retry is %v)", d, wakeAssignWithin, tdeMinRetryAfter)
	}
	t.Logf("assigned %v after the slot was freed", time.Since(since).Round(time.Millisecond))
}

// 9. The executor gate: a robot records the action from before, and the cancel sent to it is not
// confirmed (the fake commander answers no ack). When the robot drops the record, the action is
// assigned at once, through the robot's own event.
func TestEnvtest_TheExecutorGateClearingWakesTheAction(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { r.Status.AssignedAction = "pick-1" })
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", nil)
	waitWaiting(t, req, "")
	since := time.Now()
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { r.Status.AssignedAction = "" })
	requireAssignedSoon(t, req, "amr-1", since, "its robot dropping the record")
}

// 10. An assigned action renews its lease on its own timer with the adapter's heartbeats flowing
// (and filtered).
func TestEnvtest_ALeaseRenewsWithHeartbeatsFiltered(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "swarmada-config"}}
	cfg.Spec.Scheduling.LeaseDurationSeconds = 10
	if err := envK8s.Create(ctx, cfg); err != nil {
		t.Fatalf("create config: %v", err)
	}
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", nil)
	waitFor(t, "the action to be assigned", func() bool { return zrGetAction(t, req).Status.AssignedRobot == "amr-1" })
	seen := map[string]bool{}
	deadline := time.Now().Add(14 * time.Second)
	for time.Now().Before(deadline) && len(seen) < 4 {
		editAdapterStatus(t, ns, func(a *fleetv1.FleetAdapter) { a.Status.LastHeartbeat = &metav1.Time{Time: time.Now()} })
		if l := zrGetAction(t, req).Status.LeaseExpiresAt; l != nil {
			seen[l.UTC().Format(time.RFC3339Nano)] = true
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(seen) < 4 { // the first lease and three renewals
		t.Fatalf("saw %d lease horizons in 14 s with a 10 s lease (renewed every ~3.3 s), want at least 4", len(seen))
	}
}

// 11. The namespace's stop held through its processed marker over N free robots with 2N actions
// waiting, then cleared: every robot is given an action at once, not just the first three.
func TestEnvtest_ANamespaceStopClearedWakesEveryRobot(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	const n = 5
	var robots []string
	for i := range n {
		robots = append(robots, fmt.Sprintf("amr-%d", i+1))
	}
	seedWakeFleet(t, ns, robots...)
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "swarmada-config",
		Annotations: map[string]string{annEstopProcessed: "stop-1"}}}
	if err := envK8s.Create(ctx, cfg); err != nil {
		t.Fatalf("create config: %v", err)
	}
	startActionManager(t, ns)
	var reqs []ctrl.Request
	for i := range 2 * n {
		reqs = append(reqs, seedWaitingAction(t, ns, fmt.Sprintf("pick-%02d", i), "aisle-1", nil))
	}
	for _, req := range reqs {
		waitWaiting(t, req, "stop")
	}
	waitForIdleController(t, "fleetaction")
	since := time.Now()
	editObject(t, cfg, func() { delete(cfg.Annotations, annEstopProcessed) })
	assigned := func() int {
		got := map[string]bool{}
		for _, req := range reqs {
			if r := zrGetAction(t, req).Status.AssignedRobot; r != "" {
				got[r] = true
			}
		}
		return len(got)
	}
	deadline := since.Add(wakeAssignWithin)
	for assigned() < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := assigned(); got < n {
		t.Fatalf("%d of %d robots were given an action within %v of the namespace stop clearing", got, n, wakeAssignWithin)
	}
	t.Logf("all %d robots assigned %v after the clear", n, time.Since(since).Round(time.Millisecond))
}
