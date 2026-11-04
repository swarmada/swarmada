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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// FleetZone.spec.memberZones — composite zones (RFC-0001 #fleetzone-composite-zones-and-zone-expansion, FZ-C1..C6).
//
// THESE ARE ENVTESTS FOR A REASON, AND THE REASON IS THIS FIELD'S HISTORY.
// Until this change `spec.memberZones` was, in RFC-0001's own words, "specified,
// not in the v0.4 schema — a write to this path is pruned by the API server": an
// operator applying a composite zone got a success response and a zone with no
// members, and the loss was observable neither from the response nor from a
// read-back. That failure is invisible to the fake client, which performs no
// admission and prunes nothing, so a fake-client test would have passed against
// the broken state throughout. Only a real API server can tell "the field is in
// the schema" from "the field is silently discarded", which is why the first test
// below asserts a ROUND-TRIP rather than a successful Create.
//
// The two CEL rules are likewise invisible to the fake client: a typo in a rule,
// a marker on the wrong type, or a rule dropped by CRD regeneration all behave
// exactly like a correct rule there — the object is accepted either way and
// nothing ever says so.

// compositeZone builds a FleetZone grouping the named members.
func compositeZone(ns, name string, members ...string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.FleetZoneSpec{
			DisplayName: name,
			MemberZones: members,
		},
	}
}

// ---------------------------------------------------------------------------
// The field exists and survives a round trip. This is the test that would have
// failed before this change — and failed at the READ, not at the write.
// ---------------------------------------------------------------------------

func TestEnvtest_FleetZoneMemberZonesRoundTrip(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fz := compositeZone(ns, "ward-2a", "room-201", "room-202", "room-203")
	if err := envK8s.Create(ctx, fz); err != nil {
		t.Fatalf("the API server rejected a composite zone declaring three members: %v", err)
	}

	var got fleetv1.FleetZone
	if err := envK8s.Get(ctx, zoneKey(ns, "ward-2a"), &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.Spec.MemberZones) != 3 {
		t.Fatalf("spec.memberZones did not round-trip: applied 3 members, read back %d (%v). "+
			"This is the exact failure RFC-0001 recorded as 'a write to this path is pruned by "+
			"the API server': the apply succeeds, the members are discarded, and the zone expands "+
			"to nothing while an operator believes they have grouped a floor",
			len(got.Spec.MemberZones), got.Spec.MemberZones)
	}
	for _, want := range []string{"room-201", "room-202", "room-203"} {
		if !containsStr(got.Spec.MemberZones, want) {
			t.Errorf("member %q missing after round trip: %v", want, got.Spec.MemberZones)
		}
	}
}

// ---------------------------------------------------------------------------
// FZ-C4 — a composite must declare no sharedResources. Resource resolution walks
// containment edges only, so an entry here is unreachable: it means something the
// control plane will never do.
// ---------------------------------------------------------------------------

func TestEnvtest_CompositeZoneRejectsSharedResources(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fz := compositeZone(ns, "ward-2b", "room-211")
	fz.Spec.SharedResources = []fleetv1.SharedResource{{
		Name:     "main-aisle",
		Type:     fleetv1.SharedResourceCorridor,
		Capacity: 1,
	}}

	err := envK8s.Create(ctx, fz)
	if err == nil {
		t.Fatalf("the API server ACCEPTED a composite zone declaring a sharedResource. " +
			"FZ-C4 says resource resolution walks containment edges only, so that entry is " +
			"unreachable — an operator would have declared a corridor reservation that nothing " +
			"can ever resolve, and no error would have told them")
	}
	const want = "spec.sharedResources is not permitted on a composite zone"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("rejected, but not by the FZ-C4 rule: want a message containing %q, got: %v", want, err)
	}
}

// ---------------------------------------------------------------------------
// FZ-C6 — a composite must have maxConcurrentRobots 0. Capacity is enforced where
// robots are, and a robot is never IN a composite (FZ-C2).
// ---------------------------------------------------------------------------

