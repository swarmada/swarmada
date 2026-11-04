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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// The assignment gate (ADR-0032, "Assignment gate (and nothing more)").
//
// A Robot is admitted only against a Connected, conformance-Passed FleetAdapter
// (internal/webhook/robot_webhook.go). That check fires ONCE, at admission. Afterwards the
// adapter's readiness can change underneath an already-admitted robot — heartbeats lapse and the
// status controller moves Connected → Degraded → Disconnected, or conformance flips to Failed when
// the report's digest stops matching, its ConfigMap disappears, or its JSON stops parsing. Nothing
// re-examined that before this gate, so those robots kept receiving work indefinitely.
//
// SCOPE — work dispatch only. This filters the CANDIDATE set for a new assignment. It deliberately
// does not touch:
//   - telemetry, heartbeats, or capability snapshots (an unready adapter is still observed),
//   - emergency stop (ADR-0032 pins estop as version- and state-invariant: estop MUST be honoured
//     against any connected adapter irrespective of its compatibility or conformance state),
//   - work already in flight — an action that is Assigned/InProgress is NOT revoked when its
//     adapter degrades. Yanking running work on a status flip would be a new, unconfirmed stop
//     path; the existing lease/Revoking machinery (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee) owns that decision.
//
// FAIL CLOSED. Anything that leaves readiness unproven excludes the robot from dispatch: an unset
// spec.adapter.name, a FleetAdapter that does not exist, or an API error reading it. Per ADR-0032
// missing data "is treated as incompatible (fail-closed on work), never as an implicit pass". An
// excluded robot is not failed or modified — it is simply not selected, and the action stays Pending
// and requeues, so a transient read error costs latency rather than correctness.

// adapterDispatchReady reports whether a FleetAdapter may receive newly assigned work: the same
// three conditions the admission gate requires (internal/webhook/robot_webhook.go), re-checked at
// dispatch time.
//
// Phase Connected already carries the contract-version handshake verdict — an adapter that agreed no
// compatible contract version is Rejected, not Connected (ADR-0032) — so this needs no separate
// check that a version was NEGOTIATED. Which one was is filter 9's further condition
// (adapterLatchContract), read under FleetActionReconciler.RequireLatchContract and stricter than
// the admission gate, never laxer. The conformance version is separate data and is checked
// explicitly: a result earned against an out-of-range contract, or a report naming no contract
// version, is not binding here.
//
// The two gates MUST agree. Admission fires once; if dispatch were more permissive, a robot admitted
// before a contract bump would keep receiving work forever against a qualification that no longer
// applies — which is the exact hole this file was written to close.
func adapterDispatchReady(a *fleetv1.FleetAdapter) bool {
	ready, _ := adapterDispatchReadiness(a)
	return ready
}

// adapterListsClass is filter 9's class condition: the admission gate's class leg
// (internal/webhook/robot_webhook.go), re-checked at dispatch. An adapter whose
// spec.servesRobotClasses names classes, none of them the robot's, is given none of the robot's
// work; an empty list serves every class, as admission reads it.
//
// The class does not gate observation or stopping (RFC-0001 #fleetadapter-admission-gating): after
// an edit drops a robot's class, the ControlStream still takes the robot's telemetry and its
// EstopAcks (controlstream.ErrClassNotServed) and refuses the rest. The telemetry keeps the robot
// Idle, so its phase no longer withholds it, and this does: work dispatched to it would have its
// results refused.
func adapterListsClass(served []string, class string) bool {
	if len(served) == 0 {
		return true
	}
	for _, c := range served {
		if c == class {
			return true
		}
	}
	return false
}

// adapterDispatchReadiness is adapterDispatchReady plus the reason it said no, so an exclusion can
// name the condition that actually failed. Without this a version-excluded adapter would be logged
// as "not fit: phase=Connected conformance=Passed" — a self-contradiction for whoever is debugging
// why their task will not dispatch.
func adapterDispatchReadiness(a *fleetv1.FleetAdapter) (bool, string) {
	if a.Status.Phase != fleetv1.FleetAdapterPhaseConnected ||
		a.Status.Conformance != fleetv1.ConformanceStatePassed {
		return false, fmt.Sprintf("phase=%s conformance=%s (want Connected/Passed)",
			phaseOrPendingText(a.Status.Phase), conformanceOrUnknownText(a.Status.Conformance))
	}
	if supported, why := contract.Supports(a.Status.ConformanceContractVersion); !supported {
		return false, "conformance is not bound to a supported contract version: " + why
	}
	return true, ""
}

// latchContractMin is the contract version whose connections the Stop Latch binds.
const latchContractMin = "1.4.0"

