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

// A robot's FAILED install report that lands after the adapter acknowledged the push but
// before status.currentBatch is written.
//
// A FAILED outcome is correlated through the batch, not the target selector, because the
// reference adapter also reports FAILED when it DECLINES an artifact, and a declined robot
// must not be failed. The batch is written by the rollout status patch at the end of the
// reconcile, well after the acknowledgement, so a failure reported in between used to find
// no batch and be dropped: the robot read Updating for ever, the rollout never paused and
// the model's capabilities stayed suspended. The report-overwritten twin is
// modelrollout_mark_race_test.go.
//
// The report is injected at two instants, both after the acknowledgement: when the Updating
// mark is written, and when the rollout status (the batch) is written.

// failedRaceWhen names the write at which the report is injected.
type failedRaceWhen int

const (
	atTheMark failedRaceWhen = iota
	atTheBatchWrite
)

// newFailedRaceReconciler builds a rollout reconciler whose client delivers report — once,
// through the uninstrumented client — just before the chosen write is applied. The
// reconciler and the ingestor share one acknowledgement ledger, as cmd/manager wires them.
func newFailedRaceReconciler(t *testing.T, modelName string, when failedRaceWhen, report *fav1.UpdateProgress, objs ...client.Object) (*ModelRolloutReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&fleetv1.Robot{}, &fleetv1.ModelRollout{}).Build()
	acks := NewModelAckLedger()
	ing := &UpdateProgressIngestor{Client: base, Acks: acks}
	fired := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if !fired {
				hit := false
				switch o := obj.(type) {
				case *fleetv1.Robot:
					e := modelEntry(o, modelName)
					hit = when == atTheMark && e != nil && e.Status == fleetv1.ModelStatusUpdating
				case *fleetv1.ModelRollout:
					hit = when == atTheBatchWrite
				}
				if hit {
					fired = true
					if err := ing.IngestUpdateProgress(ctx, rolloutNS, report); err != nil {
						t.Fatalf("ingest the robot's report: %v", err)
					}
				}
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	t.Cleanup(func() {
		if !fired {
			t.Error("the report was never injected")
		}
	})
	return &ModelRolloutReconciler{Client: c, Scheme: scheme, Acks: acks}, base
}

func failedReport(version string) *fav1.UpdateProgress {
	return &fav1.UpdateProgress{
		RobotId: "r1", Kind: fav1.UpdateKind_UPDATE_KIND_MODEL, ModelName: "item-recognition",
		Outcome: fav1.InstallOutcome_INSTALL_OUTCOME_FAILED, ResultingVersion: version,
		FailureReason: "self-check: output is NaN",
	}
}

func TestModelRollout_FailedReportAfterAckIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		when failedRaceWhen
	}{
		{"before-the-mark", atTheMark},
		{"before-the-batch-write", atTheBatchWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := newFailedRaceReconciler(t, "item-recognition", tc.when, failedReport("3.2.1"),
				pickerRollout(), raceRobot("r1"))
			r.Pusher = &ackingPusher{}

			reconcileRollout(t, r)
			reconcileRollout(t, r)

			e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
			if e == nil || e.Status != fleetv1.ModelStatusFailed {
				t.Fatalf("entry = %+v, want Failed — the robot reported the failure after "+
					"acknowledging, and the report was dropped", e)
			}
			roll := getRollout(t, c, "roll")
			if len(roll.Status.FailedRobots) != 1 || roll.Status.FailedRobots[0].RobotName != "r1" {
				t.Errorf("failedRobots = %+v, want [r1]", roll.Status.FailedRobots)
			}
			if roll.Status.Phase != fleetv1.RolloutPhasePaused {
				t.Errorf("phase = %s, want Paused under pauseOnError", roll.Status.Phase)
			}
		})
	}
}

// The rollback twin: a revert the robot reports FAILED after acknowledging it is counted
// failed, not left reverting for ever.
func TestModelRollout_FailedRevertReportAfterAckIsRecorded(t *testing.T) {
	rob := raceRobot("r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusFailed, RunningVersion: "3.2.1",
		FailureReason: "sim: post-install health check failed",
	}}
	r, c := newFailedRaceReconciler(t, "item-recognition", atTheMark, failedReport("3.0.0"),
		autoRollbackRollout(map[string]string{"r1": "3.0.0"}), rob)
	pusher := &ackingPusher{}
	r.Pusher = pusher

	reconcileRollout(t, r)
	reconcileRollout(t, r)

	roll := getRollout(t, c, "roll")
	if len(roll.Status.FailedRobots) != 1 || roll.Status.FailedRobots[0].RobotName != "r1" {
		t.Errorf("failedRobots = %+v, want [r1] — the revert failed and was reported", roll.Status.FailedRobots)
	}
	if roll.Status.CapabilitiesSuspendedOn != 0 {
		t.Errorf("capabilitiesSuspendedOn = %d, want 0 — r1 is no longer reverting", roll.Status.CapabilitiesSuspendedOn)
	}
	if pusher.pushes != 1 {
		t.Errorf("pushes = %d, want 1 — a failed revert is not pushed a second rollback", pusher.pushes)
	}
}

// refusingPusher is an adapter that refuses the artifact: it reports FAILED, having
// installed nothing, and then declines the push.
type refusingPusher struct {
	ingest func(ctx context.Context, namespace string, u *fav1.UpdateProgress) error
}

func (p *refusingPusher) PushModelUpdate(ctx context.Context, namespace, robotID string, u command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	if err := p.ingest(ctx, namespace, failedReport(u.NewVersion)); err != nil {
		return command.ModelUpdateOutcome{}, err
	}
	return command.ModelUpdateOutcome{Acknowledged: false, Message: "artifact refused"}, nil
}

// The guard the batch correlation exists for still holds: a FAILED that answers a declined
// push is not recorded, even when an earlier push of the same model to the same robot was
// acknowledged.
func TestModelRollout_DeclineAfterAnEarlierAckIsNotRecorded(t *testing.T) {
	rob := raceRobot("r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.0.0",
	}}
	r, c := newRolloutReconciler(t, pickerRollout(), rob)
	acks := NewModelAckLedger()
	acks.ack(rolloutNS, "r1", "item-recognition") // an earlier push was acknowledged
	r.Acks = acks
	r.Pusher = &refusingPusher{ingest: (&UpdateProgressIngestor{Client: c, Acks: acks}).IngestUpdateProgress}

	reconcileRollout(t, r)

	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil || e.Status != fleetv1.ModelStatusActive || e.RunningVersion != "3.0.0" {
		t.Errorf("entry = %+v, want untouched Active@3.0.0 — a declined push installed nothing", e)
	}
	if roll := getRollout(t, c, "roll"); len(roll.Status.FailedRobots) != 0 {
		t.Errorf("failedRobots = %+v, want none", roll.Status.FailedRobots)
	}
}
