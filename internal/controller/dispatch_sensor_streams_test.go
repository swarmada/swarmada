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
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// The G2 sensor-stream gate, wired.
//
// Before this, `scheduler.SensorStreamOffered` and `ReasonSensorStreamNotOffered`
// existed and NOTHING called them: the predicate's only callers were its own tests
// and the reason string was exported and never written to any object. An action
// naming a camera no robot offered reached selection like any other, was reported as
// "no eligible robot", and an operator had no way to tell a missing declaration from
// a busy fleet. These tests assert the wiring, not the predicate — the predicate has
// its own tests in internal/scheduler.

// streamRobot builds an Idle, dispatch-eligible robot declaring the named streams.
// Capabilities are supplied separately because "declares the stream" and "can serve
// it" are the two halves the gate must distinguish.
func streamRobot(name string, streamIDs ...string) *fleetv1.Robot {
	rob := robotInPhase(name, fleetv1.RobotPhaseIdle, "")
	for _, id := range streamIDs {
		rob.Status.SensorStreams = append(rob.Status.SensorStreams,
			fleetv1.SensorStreamEntry{StreamID: id})
	}
	return rob
}

// streamAction builds a Pending external action consuming one sensor stream from the
// named robot. External because a sensor-stream input only exists under spec.executor
// — a robot action has no inputs at all.
func streamAction(name, robotName, streamID string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeExternal,
			Executor: &fleetv1.ActionExecutor{
				EndpointRef: "object-classifier",
				Inputs: []fleetv1.ExecutorInput{{
					Name: "frames",
					SensorStream: &fleetv1.SensorStreamInput{
						RobotRef: fleetv1.SensorStreamRobotRef{Name: strPtr(robotName)},
						StreamID: streamID,
					},
				}},
			},
		},
	}
}

func dependenciesCondition(t *testing.T, action *fleetv1.FleetAction) *metav1.Condition {
	t.Helper()
	return apimeta.FindStatusCondition(action.Status.Conditions, conditionExternalDependenciesReady)
}

// THE CRITICAL ONE. An action requiring a stream no candidate robot declares stays
// Pending with SensorStreamNotOffered, and NO ROBOT IS COMMITTED — which is the whole
// reason this gate is at scheduling rather than at dispatch. An eligible robot is
// present and idle precisely so that "not assigned" cannot be confused with "nothing
// was available".
func TestSensorStreamGate_NotOffered_StaysPendingAndCommitsNoRobot(t *testing.T) {
	r, c := newActionReconciler(t,
		streamAction("classify", "yard-robot-3", "front-rgb"),
		streamRobot("yard-robot-3", "mic-array"),
	)

	reconcileAction(t, r, "classify")

	act := getAction(t, c, "classify")
	if act.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending — a missing stream is a wait, never a failure", act.Status.Phase)
	}
	if act.Status.AssignedRobot != "" {
		t.Fatalf("a robot was committed (%q) despite an unsatisfied stream dependency — "+
			"this is the exact cost gating at scheduling exists to avoid", act.Status.AssignedRobot)
	}
	cond := dependenciesCondition(t, act)
	if cond == nil {
		t.Fatal("no ExternalDependenciesReady condition was written; the refusal is invisible to an operator")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != scheduler.ReasonSensorStreamNotOffered {
		t.Fatalf("condition = %s/%s, want False/%s", cond.Status, cond.Reason, scheduler.ReasonSensorStreamNotOffered)
	}
	// The message must name the stream and the robot. "SensorStreamNotOffered" alone
	// does not tell an operator which input or which robot to look at.
	for _, want := range []string{"front-rgb", "yard-robot-3", "frames"} {
		if !strings.Contains(cond.Message, want) {
			t.Fatalf("message %q does not name %q", cond.Message, want)
		}
	}
	// It must also say what the robot DOES declare, so a typo is distinguishable from
	// a genuinely absent sensor.
	if !strings.Contains(cond.Message, "mic-array") {
		t.Fatalf("message %q does not report what the robot declares; a misspelled "+
			"streamId is then indistinguishable from an absent sensor", cond.Message)
	}
	// Failure is not what happened. A failureReason on a waiting action reads as a
	// terminal outcome to every consumer that has learned to treat it as one.
	if act.Status.FailureReason != "" {
		t.Fatalf("failureReason = %q on a Pending action; the action has not failed", act.Status.FailureReason)
	}
	if act.Status.RetryCount != 0 {
		t.Fatalf("retryCount = %d; a missing declaration is not a flaky robot", act.Status.RetryCount)
	}
}

