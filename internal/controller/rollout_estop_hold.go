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
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// An over-the-air update is dispatch: a firmware pull or a model push reaches a robot the
// same way work does, so a robot a stop holds takes no update (RFC-0001 #firmwarerollout: a
// robot receiving an emergency stop is not rebooted, and every update attempt is gated on
// the robot's live state). The rollouts read the same holds a FleetAction dispatch reads:
// the robot's own stop state, trigger and record (heldFromUpdate), the stop of a zone it is
// in or of an ancestor of that zone, whatever that ancestor's propagateToChildren says, and
// the namespace's stop. The zone and namespace holds cover a robot the fan-out has not yet
// reached, and a robot below a stopped zone that does not propagate to its children.

// rolloutHolds is what one rollout pass reads to decide whether a robot is held.
type rolloutHolds struct {
	h *dispatchHolds
}

// readRolloutHolds lists the namespace's FleetZones and SwarmadaConfigs once for a pass. A
// failed read is returned as an error: the pass is not run, so no robot is entered,
// pushed or withdrawn on a hold nobody could check, and the reconcile is retried.
func readRolloutHolds(ctx context.Context, c client.Reader, namespace string) (*rolloutHolds, error) {
	h := &dispatchHolds{zones: map[string]*fleetv1.FleetZone{}}
	var zones fleetv1.FleetZoneList
	if err := c.List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing FleetZones for the estop holds: %w", err)
	}
	for i := range zones.Items {
		h.zones[zones.Items[i].Name] = &zones.Items[i]
	}
	h.tree = estopscope.TreeOf(zones.Items)
	var configs fleetv1.SwarmadaConfigList
	if err := c.List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing SwarmadaConfigs for the estop holds: %w", err)
	}
	for i := range configs.Items {
		if why := namespaceEstopInForce(&configs.Items[i]); why != "" {
			h.nsStop = why
			break
		}
	}
	return &rolloutHolds{h: h}, nil
}

// why returns why rob takes no update, or "" when no stop holds it.
func (rh *rolloutHolds) why(rob *fleetv1.Robot) string {
	if why := updateHoldReason(rob); why != "" {
		return why
	}
	if why := rh.h.zoneStop(rob); why != "" {
		return why
	}
	return rh.h.namespaceStop(rob)
}

// updateHoldReason says why heldFromUpdate holds rob, or returns "" when it does not.
func updateHoldReason(rob *fleetv1.Robot) string {
	if !heldFromUpdate(rob) {
		return ""
	}
	switch s := rob.Status.EstopState; s {
	case fleetv1.RobotEstopStopping, fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed:
		return fmt.Sprintf("robot's estopState is %s", s)
	case fleetv1.RobotEstopResuming:
		return "robot is Resuming and has not been released by an operator"
	}
	if h, held := estopscope.HeldOnRecord(rob); held {
		if h.Kind == estopscope.KindRobot {
			return fmt.Sprintf("robot carries its own %s annotation", estopscope.TriggerAnnotation)
		}
		return fmt.Sprintf("robot records the stop of %s %q (status.estopHeldBy)", h.Kind, h.Name)
	}
	return "robot is under an emergency stop"
}

// heldNow re-reads rob from reader, uncached where the caller wires an API reader, and
// says why it takes no update, or "". A model push cannot be taken back once it is sent,
// so the decision is made on the robot as the API server stores it, not on the copy the
// pass listed from the cache. A failed read holds the robot for this pass.
func heldNow(ctx context.Context, reader client.Reader, rob *fleetv1.Robot) string {
	fresh := &fleetv1.Robot{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(rob), fresh); err != nil {
		return fmt.Sprintf("robot could not be re-read before the push: %v", err)
	}
	return updateHoldReason(fresh)
}

// heldNotes remembers which held robots a rollout has already reported, so a robot that
// stays held across many passes raises its Event once rather than once per pass (an Event
// per pass would also spend the rate limit the rollout's other Events share). A robot is
// forgotten when no stop holds it, so a later stop reports again. The zero value is ready.
type heldNotes struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (n *heldNotes) first(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen[key] {
		return false
	}
	if n.seen == nil {
		n.seen = map[string]bool{}
	}
	n.seen[key] = true
	return true
}

func (n *heldNotes) forget(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.seen, key)
}

// heldNoteKey identifies one robot in one rollout for heldNotes.
func heldNoteKey(rollout client.Object, robot, what string) string {
	return string(rollout.GetUID()) + "/" + rollout.GetName() + "/" + robot + "/" + what
}

// rolloutsOfNamespace enqueues every rollout of one kind in obj's namespace. A FleetZone or
// SwarmadaConfig event can start or end a hold without writing any Robot, so the rollouts
// are woken by those too.
func rolloutsOfNamespace(ctx context.Context, c client.Reader, list client.ObjectList, obj client.Object) []reconcile.Request {
	if err := c.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	switch l := list.(type) {
	case *fleetv1.FirmwareRolloutList:
		for i := range l.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&l.Items[i])})
		}
	case *fleetv1.ModelRolloutList:
		for i := range l.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&l.Items[i])})
		}
	}
	return reqs
}

// liveReader is the reader a push decision re-reads the robot through: the uncached
// APIReader when one is wired, else the client.
func (r *ModelRolloutReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
