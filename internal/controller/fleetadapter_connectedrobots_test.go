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
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// §9.1.12 status.connectedRobots — the count of Robots this adapter currently drives.
//
// "Driven through this adapter" means BOUND to it via spec.adapter.name, not "its class appears in
// spec.servesRobotClasses". Authority follows the named binding (§9.5), so a robot bound elsewhere
// is not this adapter's to count even when the two adapters serve overlapping classes — counting by
// class would double-count a shared class and inflate every adapter in the namespace.

const crNS2 = "warehouse-count"

func countRobot(name, adapter string, phase fleetv1.RobotPhase) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: crNS2},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: adapter}},
		Status:     fleetv1.RobotStatus{Phase: phase},
	}
}

func countAdapter(name string) *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: crNS2},
		Spec:       fleetv1.FleetAdapterSpec{HeartbeatIntervalSeconds: 10},
	}
}

// newCountReconciler builds a reconciler over a fake client carrying the spec.adapter.name index
// the counter (and projectLiveness) rely on.
func newCountReconciler(t *testing.T, objs ...client.Object) (*FleetAdapterReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAdapter{}, &fleetv1.Robot{}).
		WithIndex(&fleetv1.Robot{}, robotAdapterNameField, func(o client.Object) []string {
			if n := o.(*fleetv1.Robot).Spec.Adapter.Name; n != "" {
				return []string{n}
			}
			return nil
		}).Build()
	now := time.Unix(1000, 0)
	return &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}, c
}

func reconcileAdapter(t *testing.T, r *FleetAdapterReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: crNS2}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getAdapter(t *testing.T, c client.Client, name string) *fleetv1.FleetAdapter {
	t.Helper()
	var fa fleetv1.FleetAdapter
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: crNS2}, &fa); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	return &fa
}

// N served robots → count N.
func TestConnectedRobots_CountsServedRobots(t *testing.T) {
	r, c := newCountReconciler(t,
		countAdapter("acme"),
		countRobot("amr-1", "acme", fleetv1.RobotPhaseIdle),
		countRobot("amr-2", "acme", fleetv1.RobotPhaseInProgress),
		countRobot("amr-3", "acme", fleetv1.RobotPhaseCharging),
	)
	reconcileAdapter(t, r, "acme")
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 3 {
		t.Errorf("connectedRobots = %d, want 3", got)
	}
}

// THE correction this test file exists to pin: a robot bound to a DIFFERENT adapter is not counted,
// even though both adapters could serve its class. Counting by servesRobotClasses would report 2.
func TestConnectedRobots_IgnoresRobotsBoundElsewhere(t *testing.T) {
	r, c := newCountReconciler(t,
		countAdapter("acme"),
		countAdapter("otherco"),
		countRobot("mine", "acme", fleetv1.RobotPhaseIdle),
		countRobot("theirs", "otherco", fleetv1.RobotPhaseIdle),
	)
	reconcileAdapter(t, r, "acme")
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 1 {
		t.Errorf("connectedRobots = %d, want 1 (authority follows spec.adapter.name, not class)", got)
	}
	reconcileAdapter(t, r, "otherco")
	if got := getAdapter(t, c, "otherco").Status.ConnectedRobots; got != 1 {
		t.Errorf("otherco connectedRobots = %d, want 1", got)
	}
}

// A robot leaving the fleet decrements the count.
func TestConnectedRobots_DeletedRobotDecrements(t *testing.T) {
	leaving := countRobot("amr-2", "acme", fleetv1.RobotPhaseIdle)
	r, c := newCountReconciler(t,
		countAdapter("acme"),
		countRobot("amr-1", "acme", fleetv1.RobotPhaseIdle),
		leaving,
	)
	reconcileAdapter(t, r, "acme")
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 2 {
		t.Fatalf("precondition: connectedRobots = %d, want 2", got)
	}

	if err := c.Delete(context.Background(), leaving); err != nil {
		t.Fatalf("delete robot: %v", err)
	}
	reconcileAdapter(t, r, "acme")
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 1 {
		t.Errorf("connectedRobots = %d after a robot left, want 1", got)
	}
}

