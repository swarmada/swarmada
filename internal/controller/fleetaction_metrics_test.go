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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/metrics"
)

// assignLatency gathers the assignment-latency histogram for (namespace, priority)
// and returns its observation count and summed seconds (ToFloat64 can't read a
// histogram).
func assignLatency(t *testing.T, ns, priority string) (count uint64, sum float64) {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.SchedulerAssignmentLatencySeconds)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "swarmada_scheduler_assignment_latency_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			if l["namespace"] == ns && l["priority"] == priority {
				return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
			}
		}
	}
	return 0, 0
}

// pendingAction builds an unassigned action of the given priority.
func navPendingAction(name string, priority fleetv1.ActionPriority) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Priority: priority},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
}

// On the Assigned transition, assignment_latency is observed from the persisted
// PendingSince anchor (not scheduler-look-time), and the anchor is cleared. A
// 45s-old anchor MUST measure ~45s, proving the true entering-Pending window.
func TestFleetActionMetrics_AssignmentLatencyFromAnchor(t *testing.T) {
	action := navPendingAction("t1", fleetv1.ActionPriorityNormal)
	action.Status.PendingSince = &metav1.Time{Time: time.Now().Add(-45 * time.Second)}
	r, c := newActionReconciler(t, action, robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))

	beforeN, beforeSum := assignLatency(t, actionNS, "Normal")
	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned", ft.Status.Phase)
	}
	if ft.Status.PendingSince != nil {
		t.Error("PendingSince should be cleared on the Assigned transition")
	}
	gotN, gotSum := assignLatency(t, actionNS, "Normal")
	if gotN-beforeN != 1 {
		t.Fatalf("assignment_latency observation delta = %d, want 1", gotN-beforeN)
	}
	if d := gotSum - beforeSum; d < 43 || d > 47 {
		t.Errorf("observed latency = %.1fs, want ~45s (measured from the persisted anchor, not ~0)", d)
	}
}

// A action that cannot be scheduled (no robot) has its PendingSince anchor set AND
// persisted, so the wait accumulates across requeues rather than resetting.
func TestFleetActionMetrics_PendingSinceAnchoredAndPersisted(t *testing.T) {
	r, c := newActionReconciler(t, navPendingAction("t1", fleetv1.ActionPriorityNormal)) // no robot

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase = %s, want Pending (unschedulable)", ft.Status.Phase)
	}
	if ft.Status.PendingSince == nil {
		t.Error("PendingSince must be anchored and persisted while Pending-unscheduled")
	}
}

// The priority label reflects the action's band.
func TestFleetActionMetrics_AssignmentLatencyPriorityLabel(t *testing.T) {
	r, _ := newActionReconciler(t, navPendingAction("t1", fleetv1.ActionPriorityHigh),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""))
	before, _ := assignLatency(t, actionNS, "High")

	reconcileAction(t, r, "t1")

	if got, _ := assignLatency(t, actionNS, "High"); got-before != 1 {
		t.Errorf("assignment_latency{priority=High} delta = %d, want 1", got-before)
	}
}

// A steady-state lease renewal (actionRenew) increments the renewals counter.
func TestFleetActionMetrics_LeaseRenewalCounted(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, _ := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 3, live),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	c := metrics.SchedulerLeaseRenewalsTotal.WithLabelValues(actionNS)
	before := testutil.ToFloat64(c)

	reconcileAction(t, r, "t1")

	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Errorf("lease_renewals_total delta = %v, want 1", got)
	}
}

// A Revoking action reassigning on a provably-dead lease increments lease_expiries.
func TestFleetActionMetrics_LeaseExpiryCounted(t *testing.T) {
	expired := &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}
	action := assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 3, expired)
	action.Status.DisconnectedAt = &metav1.Time{Time: time.Now().Add(-60 * time.Second)}
	r, c := newActionReconciler(t, action,
		robotInPhase("r1", fleetv1.RobotPhaseOffline, "t1"),
		actionCancelConfig(fleetv1.ActionCancellationAfterTimeout, 30), // ceiling crossed → reassigns
	)
	expiries := metrics.SchedulerLeaseExpiriesTotal.WithLabelValues(actionNS)
	before := testutil.ToFloat64(expiries)

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhasePending {
		t.Fatal("precondition: task should have reassigned to Pending")
	}
	if got := testutil.ToFloat64(expiries) - before; got != 1 {
		t.Errorf("lease_expiries_total delta = %v, want 1", got)
	}
}

// A clean condition-2 release (robot present, confirmed NOT running T) is not a
// lease expiry and must NOT increment lease_expiries.
func TestFleetActionMetrics_CleanReleaseIsNotAnExpiry(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	// InProgress action, robot reachable but Idle & not holding t1 → robotFree → the
	// evaluateLease core returns actionReassign (cond. 2), a clean release.
	r, _ := newActionReconciler(t,
		assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 3, live),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""),
	)
	expiries := metrics.SchedulerLeaseExpiriesTotal.WithLabelValues(actionNS)
	before := testutil.ToFloat64(expiries)

	reconcileAction(t, r, "t1")

	if got := testutil.ToFloat64(expiries) - before; got != 0 {
		t.Errorf("lease_expiries_total delta = %v, want 0 (clean release is not an expiry)", got)
	}
}

// A action that misses its start deadline increments assignment_failures{DeadlineExceeded}.
func TestFleetActionMetrics_DeadlineFailureCounted(t *testing.T) {
	past := &metav1.Time{Time: time.Now().Add(-1 * time.Minute)}
	action := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Deadline: past},
		Status:     fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	r, c := newActionReconciler(t, action)
	failures := metrics.SchedulerAssignmentFailuresTotal.WithLabelValues(actionNS, metrics.FailureDeadlineExceeded)
	before := testutil.ToFloat64(failures)

	reconcileAction(t, r, "t1")

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatal("precondition: task should be Failed on missed deadline")
	}
	if got := testutil.ToFloat64(failures) - before; got != 1 {
		t.Errorf("assignment_failures{DeadlineExceeded} delta = %v, want 1", got)
	}
}
