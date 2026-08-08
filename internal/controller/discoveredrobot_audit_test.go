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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// ROBOT_REJECTED (§9.6.5.1). An operator refusing a robot admission and a TTL sweep
// reaping a stale one both end with the DiscoveredRobot gone, so the disappearance is
// not evidence of anything on its own. The rejection annotation written by
// `swarmctl admit reject` is the discriminating signal, and these tests exist mainly to
// hold the line that a sweep can never produce that entry.

// rejected marks a DiscoveredRobot the way the CLI's reject verb does.
func rejected(name, reason string) *fleetv1.DiscoveredRobot {
	dr := discovered(name, time.Hour, fleetv1.DiscoveredRobotPhaseDiscovered)
	dr.Annotations = map[string]string{annRobotRejected: reason}
	return dr
}

func TestAuditDR_RejectionSealsThenDeletes(t *testing.T) {
	now := drBase
	rec := &recordingAudit{}
	r, c := newDRReconciler(t, &now, rejected("dr-1", "unrecognised serial; not our hardware"))
	r.Audit = rec

	reconcileDR(t, r, "dr-1")

	got := rec.ofType(audit.EventRobotRejected)
	if len(got) != 1 {
		t.Fatalf("want one ROBOT_REJECTED, got %d", len(got))
	}
	e := got[0]
	if e.Resource.Kind != "DiscoveredRobot" || e.Resource.Name != "dr-1" {
		t.Fatalf("the entry must name the refused object, got %+v", e.Resource)
	}
	// Denied is the whole point: the chain records that admission was refused, not that
	// an object was removed. An Allowed entry would read as a successful admission.
	if e.Outcome != audit.OutcomeDenied {
		t.Errorf("outcome = %q, want Denied", e.Outcome)
	}
	// The reason is what a later reviewer actually needs — the object it describes is
	// deleted moments later and cannot be re-read.
	if e.Detail["reason"] != "unrecognised serial; not our hardware" {
		t.Errorf("the operator's reason must be carried through, got %q", e.Detail["reason"])
	}

	// The seal is ordered BEFORE the delete, and the delete still happens.
	if dr := getDROrNil(t, c, "dr-1"); dr != nil {
		t.Fatal("a rejected DiscoveredRobot must be removed after the entry is sealed")
	}
}

func TestAuditDR_TTLSweepSealsNothing(t *testing.T) {
	// THE LOAD-BEARING TEST. The sweep deletes exactly like a rejection does; if it also
	// sealed, ROBOT_REJECTED would degrade into "a robot went away", and the chain would
	// attribute a decision to an operator who never made one.
	now := drBase
	rec := &recordingAudit{}
	r, c := newDRReconciler(t, &now, discovered("dr-stale", time.Hour, fleetv1.DiscoveredRobotPhaseDiscovered))
	r.Audit = rec

	now = drBase.Add(2 * time.Hour) // past the TTL
	reconcileDR(t, r, "dr-stale")

	if dr := getDROrNil(t, c, "dr-stale"); dr != nil {
		t.Fatal("precondition: the sweep should have reaped the expired object")
	}
	if n := len(rec.ofType(audit.EventRobotRejected)); n != 0 {
		t.Fatalf("a TTL sweep must not masquerade as a rejection, got %d entries", n)
	}
}

func TestAuditDR_RejectionWithoutAReasonStillRecordsOne(t *testing.T) {
	// The reason field is free text from an operator and can be empty. An entry whose
	// reason is blank reads as a lost value; saying so plainly keeps the absence honest.
	now := drBase
	rec := &recordingAudit{}
	r, _ := newDRReconciler(t, &now, rejected("dr-1", ""))
	r.Audit = rec

	reconcileDR(t, r, "dr-1")
	got := rec.ofType(audit.EventRobotRejected)
	if len(got) != 1 {
		t.Fatalf("want one ROBOT_REJECTED, got %d", len(got))
	}
	if got[0].Detail["reason"] == "" {
		t.Error("an unexplained rejection must still say so, not carry an empty field")
	}
}

func TestAuditDR_SealsOncePerRejection(t *testing.T) {
	// RA-1. The delete removes the annotated object, so the edge cannot recur — but the
	// first reconcile is what must be checked, since a requeue on a delete conflict would
	// otherwise re-enter the branch.
	now := drBase
	rec := &recordingAudit{}
	r, _ := newDRReconciler(t, &now, rejected("dr-1", "wrong site"))
	r.Audit = rec

	reconcileDR(t, r, "dr-1")
	reconcileDR(t, r, "dr-1") // object gone: a no-op
	if n := len(rec.ofType(audit.EventRobotRejected)); n != 1 {
		t.Fatalf("must seal once per rejection, got %d", n)
	}
}

func TestAuditDR_SinkFailureAndNilRecorderStillDelete(t *testing.T) {
	// An operator's refusal is a safety decision. An audit sink that is down must not
	// leave a robot the operator rejected sitting admissible in the namespace.
	now := drBase
	r, c := newDRReconciler(t, &now, rejected("dr-1", "unsafe"))
	r.Audit = &recordingAudit{err: errors.New("sink unavailable")}
	reconcileDR(t, r, "dr-1")
	if dr := getDROrNil(t, c, "dr-1"); dr != nil {
		t.Fatal("a failing audit sink blocked the rejection")
	}

	now2 := drBase
	r2, c2 := newDRReconciler(t, &now2, rejected("dr-2", "unsafe"))
	// r2.Audit deliberately nil.
	reconcileDR(t, r2, "dr-2")
	if dr := getDROrNil(t, c2, "dr-2"); dr != nil {
		t.Fatal("nil Audit blocked the rejection")
	}
}
