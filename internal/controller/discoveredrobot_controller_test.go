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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const drNS = "warehouse-a"

var drBase = time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

func newDRReconciler(t *testing.T, nowVal *time.Time, objs ...client.Object) (*DiscoveredRobotReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.DiscoveredRobot{}).
		Build()
	return &DiscoveredRobotReconciler{Client: c, now: func() time.Time { return *nowVal }}, c
}

// discovered builds a DiscoveredRobot connected at drBase with a TTL window.
func discovered(name string, ttl time.Duration, phase fleetv1.DiscoveredRobotPhase) *fleetv1.DiscoveredRobot {
	return &fleetv1.DiscoveredRobot{
		ObjectMeta: metav1.ObjectMeta{Namespace: drNS, Name: name},
		Status: fleetv1.DiscoveredRobotStatus{
			Phase:        phase,
			ConnectedAt:  metav1.Time{Time: drBase},
			TTLExpiresAt: &metav1.Time{Time: drBase.Add(ttl)},
		},
	}
}

func reconcileDR(t *testing.T, r *DiscoveredRobotReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: drNS, Name: name}})
	if err != nil {
		t.Fatalf("reconcile %q: %v", name, err)
	}
	return res
}

func getDROrNil(t *testing.T, c client.Client, name string) *fleetv1.DiscoveredRobot {
	t.Helper()
	dr := &fleetv1.DiscoveredRobot{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: drNS, Name: name}, dr)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return dr
}

// Past its TTL and still un-admitted → deleted.
func TestDR_ExpiredIsDeleted(t *testing.T) {
	now := drBase.Add(31 * time.Minute) // TTL 30m
	r, c := newDRReconciler(t, &now, discovered("amr-1", 30*time.Minute, fleetv1.DiscoveredRobotPhaseDiscovered))

	reconcileDR(t, r, "amr-1")

	if getDROrNil(t, c, "amr-1") != nil {
		t.Fatal("an expired DiscoveredRobot should have been deleted")
	}
}

// Inside the last quarter of the window → marked Stale, requeued to expiry.
func TestDR_ApproachingBecomesStale(t *testing.T) {
	// TTL 40m → stale window is the last 10m, i.e. from drBase+30m.
	now := drBase.Add(35 * time.Minute)
	r, c := newDRReconciler(t, &now, discovered("amr-1", 40*time.Minute, fleetv1.DiscoveredRobotPhaseDiscovered))

	res := reconcileDR(t, r, "amr-1")

	dr := getDROrNil(t, c, "amr-1")
	if dr == nil || dr.Status.Phase != fleetv1.DiscoveredRobotPhaseStale {
		t.Fatalf("phase = %v, want Stale", dr.Status.Phase)
	}
	if res.RequeueAfter <= 0 {
		t.Error("expected a requeue toward expiry")
	}
}

// Still fresh (before the stale window) → stays Discovered, requeued to staleAt.
func TestDR_FreshStaysDiscovered(t *testing.T) {
	now := drBase.Add(5 * time.Minute) // TTL 40m, stale at +30m
	r, c := newDRReconciler(t, &now, discovered("amr-1", 40*time.Minute, fleetv1.DiscoveredRobotPhaseDiscovered))

	res := reconcileDR(t, r, "amr-1")

	if dr := getDROrNil(t, c, "amr-1"); dr == nil || dr.Status.Phase != fleetv1.DiscoveredRobotPhaseDiscovered {
		t.Fatalf("phase drifted from Discovered: %v", dr.Status.Phase)
	}
	// Next event is the stale transition at +30m → ~25m from now.
	if want := 25 * time.Minute; res.RequeueAfter <= 0 || res.RequeueAfter > want+time.Minute {
		t.Errorf("requeueAfter = %v, want ~%v (to the stale transition)", res.RequeueAfter, want)
	}
}

// A nil TTL is a no-op (nothing to sweep).
func TestDR_NilTTLNoOp(t *testing.T) {
	dr := &fleetv1.DiscoveredRobot{
		ObjectMeta: metav1.ObjectMeta{Namespace: drNS, Name: "amr-1"},
		Status:     fleetv1.DiscoveredRobotStatus{Phase: fleetv1.DiscoveredRobotPhaseDiscovered},
	}
	now := drBase
	r, c := newDRReconciler(t, &now, dr)

	res := reconcileDR(t, r, "amr-1")

	if res.RequeueAfter != 0 {
		t.Errorf("nil-TTL should not requeue, got %v", res.RequeueAfter)
	}
	if getDROrNil(t, c, "amr-1") == nil {
		t.Error("a nil-TTL DiscoveredRobot must not be deleted")
	}
}

// An already-Stale robot at expiry is deleted (Stale → gone).
func TestDR_StaleThenExpiredDeleted(t *testing.T) {
	now := drBase.Add(41 * time.Minute)
	r, c := newDRReconciler(t, &now, discovered("amr-1", 40*time.Minute, fleetv1.DiscoveredRobotPhaseStale))

	reconcileDR(t, r, "amr-1")

	if getDROrNil(t, c, "amr-1") != nil {
		t.Fatal("an expired Stale DiscoveredRobot should have been deleted")
	}
}
