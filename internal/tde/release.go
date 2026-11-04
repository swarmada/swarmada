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

package tde

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// GrantNotifier tells a robot's adapter that its queued shared-resource reservation is
// now held (a reservation_granted Command, RFC-0001 #tde-shared-resource-queue-management).
// It is best-effort: a robot not told keeps waiting at the boundary, and its next
// reserve reports Granted.
type GrantNotifier func(ctx context.Context, namespace, robotID, resourceName string)

// WithGrantNotifier installs the notifier used when the engine itself promotes a waiter:
// on releasing the holds of a Robot that no longer exists, and on recovery. A release the
// adapter asks for is answered by the ControlStream server, which pushes that grant.
func (e *Engine) WithGrantNotifier(f GrantNotifier) *Engine {
	e.notify = f
	return e
}

// promotion is an adapter-initiated waiter that a release made a holder.
type promotion struct {
	namespace, robotID, resource string
}

// tell pushes each promotion's grant, outside every zone lock.
func (e *Engine) tell(ctx context.Context, ps []promotion) {
	if e.notify == nil {
		return
	}
	for _, p := range ps {
		e.notify(ctx, p.namespace, p.robotID, p.resource)
	}
}

// adapterHolders returns the robots holding q through an adapter-initiated entry.
func adapterHolders(q *fleetv1.SharedResourceQueue) map[string]bool {
	out := make(map[string]bool, len(q.CurrentHolders))
	for _, h := range q.CurrentHolders {
		if h.ActionID == h.RobotID {
			out[h.RobotID] = true
		}
	}
	return out
}

// promotedSince returns the adapter-initiated holders of q that were not holders before.
func promotedSince(namespace string, q *fleetv1.SharedResourceQueue, before map[string]bool) []promotion {
	var out []promotion
	for _, h := range q.CurrentHolders {
		if h.ActionID == h.RobotID && !before[h.RobotID] {
			out = append(out, promotion{namespace: namespace, robotID: h.RobotID, resource: q.ResourceName})
		}
	}
	return out
}

// ReleaseAbsentRobots releases every adapter-initiated shared-resource hold and wait in
// namespace whose Robot no longer exists, promoting the next waiters, and tells each
// promoted adapter-initiated waiter that it holds the resource. The Robot controller calls
// it when a Robot is deleted, so an operator frees a hold whose robot will not return by
// deleting that Robot (RFC-0001 #tde-normative-invariants, TDE-7).
//
// It applies the rule recovery applies (keepAdapterEntry), so the two cannot disagree
// about which holds survive. Before the engine has recovered it does nothing: recovery
// owns the state until then and releases the same entries itself. A Robot lookup that
// fails is returned, and nothing is released on a guess.
func (e *Engine) ReleaseAbsentRobots(ctx context.Context, namespace string) error {
	if !e.recovered.Load() || e.client == nil {
		return nil
	}
	var zl fleetv1.FleetZoneList
	if err := e.client.List(ctx, &zl, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing zones in %s: %w", namespace, err)
	}
	names := make([]string, 0, len(zl.Items))
	for i := range zl.Items {
		if len(zl.Items[i].Spec.SharedResources) > 0 {
			names = append(names, zl.Items[i].Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	states, unlock := e.lockZones(namespace, names...)
	var promoted []promotion
	for _, zone := range names {
		zs := states[zoneKey(namespace, zone)]
		changed := false
		for _, rname := range sortedResourceNames(zs.resources) {
			q := zs.resources[rname]
			for _, id := range queueActionIDs(q) {
				if _, ok := adapterEntryRobot(q, id); !ok {
					continue
				}
				keep, err := keepAdapterEntry(ctx, e.client, namespace, q, id)
				if err != nil {
					unlock()
					return fmt.Errorf("releasing %s in zone %s/%s: %w", rname, namespace, zone, err)
				}
				if keep {
					continue
				}
				before := adapterHolders(q)
				releaseHolder(q, id, e.now(), e.resourceCapacity(ctx, namespace, zone, rname))
				promoted = append(promoted, promotedSince(namespace, q, before)...)
				changed = true
			}
		}
		if changed {
			e.mirror(ctx, namespace, zone, zs)
		}
	}
	unlock()
	e.tell(ctx, promoted)
	return nil
}

func sortedResourceNames(m map[string]*fleetv1.SharedResourceQueue) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
