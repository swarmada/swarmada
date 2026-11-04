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
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// FA-R2 — carry, on the REAL DISPATCH PATH, meeting FA-R5 — decline.
//
// The two halves have never met. L1 landed the wire surface, L2 landed candidate
// filter 12 and said so in terms: "AssignAction.operating_constraints is still filled
// by nothing on a real dispatch … filter 12 enforces in the plane only". L3 implemented
// the decline in all three in-tree adapters and proved it red-then-green against a real
// adapter process. This file is where a plane that FILLS the field meets an adapter that
// ACTS on it.
//
// WHY THE WIRE STILL CARRIES WHAT FILTER 12 ALREADY CHECKED. On the happy path an
// adapter is rarely asked to decline, because filter 12 withheld the robot first. That
// is not a reason to stop sending the rule:
//
//   - filter 12 is built on the adapter's OWN WORD. An adapter's declaration is a claim,
//     not a measurement (ADR-0058 §12), so the filter can only ever be as good as the
//     claim. The decline is the adapter meeting the rule itself.
//   - a declaration can go STALE between the CapabilitiesSnapshot that produced it and
//     the assignment that relies on it.
//   - FA-R6 exists for a subject the adapter does not RECOGNISE, which no declaration
//     covers: an unrecognised subject is not an absent one.
//
// A rule enforced in one place is a rule with a bypass (ADR-0058 §5). That sentence
// applies to the filter exactly as it applies to the adapter.

// wireSubjects reports the operating subjects an assignment actually carried on the
// wire — the single seam these tests are written against, so "the plane fills the
// field" is asserted rather than assumed.
func wireSubjects(a command.AssignAction) []string {
	out := make([]string, 0, len(a.OperatingConstraints))
	for _, c := range a.OperatingConstraints {
		out = append(out, c.GetSubject())
	}
	sort.Strings(out)
	return out
}

// declineCommander is an adapter that applies L3's own predicate — decline every
// subject on the assignment the adapter did not declare it honours — to the constraint
// set the CONTROL PLANE put on the wire.
//
// It is deliberately NOT parameterised by robot name for the decline decision: the only
// input is what arrived on the assignment, so a plane that sends nothing gets an
// acceptance, which is exactly the defect this file was written red against.
type declineCommander struct {
	fakeCommander
	// cannotHonour maps a robot to the subjects its adapter cannot actually honour,
	// whatever its snapshot declared. This is the stale-declaration case: filter 12
	// admitted the robot on its own word, and the adapter is where the word fails.
	cannotHonour map[string][]string
	// declinedSubjects records what each decline named, per robot, in arrival order.
	declinedSubjects map[string][]string
	// busy makes a robot refuse for a reason that is NOT an operating constraint, so
	// the scoping of the plane's decline memory is testable.
	busy map[string]bool
}

func (f *declineCommander) PushAssignAction(
	ctx context.Context, ns, robotID string, a command.AssignAction,
) (command.AssignActionOutcome, error) {
	f.assigns = append(f.assigns, a)
	f.assignTo = append(f.assignTo, robotID)

	if f.busy[robotID] {
		return command.AssignActionOutcome{
			Accepted: false, Rejection: "ASSIGN_ACTION_REJECTION_ROBOT_BUSY",
		}, nil
	}
	cannot := map[string]bool{}
	for _, s := range f.cannotHonour[robotID] {
		cannot[s] = true
	}
	var unhonourable []string
	for _, s := range wireSubjects(a) {
		if cannot[s] {
			unhonourable = append(unhonourable, s)
		}
	}
	if len(unhonourable) > 0 {
		if f.declinedSubjects == nil {
			f.declinedSubjects = map[string][]string{}
		}
		f.declinedSubjects[robotID] = append(f.declinedSubjects[robotID], strings.Join(unhonourable, ","))
		return command.AssignActionOutcome{
			Accepted:             false,
			Rejection:            "ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET",
			UnhonourableSubjects: unhonourable,
			Message:              "cannot honour " + strings.Join(unhonourable, ", "),
		}, nil
	}
	return command.AssignActionOutcome{Accepted: true, AcceptedFencingToken: a.FencingToken}, nil
}

// icuWardZones seeds ward (declares nothing) with the leaf ward-icu denying camera —
// ADR-0058 §1's deciding case, and the shape Amendment 2 §A2.1 tabulates.
func icuWardZones(t *testing.T, ns string) {
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

// seedBattery sets a robot's battery so the scheduler's battery-descending rank is
// deterministic and the test controls which candidate is offered first.
func seedBattery(t *testing.T, ns, name string, pct int32) {
	t.Helper()
	var robot fleetv1.Robot
	if err := envK8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &robot); err != nil {
		t.Fatalf("get robot %q: %v", name, err)
	}
	robot.Status.BatteryPercent = &pct
	if err := envK8s.Status().Update(context.Background(), &robot); err != nil {
		t.Fatalf("seed battery %q: %v", name, err)
	}
}

