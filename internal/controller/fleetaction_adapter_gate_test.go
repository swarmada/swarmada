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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// ADR-0032 assignment gate: a robot is dispatch-eligible only while its bound FleetAdapter is
// Connected AND conformance Passed. The admission gate checks that once; this re-checks it at
// dispatch, FAIL CLOSED, because readiness flips afterwards (heartbeats lapse → Degraded →
// Disconnected; a report digest/ConfigMap change → Failed).

// adapterIn builds the fixture adapter in a given readiness state.
func adapterIn(phase fleetv1.FleetAdapterPhase, conf fleetv1.ConformanceState) *fleetv1.FleetAdapter {
	a := readyActionAdapter()
	a.Status.Phase, a.Status.Conformance = phase, conf
	return a
}

// pendingActionFor is a Pending action wanting any idle robot.
func pendingActionFor(name string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
}

func phaseOfAction(t *testing.T, c client.Client, name string) fleetv1.ActionPhase {
	t.Helper()
	var a fleetv1.FleetAction
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: actionNS}, &a); err != nil {
		t.Fatalf("get action: %v", err)
	}
	return a.Status.Phase
}

// A ready adapter assigns — the positive control, so a failure below means the gate bit, not that
// the fixture never worked.
func TestAssignmentGate_ReadyAdapterAssigns(t *testing.T) {
	r, c := newActionReconciler(t, pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned (a Connected/Passed adapter must not be gated)", got)
	}
}

// Every not-fit readiness state withholds the robot: the action stays Pending rather than being
// dispatched to a robot whose adapter cannot be trusted with work.
func TestAssignmentGate_UnfitAdapterWithholdsDispatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase fleetv1.FleetAdapterPhase
		conf  fleetv1.ConformanceState
	}{
		{"degraded", fleetv1.FleetAdapterPhaseDegraded, fleetv1.ConformanceStatePassed},
		{"disconnected", fleetv1.FleetAdapterPhaseDisconnected, fleetv1.ConformanceStatePassed},
		{"conformance failed", fleetv1.FleetAdapterPhaseConnected, fleetv1.ConformanceStateFailed},
		{"conformance unknown", fleetv1.FleetAdapterPhaseConnected, fleetv1.ConformanceStateUnknown},
		{"phase unset", "", fleetv1.ConformanceStatePassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newActionReconciler(t, adapterIn(tc.phase, tc.conf),
				pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
			reconcileAction(t, r, "t1")
			if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhasePending {
				t.Errorf("phase = %s, want Pending (phase=%q conformance=%q must not receive work)",
					got, tc.phase, tc.conf)
			}
			var robot fleetv1.Robot
			if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: actionNS}, &robot); err != nil {
				t.Fatalf("get robot: %v", err)
			}
			if robot.Status.AssignedAction != "" {
				t.Errorf("robot was bound to %q despite an unfit adapter", robot.Status.AssignedAction)
			}
		})
	}
}

// FAIL CLOSED: the adapter object does not exist at all.
func TestAssignmentGate_MissingAdapterFailsClosed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	// Deliberately NOT seeding a FleetAdapter (bypassing newActionReconciler's fixture).
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, "")).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).Build()
	r := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler()}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "t1", Namespace: actionNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending (a robot whose adapter does not exist must not be dispatched to)", got)
	}
}

// FAIL CLOSED: the robot carries no spec.adapter.name, so no adapter is authorized for it.
func TestAssignmentGate_RobotWithNoAdapterNameFailsClosed(t *testing.T) {
	robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "")
	robot.Spec.Adapter = fleetv1.AdapterRef{}
	r, c := newActionReconciler(t, pendingActionFor("t1"), robot)
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending (an unbound robot must not be dispatched to)", got)
	}
}

// FAIL CLOSED: an API error reading the adapter is not an implicit pass.
func TestAssignmentGate_AdapterReadErrorFailsClosed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(readyActionAdapter(), pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, "")).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isAdapter := obj.(*fleetv1.FleetAdapter); isAdapter {
					return errors.New("simulated apiserver failure reading the FleetAdapter")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler()}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "t1", Namespace: actionNS}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending (an unreadable adapter must fail closed, not pass)", got)
	}
}

