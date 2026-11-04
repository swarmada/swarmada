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
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// Every writer of Robot.status.phase writes from the resourceVersion it read. A writer
// whose informer has not yet seen a newer status must not land a phase computed from the
// older one: the robot-reported Error the status sink has just written would be replaced,
// the projector does not report an unchanged Error again, and a robot still reporting
// Error would be assignable (RFC-0001 #safety-scope-and-responsibility-boundary, Layer 3).
//
// Each case below feeds one writer a stale view, as an informer that lags the API server
// does, against a real API server, which is what enforces the resourceVersion precondition.

// staleRobotView answers every Get of one Robot with a fixed, older copy of it, and passes
// everything else, writes included, to the API server.
type staleRobotView struct {
	client.Client
	stale *fleetv1.Robot
}

func (s staleRobotView) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r, ok := obj.(*fleetv1.Robot); ok && key.Namespace == s.stale.Namespace && key.Name == s.stale.Name {
		s.stale.DeepCopyInto(r)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// seedRobotPhase writes phase and assignedAction onto the stored robot and returns the
// stored copy, which is the view a lagging informer keeps.
func seedRobotPhase(t *testing.T, ns, name string, phase fleetv1.RobotPhase, assigned string) *fleetv1.Robot {
	t.Helper()
	robot := getEnvRobot(t, ns, name)
	robot.Status.Phase = phase
	robot.Status.AssignedAction = assigned
	if err := envK8s.Status().Update(context.Background(), robot); err != nil {
		t.Fatalf("seed phase %s: %v", phase, err)
	}
	return getEnvRobot(t, ns, name)
}

func reportPhase(t *testing.T, frames *telemetry.Router, ns string, phase fleetv1.RobotPhase) {
	t.Helper()
	if err := frames.Ingest(context.Background(), telemetry.Frame{
		RobotID: "amr-1", Namespace: ns, Timestamp: time.Now(), Phase: phase,
	}); err != nil {
		t.Fatalf("ingest %s report: %v", phase, err)
	}
}

// The action on a faulted robot is stopped at the moment the robot reports Error, and the
// FleetAction reconciler releases the robot from a view taken before the Error landed.
func TestEnvtest_ReleaseOverAStaleViewKeepsTheReportedError(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	frames := resumeWitness()
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	stale := seedRobotPhase(t, ns, "amr-1", fleetv1.RobotPhaseInProgress, "task-1")
	reportPhase(t, frames, ns, fleetv1.RobotPhaseInProgress)
	reportPhase(t, frames, ns, fleetv1.RobotPhaseError)
	if got := getEnvRobot(t, ns, "amr-1").Status.Phase; got != fleetv1.RobotPhaseError {
		t.Fatalf("status.phase = %s after the robot reported Error, want Error", got)
	}

	action := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns}}
	stalled := &FleetActionReconciler{Client: staleRobotView{Client: envK8s, stale: stale}, Scheme: envScheme}
	_ = stalled.releaseRobotForStop(ctx, action, "amr-1") // a refused stale write is retried by the caller's requeue
	for i := 0; i < 3; i++ {
		reportPhase(t, frames, ns, fleetv1.RobotPhaseError) // the robot keeps reporting Error
	}
	robot := getEnvRobot(t, ns, "amr-1")
	if robot.Status.Phase != fleetv1.RobotPhaseError {
		t.Fatalf("status.phase = %s after a release from a view older than the Error, want Error; "+
			"filter 1 exclusion = %v", robot.Status.Phase, scheduler.ExcludedByForAssignment(robot, &fleetv1.FleetAction{}, false))
	}

	// The retry, from a current view, frees the robot and still leaves the Error alone.
	fresh := &FleetActionReconciler{Client: envK8s, Scheme: envScheme}
	if err := fresh.releaseRobotForStop(ctx, action, "amr-1"); err != nil {
		t.Fatalf("release from a current view: %v", err)
	}
	robot = getEnvRobot(t, ns, "amr-1")
	if robot.Status.AssignedAction != "" || robot.Status.Phase != fleetv1.RobotPhaseError {
		t.Fatalf("after the release from a current view: assignedAction=%q phase=%s, want \"\" and Error",
			robot.Status.AssignedAction, robot.Status.Phase)
	}
}

// A maintenance window closes over a robot that reported Error while it was paused, and
// the ZoneMaintenance reconciler flips it back from a view taken before the Error landed.
func TestEnvtest_MaintenanceCloseOverAStaleViewKeepsTheReportedError(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	frames := resumeWitness()
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	stale := seedRobotPhase(t, ns, "amr-1", fleetv1.RobotPhaseMaintenance, "")
	reportPhase(t, frames, ns, fleetv1.RobotPhaseIdle)
	reportPhase(t, frames, ns, fleetv1.RobotPhaseError)
	if got := getEnvRobot(t, ns, "amr-1").Status.Phase; got != fleetv1.RobotPhaseError {
		t.Fatalf("status.phase = %s after the robot reported Error in Maintenance, want Error", got)
	}

	zm := &ZoneMaintenanceReconciler{Client: envK8s, Scheme: envScheme}
	_ = zm.setRobotPhase(ctx, stale, fleetv1.RobotPhaseIdle) // refused: retried from a current view
	reportPhase(t, frames, ns, fleetv1.RobotPhaseError)
	if got := getEnvRobot(t, ns, "amr-1").Status.Phase; got != fleetv1.RobotPhaseError {
		t.Fatalf("status.phase = %s after the window closed over a view older than the Error, want Error", got)
	}
}

// The Robot reconciler advances a freshly admitted robot off Discovered. From a view taken
// before the scheduler claimed the robot, that advance wrote Idle over InProgress and left
// the claim in place: a robot holding an action, shown Idle.
func TestEnvtest_RobotLiveAdvanceOverAStaleViewKeepsTheSchedulersPhase(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
		t.Fatalf("create robot: %v", err)
	}
	robot := getEnvRobot(t, ns, "amr-1")
	now := metav1.Now()
	robot.Status.Phase = fleetv1.RobotPhaseDiscovered
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &now}
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed Discovered robot: %v", err)
	}
	stale := getEnvRobot(t, ns, "amr-1")
	seedRobotPhase(t, ns, "amr-1", fleetv1.RobotPhaseInProgress, "task-1") // the scheduler's claim

	robots := &RobotReconciler{Client: staleRobotView{Client: envK8s, stale: stale}, Scheme: envScheme}
	_, _ = robots.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "amr-1"}})
	got := getEnvRobot(t, ns, "amr-1")
	if got.Status.Phase != fleetv1.RobotPhaseInProgress || got.Status.AssignedAction != "task-1" {
		t.Fatalf("after a live advance from a view older than the claim: phase=%s assignedAction=%q, "+
			"want InProgress and task-1", got.Status.Phase, got.Status.AssignedAction)
	}
}
