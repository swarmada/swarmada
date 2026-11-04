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

// Package scheduler provides the robot-selection logic for FleetAction assignment.
//
// The Scheduler interface is intentionally thin so that future implementations
// (ML-based, cost-aware, multi-objective) can be swapped in without touching
// the FleetAction controller.
package scheduler

import (
	"errors"
	"sort"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ErrNoEligibleRobot is returned when no robot satisfies the action's constraints.
var ErrNoEligibleRobot = errors.New("no eligible robot available")

// ErrExternalAction is returned when SelectRobot is called for an action carrying
// spec.executor. Such an action is dispatched to a ComputeEndpoint, never to a
// robot (Draft 4 Option B) — this is a distinct outcome from ErrNoEligibleRobot
// (which means "no robot could do this work"), since here no robot search should
// ever have been attempted.
var ErrExternalAction = errors.New("action targets an executor endpoint, not a robot")

// Scheduler selects the best robot from a slice of candidates for a given action.
type Scheduler interface {
	// SelectRobot returns the robot that should be assigned to action, or an error
	// describing why no robot is eligible. acceptDegraded is the action's effective
	// degraded-capability acceptance, resolved by the caller from
	// spec.acceptDegradedCapabilities and the namespace default; when true, a
	// required capability in Degraded status satisfies the requirement.
	// preferSameManufacturer is the namespace-resolved
	// SwarmadaConfig.spec.scheduling.preferSameManufacturer flag; when true AND the
	// action carries a spec.preferredManufacturer hint, a manufacturer match is a
	// soft ranking tiebreak (never a hard filter). Both defaults are resolved by
	// the caller so the scheduler stays a pure decision function.
	// honorPreferredRobot is the namespace-resolved
	// SwarmadaConfig.spec.scheduling.honorPreferredRobot flag; when true AND the action
	// carries spec.preferredRobot, that robot ranks first — again a soft tiebreak,
	// never a hard filter.
	SelectRobot(action *fleetv1.FleetAction, candidates []fleetv1.Robot, acceptDegraded, preferSameManufacturer, honorPreferredRobot bool) (fleetv1.Robot, error)
}

// ── Default heuristic scheduler ───────────────────────────────────────────────

// DefaultScheduler implements a priority-weighted, capability-aware heuristic.
//
// Selection criteria (all must pass) — the RFC-0001 candidate filters this package
// owns, being the ones decidable from a (robot, action) pair with no client read:
//  1. filter 1 — robot phase is Idle
//  2. filter 3 — robot has all required capabilities
//  3. filter 5 — robot satisfies every parametric constraint
//  4. filter 6 — if action.Spec.RobotSelector is set, only that robot is considered
//
// The CROSS-OBJECT filters are not here and must not be: filter 2 (zone leaf-set
// containment) needs the FleetZone tree, filter 8 (ZoneMaintenance) needs the
// maintenance windows, filter 9 needs the FleetAdapter and filter 10 the robot's
// estop state read at dispatch time. All four are enforced at the FleetAction
// controller's single choke point (internal/controller/dispatch_filters.go), which
// covers the ordinary selection and the preemption search alike. A caller that
// builds its own candidate list MUST route it through that choke point.
//
// Among eligible robots the one with the highest battery level is chosen to
// avoid stranding a low-battery robot mid-action. When the action carries a
// preferredManufacturer hint and the namespace honours it, a manufacturer match
// is a higher-priority soft tiebreak above battery — it reorders the eligible
// set but never filters it (ADR-0022). A preferredRobot hint ranks above both
// (ADR-0034): it names one robot, so anything that could outrank it would make
// the field unobservable.
type DefaultScheduler struct{}

// NewDefaultScheduler constructs a DefaultScheduler.
func NewDefaultScheduler() *DefaultScheduler { return &DefaultScheduler{} }

// SelectRobot implements Scheduler.
func (s *DefaultScheduler) SelectRobot(action *fleetv1.FleetAction, candidates []fleetv1.Robot, acceptDegraded, preferSameManufacturer, honorPreferredRobot bool) (fleetv1.Robot, error) {
	// External actions (spec.executor set) are dispatched to a ComputeEndpoint,
	// never to a robot. This early return is the top-of-the-hottest-loop branch
	// the compute-action decision calls for: a
	// bug here means either an external action consumes a robot or a robot action
	// silently stops being scheduled, so it fires before any candidate is examined.
	if action.Spec.Executor != nil {
		return fleetv1.Robot{}, ErrExternalAction
	}

	eligible := make([]fleetv1.Robot, 0, len(candidates))

	for _, robot := range candidates {
		if !s.isEligible(&robot, action, acceptDegraded) {
			continue
		}
		eligible = append(eligible, robot)
	}

	if len(eligible) == 0 {
		return fleetv1.Robot{}, ErrNoEligibleRobot
	}

	// Rank the eligible set. Battery-descending is the base order (pick the
	// healthiest so a low-battery robot is not stranded mid-action). When the
	// namespace honours the manufacturer hint AND the action carries one, a
	// manufacturer match is a HIGHER-priority key than battery — a soft tiebreak
	// that reorders, never a filter that excludes (ADR-0022). Absent the hint or
	// the flag, ordering is byte-identical to battery-only. SliceStable keeps the
	// order deterministic on full ties.
	preferManufacturer := preferSameManufacturer && action.Spec.PreferredManufacturer != ""
	preferRobot := honorPreferredRobot && action.Spec.PreferredRobot != ""
	sort.SliceStable(eligible, func(i, j int) bool {
		// Preferred robot outranks manufacturer, which outranks battery. It is the most
		// specific thing the caller can say — it names ONE robot — so a hint that lost to
		// a battery difference would never be observable and the field would look broken.
		//
		// Still only a reordering. The preferred robot is not in `eligible` at all unless
		// it passed every filter, so this can never hand work to a robot that cannot do
		// it; when it is absent the fleet takes the action with no special case here.
		if preferRobot {
			pi := eligible[i].Name == action.Spec.PreferredRobot
			pj := eligible[j].Name == action.Spec.PreferredRobot
			if pi != pj {
				return pi
			}
		}
		if preferManufacturer {
			mi := eligible[i].Spec.Manufacturer == action.Spec.PreferredManufacturer
			mj := eligible[j].Spec.Manufacturer == action.Spec.PreferredManufacturer
			if mi != mj {
				return mi // a manufacturer match sorts before a non-match
			}
		}
		return batteryOf(&eligible[i]) > batteryOf(&eligible[j])
	})

	return eligible[0], nil
}

// isEligible checks the hard constraints for a (robot, action) pair that are decidable from the
// pair alone: filter 1 (phase) plus the capability/selector/parametric matcher.
//
// The cross-object filters (2, 8, 9, 10) are NOT checked here. They are enforced by the caller,
// in FleetActionReconciler.filterDispatchEligible, which is the single choke point for both the
// normal selection and the preemption search and is the only place that emits an operator-facing
// exclusion reason. A caller that builds its own candidate list MUST route it through that choke
// point.
func (s *DefaultScheduler) isEligible(robot *fleetv1.Robot, action *fleetv1.FleetAction, acceptDegraded bool) bool {
	return ExcludedByForAssignment(robot, action, acceptDegraded) == nil
}

// RobotMatchesAction reports whether a robot satisfies the candidate filters this
// package owns — filters 6, 3 and 5 — for everything EXCEPT the robot's phase. The
// Scheduler adds the Idle requirement on top of this (see isEligible);
// preemptor-band preemption (RFC-0001 #fleetaction-priority-semantics) reuses it to
// find a busy-but-otherwise-suitable robot, so the two paths can never diverge on
// what "eligible" means. Degraded-capability acceptance is resolved from the
// action's own spec field (the namespace default is applied by the Scheduler's
// caller, not here).
//
// ZONE IS NOT HERE, and its absence is deliberate. Filter 2 is leaf-set containment
// against Robot.status.currentZone resolved through the FleetZone tree — a client
// read this pure pair-wise matcher has no way to perform, and the string equality on
// Robot.spec.zone that used to stand in for it was wrong twice over: it never
// resolved a parent zone to its descendants, and it compared where a robot is
// CONFIGURED rather than where it currently is. Filter 2 now lives beside the other
// cross-object filters at the controller's choke point, which the preemption search
// reads through as well, so removing it from here narrows no path.
func RobotMatchesAction(robot *fleetv1.Robot, action *fleetv1.FleetAction) bool {
	return ExcludedBy(robot, action, acceptDegradedOf(action)) == nil
}

// RobotSatisfiesActionCapabilities reports whether the robot still meets the action's
// capability and parametric requirements (§control-plane filter 3 + §6.10.3) — the
// subset of RobotMatchesAction relevant to an ALREADY-assigned robot, excluding zone
// and phase. Used by the FleetAction controller to detect capability-loss on an
// in-flight action's robot (RFC-0001 Capability-loss reassignment). acceptDegraded is
// resolved by the caller (spec field OR namespace default), matching the assignment
// path so continued-execution eligibility can never diverge from initial assignment.
func RobotSatisfiesActionCapabilities(robot *fleetv1.Robot, action *fleetv1.FleetAction, acceptDegraded bool) bool {
	if action.Spec.RobotSelector != "" && robot.Name != action.Spec.RobotSelector {
		return false
	}
	capSet := schedulableCapabilities(robot, acceptDegraded)
	for _, required := range action.Spec.RequiredCapabilities {
		if !capSet[required] {
			return false
		}
	}
	for param, min := range action.Spec.Constraints {
		if !robotMeetsConstraint(robot, param, min) {
			return false
		}
	}
	return true
}

// robotMeetsConstraint reports whether the robot has a schedulable capability
// whose resolved parametric value for param is at least min. Active capabilities
// always count; a Degraded capability counts only when its degradedPolicy marks it
// DegradedSchedulable, in which case its CURRENT (reduced) resolved parameters are
// used — so a degraded capability serves lower-constraint actions but is excluded
// from those its reduced parameters can no longer meet (RFC-0001 §6.10
// degradedPolicy). Paused/Inactive capabilities never count, and an unresolved
// parameter fails the constraint (fail-closed).
func robotMeetsConstraint(robot *fleetv1.Robot, param string, min float64) bool {
	for _, c := range robot.Status.Capabilities {
		schedulable := c.Status == fleetv1.CapabilityStatusActive ||
			(c.Status == fleetv1.CapabilityStatusDegraded && c.DegradedSchedulable)
		if !schedulable {
			continue
		}
		if v, ok := c.ResolvedParameters[param]; ok && v >= min {
			return true
		}
	}
	return false
}

// acceptDegradedOf resolves a action's own degraded-capability acceptance, treating
// an unset (nil) field as false. The namespace default is layered on by the
// Scheduler's caller before scheduling; this fallback keeps the exported
// RobotMatchesAction (used by preemption) self-contained.
func acceptDegradedOf(action *fleetv1.FleetAction) bool {
	return action.Spec.AcceptDegradedCapabilities != nil && *action.Spec.AcceptDegradedCapabilities
}

// batteryOf returns the battery percentage or 0 if not reported.
func batteryOf(robot *fleetv1.Robot) int32 {
	if robot.Status.BatteryPercent == nil {
		return 0
	}
	return *robot.Status.BatteryPercent
}
