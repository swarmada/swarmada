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
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// `INVALID_ACTION` is a STANDING property of the (robot, action) pair, so the plane
// stops re-offering the same assignment to the robot that refused it.
//
// # THE ITEM'S TABLE NAMES A REJECTION VALUE THAT DOES NOT EXIST
//
// The finding this closes proposed two different fixes for two values:
// `UNSUPPORTED_ACTION_TYPE` (robot-specific → a memo) and `INVALID_ACTION` (not
// robot-specific → a terminal failure). **`AssignActionRejection` has no
// `UNSUPPORTED_ACTION_TYPE`.** Its seven values are UNSPECIFIED, STALE_FENCING_TOKEN,
// MISSING_FENCING_TOKEN, ROBOT_BUSY, ROBOT_UNAVAILABLE, INVALID_ACTION and
// OPERATING_CONSTRAINT_UNMET — and `INVALID_ACTION = 5` is documented on the wire as
// *"unparseable / unsupported"*, which is BOTH of the item's cases under one value.
//
// # WHICH IS WHY TERMINAL IS THE WRONG ANSWER, NOT MERELY THE EXPENSIVE ONE
//
// The plane cannot tell, from one enum value, "this adapter does not implement this
// action type" (robot-specific; another adapter may serve it) from "this payload is
// unparseable" (every candidate will refuse it). Failing the action terminally on that
// value would convert *one adapter cannot do this* into *nobody can* — the exact inverse
// of the defect being fixed. Instead of burning the fleet on a manifest that cannot
// succeed, it would refuse the fleet a manifest that could.
//
// So: a MEMO, which degrades correctly for both cases the value conflates.
//
//   - unsupported type → the adapter that cannot serve it is skipped and the next
//     candidate takes the work. The fix, exactly.
//   - unparseable payload → each candidate is asked ONCE, all refuse, the eligible set
//     empties, and the action reports it. Bounded by the candidate count per action
//     rather than by candidates × requeues, and the message names what happened.
//
// A terminal failure on a payload every candidate refuses is still the honest end state
// — it just needs a wire value the plane can trust, and that is a contract change.

// rejectCommander refuses assignments with a caller-chosen rejection value, per robot.
type rejectCommander struct {
	fakeCommander
	// rejectWith maps a robot to the rejection value its adapter answers with. A robot
	// absent from the map accepts.
	rejectWith map[string]string
}

func (f *rejectCommander) PushAssignAction(
	_ context.Context, _, robotID string, a command.AssignAction,
) (command.AssignActionOutcome, error) {
	f.assigns = append(f.assigns, a)
	f.assignTo = append(f.assignTo, robotID)
	if reason, refuse := f.rejectWith[robotID]; refuse {
		return command.AssignActionOutcome{Accepted: false, Rejection: reason}, nil
	}
	return command.AssignActionOutcome{Accepted: true, AcceptedFencingToken: a.FencingToken}, nil
}

// THE REPRODUCTION, and then the fix. Two candidates in one zone; the higher-battery one
// refuses INVALID_ACTION because its adapter does not implement the type. Measured before
// this landed: the assignments pushed were [amr-1 amr-1].
func TestEnvtest_InvalidAction_TheRefusingRobotIsNotOfferedItAgain(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &rejectCommander{rejectWith: map[string]string{
		"amr-1": "ASSIGN_ACTION_REJECTION_INVALID_ACTION",
	}}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "ward", "", nil, []fleetv1.EffectiveZoneRestriction{}, true)
	f12SeedRobot(t, ns, "amr-1", "ward")
	f12SeedRobot(t, ns, "amr-2", "ward")
	seedBattery(t, ns, "amr-1", 90) // offered first (battery-descending)
	seedBattery(t, ns, "amr-2", 50)

	req := zrSeedAction(t, ns, "act-1", "ward")
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if got := zrGetAction(t, req); got.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase after the refusal = %q, want Pending. INVALID_ACTION conflates "+
			"\"this adapter does not implement this type\" with \"this payload is unparseable\" "+
			"on one wire value, so a terminal failure here would refuse the fleet an action a "+
			"different adapter could serve", got.Status.Phase)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	got := zrGetAction(t, req)
	if got.Status.AssignedRobot != "amr-2" {
		t.Fatalf("assignedRobot = %q, want amr-2 (phase %q, message %q; assignments pushed to "+
			"%v). Measured before this fix: [amr-1 amr-1] — releaseRejectedAssignment records "+
			"nothing about WHICH robot refused, and SelectRobot re-ranks the same candidates by "+
			"the same battery-descending key, so the refuser is offered the work again on every "+
			"requeue. Nothing about waiting changes whether an adapter build implements an "+
			"action type",
			got.Status.AssignedRobot, got.Status.Phase, got.Status.Message, cmd.assignTo)
	}
}

