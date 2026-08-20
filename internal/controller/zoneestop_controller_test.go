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
	"sort"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/safety"

	"github.com/swarmada/swarmada/internal/metrics"
)

const zeNS = "warehouse-a"

// fakeEstopper records which robots were estopped/cleared and returns a scripted
// state.
type fakeEstopper struct {
	// mu guards the recorders. Zone and namespace estop fan out in PARALLEL (§9.6.2.1),
	// so TriggerEstop is called concurrently — the double has to be as safe as the thing
	// it stands in for, or `go test -race` fails on the test's own bookkeeping.
	mu       sync.Mutex
	estopped []string
	cleared  []string
	state    fleetv1.RobotEstopState
	// scopes records the §9.3.8 scope label each call carried. Every emit site used to
	// hard-code "robot", so a zone or namespace estop was indistinguishable from a
	// single-robot one in the metrics; recording it here is what lets a test say otherwise.
	scopes []metrics.EstopScope
}

func (f *fakeEstopper) TriggerEstop(_ context.Context, _, robotID, _, _ string,
	scope metrics.EstopScope) (safety.Result, error) {
	f.mu.Lock()
	f.scopes = append(f.scopes, scope)
	f.estopped = append(f.estopped, robotID)
	st := f.state
	f.mu.Unlock()
	if st == "" {
		st = fleetv1.RobotEstopStopped
	}
	return safety.Result{State: st, Confirmed: st == fleetv1.RobotEstopStopped}, nil
}

func (f *fakeEstopper) ClearEstop(_ context.Context, _, robotID, _ string) (fleetv1.RobotEstopState, error) {
	f.mu.Lock()
	f.cleared = append(f.cleared, robotID)
	f.mu.Unlock()
	return fleetv1.RobotEstopNormal, nil
}

// names/clearedNames return SORTED COPIES. Under a parallel fan-out the arrival order
// carries no information — asserting on it would be asserting on goroutine scheduling — so
// the doubles normalise it rather than letting every caller remember to.
func (f *fakeEstopper) names() []string        { return sortedCopy(f.estopped) }
func (f *fakeEstopper) clearedNames() []string { return sortedCopy(f.cleared) }

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func zeZone(name, parent string, policy *fleetv1.ZoneEstopPolicy, trigger string) *fleetv1.FleetZone {
	z := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: zeNS, Name: name},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent, EstopPolicy: policy},
	}
	if trigger != "" {
		z.Annotations = map[string]string{annEstopTriggered: trigger}
	}
	return z
}

func zeRobot(name, currentZone string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: zeNS, Name: name},
		Status:     fleetv1.RobotStatus{CurrentZone: currentZone},
	}
}

func newZEReconciler(t *testing.T, est *fakeEstopper, rec record.EventRecorder, objs ...client.Object) (*ZoneEstopReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&fleetv1.FleetZone{}, &fleetv1.Robot{}).WithObjects(objs...).Build()
	return &ZoneEstopReconciler{Client: c, Estopper: est, Recorder: rec}, c
}

func reconcileZE(t *testing.T, r *ZoneEstopReconciler, zone string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: zeNS, Name: zone}}); err != nil {
		t.Fatalf("reconcile %q: %v", zone, err)
	}
}

func zeGetZone(t *testing.T, c client.Client, name string) *fleetv1.FleetZone {
	t.Helper()
	z := &fleetv1.FleetZone{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zeNS, Name: name}, z); err != nil {
		t.Fatal(err)
	}
	return z
}

// propagateToChildren (default) estops robots in the zone AND its descendants.
func TestZoneEstop_PropagatesToChildren(t *testing.T) {
	// dock-1 → floor-1 → site-1. Trigger on floor-1.
	est := &fakeEstopper{}
	r, c := newZEReconciler(t, est, nil,
		zeZone("floor-1", "site-1", &fleetv1.ZoneEstopPolicy{PropagateToChildren: true}, "e1"),
		zeZone("dock-1", "floor-1", nil, ""),
		zeZone("site-1", "", nil, ""),
		zeRobot("r-dock", "dock-1"),   // descendant of floor-1 → estopped
		zeRobot("r-floor", "floor-1"), // in floor-1 → estopped
		zeRobot("r-other", "other"),   // unrelated → not estopped
	)

	reconcileZE(t, r, "floor-1")

	got := est.names()
	if len(got) != 2 || got[0] != "r-dock" || got[1] != "r-floor" {
		t.Fatalf("estopped = %v, want [r-dock r-floor]", got)
	}
	if zeGetZone(t, c, "floor-1").Annotations[annEstopProcessed] != "e1" {
		t.Error("processed marker not set")
	}
}

