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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
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

// A robot that becomes free to take work, or a free robot whose state now admits an action it
// did not, wakes the actions waiting for it.
//
// Without this, an action Pending for want of a robot is reconciled again only by its own
// requeue (pendingRecheckInterval) or by an unrelated event: robotToAction maps a Robot event only
// to the action the robot records. The FleetAdapter heartbeat used to re-enqueue every Pending
// action several times a second, which hid that; it is filtered now (fleetaction_wake_sources.go),
// so each change that can admit a waiting action has its own wake, here and in the wake sources.

// wakeLimit is the most actions one robot enqueues. One robot takes one action; the rest cover
// the actions that fail on what the wake cannot see (the adapter's readiness and its
// validate_action answer, maintenance, the stops, zone capacity, filters 11 and 12). Each costs a
// full dispatch pass on the controller's one worker, and once one has taken the robot the others
// certainly lose, so the limit is small. An action past it is reached by its own recheck.
const wakeLimit = 3

// wakeMemoTTL bounds how long an action woken and not yet reconciled is skipped by later wakes.
// It is forgotten as soon as the action's reconcile starts, which is the real release: every
// action a wake marks is enqueued by that wake, so its reconcile comes. The TTL only covers a
// reconcile that never comes, and is as long as the action's own recheck, since under load an
// action waits in the queue for seconds and a shorter TTL would let later wakes pick it again.
const wakeMemoTTL = pendingRecheckInterval

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

// robotChange is what a Robot update means to the actions waiting for a robot.
type robotChange int

const (
	// robotNoWake: the robot is not free now, or nothing the waiting actions read changed.
	robotNoWake robotChange = iota
	// robotBecameFree: it was not free and is now; it may take any action it can.
	robotBecameFree
	// robotChangedNarrowed: it was free and still is, and something the wake's filter reads
	// changed (its zone, a battery rise, its capabilities or their parameters, its models); it
	// wakes only the actions that change newly admits (mayTakeAction with prev).
	robotChangedNarrowed
	// robotChangedUnnarrowed: it was free and still is, and the rules its adapter honours
	// changed (filter 12), which the wake's filter does not model; it wakes as if it became free.
	robotChangedUnnarrowed
)

// classifyRobotChange says what an update from old to new means to the waiting actions.
func classifyRobotChange(old, new *fleetv1.Robot) robotChange {
	if !robotFreeForWork(new) {
		return robotNoWake
	}
	if !robotFreeForWork(old) {
		return robotBecameFree
	}
	if !equality.Semantic.DeepEqual(old.Status.HonouredOperatingConstraints, new.Status.HonouredOperatingConstraints) {
		return robotChangedUnnarrowed
	}
	if old.Status.CurrentZone != new.Status.CurrentZone || batteryRose(old, new) ||
		!equality.Semantic.DeepEqual(old.Status.Capabilities, new.Status.Capabilities) ||
		!equality.Semantic.DeepEqual(old.Status.InstalledModels, new.Status.InstalledModels) ||
		!equality.Semantic.DeepEqual(old.Spec.InstalledModels, new.Spec.InstalledModels) ||
		!equality.Semantic.DeepEqual(old.Status.ModelGrantedCapabilities, new.Status.ModelGrantedCapabilities) {
		return robotChangedNarrowed
	}
	return robotNoWake
}

// batteryRose reports a rise of the robot's reported battery, or its first report. Only a rise can
// lift a battery floor (filter 4).
func batteryRose(old, new *fleetv1.Robot) bool {
	if new.Status.BatteryPercent == nil {
		return false
	}
	return old.Status.BatteryPercent == nil || *new.Status.BatteryPercent > *old.Status.BatteryPercent
}

// robotWakeEvent admits the Robot events after which a waiting action may find this robot: a robot
// created free, and every update classifyRobotChange gives a wake. Every other Robot write
// (telemetry projections, a battery fall, conditions) enqueues nothing here.
var robotWakeEvent = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool {
		robot, ok := e.Object.(*fleetv1.Robot)
		return ok && robotFreeForWork(robot)
	},
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldR, ok1 := e.ObjectOld.(*fleetv1.Robot)
		newR, ok2 := e.ObjectNew.(*fleetv1.Robot)
		return ok1 && ok2 && classifyRobotChange(oldR, newR) != robotNoWake
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// wakeHandler enqueues the actions a robot event wakes, from the robot as it is now: a created
// robot, or an updated robot's new object (with its old one when the change is narrowed).
// handler.EnqueueRequestsFromMapFunc would map an update's old object on its own as well, and the
// old copy of a free robot that moved is free too.
func (r *FleetActionReconciler) wakeHandler() handler.EventHandler {
	add := func(q workqueue.RateLimitingInterface, reqs []reconcile.Request) {
		for _, req := range reqs {
			q.Add(req)
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.RateLimitingInterface) {
			if robot, ok := e.Object.(*fleetv1.Robot); ok {
				add(q, r.pendingActionsFor(ctx, robot, nil))
			}
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			oldR, ok1 := e.ObjectOld.(*fleetv1.Robot)
			newR, ok2 := e.ObjectNew.(*fleetv1.Robot)
			if !ok1 || !ok2 {
				return
			}
			switch classifyRobotChange(oldR, newR) {
			case robotBecameFree:
				add(q, r.pendingActionsFor(ctx, newR, nil))
			case robotChangedUnnarrowed:
				// The rules its adapter honours bear on zoned actions only (filter 12).
				add(q, r.pendingActionsForWhere(ctx, newR, func(a *fleetv1.FleetAction) bool { return a.Spec.Zone != "" }))
			case robotChangedNarrowed:
				add(q, r.pendingActionsFor(ctx, newR, oldR))
			}
		},
	}
}

