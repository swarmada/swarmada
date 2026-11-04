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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controller"
)

// Rows of the estop hold invariant for the remaining ways a stop could be shed between its
// trigger and its processed marker, or after it (RFC-0001 #safety-estop-scopes,
// #safety-assignment-lease-and-the-single-executor-guarantee):
//
//   - a change of spec.estopPolicy.propagateToChildren while the stop is pending;
//   - a robot with no currentZone whose first position report lands while its declared
//     zone's stop is pending;
//   - the delete of a robot under a pending stop of its zone, an ancestor or the namespace;
//   - a pending stop over a zone tree deeper than the old walk bound;
//   - the delete of a zone that carries a stop, refused to every identity (here a fleet-manager);
//   - an Assigned action paused by a stop that was never confirmed, then requeued;
//   - a zone or SwarmadaConfig deleted, created again and stopped again with the same value.

// deafWire records every push and acknowledges no cancel, as an adapter that cannot reach the
// robot answers nothing.
type deafWire struct{ wireLog }

func (w *deafWire) PushCancelAction(ctx context.Context, ns, robot string, c command.CancelAction) (command.CancelActionOutcome, error) {
	_, _ = w.wireLog.PushCancelAction(ctx, ns, robot, c)
	return command.CancelActionOutcome{}, nil
}

// refusedUntilStopApplied requires err to refuse what, naming stop and the retry.
func refusedUntilStopApplied(t *testing.T, what, stop string, err error) bool {
	t.Helper()
	t.Logf("RESULT %s while the stop is pending: %v", what, err)
	if err == nil {
		t.Errorf("INVARIANT: %s was admitted while the stop of %s was issued and not yet applied", what, stop)
		return false
	}
	if msg := err.Error(); !strings.Contains(msg, stop) || !strings.Contains(msg, "Retry once the stop is applied") {
		t.Errorf("the refusal of %s does not name the stop of %s and the retry: %v", what, stop, err)
	}
	return true
}

// refusedWhileInForce requires err to refuse what, naming stop and that a stop is released only
// through its clear.
func refusedWhileInForce(t *testing.T, what, stop string, err error) {
	t.Helper()
	t.Logf("RESULT %s while the stop is in force: %v", what, err)
	if err == nil {
		t.Errorf("INVARIANT: %s was admitted while the stop of %s was in force", what, stop)
		return
	}
	if msg := err.Error(); !strings.Contains(msg, stop) || !strings.Contains(msg, "released only through its clear") {
		t.Errorf("the refusal of %s does not name the stop of %s and its clear: %v", what, stop, err)
	}
}

// fleetManager binds the shipped fleet-manager role to a user of the row's own and returns
// a client acting as that user.
func (r *rig) fleetManager() client.Client {
	r.t.Helper()
	user := "fm-" + r.w.ns
	bindShippedClusterRole(r.t, r.w, "swarmada_fleet_manager_role.yaml", map[string]interface{}{
		"kind": "User", "name": user, "apiGroup": "rbac.authorization.k8s.io"})
	return asIdentity(r.t, r.w, user)
}

func (r *rig) waitGone(obj client.Object) {
	r.t.Helper()
	r.waitFor(obj.GetName()+" is gone", 10*time.Second, func() (bool, string) {
		err := r.w.admin.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		return apierrors.IsNotFound(err), fmt.Sprint(err)
	})
}

