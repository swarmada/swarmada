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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

func requeueReqAction(name, robot string, phase fleetv1.ActionPhase, gen int64, lease *metav1.Time) *fleetv1.FleetAction {
	ft := assignedAction(name, robot, phase, gen, lease)
	ft.Annotations = map[string]string{annRequeueRequested: "zone maintenance"}
	return ft
}

// Adapter acks the stop → the action returns to Pending (re-schedulable), the robot
// is freed to Idle, the generation is preserved, and the annotation is cleared.
func TestRequeue_AdapterAckReturnsToPending(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		requeueReqAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, live),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = &fakeCommander{cancelAck: true}

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending (requeued)", ft.Status.Phase)
	}
	if ft.Status.AssignedRobot != "" || ft.Status.LeaseExpiresAt != nil {
		t.Fatalf("binding/lease not cleared: robot=%q lease=%v", ft.Status.AssignedRobot, ft.Status.LeaseExpiresAt)
	}
	if ft.Status.AssignmentGeneration != 4 {
		t.Fatalf("generation = %d, want 4 preserved (next assign issues strictly-greater)", ft.Status.AssignmentGeneration)
	}
	if ft.Status.CompletionTime != nil {
		t.Error("a requeued task must not be terminalized (no CompletionTime)")
	}
	if _, ok := ft.Annotations[annRequeueRequested]; ok {
		t.Error("requeue annotation not cleared — task would be requeued again after re-scheduling")
	}
	rob := getRobot(t, c, "r1", actionNS)
	if rob.Status.AssignedAction != "" || rob.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot not freed: assignedAction=%q phase=%s", rob.Status.AssignedAction, rob.Status.Phase)
	}
}

// capLossReqAction is a action already marked for capability-loss requeue (as step 1's
// beginCapabilityLossReassignment leaves it), for exercising the disposition branch.
func capLossReqAction(name, robot string, gen int64, lease *metav1.Time) *fleetv1.FleetAction {
	ft := assignedAction(name, robot, fleetv1.ActionPhaseInProgress, gen, lease)
	ft.Annotations = map[string]string{annRequeueRequested: reasonCapabilityLost}
	ft.Status.ScheduledAt = &metav1.Time{Time: time.Now()} // attempted (onFailure retry semantics)
	return ft
}

// Disposition STOPPED_SAFELY: the adapter safe-stopped → requeue to Pending, exactly
// like the ZoneMaintenance path.
func TestCapabilityLoss_StoppedSafely_Requeues(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		capLossReqAction("t1", "r1", 4, live),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("safe-stop should requeue to Pending, got %s", ft.Status.Phase)
	}
	if _, ok := ft.Annotations[annRequeueRequested]; ok {
		t.Error("requeue annotation not cleared")
	}
}

// Disposition RECOVERED: the adapter recovered a mid-commitment robot → the action
// Fails with CapabilityLostDuringExecution (onFailure then governs retry).
func TestCapabilityLoss_Recovered_Fails(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		capLossReqAction("t1", "r1", 4, live),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = &fakeCommander{cancelAck: true, cancelDisp: command.CancelRecovered}

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatalf("recovery should fail the task, got %s", ft.Status.Phase)
	}
	if ft.Status.FailureReason != reasonCapabilityLostDuringExecution {
		t.Fatalf("failureReason = %q, want %q", ft.Status.FailureReason, reasonCapabilityLostDuringExecution)
	}
	if _, ok := ft.Annotations[annRequeueRequested]; ok {
		t.Error("requeue annotation not cleared on recovery")
	}
}

// Disposition COMPLETED: the robot finished the action before the cancel landed → the
// cancel is moot, the annotation is dropped, and the action is neither requeued nor
// failed (normal completion settles it).
func TestCapabilityLoss_Completed_NoReassign(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		capLossReqAction("t1", "r1", 4, live),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = &fakeCommander{cancelAck: true, cancelDisp: command.CancelCompleted}

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if _, ok := ft.Annotations[annRequeueRequested]; ok {
		t.Error("completed cancel should clear the requeue annotation")
	}
	if ft.Status.Phase == fleetv1.ActionPhasePending || ft.Status.Phase == fleetv1.ActionPhaseFailed {
		t.Fatalf("a completed task must not be requeued or failed; phase=%s", ft.Status.Phase)
	}
}

// Unreachable adapter with a LIVE lease → HOLD: the robot may still be executing,
// so the action is not requeued and the robot stays bound (single-executor safety).
func TestRequeue_UnreachableLiveLeaseHolds(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		requeueReqAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, live),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	r.Commander = &fakeCommander{cancelErr: command.ErrUnreachable}

	res := reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase == fleetv1.ActionPhasePending {
		t.Fatal("requeued while the robot may still be executing (lease alive) — double-execution hazard")
	}
	if ft.Status.AssignedRobot != "r1" {
		t.Fatalf("robot freed before provable stop: %q", ft.Status.AssignedRobot)
	}
	if ft.Status.Message != requeuingMessage {
		t.Errorf("message = %q, want requeuing", ft.Status.Message)
	}
	if res.RequeueAfter <= 0 {
		t.Error("a held requeue should requeue")
	}
	if _, ok := ft.Annotations[annRequeueRequested]; !ok {
		t.Error("annotation should persist while held (still trying)")
	}
}

// Unreachable adapter but the lease is PROVABLY DEAD → the robot self-stopped, so
// the action is requeued to Pending.
func TestRequeue_DeadLeaseReturnsToPending(t *testing.T) {
	dead := &metav1.Time{Time: time.Now().Add(-time.Minute)}
	r, c := newActionReconciler(t,
		requeueReqAction("t1", "r1", fleetv1.ActionPhaseRevoking, 4, dead),
		robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
	) // no Commander

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhasePending {
		t.Fatal("requeue not finalized after provable lease death")
	}
}
