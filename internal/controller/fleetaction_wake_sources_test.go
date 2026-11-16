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
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// An adapter's liveness heartbeat reaches no action; everything else it writes does.
func TestAdapterChangeMatters(t *testing.T) {
	base := &fleetv1.FleetAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: actionNS, ResourceVersion: "10"},
		Status: fleetv1.FleetAdapterStatus{Phase: fleetv1.FleetAdapterPhaseConnected, Conformance: fleetv1.ConformanceStatePassed,
			LastHeartbeat: &metav1.Time{Time: time.Unix(100, 0)}},
	}
	with := func(edit func(*fleetv1.FleetAdapter)) *fleetv1.FleetAdapter {
		a := base.DeepCopy()
		a.ResourceVersion = "11"
		edit(a)
		return a
	}
	beat := func(a *fleetv1.FleetAdapter) { a.Status.LastHeartbeat = &metav1.Time{Time: time.Unix(103, 0)} }
	for _, tc := range []struct {
		name string
		new  *fleetv1.FleetAdapter
		want bool
	}{
		{"a heartbeat alone", with(beat), false},
		{"a resync (nothing changed)", with(func(*fleetv1.FleetAdapter) {}), false},
		{"a heartbeat that corrects a drifted phase", with(func(a *fleetv1.FleetAdapter) {
			beat(a)
			a.Status.Phase = fleetv1.FleetAdapterPhaseDegraded
		}), true},
		{"Disconnected", with(func(a *fleetv1.FleetAdapter) { a.Status.Phase = fleetv1.FleetAdapterPhaseDisconnected }), true},
		{"conformance", with(func(a *fleetv1.FleetAdapter) { a.Status.Conformance = fleetv1.ConformanceStateFailed }), true},
		{"the negotiated contract version", with(func(a *fleetv1.FleetAdapter) { a.Status.NegotiatedContractVersion = "1.4.0" }), true},
		{"connected robots", with(func(a *fleetv1.FleetAdapter) { a.Status.ConnectedRobots = 3 }), true},
		{"a heartbeat and a message", with(func(a *fleetv1.FleetAdapter) { beat(a); a.Status.Message = "adapter live" }), true},
		{"the spec", with(func(a *fleetv1.FleetAdapter) { a.Spec.ServesRobotClasses = []string{"amr"} }), true},
		{"an annotation", with(func(a *fleetv1.FleetAdapter) { a.Annotations = map[string]string{"x": "y"} }), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := adapterChangeMatters.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: tc.new}); got != tc.want {
				t.Fatalf("adapterChangeMatters.Update = %v, want %v", got, tc.want)
			}
		})
	}
	if !adapterChangeMatters.Create(event.CreateEvent{Object: base}) || !adapterChangeMatters.Delete(event.DeleteEvent{Object: base}) {
		t.Error("a created or deleted adapter must pass")
	}
	if adapterChangeMatters.Generic(event.GenericEvent{Object: base}) {
		t.Error("a generic event passed")
	}
}

