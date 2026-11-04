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
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/robotid"
)

// Three holds the dispatch choke point applies before any numbered filter, because each
// stands for a stop the robot's own estopState may not show.
//
//   - ONE ROBOT PER WIRE ID. The registrar, the status sink and the command dispatcher
//     route a physical robot by its wire robot_id (robotid.Wire). Two Robots carrying one
//     id are two objects for one machine, and an assign sent to either goes out under
//     that id. If one of them holds a stop, the other is a clean status for the stopped
//     machine. The Robot webhook refuses a duplicate at create, but it checks a list and
//     then admits, and two creates racing each other both pass it. Both objects exist by
//     the time either could be dispatched, so this is where uniqueness is enforced: from
//     an UNCACHED list, every robot whose wire id more than one Robot holds is withheld,
//     whichever of them is stopped, until all but one are gone.
//
//   - NO WORK IN A STOPPED ZONE. A zone estop fans out to the robots in the zone when it
//     is triggered, and a robot that arrives afterwards (created into the zone, or moved
//     there) was never in that fan-out. The zone's stop gates dispatch by itself: a robot
//     whose status.currentZone or spec.zone, or any parentZone ancestor of either, has an
//     estop in force is withheld, whatever its own estopState says.
//
//   - NO WORK IN A STOPPED NAMESPACE. A namespace estop fans out to the robots in the
//     namespace when it is triggered, and a Robot created there afterwards (by a
//     fleet-manager, or admitted from discovery) was never in that fan-out. While the
//     namespace's SwarmadaConfig carries the trigger or the processed marker, every robot
//     in the namespace is withheld, whatever its own estopState says.
//
// Each reports as its own exclusion (filterRobotIDConflict, filterEstop) and none writes
// anything (see the header of dispatch_filters.go). The Robot controller writes the
// RobotIDConflict condition and the zone- and namespace-estop controllers stamp the late
// arrival; this file only refuses the work.

// filterRobotIDConflict reports a robot whose wire robot-id more than one Robot holds.
// It is not one of RFC-0001's numbered candidate filters, so it is a negative sentinel
// like filterAdapterRefusedAction and scheduler.FilterTerminating.
const filterRobotIDConflict = -3

// dispatchHolds is what the three holds read, once per filterDispatchEligible call.
type dispatchHolds struct {
	// holders maps a wire robot-id to the names of the Robots carrying it, from an
	// uncached list; nil when robotsUnread is set.
	holders      map[string][]string
	robotsUnread error

	// zones is the namespace's FleetZones by name; nil when zonesUnread is set.
	zones       map[string]*fleetv1.FleetZone
	zonesUnread error

	// nsStop is why the namespace's estop withholds every robot, or ""; configsUnread is
	// set when the SwarmadaConfig list could not be read.
	nsStop        string
	configsUnread error
	// tree is the zone list as a tree, for the stop guard's walk.
	tree *estopscope.Tree
}

// liveReader is the reader the robot-id hold lists Robots through: the uncached
// APIReader when one is wired, else the client (a unit test's fake client is uncached).
func (r *FleetActionReconciler) liveReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// newDispatchHolds performs the three list reads the holds need. Any failing withholds
// every candidate for this pass (see robotIDConflict, zoneStop and namespaceStop): the
// action requeues, so a transient read error costs latency, never a dispatch nobody could
// check.
//
// The Robot list is uncached and the FleetZone list is not, and the difference is the
// race each one closes. A twin can be admitted in the instant before dispatch, and the
// cache is exactly what has not seen it yet. A zone's estop is written by this manager's
// own zone-estop controller and reaches the cache in the time the watch takes; the robots
// it reaches are also stamped with their own estopState, which filter 10 reads. The
// SwarmadaConfig list is cached for the same reason as the FleetZones.
func (r *FleetActionReconciler) newDispatchHolds(ctx context.Context, namespace string) *dispatchHolds {
	h := &dispatchHolds{}
	var robots fleetv1.RobotList
	if err := r.liveReader().List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		h.robotsUnread = err
	} else {
		h.holders = wireIDHolders(robots.Items)
	}
	// The zone and config lists are read from the API server too (the stop guard): a trigger
	// written a moment ago is in force now, whether or not the cache has seen it.
	var zones fleetv1.FleetZoneList
	if err := r.liveReader().List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		h.zonesUnread = err
	} else {
		h.zones = make(map[string]*fleetv1.FleetZone, len(zones.Items))
		for i := range zones.Items {
			h.zones[zones.Items[i].Name] = &zones.Items[i]
		}
		h.tree = estopscope.TreeOf(zones.Items)
	}
	var configs fleetv1.SwarmadaConfigList
	if err := r.liveReader().List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		h.configsUnread = err
	} else {
		for i := range configs.Items {
			if why := namespaceEstopInForce(&configs.Items[i]); why != "" {
				h.nsStop = why
				break
			}
		}
	}
	return h
}

