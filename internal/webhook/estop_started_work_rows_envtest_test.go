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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Work a stop holds is not moved off its robot by the reference on its own (RFC-0001 safety.md,
// rows A1, A3, A4, A12 and A2, and the hold A6):
//   - A1: an Assigned action whose robot comes under a stop it confirms keeps its binding, its
//     lease and the robot's record (the binding was released at once);
//   - A12: work that was Preempted, started or not, is not released on A12's proof while a stop
//     holds its robot (it went to Pending on the acknowledged cancel);
//   - A3: a Failed (StopUnconfirmed) action whose robot a stop holds is not released when its
//     lease dies (it was released, and retried);
//   - A4: a cancel asked on that Failed action is carried out once the lease is provably dead
//     (it was never read);
//   - A2: a requeue asked on it while a stop holds the robot is carried out on A2's proof (it
//     was never read).
// No control plane runs: the stop is written as the control plane writes it, and the action is
// reconciled pass by pass.

// stopR1 writes estopState on r1 as the control plane's dispatcher does.
func (r *rig) stopR1(t *testing.T, s fleetv1.RobotEstopState) {
	t.Helper()
	rb := r.robotObj("r1")
	r.must("the control plane writes r1 "+string(s), r.update(r.w.as(t, controllerUser), rb, true, func() {
		rb.Status.EstopState = s
	}))
}

// failedStopUnconfirmed writes x as A3 leaves it: Failed (StopUnconfirmed), still bound to r1, with
// the lease horizon lease.
func (r *rig) failedStopUnconfirmed(t *testing.T, lease time.Time) {
	t.Helper()
	a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
	r.must("x fails on an unconfirmed stop", r.update(r.w.as(t, controllerUser), a, true, func() {
		now := metav1.Now()
		a.Status.Phase = fleetv1.ActionPhaseFailed
		a.Status.FailureReason = "StopUnconfirmed"
		a.Status.FailedAt = &now
		a.Status.StartedAt = &now
		a.Status.LeaseExpiresAt = &metav1.Time{Time: lease}
	}))
}

func (r *rig) actionObj(name string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
}

// setLease moves x's lease horizon, as time passing would.
func (r *rig) setLease(t *testing.T, lease time.Time) {
	t.Helper()
	a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
	r.must("x's lease horizon moves", r.update(r.w.as(t, controllerUser), a, true, func() {
		a.Status.LeaseExpiresAt = &metav1.Time{Time: lease}
	}))
}

func startedWorkRows() []invariantRow {
	var rows []invariantRow
	setup := func(r *rig) {
		r.zone("z", "", nil)
		r.config()
		r.robot("r1", "z")
	}

	// ── A1: an Assigned action keeps its robot under a confirmed stop ──
	for _, s := range []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopStopping} {
		s := s
		rows = append(rows, invariantRow{name: "held/a1-assigned-keeps-its-robot/" + string(s), opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			setup(r)
			r.hold("x", "r1", false)
			r.stopR1(t, s)
			r.assertPausedHeld(wl, "x", "r1", fleetv1.ActionPhasePaused)
			if got := r.get("r1").Status.AssignedAction; got != "x" {
				t.Errorf("INVARIANT: r1's record of x was released under its stop: %q", got)
			}
		}})
	}

	// ── A12: started work that was Preempted stays held while a stop holds its robot ──
	for _, s := range []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopResuming} {
		s := s
		rows = append(rows, invariantRow{name: "held/a12-started-preempted-work-stays/" + string(s), opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			setup(r)
			r.hold("x", "r1", true)
			a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			r.must("x is preempted after it started", r.update(r.w.as(t, controllerUser), a, true, func() {
				now := metav1.Now()
				a.Status.StartedAt = &now
				a.Status.Phase = fleetv1.ActionPhasePreempted
			}))
			r.stopR1(t, s)
			var fa *fleetv1.FleetAction
			for i := 0; i < 3; i++ {
				fa = r.work(wl, "x")
			}
			t.Logf("RESULT x after three passes under r1 %s: %s; wire: %s", s, r.actionLine("x"), wl)
			if fa.Status.Phase != fleetv1.ActionPhasePreempted || fa.Status.AssignedRobot != "r1" {
				t.Errorf("INVARIANT: started work a stop holds left r1: %s", r.actionLine("x"))
			}
		}})
	}

	// ── A12: work given but not started, then Preempted, stays held too: A12 moves work only while
	// no stop holds the robot (it went to Pending on A12's proof) ──
	for _, s := range []fleetv1.RobotEstopState{fleetv1.RobotEstopStopped, fleetv1.RobotEstopResuming} {
		s := s
		rows = append(rows, invariantRow{name: "held/a12-unstarted-preempted-work-stays/" + string(s), opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			setup(r)
			r.hold("x", "r1", false)
			a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			r.must("x is preempted before it started", r.update(r.w.as(t, controllerUser), a, true, func() {
				a.Status.Phase = fleetv1.ActionPhasePreempted
			}))
			r.stopR1(t, s)
			var fa *fleetv1.FleetAction
			for i := 0; i < 3; i++ {
				fa = r.work(wl, "x")
			}
			t.Logf("RESULT x after three passes under r1 %s: %s; wire: %s", s, r.actionLine("x"), wl)
			if fa.Status.Phase != fleetv1.ActionPhasePreempted || fa.Status.AssignedRobot != "r1" {
				t.Errorf("INVARIANT: unstarted work a stop holds left r1: %s", r.actionLine("x"))
			}
		}})
	}

	// ── A12: the same under a zone's or the namespace's stop. Issued: the stop is written and no
	// controller has applied it, so r1's own estop state holds nothing (a stop holds the robot
	// from the moment it is issued). Confirmed: the stop's controller fans it out and r1 reads
	// Stopped (it went to Pending on A12's proof) ──
	for _, sc := range []struct {
		scope     string
		confirmed bool
	}{{"zone", false}, {"zone", true}, {"namespace", false}, {"namespace", true}} {
		sc := sc
		name := "held/a12-unstarted-preempted-work-stays/" + sc.scope + "-stop-issued"
		if sc.confirmed {
			name = "held/a12-unstarted-preempted-work-stays/" + sc.scope + "-stop-confirmed"
		}
		rows = append(rows, invariantRow{name: name, opts: rigOpts{deferStart: !sc.confirmed}, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			setup(r)
			r.hold("x", "r1", false)
			a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: r.w.ns}}
			r.must("x is preempted before it started", r.update(r.w.as(t, controllerUser), a, true, func() {
				a.Status.Phase = fleetv1.ActionPhasePreempted
			}))
			if sc.scope == "zone" {
				r.trigger(r.zoneObj("z"), "z-stop")
			} else {
				r.trigger(r.configObj(), "ns-stop")
			}
			if sc.confirmed {
				r.waitStopped("r1")
			}
			var fa *fleetv1.FleetAction
			for i := 0; i < 3; i++ {
				fa = r.work(wl, "x")
			}
			t.Logf("RESULT x after three passes under the %s stop (r1 reads %q): %s; wire: %s", sc.scope,
				r.get("r1").Status.EstopState, r.actionLine("x"), wl)
			if fa.Status.Phase != fleetv1.ActionPhasePreempted || fa.Status.AssignedRobot != "r1" {
				t.Errorf("INVARIANT: unstarted work the %s stop holds left r1: %s", sc.scope, r.actionLine("x"))
			}
		}})
	}

	// ── A3/A6: a Failed (StopUnconfirmed) action is not released while a stop holds its robot ──
	rows = append(rows, invariantRow{name: "held/a3-failed-not-released-under-a-stop", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		setup(r)
		r.hold("x", "r1", true)
		r.stopR1(t, fleetv1.RobotEstopStopped)
		r.failedStopUnconfirmed(t, time.Now().Add(-time.Hour))
		var fa *fleetv1.FleetAction
		for i := 0; i < 3; i++ {
			fa = r.work(wl, "x")
		}
		t.Logf("RESULT x after its lease died under r1's stop: %s; r1 records %q", r.actionLine("x"), r.get("r1").Status.AssignedAction)
		if fa.Status.Phase != fleetv1.ActionPhaseFailed || fa.Status.AssignedRobot != "r1" {
			t.Errorf("INVARIANT: a Failed action a stop holds was released on its lease alone: %s", r.actionLine("x"))
		}
		if got := r.get("r1").Status.AssignedAction; got != "x" {
			t.Errorf("INVARIANT: r1's record of x was released under its stop: %q", got)
		}
	}})

	// ── A4: a cancel asked on a Failed (StopUnconfirmed) action waits for the lease, then runs ──
	rows = append(rows, invariantRow{name: "held/a4-cancel-on-failed-runs-on-a-dead-lease", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		setup(r)
		r.hold("x", "r1", true)
		r.failedStopUnconfirmed(t, time.Now().Add(time.Hour))
		r.must("cancel x", r.annotateAs(r.w.admin, r.actionObj("x"), "swarmada.io/cancel-requested", "operator"))
		fa := r.work(wl, "x")
		t.Logf("RESULT x with a cancel and a live lease: %s", r.actionLine("x"))
		if fa.Status.Phase != fleetv1.ActionPhaseFailed || fa.Status.AssignedRobot != "r1" {
			t.Errorf("INVARIANT: a cancel on a Failed (StopUnconfirmed) action ran before the lease died: %s", r.actionLine("x"))
		}
		r.setLease(t, time.Now().Add(-time.Hour))
		for i := 0; i < 2; i++ {
			fa = r.work(wl, "x")
		}
		t.Logf("RESULT x with a cancel and a dead lease: %s", r.actionLine("x"))
		if fa.Status.Phase != fleetv1.ActionPhaseCancelled {
			t.Errorf("A4: a cancel on a Failed (StopUnconfirmed) action was not carried out on a dead lease: %s", r.actionLine("x"))
		}
	}})

	// ── A2: a requeue asked on a Failed action a stop holds runs on A2's proof ──
	rows = append(rows, invariantRow{name: "held/a2-requeue-on-failed-under-a-stop", opts: rigOpts{deferStart: true}, run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		setup(r)
		r.hold("x", "r1", true)
		r.stopR1(t, fleetv1.RobotEstopStopped)
		r.failedStopUnconfirmed(t, time.Now().Add(time.Hour))
		r.must("requeue x as a holder of estop-clear",
			r.annotateAs(r.w.admin, r.actionObj("x"), "swarmada.io/requeue-requested", "operator"))
		var fa *fleetv1.FleetAction
		for i := 0; i < 2; i++ {
			fa = r.work(wl, "x")
		}
		t.Logf("RESULT x after a requeue under r1's stop: %s; wire: %s", r.actionLine("x"), wl)
		if fa.Status.Phase != fleetv1.ActionPhasePending || fa.Status.AssignedRobot != "" {
			t.Errorf("A2: a requeue on a Failed action a stop holds was not carried out on an acknowledged cancel: %s", r.actionLine("x"))
		}
	}})
	return rows
}
