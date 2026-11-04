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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Filter 12's widened scope on the dispatch path, against a real API server
// (ADR-0058 Amendment 2).
//
// The unit tests beside this decide who is withheld at which leaf; these prove the
// reconciler acts on it — that a rule declared BELOW the target actually stops the
// dispatch, that a candidate which can honour that same leaf is still dispatched, and
// that a sibling leaf carrying no rule is untouched.

// f12SeedICU seeds ADR-0058 Amendment 2 §A2.1's table: ward declares nothing, ward-icu denies
// camera.
func f12SeedICU(t *testing.T, ns string) {
	t.Helper()
	zrSeedZone(t, ns, "ward", "", nil, []fleetv1.EffectiveZoneRestriction{}, true)
	zrSeedZone(t, ns, "ward-icu", "ward",
		&fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied, Reason: "patient privacy"},
		}},
		[]fleetv1.EffectiveZoneRestriction{{
			Axis: fleetv1.ZoneAxisOperating, Subject: "camera",
			Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "ward-icu", Reason: "patient privacy",
		}}, true)
}

// THE ICU CASE, end to end. An action targets ward, which declares nothing. amr-1 sits
// in ward-icu, which denies a camera, and its adapter declares no operating subjects.
// Filter 11 admits the action, filter 2 admits amr-1 from the leaf set — and filter 12
// must withhold it.
func TestEnvtest_Filter12_ARuleOnADescendantLeafStopsTheDispatch(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	f12SeedICU(t, ns)
	f12SeedRobot(t, ns, "amr-1", "ward-icu") // declares nothing

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := zrGetAction(t, req)
	if got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q. A ROBOT WAS DISPATCHED INTO THE ICU WITH A CAMERA: the "+
			"action targets ward, which declares nothing; ward-icu denies camera; amr-1 is in "+
			"ward-icu and its adapter declares no operating subjects. Filter 2 admitted it from "+
			"the leaf set and filter 12 read ward's empty projection and withheld nobody "+
			"(ADR-0058 Amendment 2 §A2.1). Filter 12's scope is the RESOLVED LEAF SET, not the "+
			"target zone chain",
			got.Status.AssignedRobot)
	}
	if got.Status.Phase != fleetv1.ActionPhasePending {
		t.Errorf("phase = %q, want Pending: this is a fact about today's fleet, not a standing "+
			"refusal", got.Status.Phase)
	}
	if got.Status.FailureReason != fleetv1.ActionFailureZoneOperatingConstraintUnmet {
		t.Errorf("failureReason = %q, want %q", got.Status.FailureReason,
			fleetv1.ActionFailureZoneOperatingConstraintUnmet)
	}
	// The refusal must name WHICH SUBJECT and WHICH ZONES, the way filter 11's
	// subtree-exhausted message names every narrowed zone. An operator who cannot tell
	// which closet caused it will go hunting for an adapter setting.
	for _, want := range []string{"camera", "ward-icu", "patient privacy", "HELD, not judged"} {
		if !strings.Contains(got.Status.Message, want) {
			t.Errorf("status.message %q does not carry %q. The rule is declared BELOW the "+
				"target, so naming only the target tells the operator nothing about which zone "+
				"to look at", got.Status.Message, want)
		}
	}
}

// The leaf is dropped for the candidate that cannot honour it and KEPT for the one
// that can. Filter 11's narrowing is zone-scoped — a denied leaf is out for everyone.
// This is not: a leaf leaves the reachable set only when NO candidate can honour it.
func TestEnvtest_Filter12_ALeafDroppedForOneCandidateIsKeptForAnother(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	f12SeedICU(t, ns)
	f12SeedRobot(t, ns, "amr-1", "ward-icu")           // declares nothing — withheld here
	f12SeedRobot(t, ns, "amr-2", "ward-icu", "camera") // declares camera — a candidate here

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := zrGetAction(t, req)
	if got.Status.AssignedRobot != "amr-2" {
		t.Fatalf("assignedRobot = %q, want amr-2 (phase %q, reason %q, message %q). The SAME "+
			"leaf drops out for amr-1 and is kept for amr-2: filter 12 narrows per (candidate, "+
			"leaf), never per zone. Removing ward-icu for everyone because one robot cannot "+
			"serve it would be filter 11 with extra steps",
			got.Status.AssignedRobot, got.Status.Phase, got.Status.FailureReason, got.Status.Message)
	}
}

