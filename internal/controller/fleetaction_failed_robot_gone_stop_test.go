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

// A Failed (StopUnconfirmed) action keeps its robot until A3 releases it on a dead lease, and while
// a stop holds the robot A6 holds it past that (RFC-0001 #safety-action-table rows A3, A4 and A6,
// invariant I1). These rows hold that once the action's Robot object is gone: nothing then records
// where the robot stands, so a stop on any zone of the namespace, or the namespace stop, holds the
// work. The fixtures (deadLease, stoppedZone, quietZone, namespaceStopConfig, heldBound, released)
// are the bound-robot stop rows'.

// orphanFailed is the Failed (StopUnconfirmed) action "f", spec.zone zone, bound to r1, whose Robot
// object does not exist, on a dead lease, so A3's release is due. onFailure is Requeue with retries
// left, so a release shows as Pending.
func orphanFailed(zone string) *fleetv1.FleetAction {
	f := assignedAction("f", "r1", fleetv1.ActionPhaseFailed, 4, deadLease())
	f.Spec.Zone = zone
	f.Spec.OnFailure = fleetv1.ActionFailureRequeue
	f.Spec.RetryPolicy = &fleetv1.ActionRetryPolicy{MaxRetries: 3, BackoffSeconds: 0}
	f.Status.FailureReason = reasonStopUnconfirmed
	at := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	f.Status.ScheduledAt, f.Status.FailedAt = &at, &at
	return f
}

// heldFailed asserts that "f" is held Failed (StopUnconfirmed) on r1 under stop, with no retry.
func heldFailed(t *testing.T, c client.Client, stop string) {
	t.Helper()
	heldBound(t, c, "f", fleetv1.ActionPhaseFailed, stop)
	if a := getAction(t, c, "f"); a.Status.FailureReason != reasonStopUnconfirmed || a.Status.RetryCount != 0 {
		t.Errorf("failureReason = %q, retryCount = %d under the hold, want StopUnconfirmed and no retry",
			a.Status.FailureReason, a.Status.RetryCount)
	}
}

// A Failed (StopUnconfirmed) action whose Robot is deleted is not released at the lease horizon
// while a zone's stop stands, issued or applied, on the action's own zone or on any other.
func TestFailedStopUnconfirmed_RobotGone_HeldUnderAZoneStop(t *testing.T) {
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
			objs := append([]client.Object{orphanFailed(tc.actionZone)}, tc.zones...)
			r, c := newActionReconciler(t, objs...)
			reconcileAction(t, r, "f")
			reconcileAction(t, r, "f")
			heldFailed(t, c, tc.stop)
		})
	}
}

// The same under the namespace stop, issued or applied.
func TestFailedStopUnconfirmed_RobotGone_HeldUnderANamespaceStop(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "issued", true: "applied"}[applied], func(t *testing.T) {
			cfg := namespaceStopConfig()
			if applied {
				cfg.Annotations[estopscope.ProcessedAnnotation] = "op-stop"
			}
			r, c := newActionReconciler(t, orphanFailed("z"), quietZone("z"), cfg)
			reconcileAction(t, r, "f")
			reconcileAction(t, r, "f")
			heldFailed(t, c, "namespace/swarmada")
		})
	}
}

// The held message says why the hold rests on a stop the robot may not stand under: its Robot
// object is gone.
func TestFailedStopUnconfirmed_RobotGone_HeldMessageSaysTheRobotIsGone(t *testing.T) {
	r, c := newActionReconciler(t, orphanFailed("z"), stoppedZone("z", true))
	reconcileAction(t, r, "f")
	heldFailed(t, c, "zone/z")
	if msg := getAction(t, c, "f").Status.Message; !strings.Contains(msg, "Robot object is gone") {
		t.Errorf("the held message does not say the Robot object is gone: %q", msg)
	}
}

// The held message names the robot the action is bound to, though its Robot object is gone.
func TestFailedStopUnconfirmed_RobotGone_HeldMessageNamesTheRobot(t *testing.T) {
	r, c := newActionReconciler(t, orphanFailed("z"), stoppedZone("z", true))
	reconcileAction(t, r, "f")
	heldFailed(t, c, "zone/z")
	if msg := getAction(t, c, "f").Status.Message; !strings.Contains(msg, "robot r1 did not confirm") {
		t.Errorf("the held message does not name robot r1: %q", msg)
	}
}

