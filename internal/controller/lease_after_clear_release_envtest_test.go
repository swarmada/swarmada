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
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/safety"
)

// The work a clear interrupted stays held when the robot is released (S6) before any FleetAction
// reconcile has read it Resuming (RFC-0001 #safety-action-table, The lease): the operator release
// releases the robot, not its work. The clear records the hold on the work itself, so the reconcile
// that comes after the release still holds it: no renewal for InProgress work, and no advance of a
// Paused lease under a later stop the robot confirms.
func TestEnvtest_LeaseAfterClear_WorkReleasedBeforeAnyReconcileStaysHeld(t *testing.T) {
	requireEnvtest(t)
	for _, phase := range []fleetv1.ActionPhase{fleetv1.ActionPhaseInProgress, fleetv1.ActionPhasePaused} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			ns := envtestNamespace(t)
			frames := resumeWitness()
			dispatcher := safety.New(envK8s, nil)
			dispatcher.Telemetry = frames
			dispatcher.DeriveCapabilities = DeriveCapabilities
			estops := &RobotEstopReconciler{Client: envK8s, Scheme: envScheme, Resumer: dispatcher}
			if err := envK8s.Create(ctx, gateEnvAdapter(ns, "acme-adapter")); err != nil {
				t.Fatal(err)
			}
			gateEnvSeedAdapterStatus(t, ns, "acme-adapter", contract.Version)
			if err := envK8s.Create(ctx, envtestValidRobot(ns, "amr-1", "z-a")); err != nil {
				t.Fatal(err)
			}
			seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, time.Now())
			var robot fleetv1.Robot
			if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "amr-1"}, &robot); err != nil {
				t.Fatal(err)
			}
			robot.Status.AssignedAction = "task-1"
			if err := envK8s.Status().Update(ctx, &robot); err != nil {
				t.Fatal(err)
			}
			action := &fleetv1.FleetAction{
				ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: ns},
				Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: "z-a", Priority: fleetv1.ActionPriorityNormal},
			}
			if err := envK8s.Create(ctx, action); err != nil {
				t.Fatal(err)
			}
			// Half a duration ahead, so a stamp (now plus one duration) is told apart from it.
			lease := metav1.NewTime(time.Now().Add(defaultLeaseDuration / 2).Truncate(time.Second))
			action.Status = fleetv1.FleetActionStatus{Phase: phase, AssignedRobot: "amr-1",
				AssignmentGeneration: 4, LeaseExpiresAt: &lease}
			if phase == fleetv1.ActionPhasePaused {
				action.Status.Message = estopPausedInProgressMessage
			}
			if err := envK8s.Status().Update(ctx, action); err != nil {
				t.Fatal(err)
			}

			// The clear, then the release, with no FleetAction reconcile in between.
			if st, err := dispatcher.ClearEstop(ctx, ns, "amr-1", "alice", safety.Ends{}); err != nil || st != fleetv1.RobotEstopResuming {
				t.Fatalf("ClearEstop = %s, %v", st, err)
			}
			key := types.NamespacedName{Namespace: ns, Name: "amr-1"}
			sendResumeFrame(t, frames, ns, "amr-1")
			writeResumeRequest(t, key, "inspected")
			if _, err := estops.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("reconcile estop: %v", err)
			}
			if got := readEstopState(t, ns, "amr-1"); got != fleetv1.RobotEstopNormal {
				t.Fatalf("estopState = %s; want Normal after the release", got)
			}
			if phase == fleetv1.ActionPhasePaused {
				// A fresh stop, confirmed.
				seedResumeStatus(t, ns, "amr-1", fleetv1.RobotEstopStopped, time.Now())
			}

			cmd := &fakeCommander{}
			actions := &FleetActionReconciler{Client: envK8s, Scheme: envScheme, Commander: cmd}
			reconcileResumeAction(t, actions, ns)
			reconcileResumeAction(t, actions, ns)
			var got fleetv1.FleetAction
			if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "task-1"}, &got); err != nil {
				t.Fatal(err)
			}
			if len(cmd.renews) != 0 || got.Status.LeaseExpiresAt == nil || !got.Status.LeaseExpiresAt.Equal(&lease) {
				t.Fatalf("%s: %d renewals, lease %v (was %v); want none and the lease frozen",
					phase, len(cmd.renews), got.Status.LeaseExpiresAt, lease)
			}
		})
	}
}
