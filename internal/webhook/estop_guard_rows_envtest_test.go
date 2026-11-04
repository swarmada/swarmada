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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controller"
)

// Rows of the estop hold invariant for the two guards: the stop guard (one answer to "which
// stops cover this robot", from the API server, over the recorded and declared zone with up to
// 64 ancestors, terminating robots included, fail-closed) and the executor guard (an action is
// given to a robot, and a robot's record of it released, only once the previous executor is
// provably off it) (RFC-0001 #safety-estop-scopes,
// #safety-assignment-lease-and-the-single-executor-guarantee). Each row names the shed it
// closes in its own comment, in words.

// stopFinalizer is the finalizer the controllers hold on a carrier while it carries a stop.
const stopFinalizer = "swarmada.io/estop-in-force"

const requeueAnnotation = "swarmada.io/requeue-requested"

const pendingFirmwareVersionKey = "swarmada.io/pending-firmware-version"

func (r *rig) guardState(robot string) string {
	rb := r.get(robot)
	return fmt.Sprintf("estopState=%q spec.zone=%q currentZone=%q estopHeldBy=%s assignedAction=%q deleting=%v finalizers=%v sends=%d",
		rb.Status.EstopState, rb.Spec.Zone, rb.Status.CurrentZone, holdsOf(rb.Status.EstopHeldBy), rb.Status.AssignedAction,
		rb.DeletionTimestamp != nil, rb.Finalizers, r.sendsTo(robot))
}

// inFlight writes name as an InProgress action bound to robot with a live lease, and robot's
// record of it, as the control plane leaves them; for a robot that dispatch would not choose (one
// being deleted).
func (r *rig) inFlight(name, robot string) {
	r.t.Helper()
	cp := r.w.as(r.t, controllerUser)
	r.createAction(name)
	a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
	r.must("marking "+name+" InProgress on "+robot, r.update(cp, a, true, func() {
		a.Status.Phase = fleetv1.ActionPhaseInProgress
		a.Status.AssignedRobot = robot
		a.Status.AssignmentGeneration = 1
		a.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(30 * time.Second)}
	}))
	rb := r.robotObj(robot)
	r.must(robot+" records "+name, r.update(cp, rb, true, func() {
		rb.Status.AssignedAction = name
		rb.Status.Phase = fleetv1.RobotPhaseInProgress
	}))
}

// operator binds the shipped operator role to a user of the row's own and returns a client
// acting as that user.
func (r *rig) operator() client.Client {
	r.t.Helper()
	user := "op-" + r.w.ns
	bindShippedClusterRole(r.t, r.w, "swarmada_operator_role.yaml", map[string]interface{}{
		"kind": "User", "name": user, "apiGroup": "rbac.authorization.k8s.io"})
	return asIdentity(r.t, r.w, user)
}

// annotateAs writes annotation key=value on obj as c.
func (r *rig) annotateAs(c client.Client, obj client.Object, key, value string) error {
	return r.update(c, obj, false, func() {
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[key] = value
		obj.SetAnnotations(ann)
	})
}

// createAction creates an unpinned Navigate action.
func (r *rig) createAction(name string) {
	r.t.Helper()
	r.must("creating "+name, r.w.admin.Create(context.Background(), &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: name},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
	}))
}

// assignedTo drives name until it is Assigned to robot with a lease, over wl.
func (r *rig) assignedTo(cmd command.ActionCommander, name, robot string) *fleetv1.FleetAction {
	r.t.Helper()
	fa := r.action(name)
	for i := 0; i < 30 && (fa.Status.Phase == "" || fa.Status.Phase == fleetv1.ActionPhasePending); i++ {
		fa = r.workWith(cmd, name)
		time.Sleep(100 * time.Millisecond)
	}
	if fa.Status.Phase != fleetv1.ActionPhaseAssigned || fa.Status.AssignedRobot != robot || fa.Status.LeaseExpiresAt == nil {
		r.t.Fatalf("setup: %s was not assigned to %s with a lease: phase=%q assignedRobot=%q lease=%v message=%q", name, robot,
			fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.LeaseExpiresAt, fa.Status.Message)
	}
	return fa
}