// EVERY CANDIDATE REFUSING IS THE OTHER CASE THE ONE VALUE CONFLATES — an unparseable
// payload — AND THE PLANE CAN TELL IN AGGREGATE EVEN THOUGH IT CANNOT TELL PER REFUSAL.
//
// It stays Pending, because a conforming adapter deployed tomorrow makes the same action
// dispatchable and that is the same reasoning that keeps ZoneOperatingConstraintUnmet
// non-terminal. What changes is that the operator is TOLD, in a sentence that names the
// payload rather than leaving them to read a generic no-candidate tally.
func TestEnvtest_InvalidAction_EveryCandidateRefusingNamesThePayload(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	cmd := &rejectCommander{rejectWith: map[string]string{
		"amr-1": "ASSIGN_ACTION_REJECTION_INVALID_ACTION",
		"amr-2": "ASSIGN_ACTION_REJECTION_INVALID_ACTION",
	}}
	r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "ward", "", nil, []fleetv1.EffectiveZoneRestriction{}, true)
	f12SeedRobot(t, ns, "amr-1", "ward")
	f12SeedRobot(t, ns, "amr-2", "ward")
	seedBattery(t, ns, "amr-1", 90)
	seedBattery(t, ns, "amr-2", 50)

	req := zrSeedAction(t, ns, "act-1", "ward")
	for i := 1; i <= 4; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	got := zrGetAction(t, req)
	t.Logf("status.message = %s", got.Status.Message)
	t.Logf("assignments pushed to = %v", cmd.assignTo)

	if got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q; every candidate refused", got.Status.AssignedRobot)
	}
	// NOT TERMINAL. The value cannot be trusted to mean "the payload is bad", so the plane
	// does not act as though it does.
	if got.Status.Phase != fleetv1.ActionPhasePending || got.Status.FailedAt != nil {
		t.Fatalf("phase = %q, failedAt = %v; want Pending and never Failed. The one wire value "+
			"conflates a per-adapter limitation with a bad payload, so terminating on it would "+
			"refuse an action a different adapter could serve",
			got.Status.Phase, got.Status.FailedAt)
	}
	// THE COST IS BOUNDED: each adapter asked ONCE, not once per requeue.
	if len(cmd.assignTo) != 2 {
		t.Errorf("%d assignments across four reconciles (%v), want 2 — one per candidate. "+
			"Re-offering an assignment an adapter has already refused as invalid burns a round "+
			"trip per requeue on a manifest that adapter will never accept",
			len(cmd.assignTo), cmd.assignTo)
	}
	// AND THE OPERATOR CAN TELL THIS FROM A ROBOT-SIDE REFUSAL. "Busy" and "flat" are
	// about the robot; this is about the action, and the message has to say which.
	for _, want := range []string{"every candidate", "invalid", "spec.type", "spec.payload"} {
		if !strings.Contains(got.Status.Message, want) {
			t.Errorf("status.message does not carry %q:\n%s\n"+
				"An operator reading a generic no-candidate tally cannot tell \"the fleet is "+
				"busy\" from \"every adapter refused this manifest\", and the second one is "+
				"fixed by editing the FleetAction rather than by waiting", want, got.Status.Message)
		}
	}
}

// THE THREE MOMENT-FACTS ARE UNTOUCHED, and this is the guard the finding asked for by
// name. A busy robot becomes free; waiting is the correct response and the plane must
// keep doing it.
func TestEnvtest_InvalidAction_TheMomentFactsAreStillRetried(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, rejection string }{
		{"busy", "ASSIGN_ACTION_REJECTION_ROBOT_BUSY"},
		{"unavailable", "ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE"},
		{"stale-token", "ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := envtestNamespace(t)
			cmd := &rejectCommander{rejectWith: map[string]string{"amr-1": tc.rejection}}
			r := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
				Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}

			f12SeedAdapter(t, ns)
			zrSeedZone(t, ns, "ward", "", nil, []fleetv1.EffectiveZoneRestriction{}, true)
			f12SeedRobot(t, ns, "amr-1", "ward")

			req := zrSeedAction(t, ns, "act-1", "ward")
			for i := 1; i <= 2; i++ {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatalf("reconcile %d: %v", i, err)
				}
			}
			if len(cmd.assignTo) != 2 {
				t.Fatalf("%s: assignments pushed to %v, want amr-1 TWICE. Only a STANDING "+
					"property of the (robot, action) pair may suppress a retry; a busy robot "+
					"becomes free, an unavailable one recovers, and a stale token is superseded "+
					"by the next generation. Suppressing those would idle a fleet on facts that "+
					"expire", tc.name, cmd.assignTo)
			}
		})
	}
}
