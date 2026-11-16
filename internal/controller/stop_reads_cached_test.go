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
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
)

// stopReadCounter counts the reads made through it, by verb and object type, and can fail them.
type stopReadCounter struct {
	client.Reader
	mu    sync.Mutex
	reads map[string]int
	fail  func(verb string, obj runtime.Object) error
}

func (c *stopReadCounter) note(verb string, obj runtime.Object) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads == nil {
		c.reads = map[string]int{}
	}
	c.reads[fmt.Sprintf("%s %T", verb, obj)]++
	if c.fail != nil {
		return c.fail(verb, obj)
	}
	return nil
}

func (c *stopReadCounter) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.note("get", obj); err != nil {
		return err
	}
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *stopReadCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.note("list", list); err != nil {
		return err
	}
	return c.Reader.List(ctx, list, opts...)
}

func (c *stopReadCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.reads {
		n += v
	}
	return n
}

// staleCache is a reconciler whose cache holds objs and whose API server holds objs with live
// applied on top: the API server has seen a write the cache has not.
type staleCache struct {
	r          *FleetActionReconciler
	api, cache client.Client
	confirm    *stopReadCounter
	tde        *fakeTDE
}

func newStaleCache(t *testing.T, objs []client.Object, live ...client.Object) *staleCache {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	build := func() client.Client {
		seed := []client.Object{readyActionAdapter()}
		for _, o := range objs {
			seed = append(seed, o.DeepCopyObject().(client.Object))
		}
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed...).
			WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}).Build()
	}
	s := &staleCache{api: build(), cache: build(), tde: &fakeTDE{result: tde.ReservationResult{Status: tde.Granted}}}
	// Each live object is written at the API server only, as a write the cache has not seen yet:
	// its resourceVersion moves on there, as it would.
	ctx := context.Background()
	for _, o := range live {
		o = o.DeepCopyObject().(client.Object)
		cur := o.DeepCopyObject().(client.Object)
		if err := s.api.Get(ctx, client.ObjectKeyFromObject(o), cur); err != nil {
			if err := s.api.Create(ctx, o); err != nil {
				t.Fatalf("create %s at the API server: %v", o.GetName(), err)
			}
			continue
		}
		o.SetResourceVersion(cur.GetResourceVersion())
		status := o.DeepCopyObject().(client.Object)
		if err := s.api.Update(ctx, o); err != nil {
			t.Fatalf("update %s at the API server: %v", o.GetName(), err)
		}
		switch status.(type) {
		case *fleetv1.Robot, *fleetv1.FleetAction: // the kinds the fake serves a status subresource for
			status.SetResourceVersion(o.GetResourceVersion())
			if err := s.api.Status().Update(ctx, status); err != nil {
				t.Fatalf("update %s's status at the API server: %v", o.GetName(), err)
			}
		}
	}
	s.confirm = &stopReadCounter{Reader: s.api}
	s.r = &FleetActionReconciler{Client: cachedClient{Client: s.api, cache: s.cache}, APIReader: s.api,
		ConfirmReader: s.confirm, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler(), TDE: s.tde}
	return s
}

func (s *staleCache) reconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	res, err := s.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (s *staleCache) action(t *testing.T, name string) *fleetv1.FleetAction {
	t.Helper()
	return getAction(t, s.api, name)
}

func triggeredZone(name, parent string) *fleetv1.FleetZone {
	z := zoneObj(name, parent)
	z.Annotations = map[string]string{annEstopTriggered: "now"}
	return z
}

func freeRobotAt(name, specZone, currentZone string) *fleetv1.Robot {
	return robotPlaced(name, specZone, currentZone)
}

func pendingTo(name, zone string) *fleetv1.FleetAction {
	a := pendingInBand(name, fleetv1.ActionPriorityNormal)
	a.Spec.Zone = zone
	return a
}

