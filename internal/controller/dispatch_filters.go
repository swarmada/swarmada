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
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// The CROSS-OBJECT candidate filters (RFC-0001 {{ref:control-plane-scheduler}}).
//
// These are the filters that cannot live in internal/scheduler, because deciding
// them needs a read of an object that is neither the robot nor the action: the
// FleetZone tree (filter 2), the maintenance windows (filter 8), the serving
// FleetAdapter (filter 9). Filters 4 and 7 need no extra read but belong here for a
// different reason — RFC-0001 filter 6 says a robot pinned by spec.robotSelector
// skips filters 2, 3 and 5 but NOT 4, 7 and 8, and the scheduler's matcher
// short-circuits on the pin, so a battery or model gate placed there would fail open
// on exactly the path an operator most expects it to hold.
//
// NOTHING HERE WRITES. A candidate filter decides eligibility; it does not
// transition a robot. Every filter below is a pure read plus a reason string, and
// this file must not become the first thing on the dispatch path to patch a
// resource — a robot that is mid-model-update, drained for maintenance, or flat is
// not made so by having been passed over for an action.

// RFC-0001 candidate filter numbers for the filters implemented in this package.
// Filters 1, 3, 5 and 6 are numbered in internal/scheduler/exclusion.go.
const (
	filterZoneLeafSet      = 2
	filterMinBattery       = 4
	filterModelNotUpdating = 7
	filterZoneMaintenance  = 8
	filterAdapterReady     = 9
	filterEstop            = 10

	// filterOperatingConstraint is 12. Filter 11 has NO number here because it is not a
	// candidate filter: it eliminates the action, so it never produces a
	// scheduler.Exclusion and never appears in the no-candidate tally. 12 does both.
	filterOperatingConstraint = 12

	// filterResumeWindow is NOT one of the ten. The post-clear estop resume window
	// is withheld by a gate that sits beside filter 10 rather than inside it
	// (adapter_readiness.go), so it reports as 0 and is named, never numbered.
	filterResumeWindow = 0

	// filterAdapterRefusedAction is NOT a candidate filter either, and it is NEGATIVE
	// rather than 0 for a reason: 0 already means the resume window, and giving one
	// sentinel two meanings would make the no-candidate tally ambiguous in the one place
	// it exists to be read.
	//
	// It reports a robot whose adapter has already REFUSED this specific assignment as
	// INVALID_ACTION. That is not a property of the robot — it is a property of the
	// (robot, action) pair, learned from the wire — so no numbered filter could have
	// produced it: every one of the ten decides from state that exists before the
	// assignment is offered.
	filterAdapterRefusedAction = -1
)

// filterClass is the short operator-facing name of each filter, used to build the
// no-candidate status.message. One phrase per filter, and stable: the message is
// written on a status patch that a Pending action repeats every requeue, so a
// phrase that varied with the robot would turn every reconcile into an etcd write.
var filterClass = map[int]string{
	scheduler.FilterPhaseIdle:          "not Idle",
	filterZoneLeafSet:                  "not in the action's zone",
	scheduler.FilterRequiredCapability: "missing a required capability",
	filterMinBattery:                   "below the action's battery floor",
	scheduler.FilterParametricConstr:   "unable to meet a parametric constraint",
	scheduler.FilterRobotSelector:      "not the robot the action pins",
	filterModelNotUpdating:             "mid model update",
	filterZoneMaintenance:              "held by an active maintenance window",
	filterAdapterReady:                 "served by an adapter unfit for dispatch",
	filterEstop:                        "under an active emergency stop",
	filterOperatingConstraint:          "served by an adapter that does not honour a zone operating rule",
	filterResumeWindow:                 "in the post-clear estop resume window",
	filterAdapterRefusedAction:         "served by an adapter that refused this action as invalid",
	scheduler.FilterTerminating:        "being deleted",
	filterRobotIDConflict:              "sharing its robot-id with another Robot",
}

