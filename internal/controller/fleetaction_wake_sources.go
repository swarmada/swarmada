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
	"sync"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// The FleetAdapter watch, filtered, and the wakes for what its heartbeats used to cover.
//
// Every adapter heartbeat writes FleetAdapter.status.lastHeartbeat, and adapterToActions maps
// every FleetAdapter event to the namespace's Pending actions and to the actions bound to the
// adapter's robots. At 50 robots that was most of the FleetAction queue's input, and it worked as
// an accidental poll of every Pending action several times a second. The poll is gone
// (adapterChangeMatters), so each change that can admit a waiting action and has no Robot or
// FleetAction event of its own wakes the actions it may admit, here: a zone's or the namespace's
// stop no longer holding, a zone's restrictions changing, a zone's capacity freed, a maintenance
// window releasing robots. A robot's own changes wake through wakeHandler
// (fleetaction_robot_wake.go). What neither covers waits for the action's own recheck.

// awaitingZoneCapacity is the message of an action the TDE denied a slot in its zone.
const awaitingZoneCapacity = "awaiting zone capacity (traffic deconfliction)"

// adapterChangeMatters drops an adapter's liveness heartbeat: an update that changes nothing but
// status.lastHeartbeat (or nothing at all, a resync). Nothing outside the FleetAdapter controller
// reads lastHeartbeat. Everything else passes: Connected, Disconnected, Degraded and Rejected, a
// heartbeat that corrects a drifted phase, conformance, versions, supportedActions,
// connectedRobots, conditions, the spec, labels and annotations.
var adapterChangeMatters = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return true },
	DeleteFunc: func(event.DeleteEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldA, ok1 := e.ObjectOld.(*fleetv1.FleetAdapter)
		newA, ok2 := e.ObjectNew.(*fleetv1.FleetAdapter)
		return !ok1 || !ok2 || !heartbeatOnly(oldA, newA)
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// heartbeatOnly reports whether old and new differ in nothing but status.lastHeartbeat, apart from
// the bookkeeping every write changes (resourceVersion, managedFields).
func heartbeatOnly(old, new *fleetv1.FleetAdapter) bool {
	a, b := old.DeepCopy(), new.DeepCopy()
	for _, x := range []*fleetv1.FleetAdapter{a, b} {
		x.ResourceVersion = ""
		x.ManagedFields = nil
		x.Status.LastHeartbeat = nil
	}
	return equality.Semantic.DeepEqual(a, b)
}

// zoneHeld reports whether a zone's stop holds dispatch, as the holds read it: its estop status
// Triggered, its trigger, or its processed marker (zoneEstopInForce, which dispatch's zone stop
// hold reads; row A15's estopscope.Carries reads the trigger and the marker, a subset of it). The
// trigger alone is not enough: the hold stands through the processed marker until the clear is
// applied.
func zoneHeld(z *fleetv1.FleetZone) bool {
	return zoneEstopInForce(z) != ""
}

// zoneRestrictionsChanged reports a change of a zone's effective restrictions: status.restrictions
// or its RestrictionsResolved condition, and nothing else. The TDE writes the zone's status on
// every reservation and release, so a wider comparison would wake on every one of them.
func zoneRestrictionsChanged(old, new *fleetv1.FleetZone) bool {
	if !equality.Semantic.DeepEqual(old.Status.Restrictions, new.Status.Restrictions) {
		return true
	}
	co := apimeta.FindStatusCondition(old.Status.Conditions, fleetv1.ZoneCondRestrictionsResolved)
	cn := apimeta.FindStatusCondition(new.Status.Conditions, fleetv1.ZoneCondRestrictionsResolved)
	switch {
	case co == nil && cn == nil:
		return false
	case co == nil || cn == nil:
		return true
	default:
		return co.Status != cn.Status || co.Reason != cn.Reason
	}
}

// zoneSlotsFreed is how many of a zone's TDE slots its status mirror shows freed: the fall in its
// reservations or in its occupied count, whichever is larger. The TDE writes the mirror after
// every release, whatever released it (an action's retire, a preemption, an absent robot, a
// failed commit).
func zoneSlotsFreed(old, new *fleetv1.FleetZone) int {
	freed := len(old.Status.Reservations) - len(new.Status.Reservations)
	if occ := int(old.Status.CurrentConcurrentRobots - new.Status.CurrentConcurrentRobots); occ > freed {
		freed = occ
	}
	return max(freed, 0)
}

// zoneWakeHandler wakes, on a FleetZone update or delete:
//   - when its stop stops holding (or a zone that held is deleted): the free robots in its subtree
//     together (wakeForRobotsIn), picked by status.currentZone or spec.zone as dispatch's zone stop
//     hold picks them, which covers the actions row A15 held there; and the actions there pinned
//     to a robot, which the pin may take from outside the zone;
//   - when its restrictions change: the namespace's free robots together, among the actions whose
//     zone is it, an ancestor of it (an unresolved descendant holds an action on its ancestor), or
//     in its subtree; and those actions pinned to a robot;
//   - when the TDE frees slots in it and an action awaits its capacity: those actions, most urgent
//     first, up to wakeLimit per slot, counting the ones a robot wake already took.
//
// Every wake but the pinned actions' is bounded by the robots that could take the actions.
func (r *FleetActionReconciler) zoneWakeHandler() handler.EventHandler {
	return handler.Funcs{
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			oldZ, ok1 := e.ObjectOld.(*fleetv1.FleetZone)
			newZ, ok2 := e.ObjectNew.(*fleetv1.FleetZone)
			if !ok1 || !ok2 {
				return
			}
			lifted := zoneHeld(oldZ) && !zoneHeld(newZ)
			restricted := zoneRestrictionsChanged(oldZ, newZ)
			freed := 0
			if r.capacityWaiters.any(newZ.Namespace, newZ.Name) {
				freed = zoneSlotsFreed(oldZ, newZ)
			}
			if !lifted && !restricted && freed == 0 {
				return
			}
			enqueueAll(q, r.zoneWakes(ctx, newZ, lifted, restricted, freed, false))
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.RateLimitingInterface) {
			z, ok := e.Object.(*fleetv1.FleetZone)
			if !ok || !zoneHeld(z) {
				return
			}
			enqueueAll(q, r.zoneWakes(ctx, z, true, false, 0, true))
		},
	}
}

