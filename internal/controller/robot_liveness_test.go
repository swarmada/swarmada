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

// Robot liveness projection (ADR-0026): presence fan-out, throttling, the
// Ready↔Offline transition, and the RA-1 guarantee that telemetry never writes it.
// (getRobot, newSink, robotWithID, i32, faNS live in the sibling *_test.go files.)

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/telemetry"
)

func livenessScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return scheme
}

// newLivenessReconciler builds a FleetAdapterReconciler over a fake client with the
// Robot spec.adapter.name index registered (so MatchingFields works) and a fixed clock.
func newLivenessReconciler(t *testing.T, now time.Time, objs ...client.Object) (*FleetAdapterReconciler, client.Client) {
	t.Helper()
	scheme := livenessScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAdapter{}, &fleetv1.Robot{}).
		WithIndex(&fleetv1.Robot{}, robotAdapterNameField, func(o client.Object) []string {
			n := o.(*fleetv1.Robot).Spec.Adapter.Name
			if n == "" {
				return nil
			}
			return []string{n}
		}).
		Build()
	return &FleetAdapterReconciler{Client: c, Scheme: scheme, now: func() time.Time { return now }}, c
}

func robotBound(name, ns, adapter string, lastSeen *metav1.Time) *fleetv1.Robot {
	r := &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.RobotSpec{Adapter: fleetv1.AdapterRef{Name: adapter}},
	}
	if lastSeen != nil {
		r.Status.Connectivity = &fleetv1.ConnectivityStatus{LastSeenAt: lastSeen}
	}
	return r
}

// AdapterConnected/AdapterHeartbeat stamp lastSeenAt on every Robot bound to the
// adapter in its namespace — and on no Robot bound to a different adapter or in
// another namespace (the mTLS-scoped boundary, ADR-0026 / ADR-0025).
func TestProjectLiveness_FanOutScopedToAdapterAndNamespace(t *testing.T) {
	now := time.Unix(10000, 0)
	fa := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: faNS},
		Status:     fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhasePending},
	}
	r, c := newLivenessReconciler(t, now, fa,
		robotBound("ra1", faNS, "acme", nil),
		robotBound("ra2", faNS, "acme", nil),
		robotBound("rb", faNS, "other-adapter", nil),
		robotBound("rx", "other-ns", "acme", nil),
	)

	r.AdapterConnected(context.Background(),
		controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}, compatibleNegotiation())

	for _, name := range []string{"ra1", "ra2"} {
		conn := getRobot(t, c, name, faNS).Status.Connectivity
		if conn == nil || conn.LastSeenAt == nil || !conn.LastSeenAt.Time.Equal(now) {
			t.Errorf("%s: lastSeenAt = %v, want %v (served robot must be stamped)", name, conn, now)
		}
	}
	if conn := getRobot(t, c, "rb", faNS).Status.Connectivity; conn != nil {
		t.Errorf("rb bound to a different adapter must not be stamped, got %+v", conn)
	}
	if conn := getRobot(t, c, "rx", "other-ns").Status.Connectivity; conn != nil {
		t.Errorf("rx in another namespace must not be stamped (cross-namespace fan-out), got %+v", conn)
	}
}

// Repeated heartbeats within one refresh interval (heartbeatTimeout/2 = 15s default)
// do not write; a stale robot and a first-seen robot do.
func TestProjectLiveness_Throttle(t *testing.T) {
	now := time.Unix(10000, 0)
	id := controlstream.TLSIdentity{AdapterName: "acme", Namespace: faNS, Verified: true}

	t.Run("skips a robot refreshed within the interval", func(t *testing.T) {
		fresh := metav1.NewTime(now.Add(-5 * time.Second)) // 5s < 15s floor
		r, c := newLivenessReconciler(t, now, robotBound("ra", faNS, "acme", &fresh))
		r.AdapterHeartbeat(context.Background(), id)
		got := getRobot(t, c, "ra", faNS).Status.Connectivity.LastSeenAt.Time
		if !got.Equal(fresh.Time) {
			t.Errorf("throttle failed: lastSeenAt moved to %v, expected no write (stays %v)", got, fresh.Time)
		}
	})

	t.Run("refreshes a robot stale past the interval", func(t *testing.T) {
		stale := metav1.NewTime(now.Add(-20 * time.Second)) // 20s > 15s floor
		r, c := newLivenessReconciler(t, now, robotBound("ra", faNS, "acme", &stale))
		r.AdapterHeartbeat(context.Background(), id)
		got := getRobot(t, c, "ra", faNS).Status.Connectivity.LastSeenAt.Time
		if !got.Equal(now) {
			t.Errorf("expected refresh to %v, got %v", now, got)
		}
	})

	t.Run("stamps a first-seen robot (nil connectivity)", func(t *testing.T) {
		r, c := newLivenessReconciler(t, now, robotBound("ra", faNS, "acme", nil))
		r.AdapterHeartbeat(context.Background(), id)
		conn := getRobot(t, c, "ra", faNS).Status.Connectivity
		if conn == nil || conn.LastSeenAt == nil || !conn.LastSeenAt.Time.Equal(now) {
			t.Errorf("first-seen robot must be stamped to %v, got %+v", now, conn)
		}
	})
}