// wireIDHolders groups robots by wire robot-id. A Robot being deleted still counts: until
// it is gone it is still the object frames for that id may land on.
func wireIDHolders(robots []fleetv1.Robot) map[string][]string {
	out := make(map[string][]string, len(robots))
	for i := range robots {
		id := robotid.Wire(&robots[i])
		out[id] = append(out[id], robots[i].Name)
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

// robotIDConflict returns why robot is withheld for sharing its wire robot-id, or "".
func (h *dispatchHolds) robotIDConflict(robot *fleetv1.Robot) string {
	if h.robotsUnread != nil {
		return fmt.Sprintf("the Robot list could not be read, so whether another Robot carries this "+
			"robot's robot-id is unknown: %v", h.robotsUnread)
	}
	id := robotid.Wire(robot)
	if names := h.holders[id]; len(names) > 1 {
		return fmt.Sprintf("robot-id %q is carried by more than one Robot (%s): commands for that id "+
			"would reach one physical robot through either object, so none of them receives work until "+
			"all but one are deleted", id, strings.Join(names, ", "))
	}
	return ""
}

// zoneStop returns why robot is withheld because a zone it is in, or an ancestor of that
// zone, has an estop in force, or "".
//
// The walk follows spec.parentZone only, from status.currentZone and from spec.zone. That
// is the relation a zone estop propagates along. spec.memberZones is not walked: a
// composite groups zones for targeting and asserts nothing physical, and an estop on a
// composite is refused (FZ-C5), so no composite membership can carry a stop to a robot.
// A composite that is itself in the chain is checked like any other zone.
//
// Every ancestor's stop withholds, whatever its estopPolicy.propagateToChildren says. That
// field decides which robots the fan-out stops; withholding new work from a zone that lies
// inside a stopped one is the cautious reading of the same stop.
//
// It refuses when it cannot tell: the FleetZone list could not be read, or the chain is
// deeper than zoneEstopWalkCap. A zone the list does not contain ends the chain, as it
// does for the fan-out: it exists nowhere, so no stop is held on it.
func (h *dispatchHolds) zoneStop(robot *fleetv1.Robot) string {
	if h.zonesUnread != nil {
		return fmt.Sprintf("the FleetZone list could not be read, so whether a zone estop holds this "+
			"robot is unknown: %v", h.zonesUnread)
	}
	tree := h.tree
	if tree == nil {
		tree = estopscope.TreeOf(nil)
	}
	for _, start := range []struct{ zone, field string }{
		{robot.Status.CurrentZone, "status.currentZone"},
		{robot.Spec.Zone, "spec.zone"},
	} {
		if start.zone == "" {
			continue
		}
		// The stop guard's walk: the zone and up to its bound of ancestors; a chain still
		// climbing past it, or one that cycles, cannot be checked and withholds the robot. A
		// zone the list does not contain ends the chain, as it does for the fan-out.
		ancestors, err := tree.Ancestors(start.zone)
		if err != nil {
			return fmt.Sprintf("the parentZone chain above zone %q (%s) is deeper than %d zones, so "+
				"whether a zone estop holds this robot is unknown", start.zone, start.field, estopscope.MaxAncestors)
		}
		if z := tree.Zone(start.zone); z != nil {
			if why := zoneEstopInForce(z); why != "" {
				return fmt.Sprintf("robot is in zone %q (%s), %s", start.zone, start.field, why)
			}
		}
		for _, z := range ancestors {
			if why := zoneEstopInForce(z); why != "" {
				return fmt.Sprintf("robot is in zone %q (%s), inside zone %q, %s",
					start.zone, start.field, z.Name, why)
			}
		}
	}
	return ""
}

// zoneEstopInForce says how zone z is stopped, or returns "": its estop status reads
// Triggered, or an estop has been requested on it and not cleared, which covers the
// moment between the trigger and its fan-out.
func zoneEstopInForce(z *fleetv1.FleetZone) string {
	if z.Status.EstopStatus == fleetv1.ZoneEstopTriggered {
		return "whose estop is Triggered"
	}
	if _, requested := z.Annotations[annEstopTriggered]; requested {
		return fmt.Sprintf("on which an estop is requested (%s) and not cleared", annEstopTriggered)
	}
	if _, marked := z.Annotations[annEstopProcessed]; marked {
		return fmt.Sprintf("whose estop clear has not been applied yet (it carries %s)", annEstopProcessed)
	}
	return ""
}

// namespaceStop returns why robot is withheld because its namespace's estop is in force,
// or "". It refuses when the SwarmadaConfig list could not be read. A namespace with no
// SwarmadaConfig has no namespace estop.
func (h *dispatchHolds) namespaceStop(robot *fleetv1.Robot) string {
	if h.configsUnread != nil {
		return fmt.Sprintf("the SwarmadaConfig list could not be read, so whether a namespace estop holds "+
			"this robot is unknown: %v", h.configsUnread)
	}
	if h.nsStop != "" {
		return fmt.Sprintf("robot is in namespace %q, %s", robot.Namespace, h.nsStop)
	}
	return ""
}

// namespaceEstopInForce says how a namespace's SwarmadaConfig holds its estop, or returns
// "": an estop is requested and not cleared (from the trigger to the clear, the moment
// before the fan-out included), or the trigger is gone and the clear has not yet been
// applied (the processed marker is still there, so the robots have not been moved to
// Resuming).
func namespaceEstopInForce(cfg *fleetv1.SwarmadaConfig) string {
	if _, requested := cfg.Annotations[annEstopTriggered]; requested {
		return fmt.Sprintf("whose estop is in force (SwarmadaConfig %q carries %s)", cfg.Name, annEstopTriggered)
	}
	if _, marked := cfg.Annotations[annEstopProcessed]; marked {
		return fmt.Sprintf("whose estop clear has not been applied yet (SwarmadaConfig %q carries %s)",
			cfg.Name, annEstopProcessed)
	}
	return ""
}