// Guard: with no stop anywhere, a Failed (StopUnconfirmed) action whose Robot is gone is released
// at the lease horizon, as before (A3), and onFailure applies.
func TestFailedStopUnconfirmed_RobotGone_NoStop_ReleasedAtTheHorizon(t *testing.T) {
	r, c := newActionReconciler(t, orphanFailed("z"), quietZone("z"), quietZone("w"),
		actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30))
	reconcileAction(t, r, "f")
	released(t, c, "f")
	if a := getAction(t, c, "f"); a.Status.RetryCount != 1 {
		t.Errorf("retryCount = %d after the release, want onFailure Requeue's 1", a.Status.RetryCount)
	}
}

// The hold ends by itself once no stop stands: A3's release then runs on the dead lease.
func TestFailedStopUnconfirmed_RobotGone_ReleasedOnceTheStopClears(t *testing.T) {
	r, c := newActionReconciler(t, orphanFailed("z"), stoppedZone("z", true))
	reconcileAction(t, r, "f")
	heldFailed(t, c, "zone/z")
	z := &fleetv1.FleetZone{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: "z"}, z); err != nil {
		t.Fatal(err)
	}
	base := z.DeepCopy()
	z.Annotations = nil
	if err := c.Patch(context.Background(), z, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "f")
	released(t, c, "f")
}

// Guard: an operator's cancel still ends the hold (A4 on the dead lease).
func TestFailedStopUnconfirmed_RobotGone_HeldUnderAStop_OperatorCancelExits(t *testing.T) {
	r, c := newActionReconciler(t, orphanFailed("z"), stoppedZone("z", true))
	reconcileAction(t, r, "f")
	heldFailed(t, c, "zone/z")
	a := getAction(t, c, "f")
	base := a.DeepCopy()
	a.Annotations = map[string]string{annCancelRequested: "operator: robot inspected"}
	if err := c.Patch(context.Background(), a, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "f")
	if got := getAction(t, c, "f"); got.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %q (%q), want Cancelled on the operator's cancel", got.Status.Phase, got.Status.Message)
	}
}

// Guard: an operator's requeue still ends the hold (A2 on the dead lease).
func TestFailedStopUnconfirmed_RobotGone_HeldUnderAStop_OperatorRequeueExits(t *testing.T) {
	r, c := newActionReconciler(t, orphanFailed("z"), stoppedZone("z", true))
	reconcileAction(t, r, "f")
	heldFailed(t, c, "zone/z")
	a := getAction(t, c, "f")
	base := a.DeepCopy()
	a.Annotations = map[string]string{annRequeueRequested: "operator: robot inspected"}
	if err := c.Patch(context.Background(), a, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "f")
	if got := getAction(t, c, "f"); got.Status.Phase != fleetv1.ActionPhasePending || got.Status.AssignedRobot != "" {
		t.Fatalf("phase = %q bound to %q (%q), want Pending and unbound on the operator's requeue",
			got.Status.Phase, got.Status.AssignedRobot, got.Status.Message)
	}
}

// Guard: a namespace delete still cleans up. While the namespace is Terminating, every Robot gone
// and its stop standing, the held action takes no stop finalizer, and deleting it removes it.
func TestFailedStopUnconfirmed_RobotGone_NamespaceDeleteCleansUp(t *testing.T) {
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
		WithObjects(ns, readyActionAdapter(), orphanFailed("z"), quietZone("z"), namespaceStopConfig()).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).Build()
	r := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler()}

	reconcileAction(t, r, "f")
	if a := getAction(t, c, "f"); controllerutil.ContainsFinalizer(a, estopscope.StopFinalizer) {
		t.Fatalf("the held action took the stop finalizer with no robot recording it: %v", a.Finalizers)
	}
	if err := c.Delete(context.Background(), getAction(t, c, "f")); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "f")
	err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: "f"}, &fleetv1.FleetAction{})
	if !errors.IsNotFound(err) {
		t.Fatalf("the deleted action is still there (err %v): the namespace delete would not clean it up", err)
	}
}
