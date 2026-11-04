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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Two holds the dispatch choke point applies before filter 10 (dispatch_holds.go): one
// Robot per wire robot-id, and no work in a zone whose estop is in force. Each stands for
// a stop the robot's own estopState may not show.

func withWireID(r *fleetv1.Robot, id string) *fleetv1.Robot {
	r.Annotations = map[string]string{fleetv1.RobotIDAnnotation: id}
	return r
}

func eligibleNames(robots []fleetv1.Robot) []string {
	out := []string{}
	for i := range robots {
		out = append(out, robots[i].Name)
	}
	return out
}

func withheldReason(withheld []dispatchExclusion, robot string) string {
	for _, e := range withheld {
		if e.Robot == robot {
			return e.Reason
		}
	}
	return ""
}

// Two Robots carry one wire id, one stopped and its twin Idle. The twin used to pass
// every filter, and the assign went out under the stopped robot's id. Neither may be offered work, whichever holds the stop.
func TestDispatch_TwinsOnOneWireIDGetNoWork(t *testing.T) {
	a := withWireID(robotInPhase("dup-a", fleetv1.RobotPhaseIdle, ""), "wire-x")
	a.Status.EstopState = fleetv1.RobotEstopStopped
	b := withWireID(robotInPhase("dup-b", fleetv1.RobotPhaseIdle, ""), "wire-x")
	solo := withWireID(robotInPhase("solo", fleetv1.RobotPhaseDiscovered, ""), "wire-y")
	action := pendingInBand("t1", fleetv1.ActionPriorityNormal)
	r, c := newActionReconciler(t, a, b, solo, action)

	eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(), action,
		[]fleetv1.Robot{*a, *b, *solo})
	for _, e := range eligible {
		if e.Name == "dup-a" || e.Name == "dup-b" {
			t.Fatalf("a robot whose wire id another Robot carries is eligible: eligible=%v withheld=%+v",
				eligibleNames(eligible), withheld)
		}
	}
	if why := withheldReason(withheld, "dup-b"); !strings.Contains(why, `robot-id "wire-x"`) ||
		!strings.Contains(why, "dup-a, dup-b") {
		t.Errorf("the Normal twin's exclusion should name the id and both holders; got %q", why)
	}
	if why := withheldReason(withheld, "solo"); strings.Contains(why, "robot-id") {
		t.Errorf("a robot alone on its id must not be withheld for it; got %q", why)
	}

	reconcileAction(t, r, "t1")
	got := getAction(t, c, "t1")
	if got.Status.AssignedRobot != "" {
		t.Fatalf("the action was assigned to %q; neither twin may receive work", got.Status.AssignedRobot)
	}
	if !strings.Contains(got.Status.Message, "robot-id") {
		t.Errorf("the no-candidate message should name the robot-id hold; got %q", got.Status.Message)
	}
}

// The robot-id hold reads the Robot list itself, and a list that fails withholds rather
// than guessing that the id is unique.
func TestDispatch_RobotIDHoldFailsClosedOnAnUnreadableList(t *testing.T) {
	r, _ := newActionReconciler(t)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*fleetv1.RobotList); ok {
				return errors.New("etcd unavailable")
			}
			return c.List(ctx, list, opts...)
		},
	})
	robot := withWireID(robotInPhase("r1", fleetv1.RobotPhaseIdle, ""), "wire-1")
	eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(),
		pendingInBand("t1", fleetv1.ActionPriorityNormal), []fleetv1.Robot{*robot})
	if len(eligible) != 0 {
		t.Fatalf("an unreadable Robot list must withhold the robot; eligible=%v", eligibleNames(eligible))
	}
	if why := withheldReason(withheld, "r1"); !strings.Contains(why, "etcd unavailable") {
		t.Errorf("the exclusion should carry the read error; got %q", why)
	}
}

