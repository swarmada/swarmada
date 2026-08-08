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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// UpdateProgressIngestor surfaces an adapter's advisory per-robot rollout progress
// (UpdateProgress, §6.6/§6.7) as the active rollout's
// status.currentBatch[].updatePhase. It correlates the reported robot to the one
// InProgress FirmwareRollout/ModelRollout in its namespace whose batch contains it,
// and patches that entry's updatePhase. It writes nothing when the robot is not in
// any active batch, so a stray or late report is harmless. The rollout controller
// preserves updatePhase when it recomputes the batch, so the two writers cooperate.
type UpdateProgressIngestor struct {
	client.Client

	// now overrides the clock in tests. Nil means time.Now.
	now func() time.Time
}

// IngestUpdateProgress applies one UpdateProgress. It is a no-op (returning nil)
// when the report is empty, the robot is unknown, or the robot is not in an active
// batch of the reported kind.
func (i *UpdateProgressIngestor) IngestUpdateProgress(ctx context.Context, namespace string, u *fav1.UpdateProgress) error {
	// A terminal report need not carry a phase — the install is over, so there is no phase
	// left to be in. Requiring one (as this did) would drop exactly the message that ends a
	// rollout, which is the one message it cannot afford to lose.
	terminal := u.GetOutcome() != fav1.InstallOutcome_INSTALL_OUTCOME_UNSPECIFIED
	if u == nil || u.GetRobotId() == "" || (u.GetPhase() == "" && !terminal) {
		return nil
	}
	robot, err := findRobotByIDInNamespace(ctx, i.Client, namespace, u.GetRobotId())
	if err != nil {
		return err
	}
	if robot == nil {
		return nil // robot_id not mapped to a Robot yet; drop, don't fail
	}
	key := client.ObjectKeyFromObject(robot)

	switch u.GetKind() {
	case fav1.UpdateKind_UPDATE_KIND_FIRMWARE:
		if terminal {
			if err := projectFirmwareState(ctx, i.Client, key, firmwareStateFromOutcome(u, i.clock())); err != nil {
				return err
			}
		}
		if u.GetPhase() == "" {
			return nil
		}
		return i.applyFirmwarePhase(ctx, robot.Namespace, robot.Name, u.GetPhase())
	case fav1.UpdateKind_UPDATE_KIND_MODEL:
		if terminal {
			// UpdateProgress does not name the model; the active rollout does. Resolving it
			// here keeps the wire message small and avoids a second source of truth for
			// which model a robot is being moved to.
			name, err := i.activeModelName(ctx, robot.Namespace, robot.Name)
			if err != nil {
				return err
			}
			if name != "" {
				if err := projectModelState(ctx, i.Client, key, modelEntryFromOutcome(u, name)); err != nil {
					return err
				}
			}
		}
		if u.GetPhase() == "" {
			return nil
		}
		return i.applyModelPhase(ctx, robot.Namespace, robot.Name, u.GetPhase())
	default:
		return nil // UNSPECIFIED: cannot correlate to a rollout kind
	}
}

// clock is the ingestor's time source; nil means time.Now (overridden in tests).
func (i *UpdateProgressIngestor) clock() time.Time {
	if i.now != nil {
		return i.now()
	}
	return time.Now()
}

// activeModelName returns the model the robot's in-flight ModelRollout is installing, or ""
// when the robot is not in an active batch.
func (i *UpdateProgressIngestor) activeModelName(ctx context.Context, namespace, robotName string) (string, error) {
	var list fleetv1.ModelRolloutList
	if err := i.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return "", err
	}
	for idx := range list.Items {
		ro := &list.Items[idx]
		if ro.Status.Phase != fleetv1.RolloutPhaseInProgress {
			continue
		}
		if batchIndexOf(ro.Status.CurrentBatch, robotName) >= 0 {
			return ro.Spec.ModelName, nil
		}
	}
	return "", nil
}

// firmwareStateFromOutcome maps a terminal firmware report.
//
// resulting_version is trusted for BOTH outcomes, including failure: a failed install may
// leave the robot on its previous version, on a recovery image, or elsewhere, and only the
// robot knows which. Assuming the old version here would invent the one fact the operator
// most needs to be true.
func firmwareStateFromOutcome(u *fav1.UpdateProgress, now time.Time) *fleetv1.FirmwareInstallState {
	st := &fleetv1.FirmwareInstallState{
		RunningVersion: u.GetResultingVersion(),
		ReportedAt:     &metav1.Time{Time: now},
	}
	if u.GetOutcome() == fav1.InstallOutcome_INSTALL_OUTCOME_FAILED {
		st.Status = fleetv1.FirmwareInstallFailed
		st.FailureReason = u.GetFailureReason()
		st.AttemptedVersion = "" // filled by the rollout controller, which knows the target
		return st
	}
	st.Status = fleetv1.FirmwareInstallRunning
	return st
}

// modelEntryFromOutcome maps a terminal model report onto the robot's model entry.
func modelEntryFromOutcome(u *fav1.UpdateProgress, modelName string) fleetv1.InstalledModelStatusEntry {
	e := fleetv1.InstalledModelStatusEntry{
		Name:           modelName,
		RunningVersion: u.GetResultingVersion(),
	}
	if u.GetOutcome() == fav1.InstallOutcome_INSTALL_OUTCOME_FAILED {
		e.Status = fleetv1.ModelStatusFailed
		e.FailureReason = u.GetFailureReason()
		return e
	}
	e.Status = fleetv1.ModelStatusActive
	return e
}

func (i *UpdateProgressIngestor) applyFirmwarePhase(ctx context.Context, namespace, robotName, phase string) error {
	var list fleetv1.FirmwareRolloutList
	if err := i.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return err
	}
	for idx := range list.Items {
		ro := &list.Items[idx]
		if ro.Status.Phase != fleetv1.RolloutPhaseInProgress {
			continue
		}
		if j := batchIndexOf(ro.Status.CurrentBatch, robotName); j >= 0 {
			if ro.Status.CurrentBatch[j].UpdatePhase == phase {
				return nil // no change (RA-1: no status write per tick)
			}
			base := ro.DeepCopy()
			ro.Status.CurrentBatch[j].UpdatePhase = phase
			return i.Status().Patch(ctx, ro, client.MergeFrom(base))
		}
	}
	return nil
}

func (i *UpdateProgressIngestor) applyModelPhase(ctx context.Context, namespace, robotName, phase string) error {
	var list fleetv1.ModelRolloutList
	if err := i.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return err
	}
	for idx := range list.Items {
		ro := &list.Items[idx]
		if ro.Status.Phase != fleetv1.RolloutPhaseInProgress {
			continue
		}
		if j := batchIndexOf(ro.Status.CurrentBatch, robotName); j >= 0 {
			if ro.Status.CurrentBatch[j].UpdatePhase == phase {
				return nil
			}
			base := ro.DeepCopy()
			ro.Status.CurrentBatch[j].UpdatePhase = phase
			return i.Status().Patch(ctx, ro, client.MergeFrom(base))
		}
	}
	return nil
}

// batchIndexOf returns the index of robotName in a rollout batch, or -1.
func batchIndexOf(batch []fleetv1.RolloutBatchRobot, robotName string) int {
	for i := range batch {
		if batch[i].RobotName == robotName {
			return i
		}
	}
	return -1
}
