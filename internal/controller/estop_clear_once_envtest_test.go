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

// A pass that reads its carrier one write behind the clear (the trigger gone, the processed
// marker's removal not yet in the manager's cache) must not clear the stop again: a clear writes
// one ESTOP_CLEARED entry (RFC-0001 #safety-required-events), and a robot's second clear must not
// drop the record of a stop that came into force after the first. The stale read is handed to the
// reconciler by the test, so no race is needed.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
)

func (rig *onceRig) cleared() int {
	rig.aud.mu.Lock()
	defer rig.aud.mu.Unlock()
	n := 0
	for _, e := range rig.aud.entries {
		if e.EventType == audit.EventEstopCleared {
			n++
		}
	}
	return n
}

// setAnnotation sets the annotation on obj in the API server, or removes it when value is "".
func setAnnotation(t *testing.T, obj client.Object, key, value string) {
	t.Helper()
	if err := envK8s.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	base := obj.DeepCopyObject().(client.Object)
	an := obj.GetAnnotations()
	if an == nil {
		an = map[string]string{}
	}
	if value == "" {
		delete(an, key)
	} else {
		an[key] = value
	}
	obj.SetAnnotations(an)
	if err := envK8s.Patch(context.Background(), obj, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

// waitState waits for the robot's estopState.
func (rig *onceRig) waitState(t *testing.T, name string, want fleetv1.RobotEstopState) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); getEnvRobot(t, rig.ns, name).Status.EstopState != want; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("robot %s: estopState %s, want %s", name, getEnvRobot(t, rig.ns, name).Status.EstopState, want)
		}
	}
}

// clearTwice triggers on the carrier, reconciles, clears, and reconciles twice: the first pass
// reads the API server, the second the copy from before the first pass removed the marker.
func clearTwice(t *testing.T, rig *onceRig, r reconcileFunc, req ctrl.Request, carrier client.Object, robots ...string) {
	t.Helper()
	setAnnotation(t, carrier, annEstopTriggered, "person detected")
	if _, err := r(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, robots...)
	setAnnotation(t, carrier, annEstopTriggered, "")
	stale := carrier.DeepCopyObject().(client.Object)
	if err := envK8s.Get(context.Background(), req.NamespacedName, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := r(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.c.hold(stale)
	if _, err := r(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

type reconcileFunc func(context.Context, ctrl.Request) (ctrl.Result, error)

func TestClearOnce_ZoneClearedOnceWhenTheCacheIsOneWriteBehind(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1", "amr-2")
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: rig.ns}}
	if err := envK8s.Create(context.Background(), zone); err != nil {
		t.Fatal(err)
	}
	r := rig.zoneReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "bay"}}
	clearTwice(t, rig, r.Reconcile, req, zone, "amr-1", "amr-2")
	if got := rig.cleared(); got != 1 {
		t.Errorf("ESTOP_CLEARED entries = %d, want 1 for one clear", got)
	}
}

func TestClearOnce_NamespaceClearedOnceWhenTheCacheIsOneWriteBehind(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1", "amr-2")
	cfg := &fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Namespace: rig.ns, Name: "swarmada-config"}}
	if err := envK8s.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	r := &NamespaceEstopReconciler{Client: rig.c, Scheme: envScheme, Estopper: rig.disp, Audit: rig.aud, APIReader: envK8s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "swarmada-config"}}
	clearTwice(t, rig, r.Reconcile, req, cfg, "amr-1", "amr-2")
	if got := rig.cleared(); got != 1 {
		t.Errorf("ESTOP_CLEARED entries = %d, want 1 for one clear", got)
	}
}

func TestClearOnce_RobotClearedOnceWhenTheCacheIsOneWriteBehind(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1")
	r := &RobotEstopReconciler{Client: rig.c, Scheme: envScheme, Estopper: rig.disp, Audit: rig.aud, APIReader: envK8s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "amr-1"}}
	clearTwice(t, rig, r.Reconcile, req, getEnvRobot(t, rig.ns, "amr-1"), "amr-1")
	if got := rig.cleared(); got != 1 {
		t.Errorf("ESTOP_CLEARED entries = %d, want 1 for one clear", got)
	}
}

