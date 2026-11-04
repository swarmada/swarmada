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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/scheduler"
)

// An operator cancels a task one of whose children the API server refuses. The
// member that is already running must still stop, through the confirmed-cancel path,
// and end Cancelled (RFC-0001 #fleettask-declarative-desired-state).
func TestEnvtest_CancelReachesRunningChildWhileSiblingIsRefused(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)
	refuseCleanInNamespace(t, ns)

	task := &fleetv1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "restock", Namespace: ns},
		Spec: fleetv1.FleetTaskSpec{
			CompletionPolicy: fleetv1.CompletionPolicyAll,
			Actions: []fleetv1.FleetTaskAction{
				{Name: "deliver", Action: fleetv1.FleetActionSpec{
					Type: fleetv1.ActionTypeNavigate, Zone: "warehouse-a",
					RobotSelector: "amr-01", Priority: fleetv1.ActionPriorityNormal,
				}},
				// Starts once deliver is running, so deliver exists and runs before
				// this create is refused.
				{Name: "clean", DependsOn: []string{"deliver"}, StartCondition: fleetv1.ActionPhaseInProgress,
					Action: fleetv1.FleetActionSpec{
						Type: fleetv1.ActionTypeClean, Zone: "warehouse-a",
						RobotSelector: "vacuum-01", Priority: fleetv1.ActionPriorityNormal,
					}},
			},
		},
	}
	if err := envK8s.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	ftr := &FleetTaskReconciler{Client: envK8s, Scheme: envScheme, Recorder: record.NewFakeRecorder(32)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "restock", Namespace: ns}}
	deliverKey := types.NamespacedName{Name: childName("restock", "deliver"), Namespace: ns}

	if _, err := ftr.Reconcile(ctx, req); err != nil {
		t.Fatalf("first pass (creates deliver): %v", err)
	}
	// deliver is running on a robot, under a live lease: only a confirmed stop can end it.
	var deliver fleetv1.FleetAction
	if err := envK8s.Get(ctx, deliverKey, &deliver); err != nil {
		t.Fatalf("deliver was not created: %v", err)
	}
	base := deliver.DeepCopy()
	deliver.Status.Phase = fleetv1.ActionPhaseInProgress
	deliver.Status.AssignedRobot = "amr-01"
	deliver.Status.AssignmentGeneration = 1
	deliver.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(time.Hour)}
	if err := envK8s.Status().Patch(ctx, &deliver, client.MergeFrom(base)); err != nil {
		t.Fatalf("mark deliver running: %v", err)
	}

	// clean is now eligible, and the API server refuses it.
	if _, err := ftr.Reconcile(ctx, req); err == nil {
		t.Fatal("the clean child was admitted; the policy should have refused it")
	}

	// The operator cancels the task — the write `swarmctl cancel task` makes.
	var ft fleetv1.FleetTask
	if err := envK8s.Get(ctx, req.NamespacedName, &ft); err != nil {
		t.Fatalf("get task: %v", err)
	}
	ftBase := ft.DeepCopy()
	ft.Spec.DesiredState = fleetv1.DesiredStateCancelled
	if err := envK8s.Patch(ctx, &ft, client.MergeFrom(ftBase)); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if _, err := ftr.Reconcile(ctx, req); err == nil {
		t.Fatal("the refusal was not reported on the cancel pass; the create must still be retried")
	}

	// The FleetAction controller enacts the cancel; the adapter confirms the stop.
	cmd := &fakeCommander{cancelAck: true, cancelDisp: command.CancelStoppedSafely}
	far := &FleetActionReconciler{Client: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: cmd}
	if _, err := far.Reconcile(ctx, ctrl.Request{NamespacedName: deliverKey}); err != nil {
		t.Fatalf("reconcile deliver: %v", err)
	}
	if err := envK8s.Get(ctx, deliverKey, &deliver); err != nil {
		t.Fatalf("get deliver: %v", err)
	}
	if deliver.Spec.DesiredState != fleetv1.DesiredStateCancelled {
		t.Fatalf("running child desiredState = %q after the task was cancelled while its sibling was refused; "+
			"the cancel never reached it", deliver.Spec.DesiredState)
	}
	if deliver.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Errorf("running child phase = %q, want Cancelled", deliver.Status.Phase)
	}
	if len(cmd.cancelTo) != 1 || cmd.cancelTo[0] != "amr-01" {
		t.Errorf("cancel_action pushes = %v, want one to amr-01: the stop must be confirmed by the robot", cmd.cancelTo)
	}
}
