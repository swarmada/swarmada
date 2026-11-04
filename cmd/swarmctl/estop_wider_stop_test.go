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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// A clear of one scope is not the clear of another (RFC-0001 #safety-estop-scopes): a
// robot a different zone's stop, or the namespace's stop, still holds is not released by
// a zone's clear, and cannot be released by `swarmctl estop resume` until that stop is
// cleared. The operator is told which stop holds it.

func stoppedZoneRobot(ns, name, zone string) *fleetv1.Robot {
	r := resumingRobotObj(ns, name, fleetv1.RobotEstopStopped)
	r.Spec.Zone = zone
	return r
}

func TestEstopClear_NamesTheRobotsAWiderStopStillHolds(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).WithObjects(
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-4", Namespace: ns,
			Annotations: map[string]string{annEstopTriggered: "parent-stop"}}},
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-4a", Namespace: ns,
			Annotations: map[string]string{annEstopTriggered: "child-stop"}},
			Spec: fleetv1.FleetZoneSpec{ParentZone: "bay-4"}},
		stoppedZoneRobot(ns, "r-child", "bay-4a"),
		stoppedZoneRobot(ns, "r-parent", "bay-4"),
	).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.estopClear(context.Background(), c, authorizer(true), ns, "bay-4", "aisle clear", true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	want := `robot.swarmada.io/r-child is not released by this clear: it is still held by the estop on zone bay-4a (swarmada.io/estop-triggered="child-stop")`
	if !strings.Contains(out.String(), want) {
		t.Errorf("estop-clear output does not contain %q:\n%s", want, out.String())
	}
	if strings.Contains(out.String(), "r-parent") {
		t.Errorf("estop-clear output names r-parent, which nothing else holds:\n%s", out.String())
	}
}

func TestEstopClear_NamesTheRobotsTheNamespaceStopStillHolds(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).WithObjects(
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-4", Namespace: ns,
			Annotations: map[string]string{annEstopTriggered: "zone-stop"}}},
		&fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: ns,
			Annotations: map[string]string{annEstopTriggered: ""}}},
		stoppedZoneRobot(ns, "r1", "bay-4"),
	).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.estopClear(context.Background(), c, authorizer(true), ns, "bay-4", "aisle clear", true); err != nil {
		t.Fatalf("clear: %v", err)
	}
	want := `robot.swarmada.io/r1 is not released by this clear: it is still held by the namespace estop (SwarmadaConfig swarmada-config, swarmada.io/estop-triggered="")`
	if !strings.Contains(out.String(), want) {
		t.Errorf("estop-clear output does not contain %q:\n%s", want, out.String())
	}
}

func TestEstopResume_ARobotAWiderStopHoldsIsToldWhich(t *testing.T) {
	ns := "warehouse-a"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).WithObjects(
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-4", Namespace: ns,
			Annotations: map[string]string{annEstopTriggered: "zone-stop"}}},
		stoppedZoneRobot(ns, "r1", "bay-4"),
	).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)
	if err := o.estopResume(context.Background(), c, authorizer(true), ns, []string{"r1"}, "inspected", true, time.Second); err == nil {
		t.Fatal("releasing a robot a zone's stop holds must fail")
	}
	for _, want := range []string{
		`still held by the estop on zone bay-4 (swarmada.io/estop-triggered="zone-stop"); estopState Stopped`,
		`swarmctl estop-clear bay-4 --reason "<text>" -n warehouse-a`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}
}