// A stop written at the API server that the cache has not seen holds the assignment: the
// confirmation reads it before the TDE is asked or anything is written, and the next pass reads
// every stop from the API server and writes the hold it finds.
func TestStopConfirmation_AStopTheCacheMissedHoldsTheAssignment(t *testing.T) {
	base := func() []client.Object {
		return []client.Object{zoneObj("site", ""), zoneObj("floor", "site"), zoneObj("bay", "floor"),
			zoneObj("yard", "")}
	}
	cases := []struct {
		name   string
		robot  *fleetv1.Robot
		action *fleetv1.FleetAction
		live   []client.Object
	}{
		{"the robot's current zone", freeRobotAt("r1", "", "bay"), pendingTo("a1", ""),
			[]client.Object{triggeredZone("bay", "floor")}},
		{"an ancestor of the robot's current zone", freeRobotAt("r1", "", "bay"), pendingTo("a1", ""),
			[]client.Object{triggeredZone("site", "")}},
		{"the robot's spec.zone", freeRobotAt("r1", "bay", "yard"), pendingTo("a1", ""),
			[]client.Object{triggeredZone("bay", "floor")}},
		// Filter 2 keeps candidates inside the destination's leaf set, so the robot stands in it.
		{"the destination zone (A15)", freeRobotAt("r1", "", "bay"), pendingTo("a1", "bay"),
			[]client.Object{triggeredZone("bay", "floor")}},
		{"an ancestor of the destination (A15)", freeRobotAt("r1", "", "bay"), pendingTo("a1", "floor"),
			[]client.Object{triggeredZone("site", "")}},
		{"the namespace", freeRobotAt("r1", "", "yard"), pendingTo("a1", ""),
			[]client.Object{&fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: actionNS, Name: "swarmada-config",
				Annotations: map[string]string{annEstopTriggered: "now"}}}}},
		{"a re-parent the cache missed", freeRobotAt("r1", "", "yard"), pendingTo("a1", ""),
			[]client.Object{zoneObj("yard", "floor"), triggeredZone("floor", "site")}},
		{"the robot's own stop", freeRobotAt("r1", "", "yard"), pendingTo("a1", ""),
			[]client.Object{func() *fleetv1.Robot {
				r := freeRobotAt("r1", "", "yard")
				r.Status.EstopState = fleetv1.RobotEstopStopping
				return r
			}()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(base(), tc.robot, tc.action)
			if tc.name == "a re-parent the cache missed" {
				// floor's stop is in the cache too; only yard's new parent is not.
				objs = append(objs[:0:0], zoneObj("site", ""), triggeredZone("floor", "site"), zoneObj("bay", "floor"),
					zoneObj("yard", ""), tc.robot, tc.action)
			}
			s := newStaleCache(t, objs, tc.live...)
			res := s.reconcile(t, "a1")
			if got := s.action(t, "a1"); got.Status.AssignedRobot != "" || got.Status.Phase == fleetv1.ActionPhaseAssigned {
				t.Fatalf("assigned to %q (phase %s) under a stop only the API server shows", got.Status.AssignedRobot, got.Status.Phase)
			}
			if s.tde.requests != 0 {
				t.Errorf("the TDE was asked %d times; the confirmation runs before it", s.tde.requests)
			}
			// A stop written onto the robot itself moves its resourceVersion, so L3's re-read
			// (currentCandidate) withholds it before the confirmation runs. Every other stop is on
			// an object the re-read does not look at, and only the confirmation sees it.
			if tc.name != "the robot's own stop" {
				if !res.Requeue {
					t.Errorf("result %+v; want an immediate requeue", res)
				}
				if !s.r.uncachedNext.has(types.NamespacedName{Namespace: actionNS, Name: "a1"}) {
					t.Errorf("the next pass is not marked to read the stops from the API server")
				}
			}
			// The next pass reads every stop from the API server: still no assignment, and it writes
			// the hold it finds (row A15's condition, for a stopped destination).
			s.reconcile(t, "a1")
			got := s.action(t, "a1")
			if got.Status.AssignedRobot != "" {
				t.Fatalf("the uncached pass assigned to %q", got.Status.AssignedRobot)
			}
			if strings.Contains(tc.name, "(A15)") && !zoneStoppedHeld(got) {
				t.Fatalf("the uncached pass wrote no ZoneStopped hold: %+v", got.Status.Conditions)
			}
		})
	}
}

