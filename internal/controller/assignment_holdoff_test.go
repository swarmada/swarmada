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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
)

// The hold-off (assignment_holdoff.go): after A24 reverts an assignment that was not sent or was
// refused as ROBOT_UNAVAILABLE, the robot is not offered that action again for a while. The case it
// exists for: an action whose only robot refuses it, offered 416 times in 91 s on 7d1cd19.

type holdOffRigT struct {
	r     *FleetActionReconciler
	c     client.Client
	fc    *hookCommander
	tde   *fakeTDE
	rec   *objectRecorder
	clock time.Time
}

// holdOffRig is robot r1 (UID uid-1) alone in zone z, a Pending action t1 to z, a TDE that grants,
// and a commander that refuses every AssignAction as ROBOT_UNAVAILABLE.
func holdOffRig(t *testing.T, robot func(*fleetv1.Robot)) *holdOffRigT {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r1 := idleRobotInZone("r1", "z")
	r1.UID = "uid-1"
	if robot != nil {
		robot(r1)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(readyActionAdapter(), stopZone("z", "", false, false, nil),
			&fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: SwarmadaConfigName, Namespace: actionNS}},
			r1, zonedAction("t1", "z")).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).Build()
	h := &holdOffRigT{c: c, fc: &hookCommander{reject: true}, tde: &fakeTDE{result: tde.ReservationResult{Status: tde.Granted}},
		rec: &objectRecorder{}, clock: time.Now()}
	h.r = &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler(), Commander: h.fc,
		TDE: h.tde, Recorder: h.rec}
	h.r.holdOffs.clock = func() time.Time { return h.clock }
	return h
}

func (h *holdOffRigT) passes(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		reconcileAction(t, h.r, "t1")
	}
}

// want checks the pushes, TDE requests and generation so far.
func (h *holdOffRigT) want(t *testing.T, when string, pushes, requests int, gen int64) {
	t.Helper()
	a := getAction(t, h.c, "t1")
	if len(h.fc.assigns) != pushes || h.tde.requests != requests || a.Status.AssignmentGeneration != gen {
		t.Fatalf("%s: %d AssignActions, %d TDE requests, generation %d; want %d, %d, %d",
			when, len(h.fc.assigns), h.tde.requests, a.Status.AssignmentGeneration, pushes, requests, gen)
	}
}

// A refusal as ROBOT_UNAVAILABLE holds r1 off t1: passes inside the hold mint no generation, ask the
// TDE nothing and send nothing, however many there are. Once the hold is over r1 is offered once;
// a second refusal doubles the hold. The hold is reported as an Event, and never in status.
func TestHoldOff_ARefusalHoldsTheRobotOffAndMintsNothing(t *testing.T) {
	h := holdOffRig(t, nil)

	h.passes(t, 1)
	gen := getAction(t, h.c, "t1").Status.AssignmentGeneration
	if gen < 1 {
		t.Fatalf("generation %d after the first commit", gen)
	}
	h.want(t, "the first pass", 1, 1, gen)
	if h.tde.releases < 1 {
		t.Fatalf("%d TDE releases after the revert; want the reservation released", h.tde.releases)
	}

	// What the action's own writes and the robot's return to Idle would trigger, many times over.
	h.passes(t, 20)
	h.want(t, "20 passes inside the hold", 1, 1, gen)
	a := getAction(t, h.c, "t1")
	if a.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("phase %s inside the hold, want Pending", a.Status.Phase)
	}
	if strings.Contains(strings.ToLower(a.Status.Message), "held off") || strings.Contains(a.Status.Message, "no Robot resources") {
		t.Fatalf("status.message %q: the hold is not written to status, and the tally must not misstate the fleet", a.Status.Message)
	}
	if a.Status.Message != "adapter rejected the assignment; rescheduling" {
		t.Errorf("status.message %q; want the revert's own message to stand", a.Status.Message)
	}
	if rb := getRobot(t, h.c, "r1", actionNS); rb.Status.AssignedAction != "" || rb.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot r1 records %q in phase %s inside the hold; want Idle and nothing", rb.Status.AssignedAction, rb.Status.Phase)
	}
	evs := h.rec.on("FleetAction", "t1", EventRobotHeldOff)
	if len(evs) != 1 || !strings.Contains(evs[0].message, "r1") || !strings.Contains(evs[0].message, "1m0s") ||
		!strings.Contains(evs[0].message, "ROBOT_UNAVAILABLE") {
		t.Fatalf("RobotHeldOff Events %+v; want one naming r1, the 1m0s wait and the refusal", evs)
	}

	// The hold is over: one probe, refused again, and the next hold is twice as long.
	h.clock = h.clock.Add(holdOffFloor + time.Second)
	h.passes(t, 5)
	gen2 := getAction(t, h.c, "t1").Status.AssignmentGeneration
	if gen2 <= gen {
		t.Fatalf("generation %d after the probe, want above %d", gen2, gen)
	}
	h.want(t, "after the first hold", 2, 2, gen2)
	h.clock = h.clock.Add(holdOffFloor + time.Second) // inside the second hold (2m)
	h.passes(t, 5)
	h.want(t, "inside the second hold", 2, 2, gen2)
	if evs := h.rec.on("FleetAction", "t1", EventRobotHeldOff); len(evs) != 2 || !strings.Contains(evs[1].message, "2m0s") {
		t.Fatalf("RobotHeldOff Events %+v; want a second, for 2m0s", evs)
	}
}

