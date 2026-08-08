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
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// ZoneMaintenance producers for the §9.6.5.1 chain. A maintenance window is the
// administrative record that robots were taken out of service deliberately — the entry an
// incident review reads next to the estop ones to tell "the fleet stopped" from "the fleet
// was stopped on purpose".

func zmAuditReconcile(t *testing.T, r *ZoneMaintenanceReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: zmNS, Name: name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// zmAuditActivate runs the two reconciles a window needs to reach Active: the first adds
// the resume finalizer and returns (that update re-triggers reconciliation), the second
// activates. Keeping it in one helper leaves the tests about the audit seal rather than
// about that ordering.
func zmAuditActivate(t *testing.T, r *ZoneMaintenanceReconciler, name string) {
	t.Helper()
	zmAuditReconcile(t, r, name)
	zmAuditReconcile(t, r, name)
}

func zmWindow(name, zone string, mode fleetv1.ZoneMaintenanceMode) *fleetv1.ZoneMaintenance {
	return zoneMaint(name, fleetv1.ZoneMaintenanceSpec{
		Scope:  fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: zone},
		Mode:   mode,
		Reason: "quarterly lidar recalibration",
	})
}

// ── ZONE_MAINTENANCE_ACTIVATED ─────────────────────────────────────────────────────────

func TestAudit_MaintenanceActivated_SealsOnceWithScope(t *testing.T) {
	now := time.Now()
	a := &recordingAudit{}
	zm := zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeGraceful)
	r, _ := newZMReconciler(t, &now,
		zm,
		zmRobot("amr-1", "aisle-b3", fleetv1.RobotPhaseIdle, ""),
		zmRobot("amr-2", "aisle-b3", fleetv1.RobotPhaseIdle, ""),
		zmRobot("amr-9", "aisle-c9", fleetv1.RobotPhaseIdle, ""), // different zone
	)
	r.Audit = a

	zmAuditActivate(t, r, "zm-1")
	e := mustOne(t, a, audit.EventZoneMaintenanceActivated)

	if e.Resource.Kind != "ZoneMaintenance" || e.Resource.Name != "zm-1" {
		t.Fatalf("entry must name the window, got %+v", e.Resource)
	}
	if e.Detail["zone"] != "aisle-b3" || e.Detail["mode"] == "" || e.Detail["reason"] == "" {
		t.Fatalf("zone/mode/reason are required detail fields, got %v", e.Detail)
	}
	// The resolved scope is the field that cannot be reconstructed later: zone membership
	// and robot assignment both move on after the window opens.
	scope := e.Detail["robots_in_scope"]
	if !strings.Contains(scope, "amr-1") || !strings.Contains(scope, "amr-2") {
		t.Fatalf("in-scope robots missing from the record: %q", scope)
	}
	if strings.Contains(scope, "amr-9") {
		t.Fatalf("a robot outside the zone must not be recorded in scope: %q", scope)
	}

	// RA-1: the window stays Active across reconciles; only the edge is an event.
	zmAuditReconcile(t, r, "zm-1")
	zmAuditReconcile(t, r, "zm-1")
	if n := len(a.ofType(audit.EventZoneMaintenanceActivated)); n != 1 {
		t.Fatalf("ACTIVATED must seal once per window, got %d entries", n)
	}
}

func TestAudit_MaintenanceActivated_SinkFailureDoesNotBlockActivation(t *testing.T) {
	now := time.Now()
	a := &recordingAudit{err: errors.New("sink unavailable")}
	r, c := newZMReconciler(t, &now,
		zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeGraceful),
		zmRobot("amr-1", "aisle-b3", fleetv1.RobotPhaseIdle, ""))
	r.Audit = a

	zmAuditActivate(t, r, "zm-1")
	var got fleetv1.ZoneMaintenance
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "zm-1"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("a failing audit sink blocked activation (phase %s)", got.Status.Phase)
	}
}

