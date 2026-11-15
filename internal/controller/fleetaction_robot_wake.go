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
	"sort"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// A robot that becomes free to take work wakes the actions waiting for one.
//
// Without this, an action Pending for want of a robot is reconciled again only by its own
// requeue (pendingRecheckInterval) or by an unrelated event, and the robot's going Idle enqueues
// nothing: robotToAction maps a Robot event only to the action the robot records. Here the
// moment a robot becomes free, the Pending actions of its namespace that it could take are
// enqueued, most urgent first, at most wakeLimit of them.

// wakeLimit is the most actions one robot becoming free enqueues. One robot takes one action;
// the rest cover the actions that fail on what the wake cannot see (the adapter's readiness and
// its validate_action answer, maintenance, the stops, zone capacity, filters 11 and 12). Each
// costs a full dispatch pass on the controller's one worker, and once one has taken the robot the
// others certainly lose, so the limit is small. An action past it is reached by its own recheck,
// as it was before this wake existed.
const wakeLimit = 3

// robotFreeForWork reports whether robot could be given an action as far as its own state goes:
// not being deleted, Idle, recording no action, and holding no stop's work (estopHoldsWork: an
// estopState of Stopping, Stopped, Failed or Resuming, where the post-clear window still holds
// the robot's work; or a stop the robot records whatever that state reads, its own trigger
// annotation or a zone's or the namespace's stop in status.estopHeldBy). The candidate filters
// decide the rest.
func robotFreeForWork(robot *fleetv1.Robot) bool {
	return robot.DeletionTimestamp == nil &&
		robot.Status.Phase == fleetv1.RobotPhaseIdle && robot.Status.AssignedAction == "" &&
		!estopHoldsWork(robot)
}

// robotBecameFree admits the Robot events after which a waiting action may now find this robot:
// a robot created free, a robot becoming free (it went Idle, its action was released, its stop
// was cleared through to Normal or its hold record removed, it came back from Offline), and a
// free robot moving zone (a zone-scoped action may now reach it). Every other Robot write
// (telemetry projections, battery, conditions) enqueues nothing here.
var robotBecameFree = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		robot, ok := e.Object.(*fleetv1.Robot)
		return ok && robotFreeForWork(robot)
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldR, ok1 := e.ObjectOld.(*fleetv1.Robot)
		newR, ok2 := e.ObjectNew.(*fleetv1.Robot)
		if !ok1 || !ok2 || !robotFreeForWork(newR) {
			return false
		}
		return !robotFreeForWork(oldR) || oldR.Status.CurrentZone != newR.Status.CurrentZone
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// pendingActionsFor maps a robot that robotBecameFree admitted to the Pending actions of its
// namespace that it could take, most urgent first, at most wakeLimit of them.
//
// "Could take" drops only an action that dispatch would certainly refuse this robot, judged from
// the cache as dispatch judges it:
//   - the pair-wise filters (scheduler.ExcludedBy: an action for a ComputeEndpoint, an action
//     pinned to another robot, a required capability or parametric constraint the robot cannot
//     meet), with Degraded capabilities accepted, so the namespace's default can never make it
//     drop an action dispatch would take;
//   - candidate filters 4 and 7, which read only the robot and the action and hold for a pinned
//     robot too: the battery floor, and a model granting a required capability that is updating;
//   - candidate filter 2 for a zone-scoped action: the robot's currentZone must be among the
//     leaf zones at or below the action's zone (unset is outside every zone). An action pinned to
//     this robot skips it, as in dispatch. If the FleetZone list cannot be read, no action is
//     dropped on zone.
//
// movedFrom is set when the robot was free before this event and only its zone changed: it is
// the zone it left. Such a move changes filter 2 for zone-scoped actions only, so it wakes only
// the actions whose leaf set holds the zone it entered and not the zone it left; unzoned and
// pinned actions, and actions whose leaf set holds both zones, could take the robot before and
// are reached as before. (A move between two leaves of one action's subtree can still change
// filter 12, whose rules are per leaf; that action is reached by its recheck. So is a robot
// leaving a stopped zone in the moment before the stop's hold is recorded on it.)
//
// Then most urgent first: by priority band (Critical, High, Normal, Low; unset is Normal), then
// oldest first, then by name, and the first wakeLimit are enqueued in that order.
//
// The order is weaker than it reads. An action the workqueue already holds keeps its place (an
// Add of a queued item is dropped), so the order decides only among the actions this wake adds,
// and with one worker the most urgent of those is reconciled first. It does not stop an action
// queued earlier (by its own recheck, or by an adapter's write) from reaching the robot first.
// Nor does it hold for an action being reconciled as the robot frees: that reconcile read the
// robot before, and the wake's Add only marks the action to run again once it is done, behind
// the actions the wake queued.
//
// It is reached through wakeHandler, which maps only a Robot event's new object.
func (r *FleetActionReconciler) pendingActionsFor(ctx context.Context, obj client.Object, movedFrom *string) []reconcile.Request {
	robot, ok := obj.(*fleetv1.Robot)
	if !ok || !robotFreeForWork(robot) {
		return nil
	}
	var actions fleetv1.FleetActionList
	if err := r.List(ctx, &actions, client.InNamespace(robot.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "listing the actions a freed robot may take; they wait for their own recheck",
			"robot", robot.Name)
		return nil
	}
	zones := r.zoneLeavesFor(ctx, robot.Namespace)
	pending := make([]*fleetv1.FleetAction, 0, len(actions.Items))
	for i := range actions.Items {
		a := &actions.Items[i]
		if a.DeletionTimestamp != nil {
			continue
		}
		if a.Status.Phase != "" && a.Status.Phase != fleetv1.ActionPhasePending {
			continue
		}
		if !mayTakeAction(robot, a, zones, movedFrom) {
			continue
		}
		pending = append(pending, a)
	}
	sort.SliceStable(pending, func(i, j int) bool {
		a, b := pending[i], pending[j]
		if ra, rb := priorityRank(a.Spec.Priority), priorityRank(b.Spec.Priority); ra != rb {
			return ra < rb
		}
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Name < b.Name
	})
	if len(pending) > wakeLimit {
		pending = pending[:wakeLimit]
	}
	reqs := make([]reconcile.Request, 0, len(pending))
	for _, a := range pending {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
	}
	return reqs
}

