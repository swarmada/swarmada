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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// A namespace estop stops every robot in the namespace when it is triggered. A Robot
// created in the namespace afterwards (by a fleet-manager, or admitted from discovery) was
// never in that fan-out: it read no estop and took work while the stop was in force. The
// namespace's stop gates dispatch by itself, and the late arrival is stopped through the
// per-robot primitive under the same episode. The clear and an operator's release let
// every robot go, the late one included, and the next stop holds them again.

// clearingEstopper is confirmingEstopper whose clear also writes what the safety
// dispatcher writes on an operator's clear: a stopped robot moves to Resuming.
type clearingEstopper struct{ confirmingEstopper }

func (e *clearingEstopper) ClearEstop(ctx context.Context, namespace, robotID, _ string, _ safety.Ends) (fleetv1.RobotEstopState, error) {
	var r fleetv1.Robot
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: robotID}, &r); err != nil {
		return "", err
	}
	switch r.Status.EstopState {
	case fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed, fleetv1.RobotEstopStopping:
	default:
		return r.Status.EstopState, nil
	}
	base := r.DeepCopy()
	r.Status.EstopState = fleetv1.RobotEstopResuming
	if err := e.c.Status().Patch(ctx, &r, client.MergeFrom(base)); err != nil {
		return "", err
	}
	return fleetv1.RobotEstopResuming, nil
}

// runEstopControllers runs the namespace- and zone-estop controllers, as the manager runs
// them, with their watches, until the returned stop is called.
func runEstopControllers(t *testing.T, w *safetyWire, est controller.ZoneEstopper) (stop func()) {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:masters"}}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: w.scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatalf("building the controller manager: %v", err)
	}
	if err := (&controller.NamespaceEstopReconciler{
		Client: mgr.GetClient(), Scheme: w.scheme, Estopper: est,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("registering the namespace estop reconciler: %v", err)
	}
	if err := (&controller.ZoneEstopReconciler{
		Client: mgr.GetClient(), Scheme: w.scheme, Estopper: est,
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

// setNamespaceStop sets (trigger != "") or removes (trigger == "") the namespace estop
// annotation on the SwarmadaConfig, as the estop-trigger and estop-clear verbs do.
func setNamespaceStop(t *testing.T, w *safetyWire, trigger string) {
	t.Helper()
	ctx := context.Background()
	cfg := &fleetv1.SwarmadaConfig{}
	if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: controller.SwarmadaConfigName}, cfg); err != nil {
		t.Fatal(err)
	}
	base := cfg.DeepCopy()
	if cfg.Annotations == nil {
		cfg.Annotations = map[string]string{}
	}
	if trigger == "" {
		delete(cfg.Annotations, "swarmada.io/estop-triggered")
	} else {
		cfg.Annotations["swarmada.io/estop-triggered"] = trigger
	}
	if err := w.admin.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
		t.Fatalf("writing the namespace estop annotation (%q): %v", trigger, err)
	}
}

func namespaceStopMarker(t *testing.T, w *safetyWire) (string, bool) {
	t.Helper()
	cfg := &fleetv1.SwarmadaConfig{}
	if err := w.admin.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: controller.SwarmadaConfigName}, cfg); err != nil {
		t.Fatal(err)
	}
	v, ok := cfg.Annotations["swarmada.io/estop-processed"]
	return v, ok
}

// estopStates reads the named robots' estopState.
func estopStates(t *testing.T, w *safetyWire, names ...string) map[string]fleetv1.RobotEstopState {
	t.Helper()
	out := make(map[string]fleetv1.RobotEstopState, len(names))
	for _, n := range names {
		out[n] = holdRobot(t, w, n).Status.EstopState
	}
	return out
}

func allIn(states map[string]fleetv1.RobotEstopState, want fleetv1.RobotEstopState) bool {
	for _, s := range states {
		if s != want {
			return false
		}
	}
	return true
}

// pinnedDispatch creates a Pending FleetAction pinned to robot and runs the FleetAction
// reconciler on it once, as the manager, uncached.
func pinnedDispatch(t *testing.T, w *safetyWire, name, robot string) *fleetv1.FleetAction {
	t.Helper()
	ctx := context.Background()
	if err := w.admin.Create(ctx, &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, RobotSelector: robot},
	}); err != nil {
		t.Fatalf("creating FleetAction %s: %v", name, err)
	}
	r := &controller.FleetActionReconciler{
		Client: w.as(t, controllerUser), Scheme: w.scheme, Scheduler: scheduler.NewDefaultScheduler(),
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: w.ns, Name: name}}); err != nil {
		t.Fatalf("dispatching %s: %v", name, err)
	}
	var got fleetv1.FleetAction
	if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &got); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return &got
}