// THE FIELD IS FILLED ON A REAL DISPATCH, FROM THE RESOLVED PROJECTION AT THE LEAF.
//
// Red before this landed: operatingConstraintsForWire had no non-test caller, so the
// assignment reached the adapter carrying nothing and was ACCEPTED. The decline the
// adapter implements could never fire, because the message it acts on was empty.
func TestEnvtest_FAR2_TheAssignmentCarriesTheResolvedRulesAtTheRobotsLeaf(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &declineCommander{}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	icuWardZones(t, ns)
	// Declares camera, so filter 12 admits it; the assignment must still carry the rule.
	f12SeedRobot(t, ns, "amr-1", "ward-icu", "camera")

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
	if len(got) != 1 || got[0] != "camera" {
		t.Fatalf("the assignment carried operating subjects %v, want [camera]. FA-R2: the "+
			"control plane MUST send the RESOLVED EFFECTIVE operating rules on AssignAction. "+
			"Until it does, the adapter's decline path is a message with no sender and "+
			"ADR-0058 §5's SECOND place is a wire format, not an enforcement", got)
	}
	// Resolved, never raw: the rule carries the DECLARING zone, which is the leaf and not
	// the target, and the operator's own reason travels with it.
	c := cmd.assigns[0].OperatingConstraints[0]
	if c.GetDeclaringZone() != "ward-icu" {
		t.Errorf("declaring_zone = %q, want ward-icu — the zone an operator must actually edit",
			c.GetDeclaringZone())
	}
	if c.GetReason() != "patient privacy" {
		t.Errorf("reason = %q, want the operator's own words", c.GetReason())
	}
	if c.GetMode().String() != "OPERATING_CONSTRAINT_MODE_DENIED" {
		t.Errorf("mode = %q, want DENIED", c.GetMode())
	}
}

// THE END-TO-END: decline → Pending → the next candidate takes it.
//
// FA-R5: "Declining costs the adapter nothing: the action returns to Pending and the
// Scheduler tries the next candidate." An action that declines into a terminal failure
// when another candidate could have served it is a regression, not a refusal.
func TestEnvtest_FAR5_DeclineReturnsToPendingAndTheNextCandidateTakesIt(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	// amr-1's snapshot says it honours camera and its adapter cannot: the STALE
	// DECLARATION case, which is exactly why the wire still carries the rule that
	// filter 12 already checked.
	cmd := &declineCommander{cannotHonour: map[string][]string{"amr-1": {"camera"}}}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	icuWardZones(t, ns)
	f12SeedRobot(t, ns, "amr-1", "ward-icu", "camera") // declares it, cannot honour it
	f12SeedRobot(t, ns, "amr-2", "ward-icu", "camera") // declares it and honours it
	seedBattery(t, ns, "amr-1", 90)                    // offered first (battery-descending)
	seedBattery(t, ns, "amr-2", 50)

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// 1 — the constraint set reached the adapter.
	if len(cmd.assigns) == 0 {
		t.Fatal("no assignment was pushed at all")
	}
	if got := wireSubjects(cmd.assigns[0]); len(got) != 1 || got[0] != "camera" {
		t.Fatalf("the first assignment carried %v, want [camera]", got)
	}
	// 2 — the adapter declined, naming the subject.
	if cmd.declinedSubjects["amr-1"] == nil {
		t.Fatalf("amr-1 never declined; assignments seen: %v to %v", cmd.assigns, cmd.assignTo)
	}
	// 3 — the action returned to Pending, not Failed.
	after1 := zrGetAction(t, req)
	if after1.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase after the decline = %q, want Pending. Declining costs the adapter "+
			"nothing (FA-R5): a decline that failed the action terminally when another "+
			"candidate could have served it is a regression, not a refusal", after1.Status.Phase)
	}
	if after1.Status.FailedAt != nil {
		t.Errorf("failedAt stamped on a declined assignment: %v", after1.Status.FailedAt)
	}
	if after1.Status.AssignedRobot != "" {
		t.Errorf("assignedRobot = %q after a decline; the binding must be released",
			after1.Status.AssignedRobot)
	}
	// 4 — the Scheduler tries the NEXT candidate, and it takes it.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	after2 := zrGetAction(t, req)
	if after2.Status.AssignedRobot != "amr-2" {
		t.Fatalf("assignedRobot = %q after the decline, want amr-2 (phase %q, message %q; "+
			"assignments pushed to %v). FA-R5: the action returns to Pending and the "+
			"Scheduler TRIES THE NEXT CANDIDATE. Re-offering the assignment to the robot "+
			"that just declined it makes declining cost the adapter a round trip on every "+
			"requeue and the action everything — it never runs, though a candidate that can "+
			"serve it is standing in the same zone",
			after2.Status.AssignedRobot, after2.Status.Phase, after2.Status.Message, cmd.assignTo)
	}
}

