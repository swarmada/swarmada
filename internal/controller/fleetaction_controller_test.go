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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/scheduler"
)

const actionNS = "warehouse-a"

// testActionAdapter is the FleetAdapter every fixture robot is bound to.
const testActionAdapter = "adapter-a"

// readyActionAdapter is a dispatch-ready adapter: Connected + conformance Passed (ADR-0032).
func readyActionAdapter() *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: testActionAdapter, Namespace: actionNS},
		Status: fleetv1.FleetAdapterStatus{
			Phase:       fleetv1.FleetAdapterPhaseConnected,
			Conformance: fleetv1.ConformanceStatePassed,
			// ADR-0032: dispatch requires the conformance result to be bound to a supported contract
			// version, so a "ready" adapter has to say which one it was earned against. An unset value
			// is deliberately NOT ready (see fleetaction_adapter_gate_test.go).
			ConformanceContractVersion: contract.Version,
		},
	}
}

// boundAdapter is the spec binding every fixture robot carries, matching readyActionAdapter.
func boundAdapter() fleetv1.AdapterRef { return fleetv1.AdapterRef{Name: testActionAdapter} }

func newActionReconciler(t *testing.T, objs ...client.Object) (*FleetActionReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	// The ADR-0032 assignment gate requires each candidate robot's bound FleetAdapter to be
	// Connected + conformance Passed, so every fixture gets one (testActionAdapter). A test that
	// wants the gate to BITE overrides this object or points a robot elsewhere — see
	// fleetaction_adapter_gate_test.go.
	// Seed it only if the caller has not supplied one under that name, so a test can OVERRIDE the
	// adapter's readiness (adapterIn) without colliding with the default fixture.
	hasAdapter := false
	for _, o := range objs {
		if a, ok := o.(*fleetv1.FleetAdapter); ok && a.Name == testActionAdapter {
			hasAdapter = true
			break
		}
	}
	if !hasAdapter {
		objs = append([]client.Object{readyActionAdapter()}, objs...)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).
		Build()
	return &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler()}, c
}

func reconcileAction(t *testing.T, r *FleetActionReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: actionNS},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getAction(t *testing.T, c client.Client, name string) *fleetv1.FleetAction {
	t.Helper()
	ft := &fleetv1.FleetAction{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: actionNS}, ft); err != nil {
		t.Fatalf("get task: %v", err)
	}
	return ft
}

func assignedAction(name, robot string, phase fleetv1.ActionPhase, gen int64, lease *metav1.Time) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status: fleetv1.FleetActionStatus{
			Phase:                phase,
			AssignedRobot:        robot,
			AssignmentGeneration: gen,
			LeaseExpiresAt:       lease,
		},
	}
}

// actionCancelConfig builds a namespace SwarmadaConfig carrying an onDisconnect
// disposition so the FleetAction reconciler can resolve actionCancellation policy.
func actionCancelConfig(policy fleetv1.ActionCancellationPolicy, timeoutSecs int32) *fleetv1.SwarmadaConfig {
	tc := fleetv1.SwarmadaActionCancellationConfig{OnDisconnect: policy}
	if policy == fleetv1.ActionCancellationAfterTimeout {
		tc.DisconnectTimeoutSeconds = &timeoutSecs
	}
	return &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmada", Namespace: actionNS},
		Spec:       fleetv1.SwarmadaConfigSpec{ActionCancellation: tc},
	}
}

func robotInPhase(name string, phase fleetv1.RobotPhase, assignedAction string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.RobotSpec{Adapter: boundAdapter()},
		Status:     fleetv1.RobotStatus{Phase: phase, AssignedAction: assignedAction},
	}
}

// robotWithCaps builds a reachable robot (InProgress on the given action) with the
// supplied capability statuses, for capability-loss reassignment tests.
func robotWithCaps(name, assignedAction string, caps ...fleetv1.CapabilityStatusEntry) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.RobotSpec{Adapter: boundAdapter()},
		Status: fleetv1.RobotStatus{
			Phase:          fleetv1.RobotPhaseInProgress,
			AssignedAction: assignedAction,
			Capabilities:   caps,
		},
	}
}

