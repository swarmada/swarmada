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
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// edgeFeedZone builds an edge-node FleetZone whose status already carries the
// edge node's edgeFeedUnavailable report (the edge writes it; here we seed it).
func edgeFeedZone(name string, hasEdgeNode bool, unavailable ...string) *fleetv1.FleetZone {
	fz := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Status:     fleetv1.FleetZoneStatus{EdgeFeedUnavailable: unavailable},
	}
	if hasEdgeNode {
		fz.Spec.EdgeNode = &fleetv1.EdgeNodeConfig{}
	}
	return fz
}

func recorderOf(t *testing.T, z *ZoneController) *record.FakeRecorder {
	t.Helper()
	rec, ok := z.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatal("expected a FakeRecorder")
	}
	return rec
}

// A non-empty edgeFeedUnavailable on an edge-node zone emits a Warning.
func TestZoneReconcile_EdgeFeedUnavailable_EmitsWarning(t *testing.T) {
	z, _ := newZoneController(t, edgeFeedZone("dock", true, "r1", "r2"))

	reconcileZone(t, z, "dock")

	assertEvent(t, recorderOf(t, z), "EdgeFeedUnavailable")
}

// The Warning fires once per transition, not on every resync.
func TestZoneReconcile_EdgeFeedUnavailable_Idempotent(t *testing.T) {
	z, _ := newZoneController(t, edgeFeedZone("dock", true, "r1"))

	reconcileZone(t, z, "dock")
	reconcileZone(t, z, "dock")

	rec := recorderOf(t, z)
	assertEvent(t, rec, "EdgeFeedUnavailable")
	assertNoEvent(t, rec) // no second warning on the resync
}

// A zone without an edge node never warns, even if the field is set.
func TestZoneReconcile_EdgeFeed_NoEdgeNode_NoEvent(t *testing.T) {
	z, _ := newZoneController(t, edgeFeedZone("dock", false, "r1"))

	reconcileZone(t, z, "dock")

	assertNoEvent(t, recorderOf(t, z))
}

// Clearing the report emits a matching EdgeFeedRestored.
func TestZoneReconcile_EdgeFeedRestored(t *testing.T) {
	z, c := newZoneController(t, edgeFeedZone("dock", true, "r1"))

	reconcileZone(t, z, "dock")
	rec := recorderOf(t, z)
	assertEvent(t, rec, "EdgeFeedUnavailable")

	fz := getZone(t, c, "dock")
	fz.Status.EdgeFeedUnavailable = nil
	if err := c.Status().Update(context.Background(), fz); err != nil {
		t.Fatalf("clearing edgeFeedUnavailable: %v", err)
	}

	reconcileZone(t, z, "dock")
	assertEvent(t, rec, "EdgeFeedRestored")
}
