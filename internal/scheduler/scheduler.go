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
// Selection criteria (all must pass):
//  1. Robot phase is Idle
//  2. Robot has all required capabilities
//  3. If action.Spec.Zone is set, robot must be in that zone
//  4. If action.Spec.RobotSelector is set, only that robot is considered
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
// pair alone: filter 1 (phase) plus the zone/capability/selector/parametric matcher.
//
// Filter 10 (active emergency stop) is NOT checked here. It is enforced by the caller, in
// FleetActionReconciler.filterDispatchEligible, which is the single choke point for both the
// normal selection and the preemption search and is the only place that emits an operator-facing
// exclusion reason. A caller that builds its own candidate list MUST apply filter 10 itself.
func (s *DefaultScheduler) isEligible(robot *fleetv1.Robot, action *fleetv1.FleetAction, acceptDegraded bool) bool {
	// Must be Idle (not Busy, Charging, Degraded, Offline, or Pending), plus the
	// zone/capability/selector match shared with preemption.
	if robot.Status.Phase != fleetv1.RobotPhaseIdle {
		return false
	}
	return robotMatchesAction(robot, action, acceptDegraded)
}

// RobotMatchesAction reports whether a robot satisfies a action's zone, capability,
// and selector constraints — everything EXCEPT the robot's phase. The Scheduler
// adds the Idle requirement on top of this (see isEligible); Critical-priority
// preemption (RFC-0001 §9.1.4.3) reuses it to find a busy-but-otherwise-suitable
// robot, so the two paths can never diverge on what "eligible" means. Degraded-
// capability acceptance is resolved from the action's own spec field (the namespace
// default is applied by the Scheduler's caller, not here).
func RobotMatchesAction(robot *fleetv1.Robot, action *fleetv1.FleetAction) bool {
	return robotMatchesAction(robot, action, acceptDegradedOf(action))
}

// robotMatchesAction is the shared matcher. acceptDegraded widens the set of
// schedulable capabilities to include Degraded entries (§control-plane filter 3).
func robotMatchesAction(robot *fleetv1.Robot, action *fleetv1.FleetAction, acceptDegraded bool) bool {
	// Explicit robot selector overrides zone and capability checks.
	if action.Spec.RobotSelector != "" {
		return robot.Name == action.Spec.RobotSelector
	}

	// Zone constraint.
	if action.Spec.Zone != "" && robot.Spec.Zone != action.Spec.Zone {
		return false
	}

	// Capability requirements. Active capabilities always satisfy a requirement;
	// Degraded entries do so only when the action accepts degraded capabilities
	// (RFC-0001 §control-plane filter 3). Paused/Inactive/Unavailable/Failed
	// entries are present in status.capabilities[] for observability but are never
	// schedulable (§Robot Capability Derivation Truth Table).
	capSet := make(map[string]bool, len(robot.Status.Capabilities))
	for _, c := range robot.Status.Capabilities {
		if c.Status == fleetv1.CapabilityStatusActive ||
			(acceptDegraded && c.Status == fleetv1.CapabilityStatusDegraded) {
			capSet[c.Name] = true
		}
	}
	for _, required := range action.Spec.RequiredCapabilities {
		if !capSet[required] {
			return false
		}
	}

	// Parametric constraints (§6.10.3): each declared constraint requires an Active
	// capability that resolves the named parameter to at least the constraint value.
	// A parameter no Active capability resolves is unsatisfied — a robot that cannot
	// prove it meets the constraint is excluded (fail-closed).
	for param, min := range action.Spec.Constraints {
		if !robotMeetsConstraint(robot, param, min) {
			return false
		}
	}

	return true
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
	capSet := make(map[string]bool, len(robot.Status.Capabilities))
	for _, c := range robot.Status.Capabilities {
		if c.Status == fleetv1.CapabilityStatusActive ||
			(acceptDegraded && c.Status == fleetv1.CapabilityStatusDegraded) {
			capSet[c.Name] = true
		}
	}
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
