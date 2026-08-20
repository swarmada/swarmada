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
)

// RFC-0001 candidate filter 10: a robot under an active estop is not a dispatch
// candidate. Until this test existed, filterDispatchEligible enforced only
// filter 9 (adapter readiness), so an Idle robot that had been commanded to stop
// — including every robot in a zone under a zone-wide estop — stayed schedulable
// and would be handed new work.
//
// The table walks the whole RobotEstopState enum plus the empty value rather
// than just the two active ones, so adding a state without deciding its dispatch
// meaning fails here instead of silently defaulting to schedulable.
func TestFilterDispatchEligible_Filter10Estop(t *testing.T) {
	cases := []struct {
		name         string
		state        fleetv1.RobotEstopState
		wantWithheld bool
		why          string
	}{
		{"normal", fleetv1.RobotEstopNormal, false, "no estop"},
		{"stopping", fleetv1.RobotEstopStopping, true, "stop issued to hardware; the robot may still be moving"},
		{"stopped", fleetv1.RobotEstopStopped, true, "confirmed at rest, awaiting an operator clear"},
		// Resuming is not withheld — but note that NOTHING IN THE TREE WRITES IT
		// (ITEM-0102). safety.md:148 specifies it as the post-clear window;
		// ClearEstop goes straight to Normal, so this row currently describes a
		// state that cannot occur at runtime. The row stays because the enum
		// declares the value and the spec says the system reaches it; when
		// ITEM-0102 lands, the assertion becomes live rather than hypothetical.
		{"resuming", fleetv1.RobotEstopResuming, false, "post-clear window; unreachable today, see ITEM-0102"},
		// Failed IS withheld, per ITEM-0101 (decided 2026-08-13). It does not
		// mean the stop was refused; it means a stop was commanded and never
		// confirmed, so the robot's physical state is unknown and it must not be
		// treated as at rest. It is the single most important row in this table:
		// before the decision, an emergency stop that failed to confirm left that
		// robot — and only that robot — schedulable.
		{"failed", fleetv1.RobotEstopFailed, true, "stop commanded, never confirmed; physical state unknown"},
		{"empty", "", false, "an empty state is treated as Normal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// robotInPhase binds the fixture adapter, which newActionReconciler
			// seeds as Connected + conformance Passed, so filter 9 always passes
			// here and only filter 10 can withhold the robot.
			robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "")
			robot.Status.EstopState = tc.state

			r, _ := newActionReconciler(t, robot)
			eligible, withheld := r.filterDispatchEligible(context.Background(), []fleetv1.Robot{*robot})

			gotWithheld := len(eligible) == 0
			if gotWithheld != tc.wantWithheld {
				t.Fatalf("estopState %q: withheld=%v, want %v (%s); eligible=%d withheld=%d",
					tc.state, gotWithheld, tc.wantWithheld, tc.why, len(eligible), len(withheld))
			}
			if !tc.wantWithheld {
				return
			}
			// An operator debugging "why is my action still Pending" reads this
			// reason. A withheld robot with no stated cause is ITEM-0027.
			if len(withheld) != 1 {
				t.Fatalf("estopState %q: want exactly 1 exclusion, got %d", tc.state, len(withheld))
			}
			if !strings.Contains(withheld[0].Reason, "emergency stop") ||
				!strings.Contains(withheld[0].Reason, string(tc.state)) {
				t.Errorf("estopState %q: exclusion reason %q names neither the cause nor the state",
					tc.state, withheld[0].Reason)
			}
		})
	}
}

// The estop filter must not depend on the robot's FleetAdapter being readable: a
// zone-wide estop can fan out to robots whose adapters are also unreachable, and
// the cause an operator needs to see is the estop, not a secondary adapter
// failure that is downstream of the same incident.
func TestFilterDispatchEligible_EstopReasonWinsOverAdapter(t *testing.T) {
	robot := robotInPhase("r1", fleetv1.RobotPhaseIdle, "")
	robot.Status.EstopState = fleetv1.RobotEstopStopped
	robot.Spec.Adapter.Name = "adapter-that-does-not-exist"

	r, _ := newActionReconciler(t, robot)
	eligible, withheld := r.filterDispatchEligible(context.Background(), []fleetv1.Robot{*robot})

	if len(eligible) != 0 {
		t.Fatalf("estopped robot was dispatch-eligible")
	}
	if len(withheld) != 1 || !strings.Contains(withheld[0].Reason, "emergency stop") {
		t.Fatalf("want the estop as the stated reason, got %+v", withheld)
	}
}
