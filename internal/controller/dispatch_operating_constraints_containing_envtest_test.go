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

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Filter 12 and FA-R2 over the CONTAINMENT SET, on the real dispatch path (FZ-R10, FZ-R11).
//
// A robot stands where a sterile corridor crosses a ward bay. Its currentZone is the bay,
// the arbitrated winner; its containingZones[] names both. The corridor belongs to a
// different tree from the action's target, so it is not in the resolved leaf set at all:
// before the fold, neither filter 12 nor the assignment ever read its rule.

// corridorWardZones seeds ward (declares nothing), its leaf ward-bay denying camera, and a
// top-level corridor denying microphone, each resolved as the Zone Controller leaves it.
func corridorWardZones(t *testing.T, ns string) {
	t.Helper()
	zrSeedZone(t, ns, "ward", "", nil, []fleetv1.EffectiveZoneRestriction{}, true)
	zrSeedZone(t, ns, "ward-bay", "ward",
		&fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "camera", Mode: fleetv1.ZoneRestrictionDenied, Reason: "patient privacy"},
		}},
		[]fleetv1.EffectiveZoneRestriction{{
			Axis: fleetv1.ZoneAxisOperating, Subject: "camera",
			Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "ward-bay", Reason: "patient privacy",
		}}, true)
	zrSeedZone(t, ns, "corridor", "",
		&fleetv1.ZoneRestrictions{Operating: []fleetv1.ZoneOperatingRestriction{
			{Subject: "microphone", Mode: fleetv1.ZoneRestrictionDenied, Reason: "sterile corridor"},
		}},
		[]fleetv1.EffectiveZoneRestriction{{
			Axis: fleetv1.ZoneAxisOperating, Subject: "microphone",
			Mode: fleetv1.ZoneRestrictionDenied, InheritedFrom: "corridor", Reason: "sterile corridor",
		}}, true)
}

// seedContaining publishes a robot's containment set, as the Zone Controller's transition does.
func seedContaining(t *testing.T, ns, name string, containing ...string) {
	t.Helper()
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &robot); err != nil {
		t.Fatalf("get robot %q: %v", name, err)
	}
	robot.Status.ContainingZones = containing
	if err := envK8s.Status().Update(context.Background(), &robot); err != nil {
		t.Fatalf("seed containingZones %q: %v", name, err)
	}
}

// THE ITEM'S PROBE, FIRST HALF. The adapter honours the rule at the robot's leaf and not the
// corridor's; the robot is withheld, the action is HELD under ZoneOperatingConstraintUnmet,
// and the sentence names the corridor's subject and the corridor.
func TestEnvtest_Filter12_TheSecondContainingZonesRuleHoldsTheAction(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &declineCommander{}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	corridorWardZones(t, ns)
	f12SeedRobot(t, ns, "amr-1", "ward-bay", "camera")
	seedContaining(t, ns, "amr-1", "corridor", "ward-bay")

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := zrGetAction(t, req)
	if got.Status.AssignedRobot != "" || len(cmd.assigns) != 0 {
		t.Fatalf("assignedRobot = %q after %d assignment(s): a robot inside corridor, which denies "+
			"microphone, was dispatched by an adapter that declares only camera. Filter 12 MUST "+
			"judge the rules of every zone of status.containingZones[], not status.currentZone "+
			"alone (FZ-R10: the winner alone does not conform)", got.Status.AssignedRobot, len(cmd.assigns))
	}
	if got.Status.Phase != fleetv1.ActionPhasePending ||
		got.Status.FailureReason != fleetv1.ActionFailureZoneOperatingConstraintUnmet {
		t.Fatalf("phase %q, failureReason %q; want Pending under %q", got.Status.Phase,
			got.Status.FailureReason, fleetv1.ActionFailureZoneOperatingConstraintUnmet)
	}
	for _, want := range []string{"microphone", `"corridor"`, "sterile corridor", "amr-1"} {
		if !strings.Contains(got.Status.Message, want) {
			t.Errorf("status.message %q does not carry %q. The rule that withheld the robot is "+
				"declared on a zone outside the action's subtree; a sentence naming only the "+
				"subtree's rules sends the operator to a zone with nothing in it", got.Status.Message, want)
		}
	}
}

// THE ITEM'S PROBE, SECOND HALF. The adapter honours both subjects, so the robot is
// dispatched, and the assignment carries the corridor's rule beside the leaf's: FA-R2's set
// is folded per FZ-R11 with every containing zone, and is the set filter 12 judged.
func TestEnvtest_FAR2_TheAssignmentCarriesEveryContainingZonesRule(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &declineCommander{}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	corridorWardZones(t, ns)
	f12SeedRobot(t, ns, "amr-1", "ward-bay", "camera", "microphone")
	seedContaining(t, ns, "amr-1", "corridor", "ward-bay")

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := zrGetAction(t, req); got.Status.AssignedRobot != "amr-1" {
		t.Fatalf("assignedRobot = %q, want amr-1 (phase %q, message %q)",
			got.Status.AssignedRobot, got.Status.Phase, got.Status.Message)
	}
	if len(cmd.assigns) != 1 {
		t.Fatalf("%d assignments pushed, want 1", len(cmd.assigns))
	}
	got := wireSubjects(cmd.assigns[0])
	if strings.Join(got, ",") != "camera,microphone" {
		t.Fatalf("the assignment carried operating subjects %v, want [camera microphone]. FA-R2: the "+
			"rules on AssignAction are those at the robot's leaf FOLDED WITH EVERY ZONE OF ITS "+
			"status.containingZones[]; an adapter sent the leaf's alone has no rule for the corridor "+
			"it is standing in", got)
	}
	for _, c := range cmd.assigns[0].OperatingConstraints {
		if c.GetSubject() == "microphone" && c.GetDeclaringZone() != "corridor" {
			t.Errorf("microphone declaring_zone = %q, want corridor: the union keeps the DECLARING "+
				"zone (FZ-R11), the one an operator must edit", c.GetDeclaringZone())
		}
	}
}

// A robot NOT inside the corridor is dispatched and is sent the leaf's rule alone.
func TestEnvtest_FAR2_ARobotOutsideTheCorridorIsSentTheLeafsRulesAlone(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &declineCommander{}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	corridorWardZones(t, ns)
	f12SeedRobot(t, ns, "amr-1", "ward-bay", "camera")
	seedContaining(t, ns, "amr-1", "ward-bay")

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := zrGetAction(t, req); got.Status.AssignedRobot != "amr-1" {
		t.Fatalf("assignedRobot = %q, want amr-1: corridor does not contain the robot, so its rule "+
			"does not bind it (phase %q, message %q)", got.Status.AssignedRobot, got.Status.Phase, got.Status.Message)
	}
	if len(cmd.assigns) != 1 {
		t.Fatalf("%d assignments pushed, want 1", len(cmd.assigns))
	}
	if got := wireSubjects(cmd.assigns[0]); strings.Join(got, ",") != "camera" {
		t.Fatalf("the assignment carried operating subjects %v, want [camera]", got)
	}
}