// With no stop anywhere the confirmation lets the assignment through, at the cost the plan
// counts: one GET per zone on the chains and one SwarmadaConfig list. The robot is not read again:
// currentCandidate's re-read verified the copy, and the robot commit is locked to it.
func TestStopConfirmation_NoStopAssignsAtItsCountedCost(t *testing.T) {
	s := newStaleCache(t, []client.Object{zoneObj("site", ""), zoneObj("bay", "site"),
		freeRobotAt("r1", "", "bay"), pendingTo("a1", "bay")})
	s.reconcile(t, "a1")
	if got := s.action(t, "a1"); got.Status.AssignedRobot != "r1" {
		t.Fatalf("not assigned (phase %s, message %q)", got.Status.Phase, got.Status.Message)
	}
	want := map[string]int{
		"get *v1.Robot":               0,
		"get *v1.FleetZone":           2, // bay, site: the robot's chain and the destination's are one
		"list *v1.SwarmadaConfigList": 1,
	}
	for k, n := range want {
		if s.confirm.reads[k] != n {
			t.Errorf("%s: %d reads, want %d (all: %v)", k, s.confirm.reads[k], n, s.confirm.reads)
		}
	}
	if s.confirm.total() != 3 {
		t.Errorf("%d confirmation reads, want 3: %v", s.confirm.total(), s.confirm.reads)
	}
}

// Past confirmZoneGets zones the confirmation lists the zones once and judges every chain from
// that one list.
func TestStopConfirmation_ALongChainIsJudgedFromOneList(t *testing.T) {
	objs := []client.Object{zoneObj("z0", ""), zoneObj("z1", "z0"), zoneObj("z2", "z1"), zoneObj("z3", "z2"),
		zoneObj("z4", "z3"), freeRobotAt("r1", "", "z4"), pendingTo("a1", "")}
	s := newStaleCache(t, objs, triggeredZone("z0", ""))
	s.reconcile(t, "a1")
	if got := s.action(t, "a1"); got.Status.AssignedRobot != "" {
		t.Fatalf("assigned under z0's stop, five zones up")
	}
	if s.confirm.reads["list *v1.FleetZoneList"] != 1 || s.confirm.reads["get *v1.FleetZone"] != 0 {
		t.Errorf("want one zone list and no zone GETs when the cache shows more than %d zones: %v",
			confirmZoneGets, s.confirm.reads)
	}
}

// A chain the cache shows short and the API server long (a re-parent the cache missed) is walked by
// GETs until it needs more than confirmZoneGets zones, then judged from one list.
func TestStopConfirmation_AChainLongerThanTheCacheKnewEndsInOneList(t *testing.T) {
	objs := []client.Object{zoneObj("z0", ""), zoneObj("z1", "z0"), zoneObj("z2", "z1"), zoneObj("z3", "z2"),
		zoneObj("z4", ""), freeRobotAt("r1", "", "z4"), pendingTo("a1", "")}
	s := newStaleCache(t, objs, zoneObj("z4", "z3"), triggeredZone("z0", ""))
	s.reconcile(t, "a1")
	if got := s.action(t, "a1"); got.Status.AssignedRobot != "" {
		t.Fatalf("assigned under z0's stop, five zones up a chain the cache had not seen")
	}
	if s.confirm.reads["get *v1.FleetZone"] != confirmZoneGets || s.confirm.reads["list *v1.FleetZoneList"] != 1 {
		t.Errorf("want %d zone GETs then one list: %v", confirmZoneGets, s.confirm.reads)
	}
}

// A clear on a robot that is Normal again drops the processed marker and the stop finalizer in one
// patch: the release is judged on the copy the clear patches, the marker already removed from it.
func TestRobotStopFinalizer_AClearOnANormalRobotDropsMarkerAndFinalizer(t *testing.T) {
	ctx := context.Background()
	robot := freeRobotAt("r1", "", "bay")
	robot.Annotations = map[string]string{annEstopProcessed: "stop-1"}
	robot.Finalizers = []string{estopscope.StopFinalizer}
	s := newStaleCache(t, []client.Object{zoneObj("bay", ""), robot})
	cur := &fleetv1.Robot{}
	if err := s.api.Get(ctx, client.ObjectKeyFromObject(robot), cur); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dropClearedMarker(ctx, s.api, s.api, cur, func(rb *fleetv1.Robot) (bool, error) {
		keep, err := robotKeepsStopFinalizerCacheFirst(ctx, s.cache, s.confirm, rb)
		return !keep, err
	}); err != nil {
		t.Fatal(err)
	}
	got := &fleetv1.Robot{}
	if err := s.api.Get(ctx, client.ObjectKeyFromObject(robot), got); err != nil {
		t.Fatal(err)
	}
	if _, marked := got.Annotations[annEstopProcessed]; marked || controllerutil.ContainsFinalizer(got, estopscope.StopFinalizer) {
		t.Fatalf("annotations %v finalizers %v; want the marker and the finalizer gone", got.Annotations, got.Finalizers)
	}
	if s.confirm.reads["get *v1.Robot"] != 0 {
		t.Errorf("the release read the robot again: %v", s.confirm.reads)
	}
}