func (r *FleetActionReconciler) zoneWakes(ctx context.Context, zone *fleetv1.FleetZone, lifted, restricted bool, freed int,
	deleted bool) []reconcile.Request {
	namespace, name := zone.Namespace, zone.Name
	pending, zones, ok := r.pendingInNamespace(ctx, namespace)
	if !ok {
		return nil
	}
	var all []fleetv1.FleetZone
	if zones != nil {
		all = zones.zones
	}
	if deleted { // the cached list no longer holds it, and its children would lose their parent
		all = append(append([]fleetv1.FleetZone(nil), all...), *zone)
	}
	below := zoneSubtree(name, all)
	pinned := func(a *fleetv1.FleetAction) bool { return a.Spec.RobotSelector != "" }
	var reqs []reconcile.Request
	if freed > 0 {
		waiters := r.capacityWaiters.snapshot(namespace, name)
		// A waiter that is no longer Pending (failed by its deadline, cancelled, deleted) leaves the
		// set here: nothing else would remove it, and the zone's releases would list the namespace
		// for it forever.
		stillPending := make(map[string]bool, len(pending))
		for _, a := range pending {
			stillPending[a.Name] = true
		}
		for w := range waiters {
			if !stillPending[w] {
				r.capacityWaiters.remove(namespace, name, w)
				delete(waiters, w)
			}
		}
		woken := r.pick(pending, wakeLimit*freed, freed, func(a *fleetv1.FleetAction) bool {
			return a.Spec.Zone == name && (waiters[a.Name] || a.Status.Message == awaitingZoneCapacity)
		})
		for _, req := range woken {
			r.capacityWaiters.remove(namespace, name, req.Name)
		}
		reqs = append(reqs, woken...)
	}
	if restricted {
		related := zoneAncestors(name, all)
		for z := range below {
			related[z] = true
		}
		inRelated := func(a *fleetv1.FleetAction) bool { return related[a.Spec.Zone] }
		reqs = append(reqs, r.wakeForRobotsIn(pending, zones, r.robotsIn(ctx, namespace, func(*fleetv1.Robot) bool { return true }), inRelated)...)
		reqs = append(reqs, r.pick(pending, 0, 0, func(a *fleetv1.FleetAction) bool { return inRelated(a) && pinned(a) })...)
	}
	if lifted {
		reqs = append(reqs, r.wakeForRobotsIn(pending, zones, r.robotsIn(ctx, namespace, func(rb *fleetv1.Robot) bool {
			return below[rb.Status.CurrentZone] || below[rb.Spec.Zone]
		}), nil)...)
		reqs = append(reqs, r.pick(pending, 0, 0, func(a *fleetv1.FleetAction) bool { return below[a.Spec.Zone] && pinned(a) })...)
	}
	if len(reqs) > 0 {
		log.FromContext(ctx).Info("zone change woke waiting actions", "zone", name, "namespace", namespace,
			"stopLifted", lifted, "restrictionsChanged", restricted, "slotsFreed", freed, "woken", len(reqs))
	}
	return reqs
}

