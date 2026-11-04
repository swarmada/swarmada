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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ── Fixtures ──────────────────────────────────────────────────────────────────

// idleCandidate is a robot that passes every filter the dispatch path enforced
// before this file existed: Idle, no estop, bound to the ready fixture adapter.
// Each test below breaks exactly one thing about it, so a failure names one filter.
func idleCandidate(name string) *fleetv1.Robot {
	rob := robotInPhase(name, fleetv1.RobotPhaseIdle, "")
	pct := int32(90)
	rob.Status.BatteryPercent = &pct
	return rob
}

// zonedPending is a Pending action naming a zone — the only shape filter 2 applies to.
func zonedPending(name, zone string) *fleetv1.FleetAction {
	a := pendingInBand(name, fleetv1.ActionPriorityNormal)
	a.Spec.Zone = zone
	return a
}

// fleetZone builds a FleetZone with an optional parent, so a test can assert
// against a real tree rather than a string comparison.
func fleetZone(name, parent string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetZoneSpec{ParentZone: parent},
	}
}

// activeMaintenance is an Active ZoneMaintenance window holding the named robots.
func activeMaintenance(name string, paused, windingDown []string) *fleetv1.ZoneMaintenance {
	zm := &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "aisle-1"},
		},
		Status: fleetv1.ZoneMaintenanceStatus{Phase: fleetv1.ZoneMaintenancePhaseActive},
	}
	for _, n := range paused {
		zm.Status.PausedRobots = append(zm.Status.PausedRobots,
			fleetv1.PausedRobotEntry{Name: n, Namespace: actionNS, PausedAt: metav1.Now()})
	}
	for _, n := range windingDown {
		zm.Status.WindingDownRobots = append(zm.Status.WindingDownRobots,
			fleetv1.WindingDownRobot{Name: n, AssignedAction: "prior"})
	}
	return zm
}

// ── Filter 7: a robot mid-model-update is not a candidate ────────────────────

// RFC-0001 candidate filter 7: "Robot.status.installedModels[*].status must not be
// Updating for any model that grants a required capability."
//
// The capability here is granted through status.modelGrantedCapabilities — the
// path the OTA manager writes and the Robot controller unions in as Active
// UNCONDITIONALLY (robot_controller.go:541-548). That union does not consult the
// model's runtime status, so the derived capability stays Active for the whole
// update window and the indirect exclusion (a model-driven capability deriving to
// Inactive) never fires here. Filter 7 is the only thing standing between a
// mid-update robot and an assignment.
func TestDispatch_Filter7_ExcludesRobotMidModelUpdate(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Status.ModelGrantedCapabilities = []fleetv1.ModelGrantedCapabilityEntry{
		{ModelName: "item-recognition", GrantedBy: "rollout-1", Capabilities: []string{"vision.detect"}},
	}
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "vision.detect", Status: fleetv1.CapabilityStatusActive},
	}
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusUpdating, RunningVersion: "3.2.0"},
	}

	action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
	action.Spec.RequiredCapabilities = []string{"vision.detect"}

	r, c := newActionReconciler(t, action, rob)
	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePending || ft.Status.AssignedRobot != "" {
		t.Fatalf("filter 7 did not fire: phase=%s robot=%q — a robot mid-model-update took the action",
			ft.Status.Phase, ft.Status.AssignedRobot)
	}
	if !strings.Contains(ft.Status.Message, "filter 7") {
		t.Errorf("status.message = %q; want it to name filter 7", ft.Status.Message)
	}
}

// An Active model granting the required capability is not excluded — filter 7 must
// withhold a robot mid-update, not every robot that has a model at all.
func TestDispatch_Filter7_ActiveModelIsStillACandidate(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Status.ModelGrantedCapabilities = []fleetv1.ModelGrantedCapabilityEntry{
		{ModelName: "item-recognition", GrantedBy: "rollout-1", Capabilities: []string{"vision.detect"}},
	}
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "vision.detect", Status: fleetv1.CapabilityStatusActive},
	}
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.2.1"},
	}

	action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
	action.Spec.RequiredCapabilities = []string{"vision.detect"}

	r, c := newActionReconciler(t, action, rob)
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "r1" {
		t.Fatalf("assignedRobot = %q, want r1: filter 7 must not withhold a robot whose model is Active", got.Status.AssignedRobot)
	}
}