// An action whose API-server copy carries row A15's hold, while the cache's copy does not yet (the
// hold was written a moment ago), is scanned from the cache once; the confirmation then finds the
// stop before anything is written, so the hold is not lifted.
func TestStopConfirmation_AFreshA15HoldIsNotLiftedByAStaleActionCopy(t *testing.T) {
	cachedCopy := pendingTo("a1", "bay")
	held := cachedCopy.DeepCopy()
	held.Status.Conditions = []metav1.Condition{{Type: conditionZoneStopped, Status: metav1.ConditionTrue,
		Reason: conditionZoneStopped, Message: "held", LastTransitionTime: metav1.Now()}}
	s := newStaleCache(t, []client.Object{zoneObj("bay", ""), freeRobotAt("r1", "", "bay"), cachedCopy},
		triggeredZone("bay", ""), held)
	s.reconcile(t, "a1")
	got := s.action(t, "a1")
	if got.Status.AssignedRobot != "" || !zoneStoppedHeld(got) {
		t.Fatalf("assigned %q, conditions %+v: the hold was lifted on a stale copy", got.Status.AssignedRobot, got.Status.Conditions)
	}
}

// A confirmation read that fails assigns nothing.
func TestStopConfirmation_FailsClosedOnAReadError(t *testing.T) {
	for _, kind := range []string{"*v1.FleetZone", "*v1.SwarmadaConfigList"} {
		t.Run(kind, func(t *testing.T) {
			s := newStaleCache(t, []client.Object{zoneObj("bay", ""), freeRobotAt("r1", "", "bay"), pendingTo("a1", "")})
			s.confirm.fail = func(verb string, obj runtime.Object) error {
				if fmt.Sprintf("%T", obj) == kind {
					return errors.New("injected")
				}
				return nil
			}
			s.reconcile(t, "a1")
			if got := s.action(t, "a1"); got.Status.AssignedRobot != "" {
				t.Fatalf("assigned although the confirmation's %s read failed", kind)
			}
			if s.tde.requests != 0 {
				t.Errorf("the TDE was asked after a failed confirmation")
			}
		})
	}
}

