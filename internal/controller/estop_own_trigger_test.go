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
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A zone or namespace clear releases the robots that clear's stop held. A robot that also
// carries its own robot-scope trigger is held by that trigger too, and the wider clear is
// not its clear: removing the robot's own annotation is (RFC-0001 #safety-estop-scopes).
// The clear used to move every robot in scope to Resuming, and a resume then returned the
// robot to work with its own trigger still on it.

// ownStopRobot is a robot in zone that carries its own robot-scope trigger, already acted on.
func ownStopRobot(name, zone, trigger string) *fleetv1.Robot {
	r := zeRobot(name, zone)
	r.Annotations = map[string]string{annEstopTriggered: trigger, annEstopProcessed: trigger}
	r.Status.EstopState = fleetv1.RobotEstopStopped
	return r
}

// heldEvent returns the first recorded event that names the robot's own stop.
func heldEvent(rec *record.FakeRecorder) (string, bool) {
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "EstopHeldByOwnTrigger") {
				return e, true
			}
		default:
			return "", false
		}
	}
}

func TestZoneEstop_ClearLeavesARobotHeldByItsOwnStop(t *testing.T) {
	est := &fakeEstopper{}
	rec := record.NewFakeRecorder(16)
	zone := zeZone("bay-4", "", nil, "")
	zone.Annotations = map[string]string{annEstopProcessed: "zone-stop"} // the trigger was just removed
	emptyOwn := ownStopRobot("r-empty-own", "bay-4", "")
	r, _ := newZEReconciler(t, est, rec, zone,
		ownStopRobot("r-own", "bay-4", "r1-own"), emptyOwn, zeRobot("r-other", "bay-4"))

	reconcileZE(t, r, "bay-4")

	if got := est.clearedNames(); len(got) != 1 || got[0] != "r-other" {
		t.Fatalf("the zone clear cleared %v, want only [r-other]: a robot under its own trigger stays stopped", got)
	}
	e, ok := heldEvent(rec)
	if !ok {
		t.Fatal("no EstopHeldByOwnTrigger event for a robot the zone clear left stopped")
	}
	for _, want := range []string{"Warning", "zone bay-4", "still held by its own robot-scope estop", "Stopped"} {
		if !strings.Contains(e, want) {
			t.Errorf("event %q does not contain %q", e, want)
		}
	}
}

func TestNamespaceEstop_ClearLeavesARobotHeldByItsOwnStop(t *testing.T) {
	est := &fakeEstopper{}
	rec := record.NewFakeRecorder(16)
	r, _ := newNamespaceEstop(t, est, nsConfig("", "ns-stop"),
		ownStopRobot("r-own", "z1", "r1-own"), zeRobot("r-other", "z1"))
	r.Recorder = rec

	reconcileNsEstop(t, r)

	if got := est.clearedNames(); len(got) != 1 || got[0] != "r-other" {
		t.Fatalf("the namespace clear cleared %v, want only [r-other]: a robot under its own trigger stays stopped", got)
	}
	e, ok := heldEvent(rec)
	if !ok {
		t.Fatal("no EstopHeldByOwnTrigger event for a robot the namespace clear left stopped")
	}
	for _, want := range []string{"Warning", "namespace " + zeNS, "still held by its own robot-scope estop"} {
		if !strings.Contains(e, want) {
			t.Errorf("event %q does not contain %q", e, want)
		}
	}
}

// The robot's own clear still works once the wider stop is gone: it is the robot-scope
// clear that releases it.
func TestRobotEstop_OwnClearReleasesTheRobotAWiderClearLeft(t *testing.T) {
	est := &fakeEstopper{}
	robot := ownStopRobot("r-own", "bay-4", "r1-own")
	delete(robot.Annotations, annEstopTriggered) // the operator removes the robot's own trigger
	r, _ := newRobotEstop(t, est, robot)

	reconcileRobotEstop(t, r, "r-own")
	if got := est.clearedNames(); len(got) != 1 || got[0] != "r-own" {
		t.Fatalf("the robot's own clear cleared %v, want [r-own]", got)
	}
}
