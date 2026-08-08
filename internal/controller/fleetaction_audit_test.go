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
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// Finalizing a cancel seals a FLEETACTION_CANCELLED entry into the §9.5.4 chain,
// carrying the reason. (An unbound action finalizes immediately — no confirmed-stop
// wait — so the audit records once.)
func TestFleetAction_CancelIsAudited(t *testing.T) {
	// Unbound (AssignedRobot="") cancel-requested action → immediate finalize.
	action := cancelAction("t1", "", fleetv1.ActionPhasePending, 0, nil, "maintenance")
	rec := &captureRecorder{}
	r, c := newActionReconciler(t, action)
	r.Audit = rec

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatal("precondition: unbound cancel should finalize to Cancelled")
	}
	if len(rec.entries) != 1 {
		t.Fatalf("audit entries = %d, want exactly 1 FLEETACTION_CANCELLED", len(rec.entries))
	}
	e := rec.entries[0]
	if e.EventType != audit.EventFleetActionCancelled {
		t.Errorf("event type = %q, want %q", e.EventType, audit.EventFleetActionCancelled)
	}
	if e.Resource.Kind != "FleetAction" || e.Resource.Name != "t1" {
		t.Errorf("resource = %+v", e.Resource)
	}
	if e.Actor.Type != audit.ActorServiceAccount {
		t.Errorf("actor type = %q, want service-account", e.Actor.Type)
	}
	if e.Detail["reason"] != "maintenance" {
		t.Errorf("detail reason = %q, want maintenance", e.Detail["reason"])
	}
}

// A nil Audit recorder is safe (the cancel still finalizes).
func TestFleetAction_NilAuditRecorderIsSafe(t *testing.T) {
	action := cancelAction("t1", "", fleetv1.ActionPhasePending, 0, nil, "true")
	r, c := newActionReconciler(t, action) // r.Audit nil

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatal("cancel should finalize with a nil audit recorder")
	}
}