// The gate also covers the PREEMPTION search, which reads the same candidate slice: a Critical
// action must not displace a lower-band action onto a robot whose adapter is unfit.
func TestAssignmentGate_CoversPreemptionPath(t *testing.T) {
	victim := assignedAction("victim", "r1", fleetv1.ActionPhaseInProgress, 3, nil)
	victim.Spec.Priority = fleetv1.ActionPriorityNormal
	preemptor := pendingActionFor("crit")
	preemptor.Spec.Priority = fleetv1.ActionPriorityCritical

	// The only robot is busy with the victim, so assignment can only proceed by preemption —
	// and its adapter is Degraded.
	r, c := newActionReconciler(t, adapterIn(fleetv1.FleetAdapterPhaseDegraded, fleetv1.ConformanceStatePassed),
		victim, preemptor, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "victim"))
	reconcileAction(t, r, "crit")

	if got := phaseOfAction(t, c, "crit"); got != fleetv1.ActionPhasePending {
		t.Errorf("preemptor phase = %s, want Pending (no preemption onto an unfit adapter)", got)
	}
	if got := phaseOfAction(t, c, "victim"); got == fleetv1.ActionPhasePreempted {
		t.Error("victim was Preempted even though the preemptor could not be dispatched")
	}
}

// SCOPE: the gate must not revoke work already in flight. An adapter degrading under a running
// action is the lease/Revoking machinery's business (§9.6.3.5), not this filter's — yanking it here
// would be an unconfirmed stop.
func TestAssignmentGate_DoesNotRevokeRunningWork(t *testing.T) {
	running := assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 3, nil)
	r, c := newActionReconciler(t, adapterIn(fleetv1.FleetAdapterPhaseDisconnected, fleetv1.ConformanceStateFailed),
		running, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"))
	reconcileAction(t, r, "t1")
	if got := phaseOfAction(t, c, "t1"); got != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s, want InProgress (the dispatch gate must never revoke running work)", got)
	}
}

// The filter itself: memoized (N robots on one adapter cost one Get) and order-preserving, so
// scheduler ranking is untouched.
func TestFilterDispatchEligible_MemoizesAndPreservesOrder(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	gets := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(readyActionAdapter()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isAdapter := obj.(*fleetv1.FleetAdapter); isAdapter {
					gets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler()}

	in := []fleetv1.Robot{
		*robotInPhase("r1", fleetv1.RobotPhaseIdle, ""),
		*robotInPhase("r2", fleetv1.RobotPhaseIdle, ""),
		*robotInPhase("r3", fleetv1.RobotPhaseIdle, ""),
	}
	out, excluded := r.filterDispatchEligible(context.Background(), in)
	if len(out) != 3 || len(excluded) != 0 {
		t.Fatalf("eligible = %d, excluded = %d; want 3/0", len(out), len(excluded))
	}
	if gets != 1 {
		t.Errorf("adapter Gets = %d, want 1 (the per-call memo must collapse the shared adapter)", gets)
	}
	for i, want := range []string{"r1", "r2", "r3"} {
		if out[i].Name != want {
			t.Errorf("out[%d] = %s, want %s (input order must be preserved)", i, out[i].Name, want)
		}
	}
}

// A mixed fleet: only the robots on the fit adapter survive, and each exclusion carries a reason an
// operator can act on.
func TestFilterDispatchEligible_MixedFleetReportsReasons(t *testing.T) {
	unfit := adapterIn(fleetv1.FleetAdapterPhaseDegraded, fleetv1.ConformanceStatePassed)
	unfit.Name = "adapter-b"
	r, _ := newActionReconciler(t, unfit)

	onB := robotInPhase("r2", fleetv1.RobotPhaseIdle, "")
	onB.Spec.Adapter = fleetv1.AdapterRef{Name: "adapter-b"}
	missing := robotInPhase("r3", fleetv1.RobotPhaseIdle, "")
	missing.Spec.Adapter = fleetv1.AdapterRef{Name: "adapter-gone"}

	out, excluded := r.filterDispatchEligible(context.Background(),
		[]fleetv1.Robot{*robotInPhase("r1", fleetv1.RobotPhaseIdle, ""), *onB, *missing})

	if len(out) != 1 || out[0].Name != "r1" {
		t.Fatalf("eligible = %v, want only r1", names(out))
	}
	if len(excluded) != 2 {
		t.Fatalf("excluded = %d, want 2", len(excluded))
	}
	for _, e := range excluded {
		if e.Reason == "" {
			t.Errorf("robot %s excluded with no reason (an operator cannot debug a silent withhold)", e.Robot)
		}
	}
}

func names(rs []fleetv1.Robot) []string {
	out := make([]string, 0, len(rs))
	for i := range rs {
		out = append(out, rs[i].Name)
	}
	return out
}
