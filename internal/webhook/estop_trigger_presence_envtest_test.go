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
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// The presence of swarmada.io/estop-triggered is the trigger, whatever its value
// (RFC-0001 #safety-estop-scopes). Written with an empty value — `kubectl annotate ...
// swarmada.io/estop-triggered=`, or a script's unset variable — the write was admitted as
// an estop-trigger and stopped nothing: every controller read an empty trigger against an
// absent processed marker as "this exact estop already issued". Here each scope's empty
// trigger goes through admission as an operator writes it, and the controllers run as the
// manager runs them.
//
// The second half is the clear. A robot that carries its own robot-scope trigger is held
// by it, and a namespace clear is not its clear: it stays stopped and gets no work until
// its own annotation is removed.

// reasonEstopper is clearingEstopper that records the reason each robot was stopped with.
type reasonEstopper struct {
	clearingEstopper
	mu      sync.Mutex
	reasons map[string]string
}

func newReasonEstopper(t *testing.T, w *safetyWire) *reasonEstopper {
	return &reasonEstopper{clearingEstopper: clearingEstopper{confirmingEstopper{c: w.as(t, controllerUser)}},
		reasons: map[string]string{}}
}

func (e *reasonEstopper) TriggerEstop(ctx context.Context, namespace, robotID, reason, by string,
	scope metrics.EstopScope, carrier safety.Carrier) (safety.Result, error) {
	e.mu.Lock()
	e.reasons[robotID] = reason
	e.mu.Unlock()
	return e.clearingEstopper.TriggerEstop(ctx, namespace, robotID, reason, by, scope, carrier)
}

func (e *reasonEstopper) reason(robot string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reasons[robot]
}

// runAllEstopControllers runs the robot-, zone- and namespace-estop controllers, as the
// manager runs them, with their watches and event recorders.
func runAllEstopControllers(t *testing.T, w *safetyWire, est controller.ZoneEstopper) {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:masters"}}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: w.scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatalf("building the controller manager: %v", err)
	}
	for name, setup := range map[string]func(ctrl.Manager) error{
		"robot estop": (&controller.RobotEstopReconciler{Client: mgr.GetClient(), Scheme: w.scheme, Estopper: est,
			Recorder: mgr.GetEventRecorderFor("robotestop")}).SetupWithManager,
		"zone estop": (&controller.ZoneEstopReconciler{Client: mgr.GetClient(), Scheme: w.scheme, Estopper: est,
			Recorder: mgr.GetEventRecorderFor("zone-estop")}).SetupWithManager,
		"namespace estop": (&controller.NamespaceEstopReconciler{Client: mgr.GetClient(), Scheme: w.scheme, Estopper: est,
			Recorder: mgr.GetEventRecorderFor("namespace-estop")}).SetupWithManager,
	} {
		if err := setup(mgr); err != nil {
			t.Fatalf("registering the %s reconciler: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(20 * time.Second):
			t.Error("the controller manager did not stop")
		}
	})
}

// setTrigger writes (val != nil) or removes the estop trigger on obj, as an admin, through
// admission.
func setTrigger(t *testing.T, w *safetyWire, obj client.Object, val *string) {
	t.Helper()
	ctx := context.Background()
	if err := w.admin.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	base := obj.DeepCopyObject().(client.Object)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	if val == nil {
		delete(ann, "swarmada.io/estop-triggered")
	} else {
		ann["swarmada.io/estop-triggered"] = *val
	}
	obj.SetAnnotations(ann)
	if err := w.admin.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		t.Fatalf("writing the estop trigger (%v) on %s: %v", val, obj.GetName(), err)
	}
}

func processedMarker(t *testing.T, w *safetyWire, obj client.Object) (string, bool) {
	t.Helper()
	if err := w.admin.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	v, ok := obj.GetAnnotations()["swarmada.io/estop-processed"]
	return v, ok
}

func ptrTo(s string) *string { return &s }

