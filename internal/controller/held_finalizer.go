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
	"sort"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/executorguard"
)

// The stop finalizer on held work (RFC-0001 #safety-fleetaction-behaviour-during-estop). While a
// robot under a stop records an action, the action holds estopscope.StopFinalizer, and so does the
// robot (robotKeepsStopFinalizer, held by the robot estop controller). Deleted by any route, an
// admitted delete or one admitted while no manager replica served the delete check, the action
// stays Terminating with its record: no action can be created again under its name, and its
// FleetTask cannot create the member again, until the stop no longer holds the robot. The record
// is then released on proof as for any action that no longer exists (releaseOrphanedRecords).

// syncHeldFinalizer holds the stop finalizer on action while a robot that records it is under a
// stop, and releases it otherwise, or when the namespace is being torn down. It returns the robots
// that hold the record.
func (r *FleetActionReconciler) syncHeldFinalizer(ctx context.Context, action *fleetv1.FleetAction) ([]string, error) {
	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(action.Namespace)); err != nil {
		return nil, fmt.Errorf("listing robots for the stop finalizer: %w", err)
	}
	var holders []string
	for i := range robots.Items {
		if robots.Items[i].Status.AssignedAction != action.Name {
			continue
		}
		held, err := heldRecord(ctx, r.liveReader(), &robots.Items[i])
		if err != nil {
			return nil, err
		}
		if held {
			holders = append(holders, robots.Items[i].Name)
		}
	}
	sort.Strings(holders)
	hold := len(holders) > 0
	if hold && action.DeletionTimestamp != nil {
		terminating, err := namespaceBeingDeleted(ctx, r.liveReader(), action.Namespace)
		if err != nil {
			return nil, err
		}
		hold = !terminating
	}
	has := controllerutil.ContainsFinalizer(action, estopscope.StopFinalizer)
	if hold == has {
		return holders, nil
	}
	base := action.DeepCopy()
	if hold {
		controllerutil.AddFinalizer(action, estopscope.StopFinalizer)
	} else {
		controllerutil.RemoveFinalizer(action, estopscope.StopFinalizer)
	}
	if err := r.Patch(ctx, action, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, fmt.Errorf("setting the stop finalizer of %s (hold=%v): %w", action.Name, hold, err)
	}
	if !hold {
		return nil, nil
	}
	return holders, nil
}

// awaitDeletedHeld keeps a deleted action whose record a robot under a stop holds: nothing is
// renewed, pushed or released for it; the message and a Warning Event, once, say who decides.
func (r *FleetActionReconciler) awaitDeletedHeld(ctx context.Context, action *fleetv1.FleetAction, holders []string) (ctrl.Result, error) {
	var stops []string
	for _, name := range holders {
		rb := &fleetv1.Robot{}
		if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: action.Namespace, Name: name}, rb); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		names, err := stopNames(ctx, r.liveReader(), rb)
		if err != nil {
			return ctrl.Result{}, err
		}
		stops = append(stops, names...)
	}
	msg := awaitingDecision(action, stops, fmt.Sprintf("this action was deleted while robot %s, under that stop, "+
		"records it; it is kept until the stop no longer holds the robot, and its record is then released when the "+
		"robot is provably off it", holders[0]))
	if action.Status.Message != msg {
		original := action.DeepCopy()
		action.Status.Message = msg
		if err := r.patchActionStatus(ctx, action, original); err != nil {
			return ctrl.Result{}, fmt.Errorf("holding a deleted action under a stop: %w", err)
		}
		log.FromContext(ctx).Info("deleted action kept: a robot under a stop records it", "task", action.Name, "robots", holders)
		warnActionAndTask(r.Recorder, action, EventActionAwaitingOperatorDecision, msg)
	}
	return ctrl.Result{RequeueAfter: executorguard.CoveredRecheck}, nil
}