// The consumer: fresh lastSeenAt drives Ready=True; a stale one lapses to Offline;
// a previously-Offline robot recovers on fresh liveness.
func TestRobotController_LivenessDrivesReadyAndOffline(t *testing.T) {
	scheme := livenessScheme(t)
	reconcile := func(t *testing.T, robot *fleetv1.Robot) *fleetv1.Robot {
		t.Helper()
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(robot).
			WithStatusSubresource(&fleetv1.Robot{}).Build()
		rr := &RobotReconciler{Client: c, Scheme: scheme}
		if _, err := rr.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: robot.Name, Namespace: robot.Namespace},
		}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return getRobot(t, c, robot.Name, robot.Namespace)
	}

	t.Run("fresh lastSeenAt -> Ready=True", func(t *testing.T) {
		now := metav1.Now()
		robot := robotBound("r1", faNS, "acme", &now)
		robot.Status.Phase = fleetv1.RobotPhaseDiscovered
		got := reconcile(t, robot)
		if got.Status.Phase == fleetv1.RobotPhaseOffline {
			t.Errorf("a fresh robot must not be Offline")
		}
		cond := findCondition(got, conditionTypeReady)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "AdapterLive" {
			t.Errorf("Ready = %+v, want True/AdapterLive", cond)
		}
	})

	t.Run("stale lastSeenAt -> Offline / Ready=False", func(t *testing.T) {
		old := metav1.NewTime(time.Now().Add(-time.Hour))
		robot := robotBound("r2", faNS, "acme", &old)
		robot.Status.Phase = fleetv1.RobotPhaseDiscovered
		got := reconcile(t, robot)
		if got.Status.Phase != fleetv1.RobotPhaseOffline {
			t.Errorf("phase = %s, want Offline", got.Status.Phase)
		}
		cond := findCondition(got, conditionTypeReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Errorf("Ready = %+v, want False", cond)
		}
	})

	t.Run("Offline robot recovers to Ready on fresh liveness", func(t *testing.T) {
		now := metav1.Now()
		robot := robotBound("r3", faNS, "acme", &now)
		robot.Status.Phase = fleetv1.RobotPhaseOffline
		got := reconcile(t, robot)
		// Recovery lands on the steady summary (Idle, no action here), not back on the
		// ambiguous Discovered (ADR-0029).
		if got.Status.Phase != fleetv1.RobotPhaseIdle {
			t.Errorf("recovered robot should leave Offline for Idle, got %s", got.Status.Phase)
		}
		cond := findCondition(got, conditionTypeReady)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("Ready = %+v, want True after recovery", cond)
		}
	})
}

// RA-1: the telemetry material-update path writes owned status (battery) but MUST
// NOT write connectivity/lastSeenAt — that is the presence path's job alone.
func TestTelemetry_NeverWritesLastSeenAt(t *testing.T) {
	sink, c := newSink(t, robotWithID("sim-robot-001", faNS, "rid-live"))
	if err := sink.ApplyMaterialUpdate(context.Background(), telemetry.MaterialUpdate{
		RobotID: "rid-live", BatteryPct: i32(77),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := getRobot(t, c, "sim-robot-001", faNS)
	if got.Status.BatteryPercent == nil || *got.Status.BatteryPercent != 77 {
		t.Errorf("battery = %v, want 77 (telemetry-owned status should project)", got.Status.BatteryPercent)
	}
	if got.Status.Connectivity != nil {
		t.Errorf("RA-1 violation: telemetry wrote connectivity %+v — lastSeenAt must come from presence only", got.Status.Connectivity)
	}
}
