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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// ACTION_PAUSED_BY_ESTOP (§9.6.5.1). An emergency stop halting in-flight work is the
// event a safety review reaches for first, and the two Paused edges leave the fleet in
// materially different states — which is why prior_phase is a required detail field and
// not decoration.

func TestAudit_ActionPausedByEstop_InProgressRecordsPriorPhase(t *testing.T) {
	a := &recordingAudit{}
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, _ := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease),
		robotEstopped("r1", fleetv1.RobotEstopStopped, "t1"),
	)
	r.Audit = a

	reconcileAction(t, r, "t1")
	e := mustOne(t, a, audit.EventActionPausedByEstop)

	if e.Resource.Kind != "FleetAction" || e.Resource.Name != "t1" {
		t.Fatalf("entry must name the action, got %+v", e.Resource)
	}
	if e.Detail["action_name"] != "t1" {
		t.Fatalf("action_name is a required detail field, got %q", e.Detail["action_name"])
	}
	// From InProgress the robot is physically committed and stays bound with its lease
	// alive; nothing else can take the task until an operator decides.
	if e.Detail["prior_phase"] != string(fleetv1.ActionPhaseInProgress) {
		t.Fatalf("prior_phase must be InProgress, got %q", e.Detail["prior_phase"])
	}
}

func TestAudit_ActionPausedByEstop_AssignedRecordsPriorPhase(t *testing.T) {
	// From Assigned the robot never started and the binding is released, so the work can
	// go elsewhere. An entry that said only "paused" would not let a reviewer tell this
	// from the InProgress case, where a robot is still holding the task.
	a := &recordingAudit{}
	r, _ := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseAssigned, 4, nil),
		robotEstopped("r1", fleetv1.RobotEstopStopped, "t1"),
	)
	r.Audit = a

	reconcileAction(t, r, "t1")
	e := mustOne(t, a, audit.EventActionPausedByEstop)
	if e.Detail["prior_phase"] != string(fleetv1.ActionPhaseAssigned) {
		t.Fatalf("prior_phase must be Assigned, got %q", e.Detail["prior_phase"])
	}
}

func TestAudit_ActionPausedByEstop_SealsOnceWhilePausedPersists(t *testing.T) {
	// RA-1. A Paused action is operator-gated and stays Paused across reconciles; only the
	// edge into Paused is an event. A per-reconcile writer would fill the chain with one
	// robot's estop and bury everything else in the incident.
	a := &recordingAudit{}
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, _ := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease),
		robotEstopped("r1", fleetv1.RobotEstopStopped, "t1"),
	)
	r.Audit = a

	reconcileAction(t, r, "t1")
	reconcileAction(t, r, "t1")
	reconcileAction(t, r, "t1")
	if n := len(a.ofType(audit.EventActionPausedByEstop)); n != 1 {
		t.Fatalf("must seal once per pause, got %d entries", n)
	}
}

func TestAudit_ActionPausedByEstop_SinkFailureDoesNotBlockThePause(t *testing.T) {
	// The safety-critical direction: an estop pause must never wait on, or be prevented
	// by, an audit sink.
	a := &recordingAudit{err: errors.New("sink unavailable")}
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease),
		robotEstopped("r1", fleetv1.RobotEstopStopped, "t1"),
	)
	r.Audit = a

	reconcileAction(t, r, "t1")
	if got := getAction(t, c, "t1").Status.Phase; got != fleetv1.ActionPhasePaused {
		t.Fatalf("a failing audit sink blocked the estop pause (phase %s)", got)
	}
}

func TestAudit_ActionPausedByEstop_NilRecorderIsSafe(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, c := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease),
		robotEstopped("r1", fleetv1.RobotEstopStopped, "t1"),
	)
	// r.Audit deliberately nil.
	reconcileAction(t, r, "t1")
	if got := getAction(t, c, "t1").Status.Phase; got != fleetv1.ActionPhasePaused {
		t.Fatalf("nil Audit must not change behaviour, got %s", got)
	}
}

func TestAudit_ActionPausedByEstop_NoEstopNoEntry(t *testing.T) {
	// The producer sits on the estop branch only; an ordinary in-flight action must not
	// generate a safety entry.
	a := &recordingAudit{}
	lease := &metav1.Time{Time: time.Now().Add(leaseDuration)}
	r, _ := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, lease),
		robotEstopped("r1", fleetv1.RobotEstopNormal, "t1"),
	)
	r.Audit = a

	reconcileAction(t, r, "t1")
	if n := len(a.ofType(audit.EventActionPausedByEstop)); n != 0 {
		t.Fatalf("no estop, so no entry; got %d", n)
	}
}
