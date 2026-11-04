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

package scheduler

import (
	"fmt"
	"sort"
	"strings"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// RFC-0001 candidate filter numbers ({{ref:control-plane-scheduler}}) for the
// filters THIS package owns — the ones decidable from a (robot, action) pair with
// no client read. The cross-object filters (2 zone leaf set, 8 ZoneMaintenance,
// 9 adapter readiness, 10 estop) are numbered beside their implementation in
// internal/controller/dispatch_filters.go, because that is where the reads are.
const (
	FilterPhaseIdle          = 1
	FilterRequiredCapability = 3
	FilterParametricConstr   = 5
	FilterRobotSelector      = 6

	// FilterTerminating is NOT one of RFC-0001's numbered filters, so it is negative:
	// 0 and -1 are internal/controller's unnumbered classes. A Robot with a
	// deletionTimestamp is going, and gets no new work (see ExcludedByForAssignment).
	FilterTerminating = -2
)

// Exclusion names the candidate filter that eliminated one robot, and says so in
// words an operator can act on.
//
// It exists because "no eligible robot available" is not a diagnosis. The reason a
// robot was dropped is decided inside a predicate and, before this type, was
// discarded there: the boolean came back false and every cause — wrong zone, flat
// battery, missing capability — arrived at the operator as the same sentence. A
// filter that cannot say why it fired is indistinguishable from a filter that is
// not there, which is the defect class this whole surface addresses.
type Exclusion struct {
	// Filter is the RFC-0001 candidate filter number, so a caller can group and
	// count by filter without parsing Reason.
	Filter int
	// Reason is the operator-facing explanation, naming the values compared.
	Reason string
}

// ExcludedBy reports the first candidate filter in THIS package that eliminates
// robot for action, or nil when none does. It covers filters 6 (robotSelector),
// 3 (required capabilities) and 5 (parametric constraints) — the pure pair-wise
// hard constraints — and deliberately NOT filter 1: the preemption search reuses
// this predicate to find a busy-but-otherwise-suitable robot, so phase is the
// caller's to add (see ExcludedByForAssignment).
//
// Filter 6 short-circuits 3 and 5, exactly as RFC-0001 specifies: a pinned robot
// is an assertion by the operator that it can do the work, not a request for the
// Scheduler to confirm it.
//
// This is the ONE implementation of these three filters. RobotMatchesAction and
// the Scheduler's own eligibility check both call it, so the assignment path, the
// preemption search and the operator-facing message can never disagree about what
// "eligible" means.
func ExcludedBy(robot *fleetv1.Robot, action *fleetv1.FleetAction, acceptDegraded bool) *Exclusion {
	// External actions (spec.executor set) match NO robot — they are dispatched to a
	// ComputeEndpoint. Not a numbered filter: RFC-0001 makes it a precondition that
	// holds before any candidate is examined, so it reports as filter 0.
	if action.Spec.Executor != nil {
		return &Exclusion{Filter: 0, Reason: "action carries spec.executor and is dispatched to a ComputeEndpoint, never to a robot"}
	}

	// Filter 6 — an explicit robotSelector pins the action to one robot and
	// short-circuits filters 2, 3 and 5 for it.
	if action.Spec.RobotSelector != "" {
		if robot.Name != action.Spec.RobotSelector {
			return &Exclusion{Filter: FilterRobotSelector,
				Reason: fmt.Sprintf("action pins spec.robotSelector=%q", action.Spec.RobotSelector)}
		}
		return nil
	}

	// Filter 3 — required capabilities. Active always satisfies; Degraded does so
	// only when the action accepts degraded capabilities. Paused/Inactive/
	// Unavailable/Failed entries are present for observability and never schedulable.
	capSet := schedulableCapabilities(robot, acceptDegraded)
	var missing []string
	for _, required := range action.Spec.RequiredCapabilities {
		if !capSet[required] {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing) // deterministic: an unchanged robot yields an unchanged reason
		return &Exclusion{Filter: FilterRequiredCapability,
			Reason: fmt.Sprintf("required capability not schedulable on this robot: %s", strings.Join(missing, ", "))}
	}

	// Filter 5 — parametric constraints (§6.10.3). A parameter no schedulable
	// capability resolves is unsatisfied: a robot that cannot prove it meets the
	// constraint is excluded (fail-closed).
	params := make([]string, 0, len(action.Spec.Constraints))
	for param := range action.Spec.Constraints {
		params = append(params, param)
	}
	sort.Strings(params) // map iteration is random; the reported constraint must not be
	for _, param := range params {
		if !robotMeetsConstraint(robot, param, action.Spec.Constraints[param]) {
			return &Exclusion{Filter: FilterParametricConstr,
				Reason: fmt.Sprintf("no schedulable capability resolves %s to at least %g", param, action.Spec.Constraints[param])}
		}
	}

	return nil
}

// ExcludedByForAssignment is ExcludedBy plus filter 1 (the robot must be Idle),
// which applies to the ordinary assignment path and not to the preemption search.
//
// It also excludes a Robot that is being deleted, whatever it holds. A finalizer keeps a
// deleted Robot in the API for as long as its owner takes, and the delete was admitted
// because the robot held no stop then; work handed to it now outlives the object. The
// preemption search reaches the same exclusion through filterDispatchEligible.
func ExcludedByForAssignment(robot *fleetv1.Robot, action *fleetv1.FleetAction, acceptDegraded bool) *Exclusion {
	if ex := ExcludedAsTerminating(robot); ex != nil {
		return ex
	}
	if robot.Status.Phase != fleetv1.RobotPhaseIdle {
		return &Exclusion{Filter: FilterPhaseIdle,
			Reason: fmt.Sprintf("robot phase is %s, not Idle", phaseOrUnknown(robot.Status.Phase))}
	}
	return ExcludedBy(robot, action, acceptDegraded)
}

// ExcludedAsTerminating reports a Robot with a deletionTimestamp, or nil.
func ExcludedAsTerminating(robot *fleetv1.Robot) *Exclusion {
	if robot.DeletionTimestamp == nil {
		return nil
	}
	return &Exclusion{Filter: FilterTerminating,
		Reason: "robot is being deleted (deletionTimestamp set); it takes no new work"}
}

// schedulableCapabilities is the set of capability names on the robot that may
// satisfy a requirement. acceptDegraded widens it to include Degraded entries
// (filter 3). The Scheduler reads status.capabilities, never spec.capabilities.
func schedulableCapabilities(robot *fleetv1.Robot, acceptDegraded bool) map[string]bool {
	capSet := make(map[string]bool, len(robot.Status.Capabilities))
	for _, c := range robot.Status.Capabilities {
		if c.Status == fleetv1.CapabilityStatusActive ||
			(acceptDegraded && c.Status == fleetv1.CapabilityStatusDegraded) {
			capSet[c.Name] = true
		}
	}
	return capSet
}

// phaseOrUnknown keeps an unreported phase legible in an exclusion reason; an
// empty phase means the robot has not reported one, which is not the same as Idle.
func phaseOrUnknown(p fleetv1.RobotPhase) string {
	if p == "" {
		return "unreported"
	}
	return string(p)
}
