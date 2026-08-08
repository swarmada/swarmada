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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func newRobotEstop(t *testing.T, est *fakeEstopper, objs ...client.Object) (*RobotEstopReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fleetv1.Robot{}).WithObjects(objs...).Build()
	return &RobotEstopReconciler{Client: c, Estopper: est}, c
}

func robotWithEstop(name, trigger, processed string) *fleetv1.Robot {
	ann := map[string]string{}
	if trigger != "" {
		ann[annEstopTriggered] = trigger
	}
	if processed != "" {
		ann[annEstopProcessed] = processed
	}
	r := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: zeNS, Name: name}}
	if len(ann) > 0 {
		r.Annotations = ann
	}
	return r
}

func reconcileRobotEstop(t *testing.T, r *RobotEstopReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: zeNS, Name: name},
	}); err != nil {
		t.Fatalf("reconcile %q: %v", name, err)
	}
}

// A trigger annotation confirmed-estops the one robot and records the processed marker.
func TestRobotEstop_TriggerEstopsRobot(t *testing.T) {
	est := &fakeEstopper{}
	r, c := newRobotEstop(t, est, robotWithEstop("amr-1", "person detected", ""))

	reconcileRobotEstop(t, r, "amr-1")

	if len(est.estopped) != 1 || est.estopped[0] != "amr-1" {
		t.Fatalf("estopped = %v, want [amr-1]", est.estopped)
	}
	if got := getRobot(t, c, "amr-1", zeNS).Annotations[annEstopProcessed]; got != "person detected" {
		t.Fatalf("processed marker = %q, want the trigger value", got)
	}
}

// The same trigger value does not re-fire on a resync.
func TestRobotEstop_Idempotent(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newRobotEstop(t, est, robotWithEstop("amr-1", "stop", ""))

	reconcileRobotEstop(t, r, "amr-1")
	reconcileRobotEstop(t, r, "amr-1")

	if len(est.estopped) != 1 {
		t.Fatalf("estop fired %d times, want 1 (idempotent)", len(est.estopped))
	}
}

// Removing the trigger (annotation gone, processed marker present) clears the estop.
func TestRobotEstop_ClearResets(t *testing.T) {
	est := &fakeEstopper{}
	r, c := newRobotEstop(t, est, robotWithEstop("amr-1", "", "stop"))

	reconcileRobotEstop(t, r, "amr-1")

	if len(est.cleared) != 1 || est.cleared[0] != "amr-1" {
		t.Fatalf("cleared = %v, want [amr-1]", est.cleared)
	}
	if _, still := getRobot(t, c, "amr-1", zeNS).Annotations[annEstopProcessed]; still {
		t.Fatal("processed marker not dropped on clear")
	}
}

// A robot with no estop annotation is a no-op.
func TestRobotEstop_NoAnnotationNoop(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newRobotEstop(t, est, robotWithEstop("amr-1", "", ""))

	reconcileRobotEstop(t, r, "amr-1")

	if len(est.estopped) != 0 || len(est.cleared) != 0 {
		t.Fatalf("expected no estop action, got estopped=%v cleared=%v", est.estopped, est.cleared)
	}
}