// A model that is Updating but grants NOTHING the action requires is irrelevant to
// this action. Filter 7 is scoped to models granting a REQUIRED capability; a
// broader reading would idle a fleet during any unrelated rollout.
func TestDispatch_Filter7_UnrelatedModelUpdateDoesNotWithhold(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Status.ModelGrantedCapabilities = []fleetv1.ModelGrantedCapabilityEntry{
		{ModelName: "speech", GrantedBy: "rollout-2", Capabilities: []string{"audio.transcribe"}},
	}
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "navigation", Status: fleetv1.CapabilityStatusActive},
	}
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{
		{Name: "speech", Status: fleetv1.ModelStatusUpdating},
	}

	action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
	action.Spec.RequiredCapabilities = []string{"navigation"}

	r, c := newActionReconciler(t, action, rob)
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "r1" {
		t.Fatalf("assignedRobot = %q, want r1: an unrelated model rollout must not withhold the robot", got.Status.AssignedRobot)
	}
}

// ── Filter 8: ZoneMaintenance paused / winding-down robots ───────────────────

// RFC-0001 candidate filter 8: "No active ZoneMaintenance resource must have the
// robot in status.pausedRobots[] or status.windingDownRobots[]."
//
// A paused robot is USUALLY caught by filter 1 as well, because ZoneMaintenance
// writes Robot.status.phase = Maintenance when it pauses one. Usually is not the
// contract: the two writes are separate, so a robot can be listed here while its
// phase write has not landed, and filter 8 is what makes the exclusion synchronous
// with the list the operator is actually reading.
func TestDispatch_Filter8_ExcludesPausedRobot(t *testing.T) {
	rob := idleCandidate("r1")
	r, c := newActionReconciler(t,
		pendingInBand("t1", fleetv1.ActionPriorityNormal), rob,
		activeMaintenance("zm-1", []string{"r1"}, nil))
	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: a robot in an active window's pausedRobots[] took the action", ft.Status.AssignedRobot)
	}
	if !strings.Contains(ft.Status.Message, "filter 8") {
		t.Errorf("status.message = %q; want it to name filter 8", ft.Status.Message)
	}
}

// The preemption hole, and the reason this filter cannot be left to filter 1. The
// preemption search deliberately skips the phase check to find a busy robot, so a
// robot ZoneMaintenance is DRAINING — InProgress, holding an action, not estopped —
// satisfies every guard the search applies. Without filter 8 a Critical action
// preempts the very action whose completion is what lets the robot be paused.
func TestDispatch_Filter8_WindingDownRobotIsNotAPreemptionVictim(t *testing.T) {
	victimRobot := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "low")
	pct := int32(90)
	victimRobot.Status.BatteryPercent = &pct

	r, c := newActionReconciler(t,
		criticalPending("crit"),
		bandAction("low", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityLow, 1,
			&metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}),
		victimRobot,
		activeMaintenance("zm-1", nil, []string{"r1"}))
	reconcileAction(t, r, "crit")

	if got := getAction(t, c, "crit"); got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: a Critical action preempted a robot being drained for maintenance", got.Status.AssignedRobot)
	}
	if got := getAction(t, c, "low"); got.Status.Phase == fleetv1.ActionPhasePreempted {
		t.Fatalf("the wind-down action was preempted; draining the robot can now never complete")
	}
}

// A window that is not Active holds nobody. Scheduled (not yet started) and
// Completed (already resumed) windows must not withhold a robot, or a stale object
// left on the cluster would idle it forever.
func TestDispatch_Filter8_InactiveWindowDoesNotWithhold(t *testing.T) {
	for _, phase := range []fleetv1.ZoneMaintenancePhase{
		fleetv1.ZoneMaintenancePhaseScheduled,
		fleetv1.ZoneMaintenancePhaseCompleted,
		fleetv1.ZoneMaintenancePhaseCancelled,
	} {
		t.Run(string(phase), func(t *testing.T) {
			zm := activeMaintenance("zm-1", []string{"r1"}, nil)
			zm.Status.Phase = phase
			r, c := newActionReconciler(t,
				pendingInBand("t1", fleetv1.ActionPriorityNormal), idleCandidate("r1"), zm)
			reconcileAction(t, r, "t1")

			if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "r1" {
				t.Fatalf("assignedRobot = %q, want r1: a %s window must not withhold a robot", got.Status.AssignedRobot, phase)
			}
		})
	}
}