// adapterLatchContract is candidate filter 9's version condition: the adapter's connection
// negotiated contract 1.4.0 (status.negotiatedContractVersion), so the Stop Latch binds it (RFC-0001
// #control-plane-scheduler, filter 9). A 1.3.x connection is still sent every stop; it is only not
// given work. Fail closed: no recorded version, or one that cannot be read, withholds the robot.
// Read only under FleetActionReconciler.RequireLatchContract, which the manager sets from
// --require-latch-contract, off at v0.4: the in-tree deployments run the simulation adapter at
// 1.3.0, which this would leave with no work.
func adapterLatchContract(a *fleetv1.FleetAdapter) (bool, string) {
	v := a.Status.NegotiatedContractVersion
	below, ok := contract.Below(v, latchContractMin)
	switch {
	case !ok:
		return false, fmt.Sprintf("its connection records no readable negotiated contract version (%q); "+
			"work goes only to a connection that negotiated %s, which the Stop Latch binds", v, latchContractMin)
	case below:
		return false, fmt.Sprintf("its connection negotiated contract %s, below %s: the Stop Latch does not "+
			"bind it, so it is sent every stop and no work", v, latchContractMin)
	}
	return true, ""
}

// phaseOrPendingText and conformanceOrUnknownText keep an unset value legible in a log line; an
// empty phase is the implicit Pending state, not a missing field.
func phaseOrPendingText(p fleetv1.FleetAdapterPhase) string {
	if p == "" {
		return string(fleetv1.FleetAdapterPhasePending)
	}
	return string(p)
}

func conformanceOrUnknownText(c fleetv1.ConformanceState) string {
	if c == "" {
		return string(fleetv1.ConformanceStateUnknown)
	}
	return string(c)
}

// dispatchExclusion explains why one robot was withheld from dispatch, for logging
// and for the action's own status.message.
//
// Filter is the RFC-0001 candidate filter number that fired, carried alongside the
// prose so a caller can group and count by filter without parsing Reason. Without
// it the no-candidate message would have to pattern-match on English, which is how
// a diagnostic quietly stops being one.
type dispatchExclusion struct {
	Robot  string
	Filter int
	Reason string
}

