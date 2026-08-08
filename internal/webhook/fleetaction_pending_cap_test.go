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

package webhook

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// capReader builds a cached-style reader with the Pending-by-zone index the cap
// check relies on, seeded with the given objects.
func capReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&fleetv1.FleetAction{}, indexPendingByZone, indexActionPendingZone).
		Build()
}

func configWithCap(ns string, cap int32) *fleetv1.SwarmadaConfig {
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada", Namespace: ns},
		Spec:       fleetv1.SwarmadaConfigSpec{Scheduling: fleetv1.SwarmadaSchedulingConfig{MaxPendingActionsPerZone: cap}},
	}
}

func pendingInZone(name, ns, zone string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: zone},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
}

func newAction(ns, zone string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "incoming", Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: zone},
	}
}

func TestPendingCap_UnderCapAllowed(t *testing.T) {
	ns, zone := "warehouse-a", "dock-1"
	v := &FleetActionValidator{Reader: capReader(t,
		configWithCap(ns, 3),
		pendingInZone("p1", ns, zone),
	)}
	if _, err := v.ValidateCreate(context.Background(), newAction(ns, zone)); err != nil {
		t.Fatalf("under cap must be allowed, got: %v", err)
	}
}

func TestPendingCap_AtCapRejected(t *testing.T) {
	ns, zone := "warehouse-a", "dock-1"
	v := &FleetActionValidator{Reader: capReader(t,
		configWithCap(ns, 2),
		pendingInZone("p1", ns, zone),
		pendingInZone("p2", ns, zone),
	)}
	_, err := v.ValidateCreate(context.Background(), newAction(ns, zone))
	if err == nil {
		t.Fatal("at cap must be rejected")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("want Forbidden, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "PendingActionLimitExceeded") {
		t.Fatalf("rejection must carry the PendingActionLimitExceeded reason, got: %v", err)
	}
}

// A different zone's Pending actions do not count against this zone.
func TestPendingCap_CountIsPerZone(t *testing.T) {
	ns := "warehouse-a"
	v := &FleetActionValidator{Reader: capReader(t,
		configWithCap(ns, 1),
		pendingInZone("p1", ns, "dock-2"),
	)}
	if _, err := v.ValidateCreate(context.Background(), newAction(ns, "dock-1")); err != nil {
		t.Fatalf("a full sibling zone must not block dock-1, got: %v", err)
	}
}

// A zone-less (any-zone) action is exempt from the per-zone cap even when a zone
// is saturated.
func TestPendingCap_ZonelessActionExempt(t *testing.T) {
	ns, zone := "warehouse-a", "dock-1"
	v := &FleetActionValidator{Reader: capReader(t,
		configWithCap(ns, 1),
		pendingInZone("p1", ns, zone),
	)}
	if _, err := v.ValidateCreate(context.Background(), newAction(ns, "")); err != nil {
		t.Fatalf("a zone-less action must be exempt, got: %v", err)
	}
}

// cap of 0 means unbounded.
func TestPendingCap_ZeroIsUnbounded(t *testing.T) {
	ns, zone := "warehouse-a", "dock-1"
	v := &FleetActionValidator{Reader: capReader(t,
		configWithCap(ns, 0),
		pendingInZone("p1", ns, zone),
		pendingInZone("p2", ns, zone),
	)}
	if _, err := v.ValidateCreate(context.Background(), newAction(ns, zone)); err != nil {
		t.Fatalf("cap=0 is unbounded, got: %v", err)
	}
}

// No SwarmadaConfig ⇒ fail open (no cap enforced).
func TestPendingCap_NoConfigFailsOpen(t *testing.T) {
	ns, zone := "warehouse-a", "dock-1"
	v := &FleetActionValidator{Reader: capReader(t,
		pendingInZone("p1", ns, zone),
		pendingInZone("p2", ns, zone),
	)}
	if _, err := v.ValidateCreate(context.Background(), newAction(ns, zone)); err != nil {
		t.Fatalf("absent config must fail open, got: %v", err)
	}
}