// ── Filter 4: the battery floor ──────────────────────────────────────────────

// RFC-0001 candidate filter 4: "Robot.status.batteryPercent must be ≥
// FleetAction.spec.minBatteryPct (or Robot.spec.constraints.minBatteryPctForAction,
// whichever is higher)."
func TestDispatch_Filter4_ExcludesBelowActionMinBatteryPct(t *testing.T) {
	rob := idleCandidate("r1")
	low := int32(20)
	rob.Status.BatteryPercent = &low

	action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
	floor := int32(60)
	action.Spec.MinBatteryPct = &floor

	r, c := newActionReconciler(t, action, rob)
	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: a robot at 20%% took an action requiring 60%%", ft.Status.AssignedRobot)
	}
	if !strings.Contains(ft.Status.Message, "filter 4") {
		t.Errorf("status.message = %q; want it to name filter 4", ft.Status.Message)
	}
}

// The robot-side floor is the OTHER operand of the specified max(), and it is
// authoritative on its own: an action that sets no minBatteryPct still may not
// take a robot below the floor its class declares.
func TestDispatch_Filter4_ExcludesBelowRobotMinBatteryPctForAction(t *testing.T) {
	rob := idleCandidate("r1")
	low := int32(20)
	rob.Status.BatteryPercent = &low
	robotFloor := int32(50)
	rob.Spec.Constraints = &fleetv1.ClassConstraints{MinBatteryPctForAction: &robotFloor}

	r, c := newActionReconciler(t, pendingInBand("t1", fleetv1.ActionPriorityNormal), rob)
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: the robot's own minBatteryPctForAction floor was ignored", got.Status.AssignedRobot)
	}
}

// Whichever is HIGHER: a permissive action does not lower a robot's floor, and a
// permissive robot does not lower the action's.
func TestDispatch_Filter4_HigherFloorWins(t *testing.T) {
	cases := []struct {
		name                    string
		battery                 int32
		actionFloor, robotFloor int32
		wantAssigned            bool
	}{
		{"action floor is higher and bites", 40, 60, 10, false},
		{"robot floor is higher and bites", 40, 10, 60, false},
		{"battery clears both", 70, 60, 50, true},
		{"battery equals the higher floor", 60, 60, 50, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rob := idleCandidate("r1")
			b := tc.battery
			rob.Status.BatteryPercent = &b
			rf := tc.robotFloor
			rob.Spec.Constraints = &fleetv1.ClassConstraints{MinBatteryPctForAction: &rf}

			action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
			af := tc.actionFloor
			action.Spec.MinBatteryPct = &af

			r, c := newActionReconciler(t, action, rob)
			reconcileAction(t, r, "t1")

			assigned := getAction(t, c, "t1").Status.AssignedRobot == "r1"
			if assigned != tc.wantAssigned {
				t.Fatalf("assigned=%v want %v (battery %d, action floor %d, robot floor %d)",
					assigned, tc.wantAssigned, tc.battery, tc.actionFloor, tc.robotFloor)
			}
		})
	}
}

// Fail closed on an unknown reading. status.batteryPercent is a pointer precisely
// so an unreported charge is distinguishable from 0 ("explicit presence on
// safety-relevant scalars", AGENTS.md). A robot that cannot prove it clears the
// floor has not cleared it.
func TestDispatch_Filter4_UnreportedBatteryFailsClosed(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Status.BatteryPercent = nil

	action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
	floor := int32(10)
	action.Spec.MinBatteryPct = &floor

	r, c := newActionReconciler(t, action, rob)
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: a robot reporting no battery was treated as clearing a 10%% floor", got.Status.AssignedRobot)
	}
}

// No floor anywhere is no filter. A robot reporting nothing is still a candidate
// for an action that asks for nothing — filter 4 must not become a de-facto
// requirement that every robot report a battery level.
func TestDispatch_Filter4_NoFloorAdmitsUnreportedBattery(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Status.BatteryPercent = nil

	r, c := newActionReconciler(t, pendingInBand("t1", fleetv1.ActionPriorityNormal), rob)
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "r1" {
		t.Fatalf("assignedRobot = %q, want r1: no floor is set, so filter 4 must not fire", got.Status.AssignedRobot)
	}
}