// dispatchFilterState memoizes the cluster reads the cross-object filters need, so
// N robots cost one FleetZone list and one ZoneMaintenance list per call rather than
// per candidate. It is built once per filterDispatchEligible call and thrown away:
// caching it across reconciles would make the filters read stale topology, which is
// the failure mode filter 9 exists to prevent for adapters.
type dispatchFilterState struct {
	// zoneLeaves is the resolved leaf set for the action's zone (filter 2), or nil
	// when the action names no zone.
	zoneLeaves map[string]bool
	// drained maps a robot name to the maintenance window holding it (filter 8).
	drained map[string]string
	// maintenanceUnread is set when the ZoneMaintenance list could not be read. Filter 8
	// then withholds every candidate: the list is what names the robots a window holds,
	// so without it no robot can be shown to be clear of one.
	maintenanceUnread error
	// operating is the effective OPERATING rule set in force at each zone of the
	// action's RESOLVED LEAF SET (filter 12, ADR-0058 Amendment 2), or nil when that
	// filter is inert — no zone, no restrictions declared in the namespace, or no
	// operating rule anywhere in the reachable subtree.
	operating *operatingRuleSet
}

// newDispatchFilterState performs the two list reads the cross-object filters need.
//
// A read error is not fatal, and the two lists fail in opposite directions.
//
//   - A failed FleetZone list skips filter 2, and filter 11 holds the whole action on
//     the same error (see below), so nothing is dispatched on the skipped filter.
//   - A failed ZoneMaintenance list FAILS FILTER 8 CLOSED: every candidate is
//     withheld for this pass, the no-candidate path requeues the action, and the
//     error is logged at the default level. The list is the only record of which
//     robots a window holds, and a robot a window is draining can still be Idle (the
//     window has not flipped it yet) or InProgress (the preemption search skips
//     filter 1). Skipping the filter would dispatch into a zone a technician was told
//     was drained. A transient list error costs a requeue, not a wrong dispatch.
func (r *FleetActionReconciler) newDispatchFilterState(
	ctx context.Context, action *fleetv1.FleetAction,
) (*dispatchFilterState, []string, *zoneRestrictionVerdict) {
	st := &dispatchFilterState{}
	var degraded []string
	var restrictions *zoneRestrictionVerdict

	if action.Spec.Zone != "" {
		var zones fleetv1.FleetZoneList
		if err := r.List(ctx, &zones, client.InNamespace(action.Namespace)); err != nil {
			degraded = append(degraded, fmt.Sprintf("filter 2 (zone) not applied: listing FleetZones: %v", err))

			// FILTER 11 FAILS CLOSED ON THIS EXACT ERROR, AND FILTER 2 ONE LINE ABOVE
			// DOES NOT. Both read the list that just failed; they draw opposite
			// conclusions from the failure, and both are right.
			//
			// A failed FleetZone list proves nothing about any ROBOT, so filter 2 skips
			// and the action requeues: latency, not a wrong dispatch. It means something
			// different to filter 11, because filter 11 is not about a robot. It is
			// about whether the operator has forbidden this work HERE, and a failed
			// list means that question is UNRESOLVABLE — not answered "no".
			// An unresolvable restriction is not an absent restriction (ADR-0058 §6).
			//
			// Reusing the skip-and-continue discipline of the two filters above would
			// convert one transient list error into a dispatch into a denied zone. That
			// is the whole defect this filter exists to close, arriving through the back
			// door of a retry. The downstream Matter adapter reached the same rule
			// independently and for the same reason: an unreadable or missing policy file
			// refuses everything rather than defaulting permissive.
			//
			// FILTER 12 INHERITS THIS EXACTLY and adds nothing to it. It is robot-scoped
			// where filter 11 is action-scoped, but it draws the SAME conclusion from this
			// SAME failed read, for the same reason and by the same mechanism: st.operating
			// is left nil below, no candidate is judged against a rule set nobody could
			// read, and the action is held by the verdict built here rather than dispatched
			// against a half-known chain. A robot that cannot be shown to honour a
			// constraint has not honoured it, and a constraint that could not be READ
			// cannot be shown to be honoured by anyone.
			//
			// DO NOT "fix" this inconsistency. It is the point.
			restrictions = &zoneRestrictionVerdict{
				refuse: true, terminal: false,
				reason: fleetv1.ActionFailureZoneRestrictionsUnresolved,
				message: fmt.Sprintf("the zone restriction set for %q could not be read (listing "+
					"FleetZones: %v). NO DECISION HAS BEEN MADE about this action: it is held, "+
					"not judged, and is not dispatched while the restrictions are unresolvable",
					action.Spec.Zone, err),
			}
		} else {
			st.zoneLeaves = resolveZoneLeaves(action.Spec.Zone, zones.Items)

			// Filter 11 — action-scoped. It may refuse the action outright, and it
			// NARROWS the leaf set filter 2 then matches robots against: a denied
			// descendant is removed here, and named on the action's status by the caller.
			restrictions = evaluateZoneRestrictions(action, zones.Items)
			if restrictions != nil && restrictions.leaves != nil {
				st.zoneLeaves = restrictions.leaves
			}

			// Filter 12 — robot-scoped. The rule SET is action-scoped and resolved once
			// here; which leaf's rules a candidate is judged against, and who can honour
			// them, is decided per candidate in excludedBy.
			//
			// IT IS GIVEN st.zoneLeaves, WHICH IS FILTER 2'S INPUT AFTER FILTER 11'S
			// NARROWING. That is the whole cost of ADR-0058 Amendment 2's widening: the
			// leaf set is already resolved for filter 2 in this same call, and a leaf's
			// projection is already the fold of itself and its ancestors (FZ-R1, FZ-R4),
			// so there is no second walk, no second listing, and nothing to merge with the
			// target chain. It is given zones.Items as well, because a zone containing a
			// candidate binds it from outside the subtree too (FZ-R10): those projections
			// are folded per candidate, from this same listing.
			//
			// Computed only when filter 11 did not refuse, and the ordering is load
			// bearing rather than an optimisation. Filter 11 refuses on exactly the two
			// inputs that would make this set unsafe to trust — an unknown target zone
			// and an unresolvable chain — so a nil set here can never mean "no rules"
			// when the truth is "rules unknown". Making the dependency explicit keeps it
			// from being lost the next time this branch is edited.
			if restrictions == nil || !restrictions.refuse {
				st.operating = operatingConstraintsInForce(action, zones.Items, st.zoneLeaves)
				if restrictions != nil {
					restrictions.operating = st.operating
				}
			}
		}
	}

	var windows fleetv1.ZoneMaintenanceList
	if err := r.List(ctx, &windows, client.InNamespace(action.Namespace)); err != nil {
		st.maintenanceUnread = err
		degraded = append(degraded, fmt.Sprintf("filter 8 (maintenance) could not be read, so every "+
			"candidate is withheld until it can: listing ZoneMaintenances: %v", err))
		log.FromContext(ctx).Error(err, "candidate filter 8 failed closed: the ZoneMaintenance list "+
			"could not be read, so no robot is dispatched to this action until it can",
			"action", action.Name)
	} else {
		st.drained = drainedRobots(windows.Items)
	}

	return st, degraded, restrictions
}