// A robot copy no re-read verified (the preemption path, or a re-read that failed) is read again
// by the confirmation: a stop written onto it holds the assignment, and a read that fails holds it
// too.
func TestStopConfirmation_AnUnverifiedRobotIsReadAgain(t *testing.T) {
	ctx := context.Background()
	cached := freeRobotAt("r1", "", "yard")
	live := cached.DeepCopy()
	live.Status.EstopState = fleetv1.RobotEstopStopping
	s := newStaleCache(t, []client.Object{zoneObj("yard", ""), cached}, live)
	why, err := s.r.confirmNoStop(ctx, pendingTo("a1", ""), cached, false, nil)
	if err != nil || why == "" {
		t.Fatalf("why %q err %v; the robot's own stop must hold an unverified copy", why, err)
	}
	if s.confirm.reads["get *v1.Robot"] != 1 {
		t.Errorf("want one Robot GET for an unverified copy: %v", s.confirm.reads)
	}
	s2 := newStaleCache(t, []client.Object{zoneObj("yard", ""), cached})
	s2.confirm.fail = func(verb string, obj runtime.Object) error {
		if _, ok := obj.(*fleetv1.Robot); ok {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := s2.r.confirmNoStop(ctx, pendingTo("a1", ""), cached, false, nil); err == nil {
		t.Fatal("a failed robot read must fail the confirmation")
	}
}

// An action row A15 already holds is judged from the API server, so a cache that has not seen
// the stop cannot lift the hold.
func TestStopConfirmation_AnA15HoldIsNotLiftedByTheCache(t *testing.T) {
	held := pendingTo("a1", "bay")
	held.Status.Conditions = []metav1.Condition{{Type: conditionZoneStopped, Status: metav1.ConditionTrue,
		Reason: conditionZoneStopped, Message: "held", LastTransitionTime: metav1.Now()}}
	s := newStaleCache(t, []client.Object{zoneObj("bay", ""), freeRobotAt("r1", "", "yard"), held},
		triggeredZone("bay", ""))
	if _, cached := s.r.stopScanReader(held); cached {
		t.Fatal("an action carrying ZoneStopped is scanned from the cache")
	}
	s.reconcile(t, "a1")
	got := s.action(t, "a1")
	if !zoneStoppedHeld(got) {
		t.Fatalf("the hold was lifted on the cache's word: conditions %+v", got.Status.Conditions)
	}
}

// The stop finalizer is released only when the API server also shows no stop.
func TestHeldFinalizer_ReleasedOnlyOnTheAPIServersWord(t *testing.T) {
	action := assignedAction("a1", "r1", fleetv1.ActionPhaseInProgress, 1, nil)
	action.Finalizers = []string{estopscope.StopFinalizer}
	robot := robotInPhase("r1", fleetv1.RobotPhaseAssigned, "a1")
	robot.Status.CurrentZone = "bay"
	s := newStaleCache(t, []client.Object{zoneObj("bay", ""), robot, action}, triggeredZone("bay", ""))
	holders, err := s.r.syncHeldFinalizer(context.Background(), s.action(t, "a1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || !controllerutil.ContainsFinalizer(s.action(t, "a1"), estopscope.StopFinalizer) {
		t.Fatalf("finalizer released on the cache's word; holders %v", holders)
	}
	// With no stop at the API server either, it is released.
	s2 := newStaleCache(t, []client.Object{zoneObj("bay", ""), robot, action})
	if _, err := s2.r.syncHeldFinalizer(context.Background(), s2.action(t, "a1")); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(s2.action(t, "a1"), estopscope.StopFinalizer) {
		t.Fatal("finalizer kept with no stop anywhere")
	}
}

// robot_estop's finalizer decision: a release needs the API server's word; an add waits for the
// cache (the known limit), and costs no uncached read.
func TestRobotStopFinalizer_CacheFirst(t *testing.T) {
	ctx := context.Background()
	withFin := freeRobotAt("r1", "", "bay")
	withFin.Finalizers = []string{estopscope.StopFinalizer}
	s := newStaleCache(t, []client.Object{zoneObj("bay", ""), withFin}, triggeredZone("bay", ""))
	keep, err := robotKeepsStopFinalizerCacheFirst(ctx, s.cache, s.confirm, withFin)
	if err != nil || !keep {
		t.Fatalf("keep=%v err=%v; a stop at the API server must keep the finalizer", keep, err)
	}
	without := freeRobotAt("r2", "", "bay")
	s2 := newStaleCache(t, []client.Object{zoneObj("bay", ""), without}, triggeredZone("bay", ""))
	keep, err = robotKeepsStopFinalizerCacheFirst(ctx, s2.cache, s2.confirm, without)
	if err != nil || keep {
		t.Fatalf("keep=%v err=%v; without the finalizer the cache's word stands", keep, err)
	}
	if s2.confirm.total() != 0 {
		t.Errorf("%d uncached reads for a robot without the finalizer; want 0", s2.confirm.total())
	}
}

// The Paused check takes whether the robot confirmed its stop from the live robot, whatever the
// cache shows: a cached Stopped over a live Stopping is not a confirmed stop.
func TestStopOverCacheFirst_ConfirmedIsTheLiveAnswer(t *testing.T) {
	cached := freeRobotAt("r1", "", "bay")
	cached.Status.EstopState = fleetv1.RobotEstopStopped
	live := cached.DeepCopy()
	live.Status.EstopState = fleetv1.RobotEstopStopping
	s := newStaleCache(t, []client.Object{triggeredZone("bay", ""), cached}, live)
	s.r.StopPathReader = s.api
	stops, confirmed, err := s.r.stopOverCacheFirst(context.Background(), cached)
	if err != nil {
		t.Fatal(err)
	}
	if len(stops) == 0 || confirmed {
		t.Fatalf("stops %v confirmed %v; want the stop, unconfirmed", stops, confirmed)
	}
}

// stopNamesCacheFirst obeys a stop the cache shows, and answers "no stop" only from the API server.
func TestStopNamesCacheFirst(t *testing.T) {
	ctx := context.Background()
	robot := freeRobotAt("r1", "", "bay")
	// The cache shows a stop the API server has cleared: it holds.
	s := newStaleCache(t, []client.Object{triggeredZone("bay", ""), robot}, zoneObj("bay", ""))
	if stops, err := stopNamesCacheFirst(ctx, s.cache, s.confirm, robot); err != nil || len(stops) == 0 {
		t.Fatalf("stops %v err %v; a stop the cache shows must hold", stops, err)
	}
	if s.confirm.total() != 0 {
		t.Errorf("a stop the cache shows cost %d uncached reads", s.confirm.total())
	}
	// The cache shows none, the API server one: the API server's answer.
	s2 := newStaleCache(t, []client.Object{zoneObj("bay", ""), robot}, triggeredZone("bay", ""))
	stops, err := stopNamesCacheFirst(ctx, s2.cache, s2.confirm, robot)
	if err != nil || len(stops) == 0 || !strings.Contains(strings.Join(stops, ","), "bay") {
		t.Fatalf("stops %v err %v; want bay's stop from the API server", stops, err)
	}
}

// hookTDE is fakeTDE with a hook that runs inside RequestReservation: after currentCandidate's
// re-read and the stop confirmation, before the action and robot commits.
type hookTDE struct {
	fakeTDE
	onRequest func()
}

func (h *hookTDE) RequestReservation(ctx context.Context, req tde.ReservationRequest) (tde.ReservationResult, error) {
	if h.onRequest != nil {
		h.onRequest()
	}
	return h.fakeTDE.RequestReservation(ctx, req)
}

// The ordinary path does not read the robot a second time: the robot commit is locked to the
// version currentCandidate read. So a stop, or a zone move, written onto the robot between that
// read and the commit must fail the commit (409): nothing is dispatched, the action goes back to
// Pending, and the TDE slot taken for it is released.
func TestStopConfirmation_ARobotWrittenAfterTheReReadFailsTheCommit(t *testing.T) {
	cases := []struct {
		name  string
		write func(*fleetv1.Robot)
	}{
		{"a stop written onto the robot", func(r *fleetv1.Robot) { r.Status.EstopState = fleetv1.RobotEstopStopping }},
		{"the robot moved into another zone", func(r *fleetv1.Robot) { r.Status.CurrentZone = "bay" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := newStaleCache(t, []client.Object{zoneObj("yard", ""), zoneObj("bay", ""),
				freeRobotAt("r1", "", "yard"), pendingTo("a1", "yard")})
			hook := &hookTDE{fakeTDE: fakeTDE{result: tde.ReservationResult{Status: tde.Granted}}}
			s.r.TDE = hook
			hook.onRequest = func() {
				var rb fleetv1.Robot
				if err := s.api.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: "r1"}, &rb); err != nil {
					t.Fatal(err)
				}
				tc.write(&rb)
				if err := s.api.Status().Update(ctx, &rb); err != nil {
					t.Fatalf("write onto the robot: %v", err)
				}
			}
			// Reconcile turns a conflict into a requeue, so the 409 shows as the revert's message.
			res, err := s.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: "a1"}})
			if err != nil || !res.Requeue {
				t.Fatalf("result %+v err %v; want the conflict's requeue", res, err)
			}
			if got := s.action(t, "a1"); got.Status.Message != "robot commit failed after task assignment; reverted to Pending" {
				t.Fatalf("action %s on %q (%q): the robot commit did not fail on the version it read",
					got.Status.Phase, got.Status.AssignedRobot, got.Status.Message)
			}
			if hook.requests != 1 {
				t.Fatalf("the TDE was asked %d times; the write is injected inside that request", hook.requests)
			}
			var rb fleetv1.Robot
			if err := s.api.Get(ctx, types.NamespacedName{Namespace: actionNS, Name: "r1"}, &rb); err != nil {
				t.Fatal(err)
			}
			if rb.Status.AssignedAction != "" {
				t.Fatalf("robot records %q after a failed commit: dispatched", rb.Status.AssignedAction)
			}
			got := s.action(t, "a1")
			if got.Status.Phase != fleetv1.ActionPhasePending || got.Status.AssignedRobot != "" {
				t.Fatalf("action %s on %q; want Pending, unassigned", got.Status.Phase, got.Status.AssignedRobot)
			}
			if hook.releases != hook.requests {
				t.Fatalf("%d TDE slots taken, %d released: a reservation is left behind", hook.requests, hook.releases)
			}
		})
	}
}