func TestEnvtest_CompositeZoneRejectsMaxConcurrentRobots(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fz := compositeZone(ns, "ward-2c", "room-221")
	fz.Spec.MaxConcurrentRobots = 4

	err := envK8s.Create(ctx, fz)
	if err == nil {
		t.Fatalf("the API server ACCEPTED a composite zone with maxConcurrentRobots=4. " +
			"A composite holds no robots directly, so that cap would throttle nothing while " +
			"reading, to an operator, like a configured limit")
	}
	const want = "spec.maxConcurrentRobots must be 0 on a composite zone"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("rejected, but not by the FZ-C6 rule: want a message containing %q, got: %v", want, err)
	}
}

// ---------------------------------------------------------------------------
// THE REGRESSION GUARD. Both rules are conditioned on memberZones being non-empty,
// and a rule that fired on a NON-composite would break every zone in every existing
// deployment — the ordinary case is precisely a zone with a capacity cap and a
// corridor. This is the test that fails if either rule's guard is dropped.
// ---------------------------------------------------------------------------

func TestEnvtest_NonCompositeZoneKeepsCapacityAndSharedResources(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fz := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse-a", Namespace: ns},
		Spec: fleetv1.FleetZoneSpec{
			DisplayName:         "Receiving and staging area",
			MaxConcurrentRobots: 4,
			SharedResources: []fleetv1.SharedResource{{
				Name:     "main-aisle",
				Type:     fleetv1.SharedResourceCorridor,
				Capacity: 1,
			}},
		},
	}
	if err := envK8s.Create(ctx, fz); err != nil {
		t.Fatalf("the API server rejected an ORDINARY zone — a capacity cap and a shared "+
			"corridor, no memberZones anywhere. Both composite rules must be guarded on a "+
			"non-empty memberZones; an unguarded rule breaks every zone already deployed: %v", err)
	}

	var got fleetv1.FleetZone
	if err := envK8s.Get(ctx, zoneKey(ns, "warehouse-a"), &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.Spec.MemberZones) != 0 {
		t.Errorf("a zone that declared no members came back with %v — absence must stay "+
			"absence; nothing may default a group into existence", got.Spec.MemberZones)
	}
}

// ---------------------------------------------------------------------------
// The derived block round-trips through the status subresource, including the
// unresolved-member reason enum. status.composition is what a consumer reads
// instead of re-expanding the graph itself.
// ---------------------------------------------------------------------------

func TestEnvtest_ZoneCompositionStatusRoundTrip(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	fz := compositeZone(ns, "ward-2d", "room-231", "room-232")
	if err := envK8s.Create(ctx, fz); err != nil {
		t.Fatalf("create: %v", err)
	}

	fz.Status.Composition = &fleetv1.ZoneComposition{
		IsComposite:   true,
		ExpandedZones: []string{"room-231"},
		UnresolvedMembers: []fleetv1.UnresolvedMember{
			{Name: "room-232", Reason: fleetv1.MemberNotFound},
		},
	}
	if err := envK8s.Status().Update(ctx, fz); err != nil {
		t.Fatalf("status update carrying a composition block was rejected: %v", err)
	}

	var got fleetv1.FleetZone
	if err := envK8s.Get(ctx, zoneKey(ns, "ward-2d"), &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	c := got.Status.Composition
	if c == nil {
		t.Fatalf("status.composition was discarded — the derived state a consumer reads " +
			"instead of re-expanding the graph is not in the schema")
	}
	if !c.IsComposite || len(c.ExpandedZones) != 1 || len(c.UnresolvedMembers) != 1 {
		t.Fatalf("composition did not round-trip intact: %#v", c)
	}
	if c.UnresolvedMembers[0].Reason != fleetv1.MemberNotFound {
		t.Errorf("unresolved reason did not round-trip: got %q, want %q — the reason is how a "+
			"safety consumer tells a deleted room from a cycle, and both from a depth bound",
			c.UnresolvedMembers[0].Reason, fleetv1.MemberNotFound)
	}
}

// containsStr reports whether s contains want.
func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
