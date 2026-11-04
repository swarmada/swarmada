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

package scheduler

import fleetv1 "github.com/swarmada/swarmada/api/v1"

// ReasonArtifactStoreNotReachable is the closed reason vocabulary entry
// (the scheduler's G2 admission table) for: a consuming
// step cannot reach the ArtifactStore its input requires from where it would
// run. Written to status.conditions[type=ExternalDependenciesReady].reason
// and status.failureReason — never assigned. The action stays Pending; no
// robot is committed.
const ReasonArtifactStoreNotReachable = "ArtifactStoreNotReachable"

// ArtifactStoreReachable reports whether a consumer running in consumerZone
// can read from store:
//   - ObjectStore is reachable from anywhere.
//   - VolumeClaim is reachable from anywhere ONLY if its declared AccessMode
//     is ReadWriteMany or ReadOnlyMany; otherwise it can be read from one
//     place only, and this reports true ONLY when consumerZone matches — a
//     VolumeClaim has no zone of its own, so a non-RWX claim is treated as
//     reachable only by a consumer that does not require zone placement at
//     all (an empty consumerZone request); a zone-scoped consumer against a
//     non-RWX claim is never reachable by this function and must be
//     co-located by some other declared means.
//   - EdgeLocal is reachable ONLY from its own zone.
//
// consumerZone is the zone the consuming step would run in; empty means "no
// zone constraint" (a cluster-side step with no zone requirement of its own).
func ArtifactStoreReachable(store *fleetv1.ArtifactStoreSpec, consumerZone string) bool {
	if store == nil {
		return false
	}
	switch store.Kind {
	case fleetv1.ArtifactStoreKindObjectStore:
		return store.ObjectStore != nil
	case fleetv1.ArtifactStoreKindVolumeClaim:
		if store.VolumeClaim == nil {
			return false
		}
		if store.VolumeClaim.AccessMode.ReachableFromEverywhere() {
			return true
		}
		return consumerZone == ""
	case fleetv1.ArtifactStoreKindEdgeLocal:
		return store.EdgeLocal != nil && store.EdgeLocal.Zone != "" && store.EdgeLocal.Zone == consumerZone
	default:
		return false
	}
}
