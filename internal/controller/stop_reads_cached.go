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

// Stop checks read from the cache, and are confirmed past it before any decision that acts on
// "no stop holds".
//
// Every stop check used to list the namespace's FleetZones and SwarmadaConfigs from the API
// server: the dispatch pass twice (row A15 and the dispatch holds), the stop finalizer's sync on
// every bound action's reconcile, and robot_estop on every Robot event. Those lists share one
// client rate-limit bucket per kind, and at 50 robots they held the dispatch latency tail.
//
// The rule now: the cache may say that a stop holds, and is obeyed; it may never alone say that
// no stop holds for a decision that acts on it. Such a decision first reads, past the cache,
// exactly the objects it depends on, before the first write it leads to:
//   - an assignment: right after selection and before the TDE gate (confirmNoStop), so an abort
//     writes nothing and asks the TDE for nothing;
//   - the release of a stop finalizer: syncHeldFinalizer, and robot_estop's two callers of
//     robotKeepsStopFinalizerCacheFirst;
//   - a Paused or Revoking action acting on "no stop" (stopNamesCacheFirst).
//
// A confirmation that finds a stop sends the action's next pass down today's path, reading
// every stop from the API server (uncachedNext), so the hold it writes is the one it always
// wrote. A read that fails, fails closed.

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// confirmZoneGets is how many zones a confirmation reads one by one. A confirmation that needs
// more reads the namespace's zone list once instead, and judges every chain from that one list.
const confirmZoneGets = 4

// confirmReader is the uncached reader confirmations read through: ConfirmReader when wired (it
// carries its own caller label), else the live reader.
func (r *FleetActionReconciler) confirmReader() client.Reader {
	if r.ConfirmReader != nil {
		return r.ConfirmReader
	}
	return r.liveReader()
}

// releaseReader is the uncached reader a stop finalizer's release is confirmed through.
func (r *FleetActionReconciler) releaseReader() client.Reader {
	if r.ReleaseReader != nil {
		return r.ReleaseReader
	}
	return r.confirmReader()
}

// stopPathReader is the uncached reader the Paused and Revoking stop checks read through.
func (r *FleetActionReconciler) stopPathReader() client.Reader {
	if r.StopPathReader != nil {
		return r.StopPathReader
	}
	return r.liveReader()
}

// stopScanReader is the reader a dispatch pass scans the stops through, and whether it is the
// cache: the cache, unless the action's previous pass found a stop the cache had not shown
// (uncachedNext), or the action is already held by row A15 (a stale cache must not be what lifts
// that hold). The flag is returned, not derived by comparing readers: a reader's dynamic type need
// not be comparable.
func (r *FleetActionReconciler) stopScanReader(action *fleetv1.FleetAction) (client.Reader, bool) {
	if r.uncachedNext.has(client.ObjectKeyFromObject(action)) || zoneStoppedHeld(action) {
		return r.liveReader(), false
	}
	return r.Client, true
}

// uncachedPasses names actions whose next dispatch pass reads every stop past the cache. A
// confirmation that finds a stop the cached scan missed marks the action; the pass that reads
// uncached clears the mark when it ends.
type uncachedPasses struct {
	mu  sync.Mutex
	set map[types.NamespacedName]bool
}

func (u *uncachedPasses) mark(key types.NamespacedName) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.set == nil {
		u.set = map[types.NamespacedName]bool{}
	}
	u.set[key] = true
}

func (u *uncachedPasses) has(key types.NamespacedName) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.set[key]
}

func (u *uncachedPasses) clear(key types.NamespacedName) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.set, key)
}

// confirmNoStop reads past the cache every stop the cached scan judged for assigning action to
// robot, and returns why the assignment must not go ahead, or "" when nothing holds it. It reads:
//   - the robot: its own stop (estopHoldsWork, or its trigger or processed marker), its zones.
//     When verified, robot is at the version currentCandidate read past the cache a moment ago,
//     and the robot commit is locked to that version, so a stop or a move written onto the robot
//     since fails the commit; it is not read again. Otherwise (the preemption path, or a re-read
//     that failed) it is read with one GET;
//   - the zones of its status.currentZone and spec.zone chains, and of the action's spec.zone
//     chain, each walked up spec.parentZone as the fresh objects name it, one GET a zone, or one
//     list of the namespace's zones when the chains need more than confirmZoneGets;
//   - the namespace's SwarmadaConfigs (one list: any of them can carry the namespace's stop).
//
// Each chain is judged by the code that judged it from the cache: the robot's two by zoneStop,
// the destination by Tree.Reaching with estopscope.Carries, the namespace by
// namespaceEstopInForce. A read that fails returns an error, and the caller assigns nothing.
func (r *FleetActionReconciler) confirmNoStop(ctx context.Context, action *fleetv1.FleetAction, robot *fleetv1.Robot,
	verified bool, cached *estopscope.Tree) (string, error) {
	rd := r.confirmReader()
	live := robot
	if !verified {
		live = &fleetv1.Robot{}
		if err := rd.Get(ctx, client.ObjectKeyFromObject(robot), live); err != nil {
			return "", fmt.Errorf("reading robot %s to confirm no stop holds it: %w", robot.Name, err)
		}
	}
	if estopHoldsWork(live) || carriesOwnStop(live) {
		return fmt.Sprintf("robot %s is under a stop (estopState %q, or its own stop requested)", live.Name,
			live.Status.EstopState), nil
	}
	starts := []string{live.Status.CurrentZone, live.Spec.Zone, action.Spec.Zone}
	zones, err := freshZoneChains(ctx, rd, action.Namespace, starts, chainZones(cached, starts) > confirmZoneGets)
	if err != nil {
		return "", err
	}
	h := &dispatchHolds{zones: make(map[string]*fleetv1.FleetZone, len(zones))}
	for i := range zones {
		h.zones[zones[i].Name] = &zones[i]
	}
	h.tree = estopscope.TreeOf(zones)
	if why := h.zoneStop(live); why != "" {
		return why, nil
	}
	if action.Spec.Zone != "" {
		reaching, err := h.tree.Reaching(action.Spec.Zone, estopscope.Carries)
		if err != nil {
			return fmt.Sprintf("the zone tree above the destination zone %s could not be checked: %v",
				action.Spec.Zone, err), nil
		}
		if len(reaching) > 0 {
			return fmt.Sprintf("the destination zone %s is under the emergency stop of %v", action.Spec.Zone, reaching), nil
		}
	}
	var configs fleetv1.SwarmadaConfigList
	if err := rd.List(ctx, &configs, client.InNamespace(action.Namespace)); err != nil {
		return "", fmt.Errorf("listing SwarmadaConfigs to confirm no namespace stop holds: %w", err)
	}
	for i := range configs.Items {
		if why := namespaceEstopInForce(&configs.Items[i]); why != "" {
			return fmt.Sprintf("namespace %q, %s", action.Namespace, why), nil
		}
	}
	return "", nil
}