// camNavAction requires navigation + camera_front; acceptDegraded sets the action field.
func camNavAction(name, robot string, lease *metav1.Time, acceptDegraded *bool) *fleetv1.FleetAction {
	ft := assignedAction(name, robot, fleetv1.ActionPhaseInProgress, 3, lease)
	ft.Spec.RequiredCapabilities = []string{"navigation", "camera_front"}
	ft.Spec.AcceptDegradedCapabilities = acceptDegraded
	return ft
}

// Capability-loss reassignment — a reachable robot whose required capability degrades
// (acceptDegraded=false) is marked for the confirmed-stop requeue, WITHOUT a bare
// status flip: the action stays bound to the robot until the requeue path confirms a stop.
func TestReconcile_CapabilityLoss_InitiatesReassignment(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		camNavAction("t1", "r1", lease, nil),
		robotWithCaps("r1", "t1",
			fleetv1.CapabilityStatusEntry{Name: "navigation", Status: fleetv1.CapabilityStatusActive},
			fleetv1.CapabilityStatusEntry{Name: "camera_front", Status: fleetv1.CapabilityStatusDegraded},
		),
	)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Annotations[annRequeueRequested] != reasonCapabilityLost {
		t.Fatalf("capability loss should mark the task for requeue; annotation=%q", ft.Annotations[annRequeueRequested])
	}
	// Single-executor: no bare flip — the robot stays bound until a confirmed stop.
	if ft.Status.AssignedRobot != "r1" {
		t.Fatalf("robot was released without a confirmed stop (assignedRobot=%q) — RA-4 hazard", ft.Status.AssignedRobot)
	}
	if ft.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase flipped before confirmed stop: %s", ft.Status.Phase)
	}
}

// acceptDegradedCapabilities=true: a merely-Degraded required capability still
// satisfies, so NO reassignment is initiated (the action keeps running).
func TestReconcile_CapabilityLoss_AcceptDegradedNoReassign(t *testing.T) {
	yes := true
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		camNavAction("t1", "r1", lease, &yes),
		robotWithCaps("r1", "t1",
			fleetv1.CapabilityStatusEntry{Name: "navigation", Status: fleetv1.CapabilityStatusActive},
			fleetv1.CapabilityStatusEntry{Name: "camera_front", Status: fleetv1.CapabilityStatusDegraded},
		),
	)

	reconcileAction(t, r, "t1")

	if ft := getAction(t, c, "t1"); ft.Annotations[annRequeueRequested] != "" {
		t.Fatalf("acceptDegraded task must NOT be reassigned on Degraded; annotation=%q", ft.Annotations[annRequeueRequested])
	}
}

// A fully-Active robot is not reassigned (regression guard: the check must not fire
// on a healthy robot).
func TestReconcile_CapabilityActive_NoReassign(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		camNavAction("t1", "r1", lease, nil),
		robotWithCaps("r1", "t1",
			fleetv1.CapabilityStatusEntry{Name: "navigation", Status: fleetv1.CapabilityStatusActive},
			fleetv1.CapabilityStatusEntry{Name: "camera_front", Status: fleetv1.CapabilityStatusActive},
		),
	)

	reconcileAction(t, r, "t1")

	if ft := getAction(t, c, "t1"); ft.Annotations[annRequeueRequested] != "" {
		t.Fatalf("a healthy robot must not be reassigned; annotation=%q", ft.Annotations[annRequeueRequested])
	}
}

// Case 1 — Connectivity loss: InProgress robot goes Offline → action Revoking, robot
// stays bound, and NO reassignment happens while the lease is alive.
func TestReconcile_ConnectivityLoss_RevokesWithoutReassign(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 3, lease),
		robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
	)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseRevoking {
		t.Fatalf("phase = %s, want Revoking", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Fatalf("robot was released on connectivity loss (assignedRobot=%q) — must stay bound until provable lease death", ft.Status.AssignedRobot)
	}
	if ft.Status.AssignmentGeneration != 3 {
		t.Fatalf("generation changed on revoke: %d, want 3", ft.Status.AssignmentGeneration)
	}

	// A second reconcile while the lease is still alive must NOT reassign.
	reconcileAction(t, r, "t1")
	if ft2 := getAction(t, c, "t1"); ft2.Status.Phase != fleetv1.ActionPhaseRevoking || ft2.Status.AssignedRobot != "r1" {
		t.Fatalf("reassigned before lease horizon: phase=%s robot=%q — DOUBLE-EXECUTION HAZARD", ft2.Status.Phase, ft2.Status.AssignedRobot)
	}
}

