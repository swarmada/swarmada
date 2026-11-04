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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// The message a declarative hold writes says what the code does next for each phase: a bound
// (InProgress) action's cancel is sent once and its lease is no longer renewed to the robot; an
// Assigned action's robot keeps its record and is sent the cancel again.
func TestPausedMessageWithStop_SaysWhatTheCodeDoes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ack           bool
		disp          command.CancelDisposition
		bound         bool
		want, notWant []string
	}{
		{"bound, no answer", false, command.CancelStoppedSafely, true,
			[]string{"has not acknowledged it", "sent once and not repeated", "stops on its own lease", "until an operator requeues or cancels it"},
			[]string{"provably expired", "sent again"}},
		{"bound, rest not witnessed", true, command.CancelStopUnconfirmed, true,
			[]string{"could not witness its rest", "sent once and not repeated", "stops on its own lease"},
			[]string{"provably expired"}},
		{"unbound, no answer", false, command.CancelStoppedSafely, false,
			[]string{"keeps its record", "sent again", "lease is provably dead"},
			[]string{"sent once"}},
		{"acknowledged", true, command.CancelStoppedSafely, true,
			[]string{"acknowledged it"}, []string{"sent once", "sent again"}},
	} {
		got := pausedMessageWithStop("held", tc.ack, tc.disp, tc.bound)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q does not say %q", tc.name, got, w)
			}
		}
		for _, w := range tc.notWant {
			if strings.Contains(got, w) {
				t.Errorf("%s: %q says %q", tc.name, got, w)
			}
		}
	}
}

// A requeue of a Paused action no robot is bound to counts the unknown-horizon wait from the
// requeue, not from the action's creation: an action created an hour ago and requeued now must
// not free the robot that still records it (no acknowledgement, lease not provably dead).
func TestRequeue_CountsTheUnknownHorizonFromTheRequeue(t *testing.T) {
	action := assignedAction("t1", "", fleetv1.ActionPhasePaused, 3, nil)
	action.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	action.Annotations = map[string]string{annRequeueRequested: "admin"}
	robot := robotEstopped("r1", "", "t1")
	r, c := newActionReconciler(t, action, robot)
	reconcileAction(t, r, "t1")
	reconcileAction(t, r, "t1")
	if got := getRobot(t, c, "r1", actionNS).Status.AssignedAction; got != "t1" {
		t.Fatalf("the requeue released r1's record of an action it may still run (records %q): the wait counted from creation", got)
	}
	fa := getAction(t, c, "t1")
	if fa.Status.PendingSince == nil || time.Since(fa.Status.PendingSince.Time) > time.Minute {
		t.Errorf("the requeue did not record the re-entry into Pending: %v", fa.Status.PendingSince)
	}
	if fa.Status.AssignedRobot != "" {
		t.Errorf("t1 was given to %q while r1 may still run it", fa.Status.AssignedRobot)
	}
}