// pendingActionsFor returns the Pending actions of the robot's namespace that it could take, most
// urgent first, at most wakeLimit of them, skipping actions already woken and not yet reconciled.
//
// "Could take" drops only an action that dispatch would certainly refuse this robot, judged from
// the cache as dispatch judges it (takeable). With prev, the robot was already free, and only the
// actions its change newly admits are woken (mayTakeAction).
//
// Most urgent first: by priority band (Critical, High, Normal, Low; unset is Normal), then oldest
// first, then by name. The order is weaker than it reads. An action the workqueue already holds
// keeps its place (an Add of a queued item is dropped), so the order decides only among the
// actions this wake adds; and an action being reconciled as the robot frees read the robot
// before, so the wake's Add only marks it to run again, behind the actions the wake queued.
func (r *FleetActionReconciler) pendingActionsFor(ctx context.Context, robot, prev *fleetv1.Robot) []reconcile.Request {
	if !robotFreeForWork(robot) {
		return nil
	}
	pending, zones, ok := r.pendingInNamespace(ctx, robot.Namespace)
	if !ok {
		return nil
	}
	return r.pick(pending, wakeLimit, 0, func(a *fleetv1.FleetAction) bool { return mayTakeAction(robot, a, zones, prev) })
}

// pendingActionsForWhere is pendingActionsFor for a robot that may take any action it can, among
// the actions also admits.
func (r *FleetActionReconciler) pendingActionsForWhere(ctx context.Context, robot *fleetv1.Robot,
	also func(*fleetv1.FleetAction) bool) []reconcile.Request {
	if !robotFreeForWork(robot) {
		return nil
	}
	pending, zones, ok := r.pendingInNamespace(ctx, robot.Namespace)
	if !ok {
		return nil
	}
	return r.pick(pending, wakeLimit, 0, func(a *fleetv1.FleetAction) bool {
		return also(a) && takeable(robot, a, zones, true)
	})
}

// wakeForRobots is the wake for several robots released at once (a stop's clear, a maintenance
// window's end): one list of the namespace's Pending actions, and up to wakeLimit distinct actions
// per robot that any of them could take. Waking each robot on its own would give every one the
// same most urgent actions, from a cache that has seen none of their assignments yet.
func (r *FleetActionReconciler) wakeForRobots(ctx context.Context, namespace string, robots []*fleetv1.Robot) []reconcile.Request {
	if len(freeOf(robots)) == 0 {
		return nil
	}
	pending, zones, ok := r.pendingInNamespace(ctx, namespace)
	if !ok {
		return nil
	}
	return r.wakeForRobotsIn(pending, zones, robots, nil)
}

// wakeForRobotsIn is wakeForRobots over a list already made, among the actions also admits (nil:
// every action).
func (r *FleetActionReconciler) wakeForRobotsIn(pending []*fleetv1.FleetAction, zones *wakeZoneLeaves,
	robots []*fleetv1.Robot, also func(*fleetv1.FleetAction) bool) []reconcile.Request {
	free := freeOf(robots)
	if len(free) == 0 {
		return nil
	}
	return r.pick(pending, wakeLimit*len(free), 0, func(a *fleetv1.FleetAction) bool {
		if also != nil && !also(a) {
			return false
		}
		for _, rb := range free {
			if takeable(rb, a, zones, true) {
				return true
			}
		}
		return false
	})
}

func freeOf(robots []*fleetv1.Robot) []*fleetv1.Robot {
	free := make([]*fleetv1.Robot, 0, len(robots))
	for _, rb := range robots {
		if robotFreeForWork(rb) {
			free = append(free, rb)
		}
	}
	return free
}

