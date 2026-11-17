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
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
)

// The cases the A24 diff review found (2026-11-16).

// A Critical action whose only Idle robot is held off does not preempt: that robot passed every
// filter, so a robot is eligible, and §9.4.2 preempts only when none is.
func TestReview_AHeldOffRobotIsNoReasonToPreempt(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		criticalPending("crit"),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""),
		bandAction("norm", "r2", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r2", fleetv1.RobotPhaseInProgress, "norm"),
	)
	r.Commander = &hookCommander{err: command.ErrNotRegistered}

	reconcileAction(t, r, "crit") // r1 is chosen, not sent, reverted, held off
	if rb := getRobot(t, c, "r1", actionNS); rb.Status.AssignedAction != "" {
		t.Fatalf("r1 records %q after the revert; want nothing", rb.Status.AssignedAction)
	}
	reconcileAction(t, r, "crit") // r1 held: no Idle robot is left, but r1 is eligible
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("norm is %s: a robot held off the preemptor made it preempt another robot's work", v.Status.Phase)
	}
	if rb := getRobot(t, c, "r2", actionNS); rb.Status.AssignedAction != "norm" {
		t.Fatalf("r2 records %q; want norm, untouched", rb.Status.AssignedAction)
	}
}

// putRobotToWork moves robot name to InProgress on action, as another writer would.
func putRobotToWork(t *testing.T, c client.Client, name, action string) {
	t.Helper()
	rb := getRobot(t, c, name, actionNS)
	base := rb.DeepCopy()
	rb.Status.Phase, rb.Status.AssignedAction = fleetv1.RobotPhaseInProgress, action
	if err := c.Status().Patch(context.Background(), rb, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

// A held robot that has since gone to other work is not an eligible robot (it is not Idle), so it
// does not stop a Critical action from preempting another robot's lower-band work (§9.4.2). It is
// not preempted for the action it is held off.
func TestReview_AHeldRobotThatIsNotIdleDoesNotStopAPreemption(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		criticalPending("crit"),
		robotInPhase("r1", fleetv1.RobotPhaseIdle, ""),
		bandAction("norm", "r2", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease),
		robotInPhase("r2", fleetv1.RobotPhaseInProgress, "norm"),
	)
	hc := &hookCommander{err: command.ErrNotRegistered}
	r.Commander = hc

	reconcileAction(t, r, "crit") // r1 is chosen, not sent, reverted, held off
	if rb := getRobot(t, c, "r1", actionNS); rb.Status.AssignedAction != "" {
		t.Fatalf("r1 records %q after the revert; want nothing", rb.Status.AssignedAction)
	}
	putRobotToWork(t, c, "r1", "other") // still held off crit, and no longer Idle
	hc.err = nil

	reconcileAction(t, r, "crit")
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Fatalf("norm is %s; want Preempted: no robot is Idle, and a held robot that is not Idle "+
			"is no reason to wait", v.Status.Phase)
	}
	if crit := getAction(t, c, "crit"); crit.Status.AssignedRobot != "r2" {
		t.Fatalf("crit is assigned to %q; want r2", crit.Status.AssignedRobot)
	}
	if rb := getRobot(t, c, "r1", actionNS); rb.Status.AssignedAction != "other" {
		t.Fatalf("r1 records %q; want other, untouched", rb.Status.AssignedAction)
	}
}

// A held robot that is not Idle is not why nothing was dispatched: the tally counts it as not Idle,
// and the revert's message is not added.
func TestReview_AHeldRobotThatIsNotIdleIsTalliedNotBlamed(t *testing.T) {
	h := holdOffRig(t, nil)
	h.passes(t, 1) // r1 refused as unavailable, reverted, held off
	putRobotToWork(t, h.c, "r1", "other")
	h.passes(t, 1)
	m := getAction(t, h.c, "t1").Status.Message
	if !strings.Contains(m, "not Idle") || strings.Contains(m, "adapter rejected the assignment") {
		t.Fatalf("message %q; want the tally counting r1 as not Idle, and no revert message", m)
	}
}

// With only a held robot left, the revert's message stands and the narrowing note is kept.
func TestReview_AHeldPassKeepsTheNarrowingNote(t *testing.T) {
	h := holdOffRig(t, func(rb *fleetv1.Robot) { rb.Spec.Zone, rb.Status.CurrentZone = "z-a", "z-a" })
	ctx := context.Background()
	if err := h.c.Delete(ctx, &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: actionNS}}); err != nil {
		t.Fatal(err)
	}
	for _, z := range []fleetv1.FleetZone{zrZone("z", ""), zrZone("z-a", "z"),
		zrZone("z-b", "z", entryDenied("z-b", "closed"))} {
		z.Namespace = actionNS
		if err := h.c.Create(ctx, &z); err != nil {
			t.Fatal(err)
		}
	}
	h.passes(t, 2) // r1 refused and held; nothing else in the namespace
	m := getAction(t, h.c, "t1").Status.Message
	if !strings.HasPrefix(m, "adapter rejected the assignment; rescheduling") || !strings.Contains(m, `zone "z" was narrowed`) {
		t.Fatalf("message %q; want the revert's message, then the narrowing note", m)
	}
}

