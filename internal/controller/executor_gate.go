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
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/executorguard"
)

// The executor guard's controller half (internal/executorguard holds the decision). An action
// is given to a robot, and a robot's record of an action (Robot.status.assignedAction) is
// released, only when every other robot that records the action is provably off it: its
// adapter acknowledged a cancel for it without CANCEL_DISPOSITION_STOP_UNCONFIRMED, it reported
// the action ended, or the lease horizon plus the clock-skew margin has passed. A horizon the
// control plane does not know is bounded by the longest lease the configuration admits
// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee).

// executorGuardReason is the cancel reason the guard carries on the wire.
const executorGuardReason = "a previous executor may still run this action"

// executorGateMessage is the status message of an action held behind a previous executor.
func executorGateMessage(robot, action string, until time.Time, why string) string {
	return fmt.Sprintf("robot %s still records this action (%s) and may still be running it: no robot is given it "+
		"until the adapter acknowledges the cancel or the lease is provably dead (%s); %s", robot, action,
		until.UTC().Format(time.RFC3339), why)
}

// executorStopMessage is the status message of an action held because the robot that ran it
// stands under a stop: only a holder of estop-clear on the stop decides where it goes.
func executorStopMessage(action *fleetv1.FleetAction, robot, stop string) string {
	return awaitingDecision(action, []string{stop}, fmt.Sprintf("robot %s records this action (%s) and is under the "+
		"emergency stop of %s; no other robot is given it while that stop stands", robot, action.Name, stop))
}

// executorGate runs the executor guard for action over robots: it asks each robot that still
// records the action to cancel it, releases the record of every robot that is provably off it,
// and returns the decision: how long to wait on the first that is not, with its name, the
// reason, and the stop that keeps it when a covering stop is what blocks. A robot a stop covers
// keeps its record and is not asked to cancel unless moveAuthorized: the action is being moved by
// a requeue the estop-clear check admitted.
func (r *FleetActionReconciler) executorGate(ctx context.Context, action *fleetv1.FleetAction, robots []fleetv1.Robot,
	now time.Time, skew time.Duration, moveAuthorized bool) (executorguard.Decision, error) {
	var recorders []executorguard.Recorder
	for i := range robots {
		if robots[i].Status.AssignedAction != action.Name || robots[i].Name == action.Status.AssignedRobot {
			continue
		}
		// The robot as the API server records it: a cached copy can lag the stop that covers it.
		rb := &fleetv1.Robot{}
		if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: action.Namespace, Name: robots[i].Name}, rb); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return executorguard.Decision{}, fmt.Errorf("reading robot %s for the executor guard: %w", robots[i].Name, err)
		}
		if rb.Status.AssignedAction != action.Name {
			continue
		}
		covered, err := r.stopCovering(ctx, rb)
		if err != nil {
			return executorguard.Decision{}, err
		}
		rec := executorguard.Recorder{Robot: rb.Name, Covered: covered}
		if r.Commander != nil && !executorguard.KeepsRecord(covered, moveAuthorized) {
			ok, disp := r.pushCancel(ctx, action, rb.Name,
				unattributedCancelIntent(executorGuardReason, command.CancelCauseUnspecified))
			rec.CancelConfirmed = ok && disp != command.CancelStopUnconfirmed
		}
		recorders = append(recorders, rec)
	}
	if len(recorders) == 0 {
		return executorguard.Decision{}, nil
	}
	d := executorguard.Decide(executorguard.Input{
		Horizon:        leaseTime(action.Status.LeaseExpiresAt),
		Reference:      executorReference(action),
		Recorders:      recorders,
		Now:            now,
		Skew:           skew,
		MoveAuthorized: moveAuthorized,
	})
	for _, name := range d.Release {
		if err := r.releaseRecord(ctx, action, name); err != nil {
			return executorguard.Decision{}, err
		}
		log.FromContext(ctx).Info("a previous executor's record of the action released", "task", action.Name,
			"robot", name, "why", d.Why)
	}
	if d.Stop != "" {
		log.FromContext(ctx).Info("a robot under a stop keeps its record of the action", "task", action.Name,
			"robot", d.Blocked, "stop", d.Stop)
	}
	return d, nil
}

// stopCovering names the stops that cover robot, in force or pending, as the stop guard answers
// it (estopscope.Covering), or the robot's own stop state when it is in one; "" when none does.
// A question the guard cannot answer is an error, which the caller returns: the record stays.
func (r *FleetActionReconciler) stopCovering(ctx context.Context, robot *fleetv1.Robot) (string, error) {
	cov, err := estopscope.Covering(ctx, r.liveReader(), robot)
	if err != nil {
		return "", fmt.Errorf("deciding which stops cover robot %s: %w", robot.Name, err)
	}
	if cov.Covered() {
		return strings.Join(cov.Names(), ", "), nil
	}
	if robotUnderEstop(robot) {
		return "robot/" + robot.Name + " (estopState " + string(robot.Status.EstopState) + ")", nil
	}
	return "", nil
}

