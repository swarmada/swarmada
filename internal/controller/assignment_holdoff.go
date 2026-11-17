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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/robotid"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// THE HOLD-OFF (row A24's revert, repeated).
//
// A24 returns an assignment the robot never took up to Pending. Nothing in the revert keeps the
// next pass from choosing the same robot: the action's own status writes and the robot's return to
// Idle enqueue the action at once, ahead of any RequeueAfter. Measured on 7d1cd19, with the VDA 5050
// adapter: a FleetAction pinned to a robot whose adapter refused it as ROBOT_UNAVAILABLE was offered
// 416 times in 91 s, each time minting a generation, taking a TDE slot and writing the Robot twice.
//
// So after a revert for one of two causes, the robot is not offered that action again for a while:
//   - the command dispatcher did not send it because the robot's register is not processed on its
//     adapter's stream, or the stream is below contract 1.4.0 (A24's second trigger);
//   - the adapter refused it as ROBOT_UNAVAILABLE (A24's third trigger).
//
// The hold is checked where the candidates are filtered, before a generation is minted or the TDE is
// asked, so no event can bypass it: a held pass mints nothing, takes no slot and writes no robot.
// It starts at holdOffFloor and doubles on each revert of the same pair, up to holdOffCap. A robot
// that never registers then costs two Robot writes per holdOffCap per action: about 0.17 writes/s
// for 50 such robots with an action each, against the manager's 20/s.
//
// A robot's entries are dropped at once when the robot registers again (the dispatcher's
// RegisterEpoch moved), when its Ready condition is True and has changed since the hold, or when it
// is deleted (another UID, or gone). The entry is checked on the action's next pass, which comes
// within pendingRecheckInterval. A refusal the robot's own latch caused clears nothing on the Robot,
// so a pinned action waits for its next probe, at most holdOffCap.
//
// Like the declines memo (dispatch_operating_declines.go) it is a cache, not state: a restart
// forgets it and costs one more refusal. It is not a candidate filter, and it writes nothing to a
// resource's status (RFC-0001 §9.4.2 names the only exclusion records written there): the hold is
// reported as a Normal Event on the action, EventRobotHeldOff, and a log line.
const (
	holdOffFloor = 60 * time.Second
	holdOffCap   = 10 * time.Minute
	// holdOffForgetAfter: an entry this long past its hold is dropped, so the next revert of the
	// pair starts at the floor again and the memo does not keep every pair it has seen.
	holdOffForgetAfter = 2 * holdOffCap
)

// EventRobotHeldOff is the Event reason for a robot not offered an action for a while after the
// action's assignment to it was reverted (A24): its register was not processed, or its adapter
// refused it as unavailable.
const EventRobotHeldOff = "RobotHeldOff"

// rejectionRobotUnavailable is AssignActionRejection's ROBOT_UNAVAILABLE, as the outcome carries it.
const rejectionRobotUnavailable = "ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE"

// registrationEpochs is the command dispatcher's count of registers processed per robot
// (command.Dispatcher.RegisterEpoch). A commander without it resets no entry on a register.
type registrationEpochs interface {
	RegisterEpoch(namespace, wireID string) uint64
}

// holdOff is one (action, robot) entry.
type holdOff struct {
	until    time.Time
	reverts  int       // reverts of this pair so far; the hold doubles with each
	cause    string    // why, for the Event and the log
	message  string    // the status.message the revert wrote, kept while only held robots remain
	robotUID types.UID // the robot it was taken against: another UID is another robot
	epoch    uint64    // the robot's RegisterEpoch when taken
	ready    bool      // the robot's Ready condition was True when taken
	readyAt  time.Time // and its lastTransitionTime then
}

// holdOffMemo is the hold-offs by action (namespace/name/uid) and robot name.
type holdOffMemo struct {
	mu    sync.Mutex
	by    map[string]map[string]*holdOff
	clock func() time.Time // nil: time.Now
}

func (m *holdOffMemo) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// readyState is the robot's Ready condition: whether it is True, and its lastTransitionTime.
func readyState(robot *fleetv1.Robot) (bool, time.Time) {
	c := meta.FindStatusCondition(robot.Status.Conditions, conditionTypeReady)
	if c == nil {
		return false, time.Time{}
	}
	return c.Status == metav1.ConditionTrue, c.LastTransitionTime.Time
}

// record holds robot off action after a revert for cause, and returns the wait. epoch is the
// robot's RegisterEpoch read before the send, so a register processed during the revert ends the
// hold. message is the status.message the revert wrote.
func (m *holdOffMemo) record(action *fleetv1.FleetAction, robot *fleetv1.Robot, cause, message string,
	epoch uint64) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.sweep(now)
	if m.by == nil {
		m.by = map[string]map[string]*holdOff{}
	}
	key := declineKey(action)
	if m.by[key] == nil {
		m.by[key] = map[string]*holdOff{}
	}
	prev := m.by[key][robot.Name]
	reverts := 1
	if prev != nil && prev.robotUID == robot.UID {
		reverts = prev.reverts + 1
	}
	wait := holdOffFloor
	for i := 1; i < reverts && wait < holdOffCap; i++ {
		wait *= 2
	}
	if wait > holdOffCap {
		wait = holdOffCap
	}
	ready, readyAt := readyState(robot)
	m.by[key][robot.Name] = &holdOff{until: now.Add(wait), reverts: reverts, cause: cause, message: message,
		robotUID: robot.UID, epoch: epoch, ready: ready, readyAt: readyAt}
	return wait
}