// A zone holds dispatch through its trigger, its estop status, and its processed marker.
func TestZoneHeld(t *testing.T) {
	z := func(edit func(*fleetv1.FleetZone)) *fleetv1.FleetZone {
		zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "aisle-1", Namespace: actionNS}}
		edit(zone)
		return zone
	}
	for _, tc := range []struct {
		name string
		zone *fleetv1.FleetZone
		want bool
	}{
		{"no stop", z(func(*fleetv1.FleetZone) {}), false},
		{"its trigger", z(func(f *fleetv1.FleetZone) { f.Annotations = map[string]string{estopscope.TriggerAnnotation: "x"} }), true},
		{"its processed marker (the clear not yet applied)", z(func(f *fleetv1.FleetZone) {
			f.Annotations = map[string]string{estopscope.ProcessedAnnotation: "x"}
		}), true},
		{"its estop status Triggered", z(func(f *fleetv1.FleetZone) { f.Status.EstopStatus = fleetv1.ZoneEstopTriggered }), true},
	} {
		if got := zoneHeld(tc.zone); got != tc.want {
			t.Errorf("%s: zoneHeld = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Only the effective restrictions and their condition count as a restrictions change; the TDE's
// own status writes do not.
func TestZoneRestrictionsChangedAndSlotsFreed(t *testing.T) {
	base := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "aisle-1", Namespace: actionNS},
		Status: fleetv1.FleetZoneStatus{
			Reservations:            []fleetv1.ZoneReservation{{ActionID: "a1"}, {ActionID: "a2"}},
			CurrentConcurrentRobots: 2,
			Conditions: []metav1.Condition{{Type: fleetv1.ZoneCondRestrictionsResolved, Status: metav1.ConditionFalse,
				Reason: "Unresolved", LastTransitionTime: metav1.Unix(1, 0)}},
		}}
	with := func(edit func(*fleetv1.FleetZone)) *fleetv1.FleetZone {
		z := base.DeepCopy()
		edit(z)
		return z
	}
	if zoneRestrictionsChanged(base, with(func(z *fleetv1.FleetZone) { z.Status.Reservations = z.Status.Reservations[:1] })) {
		t.Error("a TDE release counted as a restrictions change")
	}
	if zoneRestrictionsChanged(base, with(func(z *fleetv1.FleetZone) { z.Status.Conditions[0].LastTransitionTime = metav1.Unix(2, 0) })) {
		t.Error("a condition's timestamp alone counted as a restrictions change")
	}
	if !zoneRestrictionsChanged(base, with(func(z *fleetv1.FleetZone) { z.Status.Conditions[0].Status = metav1.ConditionTrue })) {
		t.Error("the restrictions resolving did not count")
	}
	if !zoneRestrictionsChanged(base, with(func(z *fleetv1.FleetZone) {
		z.Status.Restrictions = &fleetv1.ZoneRestrictionsStatus{}
	})) {
		t.Error("the effective restrictions changing did not count")
	}
	for _, tc := range []struct {
		name string
		new  *fleetv1.FleetZone
		want int
	}{
		{"one reservation released", with(func(z *fleetv1.FleetZone) { z.Status.Reservations = z.Status.Reservations[:1] }), 1},
		{"two robots left the zone", with(func(z *fleetv1.FleetZone) { z.Status.CurrentConcurrentRobots = 0 }), 2},
		{"a reservation added", with(func(z *fleetv1.FleetZone) {
			z.Status.Reservations = append(z.Status.Reservations, fleetv1.ZoneReservation{ActionID: "a3"})
		}), 0},
		{"no change", base.DeepCopy(), 0},
	} {
		if got := zoneSlotsFreed(base, tc.new); got != tc.want {
			t.Errorf("%s: zoneSlotsFreed = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// wakeFixture is a reconciler over a fake API server holding objs, with its wake handlers.
func wakeFixture(t *testing.T, objs ...client.Object) *FleetActionReconciler {
	t.Helper()
	scheme := wakeScheme(t)
	return &FleetActionReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), Scheme: scheme}
}

func drain(q workqueue.RateLimitingInterface) []string {
	var names []string
	for q.Len() > 0 {
		item, _ := q.Get()
		names = append(names, item.(reconcile.Request).Name)
		q.Done(item)
	}
	sort.Strings(names)
	return names
}

func runUpdate(r *FleetActionReconciler, h handler.EventHandler, old, new client.Object) []string {
	q := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	defer q.ShutDown()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: old, ObjectNew: new}, q)
	return drain(q)
}

func zoneObj2(name, parent string) *fleetv1.FleetZone {
	return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS}, Spec: fleetv1.FleetZoneSpec{ParentZone: parent}}
}

func pendingIn(name, zone string, age time.Duration, msg string) *fleetv1.FleetAction {
	a := wakeAction(actionNS, name, fleetv1.ActionPriorityNormal, age, fleetv1.ActionPhasePending)
	a.Spec.Zone = zone
	a.Status.Message = msg
	return a
}

