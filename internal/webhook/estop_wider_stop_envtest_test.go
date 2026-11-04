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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
)

// A clear of one scope is not the clear of another (RFC-0001 #safety-estop-scopes). A
// robot under its own stop and a zone's, whose own trigger is removed, stays stopped and
// gets no work until the zone is cleared; a zone cleared under a namespace stop, or a
// namespace cleared under a zone stop, leaves the robots the other stop holds stopped.

func robotEvent(t *testing.T, w *safetyWire, robot, reason, contains string) func() (bool, string) {
	return func() (bool, string) {
		var evs corev1.EventList
		if err := w.admin.List(context.Background(), &evs, client.InNamespace(w.ns)); err != nil {
			return false, err.Error()
		}
		var seen []string
		for _, e := range evs.Items {
			if e.InvolvedObject.Name == robot && e.Reason == reason && strings.Contains(e.Message, contains) {
				return true, ""
			}
			seen = append(seen, e.InvolvedObject.Name+" "+e.Reason+": "+e.Message)
		}
		return false, strings.Join(seen, " | ")
	}
}

func stays(t *testing.T, w *safetyWire, what string, want fleetv1.RobotEstopState, names ...string) {
	t.Helper()
	consistently(t, what, 2*time.Second, func() (bool, string) {
		s := estopStates(t, w, names...)
		return allIn(s, want), fmt.Sprint(s)
	})
}

func reaches(t *testing.T, w *safetyWire, what string, want fleetv1.RobotEstopState, names ...string) {
	t.Helper()
	eventually(t, what, func() (bool, string) {
		s := estopStates(t, w, names...)
		return allIn(s, want), fmt.Sprint(s)
	})
}

func TestEnvtest_ARobotsOwnClearLeavesItTheZoneStop(t *testing.T) {
	w, _ := presenceFixture(t)
	r1 := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "r1"}}
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: markerRobotZone}}

	setTrigger(t, w, r1, ptrTo("r1-own"))
	eventually(t, "r1's own stop", func() (bool, string) {
		v, _ := processedMarker(t, w, r1)
		return v == "r1-own", v
	})
	setTrigger(t, w, zone, ptrTo("zone-stop"))
	reaches(t, w, "the zone stop", fleetv1.RobotEstopStopped, "r1", "r2")
	eventually(t, "the zone stop was recorded", func() (bool, string) {
		v, _ := processedMarker(t, w, zone)
		return v == "zone-stop", v
	})

	// r1's own clear: its marker goes, but the zone's stop still holds it.
	setTrigger(t, w, r1, nil)
	eventually(t, "r1's own clear was applied", func() (bool, string) {
		_, marked := processedMarker(t, w, r1)
		return !marked, fmt.Sprint(marked)
	})
	stays(t, w, "r1 stays Stopped under the zone's stop", fleetv1.RobotEstopStopped, "r1")
	if got := pinnedDispatch(t, w, "for-r1", "r1"); got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to r1 while the zone's stop holds it")
	}
	eventually(t, "r1's event names the zone's stop",
		robotEvent(t, w, "r1", "EstopHeldByWiderStop", "the estop on zone "+markerRobotZone))

	// The zone's clear releases both.
	setTrigger(t, w, zone, nil)
	reaches(t, w, "the zone's clear released both robots", fleetv1.RobotEstopResuming, "r1", "r2")
}

func TestEnvtest_AZoneClearUnderANamespaceStopLeavesTheRobotsStopped(t *testing.T) {
	w, _ := presenceFixture(t)
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: markerRobotZone}}
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: controller.SwarmadaConfigName}}

	setTrigger(t, w, cfg, ptrTo("ns-stop"))
	setTrigger(t, w, zone, ptrTo("zone-stop"))
	reaches(t, w, "both stops", fleetv1.RobotEstopStopped, "r1", "r2")
	eventually(t, "both stops were recorded", func() (bool, string) {
		zv, _ := processedMarker(t, w, zone)
		nv, _ := processedMarker(t, w, cfg)
		return zv == "zone-stop" && nv == "ns-stop", zv + " " + nv
	})

	setTrigger(t, w, zone, nil)
	eventually(t, "the zone's clear was applied", func() (bool, string) {
		_, marked := processedMarker(t, w, zone)
		return !marked, fmt.Sprint(marked)
	})
	stays(t, w, "the robots stay Stopped under the namespace stop", fleetv1.RobotEstopStopped, "r1", "r2")
	eventually(t, "r1's event names the namespace stop",
		robotEvent(t, w, "r1", "EstopHeldByWiderStop", "the namespace estop"))

	setTrigger(t, w, cfg, nil)
	reaches(t, w, "the namespace's clear released the robots", fleetv1.RobotEstopResuming, "r1", "r2")
}

func TestEnvtest_ANamespaceClearUnderAZoneStopLeavesTheRobotsStopped(t *testing.T) {
	w, _ := presenceFixture(t)
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: markerRobotZone}}
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: controller.SwarmadaConfigName}}

	setTrigger(t, w, zone, ptrTo("zone-stop"))
	setTrigger(t, w, cfg, ptrTo("ns-stop"))
	reaches(t, w, "both stops", fleetv1.RobotEstopStopped, "r1", "r2")
	eventually(t, "both stops were recorded", func() (bool, string) {
		zv, _ := processedMarker(t, w, zone)
		nv, _ := processedMarker(t, w, cfg)
		return zv == "zone-stop" && nv == "ns-stop", zv + " " + nv
	})

	setTrigger(t, w, cfg, nil)
	eventually(t, "the namespace's clear was applied", func() (bool, string) {
		_, marked := processedMarker(t, w, cfg)
		return !marked, fmt.Sprint(marked)
	})
	stays(t, w, "the robots stay Stopped under the zone's stop", fleetv1.RobotEstopStopped, "r1", "r2")

	setTrigger(t, w, zone, nil)
	reaches(t, w, "the zone's clear released the robots", fleetv1.RobotEstopResuming, "r1", "r2")
}
