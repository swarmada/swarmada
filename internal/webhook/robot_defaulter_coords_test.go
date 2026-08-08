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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func coordsDefaulter(t *testing.T, objs ...client.Object) *RobotDefaulter {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &RobotDefaulter{Client: c}
}

// No SwarmadaConfig ⇒ the coordinate annotations fall back to the CRD defaults.
func TestStampCoordinateAnnotations_Defaults(t *testing.T) {
	d := coordsDefaulter(t)
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "r1"}}

	d.stampCoordinateAnnotations(context.Background(), robot)

	if got := robot.Annotations[annLengthUnit]; got != "Meters" {
		t.Errorf("length-unit = %q, want Meters", got)
	}
	if got := robot.Annotations[annAngleUnit]; got != "Radians" {
		t.Errorf("angle-unit = %q, want Radians", got)
	}
	if got := robot.Annotations[annGroundFloor]; got != "0" {
		t.Errorf("ground-floor = %q, want 0", got)
	}
}

// A configured coordinate system is stamped onto the Robot.
func TestStampCoordinateAnnotations_FromConfig(t *testing.T) {
	cfg := &fleetv1.SwarmadaConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "swarmada-config"},
		Spec: fleetv1.SwarmadaConfigSpec{
			CoordinateSystem: fleetv1.SwarmadaCoordinateSystemConfig{
				LengthUnit:  fleetv1.LengthUnitMillimeters,
				AngleUnit:   fleetv1.AngleUnitDegrees,
				GroundFloor: 2,
			},
		},
	}
	d := coordsDefaulter(t, cfg)
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "r1"}}

	d.stampCoordinateAnnotations(context.Background(), robot)

	if got := robot.Annotations[annLengthUnit]; got != "Millimeters" {
		t.Errorf("length-unit = %q, want Millimeters", got)
	}
	if got := robot.Annotations[annAngleUnit]; got != "Degrees" {
		t.Errorf("angle-unit = %q, want Degrees", got)
	}
	if got := robot.Annotations[annGroundFloor]; got != "2" {
		t.Errorf("ground-floor = %q, want 2", got)
	}
}
