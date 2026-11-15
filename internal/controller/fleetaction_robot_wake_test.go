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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/scheduler"
)

func wakeRobot(phase fleetv1.RobotPhase, assigned string, estop fleetv1.RobotEstopState, zone string) *fleetv1.Robot {
	return &fleetv1.Robot{
		ObjectMeta: metav1.ObjectMeta{Name: "amr-1", Namespace: actionNS},
		Status:     fleetv1.RobotStatus{Phase: phase, AssignedAction: assigned, EstopState: estop, CurrentZone: zone},
	}
}

func wakeHeldByZone(r *fleetv1.Robot) *fleetv1.Robot {
	r = r.DeepCopy()
	r.Status.EstopHeldBy = []fleetv1.EstopHold{{Kind: fleetv1.EstopHoldFleetZone, Name: "aisle-1"}}
	return r
}

func wakeTriggered(r *fleetv1.Robot) *fleetv1.Robot {
	r = r.DeepCopy()
	r.Annotations = map[string]string{estopscope.TriggerAnnotation: "stop-1"}
	return r
}

func wakeDeleting(r *fleetv1.Robot) *fleetv1.Robot {
	r = r.DeepCopy()
	r.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	r.Finalizers = []string{"test/keep"}
	return r
}

// The Robot events that wake the waiting actions, and the ones that do not.
func TestRobotBecameFree(t *testing.T) {
	idle := wakeRobot(fleetv1.RobotPhaseIdle, "", fleetv1.RobotEstopNormal, "aisle-1")
	for _, tc := range []struct {
		name     string
		old, new *fleetv1.Robot
		want     bool
	}{
		{"the robot went Idle after its action", wakeRobot(fleetv1.RobotPhaseAssigned, "pick-0", "", "aisle-1"), idle, true},
		{"its action was released while Idle", wakeRobot(fleetv1.RobotPhaseIdle, "pick-0", "", "aisle-1"), idle, true},
		{"its stop cleared through to Normal", wakeRobot(fleetv1.RobotPhaseIdle, "", fleetv1.RobotEstopResuming, "aisle-1"), idle, true},
		{"an unset estopState counts as Normal", wakeRobot(fleetv1.RobotPhaseIdle, "", fleetv1.RobotEstopResuming, "aisle-1"),
			wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-1"), true},
		{"a zone's hold record was removed", wakeHeldByZone(idle), idle, true},
		{"its own stop's trigger was removed", wakeTriggered(idle), idle, true},
		{"it came back from Offline", wakeRobot(fleetv1.RobotPhaseOffline, "", "", "aisle-1"), idle, true},
		{"it came back from Charging", wakeRobot(fleetv1.RobotPhaseCharging, "", "", "aisle-1"), idle, true},
		{"a free robot moved zone", wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-2"), idle, true},
		{"a free robot's other status moved (battery, conditions)", idle.DeepCopy(), idle, false},
		{"a free robot took an action", idle, wakeRobot(fleetv1.RobotPhaseAssigned, "pick-1", "", "aisle-1"), false},
		{"a stop is still clearing (Resuming holds work)", wakeRobot(fleetv1.RobotPhaseIdle, "", fleetv1.RobotEstopStopped, "aisle-1"),
			wakeRobot(fleetv1.RobotPhaseIdle, "", fleetv1.RobotEstopResuming, "aisle-1"), false},
		{"a stop landed on a free robot", idle, wakeRobot(fleetv1.RobotPhaseIdle, "", fleetv1.RobotEstopStopping, "aisle-1"), false},
		{"Idle and Normal, but a zone's stop holds it on record", wakeRobot(fleetv1.RobotPhaseAssigned, "pick-0", "", "aisle-1"),
			wakeHeldByZone(idle), false},
		{"Idle and Normal, but its own stop's trigger is set", wakeRobot(fleetv1.RobotPhaseAssigned, "pick-0", "", "aisle-1"),
			wakeTriggered(idle), false},
		{"Idle, but being deleted", wakeRobot(fleetv1.RobotPhaseAssigned, "pick-0", "", "aisle-1"), wakeDeleting(idle), false},
		{"it went Offline", idle, wakeRobot(fleetv1.RobotPhaseOffline, "", "", "aisle-1"), false},
		{"Idle but still recording an action", wakeRobot(fleetv1.RobotPhaseAssigned, "pick-0", "", "aisle-1"),
			wakeRobot(fleetv1.RobotPhaseIdle, "pick-0", "", "aisle-1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := robotBecameFree.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.want {
				t.Fatalf("robotBecameFree.Update = %v, want %v", got, tc.want)
			}
		})
	}
	if !robotBecameFree.Create(event.CreateEvent{Object: idle}) {
		t.Error("a robot created free does not wake the waiting actions")
	}
	if robotBecameFree.Create(event.CreateEvent{Object: wakeRobot(fleetv1.RobotPhaseDiscovered, "", "", "")}) {
		t.Error("a robot created not yet free woke the waiting actions")
	}
	if robotBecameFree.Create(event.CreateEvent{Object: wakeHeldByZone(idle)}) {
		t.Error("a robot created under a zone's stop woke the waiting actions")
	}
	if robotBecameFree.Delete(event.DeleteEvent{Object: idle}) {
		t.Error("a deleted robot woke the waiting actions")
	}
}

// Every robot the wake calls free passes dispatch's own checks of the robot alone: not being
// deleted (scheduler.ExcludedAsTerminating), Idle (filter 1), under no stop and holding no
// stop's work (filter 10, robotUnderEstop and estopHoldsWork). Every combination of the fields
// those read is tried.
func TestRobotFreeForWork_ImpliesDispatchWouldConsiderIt(t *testing.T) {
	phases := []fleetv1.RobotPhase{"", fleetv1.RobotPhaseDiscovered, fleetv1.RobotPhaseIdle, fleetv1.RobotPhaseAssigned,
		fleetv1.RobotPhaseInProgress, fleetv1.RobotPhaseCharging, fleetv1.RobotPhaseOffline}
	estops := []fleetv1.RobotEstopState{"", fleetv1.RobotEstopNormal, fleetv1.RobotEstopStopping, fleetv1.RobotEstopStopped,
		fleetv1.RobotEstopResuming, fleetv1.RobotEstopFailed}
	marks := map[string]func(*fleetv1.Robot) *fleetv1.Robot{
		"none":         func(r *fleetv1.Robot) *fleetv1.Robot { return r },
		"zone hold":    wakeHeldByZone,
		"trigger":      wakeTriggered,
		"deleting":     wakeDeleting,
		"hold+deletes": func(r *fleetv1.Robot) *fleetv1.Robot { return wakeDeleting(wakeHeldByZone(r)) },
	}
	free := 0
	for _, phase := range phases {
		for _, assigned := range []string{"", "pick-0"} {
			for _, estop := range estops {
				for mark, apply := range marks {
					robot := apply(wakeRobot(phase, assigned, estop, "aisle-1"))
					if !robotFreeForWork(robot) {
						continue
					}
					free++
					what := fmt.Sprintf("phase %q, action %q, estop %q, %s", phase, assigned, estop, mark)
					switch {
					case scheduler.ExcludedAsTerminating(robot) != nil:
						t.Errorf("%s: called free, but it is being deleted", what)
					case robot.Status.Phase != fleetv1.RobotPhaseIdle || robot.Status.AssignedAction != "":
						t.Errorf("%s: called free, but it is not Idle with no action", what)
					case robotUnderEstop(robot) || estopHoldsWork(robot):
						t.Errorf("%s: called free, but a stop holds it", what)
					}
				}
			}
		}
	}
	if free != 2 { // Idle, no action, no mark, estopState unset or Normal
		t.Errorf("%d combinations called free, want 2", free)
	}
}

func wakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

var wakeT0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func wakeAction(ns, name string, prio fleetv1.ActionPriority, age time.Duration, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(wakeT0.Add(-age))},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Priority: prio},
		Status:     fleetv1.FleetActionStatus{Phase: phase},
	}
}

