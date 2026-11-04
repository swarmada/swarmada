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

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A robot's terminal install report that lands AFTER the adapter acknowledged the push
// but BEFORE the controller writes its Updating mark.
//
// The report-before-the-ack order is covered by the re-read in enterBatch and rollbackRobot
// (modelrollout_correlation_race_test.go, modelrollout_rollback_race_test.go). This is the
// other order, the natural one for an adapter that acknowledges and then installs quickly:
// the re-read sees no report, the report is written, and then the mark computed from the
// re-read copy is patched. A plain merge patch replaces installedModels with that copy, so
// the report is erased, the robot reads Updating with no runningVersion, and nothing
// reports again. Seen live with the simulation adapter in real-artifact mode, which
// acknowledges, loads and self-checks a small model, and reports within the same second.
//
// The report is injected at the one instant that matters: when the Updating mark is about
// to be written. Deterministic, not timing-dependent.

// ackingPusher acknowledges every push and reports nothing itself.
type ackingPusher struct{ pushes int }

func (p *ackingPusher) PushModelUpdate(context.Context, string, string, command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	p.pushes++
	return command.ModelUpdateOutcome{Acknowledged: true}, nil
}

// newReportRaceReconciler builds a rollout reconciler whose client delivers report — once,
// through the uninstrumented client — just before the first Robot status patch that marks
// modelName Updating is applied.
func newReportRaceReconciler(t *testing.T, modelName string, report *fav1.UpdateProgress, objs ...client.Object) (*ModelRolloutReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.ModelRollout{}).Build()
	ing := &UpdateProgressIngestor{Client: base}
	fired := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if rob, ok := obj.(*fleetv1.Robot); ok && !fired {
				if e := modelEntry(rob, modelName); e != nil && e.Status == fleetv1.ModelStatusUpdating {
					fired = true
					if err := ing.IngestUpdateProgress(ctx, rob.Namespace, report); err != nil {
						t.Fatalf("ingest the robot's report: %v", err)
					}
				}
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	t.Cleanup(func() {
		if !fired {
			t.Error("the report was never injected: no Updating mark was written")
		}
	})
	return &ModelRolloutReconciler{Client: c, Scheme: scheme}, base
}

func TestModelRollout_ReportBetweenReReadAndMarkIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	r, c := newReportRaceReconciler(t, "item-recognition", &fav1.UpdateProgress{
		RobotId:          "r1",
		Kind:             fav1.UpdateKind_UPDATE_KIND_MODEL,
		ModelName:        "item-recognition",
		Outcome:          fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED,
		ResultingVersion: "3.2.1",
	}, pickerRollout(), raceRobot("r1"))
	r.Pusher = &ackingPusher{}

	reconcileRollout(t, r)

	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil {
		t.Fatal("no installedModels entry for item-recognition")
	}
	if e.Status != fleetv1.ModelStatusActive || e.RunningVersion != "3.2.1" {
		t.Errorf("entry = %s@%q, want Active@3.2.1 — the Updating mark overwrote the report the "+
			"robot sent after acknowledging, and nothing reports again", e.Status, e.RunningVersion)
	}

	// And the rollout completes on its own.
	reconcileRollout(t, r)
	got := &fleetv1.ModelRollout{}
	if err := c.Get(ctx, modelRolloutKey(), got); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if got.Status.Phase != fleetv1.RolloutPhaseSucceeded {
		t.Errorf("rollout phase = %s, want Succeeded (status %+v)", got.Status.Phase, got.Status)
	}
}

func TestModelRollout_RevertReportBetweenReReadAndMarkIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	rob := raceRobot("r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusFailed, RunningVersion: "3.0.0",
		FailureReason: "sim: post-install health check failed",
	}}
	r, c := newReportRaceReconciler(t, "item-recognition", &fav1.UpdateProgress{
		RobotId:          "r1",
		Kind:             fav1.UpdateKind_UPDATE_KIND_MODEL,
		ModelName:        "item-recognition",
		Outcome:          fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED,
		ResultingVersion: "3.0.0",
	}, autoRollbackRollout(map[string]string{"r1": "3.0.0"}), rob)
	r.Pusher = &ackingPusher{}

	reconcileRollout(t, r)

	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil {
		t.Fatal("the robot's model entry disappeared")
	}
	if e.Status != fleetv1.ModelStatusActive || e.RunningVersion != "3.0.0" {
		t.Errorf("entry = %s@%q, want Active@3.0.0 — the reverting mark overwrote the revert the "+
			"robot reported after acknowledging, and a reverted robot never reports again", e.Status, e.RunningVersion)
	}

	reconcileRollout(t, r)
	got := &fleetv1.ModelRollout{}
	if err := c.Get(ctx, modelRolloutKey(), got); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if got.Status.CapabilitiesSuspendedOn != 0 {
		t.Errorf("capabilitiesSuspendedOn = %d, want 0 — the revert landed", got.Status.CapabilitiesSuspendedOn)
	}
	if got.Status.Phase == fleetv1.RolloutPhaseInProgress {
		t.Errorf("rollout still InProgress after the revert landed: %+v", got.Status)
	}
}
