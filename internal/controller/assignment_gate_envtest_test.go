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

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// ADR-0032, end to end against a real API server: an adapter whose conformance result is not bound to
// a supported contract version receives NO work, while remaining fully observable and fully
// stoppable. The gate is on work dispatch and nothing else.
//
// Run against envtest rather than a fake client because the properties under test are about what the
// API server ends up holding — an unassigned action, an intact status subresource — and a fake client
// does not model status subresources, defaulting, or validation the same way.

// gateEnvAdapter builds a FleetAdapter that is Connected and conformance-Passed, qualified against
// the given contract version. Everything except the version is deliberately healthy, so a refusal can
// only come from the version condition.
func gateEnvAdapter(ns, name string) *fleetv1.FleetAdapter {
	return &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetAdapterSpec{HeartbeatIntervalSeconds: 10},
	}
}

// gateEnvSeedAdapterStatus writes the status half (status is a subresource, so it cannot be set on
// create).
func gateEnvSeedAdapterStatus(t *testing.T, ns, name, contractVersion string) {
	t.Helper()
	ctx := context.Background()
	var a fleetv1.FleetAdapter
	if err := envK8s.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &a); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	a.Status.Phase = fleetv1.FleetAdapterPhaseConnected
	a.Status.Conformance = fleetv1.ConformanceStatePassed
	a.Status.ConformanceContractVersion = contractVersion
	if err := envK8s.Status().Update(ctx, &a); err != nil {
		t.Fatalf("seed adapter status: %v", err)
	}
}

// An adapter qualified against an out-of-range contract gets no assignment; the same fleet with an
// in-range qualification does. The second half is the control: without it a passing test could mean
// "the fixture never dispatched anyway".
func TestEnvtest_AssignmentGate_OutOfRangeContractBlocksDispatch(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name            string
		contractVersion string
		wantAssigned    bool
	}{
		{"out of range", "0.9.0", false},
		{"no contract version in the report", "", false},
		{"in range", contract.Version, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := envtestNamespace(t)
			r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

			if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
				t.Fatalf("create adapter: %v", err)
			}
			gateEnvSeedAdapterStatus(t, ns, "acme-adapter", tc.contractVersion)

			robot := envtestValidRobot(ns, "amr-1", "z-a")
			if err := envK8s.Create(ctx, robot); err != nil {
				t.Fatalf("create robot: %v", err)
			}
			robot.Status.Phase = fleetv1.RobotPhaseIdle
			robot.Status.Connectivity = &fleetv1.ConnectivityStatus{
				LastSeenAt: &metav1.Time{Time: time.Now()},
			}
			if err := envK8s.Status().Update(ctx, robot); err != nil {
				t.Fatalf("seed robot status: %v", err)
			}

			action := &fleetv1.FleetAction{
				ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns},
				Spec: fleetv1.FleetActionSpec{
					Type:     fleetv1.ActionTypeNavigate,
					Zone:     "z-a",
					Priority: fleetv1.ActionPriorityNormal,
				},
			}
			if err := envK8s.Create(ctx, action); err != nil {
				t.Fatalf("create action: %v", err)
			}

			if _, err := r.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "task-1", Namespace: ns}}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			var got fleetv1.FleetAction
			if err := envK8s.Get(ctx, types.NamespacedName{Name: "task-1", Namespace: ns}, &got); err != nil {
				t.Fatalf("get action: %v", err)
			}
			assigned := got.Status.AssignedRobot != ""
			if assigned != tc.wantAssigned {
				t.Fatalf("assignedRobot = %q (assigned=%v), want assigned=%v for conformance contract %q",
					got.Status.AssignedRobot, assigned, tc.wantAssigned, tc.contractVersion)
			}
			if !tc.wantAssigned && got.Status.Phase == fleetv1.ActionPhaseAssigned {
				t.Errorf("phase = %s, want the action to stay unassigned (Pending) and requeue", got.Status.Phase)
			}

			// The excluded robot is NOT modified — exclusion withholds work, it does not fail or
			// mutate the robot (an operator must not find their fleet edited by a version bump).
			var afterRobot fleetv1.Robot
			if err := envK8s.Get(ctx, types.NamespacedName{Name: "amr-1", Namespace: ns}, &afterRobot); err != nil {
				t.Fatalf("get robot: %v", err)
			}
			if !tc.wantAssigned {
				if afterRobot.Status.AssignedAction != "" {
					t.Errorf("robot carries assignedAction %q; an excluded robot must be left alone",
						afterRobot.Status.AssignedAction)
				}
				if afterRobot.Status.Phase != fleetv1.RobotPhaseIdle {
					t.Errorf("robot phase = %s, want Idle (untouched)", afterRobot.Status.Phase)
				}
			}
		})
	}
}