// Case 1b — onDisconnect=Never (the default when no SwarmadaConfig is present):
// even once the lease is PROVABLY dead, a disconnected action is NOT auto-reassigned.
// It holds in Revoking with its robot still bound, awaiting an operator cancel
// (§9.1.11.9 — safest for actions with physical side effects).
func TestReconcile_NeverHoldsRevokingOnDisconnect(t *testing.T) {
	expired := &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, expired),
		robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
	)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseRevoking {
		t.Fatalf("phase = %s, want Revoking held under onDisconnect=Never", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "r1" || ft.Status.LeaseExpiresAt == nil {
		t.Fatalf("Never must keep the binding for the operator: robot=%q lease=%v",
			ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt)
	}
	if ft.Status.Message != revokingHeldMessage {
		t.Errorf("message = %q, want the operator-cancel hold message", ft.Status.Message)
	}
	// The robot is quarantined to this action until the operator cancels.
	rb := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: actionNS}, rb); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if rb.Status.AssignedAction != "t1" {
		t.Errorf("robot binding = %q, want it held at t1 under Never", rb.Status.AssignedAction)
	}
}

// Case 1c — onDisconnect=AfterTimeout: a disconnected action HOLDS Revoking until the
// wall-clock ceiling (disconnectTimeoutSeconds, measured from DisconnectedAt) has
// elapsed on top of the provably-dead lease; only then does it auto-reassign.
func TestReconcile_AfterTimeoutHoldsThenReassigns(t *testing.T) {
	expired := &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}

	// Before the ceiling: DisconnectedAt is recent, ceiling is 30s → still holding.
	t.Run("before ceiling holds", func(t *testing.T) {
		action := assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, expired)
		action.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-5 * time.Second)}
		r, c := newActionReconciler(t, action,
			robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
			actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30),
		)

		res := reconcileAction(t, r, "t1")

		ft := getAction(t, c, "t1")
		if ft.Status.Phase != fleetv1.ActionPhaseRevoking {
			t.Fatalf("phase = %s, want Revoking held before the AfterTimeout ceiling", ft.Status.Phase)
		}
		if ft.Status.AssignedRobot != "r1" {
			t.Errorf("binding dropped before the ceiling: robot=%q", ft.Status.AssignedRobot)
		}
		if res.RequeueAfter <= 0 {
			t.Errorf("expected a requeue at the remaining ceiling, got %v", res.RequeueAfter)
		}
	})

	// After the ceiling: DisconnectedAt is older than the 30s ceiling → reassign.
	t.Run("after ceiling reassigns", func(t *testing.T) {
		action := assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, expired)
		action.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-60 * time.Second)}
		r, c := newActionReconciler(t, action,
			robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
			actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30),
		)

		reconcileAction(t, r, "t1")

		ft := getAction(t, c, "t1")
		if ft.Status.Phase != fleetv1.ActionPhasePending {
			t.Fatalf("phase = %s, want Pending after the AfterTimeout ceiling", ft.Status.Phase)
		}
		if ft.Status.AssignedRobot != "" || ft.Status.LeaseExpiresAt != nil || ft.Status.DisconnectedAt != nil {
			t.Fatalf("reassign did not clear state: robot=%q lease=%v disconnectedAt=%v",
				ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt, ft.Status.DisconnectedAt)
		}
	})
}

