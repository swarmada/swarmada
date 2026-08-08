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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func aaDiscovered(name, suggestedClass string) *fleetv1.DiscoveredRobot {
	return &fleetv1.DiscoveredRobot{
		ObjectMeta: metav1.ObjectMeta{Namespace: drNS, Name: name},
		Status: fleetv1.DiscoveredRobotStatus{
			Phase:               fleetv1.DiscoveredRobotPhaseDiscovered,
			ConnectedAt:         metav1.Time{Time: drBase},
			TTLExpiresAt:        &metav1.Time{Time: drBase.Add(30 * time.Minute)},
			SuggestedRobotClass: suggestedClass,
			Manufacturer:        "Acme",
			Model:               "X1",
			AdapterVersion:      "1.0",
		},
	}
}

func aaConfig(class, zone string) *fleetv1.SwarmadaConfig {
	return configWithSpec(drNS, fleetv1.SwarmadaConfigSpec{
		Provisioning: fleetv1.SwarmadaProvisioningConfig{AutoAdmitRobotClass: class, AutoAdmitZone: zone},
	})
}

func aaClass(name string) *fleetv1.RobotClass {
	return &fleetv1.RobotClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: drNS, Name: name},
		Spec: fleetv1.RobotClassSpec{
			Manufacturer: "Acme", Model: "X1",
			BaseAdapter: fleetv1.BaseAdapterRef{Name: "acme-adapter"},
		},
	}
}

func aaZone(name string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: drNS, Name: name}}
}

func getRobotOrNil(t *testing.T, r *DiscoveredRobotReconciler, name string) *fleetv1.Robot {
	t.Helper()
	robot := &fleetv1.Robot{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: drNS, Name: name}, robot)
	if err != nil {
		return nil
	}
	return robot
}

// A suggested-class match with a configured zone auto-admits: the Robot is created
// (from the class + zone) and the DiscoveredRobot is removed.
func TestAutoAdmit_MatchCreatesRobotAndDeletesDR(t *testing.T) {
	now := drBase
	r, c := newDRReconciler(t, &now,
		aaDiscovered("dr-1", "picker-v2"),
		aaClass("picker-v2"), aaZone("zone-a"),
		aaConfig("picker-v2", "zone-a"))

	reconcileDR(t, r, "dr-1")

	robot := getRobotOrNil(t, r, "dr-1")
	if robot == nil {
		t.Fatal("expected Robot to be auto-admitted")
	}
	if robot.Spec.Zone != "zone-a" || robot.Spec.RobotClass != "picker-v2" || robot.Spec.Adapter.Name != "acme-adapter" {
		t.Fatalf("robot spec = %+v, want zone-a/picker-v2/acme-adapter", robot.Spec)
	}
	// The announced robot_id must be stamped so telemetry status projection can map it (robot_status_sink).
	if got := robot.Annotations[RobotIDAnnotation]; got != "dr-1" {
		t.Errorf("annotation %s = %q, want the announced id %q", RobotIDAnnotation, got, "dr-1")
	}
	if getDROrNil(t, c, "dr-1") != nil {
		t.Error("DiscoveredRobot should be deleted after auto-admit")
	}
}

// Auto-admit configured with a class but no zone is inert: no Robot, DR retained.
func TestAutoAdmit_ClassWithoutZoneIsInert(t *testing.T) {
	now := drBase
	r, c := newDRReconciler(t, &now,
		aaDiscovered("dr-1", "picker-v2"),
		aaClass("picker-v2"),
		aaConfig("picker-v2", ""))

	reconcileDR(t, r, "dr-1")

	if getRobotOrNil(t, r, "dr-1") != nil {
		t.Error("no Robot should be created without autoAdmitZone")
	}
	if getDROrNil(t, c, "dr-1") == nil {
		t.Error("DiscoveredRobot should be retained for operator admission")
	}
}

// A robot whose suggested class does not match the configured class is not admitted.
func TestAutoAdmit_SuggestionMismatchNoAdmit(t *testing.T) {
	now := drBase
	r, c := newDRReconciler(t, &now,
		aaDiscovered("dr-1", "other-class"),
		aaClass("picker-v2"), aaZone("zone-a"),
		aaConfig("picker-v2", "zone-a"))

	reconcileDR(t, r, "dr-1")

	if getRobotOrNil(t, r, "dr-1") != nil {
		t.Error("no Robot should be created on class mismatch")
	}
	if getDROrNil(t, c, "dr-1") == nil {
		t.Error("DiscoveredRobot should be retained on class mismatch")
	}
}

