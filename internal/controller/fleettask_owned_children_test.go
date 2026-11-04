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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A FleetTask acts only on the FleetActions it controls. The deterministic child name finds
// a child; it does not make one. An object of that name the task does not control is a name
// conflict: the task reports it once (ChildrenAdmitted False, one Warning Event), holds the
// member, and writes nothing to that object.
//
// The reason and Event strings are literals on purpose, as in fleettask_child_refused_test.go:
// they are what an operator and a script read.

const ownedNS = "dock-7"

type ownedFixture struct {
	r       *FleetTaskReconciler
	c       client.Client
	rec     *record.FakeRecorder
	writes  map[string]int // spec/metadata writes per FleetAction name
	creates map[string]int // FleetAction creates per name
}

func newOwnedFixture(t *testing.T, objs ...client.Object) ownedFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	f := ownedFixture{writes: map[string]int{}, creates: map[string]int{}}
	f.c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*fleetv1.FleetAction); ok {
					f.writes[obj.GetName()]++
				}
				return cl.Patch(ctx, obj, p, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*fleetv1.FleetAction); ok {
					f.writes[obj.GetName()]++
				}
				return cl.Update(ctx, obj, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*fleetv1.FleetAction); ok {
					f.creates[obj.GetName()]++
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	f.rec = record.NewFakeRecorder(32)
	f.r = &FleetTaskReconciler{Client: f.c, Scheme: scheme, Recorder: f.rec}
	return f
}

func (f ownedFixture) reconcile(t *testing.T, name string, passes int) {
	t.Helper()
	for i := 0; i < passes; i++ {
		_, _ = f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ownedNS, Name: name}})
	}
}

func (f ownedFixture) task(t *testing.T, name string) *fleetv1.FleetTask {
	t.Helper()
	var ft fleetv1.FleetTask
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: ownedNS, Name: name}, &ft); err != nil {
		t.Fatalf("reading task: %v", err)
	}
	return &ft
}

func (f ownedFixture) action(t *testing.T, name string) *fleetv1.FleetAction {
	t.Helper()
	var fa fleetv1.FleetAction
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: ownedNS, Name: name}, &fa); err != nil {
		t.Fatalf("reading action %s: %v", name, err)
	}
	return &fa
}

// events counts the recorded Events whose text contains reason.
func (f ownedFixture) events(reason string) int {
	n := 0
	for {
		select {
		case e := <-f.rec.Events:
			if strings.Contains(e, reason) {
				n++
			}
		default:
			return n
		}
	}
}

func (f ownedFixture) assertConflict(t *testing.T, taskName, member, actionName string) {
	t.Helper()
	c := apimeta.FindStatusCondition(f.task(t, taskName).Status.Conditions, ConditionChildrenAdmitted)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NameConflict" ||
		!strings.Contains(c.Message, actionName) || !strings.Contains(c.Message, member) {
		t.Errorf("ChildrenAdmitted does not report the name conflict of %q with %q: %+v", member, actionName, c)
	}
	if n := f.events("ChildNameConflict"); n != 1 {
		t.Errorf("want exactly one ChildNameConflict Event over every pass, got %d", n)
	}
	if n := f.writes[actionName]; n != 0 {
		t.Errorf("the task wrote %d time(s) to %s, which it does not own", n, actionName)
	}
}

func ownedTask(name string, desired fleetv1.DesiredState, members ...string) *fleetv1.FleetTask {
	ft := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Namespace: ownedNS, Name: name, UID: types.UID(name + "-uid"), Generation: 1},
		Spec:       fleetv1.FleetTaskSpec{DesiredState: desired, CompletionPolicy: fleetv1.CompletionPolicyAll},
	}
	for _, m := range members {
		ft.Spec.Actions = append(ft.Spec.Actions, fleetv1.FleetTaskAction{Name: m,
			Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: fleetv1.ActionFailureAlert}})
	}
	return ft
}

// foreignAction is an action of name another principal created, outside the task.
func foreignAction(name string, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: ownedNS, Name: name, UID: types.UID(name + "-uid")},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, DesiredState: fleetv1.DesiredStateRunning},
		Status:     fleetv1.FleetActionStatus{Phase: phase},
	}
}

// childOf is a FleetAction the task controls.
func childOf(task *fleetv1.FleetTask, name string, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	yes := true
	fa := foreignAction(name, phase)
	fa.OwnerReferences = []metav1.OwnerReference{{APIVersion: fleetv1.GroupVersion.String(), Kind: "FleetTask",
		Name: task.Name, UID: task.UID, Controller: &yes, BlockOwnerDeletion: &yes}}
	return fa
}

func TestFleetTask_DoesNotAdoptAnActionItDoesNotOwn(t *testing.T) {
	task := ownedTask("bay", fleetv1.DesiredStateCancelled, "restock")
	f := newOwnedFixture(t, task, foreignAction("bay-restock", fleetv1.ActionPhasePending))
	f.reconcile(t, "bay", 3)
	if got := f.action(t, "bay-restock").Spec.DesiredState; got != fleetv1.DesiredStateRunning {
		t.Errorf("the task's desired state reached an action it does not own: %q", got)
	}
	if n := f.creates["bay-restock"]; n != 0 {
		t.Errorf("the task tried to create over a name it does not own %d time(s)", n)
	}
	f.assertConflict(t, "bay", "restock", "bay-restock")
}