// noSecondExecutorBefore drives name over wl until dead plus margin and fails if any assign
// reaches other before dead. It returns whether other was assigned at all.
func (r *rig) noSecondExecutorBefore(cmd command.ActionCommander, wl *wireLog, name, first, other string, dead time.Time, margin time.Duration) bool {
	r.t.Helper()
	for {
		fa := r.workWith(cmd, name)
		end := time.Now()
		if wl.count("assign", other) > 0 {
			if end.Before(dead) {
				r.t.Errorf("INVARIANT: %s was given to %s %.1fs before %s's lease was provably dead (wire: %s)",
					name, other, dead.Sub(end).Seconds(), first, wl)
			}
			r.t.Logf("RESULT %s went to %s in a pass ending %.1fs after %s's lease death; wire: %s", name, other,
				end.Sub(dead).Seconds(), first, wl)
			return true
		}
		if end.After(dead.Add(margin)) {
			r.t.Logf("RESULT %s not given to %s by %s after %s's lease death: phase=%q robot=%q message=%q; %s records %q",
				name, other, margin, first, fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message, first,
				r.get(first).Status.AssignedAction)
			return false
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func guardRows() []invariantRow {
	var rows []invariantRow

	// ── A robot being deleted is a robot: it is stopped when it enters an applied stop ──
	// A fleet-manager adds a finalizer to a clean robot, deletes it, and moves it into a zone whose
	// stop is applied. The late-arrival pass used to skip a robot with a deletion timestamp; its
	// in-flight action kept renewing and its last finalizer went. A robot that records an action is
	// no longer deleted at all while the delete check serves (recordKeyRows); the delete check has
	// failurePolicy Ignore, so the robot here is deleted while it records nothing and its in-flight
	// action is written as the control plane would have left it after a delete admitted with no
	// replica serving.
	for _, how := range []string{"spec.zone", "currentZone"} {
		how := how
		rows = append(rows, invariantRow{name: "guard/terminating-robot-enters-an-applied-stop/" + how, run: func(t *testing.T, r *rig) {
			fm := r.fleetManager()
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.robot("r1", "out")
			rb := r.robotObj("r1")
			r.must("fleet-manager adds a finalizer", r.update(fm, rb, false, func() {
				rb.Finalizers = append(rb.Finalizers, "example.com/keep")
			}))
			r.must("fleet-manager deletes r1 while nothing holds it", fm.Delete(context.Background(), r.robotObj("r1")))
			r.inFlight("a-held", "r1")
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitMarker(r.zoneObj("z"), true)
			if how == "currentZone" {
				r.must("position report", r.setCurrentZone(r.w.as(t, controllerUser), "r1", "z"))
			} else {
				rb := r.robotObj("r1")
				r.must("fleet-manager moves r1 into stopped z", r.update(fm, rb, false, func() { rb.Spec.Zone = "z" }))
			}
			r.assertHeld("r1", true, "a-held")
			t.Logf("RESULT r1 in z under its applied stop: %s", r.guardState("r1"))
			rb = r.robotObj("r1")
			err := r.update(fm, rb, false, func() { rb.Finalizers = nil })
			if err == nil {
				t.Errorf("INVARIANT: the last finalizer of r1, which a stop in force covers, was removed")
			} else {
				t.Logf("RESULT last-finalizer removal: %v", err)
			}
		}})
	}

	// ── A robot entering a zone 64 levels below an applied stop wakes that stop ──
	// z64 has 64 ancestors; the fan-out reaches z0's stop down to it, but the event mapper used
	// to stop one ancestor short, so the arrival did not wake z0 and nothing stopped the robot.
	for _, how := range []string{"spec.zone", "currentZone"} {
		how := how
		rows = append(rows, invariantRow{name: "guard/arrival-64-below-an-applied-stop/" + how, run: func(t *testing.T, r *rig) {
			r.zone("z0", "", nil)
			for i := 1; i <= 64; i++ {
				r.zone(fmt.Sprintf("z%d", i), fmt.Sprintf("z%d", i-1), nil)
			}
			r.zone("other", "", nil)
			r.robot("r1", "other")
			r.hold("a-held", "r1", true)
			r.trigger(r.zoneObj("z0"), "z0-stop")
			r.waitMarker(r.zoneObj("z0"), true)
			if how == "spec.zone" {
				r.setSpecZone("r1", "z64")
			} else {
				r.must("position report", r.setCurrentZone(r.w.as(t, controllerUser), "r1", "z64"))
			}
			r.assertHeld("r1", true, "a-held")
			t.Logf("RESULT r1 in z64 under z0's applied stop: %s", r.guardState("r1"))
		}})
	}

	// ── The same arrival with the event path withheld: the stop's resync stops the robot ──
	// The zone stop controller sees no Robot event, so the arrival wakes nothing. A stop in force
	// re-evaluates the robots it covers every 30 s; that pass alone must stop r1 under z0.
	rows = append(rows, invariantRow{name: "guard/arrival-64-below-an-applied-stop/resync-only", opts: rigOpts{zoneEventsWithheld: true},
		run: func(t *testing.T, r *rig) {
			r.zone("z0", "", nil)
			for i := 1; i <= 64; i++ {
				r.zone(fmt.Sprintf("z%d", i), fmt.Sprintf("z%d", i-1), nil)
			}
			r.zone("other", "", nil)
			r.robot("r1", "other")
			r.hold("a-held", "r1", true)
			r.trigger(r.zoneObj("z0"), "z0-stop")
			r.waitMarker(r.zoneObj("z0"), true)
			// Let the passes the stop's own writes queued finish, so the next one is the resync.
			time.Sleep(3 * time.Second)
			r.setSpecZone("r1", "z64")
			moved := time.Now()
			stopped := func() (bool, string) {
				s := r.get("r1").Status.EstopState
				return s == fleetv1.RobotEstopStopped || s == fleetv1.RobotEstopFailed, string(s)
			}
			if early, _ := r.waitFor("r1 stopped within 5s of its arrival", 5*time.Second, stopped); early {
				t.Fatalf("setup: r1 was stopped %.1fs after its arrival, before any resync: an event reached the stop", time.Since(moved).Seconds())
			}
			ok, last := r.waitFor("r1 Stopped or Failed after its arrival in z64", 40*time.Second, stopped)
			t.Logf("RESULT r1 in z64 under z0's applied stop, no Robot event: stopped=%v after %.1fs (%s); %s",
				ok, time.Since(moved).Seconds(), last, r.guardState("r1"))
			if !ok {
				t.Errorf("INVARIANT: r1 arrived 64 levels below z0's applied stop and the stop's resync never stopped it (estopState %q)", last)
			}
			r.assertHeld("r1", true, "a-held")
		}})

	// ── A re-parent that keeps a zone under the stop but pushes robots past the bound ──
	// A over C1..C33 and A over D1..D33; a robot in C33; a fleet-manager re-parents C1 from A to
	// D33, so C33 ends 66 levels below A, past the fan-out's reach. The edit is refused for the
	// depth, pending or applied; the robot stays covered.
	for _, when := range []string{"pending", "applied"} {
		when := when
		rows = append(rows, invariantRow{name: "guard/reparent-pushes-robots-past-the-bound/" + when, opts: rigOpts{deferStart: when == "pending"},
			run: func(t *testing.T, r *rig) {
				fm := r.fleetManager()
				r.zone("aa", "", nil)
				r.zone("c1", "aa", nil)
				r.zone("d1", "aa", nil)
				for i := 2; i <= 33; i++ {
					r.zone(fmt.Sprintf("c%d", i), fmt.Sprintf("c%d", i-1), nil)
					r.zone(fmt.Sprintf("d%d", i), fmt.Sprintf("d%d", i-1), nil)
				}
				r.zone("out", "", nil)
				r.robot("rk", "c33")
				r.robot("r2", "out")
				if when == "pending" {
					r.hold("a-rk", "rk", true)
				} else {
					r.hold("a-r2", "r2", true)
				}
				r.trigger(r.zoneObj("aa"), "aa-stop")
				if when == "applied" {
					r.waitMarker(r.zoneObj("aa"), true)
				}
				c1 := r.zoneObj("c1")
				err := r.update(fm, c1, false, func() { c1.Spec.ParentZone = "d33" })
				t.Logf("RESULT fm re-parents c1 from aa to d33 (%s): %v", when, err)
				if err == nil {
					t.Errorf("INVARIANT: a re-parent that puts rk's zone 66 levels below aa was admitted while aa's stop is %s", when)
				}
				if when == "pending" {
					r.start()
					r.waitMarker(r.zoneObj("aa"), true)
					r.assertHeld("rk", true, "a-rk")
					return
				}
				r.setSpecZone("r2", "c33")
				r.assertHeld("r2", true, "a-r2")
			}})
	}
	// The other shape: a1 under stopped a0 is re-parented to the bottom of a 63-zone chain that
	// also hangs under a0, so a1 stays under a0 and its child a2 ends 65 levels below.
	for _, when := range []string{"pending", "applied"} {
		when := when
		rows = append(rows, invariantRow{name: "guard/reparent-deepens-a-subtree-under-the-stop/" + when, opts: rigOpts{deferStart: when == "pending"},
			run: func(t *testing.T, r *rig) {
				fm := r.fleetManager()
				r.zone("a0", "", nil)
				r.zone("a1", "a0", nil)
				r.zone("a2", "a1", nil)
				r.zone("y0", "a0", nil)
				for i := 1; i < 63; i++ {
					r.zone(fmt.Sprintf("y%d", i), fmt.Sprintf("y%d", i-1), nil)
				}
				r.zone("out", "", nil)
				r.robot("rk", "a2")
				r.robot("r2", "out")
				if when == "pending" {
					r.hold("a-rk", "rk", true)
				} else {
					r.hold("a-r2", "r2", true)
				}
				r.trigger(r.zoneObj("a0"), "a0-stop")
				if when == "applied" {
					r.waitMarker(r.zoneObj("a0"), true)
				}
				a1 := r.zoneObj("a1")
				err := r.update(fm, a1, false, func() { a1.Spec.ParentZone = "y62" })
				t.Logf("RESULT fm re-parents a1 from a0 to y62 (%s): %v", when, err)
				if err == nil {
					t.Errorf("INVARIANT: a re-parent that puts a2 65 levels below a0 was admitted while a0's stop is %s", when)
				}
				if when == "pending" {
					r.start()
					r.waitMarker(r.zoneObj("a0"), true)
					r.assertHeld("rk", true, "a-rk")
					return
				}
				r.setSpecZone("r2", "a2")
				r.assertHeld("r2", true, "a-r2")
			}})
	}

	// ── A declarative pause on an Assigned action, then a requeue ──
	// The binding is released as the state table says, and the robot is told to stop. With an
	// adapter that acknowledges the cancel the robot's record is released at once and a requeue
	// may give the action to another robot. With one that does not, the robot keeps recording the
	// action and no other robot is given it while the first may still move: not before its lease
	// is provably dead, and, the horizon being unknown, not within the longest lease either.
	for _, variant := range []string{"acknowledged", "deaf"} {
		variant := variant
		rows = append(rows, invariantRow{name: "guard/paused-assigned-then-requeue/" + variant, run: func(t *testing.T, r *rig) {
			var wl *wireLog
			dw := &deafWire{}
			ack := &wireLog{}
			if variant == "deaf" {
				wl = &dw.wireLog
			} else {
				wl = ack
			}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			r.shortLease()
			r.robot("r1", "z")
			r.createAction("x")
			work := func() *fleetv1.FleetAction {
				if variant == "deaf" {
					return r.workWith(dw, "x")
				}
				return r.workWith(ack, "x")
			}
			var fa *fleetv1.FleetAction
			for i := 0; i < 30; i++ {
				fa = work()
				if fa.Status.Phase == fleetv1.ActionPhaseAssigned {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if fa.Status.AssignedRobot != "r1" || fa.Status.LeaseExpiresAt == nil {
				t.Fatalf("setup: x not assigned to r1 with a lease: %q %q", fa.Status.Phase, fa.Status.AssignedRobot)
			}
			dead := fa.Status.LeaseExpiresAt.Add(suSkew)
			r.robot("r2", "out")
			x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			r.must("desiredState Paused", r.update(r.w.admin, x, false, func() { x.Spec.DesiredState = fleetv1.DesiredStatePaused }))
			fa = work()
			t.Logf("RESULT x after desiredState Paused: phase=%q assignedRobot=%q lease=%v message=%q r1 records %q wire: %s",
				fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.LeaseExpiresAt, fa.Status.Message, r.get("r1").Status.AssignedAction, wl)
			if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "" {
				t.Errorf("INVARIANT: x is %q bound to %q after the hold, not Paused with the binding released", fa.Status.Phase, fa.Status.AssignedRobot)
			}
			if wl.count("cancel", "r1") == 0 {
				t.Errorf("INVARIANT: the hold did not tell r1 to stop (wire: %s)", wl)
			}
			r.setAnnotation(&fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}, requeueAnnotation, strp("operator"))
			if variant == "acknowledged" {
				if r.get("r1").Status.AssignedAction != "" {
					t.Errorf("INVARIANT: r1 still records x after its adapter acknowledged the cancel")
				}
				// The requeue may give x to either robot: r1 is idle again, provably off x.
				for i := 0; i < 30 && wl.count("assign", "r1")+wl.count("assign", "r2") < 2; i++ {
					work()
					time.Sleep(100 * time.Millisecond)
				}
				if wl.count("assign", "r1")+wl.count("assign", "r2") < 2 {
					t.Errorf("control: with the cancel acknowledged the requeue did not give x to a robot: %s", wl)
				}
				return
			}
			given := r.noSecondExecutorBefore(dw, wl, "x", "r1", "r2", dead, 8*time.Second)
			if given {
				t.Errorf("INVARIANT: x was given to r2 while r1 has not acknowledged the stop and the horizon is not known")
			}
			if r.get("r1").Status.AssignedAction != "x" {
				t.Errorf("INVARIANT: r1's record of x was released without proof")
			}
			if msg := r.action("x").Status.Message; !strings.Contains(msg, "r1") || !strings.Contains(msg, "still records") {
				t.Errorf("the hold does not name the robot that may still run x: %q", msg)
			}
		}})
	}

	// ── A deleted member created again under its name, with the lease changed in between ──
	// The predecessor's lease was granted under the old duration and nothing records it once the
	// predecessor is deleted, so the wait is bounded by the longest lease the configuration
	// admits, not by the duration read now. Before the fix the wait read the new, shorter lease
	// and a second robot was given the action inside the first robot's live lease.
	for _, variant := range []string{"lease-shortened", "config-deleted"} {
		variant := variant
		rows = append(rows, invariantRow{name: "guard/regenerated-member-after-the-" + variant, run: func(t *testing.T, r *rig) {
			dw := &deafWire{}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			r.config()
			cfg := r.configObj()
			lease := int32(60)
			if variant == "config-deleted" {
				lease = 120
			}
			r.must("the predecessor's lease", r.update(r.w.admin, cfg, false, func() {
				cfg.Spec.Scheduling.LeaseDurationSeconds = lease
				cfg.Spec.Scheduling.ClockSkewMarginSeconds = 5
			}))
			r.robot("r1", "z")
			r.createAction("x")
			fa := r.assignedTo(dw, "x", "r1")
			dead := fa.Status.LeaseExpiresAt.Add(5 * time.Second)
			r.robot("r2", "out")
			r.must("deleting x", r.w.admin.Delete(context.Background(), r.action("x")))
			r.waitGone(&fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "x"}})
			if variant == "lease-shortened" {
				r.must("lease shortened to 10 s, skew 1 s", r.update(r.w.admin, cfg, false, func() {
					cfg.Spec.Scheduling.LeaseDurationSeconds = 10
					cfg.Spec.Scheduling.ClockSkewMarginSeconds = 1
				}))
			} else {
				r.must("deleting the SwarmadaConfig", r.w.admin.Delete(context.Background(), r.configObj()))
			}
			r.createAction("x")
			t.Logf("RESULT x created again; r1 records %q; r1's lease is provably dead at %s", r.get("r1").Status.AssignedAction,
				dead.UTC().Format(time.RFC3339))
			// The deaf wire never acknowledges the cancel the guard sends, and the horizon is not
			// known, so no robot is given x before dead, nor within the margin after it.
			given := r.noSecondExecutorBefore(dw, &dw.wireLog, "x", "r1", "r2", dead, 5*time.Second)
			if given && time.Now().Before(dead) {
				t.Errorf("INVARIANT: x went to r2 before r1's lease was provably dead")
			}
			if msg := r.action("x").Status.Message; !strings.Contains(msg, "r1") {
				t.Errorf("the hold does not name r1: %q", msg)
			}
		}})
	}

	// ── A position frame read against a cache-behind Robot ──
	// The Zone Controller resolved the robot from a lagging cache that still showed it in w, so
	// the hold saw a move out of w (no stop) and let the frame write currentZone y while z's stop
	// was pending. The guard decides on the robot as the API server records it.
	rows = append(rows, invariantRow{name: "guard/cache-behind-robot-position-report", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		r.geoZone("w", 0)
		r.geoZone("z", 20)
		r.geoZone("y", 40)
		r.robot("rk", "w")
		fresh := r.positionFeed(nil, "w", "z", "y")
		if got := fresh.at("rk", 5); got != "w" {
			t.Fatalf("setup: rk at w recorded %q", got)
		}
		if got := fresh.at("rk", 25); got != "z" {
			t.Fatalf("setup: rk at z recorded %q", got)
		}
		r.hold("a-held", "rk", true)
		r.trigger(r.zoneObj("z"), "z-stop") // pending: no control plane yet
		stale := r.positionFeed(&staleRobotReads{Client: r.w.as(t, controllerUser), robot: "rk", zone: "w"}, "w", "z", "y")
		got := stale.at("rk", 45)
		t.Logf("RESULT a frame in y read against a Robot whose cached currentZone reads w: stored currentZone=%q", got)
		if got != "z" {
			t.Errorf("INVARIANT: a cache-behind read moved rk's recorded zone out of z (%q) while z's stop was pending", got)
		}
		r.start()
		r.waitMarker(r.zoneObj("z"), true)
		r.assertHeld("rk", true, "a-held")
	}})

	// ── A declared-membership pass read against a cache-behind Robot ──
	// rk is placed in z (identity-only zones: membership is declared) and re-homed to y before z's
	// stop is issued. A FleetZone reconcile of y that resolves rk from a lagging cache, which still
	// shows it in w, sees a move out of w (no stop) and writes currentZone y while z's stop is
	// pending. The hold decides on the robot as the API server records it, so rk stays in z.
	rows = append(rows, invariantRow{name: "guard/cache-behind-robot-declared-membership", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		r.zone("w", "", nil)
		r.zone("z", "", nil)
		r.zone("y", "", nil)
		r.robot("rk", "z")
		m := r.w.as(t, controllerUser)
		place := func(c client.Client, zone string) string {
			zc := &controller.ZoneController{Client: c, APIReader: m, Scheme: r.w.scheme, Recorder: record.NewFakeRecorder(1024)}
			if _, err := zc.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: zone}}); err != nil {
				t.Logf("RESULT reconciling zone %s: %v", zone, err)
			}
			return r.get("rk").Status.CurrentZone
		}
		if got := place(m, "z"); got != "z" {
			t.Fatalf("setup: rk declared in z recorded currentZone %q", got)
		}
		r.hold("a-held", "rk", true)
		r.setSpecZone("rk", "y")
		r.trigger(r.zoneObj("z"), "z-stop") // pending: no control plane yet
		got := place(&staleRobotReads{Client: m, robot: "rk", zone: "w"}, "y")
		t.Logf("RESULT a declared-membership pass for y read against a Robot whose cached currentZone reads w: stored currentZone=%q", got)
		if got != "z" {
			t.Errorf("INVARIANT: a cache-behind read moved rk's recorded zone out of z (%q) while z's stop was pending", got)
		}
		r.start()
		r.waitMarker(r.zoneObj("z"), true)
		r.assertHeld("rk", true, "a-held")
	}})

	// ── A declarative pause on an InProgress action tells the robot to stop ──
	rows = append(rows, invariantRow{name: "guard/paused-inprogress-tells-the-robot-to-stop", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
		r.holdUnpinned(wl, "x", "r1")
		x := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
		r.must("desiredState Paused", r.update(r.w.admin, x, false, func() { x.Spec.DesiredState = fleetv1.DesiredStatePaused }))
		fa := r.work(wl, "x")
		t.Logf("RESULT x after desiredState Paused while InProgress: phase=%q assignedRobot=%q message=%q wire: %s",
			fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message, wl)
		if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" {
			t.Errorf("INVARIANT: x is %q bound to %q, not Paused and still bound to r1", fa.Status.Phase, fa.Status.AssignedRobot)
		}
		if wl.count("cancel", "r1") == 0 {
			t.Errorf("INVARIANT: the robot kept driving: desiredState Paused sent r1 nothing (wire: %s)", wl)
		}
		if !strings.Contains(fa.Status.Message, "told to stop") {
			t.Errorf("the message does not say the robot was told to stop: %q", fa.Status.Message)
		}
	}})

	// ── A requeue of work an emergency stop paused needs the stop's own authority ──
	// The built-in operator role can patch fleetactions and used to requeue an action a stop
	// paused; it holds no estop-clear. The fleet-manager holds estop-trigger, not estop-clear.
	// The admin holds estop-clear and is admitted.
	for _, who := range []string{"operator", "fleet-manager", "admin"} {
		who := who
		rows = append(rows, invariantRow{name: "guard/requeue-of-estop-paused-work-needs-estop-clear/" + who, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			r.zone("z", "", nil)
			r.config()
			r.robot("r1", "z")
			r.holdUnpinned(wl, "x", "r1")
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitStopped("r1")
			fa := r.work(wl, "x")
			if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" {
				t.Fatalf("setup: x is %q bound to %q, not Paused on r1", fa.Status.Phase, fa.Status.AssignedRobot)
			}
			var c client.Client
			switch who {
			case "operator":
				c = r.operator()
			case "fleet-manager":
				c = r.fleetManager()
			default:
				c = r.w.admin
			}
			err := r.annotateAs(c, &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}, requeueAnnotation, who)
			t.Logf("RESULT %s requeues x, paused by z's stop: %v", who, err)
			if who == "admin" {
				if err != nil {
					t.Errorf("control: the admin, who holds estop-clear on z, was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("INVARIANT: %s, who holds no estop-clear on z, requeued work z's stop paused", who)
			} else if !strings.Contains(err.Error(), "estop-clear") || !strings.Contains(err.Error(), `"z"`) {
				t.Errorf("the refusal does not name the estop-clear verb and the stop: %v", err)
			}
		}})
	}

	// ── A carrier holds a finalizer while it carries a stop ──
	// The trigger lands with no manager running; the carrier already holds the finalizer, so a
	// delete admitted while no replica serves the delete check cannot complete before the manager
	// returns, fans the stop out and applies the clear. The clear releases it.
	for _, carrier := range []string{"zone", "namespace"} {
		carrier := carrier
		rows = append(rows, invariantRow{name: "guard/carrier-holds-a-finalizer-while-stopped/" + carrier, opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			r.zone("z", "", nil)
			r.robot("r1", "z")
			var obj client.Object = r.zoneObj("z")
			if carrier == "namespace" {
				r.config()
				obj = r.configObj()
			}
			r.trigger(obj, "stop")
			has := func() bool {
				r.must("reading the carrier", r.w.admin.Get(context.Background(), client.ObjectKeyFromObject(obj), obj))
				for _, f := range obj.GetFinalizers() {
					if f == stopFinalizer {
						return true
					}
				}
				return false
			}
			if !has() {
				t.Errorf("INVARIANT: the %s carrier holds no finalizer while its stop is issued and no manager has run", carrier)
			}
			r.start()
			r.waitMarker(obj, true)
			r.waitStopped("r1")
			if !has() {
				t.Errorf("INVARIANT: the %s carrier lost its finalizer while its stop is applied", carrier)
			}
			r.clearStop(obj)
			r.waitMarker(obj, false)
			if ok, _ := r.waitFor("the finalizer released after the clear", 10*time.Second, func() (bool, string) {
				return !has(), fmt.Sprintf("%v", obj.GetFinalizers())
			}); !ok {
				t.Errorf("the %s carrier keeps the finalizer after its clear was applied", carrier)
			}
		}})
	}

	// ── A hand-written firmware instruction on a robot a stop covers ──
	// Written while covered: refused to a fleet-manager. Written before the stop: withdrawn when
	// the stop comes to cover the robot.
	rows = append(rows, invariantRow{name: "guard/firmware-instruction-refused-on-a-covered-robot", run: func(t *testing.T, r *rig) {
		fm := r.fleetManager()
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "z")
		r.robot("r2", "out")
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitStopped("r1")
		err := r.annotateAs(fm, r.robotObj("r1"), pendingFirmwareVersionKey, "2.0.0")
		t.Logf("RESULT fm writes a pending firmware version on stopped r1: %v", err)
		if err == nil {
			t.Errorf("INVARIANT: a firmware instruction was written on a robot a stop covers")
		} else if !strings.Contains(err.Error(), `"z"`) {
			t.Errorf("the refusal does not name the stop: %v", err)
		}
		if err := r.annotateAs(fm, r.robotObj("r2"), pendingFirmwareVersionKey, "2.0.0"); err != nil {
			t.Errorf("control: the same write on an uncovered robot was refused: %v", err)
		}
	}})
	rows = append(rows, invariantRow{name: "guard/firmware-instruction-withdrawn-when-a-stop-covers-the-robot", run: func(t *testing.T, r *rig) {
		fm := r.fleetManager()
		r.zone("z", "", nil)
		r.robot("r1", "z")
		r.must("fm writes a pending firmware version before any stop", r.annotateAs(fm, r.robotObj("r1"), pendingFirmwareVersionKey, "2.0.0"))
		r.trigger(r.zoneObj("z"), "z-stop")
		r.waitMarker(r.zoneObj("z"), true)
		r.waitStopped("r1")
		if ok, last := r.waitFor("the firmware instruction withdrawn", 10*time.Second, func() (bool, string) {
			v, has := r.get("r1").Annotations[pendingFirmwareVersionKey]
			return !has, v
		}); !ok {
			t.Errorf("INVARIANT: the firmware instruction %q still stands on r1 under z's stop", last)
		}
	}})

	// ── A zone with 65 ancestors is refused naming the depth, not a cycle ──
	rows = append(rows, invariantRow{name: "guard/zone-65-deep-refused-naming-the-depth", run: func(t *testing.T, r *rig) {
		r.zone("z0", "", nil)
		for i := 1; i <= 64; i++ {
			r.zone(fmt.Sprintf("z%d", i), fmt.Sprintf("z%d", i-1), nil)
		}
		err := r.w.admin.Create(context.Background(), &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z65", Namespace: r.w.ns},
			Spec: fleetv1.FleetZoneSpec{ParentZone: "z64"}})
		t.Logf("RESULT create z65 (65 ancestors): %v", err)
		if err == nil {
			t.Errorf("INVARIANT: a zone with 65 ancestors was admitted")
		} else if strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "64") {
			t.Errorf("the refusal calls a deep chain a cycle, or does not name the depth: %v", err)
		}
		r.zone("top", "", nil)
		z0 := r.zoneObj("z0")
		err = r.update(r.w.admin, z0, false, func() { z0.Spec.ParentZone = "top" })
		t.Logf("RESULT re-parent z0 under top (z64 would have 65 ancestors): %v", err)
		if err == nil {
			t.Errorf("INVARIANT: a re-parent that gives z64 65 ancestors was admitted")
		} else if !strings.Contains(err.Error(), "64") {
			t.Errorf("the refusal does not name the depth: %v", err)
		}
	}})

	return rows
}

