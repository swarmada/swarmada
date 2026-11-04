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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// A rollout with pauseOnError off reaches a terminal phase and can be deleted, against a
// real API server.
//
// rollout_terminal_failure_test.go proves the rule on a fake client, which runs none of the
// CRD's schema: not the status.phase enum, not a CEL transition rule, not a required field.
// A Failed write the real schema refused would pass there and strand the rollout here — the
// exact non-termination this guards against. So the shipped reconciler runs against envtest
// until the rollout settles, and the settled record is then actually deleted (no finalizer
// may hold it). The delete WEBHOOK's half of "can be deleted" is proved over the wire in
// internal/webhook/safety_admission_envtest_test.go.
func TestEnvtest_ModelRolloutUnpausedFailureReachesFailedAndIsDeleted(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := envtestNamespace(t)

	// The unit test's failed robot, on a spec the real schema admits.
	failed := failedModelRobot("r1", "inference health check failed")
	robot := envtestValidRobot(ns, "r1", "z-a")
	robot.Labels = failed.Labels
	robot.Spec.Hardware = failed.Spec.Hardware
	wantStatus := failed.Status
	if err := envK8s.Create(ctx, robot); err != nil {
		t.Fatalf("creating the robot: %v", err)
	}
	robot.Status = wantStatus
	if err := envK8s.Status().Update(ctx, robot); err != nil {
		t.Fatalf("writing the robot's failed install status: %v", err)
	}

	roll := pickerRollout()
	roll.Namespace = ns
	roll.Spec.ModelChecksum = "sha256:" + strings.Repeat("0", 64)
	roll.Spec.Strategy.RollingUpdate.PauseOnError = apiBool(false)
	if err := envK8s.Create(ctx, roll); err != nil {
		t.Fatalf("creating the rollout: %v", err)
	}

	r := &ModelRolloutReconciler{Client: envK8s, Scheme: envK8s.Scheme()}
	key := client.ObjectKeyFromObject(roll)
	got := &fleetv1.ModelRollout{}
	// A few passes, not one: on a real server the first pass may only initialise status.
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile pass %d: %v", i+1, err)
		}
		if err := envK8s.Get(ctx, key, got); err != nil {
			t.Fatalf("re-reading the rollout: %v", err)
		}
		if got.Status.Phase == fleetv1.RolloutPhaseFailed {
			break
		}
	}
	if got.Status.Phase != fleetv1.RolloutPhaseFailed {
		t.Fatalf("phase = %q after 5 reconciles, want Failed: pauseOnError is off and the only "+
			"robot failed, so nothing is left to do (%+v)", got.Status.Phase, got.Status)
	}

	if err := envK8s.Delete(ctx, got); err != nil {
		t.Fatalf("deleting the Failed rollout: %v", err)
	}
	gone := &fleetv1.ModelRollout{}
	if err := envK8s.Get(ctx, key, gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the Failed rollout is still there after delete (finalizers %v): %v", gone.Finalizers, err)
	}
}