// EVERY CANDIDATE DECLINES. The action stays Pending, never Failed, and reports
// ZoneOperatingConstraintUnmet — "no robot here can honour the rule", which is the true
// sentence. A decline that failed the action terminally would be the regression FA-R5
// forbids; so would a decline that left the operator reading the generic no-candidate
// tally.
func TestEnvtest_FAR5_EveryCandidateDecliningHoldsThePendingActionAndNamesTheReason(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &declineCommander{cannotHonour: map[string][]string{
		"amr-1": {"camera"}, "amr-2": {"camera"},
	}}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	icuWardZones(t, ns)
	f12SeedRobot(t, ns, "amr-1", "ward-icu", "camera")
	f12SeedRobot(t, ns, "amr-2", "ward-icu", "camera")
	seedBattery(t, ns, "amr-1", 90)
	seedBattery(t, ns, "amr-2", 50)

	req := zrSeedAction(t, ns, "act-1", "ward")
	for i := 1; i <= 3; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	got := zrGetAction(t, req)
	t.Logf("status.message = %s", got.Status.Message)
	t.Logf("assignments pushed to = %v", cmd.assignTo)

	if got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q; every candidate declined", got.Status.AssignedRobot)
	}
	if got.Status.Phase != fleetv1.ActionPhasePending || got.Status.FailedAt != nil {
		t.Fatalf("phase = %q, failedAt = %v; want Pending and never Failed — declining is a "+
			"fact about today's fleet, and an action that declines into a terminal failure is "+
			"a regression, not a refusal", got.Status.Phase, got.Status.FailedAt)
	}
	if got.Status.FailureReason != fleetv1.ActionFailureZoneOperatingConstraintUnmet {
		t.Errorf("failureReason = %q, want %q — the five refusal shapes exist so an operator "+
			"can tell \"no robot here can honour the rule\" from \"the fleet is busy\"",
			got.Status.FailureReason, fleetv1.ActionFailureZoneOperatingConstraintUnmet)
	}
	// THE COST OF THE REFUSAL IS BOUNDED. Each adapter is asked once, not once per
	// requeue: three reconciles, two candidates, two pushes.
	if len(cmd.assignTo) != 2 {
		t.Errorf("%d assignments pushed across three reconciles (%v), want 2 — one per "+
			"candidate. Re-offering an assignment to an adapter that has already declined it "+
			"makes declining cost the adapter a round trip on every requeue, which is the "+
			"opposite of FA-R5's \"declining costs the adapter nothing\"",
			len(cmd.assignTo), cmd.assignTo)
	}
}

// THE MEMO IS SCOPED TO THIS REJECTION AND NOT TO REJECTION IN GENERAL. A robot that
// refuses because it is BUSY is offered the work again on the next requeue, exactly as
// it was before FA-R2 landed: a busy robot becomes free, and nothing about that fact is
// standing.
func TestEnvtest_FAR5_ABusyRejectionIsStillRetriedOnTheSameRobot(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &declineCommander{busy: map[string]bool{"amr-1": true}}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	icuWardZones(t, ns)
	f12SeedRobot(t, ns, "amr-1", "ward-icu", "camera")

	req := zrSeedAction(t, ns, "act-1", "ward")
	for i := 1; i <= 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if len(cmd.assignTo) != 2 {
		t.Fatalf("assignments pushed to %v, want amr-1 twice. Only "+
			"OPERATING_CONSTRAINT_UNMET is a standing property of a (robot, action) pair; "+
			"the other five rejections are facts about a moment and their "+
			"retry-the-same-robot behaviour must be left exactly as it was", cmd.assignTo)
	}
}

// The wire enum and the constant the reconciler compares against are pinned together. A
// rename on the wire that left this matching nothing would fail OPEN — straight back into
// the re-offer loop — and it would fail silently, which is the worse half.
func TestRejectionConstantsMatchTheWireEnum(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wire  fav1.AssignActionRejection
		local string
	}{
		{"operating-constraint-unmet",
			fav1.AssignActionRejection_ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET,
			rejectionOperatingConstraintUnmet},
		{"invalid-action",
			fav1.AssignActionRejection_ASSIGN_ACTION_REJECTION_INVALID_ACTION,
			rejectionInvalidAction},
	} {
		if got := tc.wire.String(); got != tc.local {
			t.Errorf("%s: the wire enum renders as %q; the reconciler compares against %q. The "+
				"comparison would match nothing and every refusal of that kind would be "+
				"re-offered for ever — failing OPEN, and silently", tc.name, got, tc.local)
		}
	}

	// AND THE VALUE THE FINDING NAMED DOES NOT EXIST. It proposed a separate fix for
	// UNSUPPORTED_ACTION_TYPE; AssignActionRejection has no such value, and
	// INVALID_ACTION is documented on the wire as "unparseable / unsupported" — both
	// cases under one enum. This assertion is what stops a later session re-deriving the
	// two-value premise: if such a value is ever added, this test goes red and the
	// terminal-failure option the finding wanted becomes available on purpose rather
	// than by accident.
	for name, v := range fav1.AssignActionRejection_value {
		if strings.Contains(name, "UNSUPPORTED_ACTION_TYPE") {
			t.Errorf("AssignActionRejection now has %s = %d. INVALID_ACTION's handling in "+
				"dispatch_operating_declines.go is a MEMO precisely because one value covered "+
				"both \"this adapter does not implement the type\" and \"the payload is "+
				"unparseable\". With them separable, revisit: a payload every candidate refuses "+
				"can honestly be terminal, and a per-adapter limitation still cannot",
				name, v)
		}
	}
}