func TestFleetTask_FlowDoesNotAdoptAnActionItDoesNotOwn(t *testing.T) {
	task := ownedTask("yard", fleetv1.DesiredStateCancelled, "scan")
	run := "scan"
	task.Spec.Flow = &fleetv1.FlowNode{Run: &run}
	f := newOwnedFixture(t, task, foreignAction("yard-scan", fleetv1.ActionPhasePending))
	f.reconcile(t, "yard", 3)
	if got := f.action(t, "yard-scan").Spec.DesiredState; got != fleetv1.DesiredStateRunning {
		t.Errorf("the flow task's desired state reached an action it does not own: %q", got)
	}
	f.assertConflict(t, "yard", "scan", "yard-scan")
}

func TestFleetTask_CompensationIsNotReadFromAnActionItDoesNotOwn(t *testing.T) {
	task := ownedTask("saga", fleetv1.DesiredStateRunning, "load", "lift")
	comp := fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: fleetv1.ActionFailureAlert}
	task.Spec.Actions[0].Compensation = &comp
	task.Spec.FailurePolicy = fleetv1.FailurePolicyCompensate
	f := newOwnedFixture(t, task,
		childOf(task, "saga-load", fleetv1.ActionPhaseSucceeded),
		childOf(task, "saga-lift", fleetv1.ActionPhaseFailed),
		foreignAction("saga-load-comp", fleetv1.ActionPhaseSucceeded))
	f.reconcile(t, "saga", 3)
	got := f.task(t, "saga")
	if got.Status.Phase == fleetv1.FleetTaskPhaseCompensated {
		t.Errorf("the task reported its compensation complete from an action it does not own")
	}
	if got.Status.Phase != fleetv1.FleetTaskPhaseCompensating {
		t.Errorf("the compensation should be held while its name is taken; phase %q", got.Status.Phase)
	}
	f.assertConflict(t, "saga", "load", "saga-load-comp")
}

func TestFleetTask_AdoptsTheChildItOwns(t *testing.T) {
	task := ownedTask("own", fleetv1.DesiredStatePaused, "a")
	f := newOwnedFixture(t, task, childOf(task, "own-a", fleetv1.ActionPhasePending))
	f.reconcile(t, "own", 2)
	if got := f.action(t, "own-a").Spec.DesiredState; got != fleetv1.DesiredStatePaused {
		t.Errorf("the task's own child was not adopted: desiredState %q", got)
	}
	if n := f.creates["own-a"]; n != 0 {
		t.Errorf("the task re-created its own child %d time(s)", n)
	}
	if c := apimeta.FindStatusCondition(f.task(t, "own").Status.Conditions, ConditionChildrenAdmitted); c != nil && c.Status == metav1.ConditionFalse {
		t.Errorf("the task reports a conflict with its own child: %+v", c)
	}
	if n := f.events("ChildNameConflict"); n != 0 {
		t.Errorf("a ChildNameConflict Event for the task's own child")
	}
}

// The name conflict clears once the name is free: the next pass creates the task's own child.
func TestFleetTask_StartsTheMemberOnceTheNameIsFree(t *testing.T) {
	task := ownedTask("lane", fleetv1.DesiredStateRunning, "sweep")
	f := newOwnedFixture(t, task, foreignAction("lane-sweep", fleetv1.ActionPhasePending))
	f.reconcile(t, "lane", 2)
	if err := f.c.Delete(context.Background(), f.action(t, "lane-sweep")); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, "lane", 2)
	if !metav1.IsControlledBy(f.action(t, "lane-sweep"), f.task(t, "lane")) {
		t.Errorf("the member did not start once its name was free")
	}
	if c := apimeta.FindStatusCondition(f.task(t, "lane").Status.Conditions, ConditionChildrenAdmitted); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("ChildrenAdmitted not taken back once the name was free: %+v", c)
	}
}

// Retention prunes only the iteration children the task controls. A terminal action that
// carries the task's labels but not its controller reference is left in place.
func TestFleetTask_RetentionPrunesOnlyOwnedChildren(t *testing.T) {
	task := ownedTask("loop", fleetv1.DesiredStateRunning, "pick")
	foreign := foreignAction("other-pick", fleetv1.ActionPhaseSucceeded)
	foreign.Labels = map[string]string{ownedByLabel: "loop", memberLabel: "pick"}
	own := childOf(task, "loop-pick-i1", fleetv1.ActionPhaseSucceeded)
	own.Labels = map[string]string{ownedByLabel: "loop", memberLabel: "pick"}
	f := newOwnedFixture(t, task, foreign, own)
	if err := f.r.pruneIterationChildren(context.Background(), task, "pick", fleetv1.FlowRetention{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var list fleetv1.FleetActionList
	if err := f.c.List(context.Background(), &list, client.InNamespace(ownedNS)); err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	for _, a := range list.Items {
		left[a.Name] = true
	}
	if !left["other-pick"] {
		t.Errorf("retention deleted an action the task does not own")
	}
	if left["loop-pick-i1"] {
		t.Errorf("retention kept an owned terminal child past a zero window")
	}
}
