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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The correlation race that pins a robot at Updating, reproduced deterministically.
//
// The reference adapter enqueues its terminal UpdateProgress on the SAME outbox as the
// CommandResult, and it does so FIRST (adapters/simulation/sim_adapter.py:611-617, then
// :400). So on the wire the install outcome always precedes the acknowledgement that
// PushModelUpdate is blocking on — and therefore precedes both the Robot's Updating mark
// and the rollout's status.currentBatch write, which are the only two things the ingestor
// can correlate against.
//
// racingPusher reproduces exactly that ordering: it delivers the terminal report while
// the push is still in flight, then acknowledges. Nothing here is timing-dependent — the
// ingest happens inside PushModelUpdate, so the "race" is deterministic, which is the
// point: it is not a race that a slower robot avoids, it is the order this adapter always
// produces.
type racingPusher struct {
	ingest func(ctx context.Context, namespace string, u *fav1.UpdateProgress) error
	// nameAtPush records what the ingestor could resolve the model to at the moment
	// the report landed. "" is the defect.
	nameAtPush string
	resolveErr error
	resolve    func(ctx context.Context, namespace, robotName string) (string, error)
}

func (p *racingPusher) PushModelUpdate(ctx context.Context, namespace, robotID string, u command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	if p.resolve != nil {
		p.nameAtPush, p.resolveErr = p.resolve(ctx, namespace, robotID)
	}
	// The adapter installed and reported before it acked. Deliver the terminal outcome now.
	if p.ingest != nil {
		if err := p.ingest(ctx, namespace, &fav1.UpdateProgress{
			RobotId:          robotID,
			Kind:             fav1.UpdateKind_UPDATE_KIND_MODEL,
			Outcome:          fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED,
			ResultingVersion: u.NewVersion,
		}); err != nil {
			return command.ModelUpdateOutcome{}, err
		}
	}
	return command.ModelUpdateOutcome{Acknowledged: true}, nil
}

// resolveVia adapts activeModelName (which takes the Robot, so it can read its labels) to
// the (namespace, name) shape the pusher stub observes at push time.
func resolveVia(c client.Client, ing *UpdateProgressIngestor) func(context.Context, string, string) (string, error) {
	return func(ctx context.Context, namespace, robotName string) (string, error) {
		rob := &fleetv1.Robot{}
		if err := c.Get(ctx, types.NamespacedName{Name: robotName, Namespace: namespace}, rob); err != nil {
			return "", err
		}
		// No model_name: this exercises the CORRELATION path (the push-before-batch window), which is
		// what an adapter that does not set the field still relies on.
		return ing.activeModelName(ctx, rob, &fav1.UpdateProgress{
			Outcome: fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED,
		})
	}
}

// raceRobot is targetRobot plus the robot-id annotation the ingestor resolves by.
func raceRobot(name string) *fleetv1.Robot {
	rob := targetRobot(name, fleetv1.RobotPhaseIdle, 90)
	rob.Annotations = map[string]string{RobotIDAnnotation: name}
	return rob
}

// TestModelRollout_TerminalReportBeforeBatchWrite is the regression test for the lost
// terminal install outcome.
//
// A terminal install outcome that arrives while the push is still in flight must still
// reach Robot.status.installedModels with the version the robot reported. Before the fix
// it was dropped silently and the robot pinned at Updating with an empty runningVersion.
func TestModelRollout_TerminalReportBeforeBatchWrite(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(), raceRobot("r1"))
	ing := &UpdateProgressIngestor{Client: c}
	pusher := &racingPusher{
		ingest:  ing.IngestUpdateProgress,
		resolve: resolveVia(c, ing),
	}
	r.Pusher = pusher
	reconcileRollout(t, r)

	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil {
		t.Fatal("no installedModels entry for item-recognition at all")
	}
	if e.RunningVersion != "3.2.1" {
		t.Errorf("runningVersion = %q, want %q — the terminal report was lost", e.RunningVersion, "3.2.1")
	}
	if e.Status != fleetv1.ModelStatusActive {
		t.Errorf("status = %q, want %q — the robot is pinned mid-update", e.Status, fleetv1.ModelStatusActive)
	}
}