// The hold doubles to holdOffCap and stays there: a robot that never takes the action is offered
// it once per cap. Over an hour that is at most 1 + 60 min/holdOffCap + the doubling steps.
func TestHoldOff_TheHoldIsCapped(t *testing.T) {
	h := holdOffRig(t, nil)
	h.fc.reject, h.fc.err = false, command.ErrNotRegistered
	end := h.clock.Add(time.Hour)
	for h.clock.Before(end) {
		h.passes(t, 1)
		h.clock = h.clock.Add(10 * time.Second)
	}
	// 1m, 2m, 4m, 8m, then 10m each: offers at 0, 1, 3, 7, 15, 25, 35, 45, 55 min.
	if n := len(h.fc.assigns); n < 8 || n > 10 {
		t.Fatalf("%d AssignActions in an hour for a robot that never registers; want 9 (the hold doubles to %s)", n, holdOffCap)
	}
}

// Other refusals hold nothing: ROBOT_BUSY is a fact about the moment (dispatch_operating_declines.go).
func TestHoldOff_OtherRefusalsHoldNothing(t *testing.T) {
	h := holdOffRig(t, nil)
	h.fc.reject = false
	h.fc.assignReject = true // ROBOT_BUSY
	h.r.Commander = &h.fc.fakeCommander
	h.passes(t, 1)
	h.clock = h.clock.Add(time.Second)
	h.passes(t, 1)
	if n := len(h.fc.assigns); n != 2 {
		t.Fatalf("%d AssignActions after two passes on ROBOT_BUSY; want 2: only not-sent and unavailable hold", n)
	}
	if evs := h.rec.on("FleetAction", "t1", EventRobotHeldOff); len(evs) != 0 {
		t.Fatalf("RobotHeldOff Events %+v on ROBOT_BUSY; want none", evs)
	}
}