// Case 1d — onDisconnect=WhenActionExpired: a disconnected action whose per-action
// completion window (spec.timeoutSeconds from StartTime) has passed is TERMINATED
// (expiry renders completion moot, §9.1.11.9), not reassigned; one still within its
// window — or one with no timeout — holds like Never.
func TestReconcile_WhenActionExpiredCancelsElapsedRevoking(t *testing.T) {
	expired := &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}
	timeoutSecs := int32(60)

	// Started 10 minutes ago with a 60s completion window → expired → Cancelled.
	t.Run("expired window cancels", func(t *testing.T) {
		action := assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, expired)
		action.Spec.TimeoutSeconds = &timeoutSecs
		action.Status.StartTime = &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}
		r, c := newActionReconciler(t, action,
			robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
			actionCancelConfig(fleetv1.ActionCancellationWhenActionExpired, 0),
		)

		reconcileAction(t, r, "t1")

		ft := getAction(t, c, "t1")
		if ft.Status.Phase != fleetv1.ActionPhaseCancelled {
			t.Fatalf("phase = %s, want Cancelled for an expired task", ft.Status.Phase)
		}
		if ft.Status.AssignedRobot != "" || ft.Status.LeaseExpiresAt != nil || ft.Status.CompletionTime == nil {
			t.Fatalf("expiry did not finalize cleanly: robot=%q lease=%v completion=%v",
				ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt, ft.Status.CompletionTime)
		}
		rb := &fleetv1.Robot{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "r1", Namespace: actionNS}, rb); err != nil {
			t.Fatalf("get robot: %v", err)
		}
		if rb.Status.AssignedAction != "" {
			t.Errorf("robot binding = %q, want it freed on expiry-cancel", rb.Status.AssignedAction)
		}
	})

	// Started 5 seconds ago with a 600s window → not expired → holds like Never.
	t.Run("within window holds", func(t *testing.T) {
		wide := int32(600)
		action := assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, expired)
		action.Spec.TimeoutSeconds = &wide
		action.Status.StartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Second)}
		r, c := newActionReconciler(t, action,
			robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
			actionCancelConfig(fleetv1.ActionCancellationWhenActionExpired, 0),
		)

		reconcileAction(t, r, "t1")

		ft := getAction(t, c, "t1")
		if ft.Status.Phase != fleetv1.ActionPhaseRevoking || ft.Status.AssignedRobot != "r1" {
			t.Fatalf("a task within its window must hold Revoking bound: phase=%s robot=%q",
				ft.Status.Phase, ft.Status.AssignedRobot)
		}
	})

	// No timeoutSeconds → cannot expire → holds like Never (fail-safe).
	t.Run("no timeout holds", func(t *testing.T) {
		action := assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, expired)
		action.Status.StartTime = &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}
		r, c := newActionReconciler(t, action,
			robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
			actionCancelConfig(fleetv1.ActionCancellationWhenActionExpired, 0),
		)

		reconcileAction(t, r, "t1")

		if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhaseRevoking {
			t.Fatalf("a task with no timeout cannot expire — want Revoking held, got %s", ft.Status.Phase)
		}
	})
}

// Case 2 — Control-plane restart/failover: a fresh reconcile reads the PERSISTED
// generation and mints strictly-greater on the next assignment; it never reuses.
func TestReconcile_GenerationMonotonicAcrossRestart(t *testing.T) {
	// A Pending action carrying a persisted high-water generation of 7 (as if a prior
	// assignment happened before a restart) plus an eligible Idle robot.
	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending, AssignmentGeneration: 7},
	}
	idle := robotInPhase("r1", fleetv1.RobotPhaseIdle, "")
	r, c := newActionReconciler(t, pending, idle)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned", ft.Status.Phase)
	}
	if ft.Status.AssignmentGeneration != 8 {
		t.Fatalf("generation = %d, want 8 (strictly greater than persisted 7, never reused)", ft.Status.AssignmentGeneration)
	}
	if ft.Status.LeaseExpiresAt == nil {
		t.Fatal("lease horizon not established on assignment")
	}
}

// Case 3 — Delayed/duplicate message: repeated reconciles of a Revoking action with a
// live lease are idempotent — no spurious reassignment, generation unchanged.
func TestReconcile_DuplicateReconcileIsIdempotent(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 5, lease),
		robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
	)

	for i := 0; i < 4; i++ {
		reconcileAction(t, r, "t1")
		ft := getAction(t, c, "t1")
		if ft.Status.Phase != fleetv1.ActionPhaseRevoking {
			t.Fatalf("iter %d: phase = %s, want stable Revoking", i, ft.Status.Phase)
		}
		if ft.Status.AssignedRobot != "r1" || ft.Status.AssignmentGeneration != 5 {
			t.Fatalf("iter %d: state drifted: robot=%q gen=%d", i, ft.Status.AssignedRobot, ft.Status.AssignmentGeneration)
		}
	}
}

