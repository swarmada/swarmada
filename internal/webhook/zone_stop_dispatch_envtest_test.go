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

package webhook

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

// A zone estop stops the robots in the zone when it is triggered. A Robot that arrives
// afterwards, created into the zone by a fleet-manager or moved into it, was never in
// that fan-out: it went Idle and ran work while the zone read Triggered. The zone's stop
// gates dispatch by itself, and the late arrival is stopped through the per-robot primitive.

// confirmingEstopper stands in for the safety dispatcher with an adapter that confirms
// every stop: it writes estopState Stopped as the control plane does on a confirmed ack.
type confirmingEstopper struct{ c client.Client }

func (e *confirmingEstopper) TriggerEstop(ctx context.Context, namespace, robotID, _, _ string,
	scope metrics.EstopScope, carrier safety.Carrier) (safety.Result, error) {
	var r fleetv1.Robot
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robotID}, &r); err != nil {
		return safety.Result{}, err
	}
	base := r.DeepCopy()
	r.Status.EstopState = fleetv1.RobotEstopStopped
	recordLikeTheDispatcher(&r, scope, carrier)
	if err := e.c.Status().Patch(ctx, &r, client.MergeFrom(base)); err != nil {
		return safety.Result{}, err
	}
	return safety.Result{Delivered: true, Confirmed: true, State: fleetv1.RobotEstopStopped,
		Latency: time.Millisecond}, nil
}

func (e *confirmingEstopper) ClearEstop(context.Context, string, string, string, safety.Ends) (fleetv1.RobotEstopState, error) {
	return fleetv1.RobotEstopResuming, nil
}

// recordLikeTheDispatcher appends a stop to robot's status.stops, as the safety dispatcher does
// once it has sent the stop (RFC-0001 #safety-robot-stop-table, row S1): a double that recorded
// nothing would be sent the stop again by every pass that keys on the record.
func recordLikeTheDispatcher(robot *fleetv1.Robot, scope metrics.EstopScope, carrier safety.Carrier) {
	if carrier.Kind == "" {
		carrier = safety.Carrier{Kind: fleetv1.StopCarrierRobot, Name: robot.Name, UID: string(robot.UID)}
	}
	sc := fleetv1.StopScopeRobot
	switch scope {
	case metrics.ScopeZone:
		sc = fleetv1.StopScopeZone
	case metrics.ScopeNamespace:
		sc = fleetv1.StopScopeNamespace
	}
	robot.Status.Stops = append(robot.Status.Stops, fleetv1.RobotStop{ID: safety.NewStopID(), Scope: sc,
		Carrier: &fleetv1.StopCarrier{Kind: carrier.Kind, Name: carrier.Name, UID: carrier.UID}, SentAt: metav1.Now()})
}

