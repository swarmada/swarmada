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

package webhook

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
)

// An empty-valued trigger at zone and namespace scope stops the robots in scope, and work
// created while it is in force stays Pending, as at robot scope
// (TestEnvtest_AnEmptyRobotTriggerStopsTheRobot). The stop is waited for first: a new
// FleetAction is asked about only once the controller has acted on the trigger, so a
// controller that took the empty value for an estop already issued fails here, not later.

// assertPending dispatches a new FleetAction pinned to robot and requires it to stay
// Pending, assigned to nobody.
func assertPending(t *testing.T, w *safetyWire, name, robot string) {
	t.Helper()
	got := pinnedDispatch(t, w, name, robot)
	if got.Status.AssignedRobot != "" || got.Status.Phase != fleetv1.ActionPhasePending {
		t.Errorf("new FleetAction %s: assignedRobot=%q phase=%q message=%q; want Pending and assigned to nobody "+
			"while an empty-valued stop is in force", name, got.Status.AssignedRobot, got.Status.Phase, got.Status.Message)
	}
}

func TestEnvtest_AnEmptyZoneTriggerKeepsNewWorkPending(t *testing.T) {
	w, _ := presenceFixture(t)
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: markerRobotZone}}

	setTrigger(t, w, zone, ptrTo(""))
	eventually(t, "the empty zone trigger stopped the zone's robots and was recorded", func() (bool, string) {
		v, ok := processedMarker(t, w, zone)
		s := estopStates(t, w, "r1", "r2")
		return ok && v == "" && allIn(s, fleetv1.RobotEstopStopped), fmt.Sprintf("marker %q present %v %v", v, ok, s)
	})
	eventually(t, "the zone reads Triggered", func() (bool, string) {
		var z fleetv1.FleetZone
		if err := w.admin.Get(context.Background(), client.ObjectKeyFromObject(zone), &z); err != nil {
			return false, err.Error()
		}
		return z.Status.EstopStatus == fleetv1.ZoneEstopTriggered, string(z.Status.EstopStatus)
	})
	assertPending(t, w, "for-r1", "r1")
	assertPending(t, w, "for-r2", "r2")
}

func TestEnvtest_AnEmptyNamespaceTriggerKeepsNewWorkPending(t *testing.T) {
	w, _ := presenceFixture(t)
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: controller.SwarmadaConfigName}}

	setTrigger(t, w, cfg, ptrTo(""))
	eventually(t, "the empty namespace trigger stopped every robot and was recorded", func() (bool, string) {
		v, ok := processedMarker(t, w, cfg)
		s := estopStates(t, w, "r1", "r2")
		return ok && v == "" && allIn(s, fleetv1.RobotEstopStopped), fmt.Sprintf("marker %q present %v %v", v, ok, s)
	})
	assertPending(t, w, "for-r1", "r1")
	assertPending(t, w, "for-r2", "r2")
}
