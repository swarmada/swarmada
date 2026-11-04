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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Filter 11 on the dispatch path, against a real API server.
//
// The unit tests decide the VERDICT; these prove the reconciler acts on it — that a
// terminal refusal reaches status.phase and status.failureReason, that a
// non-terminal one holds the action Pending, and that the terminal/Pending split is
// visible on the object an operator reads rather than only in a log line.

// zrSeedZone creates a FleetZone with a declared restriction and the matching
// resolved projection, the state the Zone Controller would leave it in.
func zrSeedZone(t *testing.T, ns, name, parent string,
	declared *fleetv1.ZoneRestrictions, effective []fleetv1.EffectiveZoneRestriction, resolved bool,
) {
	t.Helper()
	ctx := context.Background()
	fz := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent, Restrictions: declared},
	}
	if err := envK8s.Create(ctx, fz); err != nil {
		t.Fatalf("create zone %q: %v", name, err)
	}
	status := metav1.ConditionTrue
	msg := "resolved"
	if !resolved {
		status = metav1.ConditionFalse
		msg = "ancestor chain does not resolve"
	}
	fz.Status.Restrictions = &fleetv1.ZoneRestrictionsStatus{Effective: effective}
	fz.Status.Conditions = []metav1.Condition{{
		Type: fleetv1.ZoneCondRestrictionsResolved, Status: status,
		Reason: "Test", Message: msg, LastTransitionTime: metav1.Now(),
	}}
	if err := envK8s.Status().Update(ctx, fz); err != nil {
		t.Fatalf("seed zone status %q: %v", name, err)
	}
}

func zrSeedAction(t *testing.T, ns, name, zone string, caps ...string) ctrl.Request {
	t.Helper()
	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeNavigate, Zone: zone,
			Priority: fleetv1.ActionPriorityNormal, RequiredCapabilities: caps,
		},
	}
	if err := envK8s.Create(context.Background(), a); err != nil {
		t.Fatalf("create action: %v", err)
	}
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
}

func zrGetAction(t *testing.T, req ctrl.Request) *fleetv1.FleetAction {
	t.Helper()
	var a fleetv1.FleetAction
	if err := envK8s.Get(context.Background(), req.NamespacedName, &a); err != nil {
		t.Fatalf("get action: %v", err)
	}
	return &a
}

// A standing denial fails the action TERMINALLY, with the reason on the object.
func TestEnvtest_Filter11_EntryDeniedFailsTheActionTerminally(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	denial := &fleetv1.ZoneRestrictions{Entry: &fleetv1.ZoneEntryRestriction{
		Mode: fleetv1.ZoneRestrictionDenied, Reason: "outdoors behind the fence",
	}}
	zrSeedZone(t, ns, "patio", "", denial, []fleetv1.EffectiveZoneRestriction{{
		Axis: fleetv1.ZoneAxisEntry, Mode: fleetv1.ZoneRestrictionDenied,
		InheritedFrom: "patio", Reason: "outdoors behind the fence",
	}}, true)

	req := zrSeedAction(t, ns, "act-1", "patio")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := zrGetAction(t, req)
	if got.Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %q, want Failed. Filter 11 is action-scoped and its refusal is "+
			"TERMINAL: leaving a standing, permanent refusal Pending would make \"you told me "+
			"never to go there\" indistinguishable from \"the fleet is busy\"", got.Status.Phase)
	}
	if got.Status.FailureReason != fleetv1.ActionFailureZoneEntryDenied {
		t.Errorf("failureReason = %q, want %q", got.Status.FailureReason, fleetv1.ActionFailureZoneEntryDenied)
	}
	if got.Status.FailedAt == nil {
		t.Error("failedAt was not stamped on a terminal refusal")
	}
	if got.Status.RetryCount != 0 {
		t.Errorf("retryCount = %d, want 0 — nothing about retrying changes a standing rule, and "+
			"inflating the count makes an operator read a policy refusal as a flaky robot",
			got.Status.RetryCount)
	}
	for _, want := range []string{"patio", "outdoors behind the fence", "on its own or as part of anything else"} {
		if !strings.Contains(got.Status.Message, want) {
			t.Errorf("status.message %q does not carry %q", got.Status.Message, want)
		}
	}
}

