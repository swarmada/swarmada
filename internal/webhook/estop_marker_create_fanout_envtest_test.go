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

// ─────────────────────────────────────────────────────────────────────────────
// THE CONSEQUENCE, RUN RATHER THAN READ.
//
// estop_marker_create_envtest_test.go proves the admission decision: a non-controller
// CREATE carrying swarmada.io/estop-processed is refused. What that decision is WORTH
// was, until this test, established by reading the zone controller's switch — a marker
// equal to the incoming trigger returns before the fan-out — and never by running it.
//
// So this runs it. A real API server with the shipped validating and mutating webhooks
// served, and the SHIPPED ZoneEstopReconciler reconciling against it, as the manager's
// ServiceAccount (which is also what lets the controller stamp the marker through its
// own guard). Three steps, in order:
//
//  1. A zone with NO marker takes a default-reason trigger and fans out.
//  2. A zone that DOES carry the marker at birth swallows the identical trigger —
//     nothing delivered. This is the suppression itself, and the only way such a zone
//     can exist now is for the control plane to have made it, which is how it is made
//     here.
//  3. The route an attacker would take to step 2 — creating that zone as an ordinary
//     principal — is refused.
//
// Step 2 is deliberately NOT a proof that the fix is missing: it is what step 3 buys.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// recordingEstopper stands in for the safety dispatcher. The fan-out under test is the
// reconciler's decision to CALL the per-robot primitive at all, so the primitive itself
// is recorded rather than driven over a SafetyStream — there is no adapter here to ack.
// It answers Stopped so the reconciler takes its confirmed path.
type recordingEstopper struct {
	mu       sync.Mutex
	stopped  []string
	cleared  []string
	reasons  []string
	stopCall int
}

func (e *recordingEstopper) TriggerEstop(_ context.Context, _, robotID, reason, _ string,
	_ metrics.EstopScope, carrier safety.Carrier) (safety.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopCall++
	e.stopped = append(e.stopped, robotID)
	e.reasons = append(e.reasons, reason)
	return safety.Result{Delivered: true, Confirmed: true, State: fleetv1.RobotEstopStopped,
		Latency: time.Millisecond}, nil
}

func (e *recordingEstopper) ClearEstop(_ context.Context, _, robotID, _ string, _ safety.Ends) (fleetv1.RobotEstopState, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cleared = append(e.cleared, robotID)
	return fleetv1.RobotEstopNormal, nil
}

// delivered reports the robots stopped so far, as a snapshot.
func (e *recordingEstopper) delivered() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.stopped...)
}