// sweep drops entries long past their hold. The caller holds mu.
func (m *holdOffMemo) sweep(now time.Time) {
	for key, robots := range m.by {
		for name, h := range robots {
			if now.Sub(h.until) > holdOffForgetAfter {
				delete(robots, name)
			}
		}
		if len(robots) == 0 {
			delete(m.by, key)
		}
	}
}

// heldRobot is a robot held off an action on this pass: the robot as listed, how long its hold has
// left, and the status.message its revert wrote.
type heldRobot struct {
	robot   fleetv1.Robot
	left    time.Duration
	message string
}

// held returns, from eligible, the robots still held off action, in eligible's order, and the rest.
// An entry whose robot has registered since, is Ready again, or is another robot of the same name
// is dropped, and the robot is offered. epoch reads a robot's RegisterEpoch (nil: never moves).
func (m *holdOffMemo) held(action *fleetv1.FleetAction, eligible []fleetv1.Robot,
	epoch func(*fleetv1.Robot) uint64) (kept []fleetv1.Robot, held []heldRobot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	robots := m.by[declineKey(action)]
	if len(robots) == 0 {
		return eligible, nil
	}
	now := m.now()
	kept = eligible[:0:0]
	for i := range eligible {
		rb := &eligible[i]
		h := robots[rb.Name]
		if h == nil {
			kept = append(kept, *rb)
			continue
		}
		ready, readyAt := readyState(rb)
		reset := rb.UID != h.robotUID ||
			(epoch != nil && epoch(rb) != h.epoch) ||
			(ready && (!h.ready || !readyAt.Equal(h.readyAt)))
		if reset {
			delete(robots, rb.Name)
			kept = append(kept, *rb)
			continue
		}
		if now.Before(h.until) {
			held = append(held, heldRobot{robot: *rb, left: h.until.Sub(now), message: h.message})
			continue
		}
		kept = append(kept, *rb) // the hold is over: one probe
	}
	return kept, held
}

// forget drops every entry for action: one robot has taken it.
func (m *holdOffMemo) forget(action *fleetv1.FleetAction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.by, declineKey(action))
}

// robotEpoch reads robot's RegisterEpoch from the commander, or 0 when it keeps none.
func (r *FleetActionReconciler) robotEpoch(robot *fleetv1.Robot) uint64 {
	if e, ok := r.Commander.(registrationEpochs); ok {
		return e.RegisterEpoch(robot.Namespace, robotid.Wire(robot))
	}
	return 0
}

// dropHeldOff removes from eligible the robots held off action, and logs each. It returns the rest
// and the held robots. A held robot stays out of this pass's selection and its preemption search.
func (r *FleetActionReconciler) dropHeldOff(ctx context.Context, action *fleetv1.FleetAction,
	eligible []fleetv1.Robot) ([]fleetv1.Robot, []heldRobot) {
	kept, held := r.holdOffs.held(action, eligible, r.robotEpoch)
	for _, h := range held {
		log.FromContext(ctx).Info("robot held off this action after its assignment was not taken up (A24)",
			"action", action.Name, "robot", h.robot.Name, "remaining", h.left.Round(time.Second).String())
	}
	return kept, held
}

// assignableHeld splits the held robots into those selection would take if they were not held
// (scheduler.ExcludedByForAssignment admits them: Idle, not terminating, and its other checks)
// and the others. Only an assignable held robot is an eligible robot in §9.4.2's sense, so only
// one holds off preemption, and only one makes the hold why nothing was dispatched. message is
// the status.message the revert of the first assignable one by name wrote, "" when there is none.
// The others could not have been chosen either way; the no-candidate tally counts them with what
// keeps them.
func assignableHeld(held []heldRobot, action *fleetv1.FleetAction, acceptDegraded bool) (
	assignable int, others []fleetv1.Robot, message string) {
	first := ""
	for i := range held {
		rb := &held[i].robot
		if scheduler.ExcludedByForAssignment(rb, action, acceptDegraded) != nil {
			others = append(others, *rb)
			continue
		}
		assignable++
		if first == "" || rb.Name < first {
			first, message = rb.Name, held[i].message
		}
	}
	return assignable, others, message
}

// holdRobotOff records the hold after a revert for cause, emits EventRobotHeldOff and logs it. epoch
// is the robot's RegisterEpoch read before the send; the revert has written action's status.message.
func (r *FleetActionReconciler) holdRobotOff(ctx context.Context, action *fleetv1.FleetAction,
	robot *fleetv1.Robot, cause string, epoch uint64) {
	wait := r.holdOffs.record(action, robot, cause, action.Status.Message, epoch)
	msg := fmt.Sprintf("robot %s is not offered this action for %s: %s. It is offered again sooner if it "+
		"registers, turns Ready again or is deleted", robot.Name, wait, cause)
	log.FromContext(ctx).Info("robot held off an action (A24)", "action", action.Name, "robot", robot.Name,
		"wait", wait.String(), "cause", cause)
	if r.Recorder != nil {
		r.Recorder.Event(action, corev1.EventTypeNormal, EventRobotHeldOff, msg)
	}
}