// pendingInNamespace lists the namespace's Pending actions (an unset phase is Pending; actions
// being deleted are left out) most urgent first, with the namespace's zone leaves.
func (r *FleetActionReconciler) pendingInNamespace(ctx context.Context, namespace string) ([]*fleetv1.FleetAction, *wakeZoneLeaves, bool) {
	var actions fleetv1.FleetActionList
	if err := r.List(ctx, &actions, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).Error(err, "listing the actions a wake may enqueue; they wait for their own recheck",
			"namespace", namespace)
		return nil, nil, false
	}
	pending := make([]*fleetv1.FleetAction, 0, len(actions.Items))
	for i := range actions.Items {
		a := &actions.Items[i]
		if a.DeletionTimestamp != nil {
			continue
		}
		if a.Status.Phase != "" && a.Status.Phase != fleetv1.ActionPhasePending {
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
	return pending, r.zoneLeavesFor(ctx, namespace), true
}

// pick takes, in order, the actions take admits that no earlier wake has enqueued and their
// reconcile not yet started, up to limit of them (0: no limit), and records them as woken. Up to
// countQueued admitted actions that an earlier wake already enqueued count toward limit, though
// they are not enqueued again: the capacity wake uses it, so that the actions a robot wake already
// took for the same release are not doubled, while fresh ones are still woken in case those fail
// on a gate before the TDE.
func (r *FleetActionReconciler) pick(pending []*fleetv1.FleetAction, limit, countQueued int,
	take func(*fleetv1.FleetAction) bool) []reconcile.Request {
	now := time.Now()
	var reqs []reconcile.Request
	counted, queued := 0, 0
	for _, a := range pending {
		if limit > 0 && counted >= limit {
			break
		}
		if !take(a) {
			continue
		}
		key := types.NamespacedName{Namespace: a.Namespace, Name: a.Name}
		if !r.woken.tryMark(key, now) {
			if queued < countQueued {
				queued++
				counted++
			}
			continue
		}
		counted++
		reqs = append(reqs, reconcile.Request{NamespacedName: key})
	}
	return reqs
}

// wakeMemo holds the actions a wake has enqueued whose reconcile has not started. A later wake
// skips them, so that robots released one after another, faster than the queue drains, do not
// all pick the same actions. It is a wake's dedup and nothing else: dispatch never reads it.
type wakeMemo struct {
	mu        sync.Mutex
	m         map[types.NamespacedName]time.Time
	lastPrune time.Time
}

// queued reports whether key was woken within wakeMemoTTL and its reconcile has not started.
func (w *wakeMemo) queued(key types.NamespacedName, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.m[key]
	return ok && now.Sub(t) < wakeMemoTTL
}

// tryMark records key as woken unless it already is, in one step, so that two wakes running at
// once never both take it. It reports whether it marked.
func (w *wakeMemo) tryMark(key types.NamespacedName, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.m[key]; ok && now.Sub(t) < wakeMemoTTL {
		return false
	}
	if w.m == nil {
		w.m = map[types.NamespacedName]time.Time{}
	}
	w.m[key] = now
	if now.Sub(w.lastPrune) >= wakeMemoTTL/3 { // keep it small, without a scan on every mark
		for k, t := range w.m {
			if now.Sub(t) >= wakeMemoTTL {
				delete(w.m, k)
			}
		}
		w.lastPrune = now
	}
	return true
}

// forget is called as an action's reconcile starts: from then on, a wake may enqueue it again.
func (w *wakeMemo) forget(key types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.m, key)
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
		log.FromContext(ctx).Error(err, "listing FleetZones for a wake; no waiting action is dropped on zone")
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

// mayTakeAction reports whether a wake for robot should enqueue action. Without prev (the robot
// has just become free), whenever robot could take it (takeable, with Degraded capabilities
// accepted). With prev (the robot was already free), only when its change newly admits the
// action, under either reading of acceptDegraded: with Degraded accepted, and with it refused, as
// an action that does not accept Degraded reads it. Under one reading only, a capability going
// Degraded → Active would look like no change to the lenient one.
func mayTakeAction(robot *fleetv1.Robot, action *fleetv1.FleetAction, zones *wakeZoneLeaves, prev *fleetv1.Robot) bool {
	if prev == nil {
		return takeable(robot, action, zones, true)
	}
	for _, acceptDegraded := range []bool{false, true} {
		if takeable(robot, action, zones, acceptDegraded) && !takeable(prev, action, zones, acceptDegraded) {
			return true
		}
	}
	return false
}

// takeable reports whether dispatch could give action to robot as far as the wake can tell from
// the cache. It is false only where dispatch would certainly refuse, under the given reading of
// acceptDegraded:
//   - the pair-wise filters (scheduler.ExcludedBy: an action for a ComputeEndpoint, an action
//     pinned to another robot, a required capability or parametric constraint the robot cannot
//     meet);
//   - candidate filters 4 and 7, which read only the robot and the action and hold for a pinned
//     robot too: the battery floor, and a model granting a required capability that is updating;
//   - candidate filter 2 for a zone-scoped action: the robot's currentZone must be among the leaf
//     zones at or below the action's zone (unset is outside every zone). An action pinned to this
//     robot skips it, as in dispatch. If the FleetZone list cannot be read, no action is dropped on
//     zone.
func takeable(robot *fleetv1.Robot, action *fleetv1.FleetAction, zones *wakeZoneLeaves, acceptDegraded bool) bool {
	if scheduler.ExcludedBy(robot, action, acceptDegraded) != nil {
		return false
	}
	if excludedByBatteryFloor(robot, action) != nil || excludedByModelUpdate(robot, action) != nil {
		return false
	}
	pinnedHere := action.Spec.RobotSelector != "" && action.Spec.RobotSelector == robot.Name
	if action.Spec.Zone == "" || pinnedHere || zones == nil {
		return true
	}
	return robot.Status.CurrentZone != "" && zones.of(action.Spec.Zone)[robot.Status.CurrentZone]
}
