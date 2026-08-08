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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

func envtestValidRobot(ns, name, zone string) *fleetv1.Robot {
	return &fleetv1.Robot{
		// The robot-id annotation is the sole wire->Robot join key (ADR-0028). In
		// production the defaulting webhook guarantees it; this suite does not install
		// webhooks, so a fixture without it is a Robot that cannot exist — and any
		// projection keyed on the annotation would silently find nothing.
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Annotations: map[string]string{fleetv1.RobotIDAnnotation: name},
		},
		Spec: fleetv1.RobotSpec{
			Manufacturer: "Acme", Model: "X1",
			Adapter: fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
			Zone:    zone,
		},
	}
}

// Robot: no heartbeat past the offline threshold → Offline, and a second reconcile
// of the stable Offline robot writes nothing (idempotency, docs/testing.md).
func TestEnvtest_RobotHeartbeatOffline(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &RobotReconciler{Client: envK8s, Scheme: envScheme}

	robot := envtestValidRobot(ns, "amr-1", "z-a")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	// Seed a stale heartbeat older than the default 30s offline threshold.
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{
		LastSeenAt: &metav1.Time{Time: time.Now().Add(-60 * time.Second)},
	}
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed robot status: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "amr-1", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &fleetv1.Robot{}
	if err := envK8s.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if got.Status.Phase != fleetv1.RobotPhaseOffline {
		t.Fatalf("phase = %q, want Offline", got.Status.Phase)
	}

	// Idempotency: reconciling the now-stable Offline robot must not write status.
	rv := got.ResourceVersion
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	after := &fleetv1.Robot{}
	if err := envK8s.Get(ctx, req.NamespacedName, after); err != nil {
		t.Fatalf("get robot after: %v", err)
	}
	if after.ResourceVersion != rv {
		t.Errorf("stable reconcile wrote status (resourceVersion %s → %s): not idempotent", rv, after.ResourceVersion)
	}
	if after.Status.Phase != fleetv1.RobotPhaseOffline {
		t.Errorf("phase drifted to %q on second reconcile", after.Status.Phase)
	}
}

// FleetAction: an assigned robot that is lost (Offline) requeues the action to Pending
// once the lease is provably dead and the onDisconnect=AfterTimeout ceiling has been
// crossed (RA-4: never reassign on unreachability alone). The robot binding is
// released. A settled Pending-with-no-eligible-robot state is then idempotent.
//
// NOTE: RobotPhase has no "Degraded" value; the requeue trigger for a lost assigned
// robot is this lease-expiry path (docs/testing.md "assigned robot Degraded/Offline
// → requeue to Pending"). Capability degradation affects new assignments, not a
// running action's binding.
func TestEnvtest_FleetActionRequeueOnLostRobot(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	// Default onDisconnect=Never would HOLD a Revoking action; AfterTimeout lets a
	// provably-dead lease past the ceiling auto-requeue (§9.1.11.9).
	cfg := &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: ns},
		Spec: fleetv1.SwarmadaConfigSpec{
			ActionCancellation: fleetv1.SwarmadaActionCancellationConfig{
				OnDisconnect:             fleetv1.ActionCancellationAfterTimeout,
				DisconnectTimeoutSeconds: i32(30),
			},
		},
	}
	if err := envK8s.Create(ctx, cfg); err != nil {
		t.Fatalf("create config: %v", err)
	}

	robot := envtestValidRobot(ns, "r-lost", "z-a")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	robot.Status.Phase = fleetv1.RobotPhaseOffline // lost
	robot.Status.AssignedAction = "task-1"
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed robot status: %v", err)
	}

	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns},
		Spec: fleetv1.FleetActionSpec{
			Type:     fleetv1.ActionTypeNavigate,
			Zone:     "z-a",
			Priority: fleetv1.ActionPriorityNormal,
		},
	}
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Revoking with a provably-dead lease and a disconnect older than the 30s ceiling.
	action.Status.Phase = fleetv1.ActionPhaseRevoking
	action.Status.AssignedRobot = "r-lost"
	action.Status.AssignmentGeneration = 1
	action.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}
	action.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-2 * time.Minute)}
	if err := envK8s.Status().Update(ctx, action); err != nil {
		t.Fatalf("seed task status: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "task-1", Namespace: ns}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Transition: action requeued to Pending, robot binding released.
	gotAction := &fleetv1.FleetAction{}
	if err := envK8s.Get(ctx, req.NamespacedName, gotAction); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if gotAction.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("task phase = %q, want Pending", gotAction.Status.Phase)
	}
	if gotAction.Status.AssignedRobot != "" {
		t.Errorf("task still bound to %q, want released", gotAction.Status.AssignedRobot)
	}
	gotRobot := &fleetv1.Robot{}
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "r-lost", Namespace: ns}, gotRobot); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if gotRobot.Status.AssignedAction != "" {
		t.Errorf("robot still holds task %q, want released", gotRobot.Status.AssignedAction)
	}

	// Idempotency: the only robot is Offline, so the Pending action finds no eligible
	// robot and settles. Reconcile once to settle the Pending message/anchor, then
	// assert a further reconcile writes nothing.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("settle reconcile: %v", err)
	}
	settled := &fleetv1.FleetAction{}
	if err := envK8s.Get(ctx, req.NamespacedName, settled); err != nil {
		t.Fatalf("get settled task: %v", err)
	}
	rv := settled.ResourceVersion
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("idempotency reconcile: %v", err)
	}
	after := &fleetv1.FleetAction{}
	if err := envK8s.Get(ctx, req.NamespacedName, after); err != nil {
		t.Fatalf("get task after: %v", err)
	}
	if after.ResourceVersion != rv {
		t.Errorf("settled Pending task wrote status (resourceVersion %s → %s): not idempotent", rv, after.ResourceVersion)
	}
	if after.Status.Phase != fleetv1.ActionPhasePending {
		t.Errorf("task phase drifted to %q, want stable Pending", after.Status.Phase)
	}
}
