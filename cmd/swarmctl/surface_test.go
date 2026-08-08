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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

// estop-clear requires a non-empty --reason, enforced in the core before the
// SSAR gate or any write.
func TestEstopClearRequiresReason(t *testing.T) {
	ns := "warehouse-a"
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: ns, Annotations: map[string]string{annEstopTriggered: "boom"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(zone).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.estopClear(context.Background(), c, authorizer(true), ns, "z", "   ", true); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("expected a required-reason error, got %v", err)
	}
	got := &fleetv1.FleetZone{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "z"}, got)
	if got.Annotations[annEstopTriggered] != "boom" {
		t.Error("a reason-less estop-clear must not resume the zone")
	}
}

// delete robot on an admitted Robot is a plain delete.
func TestDeleteAdmittedRobot(t *testing.T) {
	ns := "warehouse-a"
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "amr-007", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(robot).Build()
	out := &bytes.Buffer{}
	o := newTestOptions(out, cli.OutputTable)

	if err := o.deleteAdmittedRobot(context.Background(), c, ns, "amr-007", true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "amr-007"}, &fleetv1.Robot{}); !apierrors.IsNotFound(err) {
		t.Errorf("robot should be deleted, got err=%v", err)
	}
	if !strings.Contains(out.String(), "deleted") {
		t.Errorf("missing confirmation output: %q", out.String())
	}
}

// A declined confirmation leaves the robot in place.
func TestDeleteAdmittedRobotAborts(t *testing.T) {
	ns := "warehouse-a"
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "keep", Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(robot).Build()
	out := &bytes.Buffer{}
	o := &options{streams: cli.IOStreams{In: strings.NewReader("n\n"), Out: out, Err: out}, outputFmt: cli.OutputTable}

	if err := o.deleteAdmittedRobot(context.Background(), c, ns, "keep", false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "keep"}, &fleetv1.Robot{}); err != nil {
		t.Errorf("a declined delete must not remove the robot: %v", err)
	}
}

// --discovered swaps `get robot` to DiscoveredRobots and is rejected when a name
// is given or the resource is not robot.
func TestDiscoveredTargetSwapsAndValidates(t *testing.T) {
	robotDef, _ := resolveResource("robot")
	got, err := discoveredTarget(robotDef, nil)
	if err != nil || got.singular != "discoveredrobot" {
		t.Fatalf("robot --discovered should target discoveredrobot, got %v err=%v", got, err)
	}
	if _, err := discoveredTarget(robotDef, []string{"amr-1"}); err == nil {
		t.Error("--discovered with a named robot must error")
	}
	actionDef, _ := resolveResource("fleetaction")
	if _, err := discoveredTarget(actionDef, nil); err == nil {
		t.Error("--discovered on a non-robot resource must error")
	}
}

// export audit re-serializes a chain that get/verify audit can consume.
func TestExportAuditRoundTrips(t *testing.T) {
	entries := exportedChain(t)
	var buf bytes.Buffer
	o := newTestOptions(&buf, cli.OutputTable)
	if err := o.exportAuditChain(entries, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	ro := auditOptions(buf.String(), &bytes.Buffer{}, cli.OutputTable)
	got, err := ro.readAuditChain("")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("round-trip entry count = %d, want %d", len(got), len(entries))
	}
	if err := ro.verifyAuditChain(got); err != nil {
		t.Errorf("round-tripped chain must still verify: %v", err)
	}
}
