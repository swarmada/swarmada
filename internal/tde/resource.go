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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ResourceState is the outcome of an adapter-initiated shared-resource reservation
// (§5.4.5). It maps onto the wire ReservationState.
type ResourceState string

// ResourceState values.
const (
	// ResourceGranted: the robot holds the resource and may proceed.
	ResourceGranted ResourceState = "Granted"
	// ResourceQueued: the robot is queued behind the current holder.
	ResourceQueued ResourceState = "Queued"
	// ResourceDenied: the resource is unknown/unreachable or the engine is not ready.
	ResourceDenied ResourceState = "Denied"
)

// ResourceOutcome is the synchronous result of ReserveResource/ReleaseResource.
type ResourceOutcome struct {
	State         ResourceState
	QueuePosition int // 1-based rank in the wait queue; 0 when granted
	Message       string
	Released      bool // ReleaseResource only: whether a hold/queued entry was removed
	// PromotedRobotID is the robot promoted from the wait queue to holder by a
	// release (empty if none) — the control plane must push it a reservation_granted.
	PromotedRobotID string
}

// ReserveResource reserves a single shared resource for a robot on its behalf
// (§5.4.5), resolving the resource's owning zone within the namespace. It is
// idempotent (an existing hold/queued entry for the robot returns its current
// state) and FAILS CLOSED: an unknown resource, or an engine that has not yet
// recovered reservation state, is Denied — never granted.
func (e *Engine) ReserveResource(ctx context.Context, namespace, resourceName, robotID string) (ResourceOutcome, error) {
	// Fail closed until reservation state has been recovered (§9.4.7): a resource
	// grant is a grant, and must not race an unrebuilt queue.
	if !e.recovered.Load() {
		return ResourceOutcome{State: ResourceDenied, Message: "TDE recovering"}, nil
	}
	zone, ok := e.resourceZone(ctx, namespace, resourceName)
	if !ok {
		return ResourceOutcome{State: ResourceDenied, Message: "unknown shared resource: " + resourceName}, nil
	}

	states, unlock := e.lockZones(namespace, zone)
	defer unlock()
	zs := states[zoneKey(namespace, zone)]
	now := e.now()

	q := zs.resources[resourceName]
	if q == nil {
		q = &fleetv1.SharedResourceQueue{ResourceName: resourceName}
		zs.resources[resourceName] = q
	}

	// Idempotent: report the robot's existing position rather than double-inserting.
	for i := range q.CurrentHolders {
		if q.CurrentHolders[i].RobotID == robotID {
			return ResourceOutcome{State: ResourceGranted}, nil
		}
	}
	for i := range q.WaitQueue {
		if q.WaitQueue[i].RobotID == robotID {
			return ResourceOutcome{State: ResourceQueued, QueuePosition: i + 1}, nil
		}
	}

	capacity := e.resourceCapacity(ctx, namespace, zone, resourceName)
	if int32(len(q.CurrentHolders)) < capacity {
		q.CurrentHolders = append(q.CurrentHolders, fleetv1.ResourceHolder{
			RobotID: robotID, ActionID: robotID, Priority: fleetv1.ActionPriorityNormal,
			HeldSince: metav1.NewTime(now),
		})
		e.mirror(ctx, namespace, zone, zs)
		return ResourceOutcome{State: ResourceGranted}, nil
	}

	// Occupied → queue behind the holder, ordered by the resource's policy. ActionID is
	// keyed to the robot (adapter reservations are robot-, not action-, identified), so
	// the existing action-keyed release/promotion helpers apply unchanged.
	policy := e.resourcePolicy(ctx, namespace, zone, resourceName)
	insertByPolicy(q, fleetv1.WaitQueueEntry{
		RobotID: robotID, ActionID: robotID, Priority: fleetv1.ActionPriorityNormal,
		RequestedAt: metav1.NewTime(now),
	}, policy)
	pos := 0
	for i := range q.WaitQueue {
		if q.WaitQueue[i].RobotID == robotID {
			pos = i + 1
			break
		}
	}
	e.mirror(ctx, namespace, zone, zs)
	return ResourceOutcome{State: ResourceQueued, QueuePosition: pos}, nil
}

// ReleaseResource releases a robot's hold (or queued entry) on a shared resource,
// promoting the next waiter to holder if the holder was released (§5.4.5). An
// unknown resource is Denied; a robot with no hold/entry reports Released=false.
func (e *Engine) ReleaseResource(ctx context.Context, namespace, resourceName, robotID string) (ResourceOutcome, error) {
	zone, ok := e.resourceZone(ctx, namespace, resourceName)
	if !ok {
		return ResourceOutcome{State: ResourceDenied, Message: "unknown shared resource: " + resourceName}, nil
	}

	states, unlock := e.lockZones(namespace, zone)
	defer unlock()
	zs := states[zoneKey(namespace, zone)]
	q := zs.resources[resourceName]
	if q == nil {
		return ResourceOutcome{Released: false}, nil
	}

	wasHolder := false
	for i := range q.CurrentHolders {
		if q.CurrentHolders[i].RobotID == robotID {
			wasHolder = true
			break
		}
	}
	present := wasHolder
	for i := range q.WaitQueue {
		if q.WaitQueue[i].RobotID == robotID {
			present = true
			break
		}
	}
	// Releasing a holder frees exactly one slot, so at most the head waiter is
	// promoted; capture it before the release (waiters exist only at capacity).
	promoted := ""
	if wasHolder && len(q.WaitQueue) > 0 {
		promoted = q.WaitQueue[0].RobotID
	}
	// releaseHolder keys by ActionID, which for adapter reservations equals robotID; it
	// drops a queued waiter or releases the holder and backfills from the queue.
	capacity := e.resourceCapacity(ctx, namespace, zone, resourceName)
	releaseHolder(q, robotID, e.now(), capacity)
	e.mirror(ctx, namespace, zone, zs)
	return ResourceOutcome{Released: present, PromotedRobotID: promoted}, nil
}

// resourceZone finds the zone in namespace that declares resourceName — the owning
// zone for a shared resource (§5.4.5). A SharedResource (a lift, a corridor) is
// declared once, so a namespace scan yields the same owner the up-tree walk would;
// ambiguity is resolved deterministically by the lowest zone name. Returns
// ("", false) when no zone declares it (a fail-closed "unknown resource").
func (e *Engine) resourceZone(ctx context.Context, namespace, resourceName string) (string, bool) {
	var zones fleetv1.FleetZoneList
	if err := e.client.List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		return "", false
	}
	owner := ""
	for i := range zones.Items {
		z := &zones.Items[i]
		for j := range z.Spec.SharedResources {
			if z.Spec.SharedResources[j].Name == resourceName {
				if owner == "" || z.Name < owner {
					owner = z.Name
				}
			}
		}
	}
	return owner, owner != ""
}
