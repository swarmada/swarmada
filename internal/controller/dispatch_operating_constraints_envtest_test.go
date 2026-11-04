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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Filter 12 on the dispatch path, against a real API server.
//
// The unit tests decide who is withheld; these prove the reconciler acts on it — that the action
// reaches Pending and NOT Failed, that status.failureReason carries
// ZoneOperatingConstraintUnmet, that retryCount is left alone, and that the same fleet with a
// declaring adapter dispatches. That last half is the control: without it a passing test could
// mean "the fixture never dispatched anyway".

// f12SeedRobot creates an Idle, in-zone, recently-seen robot declaring the given subjects.
func f12SeedRobot(t *testing.T, ns, name, zone string, honoured ...string) {
	t.Helper()
	ctx := context.Background()
	robot := envtestValidRobot(ns, name, zone)
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("create robot %q: %v", name, err)
	}
	robot.Status.Phase = fleetv1.RobotPhaseIdle
	// Candidate filter 2 matches status.currentZone, not spec.zone.
	robot.Status.CurrentZone = robot.Spec.Zone
	robot.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: &metav1.Time{Time: time.Now()}}
	robot.Status.HonouredOperatingConstraints = honoured
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("seed robot status %q: %v", name, err)
	}
}

// f12SeedAdapter creates a Connected, conformance-Passed adapter bound to the current contract,
// so a refusal can only come from filter 12.
func f12SeedAdapter(t *testing.T, ns string) {
	t.Helper()
	if err := envK8s.Create(context.Background(), gateEnvAdapter(ns, "acme-adapter")); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	gateEnvSeedAdapterStatus(t, ns, "acme-adapter", contract.Version)
}

// f12CameraZone seeds a zone denying `camera`, declared and resolved, as the Zone Controller
// would leave it.
func f12CameraZone(t *testing.T, ns, name string) {
	t.Helper()
	zrSeedZone(t, ns, name, "",
		&fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied, Reason: "patient privacy"},
		}},
		[]fleetv1.EffectiveZoneRestriction{{
			Axis: fleetv1.ZoneAxisOperating, Subject: "camera",
			Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: name, Reason: "patient privacy",
		}}, true)
}

// The load-bearing case: every candidate's adapter declares nothing, so the action is HELD.
// Pending, not Failed — a conforming adapter deployed tomorrow makes the same action
// dispatchable, and that is a different sentence from "you told me never to go there".
func TestEnvtest_Filter12_UndeclaredSubjectHoldsTheActionPendingNotFailed(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	f12CameraZone(t, ns, "ward-3")
	f12SeedRobot(t, ns, "amr-1", "ward-3") // declares nothing — an adapter that predates the field

	req := zrSeedAction(t, ns, "act-1", "ward-3")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := zrGetAction(t, req)
	if got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q; a robot whose adapter declares no operating subject was "+
			"dispatched into a zone that denies a camera", got.Status.AssignedRobot)
	}
	if got.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %q, want Pending. ZoneOperatingConstraintUnmet is a FACT ABOUT TODAY, "+
			"not a standing decision: a conforming adapter deployed tomorrow makes this same "+
			"action dispatchable, so it waits and fails on its deadline like any other "+
			"unsatisfied Pending", got.Status.Phase)
	}
	if got.Status.FailedAt != nil {
		t.Errorf("failedAt was stamped on a held action: %v", got.Status.FailedAt)
	}
	if got.Status.FailureReason != fleetv1.ActionFailureZoneOperatingConstraintUnmet {
		t.Errorf("failureReason = %q, want %q — collapsing this into the generic no-candidate "+
			"message costs the operator the difference between \"no robot here can honour the "+
			"rule\" and \"the fleet is busy\"",
			got.Status.FailureReason, fleetv1.ActionFailureZoneOperatingConstraintUnmet)
	}
	// retryCount treatment follows filter 11's: nothing about retrying changes what the
	// fleet can honour, and inflating the count makes an operator read a policy hold as a
	// flaky robot.
	if got.Status.RetryCount != 0 {
		t.Errorf("retryCount = %d, want 0", got.Status.RetryCount)
	}
	for _, want := range []string{"camera", "ward-3", "patient privacy", "HELD, not judged"} {
		if !strings.Contains(got.Status.Message, want) {
			t.Errorf("status.message %q does not carry %q. An operator must be able to tell WHICH "+
				"SUBJECT and WHICH ZONE, and that the action is held rather than judged, or they "+
				"go hunting for a setting", got.Status.Message, want)
		}
	}
}

// The control. The same fleet, with the adapter declaring the subject, dispatches.
func TestEnvtest_Filter12_ADeclaringRobotIsDispatched(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	f12CameraZone(t, ns, "ward-3")
	f12SeedRobot(t, ns, "amr-1", "ward-3", "camera")

	req := zrSeedAction(t, ns, "act-1", "ward-3")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := zrGetAction(t, req)
	if got.Status.AssignedRobot == "" {
		t.Fatalf("a robot DECLARING camera was not dispatched into ward-3 (phase %q, reason %q, "+
			"message %q). Without this half the withholding test could pass because the fixture "+
			"never dispatched at all", got.Status.Phase, got.Status.FailureReason, got.Status.Message)
	}
}