// With propagateToChildren=false, only robots directly in the zone are estopped.
func TestZoneEstop_NoChildrenWhenPolicyFalse(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", &fleetv1.ZoneEstopPolicy{PropagateToChildren: false}, "e1"),
		zeZone("dock-1", "floor-1", nil, ""),
		zeRobot("r-dock", "dock-1"),   // descendant → NOT estopped (policy false)
		zeRobot("r-floor", "floor-1"), // in floor-1 → estopped
	)

	reconcileZE(t, r, "floor-1")

	if got := est.names(); len(got) != 1 || got[0] != "r-floor" {
		t.Fatalf("estopped = %v, want [r-floor] only (no descendants)", got)
	}
}

// propagateToParent emits a ChildEstopTriggered event on the parent — without
// estopping the parent's own robots.
func TestZoneEstop_NotifiesParent(t *testing.T) {
	est := &fakeEstopper{}
	rec := record.NewFakeRecorder(8)
	r, _ := newZEReconciler(t, est, rec,
		zeZone("dock-1", "floor-1", &fleetv1.ZoneEstopPolicy{PropagateToChildren: true, PropagateToParent: true}, "e1"),
		zeZone("floor-1", "", nil, ""),
		zeRobot("r-dock", "dock-1"),
		zeRobot("r-floor", "floor-1"), // in the PARENT → must NOT be estopped
	)

	reconcileZE(t, r, "dock-1")

	if got := est.names(); len(got) != 1 || got[0] != "r-dock" {
		t.Fatalf("estopped = %v, want [r-dock] (parent robot not estopped)", got)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "ChildEstopTriggered") {
			t.Errorf("event = %q, want a ChildEstopTriggered", ev)
		}
	default:
		t.Error("expected a ChildEstopTriggered event on the parent")
	}
}

// Re-reconciling the same trigger does not re-fan-out (idempotent).
func TestZoneEstop_Idempotent(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", nil, "e1"),
		zeRobot("r-floor", "floor-1"),
	)

	reconcileZE(t, r, "floor-1")
	reconcileZE(t, r, "floor-1")
	reconcileZE(t, r, "floor-1")

	if got := est.names(); len(got) != 1 {
		t.Fatalf("estopped %d times, want 1 (idempotent on the processed marker)", len(got))
	}
}

// A NEW trigger value re-fires the estop.
func TestZoneEstop_NewTriggerRefires(t *testing.T) {
	est := &fakeEstopper{}
	r, c := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", nil, "e1"),
		zeRobot("r-floor", "floor-1"),
	)
	reconcileZE(t, r, "floor-1")

	// Operator re-triggers with a new value.
	z := zeGetZone(t, c, "floor-1")
	z.Annotations[annEstopTriggered] = "e2"
	if err := c.Update(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	reconcileZE(t, r, "floor-1")

	if got := est.names(); len(got) != 2 {
		t.Fatalf("estopped %d times, want 2 (new trigger re-fires)", len(got))
	}
}

// Removing the trigger annotation (operator estop-clear) resumes the scope's
// robots and drops the processed marker.
func TestZoneEstop_ClearResumesRobots(t *testing.T) {
	est := &fakeEstopper{}
	r, c := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", &fleetv1.ZoneEstopPolicy{PropagateToChildren: true}, "e1"),
		zeZone("dock-1", "floor-1", nil, ""),
		zeRobot("r-dock", "dock-1"),
		zeRobot("r-floor", "floor-1"),
	)

	reconcileZE(t, r, "floor-1") // triggers the estop
	if len(est.estopped) != 2 {
		t.Fatalf("estopped = %v, want 2", est.names())
	}

	// Operator clears the estop by removing the trigger annotation.
	z := zeGetZone(t, c, "floor-1")
	delete(z.Annotations, annEstopTriggered)
	if err := c.Update(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	reconcileZE(t, r, "floor-1")

	if got := est.clearedNames(); len(got) != 2 || got[0] != "r-dock" || got[1] != "r-floor" {
		t.Fatalf("cleared = %v, want [r-dock r-floor]", got)
	}
	if _, ok := zeGetZone(t, c, "floor-1").Annotations[annEstopProcessed]; ok {
		t.Error("processed marker not removed on clear")
	}

	// A further reconcile does not re-clear (idempotent — nothing processed now).
	reconcileZE(t, r, "floor-1")
	if len(est.cleared) != 2 {
		t.Errorf("re-cleared: %d, want a stable 2", len(est.cleared))
	}
}

// No trigger annotation → nothing happens.
func TestZoneEstop_NoTriggerNoOp(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil,
		zeZone("floor-1", "", nil, ""), // no trigger
		zeRobot("r-floor", "floor-1"),
	)
	reconcileZE(t, r, "floor-1")
	if len(est.estopped) != 0 {
		t.Fatalf("estopped %d robots without a trigger", len(est.estopped))
	}
}
