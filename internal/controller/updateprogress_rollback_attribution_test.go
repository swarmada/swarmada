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

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ADR-0055, carried through to the step that acts on it. The attribution tests in
// updateprogress_model_attribution_test.go stop at the Robot's installedModels entry; what
// makes a misattributed FAILED dangerous is the rollback pass that reads that entry under
// rollbackPolicy: Auto and pushes a revert (RFC-0001 §6.7). So this one runs that pass on
// BOTH in-flight rollouts after the report and counts what is pushed.
func TestUpdateProgress_FailureOnOneModelPushesNoRevertOfTheOther(t *testing.T) {
	ing, c := concurrentRolloutIngestor(t)

	// Both rollouts revert automatically, and both know a version to revert r1 to — so the
	// only thing that keeps a revert off "a-nav-stack" is the report landing on the right model.
	for name, prev := range map[string]string{"roll-nav": "1.3.0", "roll-vision": "1.9.0"} {
		roll := &fleetv1.ModelRollout{}
		key := types.NamespacedName{Name: name, Namespace: rolloutNS}
		if err := c.Get(context.Background(), key, roll); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		roll.Spec.RollbackPolicy = fleetv1.ModelRollbackAuto
		if err := c.Update(context.Background(), roll); err != nil {
			t.Fatalf("update %s spec: %v", name, err)
		}
		roll.Status.RollbackVersions = map[string]string{"r1": prev}
		if err := c.Status().Update(context.Background(), roll); err != nil {
			t.Fatalf("update %s status: %v", name, err)
		}
	}

	// The report is for the SECOND rollout by list order, as in the attribution tests.
	if err := ing.IngestUpdateProgress(context.Background(), rolloutNS, &fav1.UpdateProgress{
		RobotId:       "r1",
		Kind:          fav1.UpdateKind_UPDATE_KIND_MODEL,
		ModelName:     "z-vision-stack",
		Outcome:       fav1.InstallOutcome_INSTALL_OUTCOME_FAILED,
		FailureReason: "inference health check failed",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	pusher := &fakePusher{ack: true}
	r := &ModelRolloutReconciler{Client: c, Scheme: c.Scheme(), Pusher: pusher}
	for _, name := range []string{"roll-nav", "roll-vision"} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: rolloutNS},
		}); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}

	for i, p := range pusher.pushes {
		if p.ModelName == "a-nav-stack" {
			t.Errorf("push %d to %s = %+v: a revert of a model that never failed — "+
				"an install onto a healthy robot for a failure that happened to another model",
				i, pusher.targets[i], p)
		}
	}
	// The failure that did happen is still acted on: exactly one revert, of the model that failed.
	if len(pusher.pushes) != 1 || !pusher.pushes[0].Rollback ||
		pusher.pushes[0].ModelName != "z-vision-stack" || pusher.pushes[0].NewVersion != "1.9.0" {
		t.Fatalf("pushes = %+v, want exactly one rollback of z-vision-stack to 1.9.0", pusher.pushes)
	}
	if e := modelEntryOf(t, c, "r1", "a-nav-stack"); e == nil ||
		e.Status != fleetv1.ModelStatusUpdating || e.FailureReason != "" {
		t.Errorf("a-nav-stack entry = %+v after both passes, want untouched at Updating", e)
	}
}