// startZoneEstopController runs the shipped reconciler against w's API server, as the
// manager's ServiceAccount. The identity is not decoration: the reconciler records the
// processed marker with an ordinary Patch, and that Patch goes through the very guard
// this change extends — under any other identity the controller would deny itself.
func startZoneEstopController(t *testing.T, w *safetyWire) *recordingEstopper {
	t.Helper()
	cfg := rest.CopyConfig(w.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:masters"}}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  w.scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("building the controller manager: %v", err)
	}
	estopper := &recordingEstopper{}
	if err := (&controller.ZoneEstopReconciler{
		Client:   mgr.GetClient(),
		Scheme:   w.scheme,
		Estopper: estopper,
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("registering the zone estop reconciler: %v", err)
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
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the controller cache never synced")
	}
	return estopper
}

// eventually polls cond until it holds or the deadline passes, returning the last
// description so a failure says what the state actually was.
func eventually(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		ok, desc := cond()
		if ok {
			return
		}
		last = desc
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within 15s; last observed: %s", what, last)
}

// consistently requires cond to hold for the whole window. Used for the negative:
// "no fan-out" is only meaningful if it is still true after the reconciler has had
// as long as the positive case needed.
func consistently(t *testing.T, what string, window time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if ok, desc := cond(); !ok {
			t.Fatalf("%s: %s", what, desc)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestEnvtest_EstopFanoutAndTheForgedZoneCreate(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	// The FleetZone validator authorizes the estop-trigger verb by POSTing a
	// SubjectAccessReview, and this is the first test on the wire to write the
	// estop-triggered annotation — so it is the first to need that kind in the
	// scheme. Without it the trigger below fails as an Internal error from the
	// webhook, which would look like a refusal and prove the opposite of the point.
	if err := authzv1.AddToScheme(w.scheme); err != nil {
		t.Fatalf("adding the authorization types to the scheme: %v", err)
	}
	estopper := startZoneEstopController(t, w)
	manager := w.as(t, controllerUser)
	markerFixture(t, w, w.ns)

	// One robot per zone, so which zone fanned out is legible from the robot names
	// alone. Both are admitted before any trigger exists.
	zoneOf := func(zone, robot string) {
		t.Helper()
		if err := w.admin.Create(ctx, &fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Name: zone, Namespace: w.ns},
		}); err != nil {
			t.Fatalf("creating zone %s: %v", zone, err)
		}
		r := markerRobot(w.ns, robot, nil)
		r.Spec.Zone = zone
		if err := w.admin.Create(ctx, r); err != nil {
			t.Fatalf("creating robot %s: %v", robot, err)
		}
	}

	// trigger writes the estop-triggered annotation with the value the CLI uses when
	// the operator gives no reason. "true" is also the marker value a forger would
	// pre-stamp, which is what makes the default-reason estop the one that is
	// suppressible without guessing anything.
	trigger := func(zone string) {
		t.Helper()
		cur := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: zone}, cur); err != nil {
			t.Fatalf("re-reading zone %s: %v", zone, err)
		}
		base := cur.DeepCopy()
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		cur.Annotations[estopTriggeredAnnotation] = "true"
		if err := w.admin.Patch(ctx, cur, client.MergeFrom(base)); err != nil {
			t.Fatalf("triggering the estop on %s: %v", zone, err)
		}
	}

	stoppedContains := func(robot string) (bool, string) {
		got := estopper.delivered()
		for _, r := range got {
			if r == robot {
				return true, ""
			}
		}
		return false, fmt.Sprintf("estops delivered so far: %v", got)
	}

	t.Run("ZoneWithoutTheMarkerFansOutOnTrigger", func(t *testing.T) {
		zoneOf("floor-1", "amr-floor-1")
		trigger("floor-1")

		eventually(t, "the fan-out to the robot in the triggered zone", func() (bool, string) {
			return stoppedContains("amr-floor-1")
		})
		// And the controller records what it acted on — through its own guard, as the
		// manager's ServiceAccount. A refusal here would be the guard denying the
		// control plane, which is the failure mode the identity check exists to avoid.
		eventually(t, "the controller stamping the processed marker", func() (bool, string) {
			cur := &fleetv1.FleetZone{}
			if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "floor-1"}, cur); err != nil {
				return false, err.Error()
			}
			return cur.Annotations[estopProcessedAnnotation] == "true",
				fmt.Sprintf("marker = %q, zone estop status = %q",
					cur.Annotations[estopProcessedAnnotation], cur.Status.EstopStatus)
		})
	})

	t.Run("ZoneBornEstoppedAndPreStampedStopsItsRobots", func(t *testing.T) {
		// A zone born carrying BOTH the trigger and the marker equal to it skips the
		// fan-out: the first reconcile matches "this exact estop already fanned out".
		// Only the control-plane identity can make this object now (the next subtest), so
		// the control plane makes it here. The stop is in force from the marker alone
		// (status is not read to decide it), so the stop-in-force pass reaches every robot
		// in the zone that it has not reached, and the zone reads Triggered: a robot in a
		// zone that carries a stop is stopped however the zone came to carry it.
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{
			Name: "floor-2", Namespace: w.ns,
			Annotations: map[string]string{
				estopTriggeredAnnotation: "true",
				estopProcessedAnnotation: "true",
			},
		}}
		if err := manager.Create(ctx, z); err != nil {
			t.Fatalf("creating the pre-stamped zone as the control plane: %v", err)
		}
		r := markerRobot(w.ns, "amr-floor-2", nil)
		r.Spec.Zone = "floor-2"
		if err := w.admin.Create(ctx, r); err != nil {
			t.Fatalf("creating the robot in the pre-stamped zone: %v", err)
		}
		eventually(t, "the robot in a zone born estopped and pre-stamped is stopped", func() (bool, string) {
			hit, _ := stoppedContains("amr-floor-2")
			return hit, fmt.Sprintf("delivered: %v", estopper.delivered())
		})
		eventually(t, "the pre-stamped zone reads Triggered", func() (bool, string) {
			cur := &fleetv1.FleetZone{}
			if err := w.admin.Get(ctx, client.ObjectKeyFromObject(z), cur); err != nil {
				return false, err.Error()
			}
			return cur.Status.EstopStatus == fleetv1.ZoneEstopTriggered, string(cur.Status.EstopStatus)
		})
	})

	t.Run("ClientCreateOfAPreStampedZoneIsRefused", func(t *testing.T) {
		// The route to the zone above, closed. Same object, ordinary principal.
		z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{
			Name: "floor-3", Namespace: w.ns,
			Annotations: map[string]string{
				estopTriggeredAnnotation: "true",
				estopProcessedAnnotation: "true",
			},
		}}
		err := w.as(t, "mallory").Create(ctx, z)
		if err == nil || !apierrors.IsForbidden(err) {
			t.Fatalf("a non-controller CREATE of a zone pre-stamped with %s must be Forbidden, got: %v",
				estopProcessedAnnotation, err)
		}
		namesTheMarker(t, err)
		got := &fleetv1.FleetZone{}
		if err := w.admin.Get(ctx, client.ObjectKeyFromObject(z), got); !apierrors.IsNotFound(err) {
			t.Fatalf("the refused create landed anyway: %v", err)
		}
	})
}