// capacityWaiters holds, per zone, the actions the TDE denied a slot, recorded before the denial is
// written: a slot freed by the very next reconcile can be seen before that write reaches the cache.
// The zone wake reads it, so a release in a zone with no waiter costs nothing. Entries leave when a
// wake takes them, or when the action is assigned; one left behind costs one wasted wake.
type capacityWaiters struct {
	mu sync.Mutex
	m  map[string]map[string]bool // namespace/zone → action names
}

func capacityKey(namespace, zone string) string { return namespace + "/" + zone }

func (c *capacityWaiters) add(namespace, zone, action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]map[string]bool{}
	}
	k := capacityKey(namespace, zone)
	if c.m[k] == nil {
		c.m[k] = map[string]bool{}
	}
	c.m[k][action] = true
}

func (c *capacityWaiters) remove(namespace, zone, action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := capacityKey(namespace, zone)
	delete(c.m[k], action)
	if len(c.m[k]) == 0 {
		delete(c.m, k)
	}
}

func (c *capacityWaiters) any(namespace, zone string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m[capacityKey(namespace, zone)]) > 0
}

func (c *capacityWaiters) snapshot(namespace, zone string) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for a := range c.m[capacityKey(namespace, zone)] {
		out[a] = true
	}
	return out
}

// namespaceWakeHandler wakes the namespace's free robots together when its stop stops holding
// (namespaceEstopInForce, which dispatch's namespace stop hold reads: the trigger, or the processed
// marker until the clear is applied), or when the SwarmadaConfig that held it is deleted.
func (r *FleetActionReconciler) namespaceWakeHandler() handler.EventHandler {
	wake := func(ctx context.Context, cfg *fleetv1.SwarmadaConfig, q workqueue.RateLimitingInterface) {
		reqs := r.wakeForRobots(ctx, cfg.Namespace, r.robotsIn(ctx, cfg.Namespace, func(*fleetv1.Robot) bool { return true }))
		if len(reqs) > 0 {
			log.FromContext(ctx).Info("namespace stop no longer holds: woke waiting actions",
				"namespace", cfg.Namespace, "woken", len(reqs))
		}
		enqueueAll(q, reqs)
	}
	return handler.Funcs{
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			oldC, ok1 := e.ObjectOld.(*fleetv1.SwarmadaConfig)
			newC, ok2 := e.ObjectNew.(*fleetv1.SwarmadaConfig)
			if ok1 && ok2 && namespaceEstopInForce(oldC) != "" && namespaceEstopInForce(newC) == "" {
				wake(ctx, newC, q)
			}
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.RateLimitingInterface) {
			if cfg, ok := e.Object.(*fleetv1.SwarmadaConfig); ok && namespaceEstopInForce(cfg) != "" {
				wake(ctx, cfg, q)
			}
		},
	}
}

