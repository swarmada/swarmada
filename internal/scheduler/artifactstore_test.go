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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

func TestArtifactStoreReachable_ObjectStoreIsReachableFromAnywhere(t *testing.T) {
	store := &fleetv1.ArtifactStoreSpec{Kind: fleetv1.ArtifactStoreKindObjectStore, ObjectStore: &fleetv1.ObjectStoreBacking{Endpoint: "s3://evidence"}}
	for _, zone := range []string{"", "yard-west", "floor-2"} {
		if !scheduler.ArtifactStoreReachable(store, zone) {
			t.Errorf("ObjectStore should be reachable from zone %q", zone)
		}
	}
}

// THE CRITICAL ONE: a non-RWX/ROX
// VolumeClaim is placement-constrained.
func TestArtifactStoreReachable_NonRWXVolumeClaim(t *testing.T) {
	store := &fleetv1.ArtifactStoreSpec{Kind: fleetv1.ArtifactStoreKindVolumeClaim,
		VolumeClaim: &fleetv1.VolumeClaimBacking{ClaimName: "evidence-pvc", AccessMode: fleetv1.VolumeAccessModeReadWriteOnce}}

	if !scheduler.ArtifactStoreReachable(store, "") {
		t.Error("a zone-agnostic consumer should be able to use a ReadWriteOnce claim")
	}
	if scheduler.ArtifactStoreReachable(store, "yard-west") {
		t.Error("a zone-scoped consumer must NOT be considered reachable against a ReadWriteOnce claim — that is exactly the placement constraint ArtifactStoreNotReachable exists to catch")
	}
}

func TestArtifactStoreReachable_RWXVolumeClaimIsReachableFromAnywhere(t *testing.T) {
	store := &fleetv1.ArtifactStoreSpec{Kind: fleetv1.ArtifactStoreKindVolumeClaim,
		VolumeClaim: &fleetv1.VolumeClaimBacking{ClaimName: "evidence-pvc", AccessMode: fleetv1.VolumeAccessModeReadWriteMany}}

	for _, zone := range []string{"", "yard-west"} {
		if !scheduler.ArtifactStoreReachable(store, zone) {
			t.Errorf("a ReadWriteMany claim should be reachable from zone %q", zone)
		}
	}
}

// EdgeLocal is reachable ONLY from its own declared zone — never cluster-wide.
func TestArtifactStoreReachable_EdgeLocalIsZoneScoped(t *testing.T) {
	store := &fleetv1.ArtifactStoreSpec{Kind: fleetv1.ArtifactStoreKindEdgeLocal,
		EdgeLocal: &fleetv1.EdgeLocalBacking{Zone: "yard-west", Path: "/var/lib/swarmada/captures"}}

	if !scheduler.ArtifactStoreReachable(store, "yard-west") {
		t.Error("EdgeLocal must be reachable from its own zone")
	}
	if scheduler.ArtifactStoreReachable(store, "floor-2") {
		t.Error("EdgeLocal must NOT be reachable from a different zone")
	}
	if scheduler.ArtifactStoreReachable(store, "") {
		t.Error("EdgeLocal must NOT be reachable cluster-wide (no zone specified)")
	}
}

func TestArtifactStoreReachable_NilStore(t *testing.T) {
	if scheduler.ArtifactStoreReachable(nil, "yard-west") {
		t.Error("a nil store must never be reported reachable")
	}
}

func TestReasonArtifactStoreNotReachable_IsTheDesignVocabularyString(t *testing.T) {
	if scheduler.ReasonArtifactStoreNotReachable != "ArtifactStoreNotReachable" {
		t.Fatalf("ReasonArtifactStoreNotReachable = %q, want the scheduler reason string exactly", scheduler.ReasonArtifactStoreNotReachable)
	}
}
