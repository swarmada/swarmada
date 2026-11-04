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

// Package executorguard is the one decision behind every path that gives a FleetAction to a
// robot or frees a robot from one: an action is dispatched again, and a robot's record of
// it (Robot.status.assignedAction) is released, only when the previous executor is provably
// off it (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee). The decision
// is keyed on the robot's record, not on the action's binding, because every path that
// clears the binding first would otherwise bypass it: a declarative pause on an Assigned
// action, a deleted member created again, a terminal action retiring its robot.
//
// Proof is one of: the robot's adapter acknowledged a cancel for the action without
// reporting the stop unconfirmed; the robot's own adapter reported the action ended; the
// action's lease horizon plus the clock-skew margin has passed. A confirmed emergency stop is
// not proof: it says the robot is at rest now, not that it has dropped the action, and an
// adapter that was never told to cancel it may take it up again after the stop's resume.
//
// The record is also the one link from work to the stop that paused it. While a stop covers
// the robot, its record is kept and blocks the action from every other robot, whatever the
// proof says (KeepsRecord, Recorder.Covered): work a stop paused moves only through a requeue
// the estop-clear verb on that stop authorizes (Input.MoveAuthorized), or once the stop is
// cleared (RFC-0001 #safety-fleetaction-behaviour-during-estop). When the control plane does
// not know the horizon (the predecessor was deleted, or the binding was cleared as the
// FleetAction state table requires), it is bounded by the longest lease the configuration
// admits, MaxLease, plus the skew margin, counted from the latest moment the action is known
// to have re-entered Pending or been created or settled. Everything the decision reads is on
// the objects, so a manager restart re-derives it.
package executorguard

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// MaxLease is the longest lease SwarmadaConfig.spec.scheduling.leaseDurationSeconds admits
// (its CRD maximum). A horizon the control plane does not know is bounded by it.
const MaxLease = 300 * time.Second

// CoveredRecheck is how soon a decision a covering stop blocked is asked again.
const CoveredRecheck = 5 * time.Second

// MaxSkew is the widest margin SwarmadaConfig.spec.scheduling.clockSkewMarginSeconds admits
// (its CRD maximum). A check that does not read the configuration (admission) waits on it.
const MaxSkew = 60 * time.Second

// Recorder is a robot whose status.assignedAction names the action and that is not the
// action's current binding.
type Recorder struct {
	Robot string
	// CancelConfirmed: the robot's adapter acknowledged a cancel for this action without
	// CANCEL_DISPOSITION_STOP_UNCONFIRMED.
	CancelConfirmed bool
	// Ended: the robot's own adapter reported this action ended (succeeded, failed,
	// cancelled, superseded), through the stream that serves the robot.
	Ended bool
	// Covered names the stop that covers the robot, in force or pending, or is "".
	Covered string
}

// Input is what the decision reads.
type Input struct {
	// Horizon is the action's recorded lease horizon (status.leaseExpiresAt), if known.
	Horizon *time.Time
	// Reference is the latest moment the action is known to have re-entered Pending, been
	// created, or settled; the unknown-horizon bound counts from it. A zero Reference with
	// no Horizon is never proof: the decision waits MaxLease plus skew from Now.
	Reference time.Time
	Recorders []Recorder
	Now       time.Time
	Skew      time.Duration
	// MoveAuthorized: the action is being moved by a requeue the estop-clear check admitted,
	// so a covering stop does not keep a recorder that is otherwise provably off the action.
	MoveAuthorized bool
	// ActionGone: the recorded action no longer exists. That is no proof: the robot may still
	// run it, and it may be created again under its name, so DeleteLosesKey refuses.
	ActionGone bool
}

// Decision is the answer.
type Decision struct {
	// Release names the recorders whose record may be released now: each is provably off
	// the action.
	Release []string
	// Blocked names the first recorder that is not provably off the action, or "".
	Blocked string
	// Wait is how long to wait before asking again while Blocked is set; zero otherwise.
	Wait time.Duration
	// Why says, in words, what the decision rests on.
	Why string
	// Stop names the stop that keeps Blocked, when a covering stop is what blocks.
	Stop string
}

// Dispatchable reports whether no recorder blocks the action.
func (d Decision) Dispatchable() bool { return d.Blocked == "" }

