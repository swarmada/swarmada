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

// silentPusher acknowledges the push and reports NOTHING back — the primary carrier is
// deliberately dropped. This is the case ADR-0033 names the second carrier for: a terminal
// UpdateProgress lost because the stream dropped or the control plane restarted mid-install.
type silentPusher struct{ pushes int }

func (p *silentPusher) PushModelUpdate(_ context.Context, _, _ string, _ command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	p.pushes++
	return command.ModelUpdateOutcome{Acknowledged: true}, nil
}

// snapshotFor builds the CapabilitiesSnapshot the reference adapter now sends after an
// install transition — models sourced from what it is actually running, carrying
// running_version and status.
func snapshotFor(robotID, model, version string, status fav1.ModelStatus) *fav1.CapabilitiesSnapshot {
	return &fav1.CapabilitiesSnapshot{
		RobotId: robotID,
		InstalledModels: []*fav1.InstalledModel{{
			Name: model, Version: version, RunningVersion: version, Status: status,
		}},
	}
}

// With the primary acknowledgement dropped entirely, the rollout must still complete off
// the CapabilitiesSnapshot. If it cannot, the correlation race has no backstop and one
// lost message strands the fleet.
func TestModelRollout_RecoveryCarrierCompletesRollout(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(), raceRobot("r1"))
	r.Pusher = &silentPusher{}
	ctx := context.Background()

	// Push happens; nothing is reported back. The robot pins at Updating — correctly, at
	// this point it genuinely is updating as far as the control plane knows.
	reconcileRollout(t, r)
	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusUpdating {
		t.Fatalf("expected the robot to be Updating after a silent push, got %+v", e)
	}

	// The recovery carrier arrives.
	cap := &CapabilitiesIngestor{Client: c}
	if err := cap.IngestCapabilities(ctx, rolloutNS,
		snapshotFor("r1", "item-recognition", "3.2.1", fav1.ModelStatus_MODEL_STATUS_ACTIVE)); err != nil {
		t.Fatalf("ingesting recovery snapshot: %v", err)
	}

	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil || e.RunningVersion != "3.2.1" || e.Status != fleetv1.ModelStatusActive {
		t.Fatalf("recovery carrier did not deliver: %+v", e)
	}

	// And the rollout settles on the next pass.
	reconcileRollout(t, r)
	ro := &fleetv1.ModelRollout{}
	if err := c.Get(ctx, modelRolloutKey(), ro); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if ro.Status.Phase != fleetv1.RolloutPhaseSucceeded {
		t.Errorf("rollout phase = %q, want Succeeded — the recovery path did not complete the rollout", ro.Status.Phase)
	}
	if ro.Status.RobotsUpdated != 1 {
		t.Errorf("robotsUpdated = %d, want 1", ro.Status.RobotsUpdated)
	}
}

// A snapshot that repeats an unchanged projection must write no status (RA-1).
func TestModelRollout_RecoveryCarrierIsIdempotent(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(), raceRobot("r1"))
	r.Pusher = &silentPusher{}
	ctx := context.Background()
	reconcileRollout(t, r)

	cap := &CapabilitiesIngestor{Client: c}
	snap := snapshotFor("r1", "item-recognition", "3.2.1", fav1.ModelStatus_MODEL_STATUS_ACTIVE)
	if err := cap.IngestCapabilities(ctx, rolloutNS, snap); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	before := rolloutRobot(t, c, "r1").ResourceVersion
	for i := 0; i < 3; i++ {
		if err := cap.IngestCapabilities(ctx, rolloutNS, snap); err != nil {
			t.Fatalf("repeat ingest %d: %v", i, err)
		}
	}
	if after := rolloutRobot(t, c, "r1").ResourceVersion; after != before {
		t.Errorf("resourceVersion moved %q -> %q on an unchanged snapshot; RA-1 forbids the write", before, after)
	}
}