// A re-read that failed verifies nothing: currentCandidate falls back to the cached copy, and the
// confirmation reads the robot itself, so a stop written onto it still holds the assignment.
func TestStopConfirmation_AFailedReReadIsNotTakenAsVerified(t *testing.T) {
	cached := freeRobotAt("r1", "", "yard")
	live := cached.DeepCopy()
	live.Status.EstopState = fleetv1.RobotEstopStopping
	s := newStaleCache(t, []client.Object{zoneObj("yard", ""), cached, pendingTo("a1", "yard")}, live)
	failing := &stopReadCounter{Reader: s.api, fail: func(verb string, obj runtime.Object) error {
		if _, ok := obj.(*fleetv1.Robot); ok && verb == "get" {
			return errors.New("injected")
		}
		return nil
	}}
	s.r.APIReader = failing
	s.reconcile(t, "a1")
	if got := s.action(t, "a1"); got.Status.AssignedRobot != "" {
		t.Fatalf("assigned to %q: a failed re-read was taken as verified", got.Status.AssignedRobot)
	}
	if s.confirm.reads["get *v1.Robot"] != 1 {
		t.Errorf("the confirmation did not read the robot itself: %v", s.confirm.reads)
	}
	if s.tde.requests != 0 {
		t.Errorf("the TDE was asked %d times", s.tde.requests)
	}
}