// The complement, and the one that keeps the gate from being a blanket refusal: a
// robot that DOES declare the stream lets the action through. Without this, a gate
// that always refused would pass the test above.
func TestSensorStreamGate_Offered_DoesNotHoldTheAction(t *testing.T) {
	r, c := newActionReconciler(t,
		streamAction("classify", "yard-robot-3", "front-rgb"),
		streamRobot("yard-robot-3", "front-rgb", "mic-array"),
	)

	reconcileAction(t, r, "classify")

	act := getAction(t, c, "classify")
	if cond := dependenciesCondition(t, act); cond != nil && cond.Status == metav1.ConditionFalse {
		t.Fatalf("the gate refused an action whose stream IS offered: %s", cond.Message)
	}
	// It is still Pending — an external action is dispatched to a ComputeEndpoint and
	// no such dispatch path exists — but it is Pending for the ORDINARY reason, which
	// is what this asserts: the sensor-stream gate is not what is holding it.
	if act.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending", act.Status.Phase)
	}
}

// "The robot is not a candidate" and "the robot does not declare the stream" are
// different facts an operator fixes differently, and the message must say which.
func TestSensorStreamGate_RobotNotACandidate_SaysSo(t *testing.T) {
	r, c := newActionReconciler(t,
		streamAction("classify", "yard-robot-3", "front-rgb"),
		streamRobot("some-other-robot", "front-rgb"),
	)

	reconcileAction(t, r, "classify")

	cond := dependenciesCondition(t, getAction(t, c, "classify"))
	if cond == nil || cond.Reason != scheduler.ReasonSensorStreamNotOffered {
		t.Fatalf("condition = %v, want SensorStreamNotOffered", cond)
	}
	if !strings.Contains(cond.Message, "not among the") {
		t.Fatalf("message %q does not distinguish an absent robot from an absent stream", cond.Message)
	}
}

// A stream whose required capability is not Active MUST NOT be treated as offered
// (rfcs/rfc-0001-core-spec/sensor-streams.md, Declaration). Declaring `front-rgb`
// while the camera capability is Paused is a robot that cannot produce frames, and
// activating it would commit the control plane to switching on a sensor that is off.
func TestSensorStreamGate_RequiredCapabilityNotActive_IsNotOffered(t *testing.T) {
	rob := streamRobot("yard-robot-3")
	rob.Status.SensorStreams = []fleetv1.SensorStreamEntry{{
		StreamID:             "front-rgb",
		RequiredCapabilities: []string{"camera_front"},
	}}
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "camera_front", Status: fleetv1.CapabilityStatusPaused},
	}
	r, c := newActionReconciler(t, streamAction("classify", "yard-robot-3", "front-rgb"), rob)

	reconcileAction(t, r, "classify")

	act := getAction(t, c, "classify")
	if act.Status.AssignedRobot != "" {
		t.Fatalf("a robot was committed for a stream it cannot serve (%q)", act.Status.AssignedRobot)
	}
	cond := dependenciesCondition(t, act)
	if cond == nil || cond.Reason != scheduler.ReasonSensorStreamNotOffered {
		t.Fatalf("condition = %v, want SensorStreamNotOffered", cond)
	}
	if !strings.Contains(cond.Message, "camera_front") {
		t.Fatalf("message %q does not name the capability that is not Active — the "+
			"operator cannot tell this from a missing declaration", cond.Message)
	}
}

// `{fromMember}` resolves through the owning FleetTask's member projection. This is
// the common case the design names — "the robot doing the patrolling" — and the only
// one that needs a client read.
func TestSensorStreamGate_FromMember_ResolvesThroughTheOwningTask(t *testing.T) {
	act := streamAction("classify", "", "front-rgb")
	act.Labels = map[string]string{ownedByLabel: "patrol-task"}
	act.Spec.Executor.Inputs[0].SensorStream.RobotRef = fleetv1.SensorStreamRobotRef{
		FromMember: strPtr("patrol"),
	}
	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-task", Namespace: actionNS},
		Status: fleetv1.FleetTaskStatus{Actions: []fleetv1.FleetTaskActionStatus{
			{Name: "patrol", AssignedRobot: "yard-robot-3"},
		}},
	}
	r, c := newActionReconciler(t, act, task, streamRobot("yard-robot-3", "front-rgb"))

	reconcileAction(t, r, "classify")

	if cond := dependenciesCondition(t, getAction(t, c, "classify")); cond != nil && cond.Status == metav1.ConditionFalse {
		t.Fatalf("fromMember did not resolve to the member's assigned robot: %s", cond.Message)
	}
}