// A FAILED install reported through the recovery carrier must reach the failed set, so
// Auto rollback can act on it. The reference adapter used to hard-code ACTIVE in every
// model it reported, which is why rollbackPolicy: Auto was unexercised by any live demo.
func TestModelRollout_RecoveryCarrierCarriesFailure(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(), raceRobot("r1"))
	r.Pusher = &silentPusher{}
	ctx := context.Background()
	reconcileRollout(t, r)

	cap := &CapabilitiesIngestor{Client: c}
	if err := cap.IngestCapabilities(ctx, rolloutNS,
		snapshotFor("r1", "item-recognition", "3.2.1", fav1.ModelStatus_MODEL_STATUS_FAILED)); err != nil {
		t.Fatalf("ingesting failure snapshot: %v", err)
	}
	if e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition"); e == nil || e.Status != fleetv1.ModelStatusFailed {
		t.Fatalf("failure did not project: %+v", e)
	}
	reconcileRollout(t, r)

	ro := &fleetv1.ModelRollout{}
	if err := c.Get(ctx, modelRolloutKey(), ro); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if len(ro.Status.FailedRobots) != 1 {
		t.Errorf("failedRobots = %+v, want one entry", ro.Status.FailedRobots)
	}
}

// The full Auto rollback arc, driven entirely through the recovery carrier: a robot that
// reports its install FAILED is reverted to the version it was on, and the capability the
// model provides comes back once the revert is reported.
//
// This is the arc UC-1.4 steps (4)-(5) describe. It is proven here rather than on a cluster
// because the reference adapter has no way to ACCEPT an install and then report it failed —
// it can only decline, and a decline is not a failed install. See the report.
func TestModelRollout_AutoRollbackRevertsAndResumes(t *testing.T) {
	ctx := context.Background()
	ro := pickerRollout()
	ro.Spec.RollbackPolicy = fleetv1.ModelRollbackAuto

	rob := raceRobot("r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.0.0",
	}}
	r, c := newRolloutReconciler(t, ro, rob)
	pusher := &silentPusher{}
	r.Pusher = pusher

	// 1. Push. The robot enters the batch on the ack; 3.0.0 is captured as the revert target.
	reconcileRollout(t, r)
	got := &fleetv1.ModelRollout{}
	if err := c.Get(ctx, modelRolloutKey(), got); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if got.Status.RollbackVersions["r1"] != "3.0.0" {
		t.Fatalf("revert target = %q, want 3.0.0", got.Status.RollbackVersions["r1"])
	}

	// 2. The robot reports the install FAILED through the recovery carrier.
	cap := &CapabilitiesIngestor{Client: c}
	if err := cap.IngestCapabilities(ctx, rolloutNS,
		snapshotFor("r1", "item-recognition", "3.2.1", fav1.ModelStatus_MODEL_STATUS_FAILED)); err != nil {
		t.Fatalf("failure snapshot: %v", err)
	}

	// 3. The rollback pass reverts it — a second push, carrying the retained version.
	reconcileRollout(t, r)
	if pusher.pushes != 2 {
		t.Fatalf("pushes = %d, want 2 (the update and the revert)", pusher.pushes)
	}
	if err := c.Get(ctx, modelRolloutKey(), got); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if len(got.Status.RolledBackRobots) != 1 || got.Status.RolledBackRobots[0] != "r1" {
		t.Fatalf("rolledBackRobots = %v, want [r1]", got.Status.RolledBackRobots)
	}

	// 4. The robot reports the PREVIOUS version running again. Only now is the revert real —
	//    an acknowledgement is a promise to revert, not evidence of one.
	if err := cap.IngestCapabilities(ctx, rolloutNS,
		snapshotFor("r1", "item-recognition", "3.0.0", fav1.ModelStatus_MODEL_STATUS_ACTIVE)); err != nil {
		t.Fatalf("revert snapshot: %v", err)
	}
	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil || e.RunningVersion != "3.0.0" || e.Status != fleetv1.ModelStatusActive {
		t.Fatalf("after revert the robot reads %+v, want 3.0.0 Active", e)
	}

	// 5. The rollout settles, and the model is Active again — so a model-driven capability
	//    gated on it derives back to Active rather than staying suspended for ever.
	reconcileRollout(t, r)
	if err := c.Get(ctx, modelRolloutKey(), got); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if got.Status.Phase == fleetv1.RolloutPhaseInProgress {
		t.Errorf("rollout still InProgress after the revert settled: %+v", got.Status)
	}
}
