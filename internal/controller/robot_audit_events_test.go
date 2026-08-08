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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

// Robot-controller producers for the §9.6.5.1 safety audit log. Each seals on a real
// transition, carries the exact detail fields the table lists, and is best-effort: the
// chain records what happened, and must never be able to stop it happening.
//
// The RA-1 shape matters as much as the content — an entry per reconcile instead of per
// transition would flood the chain and make the record useless for reconstructing an
// incident, which is the only reason it exists.

type recordingAudit struct {
	entries []audit.Entry
	err     error
}

func (a *recordingAudit) Record(e audit.Entry) (audit.Entry, error) {
	if a.err != nil {
		return audit.Entry{}, a.err
	}
	a.entries = append(a.entries, e)
	return e, nil
}

func (a *recordingAudit) ofType(t string) []audit.Entry {
	var out []audit.Entry
	for _, e := range a.entries {
		if e.EventType == t {
			out = append(out, e)
		}
	}
	return out
}

func auditReconciler(t *testing.T, rec audit.Recorder, objs ...client.Object) (*RobotReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(confirmScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}).
		Build()
	// A nil prober keeps the confirming exchange out of the way: these tests are about the
	// audit seal on the transition, not about how the transition is decided.
	return &RobotReconciler{Client: c, Scheme: confirmScheme(t), Audit: rec, Liveness: nil}, c
}

func mustOne(t *testing.T, a *recordingAudit, eventType string) audit.Entry {
	t.Helper()
	got := a.ofType(eventType)
	if len(got) != 1 {
		t.Fatalf("want exactly one %s entry, got %d", eventType, len(got))
	}
	return got[0]
}

// ── ROBOT_OFFLINE ──────────────────────────────────────────────────────────────────────

func TestAudit_RobotOffline_SealsOnceWithItsDetailFields(t *testing.T) {
	a := &recordingAudit{}
	r, _ := auditReconciler(t, a, staleRobot("amr-1"))

	reconcileOnce(t, r, "amr-1")
	e := mustOne(t, a, audit.EventRobotOffline)

	if e.Resource.Kind != "Robot" || e.Resource.Name != "amr-1" {
		t.Fatalf("entry must name the Robot it concerns, got %+v", e.Resource)
	}
	for _, f := range []string{"last_seen_at", "offline_threshold_seconds"} {
		if e.Detail[f] == "" {
			t.Fatalf("required detail field %q missing or empty: %v", f, e.Detail)
		}
	}
	// RA-1: the robot is already Offline on the next pass, so the edge cannot re-fire.
	reconcileOnce(t, r, "amr-1")
	reconcileOnce(t, r, "amr-1")
	if n := len(a.ofType(audit.EventRobotOffline)); n != 1 {
		t.Fatalf("ROBOT_OFFLINE must seal once per transition, got %d entries", n)
	}
}

func TestAudit_RobotOffline_SinkFailureDoesNotBlockTheTransition(t *testing.T) {
	// The chain describes the outage; it must not be able to prevent one being declared.
	a := &recordingAudit{err: errors.New("sink unavailable")}
	r, c := auditReconciler(t, a, staleRobot("amr-1"))

	reconcileOnce(t, r, "amr-1")
	if got := phaseOf(t, c, "amr-1"); got != fleetv1.RobotPhaseOffline {
		t.Fatalf("a failing audit sink blocked the Offline transition (phase %s)", got)
	}
}

func TestAudit_NilRecorderIsSafe(t *testing.T) {
	r, c := auditReconciler(t, nil, staleRobot("amr-1"))
	reconcileOnce(t, r, "amr-1")
	if got := phaseOf(t, c, "amr-1"); got != fleetv1.RobotPhaseOffline {
		t.Fatalf("nil Audit must not change behaviour, got phase %s", got)
	}
}

// ── ROBOT_RECONNECTED ──────────────────────────────────────────────────────────────────

func TestAudit_RobotReconnected_SealsOnLeavingOffline(t *testing.T) {
	rob := staleRobot("amr-1")
	rob.Status.Phase = fleetv1.RobotPhaseOffline
	since := metav1.NewTime(time.Now().Add(-90 * time.Second))
	rob.Status.OfflineSince = &since
	rob.Status.FirmwareVersion = "3.4.5"
	fresh := metav1.Now()
	rob.Status.Connectivity.LastSeenAt = &fresh // telemetry is back

	a := &recordingAudit{}
	r, _ := auditReconciler(t, a, rob)
	reconcileOnce(t, r, "amr-1")

	e := mustOne(t, a, audit.EventRobotReconnected)
	if e.Detail["firmware_version"] != "3.4.5" {
		t.Fatalf("firmware_version must be recorded, got %q", e.Detail["firmware_version"])
	}
	if e.Detail["offline_duration_seconds"] == "" || e.Detail["offline_duration_seconds"] == "0" {
		t.Fatalf("offline_duration_seconds must reflect the real span, got %q",
			e.Detail["offline_duration_seconds"])
	}
	// OfflineSince is cleared on this edge, so a second reconcile cannot re-seal.
	reconcileOnce(t, r, "amr-1")
	if n := len(a.ofType(audit.EventRobotReconnected)); n != 1 {
		t.Fatalf("ROBOT_RECONNECTED must seal once per reconnect, got %d", n)
	}
}

// ── ROBOT_CRITICAL ─────────────────────────────────────────────────────────────────────

