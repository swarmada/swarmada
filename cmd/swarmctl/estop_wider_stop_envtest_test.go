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

package main

import (
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A robot under its own stop and its zone's, through swarmctl and the real estop
// dispatcher: removing its own trigger is its own clear, and the zone's stop still holds
// it (RFC-0001 #safety-estop-scopes). It stays stopped, and `swarmctl estop resume`
// refuses it naming the zone. It is not held for ever: the zone's clear moves it to
// Resuming, and it leaves only on a resume, like every robot the zone's clear reaches.
func TestEnvtest_EstopOperatorPath_ARobotsOwnClearUnderAZoneStopWaitsForTheZone(t *testing.T) {
	p := startEstopPath(t)
	p.addRobot("amr-1")
	stopped := []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed}
	setOwn := func(val *string) {
		t.Helper()
		var r fleetv1.Robot
		if err := p.admin.Get(p.ctx, client.ObjectKey{Namespace: p.ns, Name: "amr-1"}, &r); err != nil {
			t.Fatal(err)
		}
		base := r.DeepCopy()
		if val == nil {
			delete(r.Annotations, annEstopTriggered)
		} else {
			r.Annotations[annEstopTriggered] = *val
		}
		if err := p.admin.Patch(p.ctx, &r, client.MergeFrom(base)); err != nil {
			t.Fatalf("writing amr-1's own trigger: %v", err)
		}
	}
	own := "r1-own"

	setOwn(&own)
	p.stateIs("amr-1", stopped...)
	p.swarmctl("estop", "trigger", "bay-1", "--reason", "person in the aisle", "--yes")

	// Its own clear: the zone's stop still holds it.
	setOwn(nil)
	time.Sleep(2 * time.Second)
	held := p.stateIs("amr-1", stopped...)
	p.report("amr-1")
	out, err := p.run("estop", "resume", "amr-1", "--reason", "inspected", "--yes", "--wait", "20s")
	if err == nil {
		t.Fatalf("swarmctl estop resume released a robot its zone's stop holds:\n%s", out)
	}
	if !strings.Contains(out, `still held by the estop on zone bay-1 (swarmada.io/estop-triggered="person in the aisle"); estopState `+string(held)) {
		t.Errorf("swarmctl estop resume does not name the zone's stop:\n%s", out)
	}

	// The zone's clear reaches it: Resuming, not Normal, until a resume.
	out = p.swarmctl("estop-clear", "bay-1", "--reason", "aisle clear", "--yes")
	if strings.Contains(out, "amr-1 is not released") {
		t.Errorf("swarmctl estop-clear says amr-1 is still held, with nothing else holding it:\n%s", out)
	}
	p.stateIs("amr-1", fleetv1.RobotEstopResuming)
	time.Sleep(time.Second)
	p.stateIs("amr-1", fleetv1.RobotEstopResuming)
	time.Sleep(5 * time.Millisecond)
	p.report("amr-1")
	out = p.swarmctl("estop", "resume", "amr-1", "--reason", "inspected", "--yes", "--wait", "20s")
	if !strings.Contains(out, "robot.swarmada.io/amr-1 released: estopState Normal") {
		t.Fatalf("swarmctl estop resume did not release amr-1 after the zone's clear:\n%s", out)
	}
}
