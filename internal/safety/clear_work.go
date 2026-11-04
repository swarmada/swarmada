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

package safety

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ConditionRenewalHeld is the FleetAction condition that holds the work a clear reached (RFC-0001
// #safety-action-table, The lease): its lease is not renewed on the wire and leaseExpiresAt is not
// advanced until the robot's own report or an operator's decision settles the work. Its reasons:
// ReasonStopClearedBeforePause on Assigned, InProgress or Revoking work (row A1 never paused it), and
// ReasonStopClearedWhilePaused on Paused work. The FleetAction controller reads it and removes it at
// the next assignment.
const (
	ConditionRenewalHeld         = "RenewalHeld"
	ReasonStopClearedBeforePause = "StopClearedBeforePause"
	ReasonStopClearedWhilePaused = "StopClearedWhilePaused"
)

// holdReason is the RenewalHeld reason for work in phase, or "" for a phase whose lease a clear does
// not hold: Pending and terminal work hold no lease, and a Preempted action's lease is frozen already.
func holdReason(phase fleetv1.ActionPhase) string {
	switch phase {
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress, fleetv1.ActionPhaseRevoking:
		return ReasonStopClearedBeforePause
	case fleetv1.ActionPhasePaused:
		return ReasonStopClearedWhilePaused
	}
	return ""
}

// holdClearedWork records RenewalHeld on every FleetAction bound to robot whose lease a clear holds
// (holdReason), with one Warning Event each, so that nothing about that work resumes after the clear
// whether or not a FleetAction reconcile ever reads the robot Resuming. Work bound to another robot
// is not touched. An action that carries the condition already is left as it is.
//
// The clear (S5) calls it once its write has landed. The release (S6) calls it before it writes
// Normal, for work the clear's process did not reach (a change of leader between the two), and does
// not release the robot while it fails. It reads past the cache, runs outside the robot's estop write
// lock, and writes each action with a resourceVersion precondition, reading it again on a conflict.
func (d *Dispatcher) holdClearedWork(ctx context.Context, robot *fleetv1.Robot) error {
	var reader client.Reader = d.Client
	if d.APIReader != nil {
		reader = d.APIReader
	}
	var actions fleetv1.FleetActionList
	if err := reader.List(ctx, &actions, client.InNamespace(robot.Namespace)); err != nil {
		return fmt.Errorf("listing the work robot %s records: %w", robot.Name, err)
	}
	var errs []error
	for i := range actions.Items {
		a := &actions.Items[i]
		if a.Status.AssignedRobot != robot.Name || holdReason(a.Status.Phase) == "" {
			continue
		}
		if err := d.holdOne(ctx, reader, robot, client.ObjectKeyFromObject(a)); err != nil {
			errs = append(errs, fmt.Errorf("holding %s: %w", a.Name, err))
		}
	}
	return errors.Join(errs...)
}

// holdOne records RenewalHeld on one action, reading it again and checking it again on a conflict.
func (d *Dispatcher) holdOne(ctx context.Context, reader client.Reader, robot *fleetv1.Robot, key client.ObjectKey) error {
	for attempt := 0; ; attempt++ {
		a := &fleetv1.FleetAction{}
		if err := reader.Get(ctx, key, a); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		reason := holdReason(a.Status.Phase)
		if a.Status.AssignedRobot != robot.Name || reason == "" ||
			apimeta.FindStatusCondition(a.Status.Conditions, ConditionRenewalHeld) != nil {
			return nil
		}
		base := a.DeepCopy()
		msg := fmt.Sprintf("the stop on robot %s was cleared; this action's lease is not renewed or advanced, "+
			"and the robot stops on its own lease, until the robot reports the work ended or an operator cancels, "+
			"pauses or requeues it", robot.Name)
		apimeta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{
			Type: ConditionRenewalHeld, Status: metav1.ConditionTrue, Reason: reason, Message: msg,
		})
		err := d.Client.Status().Patch(ctx, a, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			log.FromContext(ctx).Info("work held after the clear", "action", a.Name, "robot", robot.Name, "reason", reason)
			if d.Recorder != nil {
				d.Recorder.Event(a, corev1.EventTypeWarning, ConditionRenewalHeld, msg)
			}
			return nil
		}
		if !apierrors.IsConflict(err) || attempt == 4 {
			return err
		}
	}
}
