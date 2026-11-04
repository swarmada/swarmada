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
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// UpdateProgressIngestor surfaces an adapter's advisory per-robot rollout progress
// (UpdateProgress, RFC-0001 #firmwarerollout-schema and #modelrollout-schema) as the active rollout's
// status.currentBatch[].updatePhase. It correlates the reported robot to the one
// InProgress FirmwareRollout/ModelRollout in its namespace whose batch contains it,
// and patches that entry's updatePhase. It writes nothing when the robot is not in
// any active batch, so a stray or late report is harmless. The rollout controller
// preserves updatePhase when it recomputes the batch, so the two writers cooperate.
type UpdateProgressIngestor struct {
	client.Client

	// Acks is the ledger of acknowledged model_update pushes, shared with the ModelRollout
	// controller. A FAILED report no batch holds is recorded when the robot's latest push
	// of that model was acknowledged. Nil drops such a report, as before.
	Acks *ModelAckLedger

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
			// UpdateProgress.model_name names the model when the adapter sets it; when it
			// does not, the model has to be recovered from the active rollout. See
			// activeModelName for why the second route can decline to answer.
			name, err := i.activeModelName(ctx, robot, u)
			if err != nil {
				return err
			}
			if name == "" {
				// A terminal outcome that cannot be correlated is the ONE message a rollout
				// cannot afford to lose, so losing it is said out loud rather than dropped in
				// silence (observed: three robots pinned at Updating for 100s with nothing
				// in any log to say why). The recovery carrier — CapabilitiesSnapshot, ADR-0033
				// — is what repairs it; this line is how an operator learns it was needed.
				logf.FromContext(ctx).Info("terminal model install outcome could not be correlated to a rollout; "+
					"awaiting the CapabilitiesSnapshot recovery path (ADR-0033)",
					"robot", robot.Name, "namespace", robot.Namespace,
					"resultingVersion", u.GetResultingVersion(), "outcome", u.GetOutcome().String())
			} else if err := projectModelState(ctx, i.Client, key, modelEntryFromOutcome(u, name)); err != nil {
				return err
			}
		}
		if u.GetPhase() == "" {
			return nil
		}
		return i.applyModelPhase(ctx, robot.Namespace, robot.Name, u.GetPhase(), u.GetModelName())
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