// freshZoneChains reads past the cache every zone on the chains that start at starts, each
// walked up spec.parentZone as the fresh object names it. A zone that does not exist ends its
// chain, as it does for the fan-out. The namespace's zones are listed once instead, and every
// chain judged from that one read, when listFirst (the cache already shows more than
// confirmZoneGets zones on the chains), or when the walk needs more than that many after all (a
// chain longer than the cache knew): the GETs made so far are then dropped.
//
// A chain longer than the stop guard's bound is therefore always judged from the list, where the
// tree's own walk refuses it (estopscope.Tree.Ancestors). A cycle stops the GET walk at the first
// zone seen twice, without error; the tree built from the zones read finds the cycle in its own walk
// and refuses it the same way.
func freshZoneChains(ctx context.Context, rd client.Reader, namespace string, starts []string, listFirst bool) ([]fleetv1.FleetZone, error) {
	if listFirst {
		return listZones(ctx, rd, namespace)
	}
	seen := map[string]bool{}
	var out []fleetv1.FleetZone
	for _, start := range starts {
		for name := start; name != "" && !seen[name]; {
			if len(seen) == confirmZoneGets {
				return listZones(ctx, rd, namespace)
			}
			seen[name] = true
			z := &fleetv1.FleetZone{}
			if err := rd.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, z); err != nil {
				if errors.IsNotFound(err) {
					break
				}
				return nil, fmt.Errorf("reading zone %s to confirm no stop holds: %w", name, err)
			}
			out = append(out, *z)
			name = z.Spec.ParentZone
		}
	}
	return out, nil
}

// chainZones counts the distinct zones on the chains that start at starts in the cached tree: each
// zone and its ancestors. A chain the cached tree cannot walk counts as longer than any bound.
func chainZones(cached *estopscope.Tree, starts []string) int {
	if cached == nil {
		return 0
	}
	seen := map[string]bool{}
	for _, start := range starts {
		if start == "" || cached.Zone(start) == nil {
			continue
		}
		seen[start] = true
		ancestors, err := cached.Ancestors(start)
		if err != nil {
			return estopscope.MaxAncestors + 1
		}
		for _, a := range ancestors {
			seen[a.Name] = true
		}
	}
	return len(seen)
}

func listZones(ctx context.Context, rd client.Reader, namespace string) ([]fleetv1.FleetZone, error) {
	var zones fleetv1.FleetZoneList
	if err := rd.List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing zones to confirm no stop holds: %w", err)
	}
	return zones.Items, nil
}

// stopNamesCacheFirst is stopNames with the zone and config read from the cache when the cache
// shows a stop over robot, and from the API server (live) when it shows none: a stop the cache
// shows holds, and "no stop" is only ever the API server's answer. robot is the caller's own
// copy, read live where the caller needs its estop state.
func stopNamesCacheFirst(ctx context.Context, cache, live client.Reader, robot *fleetv1.Robot) ([]string, error) {
	if stops, err := stopNames(ctx, cache, robot); err == nil && len(stops) > 0 {
		return stops, nil
	}
	return stopNames(ctx, live, robot)
}

// zoneStoppedHeld reports whether action carries row A15's hold (condition ZoneStopped True).
func zoneStoppedHeld(action *fleetv1.FleetAction) bool {
	for _, c := range action.Status.Conditions {
		if c.Type == conditionZoneStopped && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// robotKeepsStopFinalizerCacheFirst is robotKeepsStopFinalizer judged from the cache, confirmed
// past it before a release. A stop the cache shows keeps the finalizer. When the cache shows none:
// a robot without the finalizer is left without it (it is added once the cache shows the stop, a
// known limit), and a robot with it is judged again with the stops read from the API server, and
// loses it only if that also shows no stop. The robot judged is the caller's own copy, the one it
// patches: its callers patch under the copy's resourceVersion, so a robot written since fails the
// patch instead of losing its finalizer, and a marker the caller has just removed in memory counts
// as removed.
func robotKeepsStopFinalizerCacheFirst(ctx context.Context, cache, live client.Reader, robot *fleetv1.Robot) (bool, error) {
	keep, err := robotKeepsStopFinalizer(ctx, cache, robot)
	if err == nil && keep {
		return true, nil
	}
	if err == nil && !controllerutil.ContainsFinalizer(robot, estopscope.StopFinalizer) {
		return false, nil
	}
	return robotKeepsStopFinalizer(ctx, live, robot)
}
