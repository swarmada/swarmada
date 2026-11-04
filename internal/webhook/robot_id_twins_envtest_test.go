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
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// The robot-id check in vrobot.swarmada.io lists and then admits, so two creates carrying
// one id that arrive together can both be admitted. That is a property of any List-then-
// admit check, and no better read inside the webhook removes it. What matters is what the
// pair can do afterwards: both objects exist, and neither may be given work, whichever of
// them holds a stop. Sequential creates are still refused at admission.

// holdSetPhase writes a robot's phase (and, when stopped, estopState Stopped) as the
// control plane does.
func holdSetPhase(t *testing.T, w *safetyWire, name string, phase fleetv1.RobotPhase, stopped bool) {
	t.Helper()
	ctx := context.Background()
	m := w.as(t, controllerUser)
	var r fleetv1.Robot
	if err := m.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &r); err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	base := r.DeepCopy()
	r.Status.Phase = phase
	if stopped {
		r.Status.EstopState = fleetv1.RobotEstopStopped
	}
	if err := m.Status().Patch(ctx, &r, client.MergeFrom(base)); err != nil {
		t.Fatalf("the control plane's status write on %s was refused: %v", name, err)
	}
}

// holdDispatch creates a Pending FleetAction and runs the FleetAction reconciler on it once,
// as the manager, with an uncached client; it returns the action as stored afterwards.
func holdDispatch(t *testing.T, w *safetyWire, name string) *fleetv1.FleetAction {
	t.Helper()
	ctx := context.Background()
	if err := w.admin.Create(ctx, &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
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

func TestEnvtest_RobotIDTwinsGetNoWork(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()
	markerFixture(t, w, w.ns)
	bindShippedClusterRole(t, w, "swarmada_fleet_manager_role.yaml", map[string]interface{}{
		"kind": "User", "name": "fm", "apiGroup": "rbac.authorization.k8s.io"})
	fm := asIdentity(t, w, "fm")
	mgr := w.as(t, controllerUser)
	const n = 20

	// race creates n pairs of Robots carrying one fresh id each and returns the pairs
	// that were both admitted.
	race := func(label string, a, b client.Client, concurrent bool) [][2]string {
		var both [][2]string
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("%s-%d", label, i)
			ra := markerRobot(w.ns, id+"-a", map[string]string{fleetv1.RobotIDAnnotation: id})
			rb := markerRobot(w.ns, id+"-b", map[string]string{fleetv1.RobotIDAnnotation: id})
			var ea, eb error
			if concurrent {
				var wg sync.WaitGroup
				start := make(chan struct{})
				wg.Add(2)
				go func() { defer wg.Done(); <-start; ea = a.Create(ctx, ra) }()
				go func() { defer wg.Done(); <-start; eb = b.Create(ctx, rb) }()
				close(start)
				wg.Wait()
			} else {
				ea = a.Create(ctx, ra)
				eb = b.Create(ctx, rb)
			}
			if ea == nil && eb == nil {
				both = append(both, [2]string{ra.Name, rb.Name})
			}
		}
		return both
	}
	fmfm := race("fm-fm", fm, fm, true)
	fmmgr := race("fm-mgr", fm, mgr, true)
	seq := race("seq", fm, fm, false)
	t.Logf("race: concurrent fm-fm both-admitted=%d/%d, concurrent fm-mgr both-admitted=%d/%d, "+
		"sequential both-admitted=%d/%d", len(fmfm), n, len(fmmgr), n, len(seq), n)
	if len(seq) != 0 {
		t.Fatalf("sequential creates of one robot-id were both admitted: %v", seq)
	}

	// Dispatch over every pair the race produced: the -a robot holds a stop, its twin is
	// Idle and Normal. Every other robot stays Discovered, so the twins are the only robots
	// a dispatch could pick.
	twins := append(fmfm, fmmgr...)
	if len(twins) == 0 {
		t.Log("the race admitted no pair on this run; the dispatch half is covered by " +
			"TestDispatch_TwinsOnOneWireIDGetNoWork")
		return
	}
	isTwin := map[string]bool{}
	for _, p := range twins {
		holdSetPhase(t, w, p[0], fleetv1.RobotPhaseIdle, true)
		holdSetPhase(t, w, p[1], fleetv1.RobotPhaseIdle, false)
		isTwin[p[0]], isTwin[p[1]] = true, true
	}
	got := holdDispatch(t, w, "twin-target")
	t.Logf("dispatch over the twins: assignedRobot=%q phase=%s message=%q",
		got.Status.AssignedRobot, got.Status.Phase, got.Status.Message)
	if isTwin[got.Status.AssignedRobot] {
		t.Fatalf("FleetAction assigned to %s, one of two Robots carrying one robot-id", got.Status.AssignedRobot)
	}
	if got.Status.AssignedRobot != "" {
		t.Fatalf("FleetAction assigned to %s, which is not Idle", got.Status.AssignedRobot)
	}

	// Both twins of a pair carry RobotIDConflict, naming each other.
	rr := &controller.RobotReconciler{Client: mgr, Scheme: w.scheme}
	for _, name := range twins[0] {
		if _, err := rr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: w.ns, Name: name}}); err != nil {
			t.Fatalf("reconciling %s: %v", name, err)
		}
		var r fleetv1.Robot
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &r); err != nil {
			t.Fatal(err)
		}
		var found *metav1.Condition
		for i := range r.Status.Conditions {
			if r.Status.Conditions[i].Type == "RobotIDConflict" {
				found = &r.Status.Conditions[i]
			}
		}
		if found == nil || found.Status != metav1.ConditionTrue {
			t.Fatalf("%s: RobotIDConflict = %+v, want True", name, found)
		}
	}
}
