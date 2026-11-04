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

package scheduler_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

func robotWithStreams(name string, streamIDs ...string) fleetv1.Robot {
	r := fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, id := range streamIDs {
		r.Status.SensorStreams = append(r.Status.SensorStreams, fleetv1.SensorStreamEntry{StreamID: id})
	}
	return r
}

func strptr(s string) *string { return &s }

func TestResolveSensorStreamRobotRef_Name(t *testing.T) {
	ref := fleetv1.SensorStreamRobotRef{Name: strptr("yard-robot-3")}
	got, ok := scheduler.ResolveSensorStreamRobotRef(ref, nil)
	if !ok || got != "yard-robot-3" {
		t.Fatalf("ResolveSensorStreamRobotRef(name) = (%q, %v), want (yard-robot-3, true)", got, ok)
	}
}

func TestResolveSensorStreamRobotRef_FromMember(t *testing.T) {
	ref := fleetv1.SensorStreamRobotRef{FromMember: strptr("patrol")}
	got, ok := scheduler.ResolveSensorStreamRobotRef(ref, func(member string) (string, bool) {
		if member == "patrol" {
			return "yard-robot-3", true
		}
		return "", false
	})
	if !ok || got != "yard-robot-3" {
		t.Fatalf("ResolveSensorStreamRobotRef(fromMember) = (%q, %v), want (yard-robot-3, true)", got, ok)
	}
}

// A member whose robot is not yet assigned (e.g. still Pending) resolves to
// not-ok, never a guess — the caller sees "not yet resolvable", not a wrong name.
func TestResolveSensorStreamRobotRef_FromMemberUnresolved(t *testing.T) {
	ref := fleetv1.SensorStreamRobotRef{FromMember: strptr("patrol")}
	_, ok := scheduler.ResolveSensorStreamRobotRef(ref, func(string) (string, bool) { return "", false })
	if ok {
		t.Fatal("expected ok=false when the member's robot is not yet resolvable")
	}
}

// THE CRITICAL ONE: a candidate robot
// that offers the required stream makes the gate pass.
func TestSensorStreamOffered_True(t *testing.T) {
	robots := []fleetv1.Robot{robotWithStreams("yard-robot-3", "front-rgb", "mic-array")}
	offered, found := scheduler.SensorStreamOffered(robots, "yard-robot-3", "front-rgb")
	if !offered || !found {
		t.Fatalf("SensorStreamOffered = (%v, %v), want (true, true)", offered, found)
	}
}

// The robot exists but does not declare the stream: SensorStreamNotOffered,
// distinct from the robot not existing at all.
func TestSensorStreamOffered_RobotFoundButStreamMissing(t *testing.T) {
	robots := []fleetv1.Robot{robotWithStreams("yard-robot-3", "mic-array")}
	offered, found := scheduler.SensorStreamOffered(robots, "yard-robot-3", "front-rgb")
	if offered {
		t.Fatal("expected offered=false — the robot does not declare front-rgb")
	}
	if !found {
		t.Fatal("expected found=true — the robot exists among candidates")
	}
}

func TestSensorStreamOffered_RobotNotFound(t *testing.T) {
	robots := []fleetv1.Robot{robotWithStreams("amr-1", "front-rgb")}
	offered, found := scheduler.SensorStreamOffered(robots, "yard-robot-3", "front-rgb")
	if offered || found {
		t.Fatalf("SensorStreamOffered = (%v, %v), want (false, false)", offered, found)
	}
}

func TestReasonSensorStreamNotOffered_IsTheDesignVocabularyString(t *testing.T) {
	if scheduler.ReasonSensorStreamNotOffered != "SensorStreamNotOffered" {
		t.Fatalf("ReasonSensorStreamNotOffered = %q, want the scheduler reason-vocabulary string exactly", scheduler.ReasonSensorStreamNotOffered)
	}
}

// A stream whose required_capabilities are not all Active MUST NOT be treated as
// offered (rfcs/rfc-0001-core-spec/sensor-streams.md, Declaration). This is the rule
// that separates "the robot says it has a camera" from "the camera can produce
// frames": a Paused capability is a sensor that is off, and a gate that answered
// "offered" for it would commit the control plane to activating nothing.
func streamRobotNeeding(name, streamID string, required []string, caps map[string]fleetv1.CapabilityStatus) fleetv1.Robot {
	r := fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: name}}
	r.Status.SensorStreams = []fleetv1.SensorStreamEntry{
		{StreamID: streamID, RequiredCapabilities: required},
	}
	for capName, status := range caps {
		r.Status.Capabilities = append(r.Status.Capabilities,
			fleetv1.CapabilityStatusEntry{Name: capName, Status: status})
	}
	return r
}

