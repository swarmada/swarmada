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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// A robot that reports Error gets no new work until it reports something else
// (RFC-0001 #safety-scope-and-responsibility-boundary, Layer 3: assignment halts for
// robots in Error). The
// report reaches Robot.status.phase through the real telemetry router and status sink,
// against a real API server, and candidate filter 1 refuses the robot by that phase.
// The robot's next non-Error report returns it to Idle, and it is assignable again.
func TestEnvtest_RobotReportedError_HaltsAssignmentUntilTheRobotReportsOtherwise(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	frames := resumeWitness()
	actions := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	gateEnvSeedAdapterStatus(t, ns, "acme-adapter", contract.Version)
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopNormal, time.Now())
	report := func(phase fleetv1.RobotPhase) {
		t.Helper()
		if err := frames.Ingest(ctx, telemetry.Frame{
			RobotID: "amr-1", Namespace: ns, Timestamp: time.Now(), Phase: phase,
		}); err != nil {
			t.Fatalf("ingest %s report: %v", phase, err)
		}
	}
	report(fleetv1.RobotPhaseIdle)

	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z-a", Priority: fleetv1.ActionPriorityNormal},
	}
	if err := envK8s.Create(ctx, action); err != nil {
		t.Fatalf("create action: %v", err)
	}

	// ── the robot reports Error: the phase reaches status ─────────────────────
	report(fleetv1.RobotPhaseError)
	robot := getEnvRobot(t, ns, "amr-1")
	if robot.Status.Phase != fleetv1.RobotPhaseError {
		t.Fatalf("status.phase = %s after the robot reported Error, want Error", robot.Status.Phase)
	}

	// ── and assignment is refused, naming the phase ───────────────────────────
	excl := scheduler.ExcludedByForAssignment(robot, action, false)
	if excl == nil || excl.Filter != scheduler.FilterPhaseIdle || !strings.Contains(excl.Reason, "Error") {
		t.Fatalf("exclusion = %+v, want candidate filter 1 refusing the robot by its Error phase", excl)
	}
	reconcileResumeAction(t, actions, ns)
	assertAssigned(t, ns, false, "a robot reporting Error was assigned new work")

	// ── the robot's next non-Error report clears it ───────────────────────────
	report(fleetv1.RobotPhaseIdle)
	if got := getEnvRobot(t, ns, "amr-1").Status.Phase; got != fleetv1.RobotPhaseIdle {
		t.Fatalf("status.phase = %s after the robot reported Idle again, want Idle", got)
	}
	reconcileResumeAction(t, actions, ns)
	assertAssigned(t, ns, true, "a robot that stopped reporting Error must be assignable again")
}

// A robot that went Offline while in Error, and comes back still reporting Error, comes
// back in Error. The projector does not report an unchanged phase again, so the Robot
// reconciler's advance off Offline is what reads the report; advancing to Idle would
// make a faulted robot assignable on reconnect.
func TestEnvtest_RobotReportedError_SurvivesAReturnFromOffline(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	frames := resumeWitness()
	robots := &RobotReconciler{Client: envK8s, Scheme: envScheme, Frames: frames}

	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	robot := getEnvRobot(t, ns, "amr-1")
	now := metav1.Now()
	robot.Status.Phase = fleetv1.RobotPhaseOffline
	robot.Status.OfflineSince = &now
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &now}
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed Offline robot: %v", err)
	}
	if err := frames.Ingest(ctx, telemetry.Frame{
		RobotID: "amr-1", Namespace: ns, Timestamp: time.Now(), Phase: fleetv1.RobotPhaseError,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if _, err := robots.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "amr-1"}}); err != nil {
		t.Fatalf("reconcile robot: %v", err)
	}
	if got := getEnvRobot(t, ns, "amr-1").Status.Phase; got != fleetv1.RobotPhaseError {
		t.Fatalf("status.phase = %s after the robot came back live reporting Error, want Error", got)
	}
}

func getEnvRobot(t *testing.T, ns, name string) *fleetv1.Robot {
	t.Helper()
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &robot); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return &robot
}