// Dead reports when the executor's lease is provably dead: the known horizon plus skew, or
// the unknown-horizon bound.
func Dead(in Input) time.Time {
	if in.Horizon != nil {
		return in.Horizon.Add(in.Skew)
	}
	ref := in.Reference
	if ref.IsZero() {
		ref = in.Now
	}
	return ref.Add(MaxLease + in.Skew)
}

// Decide answers whether the action may be given to a robot now, and which records may be
// released.
func Decide(in Input) Decision {
	var d Decision
	dead := Dead(in)
	leaseDead := !in.Now.Before(dead)
	for _, r := range in.Recorders {
		if KeepsRecord(r.Covered, in.MoveAuthorized) {
			if d.Blocked == "" {
				d.Blocked, d.Wait, d.Stop = r.Robot, CoveredRecheck, r.Covered
				d.Why = "the robot is under the emergency stop of " + r.Covered + ": work a stop paused goes to " +
					"another robot only through a requeue the estop-clear verb on that stop authorizes, or once " +
					"the stop is cleared"
			}
			continue
		}
		switch {
		case r.CancelConfirmed:
			d.Release = append(d.Release, r.Robot)
			d.Why = "the adapter acknowledged the cancel"
		case r.Ended:
			d.Release = append(d.Release, r.Robot)
			d.Why = "the robot reported the action ended"
		case leaseDead:
			d.Release = append(d.Release, r.Robot)
			d.Why = "the lease is provably dead"
		default:
			if d.Blocked == "" {
				d.Blocked = r.Robot
				d.Wait = dead.Sub(in.Now) + time.Second
				if in.Horizon == nil {
					d.Why = "the previous executor's lease horizon is not known; waiting the longest lease the " +
						"configuration admits plus the clock-skew margin"
				} else {
					d.Why = "the previous executor's lease is not provably dead"
				}
			}
		}
	}
	return d
}

// Latest returns the latest of the given times, ignoring nil and zero values.
func Latest(ts ...*time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t != nil && t.After(out) {
			out = *t
		}
	}
	return out
}

// HorizonOf is the action's recorded lease horizon (status.leaseExpiresAt), or nil.
func HorizonOf(action *fleetv1.FleetAction) *time.Time {
	if action.Status.LeaseExpiresAt == nil {
		return nil
	}
	t := action.Status.LeaseExpiresAt.Time
	return &t
}

// ReferenceOf is the latest moment the action is known to have re-entered Pending, been
// created, or settled: the unknown-horizon bound counts from it.
//
// A Paused action whose hold released the horizon has none: nothing records when the hold
// began, and creation can be long before the last lease the robot was granted. Its records
// are released on the adapter's word only (a zero reference never proves the lease dead).
func ReferenceOf(action *fleetv1.FleetAction) time.Time {
	if action.Status.Phase == fleetv1.ActionPhasePaused && action.Status.LeaseExpiresAt == nil {
		return time.Time{}
	}
	created := action.CreationTimestamp.Time
	return Latest(&created, metaTime(action.Status.PendingSince), metaTime(action.Status.CompletionTime),
		metaTime(action.Status.FailedAt))
}

func metaTime(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	tt := t.Time
	return &tt
}

// KeepsRecord reports whether a robot's record of an action is kept, whatever proves the
// robot off it: a stop covers the robot (covered names it), and the action is not being
// moved by a requeue the estop-clear check admitted.
func KeepsRecord(covered string, moveAuthorized bool) bool {
	return covered != "" && !moveAuthorized
}

// DeleteLosesKey reports whether deleting robot now would remove the executor guard's key:
// the robot records the action, and the guard has no proof from the objects that the robot is
// off it (in, with the robot as the one recorder and no word from its adapter). A bound robot
// is no exception: the action that keeps its horizon can be deleted and created again under
// its name, and with the robot gone nothing would record that it may still run it. It returns
// when the lease is provably dead, or the zero time when only the adapter's acknowledgement can
// prove it.
func DeleteLosesKey(robot string, in Input) (bool, time.Time) {
	if in.ActionGone {
		return true, time.Time{}
	}
	in.Recorders = []Recorder{{Robot: robot}}
	d := Decide(in)
	if d.Dispatchable() {
		return false, time.Time{}
	}
	if in.Horizon == nil && in.Reference.IsZero() {
		return true, time.Time{}
	}
	return true, in.Now.Add(d.Wait)
}