// filterDispatchEligible returns the subset of robots fit to receive work, plus the exclusions
// and their reasons. It is the single choke point for the RFC-0001 candidate filters that need a
// read of an object other than the robot and the action ({{ref:control-plane-scheduler}}),
// applied in the order that fails cheapest first:
//
//	          — a robot whose wire robot-id another Robot also carries, a robot in a
//	            namespace whose estop is in force, and a robot in (or below) a zone whose
//	            estop is in force, are withheld first (dispatch_holds.go; the namespace
//	            and zone stops report as filter 10)
//	filter 10 — the robot MUST NOT be under an active estop
//	          — and, separately from filter 10, a robot in the post-clear Resuming
//	            window is withheld until its resume is confirmed
//	filter  4 — battery at or above max(action floor, robot floor)
//	filter  7 — no model granting a required capability is Updating
//	filter  8 — no active ZoneMaintenance holds the robot
//	filter  2 — status.currentZone within the leaf set of the action's zone
//	filter  9 — the robot's bound FleetAdapter MUST be fit for dispatch (ADR-0032), and
//	            list the robot's class when it lists any (adapterListsClass)
//
// These are enforced here rather than in the scheduler package because this is the single
// choke point the caller uses for BOTH the normal selection and the preemption search
// (fleetaction_controller.go), and because it is the only place that produces an operator-facing
// exclusion reason. Covering both paths is not incidental: the preemption search deliberately
// skips filter 1 to find a busy robot, so any filter that lived only behind filter 1 — filter 8
// most sharply, since a robot being drained for maintenance is InProgress by definition — would
// fail open on exactly that path.
//
// Filter 10 reads the robot's own estopState, which a zone- or namespace-scope estop writes
// through its per-robot fan-out (zoneestop_controller.go, namespaceestop_controller.go). That
// fan-out reaches the robots in scope when the stop is triggered, not one that arrives
// afterwards, so the zone's and the namespace's own stop are read as well
// (dispatchHolds.zoneStop, dispatchHolds.namespaceStop).
//
// Adapter reads are memoized per call, and the Robot, FleetZone, SwarmadaConfig and
// ZoneMaintenance lists are read once per call, so N robots sharing one adapter and one zone
// tree cost one Get and at most five Lists, one of them (the Robots) uncached.
// The returned slice preserves input order, so scheduler ranking is unaffected.
//
// It writes nothing. See the header of dispatch_filters.go.
func (r *FleetActionReconciler) filterDispatchEligible(
	ctx context.Context, action *fleetv1.FleetAction, robots []fleetv1.Robot,
) ([]fleetv1.Robot, []dispatchExclusion, []string, *zoneRestrictionVerdict) {
	st, degraded, restrictions := r.newDispatchFilterState(ctx, action)
	if len(robots) == 0 {
		// The zone reads happen even with no candidates, because FILTER 11 IS
		// ACTION-SCOPED: whether the operator has forbidden this work does not depend on
		// which robots exist. Short-circuiting here reported a denied zone as "no
		// eligible robot: the namespace holds no Robot resources" — the precise
		// confusion ADR-0058 §5 says the terminal refusal exists to prevent.
		return robots, nil, degraded, restrictions
	}
	holds := r.newDispatchHolds(ctx, action.Namespace)
	ready := make(map[string]bool, 4) // adapter name → dispatch-ready
	reason := make(map[string]string, 4)
	served := make(map[string][]string, 4) // adapter name → spec.servesRobotClasses, once ready

	out := make([]fleetv1.Robot, 0, len(robots))
	var excluded []dispatchExclusion

	for i := range robots {
		robot := robots[i]

		// A Robot being deleted takes no new work, whatever it holds (see
		// scheduler.ExcludedAsTerminating). Here as well as in the scheduler's own check
		// because this is the choke point the preemption search shares.
		if ex := scheduler.ExcludedAsTerminating(&robot); ex != nil {
			excluded = append(excluded, dispatchExclusion{robot.Name, ex.Filter, ex.Reason})
			continue
		}

		// One robot per wire robot-id, and no work in a stopped zone or a stopped namespace
		// (dispatch_holds.go). Each stands for a stop the robot's own estopState may not
		// show, so all three come before filter 10.
		if why := holds.robotIDConflict(&robot); why != "" {
			excluded = append(excluded, dispatchExclusion{robot.Name, filterRobotIDConflict, why})
			continue
		}
		if why := holds.namespaceStop(&robot); why != "" {
			excluded = append(excluded, dispatchExclusion{robot.Name, filterEstop, why})
			continue
		}
		if why := holds.zoneStop(&robot); why != "" {
			excluded = append(excluded, dispatchExclusion{robot.Name, filterEstop, why})
			continue
		}

		// Filter 10 — an estopped robot is not a candidate. Dispatching to one produces an
		// immediate Paused transition and no useful work, and the robot may not be at rest.
		// Checked before the adapter read because it is free and it is the safety-relevant one.
		if robotUnderEstop(&robot) {
			why := fmt.Sprintf("robot is under an active emergency stop (estopState %s)", robot.Status.EstopState)
			if by, held := estopscope.HeldOnRecord(&robot); held {
				why = fmt.Sprintf("robot is under an active emergency stop (estopState %s; held by %s)",
					stateOrNormal(robot.Status.EstopState), by)
			}
			excluded = append(excluded, dispatchExclusion{robot.Name, filterEstop, why})
			continue
		}

		// The post-clear supervised window. This is NOT filter 10 — filter 10 is
		// exactly Stopping, Stopped and Failed (RFC-0001 #control-plane-scheduler),
		// and Resuming is not an active estop: robotUnderEstop also drives the
		// estop-pause of an in-flight action and preemption-victim selection, and a
		// robot whose stop has been cleared belongs in neither.
		//
		// It is withheld here because withholding is the property the state exists
		// for. RFC-0001 #safety-estop-state-machine defines Resuming as the interval
		// in which the Fleet Adapter is re-enabling motion and capabilities are
		// restoring, left only when an operator confirms the robot's release.
		// Dispatching into that interval hands work to a robot nobody has confirmed can
		// do it, which is the hazard the state was specified to close; a Resuming robot
		// that were dispatchable would make the state decorative.
		//
		// The exit is operator-confirmed, never automatic and never timed
		// (internal/safety.Dispatcher.ConfirmResume), so a robot nobody releases stays
		// here rather than ageing into dispatch.
		if robot.Status.EstopState == fleetv1.RobotEstopResuming {
			excluded = append(excluded, dispatchExclusion{robot.Name, filterResumeWindow,
				fmt.Sprintf("robot is in the post-clear estop resume window (estopState %s); "+
					"it becomes a candidate once an operator releases it (swarmctl estop resume)",
					robot.Status.EstopState)})
			continue
		}

		// Filters 4, 7, 8 and 2 — the remaining cross-object gates, over state this
		// call has already listed. Placed before the adapter Get so a robot that is
		// flat, mid-update or drained costs no API read to reject.
		if ex := st.excludedBy(&robot, action); ex != nil {
			excluded = append(excluded, dispatchExclusion{robot.Name, ex.Filter, ex.Reason})
			continue
		}

		name := robot.Spec.Adapter.Name
		if name == "" {
			// Admission requires a named adapter (spec.adapter is a required field); a robot that
			// somehow carries none has no authorized adapter, so it cannot be dispatched to.
			excluded = append(excluded, dispatchExclusion{robot.Name, filterAdapterReady,
				"robot has no spec.adapter.name"})
			continue
		}
		if _, seen := ready[name]; !seen {
			var a fleetv1.FleetAdapter
			err := r.Get(ctx, client.ObjectKey{Namespace: robot.Namespace, Name: name}, &a)
			switch {
			case apierrors.IsNotFound(err):
				ready[name], reason[name] = false, fmt.Sprintf("FleetAdapter %q not found", name)
			case err != nil:
				// Fail closed on an unproven read; the action requeues.
				ready[name], reason[name] = false, fmt.Sprintf("reading FleetAdapter %q: %v", name, err)
			default:
				fit, why := adapterDispatchReadiness(&a)
				if fit && r.RequireLatchContract {
					fit, why = adapterLatchContract(&a)
				}
				if !fit {
					ready[name], reason[name] = false, fmt.Sprintf(
						"FleetAdapter %q is not fit for dispatch: %s", name, why)
				} else {
					ready[name] = true
					served[name] = a.Spec.ServesRobotClasses
				}
			}
		}
		if ready[name] && !adapterListsClass(served[name], robot.Spec.RobotClass) {
			excluded = append(excluded, dispatchExclusion{robot.Name, filterAdapterReady, fmt.Sprintf(
				"FleetAdapter %q does not serve robot class %q (spec.servesRobotClasses)", name, robot.Spec.RobotClass)})
			continue
		}
		if ready[name] {
			out = append(out, robot)
			continue
		}
		excluded = append(excluded, dispatchExclusion{robot.Name, filterAdapterReady, reason[name]})
	}
	return out, excluded, degraded, restrictions
}

