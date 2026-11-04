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
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// AN ADAPTER'S REASON FOR DECLINING AN ACTION REACHES THE ACTION.
//
// When the scheduler asks an adapter whether it can serve a concrete action (§9.2.3) and the
// adapter says no with a reason, the action says who refused and why. The tally alone, "1 passed
// every filter but no adapter would serve this action instance", shows THAT a robot refused but
// not WHY, nor which one.
//
// The last decline is carried on the action as condition AdapterDeclined, naming the
// adapter, the robot and the adapter's own words, written on the transition only (RA-1), with
// one Warning Event per new reason. Retry and placement are untouched: the action stays
// Pending, retryCount stays 0, the next candidate is still tried.

const keepoutReason = "the goal (0.45, 1.11) lies inside keep-out zone 'floor-room-2-hearth' " +
	"(marble hearth below the lidar plane); refused before any motion"

func declining(reasons map[string]string) *stubValidator {
	v := &stubValidator{byRobot: map[string]command.ValidateOutcome{}}
	for robot, why := range reasons {
		v.byRobot[robot] = command.ValidateOutcome{Servable: false, Message: why}
	}
	return v
}

func warnings(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func declinedCondition(t *testing.T, a *fleetv1.FleetAction) *metav1.Condition {
	t.Helper()
	c := apimeta.FindStatusCondition(a.Status.Conditions, "AdapterDeclined")
	if c == nil {
		t.Fatalf("no AdapterDeclined condition on the action (conditions %v, message %q): the "+
			"adapter's reason reached the manager's log and nothing an operator reads",
			a.Status.Conditions, a.Status.Message)
	}
	return c
}

func TestAdapterDeclined_TheReasonReachesTheActionStatus(t *testing.T) {
	r, c := newActionReconciler(t, pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Validator = declining(map[string]string{"r1": keepoutReason})
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	reconcileAction(t, r, "t1")
	got := getAction(t, c, "t1")
	cond := declinedCondition(t, got)
	if cond.Status != metav1.ConditionTrue || cond.Reason != "AdapterDeclined" {
		t.Errorf("condition = %s/%s, want True/AdapterDeclined", cond.Status, cond.Reason)
	}
	for _, want := range []string{testActionAdapter, "r1", keepoutReason} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("condition message does not name %q:\n%s", want, cond.Message)
		}
	}
	// Retry and placement are untouched.
	if got.Status.Phase != fleetv1.ActionPhasePending || got.Status.RetryCount != 0 ||
		got.Status.AssignedRobot != "" || got.Status.FailedAt != nil {
		t.Errorf("phase %q, retryCount %d, assignedRobot %q, failedAt %v; want Pending, 0, none, "+
			"none — a decline is a scheduling miss, not a failure",
			got.Status.Phase, got.Status.RetryCount, got.Status.AssignedRobot, got.Status.FailedAt)
	}
	if !strings.Contains(got.Status.Message, "no adapter would serve this action instance") {
		t.Errorf("status.message changed: %q", got.Status.Message)
	}
	ev := warnings(rec)
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning AdapterDeclined ") ||
		!strings.Contains(ev[0], keepoutReason) {
		t.Errorf("events = %q, want one Warning AdapterDeclined carrying the reason", ev)
	}
}