func TestSensorStreamOffered_RequiredCapabilityActive(t *testing.T) {
	robots := []fleetv1.Robot{streamRobotNeeding("yard-robot-3", "front-rgb",
		[]string{"camera_front"},
		map[string]fleetv1.CapabilityStatus{"camera_front": fleetv1.CapabilityStatusActive})}
	offered, found := scheduler.SensorStreamOffered(robots, "yard-robot-3", "front-rgb")
	if !offered || !found {
		t.Fatalf("SensorStreamOffered = (%v, %v), want (true, true)", offered, found)
	}
	if unmet := scheduler.SensorStreamCapabilitiesUnmet(robots, "yard-robot-3", "front-rgb"); len(unmet) != 0 {
		t.Fatalf("SensorStreamCapabilitiesUnmet = %v, want none", unmet)
	}
}

func TestSensorStreamOffered_RequiredCapabilityPaused_IsNotOffered(t *testing.T) {
	robots := []fleetv1.Robot{streamRobotNeeding("yard-robot-3", "front-rgb",
		[]string{"camera_front"},
		map[string]fleetv1.CapabilityStatus{"camera_front": fleetv1.CapabilityStatusPaused})}
	offered, found := scheduler.SensorStreamOffered(robots, "yard-robot-3", "front-rgb")
	if offered {
		t.Fatal("a stream whose required capability is Paused must not be treated as offered")
	}
	if !found {
		t.Fatal("robotFound must stay true — the robot exists, it just cannot serve the stream")
	}
	unmet := scheduler.SensorStreamCapabilitiesUnmet(robots, "yard-robot-3", "front-rgb")
	if len(unmet) != 1 || unmet[0] != "camera_front" {
		t.Fatalf("SensorStreamCapabilitiesUnmet = %v, want [camera_front] so the message can name it", unmet)
	}
}

// Degraded does NOT satisfy a stream's required capability, and the asymmetry with
// the action-level acceptDegraded flag is deliberate: that flag is an author's
// statement about the ACTION's tolerance for reduced performance; a stream's required
// capability is the adapter's statement about what the SENSOR needs to produce frames
// at all.
func TestSensorStreamOffered_DegradedDoesNotSatisfyAStreamRequirement(t *testing.T) {
	robots := []fleetv1.Robot{streamRobotNeeding("yard-robot-3", "front-rgb",
		[]string{"camera_front"},
		map[string]fleetv1.CapabilityStatus{"camera_front": fleetv1.CapabilityStatusDegraded})}
	if offered, _ := scheduler.SensorStreamOffered(robots, "yard-robot-3", "front-rgb"); offered {
		t.Fatal("a Degraded capability must not make a stream offered")
	}
}

// The two "not offered" causes must stay distinguishable. An empty unmet set on a
// refused stream means the robot does not declare it at all — a different fix.
func TestSensorStreamCapabilitiesUnmet_NilWhenTheStreamIsNotDeclared(t *testing.T) {
	robots := []fleetv1.Robot{robotWithStreams("yard-robot-3", "mic-array")}
	if unmet := scheduler.SensorStreamCapabilitiesUnmet(robots, "yard-robot-3", "front-rgb"); unmet != nil {
		t.Fatalf("SensorStreamCapabilitiesUnmet = %v, want nil — the stream is not declared at all", unmet)
	}
}

// Deterministic ordering: the unmet set is written into a status message on every
// scheduling pass, so an unchanged robot must yield an unchanged string (RA-1).
func TestSensorStreamCapabilitiesUnmet_IsSorted(t *testing.T) {
	robots := []fleetv1.Robot{streamRobotNeeding("yard-robot-3", "front-rgb",
		[]string{"zoom", "camera_front", "illumination"}, nil)}
	unmet := scheduler.SensorStreamCapabilitiesUnmet(robots, "yard-robot-3", "front-rgb")
	want := []string{"camera_front", "illumination", "zoom"}
	if len(unmet) != len(want) {
		t.Fatalf("SensorStreamCapabilitiesUnmet = %v, want %v", unmet, want)
	}
	for i := range want {
		if unmet[i] != want[i] {
			t.Fatalf("SensorStreamCapabilitiesUnmet = %v, want %v (sorted)", unmet, want)
		}
	}
}
