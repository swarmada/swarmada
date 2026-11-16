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
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A zone stop written in the instant the only robot is freed for an action into that zone. The
// robot's event can reach the manager's cache before the zone's, so the pass the robot wakes may
// scan the stops from a cache that does not show the stop yet. The stop confirmation reads the
// zone past the cache: the action is never assigned, and row A15 holds it within 1 s.
func TestEnvtest_AZoneStopWrittenAsItsRobotFreesHoldsTheAction(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	seedWakeFleet(t, ns, "amr-1")
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { r.Status.Phase = fleetv1.RobotPhaseOffline })
	startActionManager(t, ns)
	req := seedWaitingAction(t, ns, "pick-1", "aisle-1", nil)
	waitWaiting(t, req, "")

	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "aisle-1"}}
	since := time.Now()
	editObject(t, zone, func() {
		if zone.Annotations == nil {
			zone.Annotations = map[string]string{}
		}
		zone.Annotations[annEstopTriggered] = "stop-1"
	})
	editRobotStatus(t, ns, "amr-1", func(r *fleetv1.Robot) { r.Status.Phase = fleetv1.RobotPhaseIdle })

	var heldAfter time.Duration
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		a := zrGetAction(t, req)
		if a.Status.AssignedRobot != "" {
			t.Fatalf("assigned to %q into a zone under a stop", a.Status.AssignedRobot)
		}
		if heldAfter == 0 && apimeta.IsStatusConditionTrue(a.Status.Conditions, conditionZoneStopped) {
			heldAfter = time.Since(since)
		}
	}
	if heldAfter == 0 || heldAfter > time.Second {
		t.Fatalf("row A15's hold came after %v; want it within 1 s", heldAfter)
	}
	t.Logf("held by row A15 %v after the stop, never assigned", heldAfter.Round(time.Millisecond))
}
