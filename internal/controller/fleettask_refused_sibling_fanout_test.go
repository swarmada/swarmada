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

// A refused child must not stop the task's other children from hearing it.
//
// A refusal is a statement about one child. The task's desiredState is a statement
// about all of them (RFC-0001 #fleettask-declarative-desired-state), and `swarmctl
// cancel task` tells the operator every member will be cancelled. These hold the
// fan-out to reaching every child that exists while a sibling's create is refused,
// and the refusal to still being reported and retried.

// refusingCleanClient admits every create except a Clean FleetAction, which it
// refuses the way the FleetAction webhook's catalog pre-filter does.
func refusingCleanClient(t *testing.T, objs ...client.Object) (client.Client, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if fa, ok := obj.(*fleetv1.FleetAction); ok && fa.Spec.Type == fleetv1.ActionTypeClean {
					return apierrors.NewForbidden(schema.GroupResource{Group: "swarmada.io", Resource: "fleetactions"},
						fa.Name, errors.New(refusalText))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	return c, scheme
}

func runningChild(task, member string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: childName(task, member), Namespace: refusedNS,
			Labels:          map[string]string{ownedByLabel: task, memberLabel: member},
			OwnerReferences: []metav1.OwnerReference{fakeTaskControllerRef(task)}},
		Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "warehouse-a",
			DesiredState: fleetv1.DesiredStateRunning},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress, AssignedRobot: "amr-01"},
	}
}

func reconcileRefusedSibling(t *testing.T, c client.Client, scheme *runtime.Scheme, task string) {
	t.Helper()
	r := &FleetTaskReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(32)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: refusedNS, Name: task}})
	if err == nil {
		t.Fatal("reconcile returned nil although a child was refused; the refusal must stay a " +
			"reconcile error so the create is retried")
	}
	var ft fleetv1.FleetTask
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: refusedNS, Name: task}, &ft); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if cond := apimeta.FindStatusCondition(ft.Status.Conditions, ConditionChildrenAdmitted); cond == nil ||
		cond.Status != metav1.ConditionFalse {
		t.Errorf("ChildrenAdmitted = %+v, want False: the refusal must still be on the task", cond)
	}
}

func childDesiredState(t *testing.T, c client.Client, name string) fleetv1.DesiredState {
	t.Helper()
	var fa fleetv1.FleetAction
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: refusedNS, Name: name}, &fa); err != nil {
		t.Fatalf("get child %s: %v", name, err)
	}
	return fa.Spec.DesiredState
}

// dependsOn path: a running member and an eligible sibling the API server refuses.
func TestFleetTask_DesiredStateReachesRunningChildWhileSiblingIsRefused(t *testing.T) {
	for _, want := range []fleetv1.DesiredState{
		fleetv1.DesiredStateCancelled, fleetv1.DesiredStatePaused, fleetv1.DesiredStateReturning,
	} {
		t.Run(string(want), func(t *testing.T) {
			task := &fleetv1.FleetTask{
				ObjectMeta: metav1.ObjectMeta{Name: "restock", Namespace: refusedNS, Generation: 2, UID: fakeTaskUID("restock")},
				Spec: fleetv1.FleetTaskSpec{
					DesiredState: want,
					Actions: []fleetv1.FleetTaskAction{
						{Name: "deliver", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "warehouse-a"}},
						{Name: "clean", Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeClean, Zone: "warehouse-a"}},
					},
				},
				Status: fleetv1.FleetTaskStatus{Phase: fleetv1.FleetTaskPhaseRunning},
			}
			c, scheme := refusingCleanClient(t, task, runningChild("restock", "deliver"))
			reconcileRefusedSibling(t, c, scheme, "restock")
			if got := childDesiredState(t, c, childName("restock", "deliver")); got != want {
				t.Errorf("running child desiredState = %q after the task was set to %q while its sibling "+
					"was refused; the fan-out must reach every child that exists", got, want)
			}
		})
	}
}

// Flow path: a First join settles, so its running sibling is cancelled, in the same
// pass as a parallel branch's create is refused.
func TestFleetTask_FlowCancelReachesRunningSiblingWhileABranchIsRefused(t *testing.T) {
	a, b, clean := "fast", "slow", "clean"
	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "race", Namespace: refusedNS, Generation: 2, UID: fakeTaskUID("race")},
		Spec: fleetv1.FleetTaskSpec{
			DesiredState: fleetv1.DesiredStateRunning,
			Actions: []fleetv1.FleetTaskAction{
				{Name: a, Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "warehouse-a"}},
				{Name: b, Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "warehouse-a"}},
				{Name: clean, Action: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeClean, Zone: "warehouse-a"}},
			},
			Flow: &fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{
				Branches: []fleetv1.FlowNode{
					{Parallel: &fleetv1.ParallelNode{
						Branches: []fleetv1.FlowNode{{Run: &a}, {Run: &b}},
						Join:     &fleetv1.JoinSpec{Mode: fleetv1.JoinModeFirst},
						OnJoin:   fleetv1.OnJoinCancelSiblings,
					}},
					{Run: &clean},
				},
				Join: &fleetv1.JoinSpec{Mode: fleetv1.JoinModeAll},
			}},
		},
		Status: fleetv1.FleetTaskStatus{Phase: fleetv1.FleetTaskPhaseRunning},
	}
	won := runningChild("race", a)
	won.Status.Phase = fleetv1.ActionPhaseSucceeded
	won.Status.AssignedRobot = ""
	c, scheme := refusingCleanClient(t, task, won, runningChild("race", b))
	reconcileRefusedSibling(t, c, scheme, "race")
	if got := childDesiredState(t, c, childName("race", b)); got != fleetv1.DesiredStateCancelled {
		t.Errorf("the join's losing branch has desiredState %q while a parallel branch was refused, "+
			"want Cancelled: a refusal must not keep a cancelled sibling running", got)
	}
}