// A robot another writer moved out of a work phase since the commit (Error here) keeps that phase:
// the revert clears its record and does not give it back to the victim as InProgress.
func TestReview_TheRevertLeavesAnErrorPhaseAndRestoresNoVictimOntoIt(t *testing.T) {
	r, c := preemptionRig(t)
	r.Commander = &hookCommander{err: command.ErrNotRegistered, hook: func(ctx context.Context) error {
		rb := &fleetv1.Robot{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: "r1"}, rb); err != nil {
			return err
		}
		base := rb.DeepCopy()
		rb.Status.Phase = fleetv1.RobotPhaseError
		return c.Status().Patch(ctx, rb, client.MergeFrom(base))
	}}

	reconcileAction(t, r, "crit")

	rb := getRobot(t, c, "r1", actionNS)
	if rb.Status.Phase != fleetv1.RobotPhaseError || rb.Status.AssignedAction != "" {
		t.Fatalf("robot r1 is %s recording %q; want Error kept and the record cleared", rb.Status.Phase, rb.Status.AssignedAction)
	}
}

// The pause's own write losing a conflict ends the attempt with an error, and a later attempt that
// finds the stop gone does not free the robot behind the action's back: the action stays Assigned,
// the robot keeps its record, and the pass returns the error.
func TestReview_APauseConflictIsReturnedNotRetriedIntoARelease(t *testing.T) {
	r, c, _ := refusalRig(t)
	// The robot's own stop is recorded while the AssignAction is refused at the send.
	pushed := false
	r.Commander = &hookCommander{err: command.ErrNotRegistered, hook: func(ctx context.Context) error {
		pushed = true
		return estopStateOn(c, fleetv1.RobotEstopStopping)(ctx)
	}}
	conflicted := false
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if _, isAction := obj.(*fleetv1.FleetAction); isAction && pushed && !conflicted {
				conflicted = true
				// The stop is cleared at the same moment: a retry would find no stop and release.
				if err := estopStateOn(cl, fleetv1.RobotEstopNormal)(ctx); err != nil {
					return err
				}
				return apierrors.NewConflict(schema.GroupResource{Group: "swarmada.io", Resource: "fleetactions"},
					obj.GetName(), errors.New("test: the pause lost"))
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: "t1"}})
	if err == nil {
		t.Fatal("the pass returned no error after the pause's write lost")
	}
	if !conflicted {
		t.Fatal("the test's conflict was never raised")
	}
	if rb := refusalRobot(t, c); rb.Status.AssignedAction != "t1" {
		t.Fatalf("robot r1 records %q; want t1: a pause that lost must not turn into a release on a retry", rb.Status.AssignedAction)
	}
}

// A register processed while the revert runs (after the send was refused, before the hold is
// recorded) ends the hold: the next pass sends.
func TestReview_ARegisterDuringTheRevertEndsTheHold(t *testing.T) {
	h := holdOffRig(t, nil)
	h.fc.reject, h.fc.err = false, command.ErrNotRegistered
	h.fc.hook = func(context.Context) error { h.fc.epoch++; return nil } // the register lands mid-send
	h.passes(t, 1)
	h.fc.err, h.fc.hook = nil, nil
	h.passes(t, 1)
	if len(h.fc.assigns) != 2 {
		t.Fatalf("%d AssignActions; want 2: the register processed during the revert ends the hold", len(h.fc.assigns))
	}
}

// With other robots excluded too, a held pass writes the tally and the other exclusions, then the
// revert's message; with none, the revert's message alone. The string is the same on every pass.
func TestReview_AHeldPassKeepsTheOtherExclusionsInTheMessage(t *testing.T) {
	h := holdOffRig(t, nil)
	ctx := context.Background()
	r2 := idleRobotInZone("r2", "elsewhere")
	r2.UID = "uid-r2"
	if err := h.c.Create(ctx, r2); err != nil {
		t.Fatal(err)
	}
	r2.Status = idleRobotInZone("r2", "elsewhere").Status
	if err := h.c.Status().Update(ctx, r2); err != nil {
		t.Fatal(err)
	}
	h.passes(t, 2) // r1 refused and held; r2 is outside the action's zone (filter 2)
	m1 := getAction(t, h.c, "t1").Status.Message
	if !strings.Contains(m1, "not in the action's zone") || !strings.HasSuffix(m1, "adapter rejected the assignment; rescheduling") {
		t.Fatalf("message %q; want the tally naming filter 2, then the revert's message", m1)
	}
	if strings.Contains(strings.ToLower(m1), "held off") {
		t.Fatalf("message %q names the hold; it is not written to status", m1)
	}
	h.passes(t, 1)
	if m2 := getAction(t, h.c, "t1").Status.Message; m2 != m1 {
		t.Fatalf("message changed between held passes:\n%q\n%q", m1, m2)
	}
}

// The AssignmentNotSent Event is kept once per action, robot and reason: the same reason on a
// second robot is reported too.
func TestReview_TheNotSentEventIsPerRobot(t *testing.T) {
	var n notSentEvents
	key := types.NamespacedName{Namespace: actionNS, Name: "t1"}
	if !n.first(key, "r1", "x") || n.first(key, "r1", "x") || !n.first(key, "r2", "x") {
		t.Fatal("the not-sent memo does not key on the robot")
	}
}