// The destination chain is judged in its own right, not only through the robot's chain. While
// filter 2 runs, a candidate stands inside the destination, so its own chain covers the
// destination too; this test calls the confirmation directly, with the robot outside the
// destination, so the destination check stays covered if filter 2 ever changes.
func TestStopConfirmation_TheDestinationChainIsJudgedOnItsOwn(t *testing.T) {
	ctx := context.Background()
	robot := freeRobotAt("r1", "", "yard")
	for _, tc := range []struct {
		name      string
		action    *fleetv1.FleetAction
		live      client.Object
		wantsHold bool
	}{
		{"a stop on the destination", pendingTo("a1", "bay"), triggeredZone("bay", "floor"), true},
		{"a stop on an ancestor that propagates", pendingTo("a1", "bay"), triggeredZone("floor", "site"), true},
		{"no stop on the destination's chain", pendingTo("a1", "bay"), triggeredZone("yard2", ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStaleCache(t, []client.Object{zoneObj("site", ""), zoneObj("floor", "site"), zoneObj("bay", "floor"),
				zoneObj("yard", ""), robot}, tc.live)
			why, err := s.r.confirmNoStop(ctx, tc.action, robot, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			if (why != "") != tc.wantsHold {
				t.Fatalf("why %q; want a hold: %v", why, tc.wantsHold)
			}
		})
	}
}

// The uncached-pass mark of an action that is gone is cleared: the map does not grow with actions
// deleted after a confirmation found a stop.
func TestStopConfirmation_TheMarkOfAGoneActionIsCleared(t *testing.T) {
	s := newStaleCache(t, []client.Object{zoneObj("bay", "")})
	key := types.NamespacedName{Namespace: actionNS, Name: "gone"}
	s.r.uncachedNext.mark(key)
	if _, err := s.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if s.r.uncachedNext.has(key) {
		t.Fatal("the mark of an action that no longer exists was kept")
	}
}
