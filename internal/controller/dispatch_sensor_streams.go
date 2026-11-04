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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// The G2 scheduling gate for sensor streams — the wiring, not the predicate.
//
// # It eliminates the ACTION, not a candidate, and it is NOT terminal
//
// Like candidate filter 11 it is applied before selection rather than per robot,
// because its answer does not vary by robot: an action naming `front-rgb` on
// `{name: yard-robot-3}` asks one question about one robot, and asking it once per
// candidate would report a single missing stream as "no eligible robot".
//
// Unlike filter 11 the refusal is NOT terminal. Every way this gate fails is a fact
// about today: a robot that reconnects and re-declares its streams, a capability that
// returns to Active, a sibling member that gets assigned, all make the same action
// dispatchable with no edit. So the action stays `Pending`, exactly as
// rfcs/rfc-0001-core-spec/sensor-streams.md requires — "an action requiring a sensor
// stream no candidate robot offers MUST remain Pending with reason
// SensorStreamNotOffered. It MUST NOT be assigned."
//
// # Why gating here rather than at dispatch is the whole point
//
// Assignment OCCUPIES a robot, and under most action types the robot has begun moving
// by the time a missing dependency would be discovered at dispatch. A fleet that
// drives to a station to classify frames from a camera that was never offered has
// spent battery, floor time and a traffic reservation to learn something the control
// plane could have known before committing anyone. No robot is committed by a failed
// gate — that is the property, and the test asserts it by name.
//
// # Absence of a resolvable robot is a HOLD, never a pass
//
// `{fromMember: patrol}` names a sibling member of the same FleetTask, and that
// member's robot is unknown until the member is assigned. An unresolved reference is
// treated as not-offered (fail closed) rather than skipped: skipping would let an
// action carrying an unresolvable stream reference reach dispatch, which is the
// failure this gate exists to prevent. It resolves on a later pass with no edit, so
// holding costs a requeue and nothing else.

// conditionExternalDependenciesReady is the G2 condition type. False with a reason
// from the closed dependency vocabulary means the action is withheld from assignment
// because something it names is absent or not ready — never because the fleet is busy.
const conditionExternalDependenciesReady = "ExternalDependenciesReady"

// sensorStreamRefusal is one unsatisfied sensor-stream requirement, in words an
// operator can act on. It names the input, the stream and the robot, because
// "SensorStreamNotOffered" alone does not say which of an action's inputs is the
// problem or which robot was asked.
type sensorStreamRefusal struct {
	// Reason is the closed-vocabulary reason (scheduler.ReasonSensorStreamNotOffered).
	Reason string
	// Message names the input, the stream, the robot, and which of the three
	// distinguishable causes fired.
	Message string
}