// presenceFixture is one namespace with the zone, adapter and SwarmadaConfig a stop needs,
// and two Idle robots in the zone.
func presenceFixture(t *testing.T) (*safetyWire, *reasonEstopper) {
	t.Helper()
	w := startSafetyWire(t)
	markerFixture(t, w, w.ns)
	if err := w.admin.Create(context.Background(), &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{
		Name: controller.SwarmadaConfigName, Namespace: w.ns}}); err != nil {
		t.Fatalf("creating the SwarmadaConfig: %v", err)
	}
	for _, n := range []string{"r1", "r2"} {
		if err := w.admin.Create(context.Background(), markerRobot(w.ns, n, nil)); err != nil {
			t.Fatalf("creating %s: %v", n, err)
		}
		holdSetPhase(t, w, n, fleetv1.RobotPhaseIdle, false)
	}
	est := newReasonEstopper(t, w)
	runAllEstopControllers(t, w, est)
	return w, est
}

func TestEnvtest_AnEmptyRobotTriggerStopsTheRobot(t *testing.T) {
	w, est := presenceFixture(t)
	r1 := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "r1"}}

	setTrigger(t, w, r1, ptrTo(""))
	// The stop is written before the processed marker, so both are waited for.
	eventually(t, "the empty robot trigger stopped r1 and was recorded", func() (bool, string) {
		v, ok := processedMarker(t, w, r1)
		s := estopStates(t, w, "r1", "r2")
		return ok && v == "" && s["r1"] == fleetv1.RobotEstopStopped, fmt.Sprintf("marker %q present %v %v", v, ok, s)
	})
	if got := est.reason("r1"); got != "none given" {
		t.Errorf("recorded reason = %q, want %q", got, "none given")
	}
	if got := pinnedDispatch(t, w, "for-r1", "r1"); got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to %s while its empty-valued stop is in force", got.Status.AssignedRobot)
	}
	if s := holdRobot(t, w, "r2").Status.EstopState; s != "" && s != fleetv1.RobotEstopNormal {
		t.Errorf("r2 estopState = %q: a robot-scope stop reached another robot", s)
	}

	// A second stop on the robot (a new value) is issued again; removing it clears the robot.
	setTrigger(t, w, r1, ptrTo("second"))
	eventually(t, "the second robot trigger was acted on", func() (bool, string) {
		v, _ := processedMarker(t, w, r1)
		return v == "second" && est.reason("r1") == "second", fmt.Sprintf("marker %q reason %q", v, est.reason("r1"))
	})
	setTrigger(t, w, r1, nil)
	eventually(t, "removing the robot trigger cleared r1", func() (bool, string) {
		s := holdRobot(t, w, "r1").Status.EstopState
		return s == fleetv1.RobotEstopResuming, string(s)
	})
}

func TestEnvtest_AnEmptyZoneTriggerStopsTheZone(t *testing.T) {
	w, est := presenceFixture(t)
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: markerRobotZone}}

	setTrigger(t, w, zone, ptrTo(""))
	eventually(t, "the empty zone trigger stopped the zone's robots", func() (bool, string) {
		s := estopStates(t, w, "r1", "r2")
		return allIn(s, fleetv1.RobotEstopStopped), fmt.Sprint(s)
	})
	if got := est.reason("r1"); got != "none given" {
		t.Errorf("recorded reason = %q, want %q", got, "none given")
	}
	eventually(t, "the zone reads Triggered", func() (bool, string) {
		var z fleetv1.FleetZone
		if err := w.admin.Get(context.Background(), client.ObjectKeyFromObject(zone), &z); err != nil {
			return false, err.Error()
		}
		return z.Status.EstopStatus == fleetv1.ZoneEstopTriggered, string(z.Status.EstopStatus)
	})

	// A robot created into the zone while the empty-valued stop is in force is stopped.
	if err := w.admin.Create(context.Background(), markerRobot(w.ns, "late", nil)); err != nil {
		t.Fatal(err)
	}
	holdSetPhase(t, w, "late", fleetv1.RobotPhaseIdle, false)
	eventually(t, "a robot created under the empty-valued zone stop is stopped", func() (bool, string) {
		s := estopStates(t, w, "late")
		return allIn(s, fleetv1.RobotEstopStopped), fmt.Sprint(s)
	})

	setTrigger(t, w, zone, nil)
	eventually(t, "removing the zone trigger cleared its robots", func() (bool, string) {
		s := estopStates(t, w, "r1", "r2", "late")
		return allIn(s, fleetv1.RobotEstopResuming), fmt.Sprint(s)
	})
}

