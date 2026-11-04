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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// Row A15 (RFC-0001 #safety-action-table): the control plane MUST NOT dispatch an action whose
// destination, its spec.zone, is in a zone under a stop, that zone or one above it whose stop
// propagates to children. Such an action stays Pending with the condition ZoneStopped, and a
// Warning Event ZoneStopped names the zone. The robot stands in the destination zone, as candidate
// filter 2 requires; with a stop there the action holds before candidate selection, with the
// condition, and when no stop reaches the zone it is dispatched.

func stopZone(name, parent string, trigger, processed bool, propagate *bool) *fleetv1.FleetZone {
	z := &fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS, Annotations: map[string]string{}},
		Spec: fleetv1.FleetZoneSpec{ParentZone: parent}}
	if trigger {
		z.Annotations[annEstopTriggered] = "hazard"
	}
	if processed {
		z.Annotations[annEstopProcessed] = "hazard"
	}
	if propagate != nil {
		z.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: propagate}
	}
	return z
}

func zoneStoppedRig(t *testing.T, failList bool, objs ...client.Object) (*FleetActionReconciler, client.Client, *record.FakeRecorder, *fakeCommander) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs = append([]client.Object{readyActionAdapter(), stopZone("out", "", false, false, nil),
		idleRobotInZone("r1", "z")}, objs...)
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{})
	if failList {
		b = b.WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, o ...client.ListOption) error {
			if _, ok := l.(*fleetv1.FleetZoneList); ok {
				return errors.New("etcdserver: request timed out")
			}
			return cl.List(ctx, l, o...)
		}})
	}
	c := b.Build()
	rec := record.NewFakeRecorder(20)
	fc := &fakeCommander{}
	r := &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler(), Recorder: rec, Commander: fc}
	return r, c, rec, fc
}

func zoneStoppedEvents(rec *record.FakeRecorder) int {
	n := 0
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "Warning "+conditionZoneStopped+" ") {
				n++
			}
		default:
			return n
		}
	}
}

func TestDispatch_ZoneStopped_HoldsAnActionWhoseDestinationIsUnderAStop(t *testing.T) {
	no, yes := false, true
	for _, tc := range []struct {
		name   string
		zones  []client.Object
		fail   bool
		held   bool
		naming string
	}{
		{"destination-stopped", []client.Object{stopZone("z", "", true, false, nil)}, false, true, "zone z"},
		{"parent-stopped-propagating", []client.Object{stopZone("p", "", true, false, &yes), stopZone("z", "p", false, false, nil)}, false, true, "p"},
		{"parent-stopped-not-propagating", []client.Object{stopZone("p", "", true, false, &no), stopZone("z", "p", false, false, nil)}, false, false, ""},
		{"processed-marker-without-trigger", []client.Object{stopZone("z", "", false, true, nil)}, false, true, "zone z"},
		{"zone-tree-unreadable", []client.Object{stopZone("z", "", false, false, nil)}, true, true, "could not be read"},
		{"no-stop", []client.Object{stopZone("z", "", false, false, nil)}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, rec, fc := zoneStoppedRig(t, tc.fail, append(tc.zones, zonedAction("t1", "z"))...)
			reconcileAction(t, r, "t1")
			reconcileAction(t, r, "t1")
			a := getAction(t, c, "t1")
			cond := apimeta.FindStatusCondition(a.Status.Conditions, conditionZoneStopped)
			if !tc.held && tc.name == "parent-stopped-not-propagating" {
				// The robot under the parent is withheld by its own filter (10); the destination
				// is not under a stop that reaches it, so no ZoneStopped is written.
				if cond != nil {
					t.Fatalf("ZoneStopped written for a parent stop that does not propagate down: %+v", cond)
				}
				return
			}
			if !tc.held {
				if a.Status.Phase != fleetv1.ActionPhaseAssigned || cond != nil || len(fc.assigns) == 0 {
					t.Fatalf("not under a stop: phase %s, condition %+v, %d assigns; want Assigned with no ZoneStopped",
						a.Status.Phase, cond, len(fc.assigns))
				}
				return
			}
			if a.Status.Phase != fleetv1.ActionPhasePending || a.Status.AssignedRobot != "" || len(fc.assigns) != 0 {
				t.Fatalf("INVARIANT: phase %s robot %q, %d assigns: an action whose destination is under a stop was dispatched",
					a.Status.Phase, a.Status.AssignedRobot, len(fc.assigns))
			}
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditionZoneStopped || !strings.Contains(cond.Message, tc.naming) {
				t.Fatalf("condition %+v; want ZoneStopped True naming %q", cond, tc.naming)
			}
			if n := zoneStoppedEvents(rec); n != 1 {
				t.Fatalf("%d ZoneStopped Warning Events over two passes, want 1", n)
			}
		})
	}
}

// The condition holds back A15 only: an unstarted action past its deadline still fails (A16).
func TestDispatch_ZoneStopped_TheDeadlineStillFires(t *testing.T) {
	a := zonedAction("t1", "z")
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	a.Spec.Deadline = &past
	r, c, _, _ := zoneStoppedRig(t, false, stopZone("z", "", true, false, nil), a)
	reconcileAction(t, r, "t1")
	reconcileAction(t, r, "t1")
	if got := getAction(t, c, "t1"); got.Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase %s; want Failed: the deadline (A16) is not held back by ZoneStopped", got.Status.Phase)
	}
}

// Once the destination is no longer under a stop the action is dispatched, and the condition is
// gone from the same write.
func TestDispatch_ZoneStopped_IsRemovedWhenTheStopIsCleared(t *testing.T) {
	r, c, _, fc := zoneStoppedRig(t, false, stopZone("z", "", true, false, nil), zonedAction("t1", "z"))
	reconcileAction(t, r, "t1")
	if cond := apimeta.FindStatusCondition(getAction(t, c, "t1").Status.Conditions, conditionZoneStopped); cond == nil {
		t.Fatal("setup: no ZoneStopped condition while the destination is under a stop")
	}
	z := &fleetv1.FleetZone{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: actionNS, Name: "z"}, z); err != nil {
		t.Fatal(err)
	}
	z.Annotations = nil
	if err := c.Update(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	reconcileAction(t, r, "t1")
	a := getAction(t, c, "t1")
	if a.Status.Phase != fleetv1.ActionPhaseAssigned || len(fc.assigns) != 1 {
		t.Fatalf("phase %s, %d assigns after the clear; want Assigned", a.Status.Phase, len(fc.assigns))
	}
	if cond := apimeta.FindStatusCondition(a.Status.Conditions, conditionZoneStopped); cond != nil {
		t.Fatalf("ZoneStopped outlived the stop: %+v", cond)
	}
}
