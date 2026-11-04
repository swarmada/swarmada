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
	"github.com/swarmada/swarmada/internal/estopscope"
)

// Right after a zone's clear, the zone still carries the processed marker until the control
// plane applies the clear, and the robot's record of that stop holds it until then
// (RFC-0001 #safety-estop-scopes). A release asked for in that window is refused, and the
// refusal says the clear is being applied and to retry the release: it must not send the
// operator to clear the zone again, which is already done.
func TestEstopResume_RightAfterAZoneClearSaysToRetry(t *testing.T) {
	ns := "warehouse-a"
	r := stoppedZoneRobot(ns, "r1", "bay-4")
	r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "bay-4"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&fleetv1.Robot{}).WithObjects(
		&fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay-4", Namespace: ns,
			Annotations: map[string]string{estopscope.ProcessedAnnotation: "zone-stop"}}},
		r,
	).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)
	if err := o.estopResume(context.Background(), c, authorizer(true), ns, []string{"r1"}, "inspected", true, time.Second); err == nil {
		t.Fatal("releasing a robot whose zone's clear is not applied yet must fail")
	}
	for _, want := range []string{
		`still held by the estop on zone bay-4, whose clear is being applied`,
		`retry the release once that clear is applied: swarmctl estop resume r1 --reason "<text>" -n warehouse-a`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}
	for _, not := range []string{"swarmctl estop-clear", `estop-triggered=""`} {
		if strings.Contains(out.String(), not) {
			t.Errorf("output must not contain %q:\n%s", not, out.String())
		}
	}
}