func freeRobotIn(name, zone string) *fleetv1.Robot {
	r := wakeRobot(fleetv1.RobotPhaseIdle, "", "", zone)
	r.Name = name
	return r
}

// A zone's stop no longer holding wakes the actions held for their destination there, and the
// free robots there; judged on the processed marker as well as the trigger.
func TestZoneWakeHandler_StopLifted(t *testing.T) {
	r := wakeFixture(t, zoneObj2("site", ""), zoneObj2("aisle-1", "site"), zoneObj2("aisle-2", "site"),
		pendingIn("to-aisle-1", "aisle-1", time.Hour, "action held: its destination is in a zone under a stop"),
		pendingIn("to-aisle-2", "aisle-2", 50*time.Minute, ""),
		freeRobotIn("amr-1", "aisle-1"))
	held := zoneObj2("aisle-1", "site")
	held.Annotations = map[string]string{estopscope.ProcessedAnnotation: "x"}
	clear := zoneObj2("aisle-1", "site")

	triggerOnlyRemoved := held.DeepCopy()
	triggerOnlyRemoved.Annotations[estopscope.TriggerAnnotation] = "y"
	if got := runUpdate(r, r.zoneWakeHandler(), triggerOnlyRemoved, held); len(got) != 0 {
		t.Fatalf("the trigger's removal woke %v while the processed marker still holds", got)
	}
	// amr-1 in aisle-1 can take to-aisle-1 only (to-aisle-2 is another leaf), so both wakes give it.
	if got := runUpdate(r, r.zoneWakeHandler(), held, clear); !slices.Equal(got, []string{"to-aisle-1"}) {
		t.Fatalf("woke %v, want [to-aisle-1]", got)
	}
}

// A zone's restrictions changing wakes the actions on it, on its ancestors and below it, that the
// namespace's free robots could take.
func TestZoneWakeHandler_RestrictionsChanged(t *testing.T) {
	r := wakeFixture(t, zoneObj2("site", ""), zoneObj2("aisle-1", "site"), zoneObj2("bay-1", "aisle-1"), zoneObj2("aisle-2", "site"),
		pendingIn("on-site", "site", time.Hour, ""), pendingIn("on-aisle-1", "aisle-1", time.Hour, ""),
		pendingIn("on-bay-1", "bay-1", time.Hour, ""), pendingIn("on-aisle-2", "aisle-2", time.Hour, ""),
		pendingIn("unzoned", "", time.Hour, ""), freeRobotIn("amr-1", "bay-1"))
	old := zoneObj2("aisle-1", "site")
	new := old.DeepCopy()
	new.Status.Restrictions = &fleetv1.ZoneRestrictionsStatus{}
	want := []string{"on-aisle-1", "on-bay-1", "on-site"}
	if got := runUpdate(r, r.zoneWakeHandler(), old, new); !slices.Equal(got, want) {
		t.Fatalf("woke %v, want %v", got, want)
	}
}

