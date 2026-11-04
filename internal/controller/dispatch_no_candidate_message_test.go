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
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A no-candidate Pending action reported `no eligible robot available`
// whichever filter eliminated the robots — the scheduler's single sentinel error,
// written through verbatim. An operator reading only the action saw a Pending
// action, a fleet of Idle-looking robots, and nothing connecting them: the reason
// lives on the OTHER object (the robot's estopState, its battery, its zone, its
// adapter's phase) and the action never named which one.
//
// RA-1: this is the message on a status patch the dispatch path already performs
// once per reconcile of a Pending action, not a new write and not a per-candidate
// one. The message is built to be STABLE — filters sorted by number, counts not
// robot names — so an unchanged fleet produces an unchanged string and the patch
// stays empty.
func TestNoEligibleRobot_MessageNamesFilterClass(t *testing.T) {
	cases := []struct {
		name    string
		setup   func() (*fleetv1.FleetAction, []*fleetv1.Robot, []*fleetv1.ZoneMaintenance)
		wantSub string
	}{
		{
			name: "battery",
			setup: func() (*fleetv1.FleetAction, []*fleetv1.Robot, []*fleetv1.ZoneMaintenance) {
				rob := idleCandidate("r1")
				low := int32(5)
				rob.Status.BatteryPercent = &low
				a := pendingInBand("t1", fleetv1.ActionPriorityNormal)
				floor := int32(80)
				a.Spec.MinBatteryPct = &floor
				return a, []*fleetv1.Robot{rob}, nil
			},
			wantSub: "filter 4",
		},
		{
			name: "estop",
			setup: func() (*fleetv1.FleetAction, []*fleetv1.Robot, []*fleetv1.ZoneMaintenance) {
				rob := idleCandidate("r1")
				rob.Status.EstopState = fleetv1.RobotEstopStopped
				return pendingInBand("t1", fleetv1.ActionPriorityNormal), []*fleetv1.Robot{rob}, nil
			},
			wantSub: "filter 10",
		},
		{
			name: "capability",
			setup: func() (*fleetv1.FleetAction, []*fleetv1.Robot, []*fleetv1.ZoneMaintenance) {
				a := pendingInBand("t1", fleetv1.ActionPriorityNormal)
				a.Spec.RequiredCapabilities = []string{"lidar"}
				return a, []*fleetv1.Robot{idleCandidate("r1")}, nil
			},
			wantSub: "filter 3",
		},
		{
			name: "parametric constraint",
			setup: func() (*fleetv1.FleetAction, []*fleetv1.Robot, []*fleetv1.ZoneMaintenance) {
				a := pendingInBand("t1", fleetv1.ActionPriorityNormal)
				a.Spec.Constraints = map[string]float64{"maxPayloadKg": 50}
				return a, []*fleetv1.Robot{idleCandidate("r1")}, nil
			},
			wantSub: "filter 5",
		},
		{
			name: "phase not Idle",
			setup: func() (*fleetv1.FleetAction, []*fleetv1.Robot, []*fleetv1.ZoneMaintenance) {
				return pendingInBand("t1", fleetv1.ActionPriorityNormal),
					[]*fleetv1.Robot{robotInPhase("r1", fleetv1.RobotPhaseCharging, "")}, nil
			},
			wantSub: "filter 1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, robots, windows := tc.setup()
			objs := []client.Object{action}
			for _, rob := range robots {
				objs = append(objs, rob)
			}
			for _, w := range windows {
				objs = append(objs, w)
			}
			r, c := newActionReconciler(t, objs...)
			reconcileAction(t, r, "t1")

			msg := getAction(t, c, "t1").Status.Message
			if !strings.Contains(msg, tc.wantSub) {
				t.Fatalf("status.message = %q; want it to name %s — an operator cannot tell this "+
					"cause from any other", msg, tc.wantSub)
			}
		})
	}
}