// A brief drop that recovers while the lease is live re-adopts the action at the
// SAME generation (no reassignment, no new generation).
func TestReconcile_ReadoptOnLiveLease(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 5, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"), // reachable, still holding t1
	)

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s, want InProgress (re-adopted)", ft.Status.Phase)
	}
	if ft.Status.AssignmentGeneration != 5 {
		t.Fatalf("generation changed on re-adopt: %d, want 5 (same generation)", ft.Status.AssignmentGeneration)
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Fatalf("robot changed on re-adopt: %q", ft.Status.AssignedRobot)
	}
}

// Regression test for a real, observed double-assignment race: the
// Pending→Assigned commit used to patch the ROBOT status first (setting
// AssignedAction, which triggers a fresh reconcile of this same action via the
// robotToAction watch in SetupWithManager) and the TASK status second, using a
// plain merge patch with no concurrency precondition. A second, racing
// reconcile that had read the action before the first one's write could still
// see Phase=Pending, independently select a DIFFERENT robot, and commit it
// too — leaving two robots both showing status.assignedAction for the same
// action, which is exactly the symptom this test guards against.
//
// The fix commits the action FIRST, gated on an optimistic-lock precondition
// (resourceVersion), before ever touching a robot. This test proves that
// precondition actually rejects a stale, racing commit — using the exact
// same Patch call the reconciler makes — rather than silently overwriting
// the winning assignment.
func TestReconcile_StaleConassignedActionCommitIsRejected(t *testing.T) {
	pending := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	r1 := robotInPhase("r1", fleetv1.RobotPhaseIdle, "")
	r1.Status.BatteryPercent = p32(90)
	r2 := robotInPhase("r2", fleetv1.RobotPhaseIdle, "")
	r2.Status.BatteryPercent = p32(50)
	r, c := newActionReconciler(t, pending, r1, r2)

	// Snapshot the action the way a second, racing reconcile would have read it
	// BEFORE the real reconcile below commits — same resourceVersion, still
	// Pending.
	stale := &fleetv1.FleetAction{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "t1", Namespace: actionNS}, stale); err != nil {
		t.Fatalf("get stale snapshot: %v", err)
	}
	stale = stale.DeepCopy()

	// The real reconcile runs to completion: the scheduler picks the
	// higher-battery robot (r1) and commits.
	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseAssigned || ft.Status.AssignedRobot != "r1" {
		t.Fatalf("setup: phase=%s robot=%q, want Assigned/r1", ft.Status.Phase, ft.Status.AssignedRobot)
	}

	// Simulate the LOSING side of the race: a second reconcile that read the
	// action at the stale (pre-commit) resourceVersion independently decided to
	// assign it to r2, and now attempts the exact same commit call the
	// reconciler makes. With the fix's optimistic-lock precondition, this
	// must be rejected.
	raced := stale.DeepCopy()
	raced.Status.Phase = fleetv1.ActionPhaseAssigned
	raced.Status.AssignedRobot = "r2"
	raced.Status.AssignmentGeneration++
	lease := metav1.NewTime(time.Now().Add(leaseDuration))
	raced.Status.LeaseExpiresAt = &lease

	err := c.Status().Patch(context.Background(), raced, client.MergeFromWithOptions(stale, client.MergeFromWithOptimisticLock{}))
	if err == nil {
		t.Fatal("stale racing commit succeeded — double-assignment hazard reintroduced")
	}
	if !errors.IsConflict(err) {
		t.Fatalf("expected a 409 conflict rejecting the stale commit, got: %v", err)
	}

	// The action must still show the FIRST (winning) commit untouched, and no
	// second robot must ever have been marked as executing it.
	final := getAction(t, c, "t1")
	if final.Status.AssignedRobot != "r1" {
		t.Fatalf("task assignment changed after a rejected race: %q, want r1", final.Status.AssignedRobot)
	}
	rb2 := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "r2", Namespace: actionNS}, rb2); err != nil {
		t.Fatalf("get r2: %v", err)
	}
	if rb2.Status.AssignedAction != "" {
		t.Fatalf("r2 shows AssignedAction=%q after a rejected commit — this is the exact double-assignment symptom being guarded against", rb2.Status.AssignedAction)
	}
}
