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

// A live FAILED report that carries ACTION_END_CAUSE_ESTOP is the stop's outcome, row A1's, under a
// stop or not (RFC-0001 #safety-action-table, rows A1 and A25). The control plane may know no stop
// over the robot: an edge stop, or a local stop the adapter took itself. The report is then the only
// sign of the stop, and reading it as the robot's own end report would hand the work to onFailure
// and A23, which may run it on another robot while this one is held.

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/estopscope"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// liveFailedReport is a live FAILED report for t1 at token 5 with the given end cause.
func liveFailedReport(cause fav1.ActionEndCause) *fav1.ActionStatusUpdate {
	return &fav1.ActionStatusUpdate{ActionId: "t1", State: fav1.ActionState_ACTION_STATE_FAILED,
		FencingToken: u64(5), EndCause: cause, Message: "nav aborted"}
}

// Row 28: from Assigned and from InProgress, a live FAILED with ACTION_END_CAUSE_ESTOP on a robot
// that is Normal and under no stop is written Paused, with the Event and the audit of a stop
// outcome, and no failedAt, so neither onFailure nor A23 sees a Failed.
func TestIngest_FailedWithEstopEndCause_UnderNoStop_IsPaused(t *testing.T) {
	for _, phase := range []fleetv1.ActionPhase{fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress} {
		t.Run(string(phase), func(t *testing.T) {
			robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: actionNS},
				Status: fleetv1.RobotStatus{EstopState: fleetv1.RobotEstopNormal}}
			i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", phase, 5, nil), robot)
			rec := &eventTargets{}
			spy := &auditSpy{}
			i.Recorder, i.Audit = rec, spy

			ingest(t, i, liveFailedReport(fav1.ActionEndCause_ACTION_END_CAUSE_ESTOP))

			got := getAction(t, c, "t1")
			if got.Status.Phase != fleetv1.ActionPhasePaused {
				t.Fatalf("phase = %s, want Paused: a report that says an estop ended the action is the stop's "+
					"outcome (A1), not the robot's own end report", got.Status.Phase)
			}
			if got.Status.FailedAt != nil || got.Status.FailureReason != "" {
				t.Errorf("failedAt %v, failureReason %q: a stop outcome is not a failure", got.Status.FailedAt,
					got.Status.FailureReason)
			}
			if got.Status.AssignedRobot != "r1" {
				t.Errorf("assignedRobot = %q, want r1: A1 keeps the binding", got.Status.AssignedRobot)
			}
			if !strings.Contains(got.Status.Message, "ACTION_END_CAUSE_ESTOP") {
				t.Errorf("message %q does not name the end cause it was paused on", got.Status.Message)
			}
			if n := countEventsBy(rec, EventActionFailedUnderEstop); n != 1 {
				t.Errorf("%d %s Events, want 1 (%v)", n, EventActionFailedUnderEstop, rec.got)
			}
			if n := spy.count(audit.EventActionPausedByEstop); n != 1 {
				t.Errorf("%d %s audit entries, want 1 (%v)", n, audit.EventActionPausedByEstop, spy.typesRecorded())
			}
		})
	}
}

// Row 29: the same report on a Paused, Revoking or Preempted action leaves it in its phase and
// writes nothing (A25: "It leaves a Paused, Revoking or Preempted action in its phase").
func TestIngest_FailedWithEstopEndCause_LeavesHeldPhases(t *testing.T) {
	for _, phase := range []fleetv1.ActionPhase{fleetv1.ActionPhasePaused, fleetv1.ActionPhaseRevoking,
		fleetv1.ActionPhasePreempted} {
		t.Run(string(phase), func(t *testing.T) {
			i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", phase, 5, nil))
			rec := &eventTargets{}
			spy := &auditSpy{}
			i.Recorder, i.Audit = rec, spy
			before := getAction(t, c, "t1")

			ingest(t, i, liveFailedReport(fav1.ActionEndCause_ACTION_END_CAUSE_ESTOP))

			got := getAction(t, c, "t1")
			if got.Status.Phase != phase || got.ResourceVersion != before.ResourceVersion {
				t.Fatalf("phase %s (resourceVersion %s → %s), want %s untouched", got.Status.Phase,
					before.ResourceVersion, got.ResourceVersion, phase)
			}
			if len(rec.got) != 0 || len(spy.entries) != 0 {
				t.Errorf("Events %v, audit %v: nothing is written for a held action", rec.got, spy.typesRecorded())
			}
		})
	}
}

// Row 30, a control: a FAILED with no estop end cause, under a zone's stop the dispatcher has not
// yet written to the robot (it still reads Normal), is paused too. Green before and after the
// end-cause read; it shows that read did not narrow the stop the ingestor already sees.
func TestIngest_FailedUnderAZoneStop_RobotStillNormal_IsPaused(t *testing.T) {
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z1", Namespace: actionNS,
		Annotations: map[string]string{estopscope.TriggerAnnotation: "drill"}}}
	robot := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: actionNS},
		Spec:   fleetv1.RobotSpec{Zone: "z1"},
		Status: fleetv1.RobotStatus{EstopState: fleetv1.RobotEstopNormal}}
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil), robot, zone)

	ingest(t, i, liveFailedReport(fav1.ActionEndCause_ACTION_END_CAUSE_EXECUTION_FAILURE))

	if got := getAction(t, c, "t1"); got.Status.Phase != fleetv1.ActionPhasePaused || got.Status.FailedAt != nil {
		t.Fatalf("phase %s, failedAt %v: a failure reported under a zone's stop is the stop's outcome",
			got.Status.Phase, got.Status.FailedAt)
	}
}

// Row 31, the over-hold guard: a FAILED with EXECUTION_FAILURE under no stop is the robot's own end
// report (A19). It is written Failed, with failedAt, so onFailure and A23 apply. Reading the end
// cause must not turn every failure into a hold.
func TestIngest_FailedWithExecutionFailure_UnderNoStop_IsFailed(t *testing.T) {
	for _, cause := range []fav1.ActionEndCause{fav1.ActionEndCause_ACTION_END_CAUSE_EXECUTION_FAILURE,
		fav1.ActionEndCause_ACTION_END_CAUSE_UNSPECIFIED} {
		t.Run(cause.String(), func(t *testing.T) {
			i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil))
			ingest(t, i, liveFailedReport(cause))
			got := getAction(t, c, "t1")
			if got.Status.Phase != fleetv1.ActionPhaseFailed || got.Status.FailedAt == nil {
				t.Fatalf("phase %s, failedAt %v, want Failed with failedAt: the robot's own end report",
					got.Status.Phase, got.Status.FailedAt)
			}
			if got.Status.FailureReason != "nav aborted" {
				t.Errorf("failureReason = %q, want the report's message", got.Status.FailureReason)
			}
		})
	}
}

func countEventsBy(rec *eventTargets, reason string) int {
	n := 0
	for _, e := range rec.got {
		if e.reason == reason {
			n++
		}
	}
	return n
}