// The RobotIDConflict condition goes on both twins, on the transition only, and clears on
// the survivor once the other is deleted.
func TestRobotIDConflict_ConditionOnBothTwinsUntilOneIsGone(t *testing.T) {
	sc := runtime.NewScheme()
	if err := fleetv1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	a := withWireID(robotInPhase("dup-a", fleetv1.RobotPhaseIdle, ""), "wire-x")
	b := withWireID(robotInPhase("dup-b", fleetv1.RobotPhaseIdle, ""), "wire-x")
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(a, b).
		WithStatusSubresource(&fleetv1.Robot{}).Build()
	r := &RobotReconciler{Client: c, Scheme: sc}
	ctx := context.Background()
	reconcile := func(name string) *fleetv1.Robot {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: actionNS, Name: name}}); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
		got := &fleetv1.Robot{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	cond := func(rb *fleetv1.Robot) *metav1.Condition {
		for i := range rb.Status.Conditions {
			if rb.Status.Conditions[i].Type == "RobotIDConflict" {
				return &rb.Status.Conditions[i]
			}
		}
		return nil
	}

	for _, name := range []string{"dup-a", "dup-b"} {
		got := reconcile(name)
		cc := cond(got)
		if cc == nil || cc.Status != metav1.ConditionTrue {
			t.Fatalf("%s: RobotIDConflict = %+v, want True", name, cc)
		}
		if !strings.Contains(cc.Message, "dup-a, dup-b") || !strings.Contains(cc.Message, `"wire-x"`) {
			t.Errorf("%s: the condition should name the id and both Robots; got %q", name, cc.Message)
		}
	}

	// RA-1: an unchanged conflict writes nothing.
	before := reconcile("dup-a").ResourceVersion
	if after := reconcile("dup-a").ResourceVersion; after != before {
		t.Errorf("an unchanged conflict rewrote the status (resourceVersion %s -> %s)", before, after)
	}

	if err := c.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	if cc := cond(reconcile("dup-a")); cc == nil || cc.Status != metav1.ConditionFalse {
		t.Fatalf("once its twin is gone, dup-a's RobotIDConflict = %+v, want False", cc)
	}
}

// zoneHoldFixture is a namespace with the zone tree site → floor → dock and a clear zone
// yard; floor's estop is in force, and dock is below it.
func zoneHoldFixture() []client.Object {
	zone := func(name, parent string, st fleetv1.ZoneEstopStatus) *fleetv1.FleetZone {
		return &fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Namespace: actionNS, Name: name},
			Spec:       fleetv1.FleetZoneSpec{ParentZone: parent},
			Status:     fleetv1.FleetZoneStatus{EstopStatus: st},
		}
	}
	return []client.Object{
		zone("site", "", fleetv1.ZoneEstopChildTriggered),
		zone("floor", "site", fleetv1.ZoneEstopTriggered),
		zone("dock", "floor", fleetv1.ZoneEstopTriggered),
		zone("yard", "site", fleetv1.ZoneEstopClear),
	}
}

func robotPlaced(name, specZone, currentZone string) *fleetv1.Robot {
	r := robotInPhase(name, fleetv1.RobotPhaseIdle, "")
	r.Spec.Zone = specZone
	r.Status.CurrentZone = currentZone
	return r
}

// A zone's estop gates dispatch by itself, whatever the robot's own estopState says: the
// robot's zone or any parentZone ancestor of it, read from status.currentZone and from
// spec.zone. A ChildTriggered zone is a notification and stops nothing.
func TestDispatch_NoWorkInAStoppedZone(t *testing.T) {
	cases := []struct {
		name          string
		robot         *fleetv1.Robot
		withheld      bool
		reasonMention string
	}{
		{"created into the stopped zone (spec.zone only)", robotPlaced("fresh", "floor", ""), true, `"floor"`},
		{"positioned in the stopped zone", robotPlaced("here", "yard", "floor"), true, `"floor"`},
		{"declared in the stopped zone, positioned elsewhere", robotPlaced("away", "floor", "yard"), true, `"floor"`},
		{"in a zone below the stopped one", robotPlaced("below", "dock", "dock"), true, `"dock"`},
		{"in a clear zone under a ChildTriggered root", robotPlaced("clear", "yard", "yard"), false, ""},
		{"in a zone no FleetZone names", robotPlaced("nowhere", "ghost", ""), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(zoneHoldFixture(), tc.robot)
			r, _ := newActionReconciler(t, objs...)
			eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(),
				pendingInBand("t1", fleetv1.ActionPriorityNormal), []fleetv1.Robot{*tc.robot})
			why := withheldReason(withheld, tc.robot.Name)
			if tc.withheld {
				if len(eligible) != 0 {
					t.Fatalf("robot %s is eligible; a zone with an estop in force must withhold it", tc.robot.Name)
				}
				if !strings.Contains(why, tc.reasonMention) || !strings.Contains(why, "estop") {
					t.Errorf("the exclusion should name the stopped zone %s; got %q", tc.reasonMention, why)
				}
				return
			}
			if len(eligible) != 1 {
				t.Fatalf("robot %s should be dispatchable; withheld: %q", tc.robot.Name, why)
			}
		})
	}
}

