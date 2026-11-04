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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Work under a stop moves only on the robot's own end report or an operator's decision (RFC-0001
// #safety-action-table rows A6 and A12, invariant I1). These rows hold that where the stop is not
// on the robot's record: a Preempted victim whose Robot object is gone, and a Revoking action
// under onDisconnect: AfterTimeout whose robot a stop covers that is issued and not yet recorded
// on it, or whose Robot object is gone. The stops are read live.

// deadLease is a lease horizon an hour gone: provably dead under any skew.
func deadLease() *metav1.Time { return &metav1.Time{Time: time.Now().Add(-time.Hour)} }

// stoppedZone is zone name carrying a stop: issued (the trigger alone) or applied (the trigger and
// the processed marker).
func stoppedZone(name string, applied bool) *fleetv1.FleetZone {
	z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS,
		Annotations: map[string]string{estopscope.TriggerAnnotation: "op-stop"}}}
	if applied {
		z.Annotations[estopscope.ProcessedAnnotation] = "op-stop"
	}
	return z
}

// quietZone is zone name with no stop.
func quietZone(name string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS}}
}

// namespaceStopConfig is the AfterTimeout config with the namespace stop issued on it.
func namespaceStopConfig() *fleetv1.SwarmadaConfig {
	cfg := actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30)
	cfg.Annotations = map[string]string{estopscope.TriggerAnnotation: "op-stop"}
	return cfg
}

// orphanVictim is the Preempted action "v", spec.zone zone, bound to r1, whose Robot object does
// not exist, on a dead lease.
func orphanVictim(zone string) *fleetv1.FleetAction {
	v := assignedAction("v", "r1", fleetv1.ActionPhasePreempted, 2, deadLease())
	v.Spec.Zone = zone
	st := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	v.Status.StartedAt = &st
	return v
}

// revokingPastCeiling is the Revoking action "x", spec.zone zone, bound to r1, on a dead lease and
// disconnected for an hour: past any AfterTimeout ceiling the configs below set.
func revokingPastCeiling(zone string) *fleetv1.FleetAction {
	x := assignedAction("x", "r1", fleetv1.ActionPhaseRevoking, 3, deadLease())
	x.Spec.Zone = zone
	x.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	st := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	x.Status.StartedAt = &st
	return x
}

// offlineR1In is r1, Offline, recording "x", standing in zone, with no stop on its record.
func offlineR1In(zone string) *fleetv1.Robot {
	r1 := robotInPhase("r1", fleetv1.RobotPhaseOffline, "x")
	r1.Spec.Zone, r1.Status.CurrentZone = zone, zone
	return r1
}

// heldBound asserts that name is still phase, bound to r1, on its lease, with a message that names
// the operator's decision and the stop.
func heldBound(t *testing.T, c client.Client, name string, phase fleetv1.ActionPhase, stop string) {
	t.Helper()
	a := getAction(t, c, name)
	if a.Status.Phase != phase || a.Status.AssignedRobot != "r1" || a.Status.LeaseExpiresAt == nil {
		t.Fatalf("INVARIANT (A12/I1): %s is %q bound to %q (lease %v, %q), not held %s on r1 under %s",
			name, a.Status.Phase, a.Status.AssignedRobot, a.Status.LeaseExpiresAt, a.Status.Message, phase, stop)
	}
	if !strings.Contains(a.Status.Message, "awaiting operator decision") || !strings.Contains(a.Status.Message, stop) {
		t.Errorf("the held message does not name the operator's decision and %s: %q", stop, a.Status.Message)
	}
}

// released asserts that name went to Pending with its binding and lease cleared.
func released(t *testing.T, c client.Client, name string) {
	t.Helper()
	a := getAction(t, c, name)
	if a.Status.Phase != fleetv1.ActionPhasePending || a.Status.AssignedRobot != "" || a.Status.LeaseExpiresAt != nil {
		t.Fatalf("%s is %q bound to %q (lease %v, %q), want released to Pending", name, a.Status.Phase,
			a.Status.AssignedRobot, a.Status.LeaseExpiresAt, a.Status.Message)
	}
}

// A Preempted victim whose Robot is deleted is not released at the lease horizon while a
// zone's stop stands, issued or applied. Nothing records where the robot stood once the object is
// gone, so a stop on any zone of the namespace holds the work, the action's own zone or not.
func TestPreemptedVictim_RobotGone_HeldUnderAZoneStop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		actionZone string
		zones      []client.Object
		stop       string
	}{
		{"issued on its zone", "z", []client.Object{stoppedZone("z", false)}, "zone/z"},
		{"applied on its zone", "z", []client.Object{stoppedZone("z", true)}, "zone/z"},
		{"issued on another zone", "z", []client.Object{quietZone("z"), stoppedZone("w", false)}, "zone/w"},
		{"zone-less action", "", []client.Object{stoppedZone("w", true)}, "zone/w"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{orphanVictim(tc.actionZone)}, tc.zones...)
			r, c := newActionReconciler(t, objs...)
			reconcileAction(t, r, "v")
			reconcileAction(t, r, "v")
			heldBound(t, c, "v", fleetv1.ActionPhasePreempted, tc.stop)
		})
	}
}