func wakeNames(t *testing.T, r *FleetActionReconciler, robot *fleetv1.Robot) []string {
	t.Helper()
	var names []string
	for _, req := range r.pendingActionsFor(context.Background(), robot, nil) {
		if req.Namespace != robot.Namespace {
			t.Errorf("woke %s in namespace %s", req.Name, req.Namespace)
		}
		names = append(names, req.Name)
	}
	return names
}

// The waiting actions of the robot's namespace, most urgent first: Critical, High, Normal (and
// unset), Low; then oldest first; then by name. Actions not waiting, being deleted or in another
// namespace are not woken. Each case holds no more than wakeLimit actions it should wake.
func TestPendingActionsFor_MostUrgentFirst(t *testing.T) {
	scheme := wakeScheme(t)
	deleting := wakeAction(actionNS, "deleting", fleetv1.ActionPriorityCritical, time.Hour, fleetv1.ActionPhasePending)
	deleting.SetDeletionTimestamp(&metav1.Time{Time: wakeT0})
	deleting.SetFinalizers([]string{"test/keep"})
	notWaiting := []client.Object{
		wakeAction(actionNS, "running", fleetv1.ActionPriorityCritical, time.Hour, fleetv1.ActionPhaseInProgress),
		wakeAction("other-ns", "elsewhere", fleetv1.ActionPriorityCritical, time.Hour, fleetv1.ActionPhasePending),
		deleting,
	}
	for _, tc := range []struct {
		name    string
		waiting []client.Object
		want    []string
	}{
		{"bands, then names within a band and an age", []client.Object{
			wakeAction(actionNS, "normal", fleetv1.ActionPriorityNormal, time.Hour, fleetv1.ActionPhasePending),
			wakeAction(actionNS, "critical", fleetv1.ActionPriorityCritical, time.Second, fleetv1.ActionPhasePending),
			wakeAction(actionNS, "high-b", fleetv1.ActionPriorityHigh, 30*time.Second, fleetv1.ActionPhasePending),
			wakeAction(actionNS, "high-a", fleetv1.ActionPriorityHigh, 30*time.Second, fleetv1.ActionPhasePending),
		}, []string{"critical", "high-a", "high-b"}},
		{"unset is Normal, oldest first, Low last", []client.Object{
			wakeAction(actionNS, "normal-new", fleetv1.ActionPriorityNormal, time.Minute, fleetv1.ActionPhasePending),
			wakeAction(actionNS, "low-old", fleetv1.ActionPriorityLow, time.Hour, fleetv1.ActionPhasePending),
			wakeAction(actionNS, "unset-old", "", 2*time.Minute, ""),
		}, []string{"unset-old", "normal-new", "low-old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(tc.waiting, notWaiting...)...).Build()
			r := &FleetActionReconciler{Client: c, Scheme: scheme}
			if got := wakeNames(t, r, wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-1")); !slices.Equal(got, tc.want) {
				t.Fatalf("woke %v, want %v", got, tc.want)
			}
		})
	}
}