func TestAudit_MaintenanceNilRecorderIsSafe(t *testing.T) {
	now := time.Now()
	r, c := newZMReconciler(t, &now,
		zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeGraceful),
		zmRobot("amr-1", "aisle-b3", fleetv1.RobotPhaseIdle, ""))
	// r.Audit deliberately left nil.
	zmAuditActivate(t, r, "zm-1")
	var got fleetv1.ZoneMaintenance
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "zm-1"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		t.Fatalf("nil Audit must not change behaviour, got %s", got.Status.Phase)
	}
}

// ── ZONE_MAINTENANCE_DEACTIVATED ───────────────────────────────────────────────────────

func TestAudit_MaintenanceDeactivated_AutoResumeIsDistinguished(t *testing.T) {
	now := time.Now()
	a := &recordingAudit{}
	zm := zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeGraceful)
	zm.Spec.AutoResumeAfterMinutes = 30
	r, _ := newZMReconciler(t, &now, zm, zmRobot("amr-1", "aisle-b3", fleetv1.RobotPhaseIdle, ""))
	r.Audit = a

	zmAuditActivate(t, r, "zm-1") // activates, sets AutoResumeAt
	now = now.Add(31 * time.Minute)
	zmAuditReconcile(t, r, "zm-1") // deadline reached → Completed

	e := mustOne(t, a, audit.EventZoneMaintenanceDeactivated)
	if e.Detail["closed_by"] != "auto-resume" {
		t.Fatalf("a window that ran its course must record closed_by=auto-resume, got %q",
			e.Detail["closed_by"])
	}
	if e.Detail["zone"] != "aisle-b3" {
		t.Fatalf("zone is a required detail field, got %q", e.Detail["zone"])
	}
	// ~31 minutes of window; the exact value is clock-dependent, the presence is not.
	if e.Detail["duration_seconds"] == "" || e.Detail["duration_seconds"] == "0" {
		t.Fatalf("duration_seconds must reflect the real window, got %q", e.Detail["duration_seconds"])
	}
}

func TestAudit_MaintenanceDeactivated_DeleteIsDistinguished(t *testing.T) {
	// An operator deleting a window is a different fact from one running its course: the
	// delete path resumes UNGATED, so it can end a window early and over an estop that is
	// not Clear. A reviewer who cannot tell them apart cannot tell a completed maintenance
	// from an aborted one.
	now := time.Now()
	a := &recordingAudit{}
	zm := zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeGraceful)
	r, c := newZMReconciler(t, &now, zm, zmRobot("amr-1", "aisle-b3", fleetv1.RobotPhaseIdle, ""))
	r.Audit = a

	zmAuditActivate(t, r, "zm-1") // activate (adds the resume finalizer)

	var live fleetv1.ZoneMaintenance
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "zm-1"}, &live); err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := c.Delete(context.Background(), &live); err != nil {
		t.Fatalf("delete: %v", err)
	}
	zmAuditReconcile(t, r, "zm-1") // deletion path: resume, release finalizer, seal

	e := mustOne(t, a, audit.EventZoneMaintenanceDeactivated)
	if e.Detail["closed_by"] != "delete" {
		t.Fatalf("a deleted window must record closed_by=delete, got %q", e.Detail["closed_by"])
	}
}

func TestAudit_MaintenanceDeactivated_SealedOnceAcrossRetries(t *testing.T) {
	// The delete seal sits AFTER the finalizer update. Reconciling again finds the object
	// gone (or finalizer-free), so no second entry can be written for one closure.
	now := time.Now()
	a := &recordingAudit{}
	r, c := newZMReconciler(t, &now,
		zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeGraceful),
		zmRobot("amr-1", "aisle-b3", fleetv1.RobotPhaseIdle, ""))
	r.Audit = a

	zmAuditActivate(t, r, "zm-1")
	var live fleetv1.ZoneMaintenance
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "zm-1"}, &live)
	_ = c.Delete(context.Background(), &live)

	zmAuditReconcile(t, r, "zm-1")
	zmAuditReconcile(t, r, "zm-1")
	if n := len(a.ofType(audit.EventZoneMaintenanceDeactivated)); n != 1 {
		t.Fatalf("DEACTIVATED must seal once per closure, got %d entries", n)
	}
}