// A zone's stop that comes into force between a robot's clear and a pass that reads the robot from
// before that clear keeps its record on the robot: the second pass must not clear the robot again
// and drop records it did not read.
func TestClearOnce_RobotClearBehindKeepsAZoneStopsRecord(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1")
	if err := envK8s.Create(context.Background(), &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: rig.ns}}); err != nil {
		t.Fatal(err)
	}
	r := &RobotEstopReconciler{Client: rig.c, Scheme: envScheme, Estopper: rig.disp, Audit: rig.aud, APIReader: envK8s}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "amr-1"}}
	robot := getEnvRobot(t, rig.ns, "amr-1")
	setAnnotation(t, robot, annEstopTriggered, "person detected")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, "amr-1")
	setAnnotation(t, robot, annEstopTriggered, "")
	stale := getEnvRobot(t, rig.ns, "amr-1")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitState(t, "amr-1", fleetv1.RobotEstopResuming)

	setEstopTrigger(t, rig.ns, "bay", "spill")
	zr := rig.zoneReconciler()
	if _, err := zr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "bay"}}); err != nil {
		t.Fatal(err)
	}
	rig.waitState(t, "amr-1", fleetv1.RobotEstopStopped)
	if holds := getEnvRobot(t, rig.ns, "amr-1").Status.EstopHeldBy; len(holds) != 1 || holds[0].Kind != fleetv1.EstopHoldFleetZone {
		t.Fatalf("before the second pass the robot records %v, want the zone's stop", holds)
	}

	rig.c.hold(stale)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rb := getEnvRobot(t, rig.ns, "amr-1")
	if len(rb.Status.EstopHeldBy) != 1 || rb.Status.EstopHeldBy[0].Kind != fleetv1.EstopHoldFleetZone || rb.Status.EstopHeldBy[0].Name != "bay" {
		t.Errorf("after the second pass the robot records %v, want the zone's stop", rb.Status.EstopHeldBy)
	}
	if rb.Status.EstopState != fleetv1.RobotEstopStopped {
		t.Errorf("estopState = %s, want Stopped", rb.Status.EstopState)
	}
	if got := rig.cleared(); got != 1 {
		t.Errorf("ESTOP_CLEARED entries = %d, want 1", got)
	}
}

// A marker the API server still holds is a clear not yet applied, whatever this process
// remembers: the clear runs.
func TestClearOnce_MarkerReallyThereClearsAgain(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1")
	zone := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: rig.ns}}
	if err := envK8s.Create(context.Background(), zone); err != nil {
		t.Fatal(err)
	}
	r := rig.zoneReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "bay"}}
	setEstopTrigger(t, rig.ns, "bay", "person detected")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rig.waitStops(t, 1, "amr-1")
	setEstopTrigger(t, rig.ns, "bay", "")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	setAnnotation(t, zone, annEstopProcessed, "person detected")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := rig.cleared(); got != 2 {
		t.Errorf("ESTOP_CLEARED entries = %d, want 2: the marker is there, so the clear runs", got)
	}
	if _, marked := treeZone(t, rig.ns, "bay").Annotations[annEstopProcessed]; marked {
		t.Error("the processed marker is still on the zone after the clear")
	}
}

// The same value triggered and cleared twice is two stops and two clears.
func TestClearOnce_SameValueTwiceIsTwoClears(t *testing.T) {
	requireEnvtest(t)
	rig := newOnceRig(t, "amr-1")
	if err := envK8s.Create(context.Background(), &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: "bay", Namespace: rig.ns}}); err != nil {
		t.Fatal(err)
	}
	r := rig.zoneReconciler()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rig.ns, Name: "bay"}}
	for i := 1; i <= 2; i++ {
		setEstopTrigger(t, rig.ns, "bay", "person detected")
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		rig.waitState(t, "amr-1", fleetv1.RobotEstopStopped)
		setEstopTrigger(t, rig.ns, "bay", "")
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		rig.waitState(t, "amr-1", fleetv1.RobotEstopResuming)
	}
	if got, sent := rig.cleared(), rig.sent(); got != 2 || sent != 2 {
		t.Errorf("ESTOP_CLEARED entries = %d, Estops = %d; want 2 and 2", got, sent)
	}
}
