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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
)

// Row A11 (RFC-0001 #safety-action-table) takes a preemption victim only from Assigned or
// InProgress, and only on a robot under no stop; a Paused action is operator-gated and is never
// displaced (#safety-estop-is-not-a-preemption). These rows run both preemption paths: the zone
// capacity path, with the real TDE engine evicting a reservation, and the robot path.

type victimRig struct {
	r   *FleetActionReconciler
	c   client.Client
	eng *tde.Engine
	fc  *fakeCommander
}

func newVictimRig(t *testing.T, objs ...client.Object) *victimRig {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs = append([]client.Object{readyActionAdapter()}, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}, &fleetv1.FleetZone{}).Build()
	eng := tde.New(c, tde.DefaultConfig())
	eng.SetRecovered(true)
	fc := &fakeCommander{cancelAck: true}
	r := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler(), TDE: eng, Commander: fc}
	return &victimRig{r: r, c: c, eng: eng, fc: fc}
}

func victimZone(name, parent string, max int32) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetZoneSpec{ParentZone: parent, MaxConcurrentRobots: max}}
}

// zoneVictim is the Normal action "norm" bound to r1 in phase, its spec.zone zone; started sets
// status.startedAt.
func zoneVictim(zone string, phase fleetv1.ActionPhase, started bool, msg string) *fleetv1.FleetAction {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	v := bandAction("norm", "r1", phase, fleetv1.ActionPriorityNormal, 2, lease)
	v.Spec.Zone = zone
	v.Status.Message = msg
	if started {
		st := metav1.NewTime(time.Now().Add(-time.Minute))
		v.Status.StartedAt = &st
	}
	return v
}

// holdSlot gives "norm" its zone slot: Reserved, or Occupied once r1 is in the zone.
func (g *victimRig) holdSlot(t *testing.T, zone string, occupied bool) {
	t.Helper()
	res, err := g.eng.RequestReservation(context.Background(), tde.ReservationRequest{
		ActionID: "norm", RobotID: "r1", Namespace: actionNS, TargetZone: zone, Priority: fleetv1.ActionPriorityNormal})
	if err != nil || res.Status != tde.Granted {
		t.Fatalf("setup: the victim's reservation: %v %v", res.Status, err)
	}
	if occupied {
		if err := g.eng.OnRobotEnteredZone(context.Background(), actionNS, zone, "r1"); err != nil {
			t.Fatalf("setup: r1 entering %s: %v", zone, err)
		}
	}
}

func criticalIn(zone string) *fleetv1.FleetAction {
	a := criticalPending("crit")
	a.Spec.Zone = zone
	return a
}

func stoppedR1In(zone string) *fleetv1.Robot {
	r1 := robotEstopped("r1", fleetv1.RobotEstopStopped, "norm")
	r1.Spec.Zone, r1.Status.CurrentZone = zone, zone
	r1.Annotations = map[string]string{estopscope.TriggerAnnotation: "op-stop"}
	return r1
}

func idleR2In(zone string) *fleetv1.Robot {
	r2 := idleRobotInZone("r2", zone)
	r2.Status.BatteryPercent = p32(90)
	return r2
}

// keptVictim asserts that "norm" is still phase, bound to r1, that r1 still records it, and
// that no cancel_action reached r1.
func (g *victimRig) keptVictim(t *testing.T, phase fleetv1.ActionPhase) {
	t.Helper()
	v := getAction(t, g.c, "norm")
	if v.Status.Phase != phase || v.Status.AssignedRobot != "r1" {
		t.Errorf("INVARIANT: the victim is %q bound to %q (%q), not kept %s on r1", v.Status.Phase, v.Status.AssignedRobot,
			v.Status.Message, phase)
	}
	if r1 := getRobot(t, g.c, "r1", actionNS); r1.Status.AssignedAction != "norm" {
		t.Errorf("INVARIANT: r1 records %q, not norm", r1.Status.AssignedAction)
	}
	for i := range g.fc.cancels {
		if g.fc.cancelTo[i] == "r1" {
			t.Errorf("INVARIANT: cancel_action for %s sent to r1", g.fc.cancels[i].ActionID)
		}
	}
}