// An unresolved `{fromMember}` HOLDS, it does not pass. A member with no robot yet is
// not evidence that the stream is available; skipping the check would let the action
// reach dispatch carrying a reference to nothing, which is the failure the gate exists
// to prevent. It resolves on a later pass with no edit.
func TestSensorStreamGate_FromMemberUnassigned_HoldsFailClosed(t *testing.T) {
	act := streamAction("classify", "", "front-rgb")
	act.Labels = map[string]string{ownedByLabel: "patrol-task"}
	act.Spec.Executor.Inputs[0].SensorStream.RobotRef = fleetv1.SensorStreamRobotRef{
		FromMember: strPtr("patrol"),
	}
	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-task", Namespace: actionNS},
		Status: fleetv1.FleetTaskStatus{Actions: []fleetv1.FleetTaskActionStatus{
			{Name: "patrol"}, // generated, not yet assigned
		}},
	}
	r, c := newActionReconciler(t, act, task, streamRobot("yard-robot-3", "front-rgb"))

	reconcileAction(t, r, "classify")

	cond := dependenciesCondition(t, getAction(t, c, "classify"))
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatal("an unresolved fromMember passed the gate; fail-closed is the only safe direction here")
	}
	if !strings.Contains(cond.Message, "patrol") {
		t.Fatalf("message %q does not name the member that could not be resolved", cond.Message)
	}
}

// RA-1. The refusal is written ONCE on the transition. A scheduling pass that
// re-observes the same absence must produce a byte-identical status, so the patch is
// a no-op and the object's resourceVersion does not move — the same discipline every
// other repeating Pending write in this controller is held to.
func TestSensorStreamGate_ReObservingTheSameAbsenceWritesNothing(t *testing.T) {
	r, c := newActionReconciler(t,
		streamAction("classify", "yard-robot-3", "front-rgb"),
		streamRobot("yard-robot-3", "mic-array"),
	)

	reconcileAction(t, r, "classify")
	first := getAction(t, c, "classify")
	firstRV, firstTransition := first.ResourceVersion, dependenciesCondition(t, first).LastTransitionTime

	for i := 0; i < 4; i++ {
		reconcileAction(t, r, "classify")
	}

	again := getAction(t, c, "classify")
	if again.ResourceVersion != firstRV {
		t.Fatalf("four re-observations of the SAME absence moved resourceVersion %s -> %s; "+
			"the reason must be written once on the transition, not on every pass",
			firstRV, again.ResourceVersion)
	}
	if !dependenciesCondition(t, again).LastTransitionTime.Equal(&firstTransition) {
		t.Fatal("lastTransitionTime moved without a transition")
	}
}

// An action carrying no executor at all — every robot action in the fleet — is not
// touched by the gate, and pays no client read for it.
func TestSensorStreamGate_RobotActionIsUntouched(t *testing.T) {
	r, _ := newActionReconciler(t)
	act := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "nav", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
	}
	if refusal := r.sensorStreamGate(context.Background(), act, nil); refusal != nil {
		t.Fatalf("a robot action was gated on a sensor stream it does not name: %s", refusal.Message)
	}
}

// A held action that becomes satisfiable does not carry the stale False forward. A
// condition that only ever gets worse is a condition an operator learns to ignore.
func TestSensorStreamGate_ClearsOnceTheStreamAppears(t *testing.T) {
	r, c := newActionReconciler(t,
		streamAction("classify", "yard-robot-3", "front-rgb"),
		streamRobot("yard-robot-3", "mic-array"),
	)
	reconcileAction(t, r, "classify")
	if cond := dependenciesCondition(t, getAction(t, c, "classify")); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatal("precondition: the action should be held")
	}

	// The adapter reconnects and re-declares; the projection now carries front-rgb.
	rob := &fleetv1.Robot{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "yard-robot-3", Namespace: actionNS}, rob); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	rob.Status.SensorStreams = append(rob.Status.SensorStreams,
		fleetv1.SensorStreamEntry{StreamID: "front-rgb"})
	if err := c.Status().Update(context.Background(), rob); err != nil {
		t.Fatalf("update robot status: %v", err)
	}

	reconcileAction(t, r, "classify")

	cond := dependenciesCondition(t, getAction(t, c, "classify"))
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %v, want True once the dependency is satisfied", cond)
	}
}