func TestAudit_MaintenanceDeactivated_NeverActivatedRecordsNoDuration(t *testing.T) {
	// A window deleted before it ever opened has no duration. Recording 0 would assert a
	// zero-length maintenance rather than "it never started", so the field is omitted.
	now := time.Now()
	a := &recordingAudit{}
	zm := zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeGraceful)
	future := metav1.NewTime(now.Add(2 * time.Hour))
	zm.Spec.ScheduledStart = &future
	r, c := newZMReconciler(t, &now, zm)
	r.Audit = a

	zmAuditActivate(t, r, "zm-1") // Scheduled, not Active — adds the finalizer
	var live fleetv1.ZoneMaintenance
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: zmNS, Name: "zm-1"}, &live)
	_ = c.Delete(context.Background(), &live)
	zmAuditReconcile(t, r, "zm-1")

	if n := len(a.ofType(audit.EventZoneMaintenanceActivated)); n != 0 {
		t.Fatalf("a window that never activated must not record ACTIVATED, got %d", n)
	}
	e := mustOne(t, a, audit.EventZoneMaintenanceDeactivated)
	if _, present := e.Detail["duration_seconds"]; present {
		t.Fatalf("a never-activated window must omit duration_seconds, got %q",
			e.Detail["duration_seconds"])
	}
}

// ── ACTION_REQUEUED_BY_MAINTENANCE ─────────────────────────────────────────────────────

func TestAudit_ActionRequeuedByMaintenance_SealsOnceNamingTheWindow(t *testing.T) {
	// Maintenance interrupting in-flight work returns the action to Pending — it does not
	// Pause it. The event is named for that, so a reviewer is not sent looking for a robot
	// that is still holding the task.
	now := time.Now()
	a := &recordingAudit{}
	zm := zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeImmediate)
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "pick-7", Namespace: zmNS},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress, AssignedRobot: "amr-1"},
	}
	r, _ := newZMReconciler(t, &now, zm,
		zmRobot("amr-1", "aisle-b3", fleetv1.RobotPhaseInProgress, "pick-7"), action)
	r.Audit = a

	zmAuditActivate(t, r, "zm-1")

	e := mustOne(t, a, audit.EventActionRequeuedByMaintenance)
	if e.Resource.Kind != "FleetAction" || e.Resource.Name != "pick-7" {
		t.Fatalf("entry must name the action, got %+v", e.Resource)
	}
	// The WINDOW's name, not its reason text: an operator needs to know which maintenance
	// took the work, and several windows can share a reason string.
	if e.Detail["maintenance_name"] != "zm-1" {
		t.Fatalf("maintenance_name must be the window name, got %q", e.Detail["maintenance_name"])
	}
	if e.Detail["action_name"] != "pick-7" {
		t.Fatalf("action_name is a required detail field, got %q", e.Detail["action_name"])
	}

	// RA-1: requeueAction returns early once the annotation is set, so the edge is spent.
	zmAuditReconcile(t, r, "zm-1")
	zmAuditReconcile(t, r, "zm-1")
	if n := len(a.ofType(audit.EventActionRequeuedByMaintenance)); n != 1 {
		t.Fatalf("must seal once per requeue, got %d entries", n)
	}
}

func TestAudit_ActionRequeuedByMaintenance_IdleRobotHasNothingToRequeue(t *testing.T) {
	now := time.Now()
	a := &recordingAudit{}
	r, _ := newZMReconciler(t, &now,
		zmWindow("zm-1", "aisle-b3", fleetv1.ZoneMaintenanceModeImmediate),
		zmRobot("amr-1", "aisle-b3", fleetv1.RobotPhaseIdle, ""))
	r.Audit = a

	zmAuditActivate(t, r, "zm-1")
	if n := len(a.ofType(audit.EventActionRequeuedByMaintenance)); n != 0 {
		t.Fatalf("an idle robot carries no action to requeue, got %d entries", n)
	}
}
