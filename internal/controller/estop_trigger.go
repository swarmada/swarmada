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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// estopReasonNoneGiven is the reason recorded for a trigger written with an empty value.
const estopReasonNoneGiven = "none given"

// estopReason is the reason recorded for a trigger value. The presence of the trigger
// annotation is the stop, whatever its value (RFC-0001 #safety-estop-scopes), so no value
// is refused: an empty one is recorded as estopReasonNoneGiven, and "true" — what
// `swarmctl estop trigger` writes when no --reason is given — as the scope's own default.
func estopReason(trigger, scopeDefault string) string {
	switch trigger {
	case "":
		return estopReasonNoneGiven
	case "true":
		return scopeDefault
	}
	return trigger
}

// readStandingStops reads what estopscope.StandingStop needs to say which stop, other than
// the one being cleared, still holds a robot: the namespace's FleetZones and its
// SwarmadaConfigs. A clear that cannot read them is not applied, and is retried: a robot
// it would have released may still be held by a stop it could not see.
func readStandingStops(ctx context.Context, c client.Reader, namespace string) ([]fleetv1.FleetZone, []fleetv1.SwarmadaConfig, error) {
	var zones fleetv1.FleetZoneList
	if err := c.List(ctx, &zones, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing zones to find the stops still in force: %w", err)
	}
	var configs fleetv1.SwarmadaConfigList
	if err := c.List(ctx, &configs, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing SwarmadaConfigs to find the stops still in force: %w", err)
	}
	return zones.Items, configs.Items, nil
}

// heldByStandingStop tells the operator, on the robot, that a clear left it stopped because
// another stop still holds it: its own robot-scope trigger (EstopHeldByOwnTrigger), or a
// zone's or its namespace's stop (EstopHeldByWiderStop). A clear of one scope is not the
// clear of another (RFC-0001 #safety-estop-scopes); the clear of the stop named here is.
func heldByStandingStop(rec record.EventRecorder, robot *fleetv1.Robot, clearedScope string, h estopscope.Holder) {
	if rec == nil {
		return
	}
	reason := "EstopHeldByWiderStop"
	if h.Kind == estopscope.KindRobot {
		reason = "EstopHeldByOwnTrigger"
	}
	rec.Event(robot, corev1.EventTypeWarning, reason, fmt.Sprintf(
		"%s estop cleared; the robot stays %s: it is still held by %s",
		clearedScope, stateOrNormal(robot.Status.EstopState), h))
}

// heldRobots collects, for a clear's audit entry, the robots another stop still holds:
// those held by their own robot-scope trigger, and those held by a zone's or the
// namespace's stop, each as robot:kind/carrier.
type heldRobots struct {
	own, wider []string
}

func (h *heldRobots) add(robot string, by estopscope.Holder) {
	if by.Kind == estopscope.KindRobot {
		h.own = append(h.own, robot)
		return
	}
	h.wider = append(h.wider, robot+":"+string(by.Kind)+"/"+by.Name)
}

// detail adds the held robots to a clear's audit detail.
func (h *heldRobots) detail(d map[string]string) map[string]string {
	d["held_by_own_estop"] = strings.Join(h.own, ",")
	d["held_by_wider_estop"] = strings.Join(h.wider, ",")
	return d
}
