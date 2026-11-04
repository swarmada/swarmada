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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// An operator cancels a three-member dependsOn chain after its first member was created and
// while that member runs. The later members are never created: each waits on a predecessor
// that will never start it, so they are as finished as the cancelled one, and the task ends
// Cancelled, the operator's intent, not Failed (RFC-0001 #fleettask-declarative-desired-state).
// This is the shape of the shipped sample, against a real API server.
func TestEnvtest_CancelledThreeMemberChainEndsCancelled(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	member := func(robot string) fleetv1.FleetActionSpec {
		return fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "warehouse-a",
			RobotSelector: robot, Priority: fleetv1.ActionPriorityNormal}
	}
	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "inspect-dock", Namespace: ns},
		Spec: fleetv1.FleetTaskSpec{
			CompletionPolicy: fleetv1.CompletionPolicyAll,
			FailurePolicy:    fleetv1.FailurePolicyFailFast,
			Actions: []fleetv1.FleetTaskAction{
				{Name: "approach", Action: member("amr-01")},
				{Name: "inspect", DependsOn: []string{"approach"}, Action: member("amr-01")},
				{Name: "return", DependsOn: []string{"inspect"}, Action: member("amr-01")},
			},
		},
	}
	if err := envK8s.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	ftr := &FleetTaskReconciler{Client: envK8s, Scheme: envScheme, Recorder: record.NewFakeRecorder(64)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "inspect-dock", Namespace: ns}}
	key := func(member string) types.NamespacedName {
		return types.NamespacedName{Name: childName("inspect-dock", member), Namespace: ns}
	}

	if _, err := ftr.Reconcile(ctx, req); err != nil {
		t.Fatalf("first pass (creates approach): %v", err)
	}
	// approach runs on a robot, under a live lease: only a confirmed stop ends it.
	var approach fleetv1.FleetAction
	if err := envK8s.Get(ctx, key("approach"), &approach); err != nil {
		t.Fatalf("approach was not created: %v", err)
	}
	base := approach.DeepCopy()
	approach.Status.Phase = fleetv1.ActionPhaseInProgress
	approach.Status.AssignedRobot = "amr-01"
	approach.Status.AssignmentGeneration = 1
	approach.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(time.Hour)}
	if err := envK8s.Status().Patch(ctx, &approach, client.MergeFrom(base)); err != nil {
		t.Fatalf("mark approach running: %v", err)
	}
	if _, err := ftr.Reconcile(ctx, req); err != nil {
		t.Fatalf("pass with approach running: %v", err)
	}
	if err := envK8s.Get(ctx, key("inspect"), &fleetv1.FleetAction{}); !apierrors.IsNotFound(err) {
		t.Fatalf("inspect exists before approach finished (err=%v); the chain was not serial", err)
	}

	// The operator cancels the task: the write `swarmctl cancel task` makes.
	var ft fleetv1.FleetTask
	if err := envK8s.Get(ctx, req.NamespacedName, &ft); err != nil {
		t.Fatalf("get task: %v", err)
	}
	ftBase := ft.DeepCopy()
	ft.Spec.DesiredState = fleetv1.DesiredStateCancelled
	if err := envK8s.Patch(ctx, &ft, client.MergeFrom(ftBase)); err != nil {
		t.Fatalf("cancel task: %v", err)
	}

	// The task controller carries the cancel to approach; the FleetAction controller enacts
	// it and the adapter confirms the stop. Then the task settles.
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}
	far := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}
	for i := 0; i < 5; i++ {
		if _, err := ftr.Reconcile(ctx, req); err != nil {
			t.Fatalf("task pass %d: %v", i, err)
		}
		if _, err := far.Reconcile(ctx, ctrl.Request{NamespacedName: key("approach")}); err != nil {
			t.Fatalf("reconcile approach: %v", err)
		}
	}
	if _, err := ftr.Reconcile(ctx, req); err != nil {
		t.Fatalf("final task pass: %v", err)
	}

	if err := envK8s.Get(ctx, key("approach"), &approach); err != nil {
		t.Fatalf("get approach: %v", err)
	}
	if approach.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("approach phase = %q, want Cancelled: the cancel did not reach the running member", approach.Status.Phase)
	}
	for _, m := range []string{"inspect", "return"} {
		if err := envK8s.Get(ctx, key(m), &fleetv1.FleetAction{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s was created after the task was cancelled (err=%v)", m, err)
		}
	}
	if err := envK8s.Get(ctx, req.NamespacedName, &ft); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if ft.Status.Phase != fleetv1.FleetTaskPhaseCancelled {
		t.Fatalf("task phase = %q, want Cancelled: the members behind a never-created predecessor were "+
			"counted as still running or as failed, so the operator's cancellation was not reported as one",
			ft.Status.Phase)
	}
}