// resolveZoneLeaves resolves a FleetAction's spec.zone to the set of LEAF zones at
// or below it, by walking the zone tree downward (RFC-0001 candidate filter 2).
//
// Leaf-ness is computed from the listed tree — a zone with no children — rather than
// read from FleetZone.status.isLeaf. The two are the same quantity by construction
// (the Zone Controller writes `IsLeaf = len(children) == 0`, zone_controller.go:159),
// but status.isLeaf is an omitempty bool, so an unwritten status and a genuine
// non-leaf are indistinguishable on the wire. Deriving it makes the filter
// independent of whether the Zone Controller has reconciled the zone yet, which
// matters because a newly created zone would otherwise silently drop out of the set.
//
// A zone that names no known FleetZone resolves to itself: it has no children, so it
// is its own leaf. That keeps an action targeting a zone the operator has not yet
// created behaving as it did before this filter existed, rather than matching the
// whole namespace.
func resolveZoneLeaves(zone string, zones []fleetv1.FleetZone) map[string]bool {
	childrenOf, _ := buildTree(zones)

	leaves := map[string]bool{}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return // cycle guard; admission rejects cycles, this survives one anyway
		}
		seen[name] = true
		kids := childrenOf[name]
		if len(kids) == 0 {
			leaves[name] = true
			return
		}
		for _, c := range kids {
			walk(c)
		}
	}
	walk(zone)
	return leaves
}

