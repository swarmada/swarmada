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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// While any stop holds a robot — its own trigger; a zone's or the namespace's stop issued to it,
// applied or not, or recorded in status.estopHeldBy; or an estopState other than Normal,
// Resuming included — the robot carries the stop finalizer whether or not it records an action,
// and the removal of its last finalizer is refused to every identity but the control plane's
// (RFC-0001 safety.md, the robot's record of the action). The finalizer used to follow the
// robot's own trigger and its record alone, so a robot a zone stopped while it held no work
// carried none, and a delete the fail-open check admitted made it gone; created again it came
// back Normal.

// anyStopHold puts r1 under one kind of stop, as the control plane or an operator would.
type anyStopHold struct {
	name string
	put  func(t *testing.T, r *rig)
}

func anyStopHolds() []anyStopHold {
	state := func(s fleetv1.RobotEstopState) func(t *testing.T, r *rig) {
		return func(t *testing.T, r *rig) {
			rb := r.robotObj("r1")
			r.must("the control plane writes estopState "+string(s),
				r.update(r.w.as(t, controllerUser), rb, true, func() { rb.Status.EstopState = s }))
		}
	}
	recorded := func(kind fleetv1.EstopHoldKind, name string) func(t *testing.T, r *rig) {
		return func(t *testing.T, r *rig) {
			rb := r.robotObj("r1")
			r.must("the control plane records the hold", r.update(r.w.as(t, controllerUser), rb, true, func() {
				rb.Status.EstopHeldBy = append(rb.Status.EstopHeldBy,
					fleetv1.EstopHold{Kind: kind, Name: name, Since: metav1.Now()})
			}))
		}
	}
	return []anyStopHold{
		{"own-trigger", func(t *testing.T, r *rig) { r.trigger(r.robotObj("r1"), "r1-stop") }},
		{"zone-stop-issued", func(t *testing.T, r *rig) { r.trigger(r.zoneObj("z"), "z-stop") }},
		{"namespace-stop-issued", func(t *testing.T, r *rig) { r.trigger(r.configObj(), "ns-stop") }},
		{"zone-stop-recorded", recorded(fleetv1.EstopHoldFleetZone, "z")},
		{"namespace-stop-recorded", recorded(fleetv1.EstopHoldSwarmadaConfig, controller.SwarmadaConfigName)},
		{"stopping", state(fleetv1.RobotEstopStopping)},
		{"failed", state(fleetv1.RobotEstopFailed)},
		{"resuming", state(fleetv1.RobotEstopResuming)},
	}
}

// reconcileRobotEstop runs the robot estop controller once on name, as the manager would.
func (r *rig) reconcileRobotEstop(t *testing.T, name string) {
	t.Helper()
	re := &controller.RobotEstopReconciler{Client: r.w.as(t, controllerUser), Scheme: r.w.scheme,
		Estopper: &confirmingEstopper{c: r.w.as(t, controllerUser)}, APIReader: r.w.as(t, controllerUser),
		Recorder: newEvents().rec}
	_, _ = re.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: name}})
}

func anyStopFinalizerRows() []invariantRow {
	var rows []invariantRow

	// ── Any stop holds the stop finalizer on a robot that records no action ──
	// No control plane runs: the stop is laid out, and the robot estop controller reconciles r1 once.
	for _, h := range anyStopHolds() {
		h := h
		rows = append(rows, invariantRow{name: "finalizer/any-stop-holds-it/" + h.name, opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.config()
			r.robot("r1", "z")
			h.put(t, r)
			r.reconcileRobotEstop(t, "r1")
			r1 := r.get("r1")
			t.Logf("RESULT r1 under %s, recording %q: finalizers %v", h.name, r1.Status.AssignedAction, r1.Finalizers)
			if !hasStopFinalizer(r1) {
				t.Errorf("INVARIANT: r1 is under a stop (%s) and carries no stop finalizer: %v", h.name, r1.Finalizers)
			}
		}})
	}

	// A robot no stop holds carries none.
	rows = append(rows, invariantRow{name: "finalizer/any-stop-holds-it/none", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.reconcileRobotEstop(t, "r1")
		if r1 := r.get("r1"); hasStopFinalizer(r1) {
			t.Errorf("a robot no stop holds carries the stop finalizer: %v", r1.Finalizers)
		}
	}})

	// ── The last finalizer of a robot any stop holds is removed only by the control plane ──
	// The reference webhook (lastFinalizerOfAHeldRobot) pinned against the RFC's predicate.
	for _, h := range anyStopHolds() {
		h := h
		rows = append(rows, invariantRow{name: "finalizer/last-one-is-the-control-planes/" + h.name, opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			fm := r.fleetManager()
			r.zone("z", "", nil)
			r.config()
			r.robot("r1", "z")
			rb := r.robotObj("r1")
			r.must("the control plane holds the stop finalizer", r.update(r.w.as(t, controllerUser), rb, false, func() {
				controllerutil.AddFinalizer(rb, estopscope.StopFinalizer)
			}))
			h.put(t, r)
			rb = r.robotObj("r1")
			err := r.update(fm, rb, false, func() { rb.Finalizers = nil })
			after := r.get("r1")
			t.Logf("RESULT fleet-manager removes r1's last finalizer under %s: %v; finalizers now %v", h.name, err, after.Finalizers)
			if len(after.Finalizers) == 0 {
				t.Errorf("INVARIANT: a fleet-manager removed the last finalizer of r1 under %s", h.name)
			}
			rb = r.robotObj("r1")
			if err := r.update(r.w.as(t, controllerUser), rb, false, func() { rb.Finalizers = nil }); err != nil {
				t.Errorf("the control plane's removal of r1's last finalizer under %s was refused: %v", h.name, err)
			}
		}})
	}
	return rows
}