// Slots the TDE frees in a zone wake the actions awaiting its capacity, most urgent first, up to
// wakeLimit per slot.
func TestZoneWakeHandler_SlotsFreed(t *testing.T) {
	objs := []client.Object{zoneObj2("aisle-1", ""), zoneObj2("aisle-2", ""),
		pendingIn("other-zone", "aisle-2", 2*time.Hour, awaitingZoneCapacity),
		pendingIn("other-reason", "aisle-1", 2*time.Hour, "no eligible robot")}
	for i := range 5 {
		objs = append(objs, pendingIn(fmt.Sprintf("wait-%d", i), "aisle-1", time.Duration(60-i)*time.Minute, awaitingZoneCapacity))
	}
	old := zoneObj2("aisle-1", "")
	old.Status.Reservations = []fleetv1.ZoneReservation{{ActionID: "x"}}
	new := zoneObj2("aisle-1", "")

	r := wakeFixture(t, objs...)
	if got := runUpdate(r, r.zoneWakeHandler(), old, new); len(got) != 0 {
		t.Fatalf("a release in a zone this process saw no denial in woke %v", got)
	}
	r.capacityWaiters.add(actionNS, "aisle-1", "wait-4") // denied in memory, its message not yet in the cache
	want := []string{"wait-0", "wait-1", "wait-2"}
	if got := runUpdate(r, r.zoneWakeHandler(), old, new); !slices.Equal(got, want) {
		t.Fatalf("woke %v, want %v", got, want)
	}

	// Waiters a robot wake already took for the same release count toward the slot's share, one per
	// slot at most, so fresh ones are still woken in case those fail on a gate before the TDE.
	r2 := wakeFixture(t, objs...)
	r2.capacityWaiters.add(actionNS, "aisle-1", "wait-0")
	for _, n := range []string{"wait-0", "wait-1"} {
		r2.woken.tryMark(types.NamespacedName{Namespace: actionNS, Name: n}, time.Now())
	}
	if got := runUpdate(r2, r2.zoneWakeHandler(), old, new); !slices.Equal(got, []string{"wait-2", "wait-3"}) {
		t.Fatalf("with two waiters already woken, woke %v, want [wait-2 wait-3]", got)
	}
}

// A waiter recorded before its denial's write reaches the cache is woken all the same.
func TestZoneWakeHandler_SlotsFreedBeforeTheDenialIsCached(t *testing.T) {
	r := wakeFixture(t, zoneObj2("aisle-1", ""), pendingIn("just-denied", "aisle-1", time.Minute, "no eligible robot"))
	r.capacityWaiters.add(actionNS, "aisle-1", "just-denied")
	old := zoneObj2("aisle-1", "")
	old.Status.CurrentConcurrentRobots = 1
	if got := runUpdate(r, r.zoneWakeHandler(), old, zoneObj2("aisle-1", "")); !slices.Equal(got, []string{"just-denied"}) {
		t.Fatalf("woke %v, want [just-denied]", got)
	}
	if r.capacityWaiters.any(actionNS, "aisle-1") {
		t.Error("a woken waiter stayed recorded")
	}
}

// A waiter that stopped waiting (failed by its deadline, cancelled, deleted) leaves the set at the
// zone's next release, so later releases in that zone list nothing.
func TestZoneWakeHandler_AWaiterThatStoppedWaitingLeaves(t *testing.T) {
	done := pendingIn("timed-out", "aisle-1", time.Hour, awaitingZoneCapacity)
	done.Status.Phase = fleetv1.ActionPhaseFailed
	r := wakeFixture(t, zoneObj2("aisle-1", ""), done)
	r.capacityWaiters.add(actionNS, "aisle-1", "timed-out")
	r.capacityWaiters.add(actionNS, "aisle-1", "deleted-meanwhile")
	old := zoneObj2("aisle-1", "")
	old.Status.CurrentConcurrentRobots = 1
	if got := runUpdate(r, r.zoneWakeHandler(), old, zoneObj2("aisle-1", "")); len(got) != 0 {
		t.Fatalf("woke %v", got)
	}
	if r.capacityWaiters.any(actionNS, "aisle-1") {
		t.Error("waiters that are no longer Pending stayed recorded")
	}
}

// A held zone deleted outright keeps its subtree for the wake: its children are woken too.
func TestZoneWakeHandler_HeldZoneDeleted(t *testing.T) {
	r := wakeFixture(t, zoneObj2("bay-1", "aisle-1"), freeRobotIn("amr-1", "bay-1"),
		pendingIn("to-bay-1", "bay-1", time.Hour, ""))
	held := zoneObj2("aisle-1", "")
	held.Annotations = map[string]string{estopscope.TriggerAnnotation: "x"}
	q := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	defer q.ShutDown()
	r.zoneWakeHandler().Delete(context.Background(), event.DeleteEvent{Object: held}, q)
	if got := drain(q); !slices.Equal(got, []string{"to-bay-1"}) {
		t.Fatalf("woke %v, want [to-bay-1]", got)
	}
}