// ── Filter 2: zone leaf-set containment ──────────────────────────────────────

// The defect this item is named for. An action targeting a PARENT zone matched no
// robot at all, because robots carry leaf zones and a leaf string never equals its
// parent's. Resolving the action's zone down the tree is what makes a parent-zone
// action dispatchable.
func TestDispatch_Filter2_ParentZoneActionMatchesLeafDescendant(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Spec.Zone = "aisle-1"
	rob.Status.CurrentZone = "aisle-1"

	r, c := newActionReconciler(t,
		zonedPending("t1", "floor-1"), rob,
		fleetZone("floor-1", ""), fleetZone("aisle-1", "floor-1"), fleetZone("aisle-2", "floor-1"))
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "r1" {
		t.Fatalf("assignedRobot = %q, want r1: a parent-zone action still matches no robot (message: %q)",
			got.Status.AssignedRobot, got.Status.Message)
	}
}

// The second half of the defect: the filter compared spec.zone, the robot's
// CONFIGURED zone, not status.currentZone, where it actually is. A robot
// configured for aisle-1 but presently in aisle-9 passed the filter for aisle-1.
func TestDispatch_Filter2_MatchesCurrentZoneNotSpecZone(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Spec.Zone = "aisle-1"
	rob.Status.CurrentZone = "aisle-9"

	r, c := newActionReconciler(t,
		zonedPending("t1", "aisle-1"), rob,
		fleetZone("aisle-1", ""), fleetZone("aisle-9", ""))
	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: a robot physically in aisle-9 took an aisle-1 action on the strength of its spec.zone",
			ft.Status.AssignedRobot)
	}
	if !strings.Contains(ft.Status.Message, "filter 2") {
		t.Errorf("status.message = %q; want it to name filter 2", ft.Status.Message)
	}
}

// A robot whose position matches no polygon reports currentZone = "". RFC-0001
// (#robot, between-zone positions) is explicit that the Scheduler withholds new
// work from it: it cannot confirm the zone. Fail closed.
func TestDispatch_Filter2_UnmatchedPositionIsWithheld(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Spec.Zone = "aisle-1"
	rob.Status.CurrentZone = ""

	r, c := newActionReconciler(t, zonedPending("t1", "aisle-1"), rob, fleetZone("aisle-1", ""))
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: a robot whose position matches no zone was dispatched to", got.Status.AssignedRobot)
	}
}

// An action naming no zone is not zone-filtered at all, so a robot that has not
// resolved a currentZone is still a candidate for it. Filter 2 must not become a
// blanket requirement that every robot resolve a zone.
func TestDispatch_Filter2_UnzonedActionIgnoresCurrentZone(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Status.CurrentZone = ""

	r, c := newActionReconciler(t, pendingInBand("t1", fleetv1.ActionPriorityNormal), rob)
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "r1" {
		t.Fatalf("assignedRobot = %q, want r1: an action naming no zone must not be zone-filtered", got.Status.AssignedRobot)
	}
}

// A leaf-zone action still matches a robot in that leaf — the ordinary case, and
// the one every deployment depends on. Resolving downward from a leaf yields the
// leaf itself.
func TestDispatch_Filter2_LeafZoneActionStillMatches(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Status.CurrentZone = "aisle-1"

	r, c := newActionReconciler(t,
		zonedPending("t1", "aisle-1"), rob,
		fleetZone("floor-1", ""), fleetZone("aisle-1", "floor-1"))
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "r1" {
		t.Fatalf("assignedRobot = %q, want r1 (message: %q)", got.Status.AssignedRobot, got.Status.Message)
	}
}

// A sibling leaf is NOT in the resolved set. Walking downward must not widen into
// the whole tree.
func TestDispatch_Filter2_SiblingLeafIsExcluded(t *testing.T) {
	rob := idleCandidate("r1")
	rob.Status.CurrentZone = "aisle-2"

	r, c := newActionReconciler(t,
		zonedPending("t1", "aisle-1"), rob,
		fleetZone("floor-1", ""), fleetZone("aisle-1", "floor-1"), fleetZone("aisle-2", "floor-1"))
	reconcileAction(t, r, "t1")

	if got := getAction(t, c, "t1"); got.Status.AssignedRobot != "" {
		t.Fatalf("assignedRobot = %q: a robot in a SIBLING leaf took the action", got.Status.AssignedRobot)
	}
}