// The same under the namespace stop, issued or applied.
func TestPreemptedVictim_RobotGone_HeldUnderANamespaceStop(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "issued", true: "applied"}[applied], func(t *testing.T) {
			cfg := namespaceStopConfig()
			if applied {
				cfg.Annotations[estopscope.ProcessedAnnotation] = "op-stop"
			}
			r, c := newActionReconciler(t, orphanVictim("z"), quietZone("z"), cfg)
			reconcileAction(t, r, "v")
			reconcileAction(t, r, "v")
			heldBound(t, c, "v", fleetv1.ActionPhasePreempted, "namespace/swarmada")
		})
	}
}

// Guard: with no stop anywhere, a Preempted victim whose Robot is gone is released at the lease
// horizon, as before (A12 on the lease alone).
func TestPreemptedVictim_RobotGone_NoStop_ReleasedAtTheHorizon(t *testing.T) {
	r, c := newActionReconciler(t, orphanVictim("z"), quietZone("z"), quietZone("w"),
		actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30))
	reconcileAction(t, r, "v")
	released(t, c, "v")
}

// Under onDisconnect: AfterTimeout, past the ceiling and on a dead lease, a Revoking
// action whose robot a zone's or the namespace's stop covers, issued and not yet recorded on the
// robot, is held for a person; it is not reassigned.
func TestRevokingAfterTimeout_StopIssuedNotYetRecorded_Held(t *testing.T) {
	t.Run("zone stop", func(t *testing.T) {
		r, c := newActionReconciler(t, revokingPastCeiling("z"), offlineR1In("z"), stoppedZone("z", false),
			actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30))
		reconcileAction(t, r, "x")
		reconcileAction(t, r, "x")
		heldBound(t, c, "x", fleetv1.ActionPhaseRevoking, "zone/z")
		if r1 := getRobot(t, c, "r1", actionNS); r1.Status.AssignedAction != "x" {
			t.Errorf("INVARIANT: r1's record was cleared to %q under the issued stop", r1.Status.AssignedAction)
		}
	})
	t.Run("namespace stop", func(t *testing.T) {
		r, c := newActionReconciler(t, revokingPastCeiling("z"), offlineR1In("z"), quietZone("z"), namespaceStopConfig())
		reconcileAction(t, r, "x")
		reconcileAction(t, r, "x")
		heldBound(t, c, "x", fleetv1.ActionPhaseRevoking, "namespace/swarmada")
	})
}

// The same on the Revoking phase: past the AfterTimeout ceiling on a dead lease, a Revoking
// action whose Robot object is gone is held while any stop in the namespace stands.
func TestRevokingAfterTimeout_RobotGone_HeldUnderAStop(t *testing.T) {
	r, c := newActionReconciler(t, revokingPastCeiling("z"), quietZone("z"), stoppedZone("w", true),
		actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30))
	reconcileAction(t, r, "x")
	reconcileAction(t, r, "x")
	heldBound(t, c, "x", fleetv1.ActionPhaseRevoking, "zone/w")
}

// Guard: with no stop, the live read finds none and AfterTimeout reassigns past the ceiling, for a
// robot that exists and for one whose Robot is gone.
func TestRevokingAfterTimeout_NoStop_Reassigns(t *testing.T) {
	t.Run("robot offline", func(t *testing.T) {
		r, c := newActionReconciler(t, revokingPastCeiling("z"), offlineR1In("z"), quietZone("z"),
			actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30))
		reconcileAction(t, r, "x")
		released(t, c, "x")
	})
	t.Run("robot gone", func(t *testing.T) {
		r, c := newActionReconciler(t, revokingPastCeiling("z"), quietZone("z"),
			actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30))
		reconcileAction(t, r, "x")
		released(t, c, "x")
	})
}

// Guard: an operator's cancel still ends the hold on a victim whose Robot is gone (A21).
func TestPreemptedVictim_RobotGone_HeldUnderAStop_OperatorCancelExits(t *testing.T) {
	r, c := newActionReconciler(t, orphanVictim("z"), stoppedZone("z", true))
	reconcileAction(t, r, "v")
	heldBound(t, c, "v", fleetv1.ActionPhasePreempted, "zone/z")
	a := getAction(t, c, "v")
	base := a.DeepCopy()
	a.Annotations = map[string]string{annCancelRequested: "operator: robot inspected"}
	if err := c.Patch(context.Background(), a, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "v")
	if got := getAction(t, c, "v"); got.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %q (%q), want Cancelled on the operator's cancel", got.Status.Phase, got.Status.Message)
	}
}

// Guard: a namespace delete still cleans up. While the namespace is Terminating, every Robot gone
// and its stop standing, the held victim takes no stop finalizer, and deleting it removes it.
func TestPreemptedVictim_RobotGone_NamespaceDeleteCleansUp(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: actionNS, Finalizers: []string{"kubernetes"},
		DeletionTimestamp: &metav1.Time{Time: time.Now()}}}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ns, readyActionAdapter(), orphanVictim("z"), quietZone("z"), namespaceStopConfig()).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).Build()
	r := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler()}

	reconcileAction(t, r, "v")
	if a := getAction(t, c, "v"); controllerutil.ContainsFinalizer(a, estopscope.StopFinalizer) {
		t.Fatalf("the held victim took the stop finalizer with no robot recording it: %v", a.Finalizers)
	}
	if err := c.Delete(context.Background(), getAction(t, c, "v")); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "v")
	err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: "v"}, &fleetv1.FleetAction{})
	if !errors.IsNotFound(err) {
		t.Fatalf("the deleted victim is still there (err %v): the namespace delete would not clean it up", err)
	}
}