// logDispatchExclusions records why candidates were withheld. An operator debugging "why is my task
// Pending" needs this: the robot looks Idle and capable, and the reason lives on another object.
//
// TWO VERBOSITIES, and the split is about cardinality, not importance.
//
// The exceptional exclusions — an active estop, the post-clear resume window, an adapter unfit for
// dispatch — are emitted at Info, V(0), the default manager verbosity. "Why was my robot not
// picked" is the most common operator question, and the answer was once behind a verbosity flag: to
// see it an operator had to already suspect the dispatch gate and restart the manager, by which
// time the estop or adapter state that caused it may have moved. A diagnostic that answers a
// question nobody has asked yet has to be on by default to be worth having. These are also RARE —
// a fleet in which most robots are estopped or on a broken adapter has a bigger problem than log
// volume.
//
// The ROUTINE exclusions — wrong zone above all, and battery, model update, maintenance — go to
// V(1). They are not less important; they are ORDINARY. Every robot outside the action's zone is
// excluded from every zoned action, so a 500-robot namespace would emit 499 default-level lines per
// pending action per requeue, and the estop line an operator actually needs would be buried in it.
// Nothing is lost by the demotion: the caller writes the aggregate — a count per filter — to the
// action's own status.message and to a single default-level line (noCandidateMessage,
// fleetaction_controller.go), which is where an operator looks first and is a better surface than a
// log line anyway. V(1) still has the per-robot detail for whoever needs to know WHICH robot.
//
// Log, not status, per robot. This is called from the dispatch path once per candidate per
// reconcile, and a Pending action requeues every 15s: writing each reason to a resource's status
// here would be a per-tick status write (RA-1, "status-write discipline" in docs/architecture.md)
// and write amplification against the API server. Nothing in this file writes to the API server,
// and this path must not become the first thing that does.
//
// Silent when nothing is withheld, so a fully eligible fleet adds no lines.
func logDispatchExclusions(ctx context.Context, action *fleetv1.FleetAction, excluded []dispatchExclusion) {
	if len(excluded) == 0 {
		return
	}
	l := log.FromContext(ctx)
	for _, e := range excluded {
		logger := l
		if isRoutineExclusion(e.Filter) {
			logger = l.V(1)
		}
		logger.Info("robot withheld from dispatch",
			"action", action.Name, "robot", e.Robot, "filter", e.Filter, "reason", e.Reason)
	}
}

// isRoutineExclusion reports whether a filter eliminates candidates as a matter of
// course — one line per non-matching robot per reconcile — rather than flagging
// something an operator would want told at default verbosity.
func isRoutineExclusion(filter int) bool {
	switch filter {
	case filterZoneLeafSet, filterMinBattery, filterModelNotUpdating, filterZoneMaintenance:
		return true
	default:
		return false
	}
}
