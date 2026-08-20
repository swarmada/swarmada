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
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
)

// fakeTDE returns a canned reservation result and counts calls.
type fakeTDE struct {
	result       tde.ReservationResult
	requests     int
	releases     int
	phaseChanges int
	lastPhase    fleetv1.ActionPhase
}

func (f *fakeTDE) RequestReservation(context.Context, tde.ReservationRequest) (tde.ReservationResult, error) {
	f.requests++
	return f.result, nil
}
func (f *fakeTDE) ReleaseReservation(context.Context, string, string, string) error {
	f.releases++
	return nil
}
func (f *fakeTDE) OnRobotEnteredZone(context.Context, string, string, string) error { return nil }
func (f *fakeTDE) OnRobotExitedZone(context.Context, string, string, string) error  { return nil }
func (f *fakeTDE) OnActionPhaseChanged(_ context.Context, _, _, _ string, phase fleetv1.ActionPhase) error {
	f.phaseChanges++
	f.lastPhase = phase
	return nil
}
func (f *fakeTDE) ZoneStatus(context.Context, string, string) (tde.ZoneReservationStatus, error) {
	return tde.ZoneReservationStatus{}, nil
}

func zonedAction(name, zone string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: zone},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
}

func idleRobotInZone(name, zone string) *fleetv1.Robot {
	r := robotInPhase(name, fleetv1.RobotPhaseIdle, "")
	r.Spec.Zone = zone
	return r
}

// The gate must BLOCK the commit on Denied: no assignment may bypass the TDE.
func TestTDEGate_DeniedBlocksAssignment(t *testing.T) {
	r, c := newActionReconciler(t, zonedAction("t1", "z"), idleRobotInZone("r1", "z"))
	ft := &fakeTDE{result: tde.ReservationResult{Status: tde.Denied, DeniedReason: tde.DeniedZoneCapacity}}
	r.TDE = ft

	reconcileAction(t, r, "t1")

	if ft.requests != 1 {
		t.Fatalf("TDE consulted %d times, want 1 (gate must run before commit)", ft.requests)
	}
	got := getAction(t, c, "t1")
	if got.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending — a Denied reservation must NOT commit the assignment", got.Status.Phase)
	}
	if got.Status.AssignedRobot != "" || got.Status.AssignmentGeneration != 0 {
		t.Fatalf("assignment committed despite TDE Denied: robot=%q gen=%d", got.Status.AssignedRobot, got.Status.AssignmentGeneration)
	}
}

// The gate must ALLOW the commit on Granted.
func TestTDEGate_GrantedCommitsAssignment(t *testing.T) {
	r, c := newActionReconciler(t, zonedAction("t1", "z"), idleRobotInZone("r1", "z"))
	ft := &fakeTDE{result: tde.ReservationResult{Status: tde.Granted}}
	r.TDE = ft

	reconcileAction(t, r, "t1")

	if ft.requests != 1 {
		t.Fatalf("TDE consulted %d times, want 1", ft.requests)
	}
	got := getAction(t, c, "t1")
	if got.Status.Phase != fleetv1.ActionPhaseAssigned || got.Status.AssignedRobot != "r1" {
		t.Fatalf("Granted reservation did not commit: phase=%s robot=%q", got.Status.Phase, got.Status.AssignedRobot)
	}
	if got.Status.AssignmentGeneration != 1 {
		t.Fatalf("generation = %d, want 1", got.Status.AssignmentGeneration)
	}
}

// Reserve-then-crash: if the assignment commit write fails AFTER a successful
// reserve, the gate MUST release the reservation (Unreserve) so no phantom slot
// leaks. Simulated by a client whose status Patch always errors.
func TestTDEGate_UnreservesOnCommitFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(readyActionAdapter(), zonedAction("t1", "z"), idleRobotInZone("r1", "z")).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
				return errors.New("simulated commit-write failure")
			},
		}).
		Build()
	ft := &fakeTDE{result: tde.ReservationResult{Status: tde.Granted}}
	r := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler(), TDE: ft}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "t1", Namespace: actionNS},
	})
	if err == nil {
		t.Fatal("expected a commit-write error")
	}
	if ft.requests != 1 {
		t.Fatalf("reserve calls = %d, want 1", ft.requests)
	}
	if ft.releases != 1 {
		t.Fatalf("Unreserve NOT called after commit failure (releases=%d) — a leaked reservation is a phantom slot", ft.releases)
	}
}