func shedRows() []invariantRow {
	var rows []invariantRow

	// ── A change of propagateToChildren while a stop is pending ──
	// own: the stopped zone stops propagating; own-false-to-true: it starts to; intermediate:
	// a zone between the stopped zone and the robot's.
	for _, variant := range []string{"own", "own-false-to-true", "intermediate"} {
		variant := variant
		rows = append(rows, zoneTreeRowsOpts("pending-propagate-change/"+variant, rigOpts{deferStart: true}, func(t *testing.T, r *rig) {
			var initial *bool
			if variant == "own-false-to-true" {
				initial = boolp(false)
			}
			r.zone("anc", "", initial)
			parent, edited := "anc", "anc"
			if variant == "intermediate" {
				r.zone("b", "anc", nil)
				parent, edited = "b", "b"
			}
			r.zone("c", parent, nil)
			r.robot("rk", "c")
			r.hold("a-held", "rk", true)
			r.trigger(r.zoneObj("anc"), "anc-stop") // issued; no control plane runs to apply it yet
			to := variant == "own-false-to-true"
			change := func() error {
				z := r.zoneObj(edited)
				return r.update(r.w.admin, z, false, func() {
					z.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: boolp(to)}
				})
			}
			refusedUntilStopApplied(t, "the propagateToChildren change on "+edited, `"anc"`, change())
			r.start()
			r.waitMarker(r.zoneObj("anc"), true)
			// Once applied, anc's own flag set to false narrows its stop, which only its clear releases;
			// the other two changes narrow nothing.
			if err := change(); variant == "own" {
				refusedWhileInForce(t, "anc's propagateToChildren set to false once its stop is applied", `"anc"`, err)
			} else if err != nil {
				t.Errorf("INVARIANT: the propagateToChildren change was still refused after anc's stop was applied: %v", err)
			}
			if variant != "own-false-to-true" {
				r.assertHeld("rk", true, "a-held")
			}
		})...)
	}

	// ── A first position report from no currentZone while the declared zone's stop is pending ──
	rows = append(rows, invariantRow{name: "first-position-report-under-a-pending-stop", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.geoZone("a", 0)
			r.geoZone("b", 20)
			r.robot("rk", "a") // found by its declared zone: it has reported no position yet
			feed := r.positionFeed(nil, "a", "b")
			r.hold("a-held", "rk", true)
			r.trigger(r.zoneObj("a"), "a-stop")
			if z := feed.at("rk", 25); z == "b" {
				t.Errorf("INVARIANT: rk's first position report took it out of zone a while a's stop was pending (currentZone %q)", z)
			}
			r.start()
			r.waitMarker(r.zoneObj("a"), true)
			r.assertHeld("rk", true, "a-held")
			if z := feed.at("rk", 25); z != "b" {
				t.Errorf("control: once a's stop is applied rk's position report is still held (currentZone %q)", z)
			}
		}})

	// ── The delete of a robot under a pending stop ──
	for _, scope := range []string{"zone", "ancestor", "namespace"} {
		scope := scope
		rows = append(rows, invariantRow{name: "robot-delete-under-a-pending-stop/" + scope, opts: rigOpts{deferStart: true},
			run: func(t *testing.T, r *rig) {
				fm := r.fleetManager()
				r.zone("anc", "", nil)
				r.zone("z", "anc", nil)
				r.zone("out", "", nil)
				r.config()
				r.robot("rk", "z")
				carrier, stop := client.Object(r.zoneObj("z")), `"z"`
				switch scope {
				case "ancestor":
					carrier, stop = r.zoneObj("anc"), `"anc"`
				case "namespace":
					carrier, stop = r.configObj(), "SwarmadaConfig"
				}
				r.trigger(carrier, scope+"-stop")
				for _, who := range []struct {
					name string
					c    client.Client
				}{{"fleet-manager", fm}, {"admin", r.w.admin}} {
					if !refusedUntilStopApplied(t, "the delete of rk by the "+who.name, stop,
						who.c.Delete(context.Background(), r.robotObj("rk"))) {
						return
					}
				}
				r.start()
				r.waitMarker(carrier, true)
				r.assertHeld("rk", true)
				if err := fm.Delete(context.Background(), r.robotObj("rk")); err == nil {
					t.Errorf("INVARIANT: the delete of rk, which holds the applied stop, was admitted")
				}
			}})
	}
	rows = append(rows, invariantRow{name: "control-robot-delete-outside-a-pending-stop", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.robot("rk", "out")
			r.trigger(r.zoneObj("z"), "z-stop")
			if err := r.w.admin.Delete(context.Background(), r.robotObj("rk")); err != nil {
				t.Errorf("control: the delete of a robot no pending stop reaches was refused: %v", err)
			}
		}})

	// ── A pending stop over a tree deeper than the old bound of the pending walk ──
	rows = append(rows, invariantRow{name: "pending-stop-over-a-34-deep-tree", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.zone("z0", "", nil)
			for i := 1; i <= 34; i++ {
				r.zone(fmt.Sprintf("z%d", i), fmt.Sprintf("z%d", i-1), nil)
			}
			r.zone("other", "", nil)
			r.robot("rk", "z34")
			r.trigger(r.zoneObj("z0"), "z0-stop")
			refusedUntilStopApplied(t, "the move of rk out of z34", `"z0"`, r.tryMove("rk", "other"))
			r.start()
			r.waitMarker(r.zoneObj("z0"), true)
			r.assertHeld("rk", true)
		}})

	// ── The delete of a zone that carries a stop, refused to every identity (here a fleet-manager) ──
	for _, when := range []string{"pending", "applied"} {
		when := when
		rows = append(rows, invariantRow{name: "stopped-zone-delete-by-a-fleet-manager/" + when,
			opts: rigOpts{deferStart: when == "pending"}, run: func(t *testing.T, r *rig) {
				fm := r.fleetManager()
				r.zone("z", "", nil)
				r.zone("y", "", nil)
				r.robot("r1", "y")
				r.trigger(r.zoneObj("z"), "z-stop")
				if when == "applied" {
					r.waitMarker(r.zoneObj("z"), true)
				}
				err := fm.Delete(context.Background(), r.zoneObj("z"))
				t.Logf("RESULT the fleet-manager's delete of z (%s): %v", when, err)
				if err == nil {
					t.Errorf("INVARIANT: a fleet-manager, who may not clear the stop, deleted zone z, which carries it")
					return
				}
				if !strings.Contains(err.Error(), "estop-clear") {
					t.Errorf("the refusal does not name the clear: %v", err)
				}
				if when == "pending" {
					r.start()
					r.waitMarker(r.zoneObj("z"), true)
				}
				r.setSpecZone("r1", "z")
				r.assertHeld("r1", true)
			}})
	}
	rows = append(rows, invariantRow{name: "control-zone-delete-after-its-clear", run: func(t *testing.T, r *rig) {
		fm := r.fleetManager()
		r.zone("z", "", nil)
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitMarker(r.zoneObj("z"), true)
		r.clearStop(r.zoneObj("z"))
		r.waitMarker(r.zoneObj("z"), false)
		if err := fm.Delete(context.Background(), r.zoneObj("z")); err != nil {
			t.Errorf("control: the delete of a zone whose stop was cleared was refused: %v", err)
		}
	}})

	// ── An Assigned action paused by a stop that was never confirmed ──
	// The robot was sent the assignment, its stop was never delivered (no answer: Failed), and
	// a requeue follows, or none. No other robot may be given the action until the first robot's
	// lease is provably dead; then the requeue runs, or, with none, the work stays with the robot
	// and waits for an operator (invariant I1). confirmed is the control: a confirmed stop pauses x
	// and keeps the binding (row A1): a confirmed stop is not proof the robot is off the action.
	for _, variant := range []string{"robot", "zone", "no-requeue", "confirmed"} {
		variant := variant
		rows = append(rows, invariantRow{name: "assigned-under-an-unconfirmed-stop/" + variant, run: func(t *testing.T, r *rig) {
			dw := &deafWire{}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.shortLease()
			r.robot("r1", "z")
			r.must("creating x", r.w.admin.Create(context.Background(), &fleetv1.FleetAction{
				ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "x"},
				Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
			}))
			fa := r.action("x")
			for i := 0; i < 20 && (fa.Status.Phase == "" || fa.Status.Phase == fleetv1.ActionPhasePending); i++ {
				fa = r.workWith(dw, "x")
				time.Sleep(100 * time.Millisecond)
			}
			if fa.Status.Phase != fleetv1.ActionPhaseAssigned || fa.Status.AssignedRobot != "r1" {
				t.Fatalf("setup: x was not assigned to r1: phase=%q assignedRobot=%q message=%q",
					fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message)
			}
			r.robot("r2", "out")
			if variant != "confirmed" {
				r.setMode("r1", ackNone)
			}
			if variant == "zone" {
				r.trigger(r.zoneObj("z"), "z-stop")
			} else {
				r.trigger(r.robotObj("r1"), "own")
			}
			r.waitStopped("r1")
			fa = r.workWith(dw, "x")
			t.Logf("RESULT x after r1's %s stop: phase=%q assignedRobot=%q leaseExpiresAt=%v message=%q",
				r.get("r1").Status.EstopState, fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.LeaseExpiresAt, fa.Status.Message)
			if variant == "confirmed" {
				if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" {
					t.Errorf("control: a confirmed stop left x %q bound to %q, not Paused and still bound to r1 (A1)",
						fa.Status.Phase, fa.Status.AssignedRobot)
				}
				return
			}
			if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" || fa.Status.LeaseExpiresAt == nil {
				t.Errorf("INVARIANT: r1's stop is not confirmed and x is %q bound to %q (lease %v), not Paused and still bound to r1",
					fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.LeaseExpiresAt)
				if fa.Status.LeaseExpiresAt == nil {
					return
				}
			}
			dead := fa.Status.LeaseExpiresAt.Add(suSkew)
			if variant != "no-requeue" {
				r.setAnnotation(&fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}},
					"swarmada.io/requeue-requested", strp("operator"))
			}
			for {
				start := time.Now()
				fa = r.workWith(dw, "x")
				end := time.Now()
				if n := dw.count("assign", "r2"); n > 0 {
					if variant == "no-requeue" {
						t.Errorf("INVARIANT: with no requeue x was given to r2 under r1's unconfirmed stop (wire: %s)", dw)
						break
					}
					if end.Before(dead) {
						t.Errorf("INVARIANT: x was assigned to r2 %.1fs before r1's lease was provably dead (wire: %s)",
							dead.Sub(end).Seconds(), dw)
					}
					t.Logf("RESULT x assigned to r2 in a pass ending %.1fs after r1's lease was provably dead: wire: %s",
						end.Sub(dead).Seconds(), dw)
					break
				}
				if fa.Status.AssignedRobot != "r1" && end.Before(dead) {
					t.Errorf("INVARIANT: x left r1 %.1fs before its lease was provably dead (phase %q)",
						dead.Sub(end).Seconds(), fa.Status.Phase)
				}
				if variant == "no-requeue" && start.After(dead.Add(3*time.Second)) {
					if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" ||
						r.get("r1").Status.AssignedAction != "x" {
						t.Errorf("INVARIANT: with no requeue x is %q bound to %q after the lease (r1 records %q), not kept Paused with r1",
							fa.Status.Phase, fa.Status.AssignedRobot, r.get("r1").Status.AssignedAction)
					} else if !strings.HasPrefix(fa.Status.Message, "awaiting operator decision") {
						t.Errorf("with no requeue x does not say it awaits an operator after the lease: %q", fa.Status.Message)
					} else {
						t.Logf("RESULT x kept with r1 past its lease, awaiting an operator: %q", fa.Status.Message)
					}
					break
				}
				if start.After(dead.Add(5 * time.Second)) {
					t.Errorf("INVARIANT: 5s after r1's lease was provably dead x is %q bound to %q (wire: %s)",
						fa.Status.Phase, fa.Status.AssignedRobot, dw)
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
		}})
	}

	// ── A zone or SwarmadaConfig deleted, created again and stopped again with the same value ──
	// The old episode is not inherited: a robot the old episode reached, released since, and
	// under the new one is stopped again, and sent the stop.
	rows = append(rows, invariantRow{name: "carrier-recreated-and-restopped-with-the-same-value/zone", run: func(t *testing.T, r *rig) {
		r.zone("z", "", nil)
		r.zone("y", "", nil)
		r.robot("r1", "z")
		r.trigger(r.zoneObj("z"), "same")
		r.waitStopped("r1")
		r.waitMarker(r.zoneObj("z"), true)
		// A zone that carries a stop is deleted only after its clear.
		r.clearStop(r.zoneObj("z"))
		r.waitMarker(r.zoneObj("z"), false)
		r.setSpecZone("r1", "y")
		r.must("deleting the cleared z", r.w.admin.Delete(context.Background(), r.zoneObj("z")))
		r.waitGone(r.zoneObj("z"))
		t.Logf("RESULT resume after the old episode: %s", r.resume("r1", "rel"))
		if s := r.get("r1").Status.EstopState; s != fleetv1.RobotEstopNormal {
			t.Fatalf("setup: r1 is %q after its release, not Normal", s)
		}
		sends := r.sendsTo("r1")
		z := r.zoneObj("z")
		z.Annotations = map[string]string{estopTriggeredAnnotation: "same"}
		r.must("recreating z, stopped with the same value", r.w.admin.Create(context.Background(), z))
		r.waitMarker(r.zoneObj("z"), true)
		r.setSpecZone("r1", "z")
		r.assertHeld("r1", true)
		if n := r.sendsTo("r1"); n <= sends {
			t.Errorf("INVARIANT: the recreated zone's stop, with the old value, was not sent to r1 (sends %d, before %d)", n, sends)
		}
	}})
	rows = append(rows, invariantRow{name: "carrier-recreated-and-restopped-with-the-same-value/namespace", run: func(t *testing.T, r *rig) {
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.trigger(r.configObj(), "same")
		r.waitStopped("r1")
		r.waitMarker(r.configObj(), true)
		if err := r.w.admin.Delete(context.Background(), r.configObj()); err == nil {
			t.Errorf("INVARIANT: the SwarmadaConfig carrying the namespace's stop was deleted")
			r.waitGone(r.configObj())
		} else {
			r.clearStop(r.configObj())
			r.waitMarker(r.configObj(), false)
			t.Logf("RESULT resume after the old episode: %s", r.resume("r1", "rel"))
			r.must("deleting the cleared SwarmadaConfig", r.w.admin.Delete(context.Background(), r.configObj()))
			r.waitGone(r.configObj())
		}
		if s := r.get("r1").Status.EstopState; s != fleetv1.RobotEstopNormal {
			r.ownRelease("r1")
			t.Logf("RESULT resume after the old episode: %s", r.resume("r1", "rel-2"))
		}
		if s := r.get("r1").Status.EstopState; s != fleetv1.RobotEstopNormal {
			t.Fatalf("setup: r1 is %q after its release, not Normal", s)
		}
		sends := r.sendsTo("r1")
		cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: controller.SwarmadaConfigName, Namespace: r.w.ns,
			Annotations: map[string]string{estopTriggeredAnnotation: "same"}}}
		r.must("recreating the SwarmadaConfig, stopped with the same value", r.w.admin.Create(context.Background(), cfg))
		r.waitMarker(r.configObj(), true)
		r.assertHeld("r1", true)
		if n := r.sendsTo("r1"); n <= sends {
			t.Errorf("INVARIANT: the recreated namespace stop, with the old value, was not sent to r1 (sends %d, before %d)", n, sends)
		}
	}})
	// A carrier created again from a copy of the stopped one carries the old episode's processed
	// marker with its trigger: read as already fanned out, it would stop no one. The create is
	// refused to every identity but the control plane, and the same trigger without the marker
	// stops the robot again.
	for _, scope := range []string{"zone", "namespace"} {
		scope := scope
		rows = append(rows, invariantRow{name: "carrier-recreated-from-its-saved-copy/" + scope, run: func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.zone("y", "", nil)
			if scope == "namespace" {
				r.config()
			}
			r.robot("r1", "z")
			carrier := func() client.Object { return r.zoneObj("z") }
			if scope == "namespace" {
				carrier = func() client.Object { return r.configObj() }
			}
			r.trigger(carrier(), "same")
			r.waitStopped("r1")
			r.waitMarker(carrier(), true) // the move comes after the stop is applied
			// A carrier that carries a stop is deleted only after its clear.
			r.clearStop(carrier())
			r.waitMarker(carrier(), false)
			if scope == "zone" {
				r.setSpecZone("r1", "y")
			}
			r.must("deleting the cleared carrier", r.w.admin.Delete(context.Background(), carrier()))
			r.waitGone(carrier())
			t.Logf("RESULT resume after the old episode: %s", r.resume("r1", "rel"))
			if s := r.get("r1").Status.EstopState; s != fleetv1.RobotEstopNormal {
				t.Fatalf("setup: r1 is %q after its release, not Normal", s)
			}
			saved := map[string]string{estopTriggeredAnnotation: "same", estopProcessedAnnotation: "same"}
			recreate := func(ann map[string]string) error {
				if scope == "zone" {
					return r.w.admin.Create(context.Background(), &fleetv1.FleetZone{
						ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: r.w.ns, Annotations: ann}})
				}
				return r.w.admin.Create(context.Background(), &fleetv1.SwarmadaConfig{
					ObjectMeta: metav1.ObjectMeta{Name: controller.SwarmadaConfigName, Namespace: r.w.ns, Annotations: ann}})
			}
			err := recreate(saved)
			t.Logf("RESULT the %s created again carrying the old episode's marker: %v", scope, err)
			if err == nil {
				t.Errorf("INVARIANT: a %s carrying the old episode's processed marker was created", scope)
				time.Sleep(time.Second)
				if scope == "zone" {
					r.setSpecZone("r1", "z")
				}
				r.assertHeld("r1", true)
				return
			}
			sends := r.sendsTo("r1")
			r.must("creating it again with the trigger alone", recreate(map[string]string{estopTriggeredAnnotation: "same"}))
			r.waitMarker(carrier(), true)
			if scope == "zone" {
				r.setSpecZone("r1", "z")
			}
			r.assertHeld("r1", true)
			if n := r.sendsTo("r1"); n <= sends {
				t.Errorf("INVARIANT: the stop with the old value was not sent to r1 again (sends %d, before %d)", n, sends)
			}
		}})
	}
	// ── The last finalizer of a deleted robot removed while a stop is pending ──
	// The robot was deleted before the stop (admitted: nothing held it) and is kept by a
	// finalizer; removing it completes the delete, the route around the delete refusal.
	rows = append(rows, invariantRow{name: "finalizer-removed-under-a-pending-stop", opts: rigOpts{deferStart: true},
		run: func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.robot("rk", "z")
			rb := r.robotObj("rk")
			r.must("adding a finalizer", r.update(r.w.admin, rb, false, func() {
				rb.Finalizers = append(rb.Finalizers, "example.com/keep")
			}))
			r.must("deleting rk before any stop", r.w.admin.Delete(context.Background(), r.robotObj("rk")))
			r.trigger(r.zoneObj("z"), "z-stop")
			rb = r.robotObj("rk")
			err := r.update(r.w.admin, rb, false, func() { rb.Finalizers = nil })
			if !refusedUntilStopApplied(t, "the removal of rk's last finalizer", `"z"`, err) {
				return
			}
			r.start()
			r.waitMarker(r.zoneObj("z"), true)
			r.assertHeld("rk", true)
		}})

	// ── A pending stop on the farthest ancestor the fan-out reaches ──
	// z64 has 64 ancestors, which admission admits and the fan-out walks; the control has no stop.
	for _, stopped := range []bool{true, false} {
		stopped := stopped
		name := "pending-stop-on-the-64th-ancestor"
		if !stopped {
			name = "control-move-in-a-64-ancestor-tree-with-no-stop"
		}
		rows = append(rows, invariantRow{name: name, opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			r.zone("z0", "", nil)
			for i := 1; i <= 64; i++ {
				r.zone(fmt.Sprintf("z%d", i), fmt.Sprintf("z%d", i-1), nil)
			}
			r.zone("other", "", nil)
			r.robot("rk", "z64")
			if !stopped {
				if err := r.tryMove("rk", "other"); err != nil {
					t.Errorf("control: a move with no stop anywhere was refused: %v", err)
				}
				return
			}
			r.trigger(r.zoneObj("z0"), "z0-stop")
			refusedUntilStopApplied(t, "the move of rk out of z64", `"z0"`, r.tryMove("rk", "other"))
			r.start()
			r.waitMarker(r.zoneObj("z0"), true)
			r.assertHeld("rk", true)
		}})
	}

	// ── A deleted FleetTask member created again under its name ──
	// The member's robot was sent it and may still be running it: deleting the FleetAction sends
	// the robot nothing. The task creates the member again; no other robot may be given it
	// until the first one's lease is provably dead.
	rows = append(rows, invariantRow{name: "deleted-task-member-regenerated", run: func(t *testing.T, r *rig) {
		dw := &deafWire{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		r.shortLease()
		r.robot("r1", "z")
		tr := &controller.FleetTaskReconciler{Client: r.w.as(t, controllerUser), Scheme: r.w.scheme}
		r.must("creating task t", r.w.admin.Create(context.Background(), &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "t"},
			Spec: fleetv1.FleetTaskSpec{Actions: []fleetv1.FleetTaskAction{
				{Name: "m", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}}}},
		}))
		reconcileTask := func() {
			_, _ = tr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: "t"}})
		}
		var fa *fleetv1.FleetAction
		for i := 0; i < 20; i++ {
			reconcileTask()
			var got fleetv1.FleetAction
			if r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: "t-m"}, &got) == nil {
				if fa = r.workWith(dw, "t-m"); fa.Status.Phase == fleetv1.ActionPhaseAssigned {
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if fa == nil || fa.Status.AssignedRobot != "r1" {
			t.Fatalf("setup: the member was not assigned to r1: %+v", fa)
		}
		r.robot("r2", "out")
		dead := fa.Status.LeaseExpiresAt.Add(suSkew)
		r.must("deleting the member", r.w.admin.Delete(context.Background(), r.action("t-m")))
		r.waitGone(&fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "t-m"}})
		for i := 0; i < 50; i++ {
			reconcileTask()
			var got fleetv1.FleetAction
			if r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: "t-m"}, &got) == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("RESULT the task created its member again; r1 records %q", r.get("r1").Status.AssignedAction)
		// The deleted member took its lease horizon with it, and the deaf wire never acknowledges
		// the cancel the control plane sends r1: no robot is given the member before r1's lease is
		// provably dead, nor for the longest lease the configuration admits after that, since the
		// horizon is not known (the executor guard). An adapter that acknowledges ends the wait.
		for {
			start := time.Now()
			fa = r.workWith(dw, "t-m")
			end := time.Now()
			if dw.count("assign", "r2") > 0 {
				t.Errorf("INVARIANT: the member created again went to r2 %.1fs before r1's lease was provably dead, with "+
					"r1's acknowledgement never given and the horizon not known", dead.Sub(end).Seconds())
				break
			}
			if start.After(dead.Add(10 * time.Second)) {
				t.Logf("RESULT 10s after r1's lease death the member is %q: %q; r1 records %q", fa.Status.Phase,
					fa.Status.Message, r.get("r1").Status.AssignedAction)
				if (fa.Status.Phase != fleetv1.ActionPhasePending && fa.Status.Phase != "") || !strings.Contains(fa.Status.Message, "r1") {
					t.Errorf("INVARIANT: the member is not Pending behind r1, which still records it: %q %q", fa.Status.Phase, fa.Status.Message)
				}
				if dw.count("cancel", "r1") == 0 {
					t.Errorf("INVARIANT: r1 was never told to cancel the deleted member (wire: %s)", dw)
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}})

	// ── A fleet-manager narrows a zone stop that is applied ──
	// The edit would take zone c out of anc's reach: a robot that later enters c would not be
	// stopped. It is refused to every identity until anc's stop is cleared, and the refusal names
	// the clear.
	for _, edit := range []string{"propagate", "reparent", "child-delete"} {
		edit := edit
		rows = append(rows, invariantRow{name: "fleet-manager-narrows-an-applied-stop/" + edit, run: func(t *testing.T, r *rig) {
			fm := r.fleetManager()
			r.zone("anc", "", nil)
			r.zone("c", "anc", nil)
			r.zone("out", "", nil)
			r.robot("r1", "out")
			r.trigger(r.zoneObj("anc"), "anc-stop")
			r.waitMarker(r.zoneObj("anc"), true)
			var err error
			switch edit {
			case "propagate":
				z := r.zoneObj("anc")
				err = r.update(fm, z, false, func() { z.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: boolp(false)} })
			case "reparent":
				c := r.zoneObj("c")
				err = r.update(fm, c, false, func() { c.Spec.ParentZone = "" })
			case "child-delete":
				err = fm.Delete(context.Background(), r.zoneObj("c"))
			}
			t.Logf("RESULT the fleet-manager's %s under anc's applied stop: %v", edit, err)
			if err == nil {
				t.Errorf("INVARIANT: a fleet-manager, who may not clear anc's stop, narrowed it (%s)", edit)
				if edit == "child-delete" {
					r.waitGone(r.zoneObj("c"))
					r.zone("c", "", nil)
				}
			} else if !strings.Contains(err.Error(), "estop-clear") || !strings.Contains(err.Error(), `"anc"`) {
				t.Errorf("the refusal does not name anc's stop and the clear: %v", err)
			}
			r.setSpecZone("r1", "c")
			r.assertHeld("r1", true)
		}})
	}
	return rows
}