// An Offline robot is bound but not driven: "connectedRobots" must not report it. Otherwise a
// Disconnected adapter with three dead robots reads as a healthy fleet of three in swarmctl.
func TestConnectedRobots_ExcludesOffline(t *testing.T) {
	going := countRobot("amr-2", "acme", fleetv1.RobotPhaseIdle)
	r, c := newCountReconciler(t,
		countAdapter("acme"),
		countRobot("amr-1", "acme", fleetv1.RobotPhaseIdle),
		going,
	)
	reconcileAdapter(t, r, "acme")
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 2 {
		t.Fatalf("precondition: connectedRobots = %d, want 2", got)
	}

	going.Status.Phase = fleetv1.RobotPhaseOffline
	if err := c.Status().Update(context.Background(), going); err != nil {
		t.Fatalf("update robot status: %v", err)
	}
	reconcileAdapter(t, r, "acme")
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 1 {
		t.Errorf("connectedRobots = %d with one robot Offline, want 1", got)
	}
}

// An adapter with no robots reports 0 rather than leaving the field stale.
func TestConnectedRobots_NoRobotsIsZero(t *testing.T) {
	r, c := newCountReconciler(t, countAdapter("lonely"))
	reconcileAdapter(t, r, "lonely")
	if got := getAdapter(t, c, "lonely").Status.ConnectedRobots; got != 0 {
		t.Errorf("connectedRobots = %d, want 0", got)
	}
}

// RA-1 / transition-only: two reconciles over an unchanged fleet produce exactly ONE status write.
// Counting must not turn every reconcile into a write.
func TestConnectedRobots_TransitionOnlyWrite(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	writes := 0
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(countAdapter("acme"), countRobot("amr-1", "acme", fleetv1.RobotPhaseIdle)).
		WithStatusSubresource(&fleetv1.FleetAdapter{}, &fleetv1.Robot{}).
		WithIndex(&fleetv1.Robot{}, robotAdapterNameField, func(o client.Object) []string {
			if n := o.(*fleetv1.Robot).Spec.Adapter.Name; n != "" {
				return []string{n}
			}
			return nil
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, isFA := obj.(*fleetv1.FleetAdapter); isFA {
					writes++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	now := time.Unix(1000, 0)
	r := &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}

	reconcileAdapter(t, r, "acme")
	reconcileAdapter(t, r, "acme")
	if writes != 1 {
		t.Errorf("status writes = %d over two unchanged reconciles, want 1 (RA-1)", writes)
	}
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 1 {
		t.Errorf("connectedRobots = %d, want 1", got)
	}
}

// A List failure must not publish a wrong count. The previous value stands, so a transient API
// error never makes an operator think the fleet vanished.
func TestConnectedRobots_ListErrorKeepsPreviousCount(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	fa := countAdapter("acme")
	fa.Status.ConnectedRobots = 7 // a previously published count
	failList := false
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fa, countRobot("amr-1", "acme", fleetv1.RobotPhaseIdle)).
		WithStatusSubresource(&fleetv1.FleetAdapter{}, &fleetv1.Robot{}).
		WithIndex(&fleetv1.Robot{}, robotAdapterNameField, func(o client.Object) []string {
			if n := o.(*fleetv1.Robot).Spec.Adapter.Name; n != "" {
				return []string{n}
			}
			return nil
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isRobots := list.(*fleetv1.RobotList); isRobots && failList {
					return errors.New("simulated API outage")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	now := time.Unix(1000, 0)
	r := &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}

	failList = true
	reconcileAdapter(t, r, "acme")
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 7 {
		t.Errorf("connectedRobots = %d after a List error, want the previous 7 retained "+
			"(a transient failure must not report an empty fleet)", got)
	}

	// Recovery republishes the true count.
	failList = false
	reconcileAdapter(t, r, "acme")
	if got := getAdapter(t, c, "acme").Status.ConnectedRobots; got != 1 {
		t.Errorf("connectedRobots = %d after recovery, want 1", got)
	}
}