func TestAdapterDeclined_TheSameReasonAgainWritesNothing(t *testing.T) {
	r, c := newActionReconciler(t, pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	r.Validator = declining(map[string]string{"r1": keepoutReason})
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	reconcileAction(t, r, "t1")
	first := *declinedCondition(t, getAction(t, c, "t1"))
	for i := 0; i < 3; i++ {
		reconcileAction(t, r, "t1")
	}
	again := declinedCondition(t, getAction(t, c, "t1"))
	if !again.LastTransitionTime.Equal(&first.LastTransitionTime) || again.Message != first.Message {
		t.Errorf("the condition moved on an unchanged reason: %v -> %v", first, *again)
	}
	if ev := warnings(rec); len(ev) != 1 {
		t.Errorf("%d events for one reason repeated four times, want 1: %q", len(ev), ev)
	}
}

func TestAdapterDeclined_ANewReasonIsANewEvent(t *testing.T) {
	r, c := newActionReconciler(t, pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	v := declining(map[string]string{"r1": keepoutReason})
	r.Validator = v
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	reconcileAction(t, r, "t1")
	v.byRobot["r1"] = command.ValidateOutcome{Servable: false, Message: "the battery is below the return threshold"}
	reconcileAction(t, r, "t1")
	cond := declinedCondition(t, getAction(t, c, "t1"))
	if !strings.Contains(cond.Message, "the battery is below the return threshold") ||
		strings.Contains(cond.Message, keepoutReason) {
		t.Errorf("the condition does not carry the LAST reason: %q", cond.Message)
	}
	if ev := warnings(rec); len(ev) != 2 {
		t.Errorf("%d events for two reasons, want 2: %q", len(ev), ev)
	}
}

func TestAdapterDeclined_TheLastOfSeveralDeclinesIsCarried(t *testing.T) {
	r, c := newActionReconciler(t, pendingActionFor("t1"),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""), robotInPhase("r2", fleetv1.RobotPhaseIdle, ""))
	v := declining(map[string]string{"r1": "reason of r1", "r2": "reason of r2"})
	r.Validator = v
	r.Recorder = record.NewFakeRecorder(16)

	reconcileAction(t, r, "t1")
	if len(v.calls) != 2 {
		t.Fatalf("asked %v, want both robots once (the next candidate is still tried)", v.calls)
	}
	last := v.calls[len(v.calls)-1]
	cond := declinedCondition(t, getAction(t, c, "t1"))
	if !strings.Contains(cond.Message, "reason of "+last) || !strings.Contains(cond.Message, `"`+last+`"`) {
		t.Errorf("condition = %q, want the last asked (%s)", cond.Message, last)
	}
}

func TestAdapterDeclined_AnotherRobotStillTakesTheWork(t *testing.T) {
	r, c := newActionReconciler(t, pendingActionFor("t1"),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""), robotInPhase("r2", fleetv1.RobotPhaseIdle, ""))
	r.Validator = declining(map[string]string{"r1": keepoutReason})
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec

	reconcileAction(t, r, "t1")
	got := getAction(t, c, "t1")
	if got.Status.Phase != fleetv1.ActionPhaseAssigned || got.Status.AssignedRobot != "r2" {
		t.Fatalf("phase %q on %q, want Assigned to r2: placement must not change",
			got.Status.Phase, got.Status.AssignedRobot)
	}
	for _, e := range warnings(rec) {
		if strings.Contains(e, "AdapterDeclined") {
			t.Errorf("an AdapterDeclined event on an action another robot took: %q", e)
		}
	}
}

func TestAdapterDeclined_OnlyADeclineWithAReasonIsCarried(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    *stubValidator
	}{
		{"unreachable", &stubValidator{errFor: map[string]error{"r1": errors.New("no ControlStream")}}},
		{"unsupported", &stubValidator{byRobot: map[string]command.ValidateOutcome{
			"r1": {Unsupported: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newActionReconciler(t, pendingActionFor("t1"), robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
			r.Validator = tc.v
			r.Recorder = record.NewFakeRecorder(16)
			reconcileAction(t, r, "t1")
			if apimeta.FindStatusCondition(getAction(t, c, "t1").Status.Conditions, "AdapterDeclined") != nil {
				t.Errorf("an AdapterDeclined condition from a reply that declined nothing")
			}
		})
	}
}

func TestAdapterDeclined_AnAdaptersWordsAreBounded(t *testing.T) {
	msg := declineMessage(&adapterDecline{Adapter: "a", Robot: "r", Reason: strings.Repeat("é", 5000)})
	if !utf8.ValidString(msg) || len(msg) > maxDeclineReason+200 || !strings.HasSuffix(msg, "…") {
		t.Errorf("a 10000-byte reason became %d bytes (valid UTF-8: %v)", len(msg), utf8.ValidString(msg))
	}
}