// drainedRobots maps every robot named by an ACTIVE ZoneMaintenance window to that
// window's name (RFC-0001 candidate filter 8: status.pausedRobots[] and
// status.windingDownRobots[]).
//
// Only Active windows hold a robot. A Scheduled window has not started and a
// Completed or Cancelled one has already resumed its robots; a terminal
// ZoneMaintenance object is NOT garbage-collected (it stays until an operator
// deletes it, RFC-0001 #zonemaintenance), so honouring a non-Active window would let
// a forgotten object idle a robot indefinitely.
//
// Windows are visited in name order and the first claim wins, so a robot named by
// two overlapping windows always reports the same one.
func drainedRobots(windows []fleetv1.ZoneMaintenance) map[string]string {
	sorted := make([]*fleetv1.ZoneMaintenance, 0, len(windows))
	for i := range windows {
		if windows[i].Status.Phase == fleetv1.ZoneMaintenancePhaseActive {
			sorted = append(sorted, &windows[i])
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	out := map[string]string{}
	for _, zm := range sorted {
		for _, p := range zm.Status.PausedRobots {
			if _, held := out[p.Name]; !held {
				out[p.Name] = zm.Name + " (paused)"
			}
		}
		for _, w := range zm.Status.WindingDownRobots {
			if _, held := out[w.Name]; !held {
				out[w.Name] = zm.Name + " (winding down)"
			}
		}
	}
	return out
}

// excludedByCrossObjectFilter returns the first cross-object filter that eliminates
// robot, or nil. Order is by cost, cheapest first: filters 4 and 7 read only the
// robot, filters 2 and 8 read the state already listed for this call, and filter 9
// (in filterDispatchEligible) issues a per-adapter Get.
//
// Filter 2 is skipped for a robot the action PINS, per RFC-0001 filter 6: pinning is
// the operator asserting the robot can do the work. Filters 4, 7 and 8 are not
// skipped — the specification lists them as still applying to a pinned robot, and
// they are the three that protect the robot rather than the work.
func (st *dispatchFilterState) excludedBy(robot *fleetv1.Robot, action *fleetv1.FleetAction) *scheduler.Exclusion {
	if ex := excludedByBatteryFloor(robot, action); ex != nil {
		return ex
	}
	if ex := excludedByModelUpdate(robot, action); ex != nil {
		return ex
	}
	if st.maintenanceUnread != nil {
		return &scheduler.Exclusion{Filter: filterZoneMaintenance,
			Reason: fmt.Sprintf("the ZoneMaintenance list could not be read, so whether a window "+
				"holds this robot is unknown: %v", st.maintenanceUnread)}
	}
	if window, held := st.drained[robot.Name]; held {
		return &scheduler.Exclusion{Filter: filterZoneMaintenance,
			Reason: fmt.Sprintf("robot is held by the active ZoneMaintenance %s", window)}
	}
	pinned := action.Spec.RobotSelector != "" && robot.Name == action.Spec.RobotSelector
	if st.zoneLeaves != nil && !pinned {
		if ex := excludedByZone(robot, action, st.zoneLeaves); ex != nil {
			return ex
		}
	}

	// Filter 12 — adapter operating-constraint support. Applied AFTER filter 2 so a
	// robot that is not even in the action's zone is reported as being in the wrong
	// place rather than as unable to honour a rule it was never going to meet.
	//
	// A PINNED ROBOT IS NOT EXEMPT, unlike filter 2 one line above. RFC-0001 filter 6
	// lets a pin skip filters 2, 3 and 5 because pinning is the operator asserting the
	// robot can do the WORK — a judgement an operator is entitled to make. Whether a
	// vendor's adapter honours a camera constraint is not that kind of judgement: it is
	// a property of somebody else's build, and no operator can assert it into being.
	// Exempting a pin here would make spec.robotSelector an off switch for the zone's
	// rule, and a restriction with an off switch is not a restriction.
	//
	// THE RULES ARE THOSE IN FORCE AT THE LEAF THE ROBOT OCCUPIES, not at the target
	// (ADR-0058 Amendment 2). Filter 2 one line above admitted this robot from the
	// action's SUBTREE, so judging it against the target's chain alone would leave a rule
	// declared below the target enforced nowhere — filter 2 admits, filter 12 reads an
	// empty projection, and a robot is dispatched into the ICU with a camera
	// (ADR-0058 Amendment 2 §A2.1).
	// The leaf's own projection already folds in every ancestor including the target, so
	// the target chain is a lookup and not a merge.
	//
	// AND THE RULES OF EVERY OTHER ZONE CONTAINING THE ROBOT (FZ-R10, FZ-R11). currentZone
	// is the arbitrated winner of an overlap; the loser's rules bind the robot too, and
	// judging it at the winner alone is the permissive direction. forRobot folds
	// status.containingZones[] in, and names any of them whose rules could not be read,
	// which withholds the robot rather than judging it against a set weaker than the truth.
	inForce, unresolved := st.operating.forRobot(robot)
	if ex := excludedByUnreadableContainment(unresolved); ex != nil {
		return ex
	}
	if ex := excludedByOperatingConstraint(robot, inForce); ex != nil {
		return ex
	}
	return nil
}

// excludedByZone applies candidate filter 2: Robot.status.currentZone must be within
// the leaf set resolved from FleetAction.spec.zone.
//
// It reads status.currentZone, never spec.zone. spec.zone is where an operator has
// declared the robot belongs; currentZone is where the Zone Controller has resolved
// it to actually be. Dispatching zone-scoped work on the declared value sends an
// action to a robot that has left the zone, which is the case the whole
// containment derivation exists to detect.
//
// An empty currentZone is EXCLUDED, and that is the specified behaviour rather than
// a conservative choice: a robot whose position matches no polygon reports
// currentZone = "" and RFC-0001 (#robot, "Between-zone positions") states that the
// Scheduler withholds new work from it because it cannot confirm the zone. It is
// scoped to zoned actions only — an action naming no zone never reaches this filter,
// so a namespace with no zone geometry keeps dispatching exactly as before.
func excludedByZone(robot *fleetv1.Robot, action *fleetv1.FleetAction, leaves map[string]bool) *scheduler.Exclusion {
	if robot.Status.CurrentZone == "" {
		return &scheduler.Exclusion{Filter: filterZoneLeafSet,
			Reason: fmt.Sprintf("robot reports no status.currentZone, so its presence in %q cannot be confirmed",
				action.Spec.Zone)}
	}
	if leaves[robot.Status.CurrentZone] {
		return nil
	}
	return &scheduler.Exclusion{Filter: filterZoneLeafSet,
		Reason: fmt.Sprintf("robot is in zone %q, which is not among the leaf zones of the action's zone %q (%s)",
			robot.Status.CurrentZone, action.Spec.Zone, leafSetText(leaves))}
}

// leafSetText renders a resolved leaf set in a stable order for a log line.
func leafSetText(leaves map[string]bool) string {
	names := make([]string, 0, len(leaves))
	for n := range leaves {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// excludedByBatteryFloor applies candidate filter 4: status.batteryPercent must be
// at or above max(FleetAction.spec.minBatteryPct,
// Robot.spec.constraints.minBatteryPctForAction).
//
// Both operands are authoritative and neither defers to the other — the higher wins,
// which is what makes the two spellings safe to have. The action's field is a
// per-work requirement ("this job needs 60% to finish"); the robot's is a per-robot
// floor inherited from its class ("this machine refuses work below 20%"). Reading
// only one of them would let the other be set and ignored, which is the defect the
// pair was reported for.
//
// An UNREPORTED battery fails the filter whenever a floor is set. status.batteryPercent
// is a pointer precisely so an unknown reading is distinguishable from 0 (AGENTS.md,
// "explicit presence on safety-relevant scalars"); a robot that cannot prove it clears
// the floor has not cleared it. With no floor set anywhere the filter does not fire at
// all, so an unreported battery costs nothing to an action that asked for nothing.
func excludedByBatteryFloor(robot *fleetv1.Robot, action *fleetv1.FleetAction) *scheduler.Exclusion {
	floor, source := effectiveBatteryFloor(robot, action)
	if floor == nil {
		return nil
	}
	if robot.Status.BatteryPercent == nil {
		return &scheduler.Exclusion{Filter: filterMinBattery,
			Reason: fmt.Sprintf("robot reports no status.batteryPercent and cannot prove it clears the %d%% floor (%s)",
				*floor, source)}
	}
	if *robot.Status.BatteryPercent < *floor {
		return &scheduler.Exclusion{Filter: filterMinBattery,
			Reason: fmt.Sprintf("robot battery is %d%%, below the %d%% floor (%s)",
				*robot.Status.BatteryPercent, *floor, source)}
	}
	return nil
}

// effectiveBatteryFloor returns the higher of the action's and the robot's declared
// floors, and which one it came from, or nil when neither declares one.
func effectiveBatteryFloor(robot *fleetv1.Robot, action *fleetv1.FleetAction) (*int32, string) {
	var robotFloor *int32
	if robot.Spec.Constraints != nil {
		robotFloor = robot.Spec.Constraints.MinBatteryPctForAction
	}
	actionFloor := action.Spec.MinBatteryPct

	switch {
	case actionFloor == nil && robotFloor == nil:
		return nil, ""
	case robotFloor == nil:
		return actionFloor, "FleetAction.spec.minBatteryPct"
	case actionFloor == nil:
		return robotFloor, "Robot.spec.constraints.minBatteryPctForAction"
	case *actionFloor >= *robotFloor:
		return actionFloor, "FleetAction.spec.minBatteryPct, the higher of the two floors"
	default:
		return robotFloor, "Robot.spec.constraints.minBatteryPctForAction, the higher of the two floors"
	}
}

// excludedByModelUpdate applies candidate filter 7: no model that grants a required
// capability may be Updating.
//
// Why this is not already covered by filter 3. A model-driven capability declared in
// spec.capabilities with a providingModel DOES derive to Inactive while its model is
// updating (robot_controller.go, the model gate), and filter 3 then drops the robot.
// Two things escape that:
//
//  1. A capability granted through status.modelGrantedCapabilities — the path the OTA
//     manager writes after a rollout — is unioned into status.capabilities as Active
//     UNCONDITIONALLY, without consulting the model's runtime status. For those the
//     derived capability says Active for the whole update window.
//  2. The derived exclusion is eventually consistent. It holds only once the Robot
//     controller has re-derived; a dispatch between the installedModels write and
//     that reconcile is unfiltered. This filter is synchronous with the assignment.
//
// Scoped to models granting a REQUIRED capability, as specified. A broader reading —
// any model updating withholds the robot — would idle a fleet during any unrelated
// rollout, which is neither what the specification says nor what an operator running
// a staged rollout wants.
func excludedByModelUpdate(robot *fleetv1.Robot, action *fleetv1.FleetAction) *scheduler.Exclusion {
	if len(action.Spec.RequiredCapabilities) == 0 || len(robot.Status.InstalledModels) == 0 {
		return nil
	}
	updating := map[string]bool{}
	for _, m := range robot.Status.InstalledModels {
		if m.Status == fleetv1.ModelStatusUpdating {
			updating[m.Name] = true
		}
	}
	if len(updating) == 0 {
		return nil
	}

	required := make(map[string]bool, len(action.Spec.RequiredCapabilities))
	for _, c := range action.Spec.RequiredCapabilities {
		required[c] = true
	}

	var offenders []string
	note := map[string]string{}
	add := func(model, capability string) {
		if !updating[model] || note[model] != "" {
			return
		}
		note[model] = capability
		offenders = append(offenders, model)
	}
	// All three declarations of "this model grants this capability" are consulted,
	// because a robot may carry any of them: the class-inherited spec entry, the
	// rollout-written status entry, and the model-driven capability's back-reference.
	for _, m := range robot.Spec.InstalledModels {
		for _, granted := range m.GrantsCapabilities {
			if required[granted] {
				add(m.Name, granted)
			}
		}
	}
	for _, mg := range robot.Status.ModelGrantedCapabilities {
		for _, granted := range mg.Capabilities {
			if required[granted] {
				add(mg.ModelName, granted)
			}
		}
	}
	for _, capDef := range robot.Spec.Capabilities {
		if capDef.ProvidingModel != "" && required[capDef.Name] {
			add(capDef.ProvidingModel, capDef.Name)
		}
	}
	if len(offenders) == 0 {
		return nil
	}

	sort.Strings(offenders) // deterministic: an unchanged robot yields an unchanged reason
	parts := make([]string, 0, len(offenders))
	for _, m := range offenders {
		parts = append(parts, fmt.Sprintf("%s (grants %s)", m, note[m]))
	}
	return &scheduler.Exclusion{Filter: filterModelNotUpdating,
		Reason: "robot is mid model update for a model granting a required capability: " + strings.Join(parts, ", ")}
}

// ── The no-candidate message (the operator-facing half) ───────────────────────

// noCandidateMessage explains, on the action's own status, why a Pending action
// found no robot.
//
// The message this replaced was the scheduler's single sentinel — "no eligible robot
// available" — for every cause. The reason a robot was withheld lives on a DIFFERENT
// object from the action (the robot's estopState, battery, currentZone, its
// FleetAdapter's phase), so an operator reading only the action could see a Pending
// action, a fleet of Idle-looking robots, and nothing at all connecting them. Naming
// the filter is what turns "it is not working" into a place to look.
//
// It is a TALLY, not a list of robots, and that is an RA-1 decision rather than a
// stylistic one. This message rides a status patch the dispatch path already makes
// once per reconcile of a Pending action, and a Pending action requeues on a timer:
// a message that named robots would churn as candidates moved between filters and
// turn every requeue into an etcd write. Filters are reported in ascending order
// with a count each, so an unchanged fleet produces a byte-identical string and the
// patch is empty.
//
// survivors are the robots that passed the cross-object filters and were still not
// selected; they are classified here against the pure filters (1, 3, 5, 6) rather
// than at the choke point, because those need no client read and evaluating them per
// candidate per reconcile in the log would be noise. Anything that survives THAT is
// a robot every filter admitted and whose adapter declined the concrete instance.
func noCandidateMessage(
	action *fleetv1.FleetAction,
	withheld []dispatchExclusion,
	survivors []fleetv1.Robot,
	acceptDegraded bool,
	degraded []string,
) string {
	total := len(withheld) + len(survivors)
	if total == 0 {
		return "no eligible robot: the namespace holds no Robot resources"
	}

	counts := map[int]int{}
	for _, e := range withheld {
		counts[e.Filter]++
	}
	var admitted int
	for i := range survivors {
		if ex := scheduler.ExcludedByForAssignment(&survivors[i], action, acceptDegraded); ex != nil {
			counts[ex.Filter]++
			continue
		}
		admitted++
	}

	keys := make([]int, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	// Ascending by filter number, with the two unnumbered classes (0) last, so the
	// same fleet always renders the same sentence.
	sort.Slice(keys, func(i, j int) bool { return messageRank(keys[i]) < messageRank(keys[j]) })

	parts := make([]string, 0, len(keys)+2)
	for _, k := range keys {
		class, named := filterClass[k]
		switch {
		case !named:
			parts = append(parts, fmt.Sprintf("%d ineligible", counts[k]))
		case k == filterResumeWindow:
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], class))
		default:
			parts = append(parts, fmt.Sprintf("%d %s (filter %d)", counts[k], class, k))
		}
	}
	if admitted > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d passed every filter but no adapter would serve this action instance", admitted))
	}

	msg := fmt.Sprintf("no eligible robot; of %d candidate robots: %s", total, strings.Join(parts, ", "))
	if len(degraded) > 0 {
		sorted := append([]string(nil), degraded...)
		sort.Strings(sorted)
		msg += " — " + strings.Join(sorted, "; ")
	}
	return msg
}

// messageRank orders filters for the message: 1..10 ascending, then the unnumbered
// classes. filterResumeWindow shares 0 with the external-action precondition; both
// are named rather than numbered and both sort last.
func messageRank(filter int) int {
	if filter <= 0 {
		// The unnumbered classes sort last, after filter 12, so the numbered roster reads
		// in order. Negative sentinels rank just behind 0 rather than ahead of filter 1,
		// which is where a bare `return filter` would have put them.
		return 99 - filter
	}
	return filter
}
