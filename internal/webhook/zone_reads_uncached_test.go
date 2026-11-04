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

// ─────────────────────────────────────────────────────────────────────────────
// ADMISSION DECIDES ON THE API SERVER'S ANSWER, NOT THE CACHE'S.
//
// Each test hands the validator two readers that disagree: a "cache" that has not yet
// seen a write, and an API reader that has. The validator must decide on the API
// reader's answer. The cache is the manager's informer, which lags an object admitted
// milliseconds earlier; on these paths a stale read admits a parentZone cycle, or a
// Robot into a zone that has just gained a child (RFC-0001
// #fleetzone-zone-hierarchy-invariants-controller-enforced).
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func zoneClient(t *testing.T, zones ...*fleetv1.FleetZone) client.Client {
	t.Helper()
	objs := make([]client.Object, 0, len(zones))
	for _, z := range zones {
		objs = append(objs, z.DeepCopy())
	}
	return fake.NewClientBuilder().WithScheme(fzrScheme(t)).WithObjects(objs...).Build()
}

// B has just been updated to parentZone: A; the cache still has B parentless. Setting
// A's parentZone to B closes A→B→A, and only the API reader can see it.
func TestFleetZoneCycleCheckReadsTheAPIServerNotTheCache(t *testing.T) {
	stale := zoneClient(t, fzrZone("a", "", nil), fzrZone("b", "", nil))
	fresh := zoneClient(t, fzrZone("a", "", nil), fzrZone("b", "a", nil))
	v := &FleetZoneValidator{Client: stale, APIReader: fresh}

	oldA := fzrZone("a", "", nil)
	newA := fzrZone("a", "b", nil)
	_, err := v.ValidateUpdate(context.Background(), oldA, newA)
	if err == nil {
		t.Fatal("setting a.parentZone=b was ADMITTED although b.parentZone=a on the API server: " +
			"the cycle a→b→a was judged against a stale cache")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("refused, but not as a cycle: %v", err)
	}
}

// The fail-closed direction, listed for completeness: a parent created just before
// its child is found, so a correct object is not refused.
func TestFleetZoneParentExistenceReadsTheAPIServerNotTheCache(t *testing.T) {
	stale := zoneClient(t)
	fresh := zoneClient(t, fzrZone("site", "", nil))
	v := &FleetZoneValidator{Client: stale, APIReader: fresh}

	if _, err := v.ValidateCreate(context.Background(), fzrZone("hall", "site", nil)); err != nil {
		t.Fatalf("a child of a parent the API server has was refused on a stale cache miss: %v", err)
	}
}

// With no uncached reader the chain cannot be checked, and the zone is not admitted
// unchecked. A parentless zone needs no read and is unaffected.
func TestFleetZoneChainCheckFailsClosedWithoutAnAPIReader(t *testing.T) {
	c := zoneClient(t, fzrZone("site", "", nil))
	v := &FleetZoneValidator{Client: c}

	if _, err := v.ValidateCreate(context.Background(), fzrZone("hall", "site", nil)); err == nil {
		t.Fatal("a zone naming a parentZone was admitted by a validator with no uncached reader")
	}
	if _, err := v.ValidateCreate(context.Background(), fzrZone("site-2", "", nil)); err != nil {
		t.Fatalf("a parentless zone needs no read and must be admitted: %v", err)
	}
}

// A child zone was created under aisle-b3 moments ago; the cache has not seen it. A
// Robot declaring aisle-b3 must be refused, because the zone is no longer terminal.
func TestRobotLeafCheckReadsTheAPIServerNotTheCache(t *testing.T) {
	stale := xoClient(t, xoAdapter(), xoZone("aisle-b3", ""))
	fresh := xoClient(t, xoAdapter(), xoZone("aisle-b3", ""), xoZone("aisle-b3-east", "aisle-b3"))
	g := &RobotAdmissionGate{Client: stale, APIReader: fresh}

	_, err := g.ValidateCreate(context.Background(), xoRobot("amr-1", "aisle-b3", "rid-1"))
	mustRejectWith(t, err, `spec.zone "aisle-b3" is not a leaf zone (has children: [aisle-b3-east])`)
}

// The composite half reads the same zone through the same reader: members added moments
// ago are seen.
func TestRobotCompositeCheckReadsTheAPIServerNotTheCache(t *testing.T) {
	composite := xoZone("ward-2", "")
	composite.Spec.MemberZones = []string{"room-a"}
	stale := xoClient(t, xoAdapter(), xoZone("ward-2", ""), xoZone("room-a", ""))
	fresh := xoClient(t, xoAdapter(), composite, xoZone("room-a", ""))
	g := &RobotAdmissionGate{Client: stale, APIReader: fresh}

	_, err := g.ValidateCreate(context.Background(), xoRobot("amr-1", "ward-2", "rid-1"))
	if err == nil || !strings.Contains(err.Error(), "composite") {
		t.Fatalf("a Robot declaring a zone the API server has as a composite must be refused; got %v", err)
	}
}