// NOT A UNION. A candidate in a sibling leaf that carries no rule is dispatched, even
// though ward-icu denies a camera. Unioning every leaf's projection would make ward
// undispatchable for a rule on one closet.
func TestEnvtest_Filter12_ASiblingLeafWithNoRuleStillDispatches(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	f12SeedICU(t, ns)
	zrSeedZone(t, ns, "ward-store", "ward", nil, []fleetv1.EffectiveZoneRestriction{}, true)
	f12SeedRobot(t, ns, "amr-3", "ward-store") // declares nothing, and needs to declare nothing

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := zrGetAction(t, req)
	if got.Status.AssignedRobot != "amr-3" {
		t.Fatalf("assignedRobot = %q, want amr-3 (phase %q, reason %q, message %q). A rule on "+
			"ward-icu withheld a candidate standing in ward-store, which carries no operating "+
			"rule at all. That is the union option, rejected on the record: it makes a large "+
			"target zone undispatchable for a rule on one closet",
			got.Status.AssignedRobot, got.Status.Phase, got.Status.FailureReason, got.Status.Message)
	}
}

// EVERY LEAF DROPS OUT. Two leaves, each restricting a different subject, and no
// candidate can honour either. The refusal is ZoneOperatingConstraintUnmet and it stays
// Pending — never Failed — and it must name WHICH SUBJECT and WHICH ZONES, the way
// filter 11's subtree-exhausted message names every narrowed zone. An operator who
// cannot tell which closet caused it will go hunting for an adapter setting.
func TestEnvtest_Filter12_SubtreeExhaustedNamesTheSubjectAndTheZones(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	f12SeedICU(t, ns) // ward declares nothing; ward-icu denies camera
	zrSeedZone(t, ns, "ward-store", "ward",
		&fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "microphone", Mode: fleetv1.ZoneRestrictionDenied, Reason: "dictation room"},
		}},
		[]fleetv1.EffectiveZoneRestriction{{
			Axis: fleetv1.ZoneAxisOperating, Subject: "microphone",
			Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "ward-store", Reason: "dictation room",
		}}, true)
	f12SeedRobot(t, ns, "amr-1", "ward-icu")   // declares nothing
	f12SeedRobot(t, ns, "amr-2", "ward-store") // declares nothing

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := zrGetAction(t, req)
	t.Logf("status.message = %s", got.Status.Message)

	if got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q; every leaf of ward restricts a subject no candidate declares",
			got.Status.AssignedRobot)
	}
	if got.Status.Phase != fleetv1.ActionPhasePending || got.Status.FailedAt != nil {
		t.Errorf("phase = %q, failedAt = %v; want Pending and never Failed — a subtree exhausted "+
			"by filter 12 is a fact about today's fleet, unlike filter 11's, which is terminal",
			got.Status.Phase, got.Status.FailedAt)
	}
	if got.Status.FailureReason != fleetv1.ActionFailureZoneOperatingConstraintUnmet {
		t.Errorf("failureReason = %q, want %q", got.Status.FailureReason,
			fleetv1.ActionFailureZoneOperatingConstraintUnmet)
	}
	for _, want := range []string{
		"camera", "ward-icu", "patient privacy",
		"microphone", "ward-store", "dictation room",
		"in force in", "amr-1", "amr-2", "HELD, not judged",
		"the control plane does no path planning",
	} {
		if !strings.Contains(got.Status.Message, want) {
			t.Errorf("status.message does not carry %q:\n%s", want, got.Status.Message)
		}
	}

	// RA-1: the message rides a status patch a Pending action repeats on every requeue,
	// so an unchanged fleet and an unchanged zone tree must produce the same bytes.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if again := zrGetAction(t, req); again.Status.Message != got.Status.Message {
		t.Errorf("the message is not stable across requeues, so every reconcile is an etcd "+
			"write (RA-1):\n first: %s\nsecond: %s", got.Status.Message, again.Status.Message)
	}
}