// A namespace's stop no longer holding wakes its free robots together: distinct actions, up to
// wakeLimit per robot, not the same three for each.
func TestNamespaceWakeHandler_ManyRobotsAtOnce(t *testing.T) {
	var objs []client.Object
	for i := range 5 {
		objs = append(objs, freeRobotIn(fmt.Sprintf("amr-%d", i), ""))
	}
	for i := range 20 {
		objs = append(objs, pendingIn(fmt.Sprintf("act-%02d", i), "", time.Duration(100-i)*time.Minute, ""))
	}
	r := wakeFixture(t, objs...)
	held := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: "swarmada-config", Namespace: actionNS,
		Annotations: map[string]string{annEstopProcessed: "x"}}}
	clear := held.DeepCopy()
	clear.Annotations = nil
	got := runUpdate(r, r.namespaceWakeHandler(), held, clear)
	if len(got) != 5*wakeLimit {
		t.Fatalf("woke %d distinct actions for 5 robots, want %d: %v", len(got), 5*wakeLimit, got)
	}
	still := held.DeepCopy()
	still.Annotations = map[string]string{annEstopTriggered: "y"}
	fresh := wakeFixture(t, objs...)
	if got := runUpdate(fresh, fresh.namespaceWakeHandler(), still, held); len(got) != 0 {
		t.Fatalf("woke %v while the stop still holds", got)
	}
	// An edit to a config that held nothing before is not a lift.
	relabelled := clear.DeepCopy()
	relabelled.Labels = map[string]string{"edited": "yes"}
	fresh = wakeFixture(t, objs...)
	if got := runUpdate(fresh, fresh.namespaceWakeHandler(), clear, relabelled); len(got) != 0 {
		t.Fatalf("woke %v on an edit with no stop before or after", got)
	}
}

// A maintenance window releasing robots wakes them; one still holding them wakes nothing.
func TestMaintenanceWakeHandler(t *testing.T) {
	objs := []client.Object{freeRobotIn("amr-1", ""), freeRobotIn("amr-2", ""),
		pendingIn("a", "", 3*time.Hour, ""), pendingIn("b", "", 2*time.Hour, ""), pendingIn("c", "", time.Hour, "")}
	active := &fleetv1.ZoneMaintenance{ObjectMeta: metav1.ObjectMeta{Name: "mw", Namespace: actionNS},
		Status: fleetv1.ZoneMaintenanceStatus{Phase: fleetv1.ZoneMaintenancePhaseActive,
			PausedRobots: []fleetv1.PausedRobotEntry{{Name: "amr-1"}}}}
	done := active.DeepCopy()
	done.Status.Phase = fleetv1.ZoneMaintenancePhaseCompleted
	r := wakeFixture(t, objs...)
	if got := runUpdate(r, r.maintenanceWakeHandler(), active, done); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("woke %v, want [a b c]", got)
	}
	still := wakeFixture(t, objs...)
	if got := runUpdate(still, still.maintenanceWakeHandler(), active, active.DeepCopy()); len(got) != 0 {
		t.Fatalf("woke %v while the window still holds its robot", got)
	}
	// A window that lets one of two robots go wakes for that one only: wakeLimit actions, not two
	// robots' share.
	more := append([]client.Object{}, objs...)
	for i := range 4 {
		more = append(more, pendingIn(fmt.Sprintf("more-%d", i), "", time.Duration(50-i)*time.Minute, ""))
	}
	both := active.DeepCopy()
	both.Status.PausedRobots = append(both.Status.PausedRobots, fleetv1.PausedRobotEntry{Name: "amr-2"})
	one := both.DeepCopy()
	one.Status.PausedRobots = one.Status.PausedRobots[1:] // amr-1 let go, amr-2 still held
	partial := wakeFixture(t, more...)
	if got := runUpdate(partial, partial.maintenanceWakeHandler(), both, one); len(got) != wakeLimit {
		t.Fatalf("releasing one of two robots woke %d actions %v, want %d", len(got), got, wakeLimit)
	}
	q := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	defer q.ShutDown()
	r2 := wakeFixture(t, objs...)
	r2.maintenanceWakeHandler().Delete(context.Background(), event.DeleteEvent{Object: active}, q)
	if got := drain(q); len(got) != wakeLimit {
		t.Fatalf("deleting an Active window woke %v", got)
	}
}

