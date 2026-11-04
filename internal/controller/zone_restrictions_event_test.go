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

	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func restrictionsChangedEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if strings.Contains(e, zoneReasonRestrictionsChanged) {
			out = append(out, e)
		}
	}
	return out
}

func setZoneRestrictions(t *testing.T, z *ZoneController, name string, r *fleetv1.ZoneRestrictions) {
	t.Helper()
	ctx := context.Background()
	var fz fleetv1.FleetZone
	if err := z.Get(ctx, nsName(name), &fz); err != nil {
		t.Fatalf("get zone %s: %v", name, err)
	}
	fz.Spec.Restrictions = r
	fz.Generation++ // a spec write advances it; the fake client does not
	if err := z.Update(ctx, &fz); err != nil {
		t.Fatalf("update zone %s: %v", name, err)
	}
}

func denyEntry(reason string) *fleetv1.ZoneRestrictions {
	return &fleetv1.ZoneRestrictions{Entry: &fleetv1.ZoneEntryRestriction{
		Mode: fleetv1.ZoneRestrictionDenied, Reason: reason,
	}}
}

// Every change to the restrictions in force at a zone is announced once, for audit, and
// an unchanged pass announces nothing.
func TestZoneRestrictionsChanged_OnChangeOnly(t *testing.T) {
	z, _ := newZoneController(t, zoneObj("ward-3", ""))

	reconcileZone(t, z, "ward-3")
	if got := restrictionsChangedEvents(drainEvents(t, z)); len(got) != 0 {
		t.Fatalf("a zone with no restriction anywhere announced %v; an empty projection is not a change", got)
	}

	setZoneRestrictions(t, z, "ward-3", denyEntry("wet floor"))
	reconcileZone(t, z, "ward-3")
	got := restrictionsChangedEvents(drainEvents(t, z))
	if len(got) != 1 {
		t.Fatalf("%d ZoneRestrictionsChanged events after a denial was declared, want 1: %v", len(got), got)
	}
	for _, want := range []string{"Normal", "ward-3", "[none]", "Entry Denied from ward-3"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("event %q does not carry %q", got[0], want)
		}
	}

	// RA-1: a resync of the same spec is not a change.
	reconcileZone(t, z, "ward-3")
	reconcileZone(t, z, "ward-3")
	if got := restrictionsChangedEvents(drainEvents(t, z)); len(got) != 0 {
		t.Fatalf("an unchanged pass announced %v", got)
	}

	setZoneRestrictions(t, z, "ward-3", nil)
	reconcileZone(t, z, "ward-3")
	got = restrictionsChangedEvents(drainEvents(t, z))
	if len(got) != 1 || !strings.Contains(got[0], "to [none]") {
		t.Fatalf("lifting the denial announced %v, want one event ending in [none]", got)
	}
}

// A zone whose rules change because an ANCESTOR was edited is told: it is the zone an
// operator inspects when work stops going there.
func TestZoneRestrictionsChanged_InheritedChangeIsAnnouncedOnTheDescendant(t *testing.T) {
	z, _ := newZoneController(t, zoneObj("hospital", ""), zoneObj("store-room", "hospital"))
	reconcileZone(t, z, "hospital")
	reconcileZone(t, z, "store-room")
	drainEvents(t, z)

	setZoneRestrictions(t, z, "hospital", &fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
		{Subject: "uv", Mode: fleetv1.ZoneRestrictionLimited, Max: "5", Reason: "packaging degrades"},
	}})
	reconcileZone(t, z, "hospital")
	reconcileZone(t, z, "store-room")

	var onStore []string
	for _, e := range restrictionsChangedEvents(drainEvents(t, z)) {
		if strings.Contains(e, `zone "store-room"`) {
			onStore = append(onStore, e)
		}
	}
	if len(onStore) != 1 || !strings.Contains(onStore[0], "Operating/uv Limited max=5 from hospital") {
		t.Fatalf("store-room announced %v, want one event naming the inherited rule and its source", onStore)
	}
}

// A restarted manager compares against the PUBLISHED projection, so its first pass over
// an unchanged zone announces nothing.
func TestZoneRestrictionsChanged_RestartAnnouncesNothing(t *testing.T) {
	z, c := newZoneController(t, zoneObj("ward-3", ""))
	setZoneRestrictions(t, z, "ward-3", denyEntry("wet floor"))
	reconcileZone(t, z, "ward-3")
	drainEvents(t, z)

	restarted := &ZoneController{Client: c, Scheme: z.Scheme, Recorder: record.NewFakeRecorder(32)}
	reconcileZone(t, restarted, "ward-3")
	if got := restrictionsChangedEvents(drainEvents(t, restarted)); len(got) != 0 {
		t.Fatalf("a restarted manager announced %v for a zone whose restrictions did not change", got)
	}
}
