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
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// A robot that carries its own robot-scope trigger is held by it until that annotation is
// removed; a zone clear does not release it (RFC-0001 #safety-estop-scopes). The operator
// is told so where they act: by `swarmctl estop-clear` on the zone, and by `swarmctl estop
// resume` on the robot.

func ownTriggerRobot(ns, name, zone, trigger string, state fleetv1.RobotEstopState) *fleetv1.Robot {
	r := resumingRobotObj(ns, name, state)
	r.Spec.Zone = zone
	r.Annotations = map[string]string{annEstopTriggered: trigger}
	return r
}

func TestEstopClear_NamesTheRobotsItsOwnStopStillHolds(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).WithObjects(
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-4", Namespace: ns,
			Annotations: map[string]string{annEstopTriggered: "zone-stop"}}},
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-4a", Namespace: ns},
			Spec: fleetv1.FleetZoneSpec{ParentZone: "bay-4"}},
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-9", Namespace: ns}},
		ownTriggerRobot(ns, "r-own", "bay-4", "r1-own", fleetv1.RobotEstopStopped),
		ownTriggerRobot(ns, "r-child-own", "bay-4a", "", fleetv1.RobotEstopStopped),
		ownTriggerRobot(ns, "r-elsewhere", "bay-9", "x", fleetv1.RobotEstopStopped),
		resumingRobotObj(ns, "r-other", fleetv1.RobotEstopStopped),
	).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.estopClear(context.Background(), c, authorizer(true), ns, "bay-4", "aisle clear", true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		`robot.swarmada.io/r-own is not released by this clear: it is still held by its own robot-scope estop (swarmada.io/estop-triggered="r1-own")`,
		`robot.swarmada.io/r-child-own is not released by this clear: it is still held by its own robot-scope estop (swarmada.io/estop-triggered="")`,
		"kubectl annotate robot r-own swarmada.io/estop-triggered- -n warehouse-a",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("estop-clear output does not contain %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"r-elsewhere", "r-other"} {
		if strings.Contains(got, not) {
			t.Errorf("estop-clear output names %s, which is outside the zone or has no stop of its own:\n%s", not, got)
		}
	}
}

func TestEstopResume_ARobotHeldByItsOwnStopIsToldSo(t *testing.T) {
	for _, state := range []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopResuming} {
		t.Run(string(state), func(t *testing.T) {
			ns := "warehouse-a"
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).
				WithObjects(ownTriggerRobot(ns, "r-own", "bay-4", "r1-own", state)).Build()
			out := &bytes.Buffer{}
			o := newTestOptions(out, cli.OutputTable)
			if err := o.estopResume(context.Background(), c, authorizer(true), ns, []string{"r-own"}, "inspected", true, time.Second); err == nil {
				t.Fatal("releasing a robot held by its own stop must fail")
			}
			for _, want := range []string{
				`still held by its own robot-scope estop (swarmada.io/estop-triggered="r1-own", estopState ` + string(state) + `)`,
				"kubectl annotate robot r-own swarmada.io/estop-triggered- -n warehouse-a",
			} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output does not contain %q:\n%s", want, out.String())
				}
			}
			got := &fleetv1.Robot{}
			_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "r-own"}, got)
			if _, written := got.Annotations[fleetv1.EstopResumeAnnotation]; written {
				t.Fatal("a release request was written for a robot its own stop holds")
			}
		})
	}
}