func TestEnvtest_AnEmptyNamespaceTriggerStopsAndClears(t *testing.T) {
	w, est := presenceFixture(t)
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: controller.SwarmadaConfigName}}

	setTrigger(t, w, cfg, ptrTo(""))
	eventually(t, "the empty namespace trigger stopped every robot and was recorded", func() (bool, string) {
		v, ok := processedMarker(t, w, cfg)
		s := estopStates(t, w, "r1", "r2")
		return ok && v == "" && allIn(s, fleetv1.RobotEstopStopped), fmt.Sprintf("marker %q present %v %v", v, ok, s)
	})
	if got := est.reason("r2"); got != "none given" {
		t.Errorf("recorded reason = %q, want %q", got, "none given")
	}

	setTrigger(t, w, cfg, nil)
	eventually(t, "removing the namespace trigger cleared every robot", func() (bool, string) {
		_, ok := processedMarker(t, w, cfg)
		s := estopStates(t, w, "r1", "r2")
		return !ok && allIn(s, fleetv1.RobotEstopResuming), fmt.Sprintf("marker present %v %v", ok, s)
	})
}

func TestEnvtest_ANamespaceClearLeavesARobotItsOwnStop(t *testing.T) {
	w, _ := presenceFixture(t)
	r1 := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "r1"}}
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: controller.SwarmadaConfigName}}

	setTrigger(t, w, r1, ptrTo("r1-own"))
	eventually(t, "r1's own stop", func() (bool, string) {
		v, _ := processedMarker(t, w, r1)
		return v == "r1-own" && holdRobot(t, w, "r1").Status.EstopState == fleetv1.RobotEstopStopped, v
	})
	setNamespaceStop(t, w, "ns-stop")
	eventually(t, "the namespace stop", func() (bool, string) {
		m, _ := namespaceStopMarker(t, w)
		s := estopStates(t, w, "r1", "r2")
		return m == "ns-stop" && allIn(s, fleetv1.RobotEstopStopped), fmt.Sprintf("marker %q %v", m, s)
	})

	setTrigger(t, w, cfg, nil)
	eventually(t, "the namespace clear reached r2", func() (bool, string) {
		_, marked := namespaceStopMarker(t, w)
		s := estopStates(t, w, "r2")
		return !marked && allIn(s, fleetv1.RobotEstopResuming), fmt.Sprintf("marker present %v %v", marked, s)
	})
	consistently(t, "r1 stays Stopped under its own trigger", 2*time.Second, func() (bool, string) {
		s := holdRobot(t, w, "r1").Status.EstopState
		return s == fleetv1.RobotEstopStopped, string(s)
	})
	if got := pinnedDispatch(t, w, "for-r1", "r1"); got.Status.AssignedRobot != "" {
		t.Errorf("a FleetAction was assigned to r1 while its own trigger is on it")
	}
	eventually(t, "r1's event says its own stop holds it", func() (bool, string) {
		var evs corev1.EventList
		if err := w.admin.List(context.Background(), &evs, client.InNamespace(w.ns)); err != nil {
			return false, err.Error()
		}
		var seen []string
		for _, e := range evs.Items {
			if e.InvolvedObject.Name == "r1" && e.Reason == "EstopHeldByOwnTrigger" &&
				strings.Contains(e.Message, "still held by its own robot-scope estop") {
				return true, ""
			}
			seen = append(seen, e.Reason+": "+e.Message)
		}
		return false, strings.Join(seen, " | ")
	})

	// Its own clear releases it to Resuming.
	setTrigger(t, w, r1, nil)
	eventually(t, "r1's own clear", func() (bool, string) {
		s := holdRobot(t, w, "r1").Status.EstopState
		return s == fleetv1.RobotEstopResuming, string(s)
	})
}
