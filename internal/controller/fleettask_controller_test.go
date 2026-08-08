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
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

func newFleetTaskReconciler(t *testing.T, objs ...client.Object) (*FleetTaskReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetTask{}, &fleetv1.FleetAction{}).
		Build()
	return &FleetTaskReconciler{Client: c, Scheme: scheme}, c
}

func reconcileFT(t *testing.T, r *FleetTaskReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: actionNS, Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func ftAction(name string, deps []string, sc fleetv1.ActionPhase) fleetv1.FleetTaskAction {
	return fleetv1.FleetTaskAction{
		Name:           name,
		Action:         fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		DependsOn:      deps,
		StartCondition: sc,
		Trigger:        fleetv1.TriggerModeAuto,
	}
}

func fleetTask(name string, cp fleetv1.CompletionPolicy, fp fleetv1.FailurePolicy, ds fleetv1.DesiredState, actions ...fleetv1.FleetTaskAction) *fleetv1.FleetTask {
	return &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec: fleetv1.FleetTaskSpec{
			CompletionPolicy: cp,
			FailurePolicy:    fp,
			DesiredState:     ds,
			Actions:          actions,
		},
	}
}

func childAction(taskName, actionName string, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: childName(taskName, actionName), Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status:     fleetv1.FleetActionStatus{Phase: phase},
	}
}

func getFT(t *testing.T, c client.Client, name string) *fleetv1.FleetTask {
	t.Helper()
	var ft fleetv1.FleetTask
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: name}, &ft); err != nil {
		t.Fatalf("get fleettask: %v", err)
	}
	return &ft
}

func getActionOrNil(t *testing.T, c client.Client, name string) *fleetv1.FleetAction {
	t.Helper()
	var a fleetv1.FleetAction
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: actionNS, Name: name}, &a); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("get action: %v", err)
	}
	return &a
}

// A root action is generated; a dependent action is not created until its predecessor is ready.
func TestFleetTask_GeneratesRootBlocksDependent(t *testing.T) {
	task := fleetTask("t1", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, task)
	reconcileFT(t, r, "t1")

	if getActionOrNil(t, c, childName("t1", "a")) == nil {
		t.Errorf("expected child action 'a' to be generated")
	}
	if getActionOrNil(t, c, childName("t1", "b")) != nil {
		t.Errorf("child 'b' must not be generated before 'a' succeeds")
	}
	if got := getFT(t, c, "t1").Status.Phase; got != fleetv1.FleetTaskPhaseRunning {
		t.Errorf("phase = %q, want Running", got)
	}
}

// completionPolicy All: every child Succeeded → task Succeeded.
func TestFleetTask_AllSucceeded(t *testing.T) {
	task := fleetTask("t2", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, task,
		childAction("t2", "a", fleetv1.ActionPhaseSucceeded),
		childAction("t2", "b", fleetv1.ActionPhaseSucceeded),
	)
	reconcileFT(t, r, "t2")

	if got := getFT(t, c, "t2").Status.Phase; got != fleetv1.FleetTaskPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded", got)
	}
}

// completionPolicy Any: one Succeeded suffices even with another still running.
func TestFleetTask_AnySucceeds(t *testing.T) {
	task := fleetTask("t3", fleetv1.CompletionPolicyAny, fleetv1.FailurePolicyContinueOthers, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, task,
		childAction("t3", "a", fleetv1.ActionPhaseSucceeded),
		childAction("t3", "b", fleetv1.ActionPhaseInProgress),
	)
	reconcileFT(t, r, "t3")

	if got := getFT(t, c, "t3").Status.Phase; got != fleetv1.FleetTaskPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded (Any)", got)
	}
}

// completionPolicy Quorum: reaching the quorum of successes → Succeeded.
func TestFleetTask_QuorumMet(t *testing.T) {
	q := int32(2)
	task := fleetTask("t4", fleetv1.CompletionPolicyQuorum, fleetv1.FailurePolicyContinueOthers, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("c", nil, fleetv1.ActionPhaseSucceeded),
	)
	task.Spec.Quorum = &q
	r, c := newFleetTaskReconciler(t, task,
		childAction("t4", "a", fleetv1.ActionPhaseSucceeded),
		childAction("t4", "b", fleetv1.ActionPhaseSucceeded),
		childAction("t4", "c", fleetv1.ActionPhaseInProgress),
	)
	reconcileFT(t, r, "t4")

	if got := getFT(t, c, "t4").Status.Phase; got != fleetv1.FleetTaskPhaseSucceeded {
		t.Errorf("phase = %q, want Succeeded (Quorum 2/3)", got)
	}
}

// FailFast: a terminally-failed action fails the task and cancels non-terminal siblings.
func TestFleetTask_FailFastCancelsSiblings(t *testing.T) {
	task := fleetTask("t5", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
		ftAction("b", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, task,
		childAction("t5", "a", fleetv1.ActionPhaseFailed),
		childAction("t5", "b", fleetv1.ActionPhaseInProgress),
	)
	reconcileFT(t, r, "t5")

	if got := getFT(t, c, "t5").Status.Phase; got != fleetv1.FleetTaskPhaseFailed {
		t.Errorf("phase = %q, want Failed", got)
	}
	if b := getActionOrNil(t, c, childName("t5", "b")); b == nil || b.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Errorf("sibling 'b' should be desiredState Cancelled, got %+v", b)
	}
}

// desiredState on the task fans out to non-terminal children.
func TestFleetTask_DesiredStateFanout(t *testing.T) {
	task := fleetTask("t6", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStatePaused,
		ftAction("a", nil, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, task, childAction("t6", "a", fleetv1.ActionPhaseInProgress))
	reconcileFT(t, r, "t6")

	if a := getActionOrNil(t, c, childName("t6", "a")); a == nil || a.Spec.DesiredState != fleetv1.DesiredStatePaused {
		t.Errorf("child 'a' desiredState = %v, want Paused", a)
	}
}

// A dependency cycle is rejected: task → Failed with DependencyGraphValid=False.
func TestFleetTask_CycleIsFailed(t *testing.T) {
	task := fleetTask("t7", fleetv1.CompletionPolicyAll, fleetv1.FailurePolicyFailFast, fleetv1.DesiredStateRunning,
		ftAction("a", []string{"b"}, fleetv1.ActionPhaseSucceeded),
		ftAction("b", []string{"a"}, fleetv1.ActionPhaseSucceeded),
	)
	r, c := newFleetTaskReconciler(t, task)
	reconcileFT(t, r, "t7")

	ft := getFT(t, c, "t7")
	if ft.Status.Phase != fleetv1.FleetTaskPhaseFailed {
		t.Errorf("phase = %q, want Failed", ft.Status.Phase)
	}
	found := false
	for _, cond := range ft.Status.Conditions {
		if cond.Type == "DependencyGraphValid" && cond.Status == metav1.ConditionFalse {
			found = true
		}
	}
	if !found {
		t.Errorf("expected DependencyGraphValid=False condition")
	}
}
