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

// Shared FleetTask fixture builders, and the fake-client harness that remains for
// the tests still using it.
//
// The FleetTask state-machine and compensation-saga tests moved onto envtest (see
// fleettask_envtest_test.go). These helpers moved here so those files no longer
// import the fake client at all, and so the two harnesses sit side by side where
// the difference between them is visible.
//
// WHEN TO USE WHICH. The fake client performs no admission: it accepts objects the
// API server rejects, applies no CRD defaulting, and invents its own
// resourceVersion semantics. That makes it wrong for anything whose subject is the
// shape of the object — a CEL rule, a default, an early-start condition, an
// idempotency claim — and fine for a reconcile branch driven entirely by child
// phases the test writes itself, where an API round-trip buys nothing. The
// remaining fake-client tests (cancel, nil-quorum, flow) are the second kind. If a
// new test's fixture would have to be *valid* for the assertion to mean anything,
// it belongs on newFTEnv.

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

// ── fixture builders (shared by both harnesses) ──────────────────────────────

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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS, UID: fakeTaskUID(name)},
		Spec: fleetv1.FleetTaskSpec{
			CompletionPolicy: cp,
			FailurePolicy:    fp,
			DesiredState:     ds,
			Actions:          actions,
		},
	}
}

// childAction is a child the controller generated for taskName's member: it carries the task's
// controller ownerReference, as every child the controller creates does.
func childAction(taskName, actionName string, phase fleetv1.ActionPhase) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: childName(taskName, actionName), Namespace: actionNS,
			OwnerReferences: []metav1.OwnerReference{fakeTaskControllerRef(taskName)}},
		Spec:   fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status: fleetv1.FleetActionStatus{Phase: phase},
	}
}

// fakeTaskUID is the UID fleetTask gives a task, so a seeded child can name its owner.
func fakeTaskUID(taskName string) types.UID { return types.UID("fleettask-" + taskName) }

func fakeTaskControllerRef(taskName string) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: fleetv1.GroupVersion.String(), Kind: "FleetTask",
		Name: taskName, UID: fakeTaskUID(taskName), Controller: &yes, BlockOwnerDeletion: &yes}
}

// ── fake-client harness ──────────────────────────────────────────────────────

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
