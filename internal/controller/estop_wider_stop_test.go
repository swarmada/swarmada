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

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A clear of one scope is not the clear of another (RFC-0001 #safety-estop-scopes). A
// robot that a zone's stop or its namespace's stop still holds stays stopped when its own
// robot-scope trigger is removed, or when a different zone is cleared; the clear of the
// stop that holds it is what releases it. Each clear used to move every robot it reached
// to Resuming whatever else held it, and estopState then read Resuming, or Normal after a
// resume, while a stop was in force on the robot.

// clearedRobot is a robot stopped under its own trigger, whose trigger was just removed.
func clearedRobot(name, zone string) *fleetv1.Robot {
	r := zeRobot(name, zone)
	r.Annotations = map[string]string{annEstopProcessed: "r-own"}
	r.Status.EstopState = fleetv1.RobotEstopStopped
	return r
}

// stoppedRobot is a robot a wider stop stopped.
func stoppedRobot(name, zone string) *fleetv1.Robot {
	r := zeRobot(name, zone)
	r.Status.EstopState = fleetv1.RobotEstopStopped
	return r
}

// widerEvent returns the first recorded EstopHeldByWiderStop event.
func widerEvent(rec *record.FakeRecorder) (string, bool) {
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "EstopHeldByWiderStop") {
				return e, true
			}
		default:
			return "", false
		}
	}
}

func wantWiderEvent(t *testing.T, rec *record.FakeRecorder, wants ...string) {
	t.Helper()
	e, ok := widerEvent(rec)
	if !ok {
		t.Fatal("no EstopHeldByWiderStop event for a robot the clear left stopped")
	}
	for _, want := range append([]string{"Warning", "still held by"}, wants...) {
		if !strings.Contains(e, want) {
			t.Errorf("event %q does not contain %q", e, want)
		}
	}
}

func TestRobotEstop_OwnClearLeavesARobotItsZoneStillStops(t *testing.T) {
	est := &fakeEstopper{}
	rec := record.NewFakeRecorder(16)
	r, c := newRobotEstop(t, est, clearedRobot("r1", "bay-4a"),
		zeZone("bay-4", "", nil, "zone-stop"), zeZone("bay-4a", "bay-4", nil, ""))
	r.Recorder = rec

	reconcileRobotEstop(t, r, "r1")

	if got := est.clearedNames(); len(got) != 0 {
		t.Fatalf("the robot's own clear cleared %v while zone bay-4's stop holds it, want none", got)
	}
	wantWiderEvent(t, rec, "robot estop cleared", "the estop on zone bay-4")
	got := &fleetv1.Robot{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: zeNS, Name: "r1"}, got); err != nil {
		t.Fatal(err)
	}
	if _, still := got.Annotations[annEstopProcessed]; still {
		t.Error("the robot's own processed marker was not dropped: its own stop is cleared even though another holds it")
	}
}

func TestRobotEstop_OwnClearLeavesARobotItsNamespaceStillStops(t *testing.T) {
	est := &fakeEstopper{}
	rec := record.NewFakeRecorder(16)
	r, _ := newRobotEstop(t, est, clearedRobot("r1", "z1"), nsConfig("ns-stop", "ns-stop"))
	r.Recorder = rec

	reconcileRobotEstop(t, r, "r1")

	if got := est.clearedNames(); len(got) != 0 {
		t.Fatalf("the robot's own clear cleared %v while the namespace stop holds it, want none", got)
	}
	wantWiderEvent(t, rec, "the namespace estop")
}

func TestRobotEstop_OwnClearStillReleasesARobotNothingElseHolds(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newRobotEstop(t, est, clearedRobot("r1", "bay-9"),
		zeZone("bay-4", "", nil, "zone-stop"), zeZone("bay-9", "", nil, ""), nsConfig("", ""))

	reconcileRobotEstop(t, r, "r1")
	if got := est.clearedNames(); len(got) != 1 || got[0] != "r1" {
		t.Fatalf("the robot's own clear cleared %v, want [r1]: no other stop holds it", got)
	}
}

func TestZoneEstop_ClearLeavesARobotAnotherZoneStillStops(t *testing.T) {
	est := &fakeEstopper{}
	rec := record.NewFakeRecorder(16)
	parent := zeZone("bay-4", "", nil, "")
	parent.Annotations = map[string]string{annEstopProcessed: "parent-stop"} // its trigger was just removed
	r, _ := newZEReconciler(t, est, rec, parent, zeZone("bay-4a", "bay-4", nil, "child-stop"),
		stoppedRobot("r-child", "bay-4a"), stoppedRobot("r-parent", "bay-4"))

	reconcileZE(t, r, "bay-4")

	if got := est.clearedNames(); len(got) != 1 || got[0] != "r-parent" {
		t.Fatalf("the zone clear cleared %v, want only [r-parent]: zone bay-4a's stop still holds r-child", got)
	}
	wantWiderEvent(t, rec, "zone bay-4 estop cleared", "the estop on zone bay-4a")
}