// The same version-excluded adapter must remain OBSERVABLE: an adapter-sourced status projection
// still lands. Dispatch exclusion must never become a monitoring blind spot — the moment a mismatched
// adapter goes dark, the operator loses the signal that tells them what to fix.
func TestEnvtest_AssignmentGate_ExcludedAdapterStillObservable(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	gateEnvSeedAdapterStatus(t, ns, "acme-adapter", "0.9.0")

	robot := envtestValidRobot(ns, "amr-1", "z-a")
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}

	// A capabilities snapshot is an adapter-sourced observation, projected onto the adapter's
	// status.supportedActions. It must still land even though this adapter may not be given work.
	ing := &CapabilitiesIngestor{Client: envK8s}
	snap := &fav1.CapabilitiesSnapshot{
		RobotId:          "amr-1",
		SupportedActions: []*fav1.SupportedAction{{ActionType: "Navigate"}},
	}
	if err := ing.IngestCapabilities(ctx, ns, snap); err != nil {
		t.Fatalf("ingesting capabilities from a version-excluded adapter must still work, got: %v", err)
	}

	var got fleetv1.FleetAdapter
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "acme-adapter", Namespace: ns}, &got); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	if len(got.Status.SupportedActions) == 0 {
		t.Error("no capabilities projected; a version-excluded adapter must remain observable")
	}
	// And the exclusion still stands after the observation — observing did not re-qualify it.
	if adapterDispatchReady(&got) {
		t.Error("adapter became dispatch-ready after a capabilities projection")
	}
}

// Estop is version-INVARIANT (ADR-0032): a robot bound to a version-excluded adapter is still
// stoppable. This is the property that makes the whole gate safe — refusing work is only acceptable
// because refusing to STOP never happens.
func TestEnvtest_AssignmentGate_ExcludedAdapterStillStoppable(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	gateEnvSeedAdapterStatus(t, ns, "acme-adapter", "0.9.0")

	robot := envtestValidRobot(ns, "amr-1", "z-a")
	robot.Annotations = map[string]string{annEstopTriggered: "person detected"}
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot: %v", err)
	}

	est := &fakeEstopper{}
	r := &RobotEstopReconciler{Client: envK8s, Scheme: envScheme, Estopper: est}
	if _, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "amr-1", Namespace: ns}}); err != nil {
		t.Fatalf("reconcile robot estop: %v", err)
	}

	// The estop reached the adapter. status.estopState is written by the Estopper (internal/safety),
	// not by this reconciler, so what is asserted here is exactly what this layer owns: the stop was
	// ISSUED against a version-excluded adapter rather than filtered out.
	if len(est.estopped) != 1 || est.estopped[0] != "amr-1" {
		t.Fatalf("estopped = %v, want [amr-1] — estop MUST be honoured against a version-excluded adapter",
			est.estopped)
	}
	var got fleetv1.Robot
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "amr-1", Namespace: ns}, &got); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	if got.Annotations[annEstopProcessed] != "person detected" {
		t.Errorf("estop-processed marker = %q, want the trigger value recorded (the estop completed)",
			got.Annotations[annEstopProcessed])
	}

	// And the adapter is still excluded from work: being stoppable did not make it dispatchable.
	var adapter fleetv1.FleetAdapter
	if err := envK8s.Get(ctx, types.NamespacedName{Name: "acme-adapter", Namespace: ns}, &adapter); err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	if adapterDispatchReady(&adapter) {
		t.Error("a stoppable adapter must not thereby become dispatch-eligible")
	}
}
