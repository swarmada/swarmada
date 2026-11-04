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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A cleared task whose child the API server refuses must say so ON THE TASK.
//
// A person who clears a draft has authorised physical work. When admission then
// refuses the child — the FleetAction catalog pre-filter refusing an action type no
// connected adapter advertises is the case met on a live cluster — the task used to
// sit at Pending with no condition, no Event and no message, and the reason existed
// only in the manager log. These tests hold the task to naming the refusal and the
// action, writing it once (RA-1: on the transition, not on every retry), and taking
// it back when the cause clears.
//
// The condition and reason strings are written as literals on purpose: the wire
// strings are what an operator and a script read, so a rename is a contract change
// the test should notice.

const refusedNS = "warehouse-a"

// refusalText is the shape of the FleetAction webhook's catalog refusal.
const refusalText = `action type "Clean" is in no FleetAdapter's status.supportedActions in namespace "warehouse-a" (every adapter's recorded catalog is checked, connected or not)`

type refusalFixture struct {
	r       *FleetTaskReconciler
	c       client.Client
	rec     *record.FakeRecorder
	refuse  *error // the error a FleetAction create returns; nil admits it
	patches *int   // FleetTask status writes
}

func newRefusalFixture(t *testing.T, task *fleetv1.FleetTask) refusalFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	var refuse error = apierrors.NewForbidden(
		schema.GroupResource{Group: "swarmada.io", Resource: "fleetactions"},
		task.Name+"-clean", errors.New(refusalText))
	patches := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*fleetv1.FleetAction); ok && refuse != nil {
					return refuse
				}
				return cl.Create(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string,
				obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*fleetv1.FleetTask); ok {
					patches++
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	rec := record.NewFakeRecorder(32)
	return refusalFixture{
		r: &FleetTaskReconciler{Client: c, Scheme: scheme, Recorder: rec},
		c: c, rec: rec, refuse: &refuse, patches: &patches,
	}
}

// clearedCleanTask is a drafted Clean a person has cleared: spec.draft false, and the
// status the draft gate left behind (phase Draft, AwaitingActivation True).
func clearedCleanTask(flow bool) *fleetv1.FleetTask {
	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "workorder-clean", Namespace: refusedNS, Generation: 2},
		Spec: fleetv1.FleetTaskSpec{
			Draft:        false,
			DesiredState: fleetv1.DesiredStateRunning,
			Actions: []fleetv1.FleetTaskAction{{
				Name: "clean",
				Action: fleetv1.FleetActionSpec{
					Type: fleetv1.ActionTypeClean, Zone: "warehouse-a", RobotSelector: "vacuum-01",
				},
			}},
		},
		Status: fleetv1.FleetTaskStatus{
			Phase: fleetv1.FleetTaskPhaseDraft,
			Conditions: []metav1.Condition{{
				Type: ConditionAwaitingActivation, Status: metav1.ConditionTrue,
				Reason: "DraftAwaitingActivation", LastTransitionTime: metav1.Now(),
			}},
		},
	}
	if flow {
		run := "clean"
		task.Spec.Flow = &fleetv1.FlowNode{Run: &run}
	}
	return task
}

func (f refusalFixture) reconcile(t *testing.T) error {
	t.Helper()
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "workorder-clean", Namespace: refusedNS},
	})
	return err
}

func (f refusalFixture) task(t *testing.T) *fleetv1.FleetTask {
	t.Helper()
	var ft fleetv1.FleetTask
	if err := f.c.Get(context.Background(),
		types.NamespacedName{Name: "workorder-clean", Namespace: refusedNS}, &ft); err != nil {
		t.Fatalf("get task: %v", err)
	}
	return &ft
}

func drainRecorded(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func countPrefixed(events []string, prefix string) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

func assertRefusalVisible(t *testing.T, f refusalFixture) {
	t.Helper()
	got := apimeta.FindStatusCondition(f.task(t).Status.Conditions, "ChildrenAdmitted")
	if got == nil {
		t.Fatal("the API server refused the task's child and the task carries no ChildrenAdmitted " +
			"condition. The person who cleared this draft sees Pending with nothing saying it " +
			"will never start; the reason is only in the manager log")
	}
	if got.Status != metav1.ConditionFalse || got.Reason != "AdmissionRefused" {
		t.Errorf("ChildrenAdmitted = %s/%s, want False/AdmissionRefused", got.Status, got.Reason)
	}
	for _, want := range []string{`"clean"`, `Clean`, "supportedActions"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("condition message %q does not contain %q: it must name the action and the refusal", got.Message, want)
		}
	}
}