func TestEnvtest_NoWorkInAStoppedNamespace(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns) // zone bay-1, adapter acme
	bindShippedClusterRole(t, w, "swarmada_fleet_manager_role.yaml", map[string]interface{}{
		"kind": "User", "name": "fm", "apiGroup": "rbac.authorization.k8s.io"})
	fm := asIdentity(t, w, "fm")
	create := func(c client.Client, name string) {
		t.Helper()
		if err := c.Create(ctx, markerRobot(w.ns, name, nil)); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		holdSetPhase(t, w, name, fleetv1.RobotPhaseIdle, false)
	}
	if err := w.admin.Create(ctx, &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{
		Name: controller.SwarmadaConfigName, Namespace: w.ns}}); err != nil {
		t.Fatalf("creating the SwarmadaConfig: %v", err)
	}
	create(w.admin, "early")
	est := &clearingEstopper{confirmingEstopper{c: w.as(t, controllerUser)}}

	// The stop: a namespace estop, fanned out by the namespace-estop controller.
	stop := runEstopControllers(t, w, est)
	setNamespaceStop(t, w, "ns-stop-1")
	eventually(t, "the fan-out stopped the robot that was there", func() (bool, string) {
		m, _ := namespaceStopMarker(t, w)
		s := holdRobot(t, w, "early").Status.EstopState
		return m == "ns-stop-1" && s == fleetv1.RobotEstopStopped, fmt.Sprintf("marker %q early %q", m, s)
	})

	// With the controller down, the dispatch hold alone must keep work off a Robot a
	// fleet-manager creates in the stopped namespace.
	stop()
	create(fm, "late-1")
	got := holdDispatch(t, w, "after-late-create")
	t.Logf("fresh robot in the stopped namespace: assignedRobot=%q phase=%s message=%q",
		got.Status.AssignedRobot, got.Status.Phase, got.Status.Message)
	if got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to %s while the namespace estop is in force", got.Status.AssignedRobot)
	}
	if !strings.Contains(got.Status.Message, "emergency stop") {
		t.Errorf("the no-candidate message should name the estop hold; got %q", got.Status.Message)
	}

	// With the controller back, the late arrival is stopped, and so is one created while
	// it runs. The robot that was there at the trigger stays stopped.
	runEstopControllers(t, w, est)
	eventually(t, "the late arrival stopped", func() (bool, string) {
		s := estopStates(t, w, "late-1", "early")
		return allIn(s, fleetv1.RobotEstopStopped), fmt.Sprint(s)
	})
	create(fm, "late-2")
	eventually(t, "a second late arrival stopped", func() (bool, string) {
		s := estopStates(t, w, "late-2")
		return allIn(s, fleetv1.RobotEstopStopped), fmt.Sprint(s)
	})
	got = holdDispatch(t, w, "after-late-stamp")
	if got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to %s while the namespace estop is in force", got.Status.AssignedRobot)
	}

	// The clear: every robot, the late ones included, goes to Resuming and the hold lifts.
	setNamespaceStop(t, w, "")
	eventually(t, "the clear reached every robot", func() (bool, string) {
		_, marked := namespaceStopMarker(t, w)
		s := estopStates(t, w, "early", "late-1", "late-2")
		return !marked && allIn(s, fleetv1.RobotEstopResuming), fmt.Sprintf("marker present %v %v", marked, s)
	})
	if got = holdDispatch(t, w, "in-resume-window"); got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to %s before an operator released it", got.Status.AssignedRobot)
	}
	// An operator's release (what Dispatcher.ConfirmResume writes): each robot is Normal,
	// stays Normal, and takes work.
	for _, n := range []string{"early", "late-1", "late-2"} {
		r := holdRobot(t, w, n)
		base := r.DeepCopy()
		r.Status.EstopState = fleetv1.RobotEstopNormal
		if err := w.as(t, controllerUser).Status().Patch(ctx, r, client.MergeFrom(base)); err != nil {
			t.Fatalf("releasing %s: %v", n, err)
		}
	}
	consistently(t, "the released robots stay Normal", 2*time.Second, func() (bool, string) {
		s := estopStates(t, w, "early", "late-1", "late-2")
		return allIn(s, fleetv1.RobotEstopNormal), fmt.Sprint(s)
	})
	for _, n := range []string{"early", "late-1", "late-2"} {
		if got = pinnedDispatch(t, w, "released-"+n, n); got.Status.AssignedRobot != n {
			t.Errorf("released robot %s was not dispatched: assignedRobot=%q message=%q",
				n, got.Status.AssignedRobot, got.Status.Message)
		}
	}

	// A second stop after the resume holds them all again, and a robot that arrives after it.
	setNamespaceStop(t, w, "ns-stop-2")
	create(fm, "late-3")
	eventually(t, "the second stop reached every robot", func() (bool, string) {
		m, _ := namespaceStopMarker(t, w)
		s := estopStates(t, w, "early", "late-1", "late-2", "late-3")
		return m == "ns-stop-2" && allIn(s, fleetv1.RobotEstopStopped), fmt.Sprintf("marker %q %v", m, s)
	})
	create(fm, "late-4")
	eventually(t, "a late arrival after the second stop stopped", func() (bool, string) {
		s := estopStates(t, w, "late-4")
		return allIn(s, fleetv1.RobotEstopStopped), fmt.Sprint(s)
	})
	if got = pinnedDispatch(t, w, "after-second-stop", "late-4"); got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to %s under the second namespace estop", got.Status.AssignedRobot)
	}
}