// One robot becoming free wakes at most wakeLimit actions: the most urgent, in order.
func TestPendingActionsFor_WakesAtMostTheLimit(t *testing.T) {
	scheme := wakeScheme(t)
	b := fake.NewClientBuilder().WithScheme(scheme)
	var all []string
	for i := range wakeLimit + 4 {
		name := fmt.Sprintf("wait-%02d", i)
		b = b.WithObjects(wakeAction(actionNS, name, fleetv1.ActionPriorityNormal, time.Duration(100-i)*time.Minute,
			fleetv1.ActionPhasePending))
		all = append(all, name)
	}
	// One Critical action, the newest of all, goes ahead of every Normal one.
	b = b.WithObjects(wakeAction(actionNS, "urgent", fleetv1.ActionPriorityCritical, time.Second, fleetv1.ActionPhasePending))
	r := &FleetActionReconciler{Client: b.Build(), Scheme: scheme}

	got := wakeNames(t, r, wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-1"))
	want := append([]string{"urgent"}, all[:wakeLimit-1]...)
	if !slices.Equal(got, want) {
		t.Fatalf("woke %v, want %v", got, want)
	}
}

// The wake drops an action only where dispatch would certainly refuse this robot: the pair-wise
// filters (6, 3, 5, and an action for a ComputeEndpoint), filters 4 and 7, and filter 2. A
// refusal that depends on what the wake cannot see, such as the namespace's acceptance of
// Degraded capabilities, keeps the action. A free robot that only moved wakes only the actions
// whose zone it has just entered. Each case is judged action by action (mayTakeAction), before
// the limit.
func TestMayTakeAction_DropsOnlyWhatDispatchWouldRefuse(t *testing.T) {
	scheme := wakeScheme(t)
	zone := func(name, parent string) *fleetv1.FleetZone {
		return &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Namespace: actionNS, Name: name},
			Spec: fleetv1.FleetZoneSpec{ParentZone: parent}}
	}
	act := func(name string, edit func(*fleetv1.FleetAction)) *fleetv1.FleetAction {
		a := wakeAction(actionNS, name, fleetv1.ActionPriorityNormal, time.Minute, fleetv1.ActionPhasePending)
		edit(a)
		return a
	}
	inZone := func(z string) func(*fleetv1.FleetAction) { return func(a *fleetv1.FleetAction) { a.Spec.Zone = z } }
	zones := []client.Object{zone("site", ""), zone("aisle-1", "site"), zone("aisle-2", "site"), zone("dock", "")}
	actions := []*fleetv1.FleetAction{
		act("a-no-zone", func(*fleetv1.FleetAction) {}),
		act("b-its-leaf", inZone("aisle-1")),
		act("c-its-parent", inZone("site")),
		act("d-sibling-leaf", inZone("aisle-2")),
		act("e-other-root", inZone("dock")),
		act("f-unknown-zone", inZone("nowhere")),
		act("g-pinned-here-elsewhere", func(a *fleetv1.FleetAction) { a.Spec.Zone, a.Spec.RobotSelector = "dock", "amr-1" }),
		act("h-pinned-to-another", func(a *fleetv1.FleetAction) { a.Spec.RobotSelector = "amr-2" }),
		act("i-needs-lift", func(a *fleetv1.FleetAction) { a.Spec.RequiredCapabilities = []string{"lift"} }),
		act("j-needs-arm", func(a *fleetv1.FleetAction) { a.Spec.RequiredCapabilities = []string{"arm"} }),
		act("k-needs-degraded-camera", func(a *fleetv1.FleetAction) { a.Spec.RequiredCapabilities = []string{"camera"} }),
		act("l-light-load", func(a *fleetv1.FleetAction) { a.Spec.Constraints = map[string]float64{"payload_kg": 20} }),
		act("m-heavy-load", func(a *fleetv1.FleetAction) { a.Spec.Constraints = map[string]float64{"payload_kg": 80} }),
		act("n-for-a-compute-endpoint", func(a *fleetv1.FleetAction) {
			a.Spec.Executor = &fleetv1.ActionExecutor{EndpointRef: "gpu-1"}
		}),
		act("o-battery-floor-above", func(a *fleetv1.FleetAction) { a.Spec.MinBatteryPct = ptr.To[int32](60) }),
		act("p-battery-floor-below", func(a *fleetv1.FleetAction) { a.Spec.MinBatteryPct = ptr.To[int32](20) }),
		act("q-needs-a-capability-whose-model-is-updating", func(a *fleetv1.FleetAction) {
			a.Spec.RequiredCapabilities = []string{"scan"}
		}),
		act("r-pinned-here-battery-floor-above", func(a *fleetv1.FleetAction) {
			a.Spec.RobotSelector, a.Spec.MinBatteryPct = "amr-1", ptr.To[int32](60)
		}),
	}
	robot := wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-1")
	robot.Status.BatteryPercent = ptr.To[int32](30)
	robot.Status.Capabilities = []fleetv1.CapabilityStatusEntry{
		{Name: "lift", Status: fleetv1.CapabilityStatusActive, ResolvedParameters: map[string]float64{"payload_kg": 50}},
		{Name: "camera", Status: fleetv1.CapabilityStatusDegraded},
		{Name: "scan", Status: fleetv1.CapabilityStatusActive},
	}
	robot.Spec.InstalledModels = []fleetv1.ClassModel{{Name: "scanner-v2", GrantsCapabilities: []string{"scan"}}}
	robot.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{Name: "scanner-v2", Status: fleetv1.ModelStatusUpdating}}

	var zoneItems []fleetv1.FleetZone
	for _, o := range zones {
		zoneItems = append(zoneItems, *o.(*fleetv1.FleetZone))
	}
	// Every action the wake drops, dispatch's own filters refuse this robot, whichever way the
	// namespace answers acceptDegraded.
	dispatchRefuses := func(robot *fleetv1.Robot, a *fleetv1.FleetAction) bool {
		if scheduler.ExcludedBy(robot, a, false) != nil && scheduler.ExcludedBy(robot, a, true) != nil {
			return true
		}
		if excludedByBatteryFloor(robot, a) != nil || excludedByModelUpdate(robot, a) != nil {
			return true
		}
		pinned := a.Spec.RobotSelector != "" && a.Spec.RobotSelector == robot.Name
		return a.Spec.Zone != "" && !pinned && excludedByZone(robot, a, resolveZoneLeaves(a.Spec.Zone, zoneItems)) != nil
	}
	takeable := func(t *testing.T, c client.Client, robot *fleetv1.Robot, movedFrom *string, crossCheck bool) []string {
		t.Helper()
		r := &FleetActionReconciler{Client: c, Scheme: scheme}
		leaves := r.zoneLeavesFor(context.Background(), actionNS)
		var kept []string
		for _, a := range actions {
			if mayTakeAction(robot, a, leaves, movedFrom) {
				kept = append(kept, a.Name)
			} else if crossCheck && !dispatchRefuses(robot, a) {
				t.Errorf("the wake dropped %s, which dispatch's filters would not refuse this robot", a.Name)
			}
		}
		return kept
	}
	readable := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zones...).Build()

	for _, tc := range []struct {
		name      string
		robot     func(*fleetv1.Robot)
		movedFrom *string
		c         client.Client
		want      []string
	}{
		{"a robot that became free, in a leaf zone", nil, nil, readable, []string{"a-no-zone", "b-its-leaf", "c-its-parent",
			"g-pinned-here-elsewhere", "i-needs-lift", "k-needs-degraded-camera", "l-light-load", "p-battery-floor-below"}},
		{"a robot that became free, in no zone: only unzoned or pinned work",
			func(r *fleetv1.Robot) { r.Status.CurrentZone = "" }, nil, readable,
			[]string{"a-no-zone", "g-pinned-here-elsewhere", "i-needs-lift", "k-needs-degraded-camera", "l-light-load",
				"p-battery-floor-below"}},
		{"an unreadable zone list drops nothing on zone", nil, nil,
			fake.NewClientBuilder().WithScheme(scheme).WithObjects(zones...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*fleetv1.FleetZoneList); ok {
						return errors.New("the FleetZone list is unreadable")
					}
					return c.List(ctx, list, opts...)
				},
			}).Build(),
			[]string{"a-no-zone", "b-its-leaf", "c-its-parent", "d-sibling-leaf", "e-other-root", "f-unknown-zone",
				"g-pinned-here-elsewhere", "i-needs-lift", "k-needs-degraded-camera", "l-light-load", "p-battery-floor-below"}},
		{"a free robot moved from a sibling leaf: only the zone it entered", nil, ptr.To("aisle-2"), readable,
			[]string{"b-its-leaf"}},
		{"a free robot moved from another root: every zone it entered", nil, ptr.To("dock"), readable,
			[]string{"b-its-leaf", "c-its-parent"}},
		{"a free robot moved from no zone: every zone it entered", nil, ptr.To(""), readable,
			[]string{"b-its-leaf", "c-its-parent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := robot.DeepCopy()
			if tc.robot != nil {
				tc.robot(r)
			}
			got := takeable(t, tc.c, r, tc.movedFrom, tc.movedFrom == nil)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("takes %v, want %v", got, tc.want)
			}
		})
	}
}

