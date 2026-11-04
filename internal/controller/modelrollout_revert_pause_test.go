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

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A model that fails on every robot, under rollbackPolicy Auto and pauseOnError: true.
//
// A revert is an error for pauseOnError (RFC-0001 #modelrollout-schema). Before, each
// reverted robot left the failed set before the halt was judged, so the model was pushed to
// every targeted robot in turn — suspending each robot's model-granted capabilities for a
// model already proven broken on the first — and the rollout ended Succeeded. Now the
// rollout halts after the batch in which the first revert happened, and an operator resume
// acknowledges that robot and lets the next batch go. The terminal phase is unchanged.

// brokenModelPusher is an adapter whose robots accept the model, fail its self-check and
// report Failed, and revert to the retained version when told, reporting it Active. Each
// report is sent after the acknowledgement, as an adapter that installs and then checks
// does; deliver sends the ones outstanding.
type brokenModelPusher struct {
	ingest    func(ctx context.Context, namespace string, u *fav1.UpdateProgress) error
	pending   []*fav1.UpdateProgress
	forwardTo []string
	revertTo  []string
}

func (p *brokenModelPusher) PushModelUpdate(_ context.Context, _, robotID string, u command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	report := &fav1.UpdateProgress{
		RobotId: robotID, Kind: fav1.UpdateKind_UPDATE_KIND_MODEL, ModelName: u.ModelName,
		ResultingVersion: u.NewVersion,
	}
	if u.Rollback {
		p.revertTo = append(p.revertTo, robotID)
		report.Outcome = fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED
	} else {
		p.forwardTo = append(p.forwardTo, robotID)
		report.Outcome = fav1.InstallOutcome_INSTALL_OUTCOME_FAILED
		report.FailureReason = "self-check: output is NaN"
	}
	p.pending = append(p.pending, report)
	return command.ModelUpdateOutcome{Acknowledged: true}, nil
}

func (p *brokenModelPusher) deliver(t *testing.T) {
	t.Helper()
	for _, u := range p.pending {
		if err := p.ingest(context.Background(), rolloutNS, u); err != nil {
			t.Fatalf("ingest %s report for %s: %v", u.GetOutcome(), u.GetRobotId(), err)
		}
	}
	p.pending = nil
}

// onPreviousModel is a raceRobot running item-recognition 3.0.0.
func onPreviousModel(name string) *fleetv1.Robot {
	rob := raceRobot(name)
	rob.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{
		Name: "item-recognition", Status: fleetv1.ModelStatusActive, RunningVersion: "3.0.0",
	}}
	return rob
}

// reconcileRolloutN reconciles n times, delivering the robots' outstanding reports before
// each pass.
func reconcileRolloutN(t *testing.T, r *ModelRolloutReconciler, p *brokenModelPusher, n int) {
	t.Helper()
	for range n {
		p.deliver(t)
		reconcileRollout(t, r)
	}
}

func resumeRollout(t *testing.T, c client.Client, reason string) {
	t.Helper()
	roll := getRollout(t, c, "roll")
	if roll.Annotations == nil {
		roll.Annotations = map[string]string{}
	}
	roll.Annotations[rolloutResumeAnnotation] = reason
	if err := c.Update(context.Background(), roll); err != nil {
		t.Fatalf("request resume: %v", err)
	}
}

