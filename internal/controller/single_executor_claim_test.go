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
	"os"
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Issue #6 — two FleetActions must never hold an assignment lease on the same Robot.
//
// The optimistic-concurrency guard in the reconciler is on the ACTION, so two
// reconciles for two different actions each pass their own guard independently.
// The robot claim was a plain merge patch with no precondition, so a second write
// silently overwrote the first: Robot.status.assignedAction holds one name, and
// the losing action sat Assigned to a robot executing something else.
//
// Drives two actions against one Idle robot and asserts the invariant the
// specification calls the single-executor guarantee: at most one action is bound
// to a robot, the loser is returned to Pending rather than stranded, and the
// robot's own status names the action that actually holds it.
func TestSingleExecutor_TwoActionsCannotClaimOneRobot(t *testing.T) {
	r, c := newActionReconciler(t,
		pendingAction("t1"),
		pendingAction("t2"),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""),
	)

	reconcileAction(t, r, "t1")
	reconcileAction(t, r, "t2")

	a1, a2 := getAction(t, c, "t1"), getAction(t, c, "t2")

	var holder string
	bound := 0
	for _, a := range []*fleetv1.FleetAction{a1, a2} {
		if a.Status.AssignedRobot == "r1" {
			bound++
			holder = a.Name
		}
	}
	if bound > 1 {
		t.Fatalf("both actions hold robot r1 — single-executor guarantee violated "+
			"(t1=%q/%s, t2=%q/%s)",
			a1.Status.AssignedRobot, a1.Status.Phase, a2.Status.AssignedRobot, a2.Status.Phase)
	}

	// The loser must go back to Pending, not sit Assigned to nothing.
	for _, a := range []*fleetv1.FleetAction{a1, a2} {
		if a.Name == holder {
			continue
		}
		if a.Status.AssignedRobot != "" {
			t.Errorf("%s lost the claim but still names robot %q", a.Name, a.Status.AssignedRobot)
		}
		if a.Status.Phase != fleetv1.ActionPhasePending && a.Status.Phase != "" {
			t.Errorf("%s should be Pending after losing the claim, is %q", a.Name, a.Status.Phase)
		}
	}

	// A robot naming an action that does not name it back is the same defect from
	// the other end.
	rob := getRobotByName(t, c, "r1")
	if rob.Status.AssignedAction != holder {
		t.Errorf("robot names %q, action holding it is %q", rob.Status.AssignedAction, holder)
	}
}

// The functional test above can pass for the wrong reason: it depends on the fake
// client enforcing resourceVersion preconditions, and a test that would also pass
// against a plain merge patch proves nothing about the fix.
//
// So assert the mechanism directly. This is unusual for a unit test and it is
// deliberate: the defect is a race, the guard is one call, and a refactor that
// reverts to client.MergeFrom would reopen issue #6 while every behavioural test
// still passed on a fast machine.
func TestSingleExecutor_RobotClaimUsesOptimisticLock(t *testing.T) {
	src, err := os.ReadFile("fleetaction_controller.go")
	if err != nil {
		t.Fatalf("reading the controller source: %v", err)
	}
	const claim = "robot.Status.AssignedAction = action.Name"
	i := strings.Index(string(src), claim)
	if i < 0 {
		t.Fatalf("the robot claim site %q was not found — this test's anchor is stale "+
			"and the guard it protects is now unverified", claim)
	}
	// Find the patch that COMMITS this claim, rather than scanning a fixed number of
	// bytes after it. The first version used a 900-byte window and failed against a
	// correct fix, because the explanatory comment on the guard is itself ~900 bytes:
	// the test could not see past the prose justifying the thing it was checking.
	rest := string(src[i:])
	j := strings.Index(rest, ".Patch(ctx, &robot")
	if j < 0 {
		t.Fatalf("no robot status patch follows the claim at %q — the commit path moved "+
			"and this test no longer guards anything", claim)
	}
	line := rest[j:]
	if k := strings.IndexByte(line, '\n'); k > 0 {
		line = line[:k]
	}
	if !strings.Contains(line, "MergeFromWithOptimisticLock") {
		t.Errorf("the robot claim patches without an optimistic-lock precondition, so two "+
			"actions can claim one robot (issue #6). The commit reads:\n    %s", strings.TrimSpace(line))
	}
}