// Each reset offers r1 at once: a register processed since the hold, its Ready condition turning
// True since, or another robot of the same name (the first one deleted).
func TestHoldOff_ResetsOfferTheRobotAtOnce(t *testing.T) {
	setReady := func(c client.Client, status metav1.ConditionStatus, at time.Time) error {
		rb := &fleetv1.Robot{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: "r1"}, rb); err != nil {
			return err
		}
		base := rb.DeepCopy()
		rb.Status.Conditions = []metav1.Condition{{Type: conditionTypeReady, Status: status, Reason: "Test",
			LastTransitionTime: metav1.NewTime(at)}}
		return c.Status().Patch(context.Background(), rb, client.MergeFrom(base))
	}
	readyFalse := func(rb *fleetv1.Robot) {
		rb.Status.Conditions = []metav1.Condition{{Type: conditionTypeReady, Status: metav1.ConditionFalse, Reason: "Test",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour))}}
	}
	for _, tc := range []struct {
		name  string
		robot func(*fleetv1.Robot)
		reset func(t *testing.T, h *holdOffRigT)
	}{
		{"registered", nil, func(t *testing.T, h *holdOffRigT) { h.fc.epoch++ }},
		{"ready-again", readyFalse, func(t *testing.T, h *holdOffRigT) {
			if err := setReady(h.c, metav1.ConditionTrue, time.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"recreated", nil, func(t *testing.T, h *holdOffRigT) {
			ctx := context.Background()
			old := getRobot(t, h.c, "r1", actionNS)
			if err := h.c.Delete(ctx, old); err != nil {
				t.Fatal(err)
			}
			n := idleRobotInZone("r1", "z")
			n.UID = "uid-2"
			if err := h.c.Create(ctx, n); err != nil {
				t.Fatal(err)
			}
			n.Status = idleRobotInZone("r1", "z").Status
			if err := h.c.Status().Update(ctx, n); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := holdOffRig(t, tc.robot)
			h.passes(t, 2)
			if len(h.fc.assigns) != 1 {
				t.Fatalf("%d AssignActions before the reset; want 1 (held)", len(h.fc.assigns))
			}
			tc.reset(t, h)
			h.passes(t, 1)
			if len(h.fc.assigns) != 2 {
				t.Fatalf("%d AssignActions after the reset; want 2: the robot is offered again at once", len(h.fc.assigns))
			}
		})
	}
}

// What does not reset it: the robot Ready all along, its Ready condition unchanged, and another
// robot registering.
func TestHoldOff_AnUnchangedReadyRobotStaysHeld(t *testing.T) {
	h := holdOffRig(t, func(rb *fleetv1.Robot) {
		rb.Status.Conditions = []metav1.Condition{{Type: conditionTypeReady, Status: metav1.ConditionTrue, Reason: "Test",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour))}}
	})
	h.passes(t, 5)
	if len(h.fc.assigns) != 1 {
		t.Fatalf("%d AssignActions; want 1: a robot Ready all along is not Ready again", len(h.fc.assigns))
	}
}

// The hold is per (action, robot): another action is offered r1, and t1 goes to another robot.
func TestHoldOff_IsPerActionAndRobot(t *testing.T) {
	h := holdOffRig(t, nil)
	h.passes(t, 1) // t1 refused on r1, held
	ctx := context.Background()
	r2 := idleRobotInZone("r2", "z")
	r2.UID = "uid-r2"
	if err := h.c.Create(ctx, r2); err != nil {
		t.Fatal(err)
	}
	r2.Status = idleRobotInZone("r2", "z").Status
	if err := h.c.Status().Update(ctx, r2); err != nil {
		t.Fatal(err)
	}
	h.fc.reject = false // the adapter now accepts
	h.passes(t, 1)
	t1 := getAction(t, h.c, "t1")
	if t1.Status.AssignedRobot != "r2" {
		t.Fatalf("t1 assigned to %q; want r2, the robot not held off it", t1.Status.AssignedRobot)
	}
	if n := len(h.r.holdOffs.by[declineKey(t1)]); n != 0 {
		t.Fatalf("%d hold-off entries kept for t1 after r2 took it; want none (forgotten on an accepted send)", n)
	}
	if err := h.c.Create(ctx, zonedAction("t2", "z")); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, h.r, "t2")
	if a := getAction(t, h.c, "t2"); a.Status.AssignedRobot != "r1" {
		t.Fatalf("t2 assigned to %q; want r1: r1 is held off t1 only", a.Status.AssignedRobot)
	}
}

// A refusal under a stop reverts nothing (A24), so it holds nothing off either.
func TestHoldOff_ARefusalUnderAStopHoldsNothing(t *testing.T) {
	r, c, fc := refusalRig(t)
	fc.issue = estopStateOn(c, fleetv1.RobotEstopStopping)
	rec := &objectRecorder{}
	r.Recorder = rec
	reconcileAction(t, r, "t1")
	if evs := rec.on("FleetAction", "t1", EventRobotHeldOff); len(evs) != 0 {
		t.Fatalf("RobotHeldOff Events %+v for a refusal under a stop; want none", evs)
	}
	if n := len(r.holdOffs.by); n != 0 {
		t.Fatalf("%d actions in the hold-off memo after a refusal under a stop; want 0", n)
	}
}