// Two causes, both named. A fleet is rarely eliminated by one filter, and a
// message that reported only the first would send an operator to fix one robot and
// leave the action Pending on the other.
func TestNoEligibleRobot_MessageNamesEveryFilterThatFired(t *testing.T) {
	flat := idleCandidate("r1")
	low := int32(5)
	flat.Status.BatteryPercent = &low
	stopped := idleCandidate("r2")
	stopped.Status.EstopState = fleetv1.RobotEstopStopped

	action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
	floor := int32(80)
	action.Spec.MinBatteryPct = &floor

	r, c := newActionReconciler(t, action, flat, stopped)
	reconcileAction(t, r, "t1")

	msg := getAction(t, c, "t1").Status.Message
	for _, want := range []string{"filter 4", "filter 10"} {
		if !strings.Contains(msg, want) {
			t.Errorf("status.message = %q; missing %s", msg, want)
		}
	}
}

// Deterministic output (docs/coding-standards.md). Two reconciles over an
// unchanged fleet must produce a byte-identical message, or every requeue of a
// Pending action becomes a status write against etcd.
func TestNoEligibleRobot_MessageIsStableAcrossReconciles(t *testing.T) {
	robots := []client.Object{}
	for _, n := range []string{"r1", "r2", "r3"} {
		rob := idleCandidate(n)
		low := int32(5)
		rob.Status.BatteryPercent = &low
		robots = append(robots, rob)
	}
	action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
	floor := int32(80)
	action.Spec.MinBatteryPct = &floor

	r, c := newActionReconciler(t, append([]client.Object{action}, robots...)...)

	reconcileAction(t, r, "t1")
	first := getAction(t, c, "t1").Status.Message
	reconcileAction(t, r, "t1")
	second := getAction(t, c, "t1").Status.Message

	if first != second {
		t.Fatalf("message churned between reconciles:\n  1: %q\n  2: %q", first, second)
	}
	if first == "" {
		t.Fatal("no message written at all")
	}
}

// An empty namespace is its own diagnosis and must not read as "your robots were
// all rejected".
func TestNoEligibleRobot_MessageSaysSoWhenThereAreNoRobots(t *testing.T) {
	r, c := newActionReconciler(t, pendingInBand("t1", fleetv1.ActionPriorityNormal))
	reconcileAction(t, r, "t1")

	msg := getAction(t, c, "t1").Status.Message
	if !strings.Contains(msg, "no Robot") {
		t.Fatalf("status.message = %q; want it to say the namespace holds no robots", msg)
	}
}

// The exact sentence, pinned. The shape matters as much as the content: filters
// ascending, a count each, no robot names. Read it as the operator does —
//
//	no eligible robot; of 3 candidate robots: 1 not in the action's zone (filter 2),
//	1 below the action's battery floor (filter 4), 1 under an active emergency stop (filter 10)
//
// — and every clause points at a different object to go and look at.
func TestNoEligibleRobot_MessageFormat(t *testing.T) {
	wrongZone := idleCandidate("r1")
	wrongZone.Status.CurrentZone = "aisle-9"
	flat := idleCandidate("r2")
	flat.Status.CurrentZone = "aisle-1"
	low := int32(5)
	flat.Status.BatteryPercent = &low
	stopped := idleCandidate("r3")
	stopped.Status.CurrentZone = "aisle-1"
	stopped.Status.EstopState = fleetv1.RobotEstopStopped

	action := zonedPending("t1", "aisle-1")
	floor := int32(80)
	action.Spec.MinBatteryPct = &floor

	r, c := newActionReconciler(t, action, wrongZone, flat, stopped,
		fleetZone("aisle-1", ""), fleetZone("aisle-9", ""))
	reconcileAction(t, r, "t1")

	const want = "no eligible robot; of 3 candidate robots: 1 not in the action's zone (filter 2), " +
		"1 below the action's battery floor (filter 4), 1 under an active emergency stop (filter 10)"
	if got := getAction(t, c, "t1").Status.Message; got != want {
		t.Fatalf("status.message mismatch\n got: %q\nwant: %q", got, want)
	}
}