// An UNRESOLVED chain holds the action Pending. Not Failed: it is a fact about today.
func TestEnvtest_Filter11_UnresolvedChainHoldsPendingNotFailed(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	denial := &fleetv1.ZoneRestrictions{Entry: &fleetv1.ZoneEntryRestriction{Mode: fleetv1.ZoneRestrictionDenied}}
	// A zone that USES restrictions, so the filter is live in this namespace...
	zrSeedZone(t, ns, "locked", "", denial, []fleetv1.EffectiveZoneRestriction{{
		Axis: fleetv1.ZoneAxisEntry, Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "locked",
	}}, true)
	// ...and the target, whose own chain does not resolve.
	zrSeedZone(t, ns, "orphan", "", nil, nil, false)

	req := zrSeedAction(t, ns, "act-2", "orphan")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := zrGetAction(t, req)
	if got.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %q, want Pending. ZoneRestrictionsUnresolved is a fact about today — "+
			"a FleetZone that becomes readable makes the same action dispatchable — so the "+
			"action waits and fails on its deadline like any other unsatisfied Pending",
			got.Status.Phase)
	}
	if got.Status.FailureReason != "" {
		t.Errorf("failureReason = %q on a Pending action; the field is set when the phase "+
			"transitions to Failed and empty otherwise", got.Status.FailureReason)
	}
	low := strings.ToLower(got.Status.Message)
	if !strings.Contains(low, "no decision") {
		t.Errorf("status.message %q must say NO DECISION HAS BEEN MADE — the action is held, "+
			"not judged, and reporting it as a denial would claim the operator forbade "+
			"something they did not", got.Status.Message)
	}
}

// A capability denial fails terminally and says the ZONE is still open.
func TestEnvtest_Filter11_CapabilityDeniedDoesNotCloseTheZone(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	declared := &fleetv1.ZoneRestrictions{Capabilities: []fleetv1.ZoneCapabilityRestriction{{
		Name: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied, Reason: "carpet that must not get wet",
	}}}
	zrSeedZone(t, ns, "office", "", declared, []fleetv1.EffectiveZoneRestriction{{
		Axis: fleetv1.ZoneAxisCapability, Subject: "clean.mop",
		Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "office",
		Reason: "carpet that must not get wet",
	}}, true)

	req := zrSeedAction(t, ns, "act-mop", "office", "clean.mop")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := zrGetAction(t, req)
	if got.Status.Phase != fleetv1.ActionPhaseFailed ||
		got.Status.FailureReason != fleetv1.ActionFailureZoneCapabilityDenied {
		t.Fatalf("phase=%q reason=%q, want Failed/%s", got.Status.Phase,
			got.Status.FailureReason, fleetv1.ActionFailureZoneCapabilityDenied)
	}
	if !strings.Contains(got.Status.Message, "NOT CLOSED") {
		t.Errorf("status.message %q does not say the zone is NOT CLOSED. A capability denial "+
			"that reads like an entry denial costs the operator the room", got.Status.Message)
	}

	// The control: a different capability in the same zone is NOT refused by filter 11.
	req2 := zrSeedAction(t, ns, "act-vac", "office", "clean.vacuum")
	if _, err := r.Reconcile(context.Background(), req2); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got2 := zrGetAction(t, req2); got2.Status.FailureReason == fleetv1.ActionFailureZoneCapabilityDenied {
		t.Errorf("an action needing an unrestricted capability was refused as ZoneCapabilityDenied "+
			"in a zone that denies only clean.mop: %q", got2.Status.Message)
	}
}

// The narrowed subtree is NAMED on the action's status, and the action proceeds.
func TestEnvtest_Filter11_NarrowedSubtreeIsNamedOnStatus(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	zrSeedZone(t, ns, "floor-2", "", nil, nil, true)
	zrSeedZone(t, ns, "office", "floor-2", nil, nil, true)
	denial := &fleetv1.ZoneRestrictions{Entry: &fleetv1.ZoneEntryRestriction{
		Mode: fleetv1.ZoneRestrictionDenied, Reason: "the robot cannot get out",
	}}
	zrSeedZone(t, ns, "under-bed-strip", "floor-2", denial, []fleetv1.EffectiveZoneRestriction{{
		Axis: fleetv1.ZoneAxisEntry, Mode: fleetv1.ZoneRestrictionDenied,
		InheritedFrom: "under-bed-strip", Reason: "the robot cannot get out",
	}}, true)

	req := zrSeedAction(t, ns, "act-floor", "floor-2")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := zrGetAction(t, req)
	if got.Status.Phase == fleetv1.ActionPhaseFailed {
		t.Fatalf("a denial on a DESCENDANT failed the whole action: %q. Refusing a whole floor "+
			"because one closet inside it is denied would make the floor undispatchable for a "+
			"reason no operator can act on", got.Status.Message)
	}
	if !strings.Contains(got.Status.Message, "under-bed-strip") {
		t.Errorf("status.message %q does not name the excluded zone. Trimming silently is how "+
			"an operator comes to believe a region was serviced that was never entered",
			got.Status.Message)
	}
}