func TestAudit_RobotCritical_SealsOnceAndNamesStrandedActions(t *testing.T) {
	rob := staleRobot("amr-1")
	rob.Status.Phase = fleetv1.RobotPhaseOffline
	since := metav1.NewTime(time.Now().Add(-1 * time.Hour)) // well past any T2 default
	rob.Status.OfflineSince = &since

	// At T2 nothing is requeued — reassignment waits for lease expiry — so the record has
	// to say which work is stranded, or the entry cannot answer the question it exists for.
	stranded := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "pick-9", Namespace: "warehouse-a"},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseRevoking, AssignedRobot: "amr-1"},
	}
	other := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "pick-8", Namespace: "warehouse-a"},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseRevoking, AssignedRobot: "amr-2"},
	}

	a := &recordingAudit{}
	r, _ := auditReconciler(t, a, rob, stranded, other)
	reconcileOnce(t, r, "amr-1")

	e := mustOne(t, a, audit.EventRobotCritical)
	if !strings.Contains(e.Detail["revoking_actions"], "pick-9") {
		t.Fatalf("this robot's Revoking action must be listed, got %q", e.Detail["revoking_actions"])
	}
	if strings.Contains(e.Detail["revoking_actions"], "pick-8") {
		t.Fatalf("another robot's action must not be listed, got %q", e.Detail["revoking_actions"])
	}
	if e.Detail["offline_duration_seconds"] == "" {
		t.Fatal("offline_duration_seconds is a required detail field")
	}
	// The condition is already True next time round, so the escalation edge is spent.
	reconcileOnce(t, r, "amr-1")
	if n := len(a.ofType(audit.EventRobotCritical)); n != 1 {
		t.Fatalf("ROBOT_CRITICAL must seal once per escalation, got %d", n)
	}
}

// ── CAPABILITY_DEGRADED ────────────────────────────────────────────────────────────────

func TestAudit_CapabilityDegraded_FiresOnTheTransitionOnly(t *testing.T) {
	// A hardware-native capability whose component is Failed derives non-Active. Seeding a
	// prior Active entry makes this reconcile the Active → non-Active edge.
	rob := staleRobot("amr-1")
	rob.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "lidar-360", Type: "Lidar"}}
	rob.Spec.Capabilities = []fleetv1.ClassCapability{{
		Name: "navigation.2d", Type: "hardware-native", RequiredHardware: []string{"lidar-360"},
	}}
	rob.Status.Hardware = []fleetv1.HardwareComponentStatus{{
		Name: "lidar-360", Status: fleetv1.HardwareFailed, DegradationReason: "no returns",
	}}
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{{
		Name: "navigation.2d", Status: fleetv1.CapabilityStatusActive,
	}}

	a := &recordingAudit{}
	r, _ := auditReconciler(t, a, rob)
	reconcileOnce(t, r, "amr-1")

	e := mustOne(t, a, audit.EventCapabilityDegraded)
	if e.Detail["capability_name"] != "navigation.2d" {
		t.Fatalf("capability_name wrong: %q", e.Detail["capability_name"])
	}
	if e.Detail["prior_status"] != string(fleetv1.CapabilityStatusActive) {
		t.Fatalf("prior_status must be the status it left, got %q", e.Detail["prior_status"])
	}
	if e.Detail["new_status"] == "" || e.Detail["new_status"] == string(fleetv1.CapabilityStatusActive) {
		t.Fatalf("new_status must be the non-Active status it reached, got %q", e.Detail["new_status"])
	}

	// RA-1, and the reason this is a diff rather than a state check: the capability STAYS
	// non-Active across later reconciles, and a per-reconcile writer would seal on each one.
	reconcileOnce(t, r, "amr-1")
	reconcileOnce(t, r, "amr-1")
	if n := len(a.ofType(audit.EventCapabilityDegraded)); n != 1 {
		t.Fatalf("CAPABILITY_DEGRADED must seal once per transition, got %d entries", n)
	}
}

func TestAudit_CapabilityDegraded_HealthyCapabilityIsNotRecorded(t *testing.T) {
	rob := staleRobot("amr-1")
	rob.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "lidar-360", Type: "Lidar"}}
	rob.Spec.Capabilities = []fleetv1.ClassCapability{{
		Name: "navigation.2d", Type: "hardware-native", RequiredHardware: []string{"lidar-360"},
	}}
	rob.Status.Hardware = []fleetv1.HardwareComponentStatus{{
		Name: "lidar-360", Status: fleetv1.HardwareHealthy,
	}}
	rob.Status.Capabilities = []fleetv1.CapabilityStatusEntry{{
		Name: "navigation.2d", Status: fleetv1.CapabilityStatusActive,
	}}

	a := &recordingAudit{}
	r, _ := auditReconciler(t, a, rob)
	reconcileOnce(t, r, "amr-1")
	if n := len(a.ofType(audit.EventCapabilityDegraded)); n != 0 {
		t.Fatalf("a capability that stayed Active must not be recorded, got %d", n)
	}
}

func TestAudit_CapabilityDegraded_NewCapabilityIsNotADegradation(t *testing.T) {
	// A capability with no prior entry is newly declared, not degraded. Recording it would
	// put a false degradation in the chain every time an operator adds a capability.
	rob := staleRobot("amr-1")
	rob.Spec.Hardware = []fleetv1.HardwareComponent{{Name: "lidar-360", Type: "Lidar"}}
	rob.Spec.Capabilities = []fleetv1.ClassCapability{{
		Name: "navigation.2d", Type: "hardware-native", RequiredHardware: []string{"lidar-360"},
	}}
	rob.Status.Hardware = []fleetv1.HardwareComponentStatus{{
		Name: "lidar-360", Status: fleetv1.HardwareFailed,
	}}
	rob.Status.Capabilities = nil // never derived before

	a := &recordingAudit{}
	r, _ := auditReconciler(t, a, rob)
	reconcileOnce(t, r, "amr-1")
	if n := len(a.ofType(audit.EventCapabilityDegraded)); n != 0 {
		t.Fatalf("a first-time derivation is not a degradation, got %d entries", n)
	}
}