// runZoneEstopController runs the zone-estop controller, as the manager, with its watches,
// until the returned stop is called.
func runZoneEstopController(t *testing.T, w *safetyWire) (stop func()) {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:masters"}}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: w.scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatalf("building the controller manager: %v", err)
	}
	if err := (&controller.ZoneEstopReconciler{
		Client: mgr.GetClient(), Scheme: w.scheme, Estopper: &confirmingEstopper{c: w.as(t, controllerUser)},
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("registering the zone estop reconciler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the controller manager did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

func holdRobot(t *testing.T, w *safetyWire, name string) *fleetv1.Robot {
	t.Helper()
	var r fleetv1.Robot
	if err := w.admin.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: name}, &r); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return &r
}

// A stopped zone, then a fresh Robot created into it and a Robot moved into it: a
// FleetAction issued after either ends unassigned, a robot in a Normal zone is still
// dispatched, and the late arrivals end stopped.
func TestEnvtest_NoWorkInAStoppedZone(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns) // zone bay-1, adapter acme
	bindShippedClusterRole(t, w, "swarmada_fleet_manager_role.yaml", map[string]interface{}{
		"kind": "User", "name": "fm", "apiGroup": "rbac.authorization.k8s.io"})
	fm := asIdentity(t, w, "fm")
	if err := w.admin.Create(ctx, &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Name: "bay-2", Namespace: w.ns}}); err != nil {
		t.Fatalf("creating bay-2: %v", err)
	}
	create := func(c client.Client, name, zone string) {
		t.Helper()
		r := markerRobot(w.ns, name, nil)
		r.Spec.Zone = zone
		if err := c.Create(ctx, r); err != nil {
			t.Fatalf("creating %s in %s: %v", name, zone, err)
		}
	}
	create(w.admin, "sim-robot-001", markerRobotZone)
	holdSetPhase(t, w, "sim-robot-001", fleetv1.RobotPhaseIdle, false)
	create(w.admin, "sim-robot-002", "bay-2")
	create(w.admin, "sim-robot-ctl", "bay-2")

	// The stop: an estop on bay-1, fanned out by the zone-estop controller.
	stop := runZoneEstopController(t, w)
	zone := &fleetv1.FleetZone{}
	if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: markerRobotZone}, zone); err != nil {
		t.Fatal(err)
	}
	base := zone.DeepCopy()
	zone.Annotations = map[string]string{"swarmada.io/estop-triggered": "bay-1-stop"}
	if err := w.admin.Patch(ctx, zone, client.MergeFrom(base)); err != nil {
		t.Fatalf("triggering the zone estop: %v", err)
	}
	eventually(t, "bay-1 Triggered and sim-robot-001 Stopped", func() (bool, string) {
		z := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: markerRobotZone}, z); err != nil {
			return false, err.Error()
		}
		r := holdRobot(t, w, "sim-robot-001")
		return z.Status.EstopStatus == fleetv1.ZoneEstopTriggered && r.Status.EstopState == fleetv1.RobotEstopStopped,
			fmt.Sprintf("zone %q lastEstopAt=%v annotations=%v robot %q", z.Status.EstopStatus,
				z.Status.LastEstopAt, z.Annotations, r.Status.EstopState)
	})

	// With the controller down, the dispatch hold alone must keep work out of the zone.
	stop()

	// The fresh object: created by a fleet-manager into the stopped zone with no
	// robot-id annotation, so the defaulter stamps its own name. Adapter liveness takes it
	// to Idle.
	create(fm, "sim-robot-001-c", markerRobotZone)
	holdSetPhase(t, w, "sim-robot-001-c", fleetv1.RobotPhaseIdle, false)
	got := holdDispatch(t, w, "after-fresh-create")
	t.Logf("fresh robot in the stopped zone: assignedRobot=%q message=%q", got.Status.AssignedRobot, got.Status.Message)
	if got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to %s, a robot in a zone whose estop is Triggered",
			got.Status.AssignedRobot)
	}
	if !strings.Contains(got.Status.Message, "emergency stop") {
		t.Errorf("the no-candidate message should name the estop hold; got %q", got.Status.Message)
	}

	// The zone change: a fleet-manager moves a robot from the Normal bay-2 into bay-1.
	moved := holdRobot(t, w, "sim-robot-002")
	mbase := moved.DeepCopy()
	moved.Spec.Zone = markerRobotZone
	if err := fm.Patch(ctx, moved, client.MergeFrom(mbase)); err != nil {
		t.Fatalf("moving sim-robot-002 into bay-1: %v", err)
	}
	holdSetPhase(t, w, "sim-robot-002", fleetv1.RobotPhaseIdle, false)
	got = holdDispatch(t, w, "after-zone-change")
	t.Logf("robot moved into the stopped zone: assignedRobot=%q message=%q", got.Status.AssignedRobot, got.Status.Message)
	if got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to %s after it was moved into a zone whose estop is Triggered",
			got.Status.AssignedRobot)
	}

	// The control: a robot in the Normal zone is still dispatched.
	holdSetPhase(t, w, "sim-robot-ctl", fleetv1.RobotPhaseIdle, false)
	got = holdDispatch(t, w, "in-normal-zone")
	if got.Status.AssignedRobot != "sim-robot-ctl" {
		t.Fatalf("a robot in a Normal zone was not dispatched: assignedRobot=%q message=%q",
			got.Status.AssignedRobot, got.Status.Message)
	}

	// With the controller back, both late arrivals are stopped, so the fleet shows them
	// held; the robot in the Normal zone is not.
	runZoneEstopController(t, w)
	eventually(t, "the late arrivals stopped", func() (bool, string) {
		c, m := holdRobot(t, w, "sim-robot-001-c"), holdRobot(t, w, "sim-robot-002")
		return c.Status.EstopState == fleetv1.RobotEstopStopped && m.Status.EstopState == fleetv1.RobotEstopStopped,
			fmt.Sprintf("sim-robot-001-c %q sim-robot-002 %q", c.Status.EstopState, m.Status.EstopState)
	})
	if s := holdRobot(t, w, "sim-robot-ctl").Status.EstopState; s != "" && s != fleetv1.RobotEstopNormal {
		t.Errorf("the robot in the Normal zone was stopped: estopState %q", s)
	}
}
