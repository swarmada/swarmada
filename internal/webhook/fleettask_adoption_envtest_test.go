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

package webhook

import (
	"context"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controller"
)

// A FleetTask acts only on the FleetActions it created (RFC-0001 #fleettask: the controller
// is the only writer of the actions it owns). A child is an object the task controls through
// its ownerReference; an existing object that merely has the name a member's child would get
// is a conflict, reported on the task and left unchanged. Only the control plane sets or
// changes a FleetAction's owner references, so ownership cannot be written onto another
// principal's action from outside.
//
// Each row runs against the real API server with the FleetAction and FleetTask webhooks
// served. The task's author reaches the API server under RBAC alone (no system:masters),
// through a role that grants what the row names and never `cancel`, so the cancel check the
// FleetAction webhook runs for that author is a real SubjectAccessReview deny. The FleetTask
// reconciler runs as the manager's principal, as the manager does.
func TestEnvtest_FleetTaskActsOnlyOnActionsItCreated(t *testing.T) {
	w := startSafetyWire(t)
	ctx := context.Background()

	rbacScheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(rbacScheme); err != nil {
		t.Fatal(err)
	}
	rc, err := client.New(w.cfg, client.Options{Scheme: rbacScheme})
	if err != nil {
		t.Fatal(err)
	}
	// author returns a client for user, bound in the test namespace to a role with rules.
	author := func(user string, rules ...rbacv1.PolicyRule) client.Client {
		t.Helper()
		cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "adoption-" + user}, Rules: rules}
		if err := rc.Create(ctx, cr); err != nil {
			t.Fatalf("installing %s: %v", cr.Name, err)
		}
		if err := rc.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "adoption-" + user, Namespace: w.ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cr.Name},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
		}); err != nil {
			t.Fatalf("binding %s: %v", cr.Name, err)
		}
		ucfg := rest.CopyConfig(w.cfg)
		ucfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:authenticated"}}
		uc, err := client.New(ucfg, client.Options{Scheme: w.scheme})
		if err != nil {
			t.Fatal(err)
		}
		return uc
	}
	taskRule := func(verbs ...string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: []string{fleetv1.GroupVersion.Group}, Resources: []string{"fleettasks"}, Verbs: verbs}
	}
	actionRule := func(verbs ...string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: []string{fleetv1.GroupVersion.Group}, Resources: []string{"fleetactions"}, Verbs: verbs}
	}

	manager := w.as(t, controllerUser)
	reconcileTask := func(name string, passes int) {
		t.Helper()
		tr := &controller.FleetTaskReconciler{Client: manager, Scheme: w.scheme, Recorder: record.NewFakeRecorder(64)}
		for i := 0; i < passes; i++ {
			if _, err := tr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: w.ns, Name: name}}); err != nil {
				t.Logf("task %s reconcile: %v", name, err)
			}
		}
	}
	action := func(name string) *fleetv1.FleetAction {
		t.Helper()
		var fa fleetv1.FleetAction
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &fa); err != nil {
			t.Fatalf("reading action %s: %v", name, err)
		}
		return &fa
	}
	task := func(name string) *fleetv1.FleetTask {
		t.Helper()
		var ft fleetv1.FleetTask
		if err := w.admin.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: name}, &ft); err != nil {
			t.Fatalf("reading task %s: %v", name, err)
		}
		return &ft
	}
	navigate := func() fleetv1.FleetActionSpec {
		return fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, OnFailure: fleetv1.ActionFailureAlert}
	}
	// standalone is an action created by another principal, outside any task.
	standalone := func(name string) *fleetv1.FleetAction {
		t.Helper()
		fa := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name}, Spec: navigate()}
		fa.Spec.DesiredState = fleetv1.DesiredStateRunning
		if err := w.admin.Create(ctx, fa); err != nil {
			t.Fatalf("creating action %s: %v", name, err)
		}
		return fa
	}
	setPhase := func(name string, phase fleetv1.ActionPhase) {
		t.Helper()
		fa := action(name)
		base := fa.DeepCopy()
		fa.Status.Phase = phase
		if err := w.admin.Status().Patch(ctx, fa, client.MergeFrom(base)); err != nil {
			t.Fatalf("setting %s %s: %v", name, phase, err)
		}
	}
	unchanged := func(t *testing.T, name string) {
		t.Helper()
		got := action(name)
		if got.Spec.DesiredState != fleetv1.DesiredStateRunning {
			t.Errorf("action %s was changed by a task that does not own it: desiredState %q", name, got.Spec.DesiredState)
		}
		if len(got.OwnerReferences) != 0 {
			t.Errorf("action %s gained an owner: %+v", name, got.OwnerReferences)
		}
	}
	conflictReported := func(t *testing.T, taskName, actionName string) {
		t.Helper()
		c := apimeta.FindStatusCondition(task(taskName).Status.Conditions, controller.ConditionChildrenAdmitted)
		if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, actionName) {
			t.Errorf("task %s does not report the name conflict with %s: %+v", taskName, actionName, c)
		}
	}

	// The author of rows 1 and 2 may create tasks and read them, and nothing else.
	creator := author("task-creator", taskRule("create", "get"))
	// The author cannot cancel the action itself: RBAC refuses the write.
	standalone("fleetaction-pick-order-8821")
	if err := creator.Patch(ctx, &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "fleetaction-pick-order-8821"}},
		client.RawPatch(types.MergePatchType, []byte(`{"spec":{"desiredState":"Cancelled"}}`))); err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("precondition: the task author must not be able to cancel the action directly: %v", err)
	}

	t.Run("same-name-task-cancelled-leaves-the-action-alone", func(t *testing.T) {
		if err := creator.Create(ctx, &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "fleetaction"},
			Spec: fleetv1.FleetTaskSpec{DesiredState: fleetv1.DesiredStateCancelled,
				Actions: []fleetv1.FleetTaskAction{{Name: "pick-order-8821", Action: navigate()}}},
		}); err != nil {
			t.Fatalf("creating the task: %v", err)
		}
		reconcileTask("fleetaction", 3)
		unchanged(t, "fleetaction-pick-order-8821")
		conflictReported(t, "fleetaction", "fleetaction-pick-order-8821")
	})

	t.Run("same-name-task-paused-leaves-the-action-alone", func(t *testing.T) {
		standalone("dock-sweep-north")
		if err := creator.Create(ctx, &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "dock"},
			Spec: fleetv1.FleetTaskSpec{DesiredState: fleetv1.DesiredStatePaused,
				Actions: []fleetv1.FleetTaskAction{{Name: "sweep-north", Action: navigate()}}},
		}); err != nil {
			t.Fatalf("creating the task: %v", err)
		}
		reconcileTask("dock", 3)
		unchanged(t, "dock-sweep-north")
		conflictReported(t, "dock", "dock-sweep-north")
	})

	t.Run("owner-reference-written-by-a-client-is-refused", func(t *testing.T) {
		standalone("bay-restock")
		patcher := author("action-patcher", taskRule("create", "get"), actionRule("get", "list", "patch", "update"))
		if err := patcher.Create(ctx, &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "bay"},
			Spec: fleetv1.FleetTaskSpec{DesiredState: fleetv1.DesiredStateCancelled,
				Actions: []fleetv1.FleetTaskAction{{Name: "restock", Action: navigate()}}},
		}); err != nil {
			t.Fatalf("creating the task: %v", err)
		}
		var own fleetv1.FleetTask
		if err := patcher.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: "bay"}, &own); err != nil {
			t.Fatal(err)
		}
		// The patcher holds patch on fleetactions but not cancel.
		if err := patcher.Patch(ctx, &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "bay-restock"}},
			client.RawPatch(types.MergePatchType, []byte(`{"spec":{"desiredState":"Cancelled"}}`))); err == nil || !strings.Contains(err.Error(), "admission webhook") {
			t.Fatalf("precondition: a direct cancel by a principal without cancel must be refused by the webhook: %v", err)
		}
		fa := action("bay-restock")
		base := fa.DeepCopy()
		yes := true
		fa.OwnerReferences = []metav1.OwnerReference{{APIVersion: fleetv1.GroupVersion.String(), Kind: "FleetTask",
			Name: own.Name, UID: own.UID, Controller: &yes, BlockOwnerDeletion: &yes}}
		perr := patcher.Patch(ctx, fa, client.MergeFrom(base))
		if perr == nil {
			t.Errorf("a client set a FleetTask controller reference on an action it does not own")
		} else if !apierrors.IsForbidden(perr) || !strings.Contains(perr.Error(), "admission webhook") {
			t.Errorf("the owner-reference write was refused, but not by admission: %v", perr)
		}
		reconcileTask("bay", 3)
		got := action("bay-restock")
		if got.Spec.DesiredState != fleetv1.DesiredStateRunning {
			t.Errorf("a task changed an action it did not create: desiredState %q", got.Spec.DesiredState)
		}
	})

	t.Run("own-children-are-still-adopted-after-a-restart", func(t *testing.T) {
		if err := w.admin.Create(ctx, &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "own"},
			Spec:       fleetv1.FleetTaskSpec{Actions: []fleetv1.FleetTaskAction{{Name: "a", Action: navigate()}}},
		}); err != nil {
			t.Fatal(err)
		}
		reconcileTask("own", 2)
		child := action("own-a")
		if !metav1.IsControlledBy(child, task("own")) {
			t.Fatalf("the task's own child is not controlled by it: %+v", child.OwnerReferences)
		}
		// A fresh reconciler (a manager restart) finds the child by name and adopts it: a
		// pause on the task reaches it.
		cur := task("own")
		base := cur.DeepCopy()
		cur.Spec.DesiredState = fleetv1.DesiredStatePaused
		if err := w.admin.Patch(ctx, cur, client.MergeFrom(base)); err != nil {
			t.Fatal(err)
		}
		reconcileTask("own", 2)
		if got := action("own-a").Spec.DesiredState; got != fleetv1.DesiredStatePaused {
			t.Errorf("the task's own child was not adopted after a restart: desiredState %q", got)
		}
		if c := apimeta.FindStatusCondition(task("own").Status.Conditions, controller.ConditionChildrenAdmitted); c != nil && c.Status == metav1.ConditionFalse {
			t.Errorf("the task reports a conflict with its own child: %+v", c)
		}
	})

	t.Run("flow-task-leaves-a-same-name-action-alone", func(t *testing.T) {
		standalone("yard-scan")
		if err := creator.Create(ctx, &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "yard"},
			Spec: fleetv1.FleetTaskSpec{DesiredState: fleetv1.DesiredStateCancelled,
				Actions: []fleetv1.FleetTaskAction{{Name: "scan", Action: navigate()}},
				Flow:    &fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{{Run: strp("scan")}}}},
		}); err != nil {
			t.Fatalf("creating the task: %v", err)
		}
		reconcileTask("yard", 3)
		unchanged(t, "yard-scan")
		conflictReported(t, "yard", "yard-scan")
	})

	t.Run("compensation-is-not-read-from-a-same-name-action", func(t *testing.T) {
		// Created by another principal, already Succeeded.
		standalone("saga-load-comp")
		setPhase("saga-load-comp", fleetv1.ActionPhaseSucceeded)
		if err := creator.Create(ctx, &fleetv1.FleetTask{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: "saga"},
			Spec: fleetv1.FleetTaskSpec{FailurePolicy: fleetv1.FailurePolicyCompensate, Actions: []fleetv1.FleetTaskAction{
				{Name: "load", Action: navigate(), Compensation: func() *fleetv1.FleetActionSpec { s := navigate(); return &s }()},
				{Name: "lift", Action: navigate()},
			}},
		}); err != nil {
			t.Fatalf("creating the task: %v", err)
		}
		reconcileTask("saga", 2)
		setPhase("saga-load", fleetv1.ActionPhaseSucceeded)
		setPhase("saga-lift", fleetv1.ActionPhaseFailed)
		reconcileTask("saga", 3)
		got := task("saga")
		if got.Status.Phase == fleetv1.FleetTaskPhaseCompensated {
			t.Errorf("the task reported its compensation complete from an action it did not create")
		}
		conflictReported(t, "saga", "saga-load-comp")
		if fa := action("saga-load-comp"); len(fa.OwnerReferences) != 0 {
			t.Errorf("the same-name action gained an owner: %+v", fa.OwnerReferences)
		}
	})
}