func testRefusalIsVisibleOnceAndClears(t *testing.T, flow bool) {
	f := newRefusalFixture(t, clearedCleanTask(flow))

	if err := f.reconcile(t); err == nil {
		t.Fatal("reconcile returned nil after admission refused the child; the refusal must stay " +
			"a reconcile error so the create is retried when the adapter's catalog changes")
	}
	assertRefusalVisible(t, f)
	events := drainRecorded(f.rec)
	if n := countPrefixed(events, "Warning ChildAdmissionRefused"); n != 1 {
		t.Fatalf("Warning ChildAdmissionRefused events = %d, want 1 (events: %q)", n, events)
	}
	if !strings.Contains(strings.Join(events, "\n"), "supportedActions") {
		t.Errorf("the Warning Event does not carry the admission message: %q", events)
	}

	// The same refusal again: RA-1 — no status write and no second Event.
	before := *f.patches
	for i := 0; i < 3; i++ {
		if err := f.reconcile(t); err == nil {
			t.Fatal("a repeated refusal returned nil")
		}
	}
	if *f.patches != before {
		t.Errorf("an unchanged refusal wrote the task's status %d more time(s); RA-1 allows a write "+
			"on the transition only", *f.patches-before)
	}
	if n := countPrefixed(drainRecorded(f.rec), "Warning ChildAdmissionRefused"); n != 0 {
		t.Errorf("an unchanged refusal emitted %d more Warning Event(s), want 0", n)
	}

	// The cause clears: the catalog now admits the child.
	*f.refuse = nil
	if err := f.reconcile(t); err != nil {
		t.Fatalf("reconcile after the refusal cleared: %v", err)
	}
	var child fleetv1.FleetAction
	if err := f.c.Get(context.Background(),
		types.NamespacedName{Name: "workorder-clean-clean", Namespace: refusedNS}, &child); err != nil {
		t.Fatalf("the child was not created once admission allowed it: %v", err)
	}
	got := apimeta.FindStatusCondition(f.task(t).Status.Conditions, "ChildrenAdmitted")
	if got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("ChildrenAdmitted = %+v after the child was admitted, want True: a refusal that "+
			"no longer applies must not go on being reported", got)
	}
}

func TestFleetTask_RefusedChildIsVisibleOnTheTask_DependsOnPath(t *testing.T) {
	testRefusalIsVisibleOnceAndClears(t, false)
}

func TestFleetTask_RefusedChildIsVisibleOnTheTask_FlowPath(t *testing.T) {
	testRefusalIsVisibleOnceAndClears(t, true)
}

// A transient failure is not a refusal. An API server that could not be reached, or a
// webhook that timed out, says nothing about whether the child is acceptable, so it
// stays a retried reconcile error and puts no condition on the task.
func TestFleetTask_TransientCreateErrorIsNotReportedAsARefusal(t *testing.T) {
	f := newRefusalFixture(t, clearedCleanTask(false))
	*f.refuse = apierrors.NewInternalError(errors.New(`failed calling webhook "vfleetaction.swarmada.io": context deadline exceeded`))

	if err := f.reconcile(t); err == nil {
		t.Fatal("reconcile returned nil on a failed create")
	}
	if c := apimeta.FindStatusCondition(f.task(t).Status.Conditions, "ChildrenAdmitted"); c != nil {
		t.Errorf("a transient create failure was reported as %s/%s; only an admission refusal is", c.Status, c.Reason)
	}
	if n := countPrefixed(drainRecorded(f.rec), "Warning ChildAdmissionRefused"); n != 0 {
		t.Errorf("a transient create failure emitted %d refusal Event(s)", n)
	}
}

// A task that never had a refusal carries no ChildrenAdmitted condition at all, so the
// fix adds nothing to the status of the tasks it does not concern.
func TestFleetTask_NoRefusalWritesNoChildrenAdmittedCondition(t *testing.T) {
	f := newRefusalFixture(t, clearedCleanTask(false))
	*f.refuse = nil
	if err := f.reconcile(t); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if c := apimeta.FindStatusCondition(f.task(t).Status.Conditions, "ChildrenAdmitted"); c != nil {
		t.Errorf("a task whose child was admitted first time carries ChildrenAdmitted %s/%s", c.Status, c.Reason)
	}
}
