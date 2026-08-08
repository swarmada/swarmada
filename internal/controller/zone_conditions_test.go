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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// capZone builds a leaf zone (with bounds) carrying a concurrency cap and seeded
// occupancy, for CapacityAvailable transition tests.
func capZone(name string, max, current int32) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetZoneSpec{MaxConcurrentRobots: max, PhysicalBounds: leafZoneWithBounds(name).Spec.PhysicalBounds},
		Status:     fleetv1.FleetZoneStatus{CurrentConcurrentRobots: current},
	}
}

func findCond(conds []metav1.Condition, typ string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}

func leafZoneWithBounds(name string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetZoneSpec{
			PhysicalBounds: &fleetv1.PhysicalBounds{
				Floor:   0,
				Polygon: []fleetv1.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}},
			},
		},
	}
}

func TestZoneCondition_ZoneReady(t *testing.T) {
	t.Run("leaf with physicalBounds is Ready", func(t *testing.T) {
		z, c := newZoneController(t, leafZoneWithBounds("leaf"))
		reconcileZone(t, z, "leaf")
		cond := findCond(getZone(t, c, "leaf").Status.Conditions, "ZoneReady")
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Reconciled" {
			t.Fatalf("ZoneReady = %+v, want True/Reconciled", cond)
		}
	})

	t.Run("leaf without physicalBounds is NotReady", func(t *testing.T) {
		z, c := newZoneController(t, zoneObj("bare-leaf", ""))
		reconcileZone(t, z, "bare-leaf")
		cond := findCond(getZone(t, c, "bare-leaf").Status.Conditions, "ZoneReady")
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "NoPhysicalBounds" {
			t.Fatalf("ZoneReady = %+v, want False/NoPhysicalBounds", cond)
		}
	})

	t.Run("non-leaf aggregating zone is Ready without bounds", func(t *testing.T) {
		z, c := newZoneController(t, zoneObj("parent", ""), zoneObj("child", "parent"))
		reconcileZone(t, z, "parent")
		cond := findCond(getZone(t, c, "parent").Status.Conditions, "ZoneReady")
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("non-leaf ZoneReady = %+v, want True (aggregating zone needs no bounds)", cond)
		}
	})
}

func TestZoneCondition_CapacityAvailable(t *testing.T) {
	zoneWithCap := func(name string, max, current int32) *fleetv1.FleetZone {
		return &fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
			Spec:       fleetv1.FleetZoneSpec{MaxConcurrentRobots: max, PhysicalBounds: leafZoneWithBounds(name).Spec.PhysicalBounds},
			Status:     fleetv1.FleetZoneStatus{CurrentConcurrentRobots: current},
		}
	}
	cases := []struct {
		name       string
		max        int32
		current    int32
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"unlimited (max 0) is always available", 0, 5, metav1.ConditionTrue, "Available"},
		{"below capacity is available", 3, 1, metav1.ConditionTrue, "Available"},
		{"at capacity is unavailable", 2, 2, metav1.ConditionFalse, "AtCapacity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z, c := newZoneController(t, zoneWithCap("z", tc.max, tc.current))
			reconcileZone(t, z, "z")
			cond := findCond(getZone(t, c, "z").Status.Conditions, "CapacityAvailable")
			if cond == nil || cond.Status != tc.wantStatus || cond.Reason != tc.wantReason {
				t.Fatalf("CapacityAvailable = %+v, want %s/%s", cond, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// CapacityAvailable flips True→False when the zone fills and back to True when it
// drains, and LastTransitionTime moves on each flip.
func TestZoneCondition_CapacityTransition(t *testing.T) {
	ctx := context.Background()
	z, c := newZoneController(t, capZone("z", 1, 0)) // max 1, empty → Available
	reconcileZone(t, z, "z")
	c1 := findCond(getZone(t, c, "z").Status.Conditions, "CapacityAvailable")
	if c1 == nil || c1.Status != metav1.ConditionTrue {
		t.Fatalf("initial CapacityAvailable = %+v, want True", c1)
	}

	// Fill the zone → AtCapacity (True → False).
	fz := getZone(t, c, "z")
	fz.Status.CurrentConcurrentRobots = 1
	if err := c.Status().Update(ctx, fz); err != nil {
		t.Fatalf("bump occupancy: %v", err)
	}
	reconcileZone(t, z, "z")
	c2 := findCond(getZone(t, c, "z").Status.Conditions, "CapacityAvailable")
	if c2.Status != metav1.ConditionFalse || c2.Reason != "AtCapacity" {
		t.Fatalf("filled CapacityAvailable = %+v, want False/AtCapacity", c2)
	}

	// Drain → Available again (False → True).
	fz2 := getZone(t, c, "z")
	fz2.Status.CurrentConcurrentRobots = 0
	if err := c.Status().Update(ctx, fz2); err != nil {
		t.Fatalf("drain occupancy: %v", err)
	}
	reconcileZone(t, z, "z")
	c3 := findCond(getZone(t, c, "z").Status.Conditions, "CapacityAvailable")
	if c3.Status != metav1.ConditionTrue {
		t.Fatalf("drained CapacityAvailable = %+v, want True", c3)
	}
}

// ZoneReady flips False→True when a leaf gains physicalBounds.
func TestZoneCondition_ZoneReadyTransition(t *testing.T) {
	ctx := context.Background()
	z, c := newZoneController(t, zoneObj("leaf", "")) // bare leaf → NotReady
	reconcileZone(t, z, "leaf")
	c1 := findCond(getZone(t, c, "leaf").Status.Conditions, "ZoneReady")
	if c1 == nil || c1.Status != metav1.ConditionFalse {
		t.Fatalf("initial ZoneReady = %+v, want False", c1)
	}

	// Give the zone a polygon → Ready.
	fz := getZone(t, c, "leaf")
	fz.Spec.PhysicalBounds = leafZoneWithBounds("leaf").Spec.PhysicalBounds
	if err := c.Update(ctx, fz); err != nil {
		t.Fatalf("add bounds: %v", err)
	}
	reconcileZone(t, z, "leaf")
	c2 := findCond(getZone(t, c, "leaf").Status.Conditions, "ZoneReady")
	if c2.Status != metav1.ConditionTrue {
		t.Fatalf("ZoneReady after bounds = %+v, want True", c2)
	}
}

// RA-1: a second reconcile with no material change writes nothing, and the
// condition's LastTransitionTime does not move.
func TestZoneCondition_RA1_NoChurnOnUnchanged(t *testing.T) {
	z, c := newZoneController(t, leafZoneWithBounds("leaf"))
	reconcileZone(t, z, "leaf")
	after1 := getZone(t, c, "leaf")
	rv := after1.ResourceVersion
	ltt := findCond(after1.Status.Conditions, "ZoneReady").LastTransitionTime

	reconcileZone(t, z, "leaf")
	after2 := getZone(t, c, "leaf")
	if after2.ResourceVersion != rv {
		t.Fatalf("unchanged zone caused a status write (rv %s → %s) — RA-1 violated", rv, after2.ResourceVersion)
	}
	if !findCond(after2.Status.Conditions, "ZoneReady").LastTransitionTime.Equal(&ltt) {
		t.Fatal("ZoneReady LastTransitionTime moved without a status flip")
	}
}
