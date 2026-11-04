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
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// The post-clear supervised window, end to end against a real API server: an
// operator clears a confirmed stop, the robot is observably Resuming, it is NOT
// dispatched while it is — not even once healthy telemetry has arrived after the
// clear — and it reaches Normal only when an operator releases it (RFC-0001
// #safety-estop-state-machine).
//
// Run against envtest rather than a fake client for two reasons a fake client
// cannot cover: `Resuming` is a validated CRD enum value
// (config/crd/bases/swarmada.io_robots.yaml), so a real API server is what proves
// the control plane can actually persist the state it is specified to author; and
// the transitions go through the real status subresource and merge-patch semantics
// the dispatcher relies on.
//
// The dispatch half is the property the state exists for. Before this landed,
// clearing an estop returned a robot to the scheduler's candidate set in the same
// instant — the S1 behaviour — and an assertion that only checked the state string
// would not have noticed.
func TestEnvtest_EstopResume_ClearedRobotIsWithheldUntilConfirmed(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	frames := resumeWitness()
	dispatcher := safety.New(envK8s, nil)
	dispatcher.Telemetry = frames
	dispatcher.DeriveCapabilities = DeriveCapabilities
	actions := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}
	estops := &RobotEstopReconciler{Client: envK8s, Scheme: envScheme, Resumer: dispatcher}

	// A fleet that would otherwise dispatch: healthy adapter, Idle robot, Active
	// capabilities, a Pending action it matches.
	if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	gateEnvSeedAdapterStatus(t, ns, "acme-adapter", contract.Version)

	robot := envtestValidRobot(ns, "amr-1", "z-a")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}

	stoppedAt := time.Now()
	// The projection standing when the hazard is raised. It reads perfectly healthy —
	// which is exactly why the window has to measure freshness rather than trust it.
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, stoppedAt)

	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns},
		Spec: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeNavigate, Zone: "z-a", Priority: fleetv1.ActionPriorityNormal,
		},
	}
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	// ── under the confirmed stop: withheld by candidate filter 10 ────────────
	reconcileResumeAction(t, actions, ns)
	assertAssigned(t, ns, false, "a robot under a confirmed stop was dispatched to")

	// ── the operator clear: Resuming, persisted by a real API server ─────────
	state, err := dispatcher.ClearEstop(ctx, ns, "amr-1", "alice", safety.Ends{})
	if err != nil {
		t.Fatalf("ClearEstop: %v", err)
	}
	if state != fleetv1.RobotEstopResuming {
		t.Fatalf("ClearEstop returned %s, want Resuming", state)
	}
	if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("persisted estopState = %s, want Resuming", got)
	}

	// ── the property the state exists for ────────────────────────────────────
	reconcileResumeAction(t, actions, ns)
	assertAssigned(t, ns, false,
		"a robot became dispatchable the instant an operator cleared its estop, with no window for capabilities to restore")

	// ── healthy telemetry after the clear releases nothing by itself ─────────
	key := types.NamespacedName{Namespace: ns, Name: "amr-1"}
	sendResumeFrame(t, frames, ns, "amr-1")
	out, err := estops.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile estop: %v", err)
	}
	if out.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0: nothing but an operator's request releases the robot", out.RequeueAfter)
	}
	if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopResuming {
		t.Fatalf("estopState = %s, want Resuming: release is operator-confirmed only", got)
	}
	reconcileResumeAction(t, actions, ns)
	assertAssigned(t, ns, false, "a Resuming robot was dispatched to before any operator released it")

	// ── the operator releases it ─────────────────────────────────────────────
	writeResumeRequest(t, key, "inspected")
	if _, err := estops.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile estop: %v", err)
	}
	if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopNormal {
		t.Fatalf("estopState = %s, want Normal after the operator's release", got)
	}

	// ── and only now is it a candidate ───────────────────────────────────────
	reconcileResumeAction(t, actions, ns)
	assertAssigned(t, ns, true, "a confirmed-resumed robot must be dispatchable again")
}

// seedResumeStatus writes the whole projection the resume window reads — the estop
// state and an Idle robot with Active capabilities — with an adapter presence stamp
// at seenAt. The stamp is not the resume witness; the robot's own frame is.
func seedResumeStatus(t *testing.T, ns, name string, state fleetv1.RobotEstopState, seenAt time.Time) {
	t.Helper()
	ctx := context.Background()
	var robot fleetv1.Robot
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &robot); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	stamp := metav1.NewTime(seenAt)
	robot.Status.EstopState = state
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	// Candidate filter 2 matches status.currentZone, not spec.zone: a robot whose
	// position has never resolved is withheld from a zoned action by design, so a
	// fixture that is meant to be dispatchable has to have been located.
	robot.Status.CurrentZone = robot.Spec.Zone
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &stamp}
	robot.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "navigation", Status: fleetv1.CapabilityStatusActive},
	}
	if err := envK8s.Status().Update(ctx, &robot); err != nil {
		t.Fatalf("seed robot status (%s): %v", state, err)
	}
}

func readEstopState(t *testing.T, ns, name string) fleetv1.RobotEstopState {
	t.Helper()
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &robot); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return robot.Status.EstopState
}

func reconcileResumeAction(t *testing.T, r *FleetActionReconciler, ns string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: "task-1"}}); err != nil {
		t.Fatalf("reconcile action: %v", err)
	}
}

func assertAssigned(t *testing.T, ns string, want bool, why string) {
	t.Helper()
	var got fleetv1.FleetAction
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "task-1"}, &got); err != nil {
		t.Fatalf("get action: %v", err)
	}
	if assigned := got.Status.AssignedRobot != ""; assigned != want {
		t.Fatalf("assignedRobot = %q (assigned=%v), want assigned=%v: %s",
			got.Status.AssignedRobot, assigned, want, why)
	}
}