// executorReference is the latest moment the action is known to have re-entered Pending, been
// created, or settled: the unknown-horizon bound counts from it.
func executorReference(action *fleetv1.FleetAction) time.Time {
	return executorguard.ReferenceOf(action)
}

// releaseRecord clears robot's record of action and returns it to Idle, if it still records the
// action. The write carries the resourceVersion it read. Called only with the executor guard's
// proof.
func (r *FleetActionReconciler) releaseRecord(ctx context.Context, action *fleetv1.FleetAction, robot string) error {
	rb := &fleetv1.Robot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: action.Namespace, Name: robot}, rb); err != nil {
		return client.IgnoreNotFound(err)
	}
	if rb.Status.AssignedAction != action.Name {
		return nil
	}
	base := rb.DeepCopy()
	rb.Status.AssignedAction = ""
	if rb.Status.Phase == fleetv1.RobotPhaseInProgress || rb.Status.Phase == fleetv1.RobotPhaseAssigned {
		rb.Status.Phase = fleetv1.RobotPhaseIdle
	}
	if err := r.Status().Patch(ctx, rb, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if errors.IsConflict(err) {
			return fmt.Errorf("releasing robot %s's record of %s: the robot changed; retrying", robot, action.Name)
		}
		return fmt.Errorf("releasing robot %s's record of %s: %w", robot, action.Name, err)
	}
	return nil
}

// releaseRecorders runs the executor guard for a settled, Paused or Pending action over every
// robot in the namespace and reports the wait, if a previous executor still blocks the release.
func (r *FleetActionReconciler) releaseRecorders(ctx context.Context, action *fleetv1.FleetAction, now time.Time,
	skew time.Duration) (time.Duration, string, string, error) {
	d, err := r.recordersDecision(ctx, action, now, skew, false)
	return d.Wait, d.Blocked, d.Why, err
}

// recordersDecision is releaseRecorders with the whole decision, and the requeue's authority.
func (r *FleetActionReconciler) recordersDecision(ctx context.Context, action *fleetv1.FleetAction, now time.Time,
	skew time.Duration, moveAuthorized bool) (executorguard.Decision, error) {
	var robots fleetv1.RobotList
	if err := r.liveReader().List(ctx, &robots, client.InNamespace(action.Namespace)); err != nil {
		return executorguard.Decision{}, fmt.Errorf("listing robots for the executor guard: %w", err)
	}
	return r.executorGate(ctx, action, robots.Items, now, skew, moveAuthorized)
}

// orphanClock remembers when this process first saw a robot record an action that no longer
// exists.
type orphanClock struct {
	mu   sync.Mutex
	seen map[types.NamespacedName]time.Time
}

// since returns when name was first seen orphaned, recording now if it was not.
func (c *orphanClock) since(name types.NamespacedName, now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[types.NamespacedName]time.Time{}
	}
	if t, ok := c.seen[name]; ok {
		return t
	}
	c.seen[name] = now
	return now
}

// forget drops name: it exists again, or no robot records it.
func (c *orphanClock) forget(name types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, name)
}

// releaseOrphanedRecords runs the executor guard for an action that no longer exists over every
// robot that still records it. A robot may still run a deleted action until its lease lapses, and
// the action may be created again under its name, so the record is the guard's only key: it is
// released when the robot's adapter acknowledges the cancel it is sent, or once the longest lease
// the configuration admits plus the skew margin has passed since this process first saw the
// record orphaned (the deleted action's own horizon is gone with it). A robot a stop covers keeps
// its record until the stop is cleared, as for an action that exists.
func (r *FleetActionReconciler) releaseOrphanedRecords(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// The cache can lag a create: only an action the API server does not have is gone.
	if err := r.liveReader().Get(ctx, req.NamespacedName, &fleetv1.FleetAction{}); err == nil {
		return ctrl.Result{Requeue: true}, nil
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("reading FleetAction %s for the record of a deleted action: %w", req.Name, err)
	}
	var robots fleetv1.RobotList
	if err := r.liveReader().List(ctx, &robots, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing robots for the record of a deleted action: %w", err)
	}
	recorded := false
	for i := range robots.Items {
		if robots.Items[i].Status.AssignedAction == req.Name {
			recorded = true
			break
		}
	}
	if !recorded {
		r.orphans.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	now := time.Now()
	// The deleted action as the guard sees it: no binding, no horizon, and the moment it was
	// first seen gone as the reference the unknown-horizon bound counts from.
	gone := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: req.Namespace, Name: req.Name,
		CreationTimestamp: metav1.Time{Time: r.orphans.since(req.NamespacedName, now)}}}
	_, _, skew := leaseTimings(ctx, r.Client, req.Namespace)
	d, err := r.executorGate(ctx, gone, robots.Items, now, skew, false)
	if err != nil {
		return ctrl.Result{}, err
	}
	if d.Wait > 0 {
		log.FromContext(ctx).Info("a robot records a deleted action and may still run it; its record is kept",
			"task", req.Name, "robot", d.Blocked, "why", d.Why, "wait", d.Wait)
		return ctrl.Result{RequeueAfter: d.Wait}, nil
	}
	r.orphans.forget(req.NamespacedName)
	return ctrl.Result{}, nil
}