func TestZoneEstop_ClearLeavesARobotTheNamespaceStillStops(t *testing.T) {
	est := &fakeEstopper{}
	rec := record.NewFakeRecorder(16)
	zone := zeZone("bay-4", "", nil, "")
	zone.Annotations = map[string]string{annEstopProcessed: "zone-stop"}
	r, _ := newZEReconciler(t, est, rec, zone, stoppedRobot("r1", "bay-4"))
	if err := r.Create(t.Context(), nsConfig("ns-stop", "ns-stop")); err != nil {
		t.Fatal(err)
	}

	reconcileZE(t, r, "bay-4")

	if got := est.clearedNames(); len(got) != 0 {
		t.Fatalf("the zone clear cleared %v while the namespace stop holds them, want none", got)
	}
	wantWiderEvent(t, rec, "the namespace estop")
}

func TestNamespaceEstop_ClearLeavesARobotAZoneStillStops(t *testing.T) {
	est := &fakeEstopper{}
	rec := record.NewFakeRecorder(16)
	r, _ := newNamespaceEstop(t, est, nsConfig("", "ns-stop"),
		zeZone("bay-4", "", nil, "zone-stop"), stoppedRobot("r-zone", "bay-4"), stoppedRobot("r-free", "bay-9"))
	r.Recorder = rec

	reconcileNsEstop(t, r)

	if got := est.clearedNames(); len(got) != 1 || got[0] != "r-free" {
		t.Fatalf("the namespace clear cleared %v, want only [r-free]: zone bay-4's stop still holds r-zone", got)
	}
	wantWiderEvent(t, rec, "namespace "+zeNS+" estop cleared", "the estop on zone bay-4")
}

// A release request for a robot another stop still holds is refused, and says which.
func TestRobotEstopResume_RefusedWhileAWiderStopHolds(t *testing.T) {
	res := &fakeResumer{reply: fleetv1.RobotEstopNormal}
	r, rec, get := resumingRobot(t, res, "req-1")
	robot := get()
	robot.Status.CurrentZone = "bay-4"
	if err := r.Status().Update(t.Context(), robot); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(t.Context(), zeZone("bay-4", "", nil, "zone-stop")); err != nil {
		t.Fatal(err)
	}

	reconcileResume(t, r)

	if len(res.attempts) != 0 {
		t.Fatalf("the release was relayed to the dispatcher %d time(s) while zone bay-4's stop holds the robot", len(res.attempts))
	}
	var refused string
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.Contains(e, "EstopResumeRefused") {
			refused = e
		}
	}
	if !strings.Contains(refused, "still held by the estop on zone bay-4") {
		t.Fatalf("refusal event = %q, want it to name zone bay-4's stop", refused)
	}
}

// A child zone's clear is skipped for a robot only while a parent's stop reaches it: when
// the parent's estopPolicy propagates to children (the default). A parent that does not
// propagate never stopped the child's robots, so the child's clear releases them.
func TestZoneEstop_AChildClearIsHeldOnlyByAParentStopThatPropagates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		propagate *bool
		wantClear bool
	}{
		{name: "propagateToChildren default (true)", propagate: nil, wantClear: false},
		{name: "propagateToChildren true", propagate: apiBool(true), wantClear: false},
		{name: "propagateToChildren false", propagate: apiBool(false), wantClear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			est := &fakeEstopper{}
			rec := record.NewFakeRecorder(16)
			var policy *fleetv1.ZoneEstopPolicy
			if tc.propagate != nil {
				policy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: tc.propagate}
			}
			child := zeZone("bay-4a", "bay-4", nil, "")
			child.Annotations = map[string]string{annEstopProcessed: "child-stop"} // its trigger was just removed
			r, _ := newZEReconciler(t, est, rec, zeZone("bay-4", "", policy, "parent-stop"), child,
				stoppedRobot("r-child", "bay-4a"))

			reconcileZE(t, r, "bay-4a")

			got := est.clearedNames()
			if tc.wantClear && (len(got) != 1 || got[0] != "r-child") {
				t.Fatalf("the child zone's clear cleared %v, want [r-child]: the parent's stop does not propagate", got)
			}
			if !tc.wantClear {
				if len(got) != 0 {
					t.Fatalf("the child zone's clear cleared %v while the parent's propagating stop holds r-child", got)
				}
				wantWiderEvent(t, rec, "zone bay-4a estop cleared", "the estop on zone bay-4")
			}
		})
	}
}