// Robots freed one after another, faster than the queue drains, take distinct actions; an action
// woken again once its reconcile has started.
func TestWakeMemo_RobotsFreedOneAfterAnother(t *testing.T) {
	var objs []client.Object
	for i := range 8 {
		objs = append(objs, pendingIn(fmt.Sprintf("act-%d", i), "", time.Duration(100-i)*time.Minute, ""))
	}
	r := wakeFixture(t, objs...)
	names := func(reqs []reconcile.Request) []string {
		var out []string
		for _, q := range reqs {
			out = append(out, q.Name)
		}
		return out
	}
	first := names(r.pendingActionsFor(context.Background(), freeRobotIn("amr-1", ""), nil))
	second := names(r.pendingActionsFor(context.Background(), freeRobotIn("amr-2", ""), nil))
	if !slices.Equal(first, []string{"act-0", "act-1", "act-2"}) || !slices.Equal(second, []string{"act-3", "act-4", "act-5"}) {
		t.Fatalf("woke %v then %v, want the first three then the next three", first, second)
	}
	r.woken.forget(types.NamespacedName{Namespace: actionNS, Name: "act-0"})
	third := names(r.pendingActionsFor(context.Background(), freeRobotIn("amr-3", ""), nil))
	if !slices.Equal(third, []string{"act-0", "act-6", "act-7"}) {
		t.Fatalf("after act-0's reconcile started, woke %v, want [act-0 act-6 act-7]", third)
	}
	// An entry whose reconcile never came expires.
	key := types.NamespacedName{Namespace: actionNS, Name: "act-1"}
	if !r.woken.queued(key, time.Now()) || r.woken.queued(key, time.Now().Add(wakeMemoTTL)) {
		t.Error("the memo does not expire after wakeMemoTTL")
	}
}

// A requeue of an unstarted action is cut to just past its deadline; others are unchanged, and so
// is an action whose deadline has already passed or that is being deleted (clamping those would
// loop on a path that returns before the deadline check).
func TestUnstartedDeadlineAndClamp(t *testing.T) {
	in := func(phase fleetv1.ActionPhase, deadline *time.Time, deleting bool) *fleetv1.FleetAction {
		a := wakeAction(actionNS, "a", fleetv1.ActionPriorityNormal, time.Minute, phase)
		if deadline != nil {
			a.Spec.Deadline = &metav1.Time{Time: *deadline}
		}
		if deleting {
			a.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			a.Finalizers = []string{"test/keep"}
		}
		return a
	}
	soon, past := time.Now().Add(5*time.Second), time.Now().Add(-time.Second)
	for _, tc := range []struct {
		name   string
		action *fleetv1.FleetAction
		want   bool
	}{
		{"Pending, deadline ahead", in(fleetv1.ActionPhasePending, &soon, false), true},
		{"no phase yet, deadline ahead", in("", &soon, false), true},
		{"Pending, deadline passed", in(fleetv1.ActionPhasePending, &past, false), false},
		{"Pending, no deadline", in(fleetv1.ActionPhasePending, nil, false), false},
		{"Assigned, deadline ahead", in(fleetv1.ActionPhaseAssigned, &soon, false), false},
		{"Pending, deadline ahead, being deleted", in(fleetv1.ActionPhasePending, &soon, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := wakeFixture(t, tc.action)
			got := r.unstartedDeadline(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: "a"}})
			if (got != nil) != tc.want {
				t.Fatalf("unstartedDeadline = %v, want set %v", got, tc.want)
			}
		})
	}
	now := time.Now()
	if got := clampToDeadline(15*time.Second, now.Add(5*time.Second), now); got != 5*time.Second+deadlineRequeueMargin {
		t.Errorf("a 15 s requeue before a deadline 5 s out became %v", got)
	}
	if got := clampToDeadline(2*time.Second, now.Add(5*time.Second), now); got != 2*time.Second {
		t.Errorf("a requeue before the deadline became %v", got)
	}
}

