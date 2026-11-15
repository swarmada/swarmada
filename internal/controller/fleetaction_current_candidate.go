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

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/robotid"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// currentCandidate re-reads the robot selection chose, past the cache, before its adapter is
// asked whether it can serve the action and before a TDE slot is taken for it.
//
// The candidates come from the cache, and the cache trails the API server. When one robot
// becoming free wakes several actions (pendingActionsFor), the first is assigned and commits the
// robot, and the next is reconciled a moment later, usually before the cache has seen that
// commit. It would select the same robot, ask its adapter, take a TDE slot (which may mark other
// actions Preempted), mint a generation and write itself Assigned, and only then lose the robot
// commit's optimistic lock and revert to Pending, behind every other queued action. The robot
// commit's lock keeps the robot to one action either way; this read keeps the loser from doing
// anything before it finds out. That holds with the controller's one worker, whose previous
// reconcile has finished its robot commit before this read; with more workers, two reconciles
// can both read the robot free, and the lock and the revert decide between them as before.
//
// One Get per selection attempt. The answer:
//   - the robot is unchanged since the cache read: dispatch goes on with the cached copy;
//   - it changed: its live copy is judged again (its wire robot-id must be the one the pass
//     listed, for the pass's robot-id hold to hold), by the scheduler's filters and by judge, the
//     dispatch pass's own filter state and holds (no further list), and must still be free for
//     work (robotFreeForWork). Dispatch goes on with the live copy only if it passes, so the
//     robot commit's lock is taken on the state the filters judged; otherwise the robot is
//     withheld from this pass, with the gate that refused it;
//   - it is gone: it is withheld;
//   - the read fails: dispatch goes on with the cached copy, as it did before this read existed,
//     and the robot commit's lock decides.
func (r *FleetActionReconciler) currentCandidate(ctx context.Context, action *fleetv1.FleetAction,
	selected *fleetv1.Robot, judge *dispatchJudge, acceptDegraded bool) (fleetv1.Robot, *dispatchExclusion) {
	logger := log.FromContext(ctx).WithValues("robot", selected.Name)
	var live fleetv1.Robot
	if err := r.liveReader().Get(ctx, client.ObjectKeyFromObject(selected), &live); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("the selected robot no longer exists; selecting again")
			return fleetv1.Robot{}, &dispatchExclusion{selected.Name, scheduler.FilterTerminating,
				"robot no longer exists (read past the cache before dispatch)"}
		}
		logger.Info("could not re-read the selected robot; the robot commit's lock decides", "err", err.Error())
		return *selected, nil
	}
	if live.ResourceVersion == selected.ResourceVersion {
		return *selected, nil
	}
	ex := func() *dispatchExclusion {
		// The pass's robot-id hold is keyed by the wire ids it listed. A robot whose wire id
		// changed since (only possible where the admission webhook is not installed) could
		// carry another robot's id without the hold seeing it, so it is withheld this pass.
		if robotid.Wire(&live) != robotid.Wire(selected) {
			return &dispatchExclusion{live.Name, filterRobotIDConflict,
				fmt.Sprintf("robot's wire robot-id changed since this pass listed the robots (%q, was %q)",
					robotid.Wire(&live), robotid.Wire(selected))}
		}
		if ex := scheduler.ExcludedByForAssignment(&live, action, acceptDegraded); ex != nil {
			return &dispatchExclusion{live.Name, ex.Filter, ex.Reason}
		}
		if ex := judge.judge(ctx, &live); ex != nil {
			return ex
		}
		if !robotFreeForWork(&live) {
			return &dispatchExclusion{live.Name, scheduler.FilterPhaseIdle,
				fmt.Sprintf("robot is Idle but still records action %s", live.Status.AssignedAction)}
		}
		return nil
	}()
	if ex != nil {
		logger.Info("the selected robot changed after the cache read and no longer qualifies; selecting again",
			"filter", ex.Filter, "reason", ex.Reason)
		return fleetv1.Robot{}, ex
	}
	return live, nil
}

// withoutRobots returns robots without the ones named in dropped.
func withoutRobots(robots []fleetv1.Robot, dropped []dispatchExclusion) []fleetv1.Robot {
	gone := make(map[string]bool, len(dropped))
	for _, d := range dropped {
		gone[d.Robot] = true
	}
	out := make([]fleetv1.Robot, 0, len(robots))
	for i := range robots {
		if !gone[robots[i].Name] {
			out = append(out, robots[i])
		}
	}
	return out
}