// sensorStreamGate reports the first sensor-stream requirement this action carries
// that no candidate robot satisfies, or nil when every requirement is satisfied (and
// when the action carries none at all, which is every robot action in the fleet).
//
// `candidates` is the ALREADY-FILTERED eligible set, so a robot withheld by the
// adapter-readiness or zone filters cannot satisfy a stream requirement here. That is
// deliberate: a stream is served by the same ControlStream the withheld robot is not
// reachable on, so counting it would offer the control plane a camera it cannot
// switch on.
//
// Inputs are examined in spec order and the FIRST unsatisfied one is reported. Not
// all of them: the message is written to status on every requeue, so it must be a
// stable string over a stable fleet (RA-1), and spec order is stable where a map or a
// set would not be.
func (r *FleetActionReconciler) sensorStreamGate(
	ctx context.Context, action *fleetv1.FleetAction, candidates []fleetv1.Robot,
) *sensorStreamRefusal {
	if action.Spec.Executor == nil || len(action.Spec.Executor.Inputs) == 0 {
		return nil
	}

	// Resolved lazily and at most once per reconcile: an action with no
	// {fromMember} reference never reads the FleetTask at all.
	var memberRobot func(string) (string, bool)
	memberRobot = func(member string) (string, bool) {
		lookup := r.taskMemberRobots(ctx, action)
		memberRobot = func(m string) (string, bool) { // memoised for the remaining inputs
			robot, ok := lookup[m]
			return robot, ok
		}
		return memberRobot(member)
	}

	for i := range action.Spec.Executor.Inputs {
		in := &action.Spec.Executor.Inputs[i]
		if in.SensorStream == nil {
			continue
		}
		robotName, resolved := scheduler.ResolveSensorStreamRobotRef(in.SensorStream.RobotRef, memberRobot)
		if !resolved {
			return &sensorStreamRefusal{
				Reason: scheduler.ReasonSensorStreamNotOffered,
				Message: fmt.Sprintf(
					"input %q requires sensor stream %q from member %q, whose robot is not yet assigned; "+
						"no robot is committed while the reference is unresolved",
					in.Name, in.SensorStream.StreamID, refMemberName(in.SensorStream.RobotRef)),
			}
		}

		offered, robotFound := scheduler.SensorStreamOffered(candidates, robotName, in.SensorStream.StreamID)
		if offered {
			continue
		}
		if !robotFound {
			// The robot is not among the candidates. That is a different fact from
			// "it does not declare the stream" and an operator fixes it differently
			// — one is a fleet-membership/eligibility problem, the other a
			// declaration problem.
			return &sensorStreamRefusal{
				Reason: scheduler.ReasonSensorStreamNotOffered,
				Message: fmt.Sprintf(
					"input %q requires sensor stream %q from robot %q, which is not among the "+
						"%d candidate robot(s) for this action",
					in.Name, in.SensorStream.StreamID, robotName, len(candidates)),
			}
		}
		if unmet := scheduler.SensorStreamCapabilitiesUnmet(candidates, robotName, in.SensorStream.StreamID); len(unmet) > 0 {
			return &sensorStreamRefusal{
				Reason: scheduler.ReasonSensorStreamNotOffered,
				Message: fmt.Sprintf(
					"input %q requires sensor stream %q on robot %q, which declares it but cannot serve it: "+
						"required capability not Active: %s",
					in.Name, in.SensorStream.StreamID, robotName, strings.Join(unmet, ", ")),
			}
		}
		return &sensorStreamRefusal{
			Reason: scheduler.ReasonSensorStreamNotOffered,
			Message: fmt.Sprintf(
				"input %q requires sensor stream %q, which robot %q does not declare in "+
					"status.sensorStreams[] (declares: %s)",
				in.Name, in.SensorStream.StreamID, robotName, declaredStreams(candidates, robotName)),
		}
	}
	return nil
}

// taskMemberRobots maps every member name of this action's owning FleetTask to the
// robot currently assigned to it, reading FleetTask.status.actions[] — the projection
// the composite controller already maintains. Read-only, and empty for an action that
// is not a task member at all (which is then correctly unresolvable: `fromMember` on
// a standalone FleetAction names nothing).
//
// A member with no robot yet is ABSENT from the map rather than present with an empty
// name, so the caller's `ok` is false and the gate holds instead of asking whether
// robot "" offers a stream.
func (r *FleetActionReconciler) taskMemberRobots(ctx context.Context, action *fleetv1.FleetAction) map[string]string {
	taskName := action.Labels[ownedByLabel]
	if taskName == "" {
		return nil
	}
	task := &fleetv1.FleetTask{}
	if err := r.Get(ctx, types.NamespacedName{Name: taskName, Namespace: action.Namespace}, task); err != nil {
		if !errors.IsNotFound(err) {
			// A read failure is not evidence of a robot. Returning an empty map holds
			// the action, which is the fail-closed direction.
			log.FromContext(ctx).V(1).Info("resolving fromMember: FleetTask unreadable; holding the action",
				"task", taskName, "reason", err.Error())
		}
		return nil
	}
	out := make(map[string]string, len(task.Status.Actions))
	for _, m := range task.Status.Actions {
		if m.AssignedRobot != "" {
			out[m.Name] = m.AssignedRobot
		}
	}
	return out
}

