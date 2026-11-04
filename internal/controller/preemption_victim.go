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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// mayDisplace is row A11's condition on a preemption victim (RFC-0001 #safety-action-table): a
// pending Critical or High action displaces an action only from Assigned or InProgress, only a
// Normal or Low one, and only on a robot under no stop. A Paused action is operator-gated and is
// never displaced (MaySupersede; RFC-0001 #safety-estop-is-not-a-preemption), whatever its band
// and whether or not a stop still stands. Both preemption paths take a victim only through this
// check: the robot path (findPreemptionCandidate) and the zone-capacity path (the TDE's eviction,
// through ReservationRequest.MayEvict, and again when the victim is marked).
//
// The robot is read from the API server, with the stops that cover it, so a stop issued a moment
// ago counts; a robot under a stop, Resuming included (the stop is cleared and the operator has
// not yet resumed it), is no victim's robot. A read that fails, or a victim with no robot, admits
// nothing: the preemptor waits, which is the safe direction.
func (r *FleetActionReconciler) mayDisplace(ctx context.Context, victim *fleetv1.FleetAction) bool {
	if !MaySupersede(victim) || !isPreemptible(victim.Spec.Priority) || victim.Status.AssignedRobot == "" {
		return false
	}
	robot := &fleetv1.Robot{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: victim.Namespace, Name: victim.Status.AssignedRobot}, robot); err != nil {
		log.FromContext(ctx).Info("preemption victim not taken: its robot could not be read",
			"victim", victim.Name, "robot", victim.Status.AssignedRobot, "err", err.Error())
		return false
	}
	if estopHoldsWork(robot) {
		return false
	}
	stops, err := stopNames(ctx, r.liveReader(), robot)
	if err != nil {
		log.FromContext(ctx).Info("preemption victim not taken: the stops over its robot could not be read",
			"victim", victim.Name, "robot", robot.Name, "err", err.Error())
		return false
	}
	return len(stops) == 0
}

// mayEvict is the TDE's victim condition for a zone-capacity preemption in namespace: the
// reservation's action, read from the API server, passes mayDisplace. An action that cannot be
// read is not evicted.
func (r *FleetActionReconciler) mayEvict(ctx context.Context, namespace string) func(actionID string) bool {
	return func(actionID string) bool {
		victim := &fleetv1.FleetAction{}
		if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: actionID}, victim); err != nil {
			return false
		}
		return r.mayDisplace(ctx, victim)
	}
}