// THE INERT GATE, on the dispatch path. A namespace with zone geometry and NO restriction block
// anywhere dispatches a non-declaring robot exactly as it did before ADR-0058.
func TestEnvtest_Filter12_IsInertInANamespaceWithNoRestrictions(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "ward-3", "", nil, nil, true) // geometry, no restrictions
	f12SeedRobot(t, ns, "amr-1", "ward-3")          // declares nothing, as every adapter today does

	req := zrSeedAction(t, ns, "act-1", "ward-3")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := zrGetAction(t, req)
	if got.Status.AssignedRobot == "" {
		t.Fatalf("a namespace that declares NO restriction withheld a robot (phase %q, reason %q, "+
			"message %q). Every adapter in existence today declares nothing; without the inert "+
			"gate this filter is a permanent lockout for every deployment that never asked for "+
			"any of it", got.Status.Phase, got.Status.FailureReason, got.Status.Message)
	}
}

// Robot-scoped, end to end: ONE robot, TWO zones, withheld from one and dispatched in the other.
// Filter 11 could not produce this, which is why both filters exist.
func TestEnvtest_Filter12_SameRobotWithheldHereAndDispatchedThere(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	f12CameraZone(t, ns, "ward-3")
	// loading-bay restricts NOISE, which this robot declares.
	zrSeedZone(t, ns, "loading-bay", "",
		&fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "noise", Mode: fleetv1.ZoneRestrictionLimited, Max: "65", Reason: "night shift"},
		}},
		[]fleetv1.EffectiveZoneRestriction{{
			Axis: fleetv1.ZoneAxisOperating, Subject: "noise",
			Mode: fleetv1.ZoneRestrictionLimited, Max: "65",
			InheritedFrom: "loading-bay", Reason: "night shift",
		}}, true)

	// One robot in ward-3 declaring only noise, one in loading-bay declaring only noise.
	f12SeedRobot(t, ns, "amr-ward", "ward-3", "noise")
	f12SeedRobot(t, ns, "amr-bay", "loading-bay", "noise")

	wardReq := zrSeedAction(t, ns, "act-ward", "ward-3")
	if _, err := r.Reconcile(ctx, wardReq); err != nil {
		t.Fatalf("reconcile ward: %v", err)
	}
	if a := zrGetAction(t, wardReq); a.Status.AssignedRobot != "" {
		t.Fatalf("a robot declaring only noise was dispatched into ward-3, which denies camera: %q",
			a.Status.AssignedRobot)
	}

	bayReq := zrSeedAction(t, ns, "act-bay", "loading-bay")
	if _, err := r.Reconcile(ctx, bayReq); err != nil {
		t.Fatalf("reconcile bay: %v", err)
	}
	if a := zrGetAction(t, bayReq); a.Status.AssignedRobot == "" {
		t.Fatalf("the same declaration that was insufficient in ward-3 was also refused in "+
			"loading-bay, whose only rule is the noise it declares (phase %q, reason %q, message %q). "+
			"Filter 12 is ROBOT-SCOPED and per-zone; withholding globally would be filter 11 with "+
			"extra steps", a.Status.Phase, a.Status.FailureReason, a.Status.Message)
	}
}

// A namespace that uses restrictions but puts no OPERATING rule on this action's chain must not
// have its candidates filtered by 12 — the gate is "is this axis in force here", not "does this
// namespace use restrictions at all".
func TestEnvtest_Filter12_EntryOnlyRestrictionsDoNotWithholdCandidates(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}

	f12SeedAdapter(t, ns)
	// patio denies ENTRY; ward-3 declares nothing. The namespace uses restrictions.
	zrSeedZone(t, ns, "patio", "",
		&fleetv1.ZoneRestrictions{Entry: &fleetv1.ZoneEntryRestriction{
			Mode: fleetv1.ZoneRestrictionDenied, Reason: "outdoors behind the fence"}},
		[]fleetv1.EffectiveZoneRestriction{{
			Axis: fleetv1.ZoneAxisEntry, Mode: fleetv1.ZoneRestrictionDenied,
			InheritedFrom: "patio", Reason: "outdoors behind the fence",
		}}, true)
	zrSeedZone(t, ns, "ward-3", "", nil, []fleetv1.EffectiveZoneRestriction{}, true)
	f12SeedRobot(t, ns, "amr-1", "ward-3") // declares nothing

	req := zrSeedAction(t, ns, "act-1", "ward-3")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if a := zrGetAction(t, req); a.Status.AssignedRobot == "" {
		t.Fatalf("an ENTRY denial on an unrelated zone withheld a candidate from ward-3 "+
			"(phase %q, reason %q, message %q). Filter 12 fires on the operating subjects in "+
			"force on THIS action's chain, not on whether the namespace uses restrictions at all",
			a.Status.Phase, a.Status.FailureReason, a.Status.Message)
	}
	_ = types.NamespacedName{}
	_ = ctrl.Request{}
}
