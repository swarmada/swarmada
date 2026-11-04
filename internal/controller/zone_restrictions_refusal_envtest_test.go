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

// The zone-restrictions profile's ControlPlane rows CP7.1 and CP7.2 are read off the
// shipped schema: they prove a restriction can be WRITTEN. These prove it is ENFORCED:
// a restriction declared on an ancestor, resolved by the real Zone Controller rather
// than seeded, refuses at dispatch an action targeting a descendant, before any robot is
// assigned, with the reason on the action's status. A control namespace with no rule
// assigns the same robot, so the refusal is the check and not an unfit fleet.

// cp7Floor creates site → site-bay through the API server, with rules on site only, and
// runs the Zone Controller over both so the projection filter 11 reads is the one the
// plane itself resolves.
func cp7Floor(t *testing.T, ns string, siteRules *fleetv1.ZoneRestrictions) {
	t.Helper()
	ctx := context.Background()
	for _, fz := range []*fleetv1.FleetZone{
		{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: ns},
			Spec: fleetv1.FleetZoneSpec{Restrictions: siteRules}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-bay", Namespace: ns},
			Spec: fleetv1.FleetZoneSpec{ParentZone: "site"}},
	} {
		if err := envK8s.Create(ctx, fz); err != nil {
			t.Fatalf("create zone %q: %v", fz.Name, err)
		}
	}
	zc := &ZoneController{Client: envK8s, Scheme: envScheme}
	for _, name := range []string{"site", "site-bay"} {
		if _, err := zc.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}); err != nil {
			t.Fatalf("zone reconcile %q: %v", name, err)
		}
	}
}

func cp7Dispatch(t *testing.T, ns string, caps ...string) *fleetv1.FleetAction {
	t.Helper()
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Scheduler: scheduler.NewDefaultScheduler()}
	f12SeedAdapter(t, ns)
	f12SeedRobot(t, ns, "amr-1", "site-bay")
	req := zrSeedAction(t, ns, "act-1", "site-bay", caps...)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return zrGetAction(t, req)
}

func TestEnvtest_ZoneRestrictionRefusal_ControlAssigns(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	cp7Floor(t, ns, nil)

	got := cp7Dispatch(t, ns)
	if got.Status.AssignedRobot != "amr-1" {
		t.Fatalf("control: assignedRobot = %q (phase %q, reason %q, message %q), want amr-1. "+
			"Without this the refusal tests below could pass on a fleet that assigns nothing",
			got.Status.AssignedRobot, got.Status.Phase, got.Status.FailureReason, got.Status.Message)
	}
}

func TestEnvtest_ZoneRestrictionRefusal_AncestorEntryDenialRefusesBeforeAssignment(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	cp7Floor(t, ns, &fleetv1.ZoneRestrictions{Entry: &fleetv1.ZoneEntryRestriction{
		Mode: fleetv1.ZoneRestrictionDenied, Reason: "construction until further notice",
	}})

	got := cp7Dispatch(t, ns)
	if got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: an action targeting a descendant of an entry-denied zone "+
			"was dispatched", got.Status.AssignedRobot)
	}
	if got.Status.Phase != fleetv1.ActionPhaseFailed || got.Status.FailureReason != fleetv1.ActionFailureZoneEntryDenied {
		t.Fatalf("phase=%q reason=%q, want Failed/%s: a standing rule is a terminal refusal",
			got.Status.Phase, got.Status.FailureReason, fleetv1.ActionFailureZoneEntryDenied)
	}
	for _, want := range []string{`"site"`, "construction until further notice"} {
		if !strings.Contains(got.Status.Message, want) {
			t.Errorf("status.message %q does not carry %s: the refusal must name the zone that "+
				"DECLARED the rule, which is not the target", got.Status.Message, want)
		}
	}
}

func TestEnvtest_ZoneRestrictionRefusal_AncestorCapabilityDenialRefusesBeforeAssignment(t *testing.T) {
	requireEnvtest(t)
	ns := envtestNamespace(t)
	cp7Floor(t, ns, &fleetv1.ZoneRestrictions{Capabilities: []fleetv1.ZoneCapabilityRestriction{{
		Name: "clean.mop", Mode: fleetv1.ZoneRestrictionDenied, Reason: "carpet that must not get wet",
	}}})

	got := cp7Dispatch(t, ns, "clean.mop")
	if got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: an action needing a capability denied on the target's "+
			"ancestor was dispatched", got.Status.AssignedRobot)
	}
	if got.Status.Phase != fleetv1.ActionPhaseFailed || got.Status.FailureReason != fleetv1.ActionFailureZoneCapabilityDenied {
		t.Fatalf("phase=%q reason=%q, want Failed/%s",
			got.Status.Phase, got.Status.FailureReason, fleetv1.ActionFailureZoneCapabilityDenied)
	}
	if !strings.Contains(got.Status.Message, "carpet that must not get wet") {
		t.Errorf("status.message %q does not carry the declared reason", got.Status.Message)
	}
}