// An estop requested on a zone holds new work in the moment before its fan-out writes
// the zone's status.
func TestDispatch_ARequestedZoneEstopHoldsBeforeItsFanOut(t *testing.T) {
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: actionNS, Name: "bay",
		Annotations: map[string]string{annEstopTriggered: "now"}}}
	robot := robotPlaced("r1", "bay", "")
	r, _ := newActionReconciler(t, zone, robot)
	eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(),
		pendingInBand("t1", fleetv1.ActionPriorityNormal), []fleetv1.Robot{*robot})
	if len(eligible) != 0 {
		t.Fatalf("a robot in a zone with a requested estop is eligible; withheld=%+v", withheld)
	}
}

// Composition is not containment: a composite whose status reads Triggered (an estop on a
// composite is refused, FZ-C5, so this is a forged status) carries no stop to the robots
// of its members. The walk follows parentZone only.
func TestDispatch_ZoneStopDoesNotWalkCompositeMembership(t *testing.T) {
	member := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: actionNS, Name: "aisle"}}
	composite := &fleetv1.FleetZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: actionNS, Name: "all-aisles"},
		Spec:       fleetv1.FleetZoneSpec{MemberZones: []string{"aisle"}},
		Status:     fleetv1.FleetZoneStatus{EstopStatus: fleetv1.ZoneEstopTriggered},
	}
	robot := robotPlaced("r1", "aisle", "aisle")
	r, _ := newActionReconciler(t, member, composite, robot)
	eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(),
		pendingInBand("t1", fleetv1.ActionPriorityNormal), []fleetv1.Robot{*robot})
	if len(eligible) != 1 {
		t.Fatalf("composite membership carried a stop to a member's robot; withheld=%+v", withheld)
	}
}

// A FleetZone list that cannot be read withholds every candidate: whether a zone estop
// holds the robot is unknown.
func TestDispatch_ZoneStopFailsClosedOnAnUnreadableZoneList(t *testing.T) {
	robot := robotPlaced("r1", "", "")
	r, _ := newActionReconciler(t, robot)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*fleetv1.FleetZoneList); ok {
				return errors.New("zones unavailable")
			}
			return c.List(ctx, list, opts...)
		},
	})
	eligible, withheld, _, _ := r.filterDispatchEligible(context.Background(),
		pendingInBand("t1", fleetv1.ActionPriorityNormal), []fleetv1.Robot{*robot})
	if len(eligible) != 0 {
		t.Fatalf("an unreadable FleetZone list must withhold the robot")
	}
	if why := withheldReason(withheld, "r1"); !strings.Contains(why, "zones unavailable") {
		t.Errorf("the exclusion should carry the read error; got %q", why)
	}
}

// inForceZone is a zone whose estop has fanned out and is in force: the trigger, the
// matching processed marker, and the fan-out's stamp.
func inForceZone(name, parent string, policy *fleetv1.ZoneEstopPolicy) *fleetv1.FleetZone {
	z := zeZone(name, parent, policy, "e1")
	z.Annotations[annEstopProcessed] = "e1"
	at := metav1.Now()
	z.Status = fleetv1.FleetZoneStatus{EstopStatus: fleetv1.ZoneEstopTriggered, LastEstopAt: &at}
	return z
}