// staleRobotReads answers reads of one Robot with an older status.currentZone, as a lagging
// cache would; every write goes to the API server.
type staleRobotReads struct {
	client.Client
	robot, zone string
}

func (s *staleRobotReads) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := s.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if rl, ok := list.(*fleetv1.RobotList); ok {
		for i := range rl.Items {
			if rl.Items[i].Name == s.robot {
				rl.Items[i].Status.CurrentZone = s.zone
			}
		}
	}
	return nil
}

func (s *staleRobotReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := s.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if rb, ok := obj.(*fleetv1.Robot); ok && rb.Name == s.robot {
		rb.Status.CurrentZone = s.zone
	}
	return nil
}

var (
	_ = apierrors.IsNotFound
	_ = types.NamespacedName{}
	_ = ctrl.Request{}
	_ = controller.SwarmadaConfigName
)

// taskCancelRows: a FleetTask cancelled while a stop holds its member's robot. The task's
// desiredState reaches every member, the member's cancel is confirmed through the executor
// guard, and the robot is released to Idle once the stop is cleared and the robot resumed.
func taskCancelRows() []invariantRow {
	var rows []invariantRow
	for _, when := range []string{"cancel-while-stopped", "cancel-after-resume"} {
		when := when
		rows = append(rows, invariantRow{name: "guard/task-cancel-frees-an-estop-paused-member/" + when, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			r.zone("z", "", nil)
			r.config()
			r.robot("r1", "z")
			tr := &controller.FleetTaskReconciler{Client: r.w.as(t, controllerUser), Scheme: r.w.scheme}
			r.must("creating task h", r.w.admin.Create(context.Background(), &fleetv1.FleetTask{
				ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: "h"},
				// A flow task, as the handover demo's is: two members in sequence.
				Spec: fleetv1.FleetTaskSpec{Actions: []fleetv1.FleetTaskAction{
					{Name: "t1", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: fleetv1.ActionFailureAlert}},
					{Name: "t2", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: fleetv1.ActionFailureAlert}},
				}, Flow: &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{{Run: strp("t1")}, {Run: strp("t2")}}}},
			}))
			reconcileTask := func() {
				_, err := tr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: "h"}})
				if err != nil {
					t.Logf("RESULT task reconcile: %v", err)
				}
			}
			var fa *fleetv1.FleetAction
			for i := 0; i < 30; i++ {
				reconcileTask()
				var got fleetv1.FleetAction
				if r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: "h-t1"}, &got) == nil {
					if fa = r.work(wl, "h-t1"); fa.Status.Phase == fleetv1.ActionPhaseAssigned {
						break
					}
				}
				time.Sleep(100 * time.Millisecond)
			}
			if fa == nil || fa.Status.AssignedRobot != "r1" {
				t.Fatalf("setup: the member was not assigned to r1: %+v", fa)
			}
			a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "h-t1", Namespace: r.w.ns}}
			r.must("marking h-t1 InProgress", r.update(r.w.as(t, controllerUser), a, true, func() { a.Status.Phase = fleetv1.ActionPhaseInProgress }))
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitStopped("r1")
			fa = r.work(wl, "h-t1")
			if fa.Status.Phase != fleetv1.ActionPhasePaused || fa.Status.AssignedRobot != "r1" {
				t.Fatalf("setup: h-t1 is %q bound to %q, not Paused on r1", fa.Status.Phase, fa.Status.AssignedRobot)
			}
			if when == "cancel-after-resume" {
				r.clearStop(r.zoneObj("z"))
				r.waitMarker(r.zoneObj("z"), false)
				r.resume("r1", "ok")
			}
			task := &fleetv1.FleetTask{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: r.w.ns}}
			r.must("cancelling the task", r.update(r.w.admin, task, false, func() { task.Spec.DesiredState = fleetv1.DesiredStateCancelled }))
			reconcileTask()
			fa = r.action("h-t1")
			t.Logf("RESULT after the task cancel: h-t1 desiredState=%q phase=%q robot=%q message=%q", fa.Spec.DesiredState, fa.Status.Phase,
				fa.Status.AssignedRobot, fa.Status.Message)
			if fa.Spec.DesiredState != fleetv1.DesiredStateCancelled {
				t.Errorf("INVARIANT: the task's cancel did not reach its paused member (desiredState %q)", fa.Spec.DesiredState)
			}
			for i := 0; i < 50 && fa.Status.Phase != fleetv1.ActionPhaseCancelled; i++ {
				fa = r.work(wl, "h-t1")
				time.Sleep(100 * time.Millisecond)
			}
			t.Logf("RESULT h-t1 after the cancel was driven: phase=%q robot=%q message=%q wire: %s r1: %s", fa.Status.Phase,
				fa.Status.AssignedRobot, fa.Status.Message, wl, r.guardState("r1"))
			if fa.Status.Phase != fleetv1.ActionPhaseCancelled {
				t.Errorf("INVARIANT: the paused member was not cancelled (phase %q, message %q)", fa.Status.Phase, fa.Status.Message)
			}
			if wl.count("cancel", "r1") == 0 {
				t.Errorf("INVARIANT: r1 was never told to cancel (wire: %s)", wl)
			}
			if when == "cancel-while-stopped" {
				r.clearStop(r.zoneObj("z"))
				r.waitMarker(r.zoneObj("z"), false)
				r.resume("r1", "ok")
			}
			for i := 0; i < 30; i++ {
				_ = r.work(wl, "h-t1")
				if rb := r.get("r1"); rb.Status.AssignedAction == "" && rb.Status.Phase == fleetv1.RobotPhaseIdle {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			rb := r.get("r1")
			t.Logf("RESULT r1 after cancel, clear and resume: phase=%q assignedAction=%q estopState=%q", rb.Status.Phase, rb.Status.AssignedAction, rb.Status.EstopState)
			if rb.Status.AssignedAction != "" || rb.Status.Phase != fleetv1.RobotPhaseIdle {
				t.Errorf("INVARIANT: r1 is %q recording %q after the cancel, the clear and the resume, not Idle", rb.Status.Phase, rb.Status.AssignedAction)
			}
		}})
	}
	return rows
}