// StatusPatch is the patch every FleetAction status write of the control plane is made with: a
// merge patch from original, on the condition that the object is still at original's
// resourceVersion. A merge patch carries only the fields it changes, so without the condition a
// phase computed from a stale copy lands on whatever the API server holds, a terminal phase
// included; with it the write fails and the action is read again.
func StatusPatch(original *fleetv1.FleetAction) client.Patch {
	return client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})
}

// StopOutcome reports whether a failure a robot's adapter reports for an action is the outcome
// of a stop: stops names the stops that cover the robot, in force or pending. Such an action is
// Paused, not Failed, so neither its onFailure nor its task's failure policy applies and nothing
// is retried on another robot while the stop stands.
func StopOutcome(stops []string) bool { return len(stops) > 0 }

// WaitsForAPerson reports whether a robot's unfinished work waits for a person: a stop covers the
// robot (stops), the robot has not confirmed it, and the robot's lease is provably dead. Work under
// a stop moves only on the robot's own end report or an operator's decision (RFC-0001
// #safety-how-the-two-interact, invariant I1). A dead lease is no proof under a stop, and work the
// robot never reported started may have started with its running report lost, so no automatic
// step moves it, started or not: the work stays with its robot, and an operator inspects the robot,
// then requeues the work, cancels it, or clears the stop and resumes the robot. While the stop is
// confirmed, or the lease may be alive, the work stays with its robot too, and so does work an
// operator holds (spec.desiredState Paused or Returning, with no requeue asked): heldByOperator.
func WaitsForAPerson(stops []string, confirmed, leaseDead, heldByOperator bool) bool {
	return len(stops) > 0 && !confirmed && leaseDead && !heldByOperator
}

// PausedRequeueRuns reports whether a requeue request on a Paused action may run now. covered:
// a stop covers the robot that holds the work. seen: the control plane has handled the request
// before; askedUnderThese: when it first did, the same stops covered the robot. A request first
// handled with no stop, or under other stops, was asked before the stops that cover the robot
// now: the stop wins, and the request waits for their clear. A request handled for the first
// time under a stop is taken as asked under it, since the requeue check admits one only to that
// stop's authority.
func PausedRequeueRuns(covered, seen, askedUnderThese bool) bool {
	return !covered || !seen || askedUnderThese
}

// RequeueChecked reports whether a write carrying the requeue annotation is held to the requeue
// check: a create carrying it, or an update that adds it. A create under the name of work a stop
// paused would otherwise move that work with no check at all.
func RequeueChecked(create, hadAnnotation, hasAnnotation bool) bool {
	return hasAnnotation && (create || !hadAnnotation)
}

// KeepsStopFinalizer reports whether the stop finalizer stays on a robot: it carries its own stop
// (ownStop), or any stop holds it (underStop: a zone's or the namespace's stop issued to it,
// applied or not, or recorded on it, or an estop state other than Normal, Resuming included),
// whether or not it records an action. Deleted by any route, such a robot stays Terminating and
// under its stop, with any record it holds, until neither is true (RFC-0001 safety.md, the
// robot's record of the action).
func KeepsStopFinalizer(ownStop, underStop bool) bool {
	return ownStop || underStop
}

// GenerationCeiling is the highest assignment generation ever issued: well below the largest
// int64, so a counter never wraps (a wrapped token would be refused by every adapter as stale,
// for good). A generation past it is refused, not issued.
const GenerationCeiling int64 = 1 << 62

// NextGeneration is the assignment generation an action's next assignment carries: one above both
// the namespace's high-water mark and the action's previous generation. Every generation in a
// namespace is drawn this way from one stored mark, so the fencing tokens a robot is given rise
// across all its actions, and an action created again under a name never repeats or undercuts a
// token an earlier one used. No clock is read.
func NextGeneration(highWater, previous int64) int64 {
	return max(highWater, previous) + 1
}

// SeedGeneration is the mark a namespace's generation store starts from: no lower than the highest
// generation an existing action holds, nor than now in microseconds, which is above every per-action
// count issued before the store existed.
func SeedGeneration(highest int64, now time.Time) int64 {
	return max(highest, now.UnixMicro())
}
