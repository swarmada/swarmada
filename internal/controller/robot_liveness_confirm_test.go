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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// The confirming heartbeat exchange (RFC-0001 §9.6.3.2). Before this, the Offline
// transition was driven by elapsed time since lastSeenAt alone: a robot whose telemetry
// stalled while its stream was still live was declared Offline without ever being asked.
//
// That is not a cosmetic distinction. Offline is the single trigger for in-flight action
// revocation (§9.6.3.5), so a false Offline takes work away from a robot that is still
// doing it. The tests below therefore care about two things in opposite directions: that a
// robot which answers is NOT declared Offline, and that one which cannot answer still IS.

type stubProber struct {
	alive   bool
	err     error
	calls   int
	perCall []bool // optional: answer per call, overriding `alive`
}

func (s *stubProber) Heartbeat(_ context.Context, _, _ string) (bool, error) {
	s.calls++
	if len(s.perCall) > 0 {
		i := s.calls - 1
		if i < len(s.perCall) {
			return s.perCall[i], nil
		}
		return s.perCall[len(s.perCall)-1], nil
	}
	return s.alive, s.err
}

func confirmScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := fleetv1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

// staleRobot is past the offline threshold on telemetry age but has not yet been
// declared Offline — the exact edge the exchange guards.
func staleRobot(name string) *fleetv1.Robot {
	seen := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "warehouse-a"},
		Spec: fleetv1.RobotSpec{
			Zone: "aisle-b3", Manufacturer: "acme", Model: "amr-100",
			Adapter: fleetv1.AdapterRef{Name: "acme-adapter", Version: "1.0.0"},
		},
		Status: fleetv1.RobotStatus{
			Phase:        fleetv1.RobotPhaseIdle,
			Connectivity: &fleetv1.ConnectivityStatus{LastSeenAt: &seen},
		},
	}
}

func newConfirmReconciler(t *testing.T, p LivenessProber, objs ...client.Object) (*RobotReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(confirmScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}).
		Build()
	return &RobotReconciler{Client: c, Scheme: confirmScheme(t), Liveness: p}, c
}

func reconcileOnce(t *testing.T, r *RobotReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "warehouse-a", Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func phaseOf(t *testing.T, c client.Client, name string) fleetv1.RobotPhase {
	t.Helper()
	var rob fleetv1.Robot
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "warehouse-a", Name: name}, &rob); err != nil {
		t.Fatalf("get robot: %v", err)
	}
	return rob.Status.Phase
}

func TestLiveness_AnsweringRobotIsNotDeclaredOffline(t *testing.T) {
	// THE POINT OF THE CHANGE. Telemetry is stale, but the robot answers — so it is alive,
	// and declaring it Offline would revoke work it is still performing.
	p := &stubProber{alive: true}
	r, c := newConfirmReconciler(t, p, staleRobot("amr-1"))

	res := reconcileOnce(t, r, "amr-1")
	if got := phaseOf(t, c, "amr-1"); got == fleetv1.RobotPhaseOffline {
		t.Fatal("a robot that answered a heartbeat was declared Offline")
	}
	if p.calls != 1 {
		t.Fatalf("want exactly one confirmation attempt, got %d", p.calls)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("an answered confirmation must still requeue; the stall has not been explained")
	}
}

func TestLiveness_SilentRobotGoesOfflineAfterTheFullBudget(t *testing.T) {
	// The other direction: confirmation must not become a way to never declare Offline.
	p := &stubProber{alive: false}
	r, c := newConfirmReconciler(t, p, staleRobot("amr-1"))

	for i := 1; i < heartbeatConfirmAttempts; i++ {
		reconcileOnce(t, r, "amr-1")
		if got := phaseOf(t, c, "amr-1"); got == fleetv1.RobotPhaseOffline {
			t.Fatalf("declared Offline after %d attempt(s); the budget is %d", i, heartbeatConfirmAttempts)
		}
	}
	reconcileOnce(t, r, "amr-1")
	if got := phaseOf(t, c, "amr-1"); got != fleetv1.RobotPhaseOffline {
		t.Fatalf("after %d unanswered attempts the robot must be Offline, got %s",
			heartbeatConfirmAttempts, got)
	}
	if p.calls != heartbeatConfirmAttempts {
		t.Fatalf("want %d attempts, got %d", heartbeatConfirmAttempts, p.calls)
	}
}