// A robot that comes into the scope of a zone estop after its fan-out, created into the
// zone or moved into it, is stopped through the confirmed per-robot primitive. A robot the
// stop already holds is not stopped again, and a reconcile with nobody new does nothing.
func TestZoneEstop_StopsARobotThatArrivesAfterTheFanOut(t *testing.T) {
	est := &fakeEstopper{}
	held := zeRobot("held", "floor-1")
	held.Status.EstopState = fleetv1.RobotEstopStopped
	// The fan-out reached it: the stop's record names the zone (RFC-0001 #safety-robot-stop-table, S1).
	held.Status.Stops = []fleetv1.RobotStop{{ID: "e1-held", Scope: fleetv1.StopScopeZone, SentAt: metav1.Now(),
		Carrier: &fleetv1.StopCarrier{Kind: fleetv1.StopCarrierFleetZone, Name: "floor-1"}}}
	r, c := newZEReconciler(t, est, nil,
		inForceZone("floor-1", "", nil),
		zeZone("dock-1", "floor-1", nil, ""),
		zeZone("yard", "", nil, ""),
		held,
		zeRobot("created-in", ""),
		zeRobot("elsewhere", "yard"),
	)
	ctx := context.Background()
	fresh := &fleetv1.Robot{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: zeNS, Name: "created-in"}, fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Spec.Zone = "dock-1" // created into a zone below the stopped one, no position yet
	if err := c.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	reconcileZE(t, r, "floor-1")
	if got := est.names(); len(got) != 1 || got[0] != "created-in" {
		t.Fatalf("stopped %v, want [created-in]: the late arrival, not the held robot or the one elsewhere", got)
	}

	reconcileZE(t, r, "floor-1")
	if got := est.names(); len(got) != 1 {
		t.Fatalf("a second reconcile stopped again: %v", got)
	}

	// Moved in: the robot in the yard is placed in the stopped zone.
	moved := &fleetv1.Robot{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: zeNS, Name: "elsewhere"}, moved); err != nil {
		t.Fatal(err)
	}
	moved.Status.CurrentZone = "floor-1"
	if err := c.Status().Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	reconcileZE(t, r, "floor-1")
	if got := est.names(); len(got) != 2 || got[1] != "elsewhere" {
		t.Fatalf("stopped %v, want the moved robot added", got)
	}
}

// A zone whose estop does not propagate to its children does not stop a robot that
// arrives in a child zone; the dispatch hold withholds it all the same.
func TestZoneEstop_LateArrivalFollowsPropagateToChildren(t *testing.T) {
	est := &fakeEstopper{}
	r, _ := newZEReconciler(t, est, nil,
		inForceZone("floor-1", "", &fleetv1.ZoneEstopPolicy{PropagateToChildren: apiBool(false)}),
		zeZone("dock-1", "floor-1", nil, ""),
		zeRobot("in-child", "dock-1"),
	)
	reconcileZE(t, r, "floor-1")
	if got := est.names(); len(got) != 0 {
		t.Fatalf("stopped %v; propagateToChildren=false keeps the stop off the child zone", got)
	}
}

// The status projection re-derives every zone in the namespace from the cache. A reconcile of
// one zone that reads another zone's copy from before its fan-out finished (status written,
// marker not yet) must not write that stale derivation over it: the zone would read Clear,
// lose its stamp, and never read Triggered again while its stop stays in force.
func TestZoneEstop_StaleCopyDoesNotClearAnotherZonesStop(t *testing.T) {
	est := &fakeEstopper{}
	r, c := newZEReconciler(t, est, nil,
		inForceZone("bay-1", "", nil),
		zeZone("bay-2", "", nil, ""),
	)
	ctx := context.Background()
	live := &fleetv1.FleetZone{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: zeNS, Name: "bay-1"}, live); err != nil {
		t.Fatal(err)
	}
	stale := live.DeepCopy()
	delete(stale.Annotations, annEstopProcessed) // the marker write has not reached this copy
	stale.ResourceVersion = "1"
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := cl.List(ctx, list, opts...); err != nil {
				return err
			}
			if zl, ok := list.(*fleetv1.FleetZoneList); ok {
				for i := range zl.Items {
					if zl.Items[i].Name == "bay-1" {
						zl.Items[i] = *stale.DeepCopy()
					}
				}
			}
			return nil
		},
	})
	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: zeNS, Name: "bay-2"}})
	got := &fleetv1.FleetZone{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: zeNS, Name: "bay-1"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.EstopStatus != fleetv1.ZoneEstopTriggered || got.Status.LastEstopAt == nil {
		t.Fatalf("a stale copy cleared bay-1's stop: estopStatus=%q lastEstopAt=%v",
			got.Status.EstopStatus, got.Status.LastEstopAt)
	}
}