// The recheck after every asked adapter was unreachable backs off: 2, 4, 8, then 15 s, and starts
// again after a minute without one.
func TestUnreachableBackoff(t *testing.T) {
	var u unreachableBackoff
	key := types.NamespacedName{Namespace: actionNS, Name: "a"}
	now := time.Now()
	var got []time.Duration
	for i := range 5 {
		got = append(got, u.next(key, now.Add(time.Duration(i)*time.Second)))
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("backoff %v, want %v", got, want)
	}
	if d := u.next(key, now.Add(2*time.Minute)); d != 2*time.Second {
		t.Fatalf("after a quiet minute the recheck is %v, want 2 s", d)
	}
	// An adapter that stays unreachable for a long time: the recheck stays at 15 s (never 0 or
	// negative, which would end the requeue for good).
	for i := range 100 {
		if d := u.next(key, now.Add(2*time.Minute+time.Duration(i+1)*15*time.Second)); d != 15*time.Second && i > 2 {
			t.Fatalf("try %d: recheck %v, want 15 s", i+4, d)
		}
	}
}

// Two wakes running at once never both take an action.
func TestWakeMemo_TryMarkIsOneStep(t *testing.T) {
	var w wakeMemo
	key := types.NamespacedName{Namespace: actionNS, Name: "a"}
	now := time.Now()
	if !w.tryMark(key, now) || w.tryMark(key, now) {
		t.Fatal("tryMark marked twice, or not at all")
	}
	w.forget(key)
	if !w.tryMark(key, now) {
		t.Fatal("tryMark refused an action whose reconcile has started")
	}
	if !w.tryMark(key, now.Add(wakeMemoTTL)) {
		t.Fatal("an entry whose reconcile never came did not expire")
	}
}

// A free robot whose adapter now honours a new rule wakes zoned actions only (filter 12 reads only
// those).
func TestWakeHandler_HonouredRulesWakeZonedActionsOnly(t *testing.T) {
	r := wakeFixture(t, zoneObj2("aisle-1", ""), pendingIn("zoned", "aisle-1", time.Hour, ""), pendingIn("unzoned", "", 2*time.Hour, ""))
	old := freeRobotIn("amr-1", "aisle-1")
	new := old.DeepCopy()
	new.Status.HonouredOperatingConstraints = []string{"camera"}
	if got := runUpdate(r, r.wakeHandler(), old, new); !slices.Equal(got, []string{"zoned"}) {
		t.Fatalf("woke %v, want [zoned]", got)
	}
}

// Selection reports when every robot whose adapter it asked was unreachable.
func TestSelectServable_ReportsEveryCandidateUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    *stubValidator
		want bool
	}{
		{"every adapter unreachable", &stubValidator{errFor: map[string]error{
			"amr-1": errors.New("no stream"), "amr-2": errors.New("no stream")}}, true},
		{"one unreachable, one declining", &stubValidator{errFor: map[string]error{"amr-1": errors.New("no stream")},
			byRobot: map[string]command.ValidateOutcome{"amr-2": {Servable: false, Message: "no"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &FleetActionReconciler{Validator: tc.v, Scheduler: &fixedScheduler{}}
			robots := withStoredRobots(t, r, validateRobot("amr-1"), validateRobot("amr-2"))
			judge, _, _ := r.newDispatchJudge(context.Background(), validateAction())
			_, _, _, allUnreachable, err := r.selectServable(context.Background(), validateAction(), robots, judge, false, false, false)
			if err == nil {
				t.Fatal("a robot was selected")
			}
			if allUnreachable != tc.want {
				t.Fatalf("allUnreachable = %v, want %v", allUnreachable, tc.want)
			}
		})
	}
}