func TestLiveness_OneLateAnswerResetsTheBudget(t *testing.T) {
	// A robot that misses two probes and then answers is alive. The counter must reset, or
	// a single flaky window would spend the budget permanently and the next miss would
	// declare Offline with no confirmation at all.
	p := &stubProber{perCall: []bool{false, false, true, false}}
	r, c := newConfirmReconciler(t, p, staleRobot("amr-1"))

	reconcileOnce(t, r, "amr-1") // miss
	reconcileOnce(t, r, "amr-1") // miss
	reconcileOnce(t, r, "amr-1") // answered → reset
	if got := phaseOf(t, c, "amr-1"); got == fleetv1.RobotPhaseOffline {
		t.Fatal("an answered probe must not leave the robot Offline")
	}
	reconcileOnce(t, r, "amr-1") // first miss of a FRESH budget
	if got := phaseOf(t, c, "amr-1"); got == fleetv1.RobotPhaseOffline {
		t.Fatal("the attempt counter did not reset after a successful answer")
	}
}

func TestLiveness_UnreachableAdapterStillDeclaresOffline(t *testing.T) {
	// No stream at all is evidence of loss, not a reason to withhold the transition. This
	// is the direction where failing "closed" would be wrong: refusing to declare Offline
	// because we cannot ask would leave a dead robot holding its assignment forever.
	p := &stubProber{alive: false, err: errors.New("no ControlStream to adapter")}
	r, c := newConfirmReconciler(t, p, staleRobot("amr-1"))

	for i := 0; i < heartbeatConfirmAttempts; i++ {
		reconcileOnce(t, r, "amr-1")
	}
	if got := phaseOf(t, c, "amr-1"); got != fleetv1.RobotPhaseOffline {
		t.Fatalf("an unreachable robot must still reach Offline, got %s", got)
	}
}

func TestLiveness_NoProberFallsBackToElapsedTime(t *testing.T) {
	// With ControlStream disabled there is no push path. The transition must fall back to
	// the pre-confirmation behaviour immediately — a control plane that cannot ask must
	// still be able to detect a dead robot.
	r, c := newConfirmReconciler(t, nil, staleRobot("amr-1"))
	reconcileOnce(t, r, "amr-1")
	if got := phaseOf(t, c, "amr-1"); got != fleetv1.RobotPhaseOffline {
		t.Fatalf("with no prober the robot must go Offline on elapsed time alone, got %s", got)
	}
}

func TestLiveness_FreshRobotIsNeverProbed(t *testing.T) {
	// The exchange belongs on the Offline edge only. Probing a robot whose telemetry is
	// current would put a push on every reconcile of every healthy robot in the fleet.
	seen := metav1.NewTime(time.Now())
	rob := staleRobot("amr-1")
	rob.Status.Connectivity.LastSeenAt = &seen
	p := &stubProber{alive: true}
	r, _ := newConfirmReconciler(t, p, rob)

	reconcileOnce(t, r, "amr-1")
	if p.calls != 0 {
		t.Fatalf("a robot with fresh telemetry must not be probed, got %d call(s)", p.calls)
	}
}

func TestLiveness_AlreadyOfflineRobotIsNotReprobed(t *testing.T) {
	// Once Offline, the decision is made. Re-probing on every requeue would push a
	// heartbeat at every disconnected robot forever.
	rob := staleRobot("amr-1")
	rob.Status.Phase = fleetv1.RobotPhaseOffline
	p := &stubProber{alive: false}
	r, _ := newConfirmReconciler(t, p, rob)

	reconcileOnce(t, r, "amr-1")
	if p.calls != 0 {
		t.Fatalf("an already-Offline robot must not be re-probed, got %d call(s)", p.calls)
	}
}