// refMemberName renders the member a robotRef names, for a message written before the
// reference could be resolved. Never returns an empty string silently: an unresolved
// `{name: ...}` cannot occur (ResolveSensorStreamRobotRef always resolves it), so the
// only way here is a `{fromMember: ...}`.
func refMemberName(ref fleetv1.SensorStreamRobotRef) string {
	if ref.FromMember != nil {
		return *ref.FromMember
	}
	return "<unset>"
}

// declaredStreams renders what the robot DOES declare, sorted, so the message tells an
// operator whether the stream id is misspelled or genuinely absent. Sorted because
// status.sensorStreams[] order is the adapter's and a reordering must not rewrite the
// action's status (RA-1).
func declaredStreams(candidates []fleetv1.Robot, robotName string) string {
	for _, rob := range candidates {
		if rob.Name != robotName {
			continue
		}
		ids := make([]string, 0, len(rob.Status.SensorStreams))
		for _, s := range rob.Status.SensorStreams {
			ids = append(ids, s.StreamID)
		}
		if len(ids) == 0 {
			return "none"
		}
		sort.Strings(ids)
		return strings.Join(ids, ", ")
	}
	return "none"
}

// applySensorStreamRefusal records a failed gate in the three places an operator may
// be looking, and does it so that a re-observation of the SAME absence writes nothing
// (RA-1): the condition carries the same status/reason/message, so
// SetStatusCondition leaves lastTransitionTime alone and the patch is a no-op; the
// Event is emitted only when the condition actually transitioned.
//
// status.failureReason is NOT set. The action has not failed — it is waiting, and a
// failureReason on a Pending action reads as a terminal outcome to every consumer
// that has learned to treat it as one.
func (r *FleetActionReconciler) applySensorStreamRefusal(action *fleetv1.FleetAction, refusal *sensorStreamRefusal) {
	// Captured BEFORE the write: "did this transition" is a question about the prior
	// observation, and SetStatusCondition overwrites the answer.
	before := conditionReasonMessage(action.Status.Conditions, conditionExternalDependenciesReady)
	transitioned := before != string(metav1.ConditionFalse)+"\x00"+refusal.Reason+"\x00"+refusal.Message

	apimeta.SetStatusCondition(&action.Status.Conditions, metav1.Condition{
		Type:               conditionExternalDependenciesReady,
		Status:             metav1.ConditionFalse,
		Reason:             refusal.Reason,
		Message:            refusal.Message,
		ObservedGeneration: action.Generation,
	})
	action.Status.Phase = fleetv1.ActionPhasePending
	action.Status.Message = refusal.Message

	if transitioned && r.Recorder != nil {
		r.Recorder.Event(action, corev1.EventTypeWarning, refusal.Reason, refusal.Message)
	}
}

// clearSensorStreamRefusal marks the dependencies satisfied once they are, so an
// action that was held and is now schedulable does not carry a stale False condition
// into its assignment. A no-op when the condition was never written, which is the
// common case for every action in the fleet that names no stream.
func clearSensorStreamRefusal(action *fleetv1.FleetAction) {
	if apimeta.FindStatusCondition(action.Status.Conditions, conditionExternalDependenciesReady) == nil {
		return
	}
	apimeta.SetStatusCondition(&action.Status.Conditions, metav1.Condition{
		Type:               conditionExternalDependenciesReady,
		Status:             metav1.ConditionTrue,
		Reason:             "ExternalDependenciesReady",
		Message:            "every external dependency this action names is present and ready",
		ObservedGeneration: action.Generation,
	})
}

// conditionReasonMessage joins a condition's status, reason and message with a
// separator that cannot occur in any of them, so two observations compare equal only
// when ALL THREE halves match. An absent condition returns a value no live condition
// can produce, so "never written" never compares equal to "written and unchanged".
func conditionReasonMessage(conds []metav1.Condition, condType string) string {
	c := apimeta.FindStatusCondition(conds, condType)
	if c == nil {
		return "\x00absent"
	}
	return string(c.Status) + "\x00" + c.Reason + "\x00" + c.Message
}
