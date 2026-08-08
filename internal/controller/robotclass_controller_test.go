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

func robotClass(ns, name string, gen int64) *fleetv1.RobotClass {
	return &fleetv1.RobotClass{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: gen}}
}

func robotInClass(ns, name, class string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       fleetv1.RobotSpec{RobotClass: class},
	}
}

func newRobotClassReconciler(t *testing.T, objs ...client.Object) (*RobotClassReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.RobotClass{}).
		Build()
	return &RobotClassReconciler{Client: c, Scheme: scheme}, c
}

func reconcileClass(t *testing.T, r *RobotClassReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getClass(t *testing.T, c client.Client, ns, name string) *fleetv1.RobotClass {
	t.Helper()
	var rc fleetv1.RobotClass
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &rc); err != nil {
		t.Fatalf("get RobotClass: %v", err)
	}
	return &rc
}

func TestRobotClass_CountsReferencingRobotsAndGeneration(t *testing.T) {
	r, c := newRobotClassReconciler(t,
		robotClass("warehouse-a", "amr-x", 7),
		robotInClass("warehouse-a", "r1", "amr-x"),
		robotInClass("warehouse-a", "r2", "amr-x"),
		robotInClass("warehouse-a", "r3", "other-class"), // different class → excluded
		robotInClass("other-ns", "r4", "amr-x"),          // different namespace → excluded
	)
	reconcileClass(t, r, "warehouse-a", "amr-x")

	rc := getClass(t, c, "warehouse-a", "amr-x")
	if rc.Status.ReferencingRobots != 2 {
		t.Errorf("ReferencingRobots = %d, want 2", rc.Status.ReferencingRobots)
	}
	if rc.Status.ObservedGeneration != 7 {
		t.Errorf("ObservedGeneration = %d, want 7 (the merge generation)", rc.Status.ObservedGeneration)
	}
}

func TestRobotClass_ZeroWhenNoReferences(t *testing.T) {
	r, c := newRobotClassReconciler(t, robotClass("warehouse-a", "amr-x", 1))
	reconcileClass(t, r, "warehouse-a", "amr-x")
	if got := getClass(t, c, "warehouse-a", "amr-x").Status.ReferencingRobots; got != 0 {
		t.Errorf("ReferencingRobots = %d, want 0", got)
	}
}

// The count tracks robot lifecycle: it drops when a referencing robot is deleted.
func TestRobotClass_CountUpdatesOnRobotRemoval(t *testing.T) {
	r, c := newRobotClassReconciler(t,
		robotClass("warehouse-a", "amr-x", 1),
		robotInClass("warehouse-a", "r1", "amr-x"),
		robotInClass("warehouse-a", "r2", "amr-x"),
	)
	reconcileClass(t, r, "warehouse-a", "amr-x")
	if got := getClass(t, c, "warehouse-a", "amr-x").Status.ReferencingRobots; got != 2 {
		t.Fatalf("initial ReferencingRobots = %d, want 2", got)
	}

	if err := c.Delete(context.Background(), robotInClass("warehouse-a", "r2", "amr-x")); err != nil {
		t.Fatalf("delete robot: %v", err)
	}
	reconcileClass(t, r, "warehouse-a", "amr-x")
	if got := getClass(t, c, "warehouse-a", "amr-x").Status.ReferencingRobots; got != 1 {
		t.Errorf("after removal ReferencingRobots = %d, want 1", got)
	}
}

// A missing RobotClass is a clean no-op (e.g. reconcile racing a delete).
func TestRobotClass_MissingIsNoop(t *testing.T) {
	r, _ := newRobotClassReconciler(t)
	reconcileClass(t, r, "warehouse-a", "gone")
}
