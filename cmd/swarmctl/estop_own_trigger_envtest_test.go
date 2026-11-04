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

// A robot under its own robot-scope stop, then a zone stop and the zone's clear, through
// swarmctl and the real estop dispatcher: the zone clear releases the zone's other robot,
// not this one, and `swarmctl estop resume` does not release it either. Removing its own
// annotation is its clear (RFC-0001 #safety-estop-scopes); after that, a report and a
// resume return it to Normal.
func TestEnvtest_EstopOperatorPath_AZoneClearLeavesARobotItsOwnStop(t *testing.T) {
	p := startEstopPath(t)
	p.addRobot("amr-1")
	p.addRobot("amr-2")
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
	stopped := []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed}

	setOwn(&own)
	p.stateIs("amr-1", stopped...)
	p.swarmctl("estop", "trigger", "bay-1", "--reason", "person in the aisle", "--yes")
	p.stateIs("amr-2", stopped...)

	out := p.swarmctl("estop-clear", "bay-1", "--reason", "aisle clear", "--yes")
	if !strings.Contains(out, `robot.swarmada.io/amr-1 is not released by this clear: it is still held by its own robot-scope estop (swarmada.io/estop-triggered="r1-own")`) {
		t.Errorf("swarmctl estop-clear does not say amr-1's own stop still holds it:\n%s", out)
	}
	p.stateIs("amr-2", fleetv1.RobotEstopResuming)
	time.Sleep(2 * time.Second) // as long as amr-2's clear took, and more
	held := p.stateIs("amr-1", stopped...)

	time.Sleep(5 * time.Millisecond)
	p.report("amr-1")
	out, err := p.run("estop", "resume", "amr-1", "--reason", "inspected", "--yes", "--wait", "20s")
	if err == nil {
		t.Fatalf("swarmctl estop resume released a robot its own stop holds:\n%s", out)
	}
	if !strings.Contains(out, `still held by its own robot-scope estop (swarmada.io/estop-triggered="r1-own", estopState `+string(held)+`)`) {
		t.Errorf("swarmctl estop resume does not say amr-1's own stop holds it:\n%s", out)
	}
	p.stateIs("amr-1", stopped...)

	// Its own clear, a fresh report, then the release.
	setOwn(nil)
	p.stateIs("amr-1", fleetv1.RobotEstopResuming)
	time.Sleep(5 * time.Millisecond)
	p.report("amr-1")
	out = p.swarmctl("estop", "resume", "amr-1", "--reason", "inspected", "--yes", "--wait", "20s")
	if !strings.Contains(out, "robot.swarmada.io/amr-1 released: estopState Normal") {
		t.Fatalf("swarmctl estop resume did not release amr-1 after its own clear:\n%s", out)
	}
}