func TestModelRollout_AutoRevertPausesOnError(t *testing.T) {
	r, c := newRolloutReconciler(t, autoRollbackRollout(nil),
		onPreviousModel("r1"), onPreviousModel("r2"), onPreviousModel("r3"))
	pusher := &brokenModelPusher{ingest: (&UpdateProgressIngestor{Client: c}).IngestUpdateProgress}
	r.Pusher = pusher

	reconcileRolloutN(t, r, pusher, 6)

	roll := getRollout(t, c, "roll")
	if len(pusher.forwardTo) != 1 || pusher.forwardTo[0] != "r1" {
		t.Fatalf("the failing model was pushed to %v; want only r1 — a revert must halt the rollout "+
			"after the batch it happened in, not let the model reach every robot", pusher.forwardTo)
	}
	if roll.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Errorf("phase = %s, want Paused after r1 failed and was reverted", roll.Status.Phase)
	}
	if len(roll.Status.RolledBackRobots) != 1 || roll.Status.RolledBackRobots[0] != "r1" || roll.Status.RobotsRolledBack != 1 {
		t.Errorf("rolledBackRobots = %v, robotsRolledBack = %d; want [r1], 1",
			roll.Status.RolledBackRobots, roll.Status.RobotsRolledBack)
	}
	if roll.Status.PausedAt == nil {
		t.Error("status.pausedAt is not set on a revert-triggered pause")
	}
	for _, name := range []string{"r2", "r3"} {
		if e := modelEntry(rolloutRobot(t, c, name), "item-recognition"); e == nil ||
			e.Status != fleetv1.ModelStatusActive || e.RunningVersion != "3.0.0" {
			t.Errorf("%s entry = %+v, want untouched Active@3.0.0", name, e)
		}
	}

	// The operator resumes past r1: the next batch goes, fails and reverts, and halts again.
	resumeRollout(t, c, "r1 reverted; inspecting on r2")
	reconcileRolloutN(t, r, pusher, 6)
	roll = getRollout(t, c, "roll")
	if len(pusher.forwardTo) != 2 || pusher.forwardTo[1] != "r2" {
		t.Fatalf("after one resume the model was pushed to %v; want r1 then r2", pusher.forwardTo)
	}
	if roll.Status.Phase != fleetv1.RolloutPhasePaused {
		t.Errorf("phase after r2 reverted = %s, want Paused", roll.Status.Phase)
	}
	if len(roll.Status.ExcludedRobots) != 1 || roll.Status.ExcludedRobots[0] != "r1" {
		t.Errorf("excludedRobots = %v, want [r1] — the acknowledged revert must stay acknowledged",
			roll.Status.ExcludedRobots)
	}

	// Resumed again, the last robot fails and reverts. Every robot has now settled, and the
	// terminal phase is what it was before this rule: a landed revert settles toward Succeeded.
	resumeRollout(t, c, "r2 reverted; last robot")
	reconcileRolloutN(t, r, pusher, 6)
	roll = getRollout(t, c, "roll")
	if len(pusher.forwardTo) != 3 {
		t.Fatalf("after two resumes the model was pushed to %v; want r1, r2, r3", pusher.forwardTo)
	}
	if roll.Status.Phase != fleetv1.RolloutPhaseSucceeded || roll.Status.RobotsRolledBack != 3 {
		t.Errorf("final phase = %s, robotsRolledBack = %d; want Succeeded, 3 (terminal phase unchanged)",
			roll.Status.Phase, roll.Status.RobotsRolledBack)
	}
}

// pauseOnError: false keeps the old behaviour: every robot is tried, each failure reverted.
func TestModelRollout_AutoRevertWithoutPauseOnErrorContinues(t *testing.T) {
	roll := autoRollbackRollout(nil)
	roll.Spec.Strategy.RollingUpdate.PauseOnError = apiBool(false)
	r, c := newRolloutReconciler(t, roll, onPreviousModel("r1"), onPreviousModel("r2"), onPreviousModel("r3"))
	pusher := &brokenModelPusher{ingest: (&UpdateProgressIngestor{Client: c}).IngestUpdateProgress}
	r.Pusher = pusher

	reconcileRolloutN(t, r, pusher, 12)

	got := getRollout(t, c, "roll")
	if len(pusher.forwardTo) != 3 {
		t.Errorf("the model was pushed to %v; want all three robots under pauseOnError: false", pusher.forwardTo)
	}
	if got.Status.Phase != fleetv1.RolloutPhaseSucceeded || got.Status.RobotsRolledBack != 3 {
		t.Errorf("phase = %s, robotsRolledBack = %d; want Succeeded, 3", got.Status.Phase, got.Status.RobotsRolledBack)
	}
}
