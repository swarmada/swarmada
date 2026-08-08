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
//     path; the existing lease/Revoking machinery (§9.6.3.5) owns that decision.
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
// check for the NEGOTIATED version. The conformance version is separate data and is checked
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

// dispatchExclusion explains why one robot was withheld from dispatch, for logging.
type dispatchExclusion struct {
	Robot  string
	Reason string
}

// filterDispatchEligible returns the subset of robots whose bound FleetAdapter is fit to receive
// work, plus the exclusions and their reasons. Adapter reads are memoized per call, so N robots
// sharing one adapter cost one Get.
//
// The returned slice preserves input order, so scheduler ranking is unaffected.
func (r *FleetActionReconciler) filterDispatchEligible(ctx context.Context, robots []fleetv1.Robot) ([]fleetv1.Robot, []dispatchExclusion) {
	if len(robots) == 0 {
		return robots, nil
	}
	ready := make(map[string]bool, 4) // adapter name → dispatch-ready
	reason := make(map[string]string, 4)

	out := make([]fleetv1.Robot, 0, len(robots))
	var excluded []dispatchExclusion

	for i := range robots {
		robot := robots[i]
		name := robot.Spec.Adapter.Name
		if name == "" {
			// Admission requires a named adapter (spec.adapter is a required field); a robot that
			// somehow carries none has no authorized adapter, so it cannot be dispatched to.
			excluded = append(excluded, dispatchExclusion{robot.Name, "robot has no spec.adapter.name"})
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
				if fit, why := adapterDispatchReadiness(&a); !fit {
					ready[name], reason[name] = false, fmt.Sprintf(
						"FleetAdapter %q is not fit for dispatch: %s", name, why)
				} else {
					ready[name] = true
				}
			}
		}
		if ready[name] {
			out = append(out, robot)
			continue
		}
		excluded = append(excluded, dispatchExclusion{robot.Name, reason[name]})
	}
	return out, excluded
}

// logDispatchExclusions records why candidates were withheld. An operator debugging "why is my task
// Pending" needs this: the robot looks Idle and capable, and the reason lives on another object.
func logDispatchExclusions(ctx context.Context, action *fleetv1.FleetAction, excluded []dispatchExclusion) {
	if len(excluded) == 0 {
		return
	}
	l := log.FromContext(ctx)
	for _, e := range excluded {
		l.V(1).Info("robot withheld from dispatch: adapter not fit for work (ADR-0032 assignment gate)",
			"action", action.Name, "robot", e.Robot, "reason", e.Reason)
	}
}
