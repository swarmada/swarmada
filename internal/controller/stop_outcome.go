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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/executorguard"
)

// What a stop makes of work its robot was given (RFC-0001 #safety-fleetaction-behaviour-during-estop):
// a failure the robot reports while a stop covers it is a stop outcome, a requeue asked before the
// stop waits for its clear, and the work of a robot that never confirmed its stop moves once the
// robot's lease is provably dead. Each is told to the operator by a Warning Event on the action
// and on the FleetTask that owns it.
const (
	// EventActionFailedUnderEstop: the robot's adapter reported the action failed while a stop
	// covered the robot. The action is Paused, not Failed, so neither its onFailure nor its
	// task's failure policy applies.
	EventActionFailedUnderEstop = "ActionFailedUnderEstop"
	// EventRequeueHeldByEstop: a requeue asked before a stop reached the robot is held while
	// the stop stands.
	EventRequeueHeldByEstop = "RequeueHeldByEstop"
)

// stopNames names the stops that cover robot, in force or pending, as the stop guard answers
// it over a read of the API server through rd (estopscope.Covering), and the robot's own stop
// when its estop state or its record holds one; sorted, nil when none does. A question the
// guard cannot answer is an error.
func stopNames(ctx context.Context, rd client.Reader, robot *fleetv1.Robot) ([]string, error) {
	cov, err := estopscope.Covering(ctx, rd, robot)
	if err != nil {
		return nil, fmt.Errorf("deciding which stops cover robot %s: %w", robot.Name, err)
	}
	names := cov.Names()
	if robotUnderEstop(robot) && !cov.Covered() {
		names = append(names, "robot/"+robot.Name)
	}
	sort.Strings(names)
	return names, nil
}

// stopConfirmed reports whether robot has confirmed its stop: its estop state is Stopped.
// Stopping, Failed, and a stop recorded but not yet written are not confirmed.
func stopConfirmed(robot *fleetv1.Robot) bool {
	return robot.Status.EstopState == fleetv1.RobotEstopStopped
}

// ownerTask is the FleetTask that owns action as its controller, or nil. It is built from the
// owner reference, so emitting an Event on it needs no read.
func ownerTask(action *fleetv1.FleetAction) *fleetv1.FleetTask {
	ref := metav1.GetControllerOf(action)
	if ref == nil || ref.Kind != "FleetTask" {
		return nil
	}
	return &fleetv1.FleetTask{ObjectMeta: metav1.ObjectMeta{Namespace: action.Namespace, Name: ref.Name, UID: ref.UID}}
}

// warnActionAndTask emits a Warning Event with reason and msg on action and on the FleetTask
// that owns it.
func warnActionAndTask(rec record.EventRecorder, action *fleetv1.FleetAction, reason, msg string) {
	if rec == nil {
		return
	}
	rec.Event(action, corev1.EventTypeWarning, reason, msg)
	if task := ownerTask(action); task != nil {
		rec.Event(task, corev1.EventTypeWarning, reason, "member "+action.Name+": "+msg)
	}
}

// failedUnderStopMessage is the status message of an action whose robot reported it failed
// while a stop covered the robot. It quotes the adapter's reason.
func failedUnderStopMessage(reason string, stops []string) string {
	return fmt.Sprintf("paused by estop — the robot reported the action failed while the emergency stop of %s "+
		"covered it (%q): a stop outcome, not a failure, so it is not retried; an operator resume, a cancel, or a "+
		"requeue by a holder of estop-clear on that stop is required", strings.Join(stops, ", "), reason)
}

// EventActionAwaitingOperatorDecision is the Event reason when work a stop holds has no proof that
// would release it, and only a holder of estop-clear on the stop decides where it goes (RFC-0001
// #safety-fleetaction-behaviour-during-estop).
const EventActionAwaitingOperatorDecision = "ActionAwaitingOperatorDecision"

// heldRecord reports whether robot holds a record of work under a stop: it records an action
// (status.assignedAction) and a stop holds it, recorded on the robot (its estop state, or a stop
// written onto it) or issued over it and not yet written (the stop guard, read through rd).
func heldRecord(ctx context.Context, rd client.Reader, robot *fleetv1.Robot) (bool, error) {
	if robot.Status.AssignedAction == "" {
		return false, nil
	}
	if robotUnderEstop(robot) {
		return true, nil
	}
	stops, err := stopNames(ctx, rd, robot)
	return len(stops) > 0, err
}

// carriesOwnStop reports whether robot carries its own stop: its trigger, or the processed marker
// that stands until the clear has been applied.
func carriesOwnStop(robot *fleetv1.Robot) bool {
	_, firing := robot.Annotations[annEstopTriggered]
	_, processed := robot.Annotations[annEstopProcessed]
	return firing || processed
}

// robotKeepsStopFinalizer reports whether the stop finalizer stays on robot: it carries its own
// stop, or any stop holds it — its estop state is not Normal (Resuming included), a stop is
// recorded on it, or a zone's or the namespace's stop is issued over it and not yet written —
// whether or not it records an action. Deleted by any route, such a robot stays Terminating and
// under its stop until none is true. A question the stop guard cannot answer is an error, and
// the finalizer stays.
func robotKeepsStopFinalizer(ctx context.Context, rd client.Reader, robot *fleetv1.Robot) (bool, error) {
	if executorguard.KeepsStopFinalizer(carriesOwnStop(robot), estopHoldsWork(robot)) {
		return true, nil
	}
	stops, err := stopNames(ctx, rd, robot)
	if err != nil {
		return true, err
	}
	return executorguard.KeepsStopFinalizer(false, len(stops) > 0), nil
}

// awaitingDecision is the status message and Event text of work a stop holds that only a holder of
// estop-clear on the stop can move: what holds it, then who decides and how.
func awaitingDecision(action *fleetv1.FleetAction, stops []string, why string) string {
	return fmt.Sprintf("awaiting operator decision (estop-clear on %s): %s. A holder of estop-clear on that stop "+
		"(swarmada:admin by default) decides: requeue it to another robot (kubectl annotate fleetaction %s "+
		"swarmada.io/requeue-requested=\"<reason>\" -n %s), cancel it (swarmctl cancel action %s --reason \"<text>\" -n %s), "+
		"or clear the stop and resume the robot", strings.Join(stops, ", "), why, action.Name, action.Namespace,
		action.Name, action.Namespace)
}
