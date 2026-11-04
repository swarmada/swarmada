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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
)

// The same robot_id on a Robot in each of two namespaces. The control plane does not
// enforce robot_id uniqueness across namespaces (RFC-0001
// #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3), so these are two
// robots, and telemetry for each must reach its own object and no other.
//
// The shared id is unique to this file: a frame with no namespace falls back to a
// cluster-wide search, and envtest namespaces are never garbage-collected.

// twoNamespaceRobots creates a Robot carrying robotID in each of two fresh
// namespaces, declared into zoneName, and returns the namespaces.
func twoNamespaceRobots(t *testing.T, robotID, nameA, nameB, zoneA, zoneB string) (nsA, nsB string) {
	t.Helper()
	nsA, nsB = envtestNamespace(t), envtestNamespace(t)
	for _, r := range []struct{ ns, name, zone string }{{nsA, nameA, zoneA}, {nsB, nameB, zoneB}} {
		robot := envtestValidRobot(r.ns, r.name, r.zone)
		robot.Annotations[fleetv1.RobotIDAnnotation] = robotID
		if err := envK8s.Create(context.Background(), robot); err != nil {
			t.Fatalf("create robot %s/%s: %v", r.ns, r.name, err)
		}
	}
	return nsA, nsB
}

func etRobot(t *testing.T, ns, name string) *fleetv1.Robot {
	t.Helper()
	var r fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &r); err != nil {
		t.Fatalf("get robot %s/%s: %v", ns, name, err)
	}
	return &r
}

// Both robots' status is projected: battery and hardware land on the right object
// in the right namespace, and neither frame is refused. The frames go through the
// production Router, whose "" plane is where a bare wire robot_id is routed, so both
// robots share one plane as they do in a running manager.
func TestEnvtest_OneRobotIDInTwoNamespaces_BothRobotsProjectStatus(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	const robotID = "twons-status-chassis"
	nsA, nsB := twoNamespaceRobots(t, robotID, "twons-status-a", "twons-status-b", "dock", "dock")

	router := telemetry.NewRouter(
		func(string) telemetry.PlaneConfig { return telemetry.PlaneConfig{} },
		&RobotStatusSink{Client: envK8s},
	)
	frames := []telemetry.Frame{
		{RobotID: robotID, Namespace: nsA, BatteryPct: i32(80),
			Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareHealthy}},
		{RobotID: robotID, Namespace: nsB, BatteryPct: i32(20),
			Hardware: map[string]fleetv1.HardwareStatus{"lidar": fleetv1.HardwareDegraded}},
	}
	for _, f := range frames {
		if err := router.Ingest(ctx, f); err != nil {
			t.Fatalf("telemetry for %s/%s refused: %v", f.Namespace, f.RobotID, err)
		}
	}

	for _, want := range []struct {
		ns, name string
		battery  int32
		lidar    fleetv1.HardwareStatus
	}{
		{nsA, "twons-status-a", 80, fleetv1.HardwareHealthy},
		{nsB, "twons-status-b", 20, fleetv1.HardwareDegraded},
	} {
		r := etRobot(t, want.ns, want.name)
		if r.Status.BatteryPercent == nil || *r.Status.BatteryPercent != want.battery {
			t.Fatalf("%s/%s batteryPercent = %v, want %d", want.ns, want.name, r.Status.BatteryPercent, want.battery)
		}
		if len(r.Status.Hardware) != 1 || r.Status.Hardware[0].Name != "lidar" || r.Status.Hardware[0].Status != want.lidar {
			t.Fatalf("%s/%s hardware = %+v, want lidar %s", want.ns, want.name, r.Status.Hardware, want.lidar)
		}
	}
}

// The zone path: each robot's currentZone is derived from its own namespace's
// zones. Both namespaces have a zone with the same polygon under different names, so
// a lookup that crossed namespaces would show up as the wrong name. Interleaved
// identical frames then write nothing to either robot (RA-1).
func TestEnvtest_OneRobotIDInTwoNamespaces_ZoneFromOwnNamespace(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	const robotID = "twons-zone-chassis"
	nsA, nsB := twoNamespaceRobots(t, robotID, "twons-zone-a", "twons-zone-b", "twons-bay-a", "twons-bay-b")

	zc := &ZoneController{Client: envK8s, Scheme: envScheme, Recorder: record.NewFakeRecorder(64)}
	for _, zz := range []struct{ ns, name string }{{nsA, "twons-bay-a"}, {nsB, "twons-bay-b"}} {
		fz := &fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Name: zz.name, Namespace: zz.ns},
			Spec: fleetv1.FleetZoneSpec{PhysicalBounds: &fleetv1.PhysicalBounds{
				Floor:   0,
				Polygon: []fleetv1.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}},
				Frame:   &fleetv1.CoordinateFrame{ID: "site", Alignment: fleetv1.FrameSiteAligned},
			}},
		}
		if err := envK8s.Create(ctx, fz); err != nil {
			t.Fatalf("create zone %s/%s: %v", zz.ns, zz.name, err)
		}
		if _, err := zc.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: zz.ns, Name: zz.name},
		}); err != nil {
			t.Fatalf("reconcile zone %s/%s: %v", zz.ns, zz.name, err)
		}
	}

	frameA := telemetry.Frame{RobotID: robotID, Namespace: nsA, Position: &telemetry.Position{X: 5, Y: 5}}
	frameB := telemetry.Frame{RobotID: robotID, Namespace: nsB, Position: &telemetry.Position{X: 5, Y: 5}}
	zc.ObservePosition(ctx, frameA)
	if got := etRobot(t, nsB, "twons-zone-b").Status.CurrentZone; got != "" {
		t.Fatalf("a frame for the robot in %s moved the robot in %s to %q", nsA, nsB, got)
	}
	zc.ObservePosition(ctx, frameB)

	a, b := etRobot(t, nsA, "twons-zone-a"), etRobot(t, nsB, "twons-zone-b")
	if a.Status.CurrentZone != "twons-bay-a" {
		t.Fatalf("%s robot currentZone = %q, want twons-bay-a (its own namespace's zone)", nsA, a.Status.CurrentZone)
	}
	if b.Status.CurrentZone != "twons-bay-b" {
		t.Fatalf("%s robot currentZone = %q, want twons-bay-b (its own namespace's zone)", nsB, b.Status.CurrentZone)
	}

	rvA, rvB := a.ResourceVersion, b.ResourceVersion
	for i := 0; i < 5; i++ {
		zc.ObservePosition(ctx, frameA)
		zc.ObservePosition(ctx, frameB)
	}
	if got := etRobot(t, nsA, "twons-zone-a").ResourceVersion; got != rvA {
		t.Fatalf("identical frames rewrote the %s robot (rv %s -> %s): RA-1", nsA, rvA, got)
	}
	if got := etRobot(t, nsB, "twons-zone-b").ResourceVersion; got != rvB {
		t.Fatalf("identical frames rewrote the %s robot (rv %s -> %s): RA-1", nsB, rvB, got)
	}
}