// Zone-capacity path: no Paused action's reservation is evicted, whatever held it.
func TestPreemption_ZoneCapacity_NeverEvictsAPausedAction(t *testing.T) {
	cases := []struct {
		name     string
		occupied bool
		objs     func() []client.Object
	}{
		{name: "robot-stop/not-started", objs: func() []client.Object {
			return []client.Object{victimZone("z", "", 1), zoneVictim("z", fleetv1.ActionPhasePaused, false, estopPausedAssignedMessage),
				stoppedR1In("z"), idleR2In("z"), criticalIn("z")}
		}},
		{name: "robot-stop/started", occupied: true, objs: func() []client.Object {
			return []client.Object{victimZone("z", "", 1), zoneVictim("z", fleetv1.ActionPhasePaused, true, estopPausedInProgressMessage),
				stoppedR1In("z"), idleR2In("z"), criticalIn("z")}
		}},
		{name: "zone-stop-on-a-child/started", occupied: true, objs: func() []client.Object {
			a := victimZone("a", "p", 0)
			a.Annotations = map[string]string{annEstopTriggered: "zs", annEstopProcessed: "zs"}
			a.Status.EstopStatus = fleetv1.ZoneEstopTriggered
			r1 := robotEstopped("r1", fleetv1.RobotEstopStopped, "norm")
			r1.Spec.Zone, r1.Status.CurrentZone = "a", "a"
			r1.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "a"}}
			return []client.Object{victimZone("p", "", 1), a, victimZone("b", "p", 0),
				zoneVictim("p", fleetv1.ActionPhasePaused, true, estopPausedInProgressMessage), r1, idleR2In("b"), criticalIn("p")}
		}},
		{name: "desired-state-paused/no-stop", occupied: true, objs: func() []client.Object {
			v := zoneVictim("z", fleetv1.ActionPhasePaused, true, desiredPausedInProgressMessage)
			v.Spec.DesiredState = fleetv1.DesiredStatePaused
			r1 := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm")
			r1.Spec.Zone, r1.Status.CurrentZone = "z", "z"
			return []client.Object{victimZone("z", "", 1), v, r1, idleR2In("z"), criticalIn("z")}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newVictimRig(t, tc.objs()...)
			zone := getAction(t, g.c, "norm").Spec.Zone
			g.holdSlot(t, zone, tc.occupied)
			reconcileAction(t, g.r, "crit")
			reconcileAction(t, g.r, "norm")
			reconcileAction(t, g.r, "norm")
			g.keptVictim(t, fleetv1.ActionPhasePaused)
			if crit := getAction(t, g.c, "crit"); crit.Status.Phase != fleetv1.ActionPhasePending {
				t.Errorf("the preemptor is %q on %q: it took the slot of a Paused action", crit.Status.Phase, crit.Status.AssignedRobot)
			}
			if st, _ := g.eng.ZoneStatus(context.Background(), actionNS, zone); st.Occupied+st.Reserved != 1 {
				t.Errorf("the victim's reservation was evicted: zone %s status %+v", zone, st)
			}
		})
	}
}