// activeModelName returns the model a terminal report is about, or "" when it cannot be
// established without guessing.
//
// THE REPORT ITSELF IS THE FIRST SOURCE. UpdateProgress.model_name (ADR-0055) names the
// model directly. It was added because the correlation below has a precondition it cannot
// enforce: it assumes a robot is in at most one in-flight model rollout, and a robot in two
// at once is by DESIGN, not misuse — classifyModel and upsertModelStatus key strictly on
// modelName, so a robot marked Updating for nav-stack still classifies as pending for
// vision-stack, becomes eligible, and enters the second rollout's batch while still in the
// first. An adapter that sets the field removes the question; the correlation
// remains for adapters that do not, and now declines rather than guessing.
//
// A named model does NOT license a report the guards below would refuse — see the FAILED
// asymmetry. It says WHICH model, never WHETHER to record.
//
// TWO correlation sources, in order of authority, because the obvious one is not available
// when it is most needed:
//
//  1. status.currentBatch — the control plane has recorded that this robot is in this
//     rollout's batch. Authoritative for WHICH ROLLOUT; ambiguous for WHICH MODEL only when
//     the robot is in two batches and the report named no model.
//
//  2. spec.targetSelector — the in-flight window. enterBatch PUSHES before it records
//     anything (deliberately: an undeliverable update must never suspend a capability),
//     and the reference adapter answers with its terminal outcome BEFORE the ack that push
//     is blocked on. So for a fast robot the report ALWAYS arrives while (1) is still
//     empty, and correlating on (1) alone drops exactly the message that ends the rollout.
//     The spec is written before the controller ever runs and is not subject to that
//     ordering, so it can answer during the window. SUCCEEDED outcomes only — see below.
//
// Two rollouts targeting one robot for DIFFERENT models is ambiguous, and this refuses to
// guess: an install outcome attributed to the wrong model would write a running version
// the robot never reported for it. It returns "" and lets the recovery carrier settle it.
func (i *UpdateProgressIngestor) activeModelName(ctx context.Context, robot *fleetv1.Robot,
	u *fav1.UpdateProgress,
) (string, error) {
	reported, outcome := u.GetModelName(), u.GetOutcome()
	var list fleetv1.ModelRolloutList
	if err := i.List(ctx, &list, client.InNamespace(robot.Namespace)); err != nil {
		return "", err
	}
	// Source (1). Every InProgress rollout holding this robot is examined rather than the
	// first one found: taking the first is what made the outcome land on whichever model
	// the API server's name-sorted list order happened to put in front.
	var batched string
	for idx := range list.Items {
		ro := &list.Items[idx]
		if ro.Status.Phase != fleetv1.RolloutPhaseInProgress {
			continue
		}
		if batchIndexOf(ro.Status.CurrentBatch, robot.Name) < 0 {
			continue
		}
		if reported != "" && ro.Spec.ModelName != reported {
			continue // a different model's rollout; this report is not about it
		}
		if batched != "" && batched != ro.Spec.ModelName {
			// Two in-flight models for this robot and a report that does not say which.
			// Refuse: a wrong-model write puts a model in the failed set and, under
			// rollbackPolicy: Auto, pushes a revert of a model that never failed.
			logf.FromContext(ctx).Info("model install outcome matches two in-flight rollouts and the report "+
				"names no model; refusing to guess (set UpdateProgress.model_name — ADR-0055)",
				"robot", robot.Name, "namespace", robot.Namespace,
				"candidates", []string{batched, ro.Spec.ModelName})
			return "", nil
		}
		batched = ro.Spec.ModelName
	}
	if batched != "" {
		return batched, nil
	}

	// The fallback below is for SUCCEEDED outcomes ONLY, and the asymmetry is the whole
	// point rather than caution.
	//
	// A SUCCEEDED outcome carries resulting_version: the robot is telling us what it is NOW
	// RUNNING. That is true independently of the control plane's bookkeeping, so recording
	// it against the one rollout that targets this robot for this model is safe.
	//
	// A FAILED outcome is not symmetric. The reference adapter emits INSTALL_OUTCOME_FAILED
	// when it REFUSES an artifact — a push it declined, having installed nothing and having
	// changed nothing about itself (sim_adapter.py, _on_model_update). enterBatch never
	// enters a declined robot into the batch, deliberately: "an update which cannot be
	// delivered never costs the fleet a capability." Correlating that refusal through the
	// selector would mark the model Failed on a robot that is still happily running the
	// previous version, take its model-driven capabilities Inactive, and pause every rollout
	// targeting that model — the exact outcome the decline path exists to prevent. Observed
	// doing precisely that before this guard was added.
	//
	// So a failure is correlated only to a robot whose push was acknowledged: through
	// currentBatch, or, because the batch is written only by the rollout status patch at the
	// end of the reconcile, through the ledger of acknowledgements for a failure reported
	// between the acknowledgement and that write. A decline's FAILED report is ingested before
	// its push returns, so it never finds a ledger entry (ModelAckLedger).
	if outcome != fav1.InstallOutcome_INSTALL_OUTCOME_SUCCEEDED {
		return i.Acks.acknowledgedModel(robot.Namespace, robot.Name, reported), nil
	}

	// A SUCCEEDED report that NAMES its model needs no correlation at all, and this is the
	// case the selector fallback cannot serve: an adapter may legitimately report an install
	// of a model no rollout is driving (a manual push, a model restored on the robot). The
	// robot is describing its own state, which is what status.installedModels records; the
	// rollout list is bookkeeping about that state, not a licence for it.
	if reported != "" {
		return reported, nil
	}

	robotLabels := labels.Set(robot.GetLabels())
	var candidate string
	for idx := range list.Items {
		ro := &list.Items[idx]
		// An EdgeNode rollout resolves its selector against nodes, not robots, and is not
		// implemented at v0.4 — matching a robot against it would attribute an install to a
		// rollout that never pushed anything.
		if ro.Spec.TargetKind == fleetv1.RolloutTargetEdgeNode {
			continue
		}
		if rolloutIsSettled(ro.Status.Phase) {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&ro.Spec.TargetSelector)
		if err != nil {
			continue // an unparseable selector targets nothing; the controller reports it
		}
		if !sel.Matches(robotLabels) {
			continue
		}
		if candidate != "" && candidate != ro.Spec.ModelName {
			return "", nil // ambiguous — do not guess
		}
		candidate = ro.Spec.ModelName
	}
	return candidate, nil
}

// rolloutIsSettled reports whether a rollout has reached a terminal phase and can no
// longer be the source of an in-flight install.
func rolloutIsSettled(p fleetv1.RolloutPhase) bool {
	return p == fleetv1.RolloutPhaseSucceeded || p == fleetv1.RolloutPhaseFailed
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

// applyModelPhase patches the advisory updatePhase onto the rollout the report belongs to.
//
// It carries the SAME correlation problem as activeModelName and is fixed the same way: a
// robot can sit in two InProgress model rollouts at once, and writing the first match meant
// one model's "Downloading" overwrote the other model's phase. modelName (from
// UpdateProgress.model_name) selects; an unnamed report that matches two rollouts writes
// nothing rather than the wrong one. A dropped advisory phase costs a progress display; a
// wrong one misreports which install a robot is in the middle of.
func (i *UpdateProgressIngestor) applyModelPhase(ctx context.Context, namespace, robotName, phase, modelName string) error {
	var list fleetv1.ModelRolloutList
	if err := i.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return err
	}
	var target *fleetv1.ModelRollout
	var at int
	for idx := range list.Items {
		ro := &list.Items[idx]
		if ro.Status.Phase != fleetv1.RolloutPhaseInProgress {
			continue
		}
		j := batchIndexOf(ro.Status.CurrentBatch, robotName)
		if j < 0 {
			continue
		}
		if modelName != "" && ro.Spec.ModelName != modelName {
			continue
		}
		if target != nil && target.Spec.ModelName != ro.Spec.ModelName {
			logf.FromContext(ctx).V(1).Info("advisory model updatePhase matches two in-flight rollouts and the "+
				"report names no model; dropping rather than writing the wrong one (ADR-0055)",
				"robot", robotName, "namespace", namespace, "phase", phase)
			return nil
		}
		target, at = ro, j
	}
	if target == nil {
		return nil
	}
	if target.Status.CurrentBatch[at].UpdatePhase == phase {
		return nil // no change (RA-1: no status write per tick)
	}
	base := target.DeepCopy()
	target.Status.CurrentBatch[at].UpdatePhase = phase
	return i.Status().Patch(ctx, target, client.MergeFrom(base))
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