// maintenanceHolds names the robots an Active window holds (candidate filter 8: status.pausedRobots
// and status.windingDownRobots, as drainedRobots reads them). A window in any other phase holds
// none.
func maintenanceHolds(zm *fleetv1.ZoneMaintenance) map[string]bool {
	held := map[string]bool{}
	if zm.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		return held
	}
	for _, p := range zm.Status.PausedRobots {
		held[p.Name] = true
	}
	for _, w := range zm.Status.WindingDownRobots {
		held[w.Name] = true
	}
	return held
}

// maintenanceWakeHandler wakes the robots a window releases, together: the robots it held and
// holds no more (it ended, or let them go), or every robot it held when it is deleted. A robot the
// window kept Idle throughout has no Robot event of its own when it is released. One the window
// writes back to Idle first wakes through wakeHandler and loses to filter 8 while the window is
// still Active; this wake then follows the window's own write.
func (r *FleetActionReconciler) maintenanceWakeHandler() handler.EventHandler {
	wake := func(ctx context.Context, namespace string, released map[string]bool, q workqueue.RateLimitingInterface) {
		if len(released) == 0 {
			return
		}
		reqs := r.wakeForRobots(ctx, namespace, r.robotsIn(ctx, namespace, func(rb *fleetv1.Robot) bool { return released[rb.Name] }))
		if len(reqs) > 0 {
			log.FromContext(ctx).Info("maintenance window released robots: woke waiting actions",
				"namespace", namespace, "released", len(released), "woken", len(reqs))
		}
		enqueueAll(q, reqs)
	}
	return handler.Funcs{
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			oldM, ok1 := e.ObjectOld.(*fleetv1.ZoneMaintenance)
			newM, ok2 := e.ObjectNew.(*fleetv1.ZoneMaintenance)
			if !ok1 || !ok2 {
				return
			}
			released := maintenanceHolds(oldM)
			for name := range maintenanceHolds(newM) {
				delete(released, name)
			}
			wake(ctx, newM.Namespace, released, q)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.RateLimitingInterface) {
			if zm, ok := e.Object.(*fleetv1.ZoneMaintenance); ok {
				wake(ctx, zm.Namespace, maintenanceHolds(zm), q)
			}
		},
	}
}

// robotsIn returns the namespace's robots, from the cache, that match.
func (r *FleetActionReconciler) robotsIn(ctx context.Context, namespace string, match func(*fleetv1.Robot) bool) []*fleetv1.Robot {
	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).Error(err, "listing robots for a wake; their waiting actions wait for their own recheck",
			"namespace", namespace)
		return nil
	}
	var out []*fleetv1.Robot
	for i := range robots.Items {
		if match(&robots.Items[i]) {
			out = append(out, &robots.Items[i])
		}
	}
	return out
}

// zoneSubtree is zone and every zone below it in the listed tree (a cycle ends the walk).
func zoneSubtree(zone string, zones []fleetv1.FleetZone) map[string]bool {
	childrenOf, _ := buildTree(zones)
	out := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if out[name] {
			return
		}
		out[name] = true
		for _, c := range childrenOf[name] {
			walk(c)
		}
	}
	walk(zone)
	return out
}

// zoneAncestors is zone and every zone above it in the listed tree, up to estopscope.MaxAncestors
// (a cycle or a missing parent ends the walk).
func zoneAncestors(zone string, zones []fleetv1.FleetZone) map[string]bool {
	parentOf := make(map[string]string, len(zones))
	for i := range zones {
		parentOf[zones[i].Name] = zones[i].Spec.ParentZone
	}
	out := map[string]bool{zone: true}
	cur := zone
	for range estopscope.MaxAncestors {
		p := parentOf[cur]
		if p == "" || out[p] {
			break
		}
		out[p] = true
		cur = p
	}
	return out
}

func enqueueAll(q workqueue.RateLimitingInterface, reqs []reconcile.Request) {
	for _, req := range reqs {
		q.Add(req)
	}
}
