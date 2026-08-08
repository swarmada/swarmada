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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

const zoneNS = "warehouse-a"

func newZoneValidator(t *testing.T, objs ...client.Object) *FleetZoneValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &FleetZoneValidator{Client: c}
}

func zoneWithParent(name, parent string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent},
	}
}

func squareZone(name string) *fleetv1.FleetZone {
	z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: name}}
	z.Spec.PhysicalBounds = &fleetv1.PhysicalBounds{Polygon: []fleetv1.Point{
		{X: 0, Y: 0}, {X: 4, Y: 0}, {X: 4, Y: 4}, {X: 0, Y: 4}}}
	return z
}

func TestFleetZone_ValidCreatePasses(t *testing.T) {
	v := newZoneValidator(t, zoneWithParent("root", ""))
	child := squareZone("dock")
	child.Spec.ParentZone = "root"
	if _, err := v.ValidateCreate(context.Background(), child); err != nil {
		t.Fatalf("expected valid zone to pass: %v", err)
	}
}

func TestFleetZone_MissingParentRejected(t *testing.T) {
	v := newZoneValidator(t)
	if _, err := v.ValidateCreate(context.Background(), zoneWithParent("dock", "ghost")); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected parentZone-not-found rejection, got: %v", err)
	}
}

func TestFleetZone_SelfParentRejected(t *testing.T) {
	v := newZoneValidator(t)
	if _, err := v.ValidateCreate(context.Background(), zoneWithParent("dock", "dock")); err == nil ||
		!strings.Contains(err.Error(), "its own parentZone") {
		t.Fatalf("expected self-parent rejection, got: %v", err)
	}
}

// A→B→A: creating/updating A to point at B while B already points at A is a cycle.
func TestFleetZone_CycleRejected(t *testing.T) {
	// B exists and its parent is A. Now admit A with parentZone=B → cycle A→B→A.
	v := newZoneValidator(t, zoneWithParent("B", "A"))
	if _, err := v.ValidateUpdate(context.Background(), zoneWithParent("A", ""), zoneWithParent("A", "B")); err == nil ||
		!strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle rejection, got: %v", err)
	}
}

func TestFleetZone_SelfIntersectingPolygonRejected(t *testing.T) {
	v := newZoneValidator(t)
	bowtie := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: zoneNS, Name: "z"}}
	bowtie.Spec.PhysicalBounds = &fleetv1.PhysicalBounds{Polygon: []fleetv1.Point{
		{X: 0, Y: 0}, {X: 4, Y: 4}, {X: 4, Y: 0}, {X: 0, Y: 4}}}
	if _, err := v.ValidateCreate(context.Background(), bowtie); err == nil ||
		!strings.Contains(err.Error(), "self-intersecting") {
		t.Fatalf("expected self-intersecting-polygon rejection, got: %v", err)
	}
	// The same zone with a simple square passes.
	if _, err := v.ValidateCreate(context.Background(), squareZone("z")); err != nil {
		t.Fatalf("simple square should pass: %v", err)
	}
}

func TestFleetZone_DeleteWithChildrenRejected(t *testing.T) {
	v := newZoneValidator(t, zoneWithParent("root", ""), zoneWithParent("dock", "root"))
	// Deleting root (which has child "dock") is rejected.
	if _, err := v.ValidateDelete(context.Background(), zoneWithParent("root", "")); err == nil ||
		!strings.Contains(err.Error(), "child zones") {
		t.Fatalf("expected delete-with-children rejection, got: %v", err)
	}
	// Deleting the leaf child is allowed.
	if _, err := v.ValidateDelete(context.Background(), zoneWithParent("dock", "root")); err != nil {
		t.Fatalf("deleting a leaf zone should be allowed: %v", err)
	}
}