// TestModelRollout_CorrelationTargetAbsentDuringPush isolates the MECHANISM rather than
// the symptom: at the instant the adapter's report arrives, is there anything for the
// ingestor to correlate it to? Before the fix, no — status.currentBatch is written after
// the whole push loop, so it is empty for the first robot of the first batch.
func TestModelRollout_CorrelationTargetAbsentDuringPush(t *testing.T) {
	r, c := newRolloutReconciler(t, pickerRollout(), raceRobot("r1"))
	ing := &UpdateProgressIngestor{Client: c}
	pusher := &racingPusher{resolve: resolveVia(c, ing)}
	r.Pusher = pusher
	reconcileRollout(t, r)

	if pusher.resolveErr != nil {
		t.Fatalf("resolving the model name at push time: %v", pusher.resolveErr)
	}
	if pusher.nameAtPush != "item-recognition" {
		t.Errorf("model name resolvable at push time = %q, want %q; "+
			"an unresolvable name is what drops the terminal report",
			pusher.nameAtPush, "item-recognition")
	}
}

// The rollback revert target must survive the same early report. A robot that reports the
// new version before the batch is recorded still has to be revertible.
func TestModelRollout_EarlyReportKeepsRollbackTarget(t *testing.T) {
	rob := raceRobot("r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.0.0",
		ActiveAt: &metav1.Time{},
	}}
	r, c := newRolloutReconciler(t, pickerRollout(), rob)
	ing := &UpdateProgressIngestor{Client: c}
	r.Pusher = &racingPusher{ingest: ing.IngestUpdateProgress, resolve: resolveVia(c, ing)}
	reconcileRollout(t, r)

	ro := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), modelRolloutKey(), ro); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if got := ro.Status.RollbackVersions["r1"]; got != "3.0.0" {
		t.Errorf("rollbackVersions[r1] = %q, want %q — the revert target was lost", got, "3.0.0")
	}
}

// decliningPusher refuses the artifact and, like the reference adapter, reports a TERMINAL
// FAILED outcome for the refusal — it declined, so it installed nothing and changed nothing
// about itself.
type decliningPusher struct {
	ingest func(ctx context.Context, namespace string, u *fav1.UpdateProgress) error
}

func (p *decliningPusher) PushModelUpdate(ctx context.Context, namespace, robotID string, _ command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	if p.ingest != nil {
		if err := p.ingest(ctx, namespace, &fav1.UpdateProgress{
			RobotId:       robotID,
			Kind:          fav1.UpdateKind_UPDATE_KIND_MODEL,
			Outcome:       fav1.InstallOutcome_INSTALL_OUTCOME_FAILED,
			FailureReason: "refused: checksum mismatch",
		}); err != nil {
			return command.ModelUpdateOutcome{}, err
		}
	}
	return command.ModelUpdateOutcome{Acknowledged: false, Message: "sim: refusing unverified model artifact"}, nil
}

// A DECLINED push must not cost the fleet a capability. enterBatch never enters a declined
// robot into the batch precisely so that an undeliverable update is free; a terminal FAILED
// outcome for that refusal must not sneak the same cost in through the ingestor.
//
// This guards a regression that was live on a cluster: correlating the refusal through the
// target selector marked the model Failed on robots still running the previous version,
// took autonomous_navigation Inactive on all three, and paused every rollout for that
// model — including two that had already Succeeded.
func TestModelRollout_DeclinedPushDoesNotFailTheModel(t *testing.T) {
	rob := raceRobot("r1")
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.0.0",
	}}
	r, c := newRolloutReconciler(t, pickerRollout(), rob)
	ing := &UpdateProgressIngestor{Client: c}
	r.Pusher = &decliningPusher{ingest: ing.IngestUpdateProgress}
	reconcileRollout(t, r)

	e := modelEntry(rolloutRobot(t, c, "r1"), "item-recognition")
	if e == nil {
		t.Fatal("the robot's existing model entry disappeared")
	}
	if e.Status == fleetv1.ModelStatusFailed {
		t.Error("a DECLINED push marked the model Failed; the robot never installed anything " +
			"and is still running its previous version")
	}
	if e.RunningVersion != "3.0.0" {
		t.Errorf("runningVersion = %q, want 3.0.0 — a refusal must not move it", e.RunningVersion)
	}

	ro := &fleetv1.ModelRollout{}
	if err := c.Get(context.Background(), modelRolloutKey(), ro); err != nil {
		t.Fatalf("get rollout: %v", err)
	}
	if ro.Status.Phase == fleetv1.RolloutPhasePaused {
		t.Error("a declined push paused the rollout; it should stay Pending and retry")
	}
}