// A missing RobotClass leaves the DiscoveredRobot for the operator (no admit).
func TestAutoAdmit_MissingClassNoAdmit(t *testing.T) {
	now := drBase
	r, c := newDRReconciler(t, &now,
		aaDiscovered("dr-1", "picker-v2"),
		aaZone("zone-a"),
		aaConfig("picker-v2", "zone-a"))

	reconcileDR(t, r, "dr-1")

	if getRobotOrNil(t, r, "dr-1") != nil {
		t.Error("no Robot should be created when the RobotClass is missing")
	}
	if getDROrNil(t, c, "dr-1") == nil {
		t.Error("DiscoveredRobot should be retained when the RobotClass is missing")
	}
}

// Auto-admit runs through the same builder as the operator path (§9.1.2.5), so the hardware
// union applies here too. This path had no hardware coverage at all while it was a separate
// implementation, which is how it came to silently discard the robot's reported inventory.
func TestAutoAdmit_UnionsReportedHardwareWithTheClassTemplate(t *testing.T) {
	now := drBase
	dr := aaDiscovered("dr-1", "picker-v2")
	dr.Status.ReportedHardware = []fleetv1.DiscoveredHardwareComponent{
		{Name: "cam", Type: fleetv1.HardwareTypeCamera, Status: fleetv1.HardwareHealthy},
	}
	class := aaClass("picker-v2")
	class.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "lidar", Type: fleetv1.HardwareTypeLidar}}

	r, _ := newDRReconciler(t, &now, dr, class, aaZone("zone-a"), aaConfig("picker-v2", "zone-a"))
	reconcileDR(t, r, "dr-1")

	robot := getRobotOrNil(t, r, "dr-1")
	if robot == nil {
		t.Fatal("expected Robot to be auto-admitted")
	}
	// A zero-touch admission must not quietly drop a component the robot said it has: the
	// operator never reviewed this spec, so nobody would catch the omission.
	if len(robot.Spec.Hardware) != 2 {
		t.Fatalf("hardware must union report and template, got %+v", robot.Spec.Hardware)
	}
	if robot.Spec.Hardware[0].Name != "cam" || robot.Spec.Hardware[1].Name != "lidar" {
		t.Errorf("hardware = %+v, want the reported camera plus the class lidar", robot.Spec.Hardware)
	}
}

// The payload clamp reaches the zero-touch path too. This is where it matters most: nobody
// reviews an auto-admitted spec, so an overstated class capacity would go straight into the
// number the scheduler dispatches against.
func TestAutoAdmit_ClampsPayloadToTheReportedHardware(t *testing.T) {
	now := drBase
	forty, fortyFive := 40.0, 45.0
	dr := aaDiscovered("dr-1", "picker-v2")
	dr.Status.ReportedHardware = []fleetv1.DiscoveredHardwareComponent{
		{Name: "load-platform", Type: fleetv1.HardwareTypeLoadPlatform, MaxPayloadKg: &forty},
	}
	class := aaClass("picker-v2")
	class.Spec.Hardware = []fleetv1.HardwareComponent{
		{Name: "load-platform", Type: fleetv1.HardwareTypeLoadPlatform, MaxPayloadKg: &fortyFive},
	}
	class.Spec.DefaultConstraints = &fleetv1.ClassConstraints{MaxPayloadKg: &fortyFive}

	r, _ := newDRReconciler(t, &now, dr, class, aaZone("zone-a"), aaConfig("picker-v2", "zone-a"))
	reconcileDR(t, r, "dr-1")

	robot := getRobotOrNil(t, r, "dr-1")
	if robot == nil {
		t.Fatal("expected Robot to be auto-admitted")
	}
	if robot.Spec.Constraints == nil || *robot.Spec.Constraints.MaxPayloadKg != 40 {
		t.Fatalf("payload constraint = %+v, want the reported 40 kg", robot.Spec.Constraints)
	}
}