// A free robot moving zone wakes the actions of the zone it entered, not of the one it left:
// the handler maps the robot as it is now, never the update's old object.
func TestWakeHandler_MapsOnlyTheRobotAsItIsNow(t *testing.T) {
	scheme := wakeScheme(t)
	zoned := func(name, zone string) client.Object {
		a := wakeAction(actionNS, name, fleetv1.ActionPriorityNormal, time.Minute, fleetv1.ActionPhasePending)
		a.Spec.Zone = zone
		return a
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zoned("in-aisle-1", "aisle-1"), zoned("in-aisle-2", "aisle-2")).Build()
	r := &FleetActionReconciler{Client: c, Scheme: scheme}
	q := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	defer q.ShutDown()

	r.wakeHandler().Update(context.Background(), event.UpdateEvent{
		ObjectOld: wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-2"),
		ObjectNew: wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-1"),
	}, q)

	var got []string
	for q.Len() > 0 {
		item, _ := q.Get()
		got = append(got, item.(reconcile.Request).Name)
		q.Done(item)
	}
	if !slices.Equal(got, []string{"in-aisle-1"}) {
		t.Fatalf("woke %v, want [in-aisle-1]", got)
	}
}

// A free robot that only moved wakes nothing for unzoned work, which it could take before; a
// robot that became free as it moved wakes it.
func TestWakeHandler_AMoveAloneWakesOnlyZoneWork(t *testing.T) {
	scheme := wakeScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		wakeAction(actionNS, "anywhere", fleetv1.ActionPriorityNormal, time.Minute, fleetv1.ActionPhasePending)).Build()
	r := &FleetActionReconciler{Client: c, Scheme: scheme}
	woken := func(old *fleetv1.Robot) int {
		q := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
		defer q.ShutDown()
		r.wakeHandler().Update(context.Background(), event.UpdateEvent{
			ObjectOld: old, ObjectNew: wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-1")}, q)
		return q.Len()
	}
	if n := woken(wakeRobot(fleetv1.RobotPhaseIdle, "", "", "aisle-2")); n != 0 {
		t.Errorf("a free robot's move woke %d unzoned actions, want 0", n)
	}
	if n := woken(wakeRobot(fleetv1.RobotPhaseAssigned, "pick-0", "", "aisle-2")); n != 1 {
		t.Errorf("a robot that became free as it moved woke %d unzoned actions, want 1", n)
	}
}