// wakeHandler enqueues pendingActionsFor of the robot as it is now: a created robot, or an
// updated robot's new object. handler.EnqueueRequestsFromMapFunc would map an update's old object
// as well, and on a free robot's zone move the old copy is free too: the namespace would be
// listed twice, and actions of the zone the robot just left woken for it.
func (r *FleetActionReconciler) wakeHandler() handler.EventHandler {
	enqueue := func(ctx context.Context, obj client.Object, movedFrom *string, q workqueue.RateLimitingInterface) {
		for _, req := range r.pendingActionsFor(ctx, obj, movedFrom) {
			q.Add(req)
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.RateLimitingInterface) {
			enqueue(ctx, e.Object, nil, q)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			var movedFrom *string
			if old, ok := e.ObjectOld.(*fleetv1.Robot); ok && robotFreeForWork(old) {
				zone := old.Status.CurrentZone
				movedFrom = &zone
			}
			enqueue(ctx, e.ObjectNew, movedFrom, q)
		},
	}
}

// wakeZoneLeaves resolves an action's zone to its leaf set from one cached FleetZone list,
// memoized per zone for one wake. nil means the list could not be read, and then no action is
// dropped on zone.
type wakeZoneLeaves struct {
	zones  []fleetv1.FleetZone
	leaves map[string]map[string]bool
}

func (r *FleetActionReconciler) zoneLeavesFor(ctx context.Context, namespace string) *wakeZoneLeaves {
	var zones fleetv1.FleetZoneList
	if err := r.List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).Error(err, "listing FleetZones for a freed robot's wake; no waiting action is dropped on zone")
		return nil
	}
	return &wakeZoneLeaves{zones: zones.Items, leaves: map[string]map[string]bool{}}
}

func (w *wakeZoneLeaves) of(zone string) map[string]bool {
	if l, ok := w.leaves[zone]; ok {
		return l
	}
	l := resolveZoneLeaves(zone, w.zones)
	w.leaves[zone] = l
	return l
}

// mayTakeAction reports whether dispatch could give action to robot, as far as the wake can
// tell from the cache, and, for a free robot that only moved (movedFrom set), whether the move is
// what made it possible. It returns false only where dispatch would certainly refuse, or where
// nothing the move changed bears on the action; see pendingActionsFor.
func mayTakeAction(robot *fleetv1.Robot, action *fleetv1.FleetAction, zones *wakeZoneLeaves, movedFrom *string) bool {
	if scheduler.ExcludedBy(robot, action, true) != nil {
		return false
	}
	if excludedByBatteryFloor(robot, action) != nil || excludedByModelUpdate(robot, action) != nil {
		return false
	}
	pinnedHere := action.Spec.RobotSelector != "" && action.Spec.RobotSelector == robot.Name
	if movedFrom != nil && (action.Spec.Zone == "" || pinnedHere) {
		return false
	}
	if action.Spec.Zone == "" || pinnedHere || zones == nil {
		return true
	}
	leaves := zones.of(action.Spec.Zone)
	if robot.Status.CurrentZone == "" || !leaves[robot.Status.CurrentZone] {
		return false
	}
	return movedFrom == nil || !leaves[*movedFrom]
}
