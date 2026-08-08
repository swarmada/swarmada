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

// The RobotDefaulter backfills swarmada.io/robot-id to metadata.name so
// operator-created Robots resolve robot_id → Robot for telemetry status projection
// (RFC-0001 §9.3.1); an already-set value is preserved.

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func newRobotIDDefaulter(t *testing.T) *RobotDefaulter {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &RobotDefaulter{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
}

func TestDefault_StampsRobotIDWhenAbsent(t *testing.T) {
	d := newRobotIDDefaulter(t)
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "robot-al", Namespace: "default"}}

	if err := d.Default(context.Background(), robot); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if got := robot.Annotations[fleetv1.RobotIDAnnotation]; got != "robot-al" {
		t.Errorf("%s = %q, want the metadata.name %q", fleetv1.RobotIDAnnotation, got, "robot-al")
	}
}

func TestDefault_KeepsExistingRobotID(t *testing.T) {
	d := newRobotIDDefaulter(t)
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{
		Name:        "robot-al",
		Namespace:   "default",
		Annotations: map[string]string{fleetv1.RobotIDAnnotation: "serial-1234"},
	}}

	if err := d.Default(context.Background(), robot); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if got := robot.Annotations[fleetv1.RobotIDAnnotation]; got != "serial-1234" {
		t.Errorf("%s = %q, want the preserved value %q", fleetv1.RobotIDAnnotation, got, "serial-1234")
	}
}
