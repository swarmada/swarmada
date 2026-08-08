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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func i32(v int32) *int32 { return &v }

func sampleRobots() []client.Object {
	return []client.Object{
		&fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Name: "sim-robot-001", Namespace: "warehouse-a"},
			Spec:       fleetv1.RobotSpec{Zone: "warehouse-a", RobotClass: "picker-v2", Adapter: fleetv1.AdapterRef{Name: "sim-adapter", Version: "1.0"}, Manufacturer: "SimBot", Model: "SimBot-250"},
			Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseIdle, BatteryPercent: i32(87)},
		},
		&fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Name: "sim-robot-003", Namespace: "warehouse-a"},
			Spec:       fleetv1.RobotSpec{Zone: "warehouse-a", Adapter: fleetv1.AdapterRef{Name: "sim-adapter"}},
			Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseCharging, BatteryPercent: i32(22)},
		},
		&fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Name: "sim-robot-005", Namespace: "warehouse-a"},
			Spec:       fleetv1.RobotSpec{Zone: "warehouse-b", Adapter: fleetv1.AdapterRef{Name: "sim-adapter"}},
			Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseOffline, AssignedAction: "restock-run-118"},
		},
	}
}

// newTestOptions returns an options wired to a buffer and a fixed output format,
// with color off (deterministic, no ANSI in assertions).
func newTestOptions(out *bytes.Buffer, format cli.OutputFormat) *options {
	return &options{
		streams:     cli.IOStreams{Out: out, Err: out},
		outputFmt:   format,
		colorStdout: false,
	}
}

func TestGetRobotsTable(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sampleRobots()...).Build()
	def, _ := resolveResource("rob") // short name resolves
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)
	if err := o.getList(context.Background(), c, def, "warehouse-a"); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	// Header order mirrors the Robot CRD's print columns (no wide -> no CLASS).
	header := firstLine(got)
	for _, col := range []string{"NAME", "PHASE", "ZONE", "BATTERY", "TASK", "AGE"} {
		if !strings.Contains(header, col) {
			t.Errorf("header missing %s: %q", col, header)
		}
	}
	if strings.Contains(header, "CLASS") {
		t.Errorf("CLASS is a wide-only column, should not appear: %q", header)
	}
	// Rows: names, an unassigned action renders <none>, offline battery em-dash.
	for _, want := range []string{"sim-robot-001", "87%", "Charging", "⚡ 22%", "Offline", "restock-run-118", cli.None, cli.Unknown} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}
}

func TestGetRobotsWideAddsClass(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sampleRobots()...).Build()
	def, _ := resolveResource("robots")
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputWide)
	if err := o.getList(context.Background(), c, def, "warehouse-a"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstLine(out.String()), "CLASS") {
		t.Errorf("wide output must include CLASS column: %q", firstLine(out.String()))
	}
	if !strings.Contains(out.String(), "picker-v2") {
		t.Errorf("wide output must show the robot class value:\n%s", out.String())
	}
}

func TestGetRobotsYAML(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sampleRobots()...).Build()
	def, _ := resolveResource("robot")
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputYAML)
	if err := o.getList(context.Background(), c, def, "warehouse-a"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "kind: RobotList") {
		t.Errorf("list YAML must carry the List GVK:\n%s", got)
	}
	if !strings.Contains(got, "name: sim-robot-001") {
		t.Errorf("list YAML must contain items:\n%s", got)
	}
}

func TestGetEmptyPrintsNotice(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	def, _ := resolveResource("robots")
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)
	if err := o.getList(context.Background(), c, def, "empty-ns"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No resources found in empty-ns namespace.") {
		t.Errorf("expected kubectl-style empty notice, got: %q", out.String())
	}
}

func TestDescribeRobot(t *testing.T) {
	r := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sim-robot-004", Namespace: "warehouse-b",
			Labels: map[string]string{"swarmada.io/model": "SimBot-250"},
		},
		Spec: fleetv1.RobotSpec{Manufacturer: "SimBot", Model: "SimBot-250", Zone: "warehouse-b", Adapter: fleetv1.AdapterRef{Name: "sim-adapter", Version: "1.0"}},
		Status: fleetv1.RobotStatus{
			Phase:          fleetv1.RobotPhaseError,
			BatteryPercent: i32(45),
			Position:       &fleetv1.RobotPosition{X: 8.2, Y: 15.6, Yaw: -0.42},
			Hardware: []fleetv1.HardwareComponentStatus{
				{Name: "lidar-front", Status: fleetv1.HardwareHealthy},
				{Name: "drive-motor-r", Status: fleetv1.HardwareDegraded},
			},
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "HeartbeatOK", Message: "Robot is responsive."},
			},
		},
	}
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)
	o.describeRobot(r)
	got := out.String()

	for _, want := range []string{
		"Name:         sim-robot-004",
		"Namespace:    warehouse-b",
		"swarmada.io/model=SimBot-250",
		"Kind:         Robot",
		"Adapter:      sim-adapter (v1.0)",
		"Phase:",
		"Battery:",
		"[", "]", // battery bar brackets
		"Position (coarse, display-only",
		"X: 8.20", "Y: 15.60", "Yaw: -0.42",
		"Hardware:",
		"lidar-front", "drive-motor-r",
		"Conditions:",
		"Ready", "HeartbeatOK",
		"Events:  <none>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("describe robot missing %q:\n%s", want, got)
		}
	}
}

func TestDescribeGenericFleetAction(t *testing.T) {
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "pick-run-4471", Namespace: "warehouse-a"},
		Spec:       fleetv1.FleetActionSpec{Type: "pick", Zone: "warehouse-a", Priority: "Normal"},
		Status:     fleetv1.FleetActionStatus{Phase: "InProgress", AssignedRobot: "sim-robot-002"},
	}
	def, _ := resolveResource("ft")
	var out bytes.Buffer
	o := newTestOptions(&out, cli.OutputTable)
	o.describeGeneric(def, action)
	got := out.String()
	for _, want := range []string{"Name:         pick-run-4471", "Kind:         FleetAction", "Assigned Robot", "sim-robot-002", "Events:  <none>"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe fleetaction missing %q:\n%s", want, got)
		}
	}
}

func TestRegistryCoversTwelveCRDs(t *testing.T) {
	if len(resourceOrder) != 12 {
		t.Fatalf("expected 12 registered resources, got %d", len(resourceOrder))
	}
	// Every short name resolves back to its kind.
	shorts := map[string]string{
		"rob": "Robot", "ft": "FleetAction", "fz": "FleetZone", "rc": "RobotClass",
		"dr": "DiscoveredRobot", "fa": "FleetAdapter", "rp": "RobotProbe",
		"fwr": "FirmwareRollout", "mr": "ModelRollout", "mp": "ModelPolicy",
		"sc": "SwarmadaConfig", "zm": "ZoneMaintenance",
	}
	for short, kind := range shorts {
		def, err := resolveResource(short)
		if err != nil || def.kind != kind {
			t.Errorf("short %q => %v (err %v), want kind %s", short, def, err, kind)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
