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

package webhook

import (
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func i32(v int32) *int32     { return &v }
func f64(v float64) *float64 { return &v }

// A RobotClass with defaultTelemetry fills each telemetry field the Robot left unset.
func TestMergeRobotClass_DefaultTelemetryFillsUnsetFields(t *testing.T) {
	robot := &fleetv1.Robot{}
	class := &fleetv1.RobotClass{
		Spec: fleetv1.RobotClassSpec{
			DefaultTelemetry: &fleetv1.ClassTelemetryDefaults{
				TelemetryIntervalSeconds: i32(10),
				MotionThresholdMeters:    f64(0.1),
				MaxIdleIntervalSeconds:   i32(60),
			},
		},
	}

	mergeRobotClass(robot, class)

	if robot.Spec.TelemetryIntervalSeconds == nil || *robot.Spec.TelemetryIntervalSeconds != 10 {
		t.Errorf("TelemetryIntervalSeconds = %v, want 10 (inherited)", robot.Spec.TelemetryIntervalSeconds)
	}
	if robot.Spec.MotionThresholdMeters == nil || *robot.Spec.MotionThresholdMeters != 0.1 {
		t.Errorf("MotionThresholdMeters = %v, want 0.1 (inherited)", robot.Spec.MotionThresholdMeters)
	}
	if robot.Spec.MaxIdleIntervalSeconds == nil || *robot.Spec.MaxIdleIntervalSeconds != 60 {
		t.Errorf("MaxIdleIntervalSeconds = %v, want 60 (inherited)", robot.Spec.MaxIdleIntervalSeconds)
	}
}

// The Robot's own telemetry values always win over the class defaults.
func TestMergeRobotClass_RobotTelemetryValuesWin(t *testing.T) {
	robot := &fleetv1.Robot{Spec: fleetv1.RobotSpec{
		TelemetryIntervalSeconds: i32(2),
		MotionThresholdMeters:    f64(0.02),
		MaxIdleIntervalSeconds:   i32(15),
	}}
	class := &fleetv1.RobotClass{Spec: fleetv1.RobotClassSpec{
		DefaultTelemetry: &fleetv1.ClassTelemetryDefaults{
			TelemetryIntervalSeconds: i32(10),
			MotionThresholdMeters:    f64(0.1),
			MaxIdleIntervalSeconds:   i32(60),
		},
	}}

	mergeRobotClass(robot, class)

	if *robot.Spec.TelemetryIntervalSeconds != 2 {
		t.Errorf("TelemetryIntervalSeconds = %d, want 2 (robot wins)", *robot.Spec.TelemetryIntervalSeconds)
	}
	if *robot.Spec.MotionThresholdMeters != 0.02 {
		t.Errorf("MotionThresholdMeters = %v, want 0.02 (robot wins)", *robot.Spec.MotionThresholdMeters)
	}
	if *robot.Spec.MaxIdleIntervalSeconds != 15 {
		t.Errorf("MaxIdleIntervalSeconds = %d, want 15 (robot wins)", *robot.Spec.MaxIdleIntervalSeconds)
	}
}

// Per-field inheritance: the class fills only the fields the Robot omitted.
func TestMergeRobotClass_DefaultTelemetryPerFieldFill(t *testing.T) {
	robot := &fleetv1.Robot{Spec: fleetv1.RobotSpec{TelemetryIntervalSeconds: i32(3)}}
	class := &fleetv1.RobotClass{Spec: fleetv1.RobotClassSpec{
		DefaultTelemetry: &fleetv1.ClassTelemetryDefaults{
			TelemetryIntervalSeconds: i32(10),
			MotionThresholdMeters:    f64(0.1),
		},
	}}

	mergeRobotClass(robot, class)

	if *robot.Spec.TelemetryIntervalSeconds != 3 {
		t.Errorf("TelemetryIntervalSeconds = %d, want 3 (robot's own kept)", *robot.Spec.TelemetryIntervalSeconds)
	}
	if robot.Spec.MotionThresholdMeters == nil || *robot.Spec.MotionThresholdMeters != 0.1 {
		t.Errorf("MotionThresholdMeters = %v, want 0.1 (inherited)", robot.Spec.MotionThresholdMeters)
	}
	if robot.Spec.MaxIdleIntervalSeconds != nil {
		t.Errorf("MaxIdleIntervalSeconds = %v, want nil (class did not set it)", robot.Spec.MaxIdleIntervalSeconds)
	}
}

// Nil defaultTelemetry is a no-op — no panic, no fields set.
func TestMergeRobotClass_NilDefaultTelemetryIsNoop(t *testing.T) {
	robot := &fleetv1.Robot{}
	mergeRobotClass(robot, &fleetv1.RobotClass{})
	if robot.Spec.TelemetryIntervalSeconds != nil || robot.Spec.MotionThresholdMeters != nil ||
		robot.Spec.MaxIdleIntervalSeconds != nil {
		t.Error("expected no telemetry fields set when the class has no defaultTelemetry")
	}
}

// The inherited value is copied, not aliased to the class object.
func TestMergeRobotClass_DefaultTelemetryIsValueCopied(t *testing.T) {
	class := &fleetv1.RobotClass{Spec: fleetv1.RobotClassSpec{
		DefaultTelemetry: &fleetv1.ClassTelemetryDefaults{TelemetryIntervalSeconds: i32(10)},
	}}
	robot := &fleetv1.Robot{}
	mergeRobotClass(robot, class)

	*robot.Spec.TelemetryIntervalSeconds = 99 // mutate the robot's copy
	if *class.Spec.DefaultTelemetry.TelemetryIntervalSeconds != 10 {
		t.Error("mutating the robot's telemetry value must not affect the class object")
	}
}
