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

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A robot's own report of a completed revert, arriving before the acknowledgement the
// revert was pushed on - the report-overtakes-the-ack ordering, on the ROLLBACK push.
//
// enterBatch was given a post-push re-read so a completed install is not overwritten by
// the Updating mark. rollbackRobot has the identical shape — push, block on the ack, then
// mark Updating — and did not get it. The reference adapter's rollback branch
// (sim_adapter.py, _on_model_update, `if upd.rollback`) enqueues its terminal SUCCEEDED
// and its ADR-0033 CapabilitiesSnapshot on the outbox BEFORE the CommandResult, so the
// revert report always overtakes the acknowledgement rollbackRobot blocks on.
//
// Why this one is worse than the forward case rather than a duplicate of it: the forward
// path's overwrite is repaired by the snapshot the adapter re-sends on its NEXT install
// transition. A reverted robot has no next transition — the revert was the last one — so
// nothing ever repairs it. Reproduced on a live three-robot cluster: every robot pinned at
// `<model>@<previous version> (Updating)` with the model-driven capability suspended and
// the rollout stuck InProgress, while all three were in fact running the previous version
// and had said so. That is why the assertion below is Active rather than merely "not
// Failed": the capability returns only on Active, so an overwritten revert costs the fleet
// that capability for good.
//
// Deterministic, not timing-dependent: the report is delivered inside the push, which is
// the order this adapter always produces.
type revertingPusher struct {
	ingest    func(ctx context.Context, namespace string, u *fav1.UpdateProgress) error
	capIngest func(ctx context.Context, namespace string, snap *fav1.CapabilitiesSnapshot) error
	pushes    int
}

func (p *revertingPusher) PushModelUpdate(ctx context.Context, namespace, robotID string, u command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	p.pushes++
	if u.Rollback {
		// The adapter reactivated the retained version and reported it — on both carriers,
		// both BEFORE the acknowledgement below.
		if p.ingest != nil {
			if err := p.ingest(ctx, namespace, &fav1.UpdateProgress{
				RobotId:          robotID,
				Kind:             fav1.UpdateKind_UPDATE_KIND_MODEL,
				ModelName:        u.ModelName,
				Outcome:          fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED,
				ResultingVersion: u.NewVersion,
			}); err != nil {
				return command.ModelUpdateOutcome{}, err
			}
		}
		if p.capIngest != nil {
			if err := p.capIngest(ctx, namespace,
				snapshotFor(robotID, u.ModelName, u.NewVersion, fav1.ModelStatus_MODEL_STATUS_ACTIVE)); err != nil {
				return command.ModelUpdateOutcome{}, err
			}
		}
	}
	return command.ModelUpdateOutcome{Acknowledged: true}, nil
}

// A revert that the robot reports before it acknowledges must not be overwritten by the
// reverting mark. If it is, the capability the model provides never comes back.
func TestModelRollout_RevertReportBeforeRollbackAck(t *testing.T) {
	ctx := context.Background()
	rob := raceRobot("r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusFailed, RunningVersion: "3.0.0",
		FailureReason: "sim: post-install health check failed",
	}}
	r, c := newRolloutReconciler(t, autoRollbackRollout(map[string]string{"r1": "3.0.0"}), rob)
	ing := &UpdateProgressIngestor{Client: c}
	capIng := &CapabilitiesIngestor{Client: c}
	r.Pusher = &revertingPusher{ingest: ing.IngestUpdateProgress, capIngest: capIng.IngestCapabilities}

	reconcileRollout(t, r)

	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil {
		t.Fatal("the robot's model entry disappeared")
	}
	if e.Status != fleetv1.ModelStatusActive {
		t.Errorf("status = %q, want %q — the reverting mark overwrote the revert the robot "+
			"had already reported, and nothing re-sends it", e.Status, fleetv1.ModelStatusActive)
	}
	if e.RunningVersion != "3.0.0" {
		t.Errorf("runningVersion = %q, want 3.0.0", e.RunningVersion)
	}

	// And the rollout settles rather than sitting InProgress over a robot that is fine.
	reconcileRollout(t, r)
	got := &fleetv1.ModelRollout{}
	if err := c.Get(ctx, modelRolloutKey(), got); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if got.Status.Phase == fleetv1.RolloutPhaseInProgress {
		t.Errorf("rollout still InProgress after the revert landed: %+v", got.Status)
	}
	if got.Status.CapabilitiesSuspendedOn != 0 {
		t.Errorf("capabilitiesSuspendedOn = %d, want 0 — the revert landed, so nothing is suspended",
			got.Status.CapabilitiesSuspendedOn)
	}
}

// The guard must not fire on a revert that has NOT landed: a robot that has only been
// pushed a rollback is still reverting, its capabilities are still suspended, and the
// rollout still owns the state that restores them.
func TestModelRollout_UnreportedRevertStillMarksReverting(t *testing.T) {
	rob := raceRobot("r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusFailed, RunningVersion: "3.0.0",
	}}
	r, c := newRolloutReconciler(t, autoRollbackRollout(map[string]string{"r1": "3.0.0"}), rob)
	r.Pusher = &revertingPusher{} // acknowledges, reports nothing

	reconcileRollout(t, r)

	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil || e.Status != fleetv1.ModelStatusUpdating {
		t.Fatalf("entry = %+v, want Updating — an unreported revert must stay suspended", e)
	}
	got := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), modelRolloutKey(), got); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if got.Status.CapabilitiesSuspendedOn != 1 {
		t.Errorf("capabilitiesSuspendedOn = %d, want 1", got.Status.CapabilitiesSuspendedOn)
	}
}