// Zone-capacity path, the control: an InProgress victim on a robot under no stop is evicted and
// marked Preempted, keeping its binding (rows A11, A12).
func TestPreemption_ZoneCapacity_EvictsAnInProgressVictimUnderNoStop(t *testing.T) {
	r1 := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm")
	r1.Spec.Zone, r1.Status.CurrentZone = "z", "z"
	g := newVictimRig(t, victimZone("z", "", 1), zoneVictim("z", fleetv1.ActionPhaseInProgress, true, ""), r1,
		idleR2In("z"), criticalIn("z"))
	g.holdSlot(t, "z", true)
	reconcileAction(t, g.r, "crit")
	if v := getAction(t, g.c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted || v.Status.AssignedRobot != "r1" {
		t.Errorf("control: the InProgress victim is %q bound to %q, not Preempted on r1", v.Status.Phase, v.Status.AssignedRobot)
	}
}

// Zone-capacity path: an InProgress victim whose robot a stop in force covers, before the stop is
// written onto the robot, is no victim (A11: "its robot not under a stop").
func TestPreemption_ZoneCapacity_SkipsAVictimAStopCovers(t *testing.T) {
	z := victimZone("z", "", 1)
	z.Annotations = map[string]string{annEstopTriggered: "zs", annEstopProcessed: "zs"}
	r1 := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm")
	r1.Spec.Zone, r1.Status.CurrentZone = "z", "z"
	g := newVictimRig(t, z, zoneVictim("z", fleetv1.ActionPhaseInProgress, true, ""), r1, idleR2In("z"), criticalIn("z"))
	g.holdSlot(t, "z", true)
	reconcileAction(t, g.r, "crit")
	g.keptVictim(t, fleetv1.ActionPhaseInProgress)
}

// Robot path: a Paused action on a robot whose stop is cleared is still operator-gated, and is
// not displaced, in place or through a type-changing handover.
func TestPreemption_RobotPath_NeverDisplacesAPausedAction(t *testing.T) {
	for _, typeChange := range []bool{false, true} {
		name := "same-type"
		if typeChange {
			name = "type-change"
		}
		t.Run(name, func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
			victim := bandAction("norm", "r1", fleetv1.ActionPhasePaused, fleetv1.ActionPriorityNormal, 2, lease)
			victim.Status.Message = estopPausedInProgressMessage
			if typeChange {
				victim.Spec.Type = fleetv1.ActionTypePatrol
			}
			rob := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm")
			rob.Status.EstopState = fleetv1.RobotEstopNormal
			r, c := newActionReconciler(t, criticalPending("crit"), victim, rob)
			fc := &fakeCommander{cancelAck: true}
			r.Commander = fc

			reconcileAction(t, r, "crit")

			if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePaused || v.Status.AssignedRobot != "r1" {
				t.Errorf("INVARIANT: a Paused (operator-gated) action was displaced: %q bound to %q", v.Status.Phase, v.Status.AssignedRobot)
			}
			if crit := getAction(t, c, "crit"); crit.Status.AssignedRobot == "r1" {
				t.Errorf("INVARIANT: the preemptor was given r1, which holds a Paused action")
			}
			if len(fc.cancels) > 0 {
				t.Errorf("INVARIANT: cancel_action sent to the Paused victim: %+v", fc.cancels)
			}
		})
	}
}

// mayDisplace, row A11's victim condition, phase by phase and stop by stop.
func TestMayDisplace_IsRowA11(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase fleetv1.ActionPhase
		prio  fleetv1.ActionPriority
		estop fleetv1.RobotEstopState
		want  bool
	}{
		{"inprogress/normal/no-stop", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, fleetv1.RobotEstopNormal, true},
		{"assigned/low/no-stop", fleetv1.ActionPhaseAssigned, fleetv1.ActionPriorityLow, fleetv1.RobotEstopNormal, true},
		{"paused/normal/no-stop", fleetv1.ActionPhasePaused, fleetv1.ActionPriorityNormal, fleetv1.RobotEstopNormal, false},
		{"revoking/normal/no-stop", fleetv1.ActionPhaseRevoking, fleetv1.ActionPriorityNormal, fleetv1.RobotEstopNormal, false},
		{"preempted/normal/no-stop", fleetv1.ActionPhasePreempted, fleetv1.ActionPriorityNormal, fleetv1.RobotEstopNormal, false},
		{"inprogress/high/no-stop", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityHigh, fleetv1.RobotEstopNormal, false},
		{"inprogress/normal/stopping", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, fleetv1.RobotEstopStopping, false},
		{"inprogress/normal/stopped", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, fleetv1.RobotEstopStopped, false},
		{"inprogress/normal/failed", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, fleetv1.RobotEstopFailed, false},
		{"inprogress/normal/resuming", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, fleetv1.RobotEstopResuming, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
			v := bandAction("norm", "r1", tc.phase, tc.prio, 2, lease)
			rob := robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm")
			rob.Status.EstopState = tc.estop
			r, _ := newActionReconciler(t, v, rob)
			if got := r.mayDisplace(context.Background(), v); got != tc.want {
				t.Errorf("mayDisplace = %v, want %v", got, tc.want)
			}
		})
	}
}

// The mark that follows a zone-capacity eviction checks row A11 again: a victim the stop paused
// between the eviction and the mark keeps its phase.
func TestPreemption_TheMarkChecksTheVictimAgain(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t, bandAction("norm", "r1", fleetv1.ActionPhasePaused, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"))
	r.markActionPreempted(context.Background(), actionNS, "norm", "crit")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePaused {
		t.Fatalf("INVARIANT: the mark drove a Paused victim to %s", v.Status.Phase)
	}
}