// A terminal action releases its zone reservation (§9.4.2).
func TestTDELifecycle_ReleasesOnTerminal(t *testing.T) {
	done := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z"},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseSucceeded},
	}
	r, _ := newActionReconciler(t, done)
	ft := &fakeTDE{}
	r.TDE = ft

	reconcileAction(t, r, "t1")

	if ft.releases != 1 {
		t.Fatalf("terminal task released reservation %d times, want 1", ft.releases)
	}
}

// Entering Revoking extends the reservation TTL through the disconnect window.
func TestTDELifecycle_ExtendsTTLOnRevoking(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	action := assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 3, lease)
	action.Spec.Zone = "z"
	r, _ := newActionReconciler(t, action, robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"))
	ft := &fakeTDE{}
	r.TDE = ft

	reconcileAction(t, r, "t1")

	if ft.phaseChanges != 1 || ft.lastPhase != fleetv1.ActionPhaseRevoking {
		t.Fatalf("OnActionPhaseChanged = %d call(s), last=%s; want 1 Revoking", ft.phaseChanges, ft.lastPhase)
	}
}

// preemptScenario builds a Critical Pending action (zone z), a Normal InProgress
// victim on r1, and the robot r1 (InProgress in zone z, eligible).
func preemptScenario(t *testing.T, tdeResult tde.ReservationResult) (*FleetActionReconciler, client.Client) {
	t.Helper()
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	crit := criticalPending("crit")
	crit.Spec.Zone = "z"
	victim := bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease)
	robot := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm")
	robot.Spec.Zone = "z"
	r, c := newActionReconciler(t, crit, victim, robot)
	r.TDE = &fakeTDE{result: tdeResult}
	return r, c
}

// Integrated preemption: a TDE Denial must NOT strand a §C preemption victim —
// the victim is marked only after the gate grants.
func TestPreempt_NoNeedlessPreemptWhenTDEDenies(t *testing.T) {
	r, c := preemptScenario(t, tde.ReservationResult{Status: tde.Denied, DeniedReason: tde.DeniedZoneCapacity})

	reconcileAction(t, r, "crit")

	if v := getAction(t, c, "norm").Status.Phase; v != fleetv1.ActionPhaseInProgress {
		t.Fatalf("victim phase = %s — a TDE Denial must NOT preempt the §C victim", v)
	}
	if g := getAction(t, c, "crit").Status.Phase; g != fleetv1.ActionPhasePending {
		t.Fatalf("Critical phase = %s, want Pending on TDE Denied", g)
	}
}

// Integrated preemption: when the TDE grants, the §C victim is preempted and the
// Critical action takes the freed robot.
func TestPreempt_CommitsVictimWhenTDEGrants(t *testing.T) {
	r, c := preemptScenario(t, tde.ReservationResult{Status: tde.Granted})

	reconcileAction(t, r, "crit")

	if v := getAction(t, c, "norm").Status.Phase; v != fleetv1.ActionPhasePreempted {
		t.Fatalf("victim phase = %s, want Preempted once the gate granted", v)
	}
	crit := getAction(t, c, "crit")
	if crit.Status.Phase != fleetv1.ActionPhaseAssigned || crit.Status.AssignedRobot != "r1" {
		t.Fatalf("Critical not assigned to freed robot: phase=%s robot=%q", crit.Status.Phase, crit.Status.AssignedRobot)
	}
}

func TestClampRetryAfter(t *testing.T) {
	if got := clampRetryAfter(0, tdeMinRetryAfter, tdeMaxRetryAfter); got != tdeMinRetryAfter {
		t.Errorf("clamp(0) = %v, want %v", got, tdeMinRetryAfter)
	}
	if got := clampRetryAfter(5*time.Minute, tdeMinRetryAfter, tdeMaxRetryAfter); got != tdeMaxRetryAfter {
		t.Errorf("clamp(5m) = %v, want %v", got, tdeMaxRetryAfter)
	}
	if got := clampRetryAfter(30*time.Second, tdeMinRetryAfter, tdeMaxRetryAfter); got != 30*time.Second {
		t.Errorf("clamp(30s) = %v, want 30s", got)
	}
}

// A nil TDE (unit-test only) skips the gate — backward-compatible with the
// existing lease/estop/preempt tests that don't configure a TDE.
func TestTDEGate_NilTDESkipsGate(t *testing.T) {
	r, c := newActionReconciler(t, zonedAction("t1", "z"), idleRobotInZone("r1", "z"))
	r.TDE = nil // explicit

	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatalf("nil TDE should not block assignment: phase=%s", got.Status.Phase)
	}
}
