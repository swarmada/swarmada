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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/command"
)

// ModelRolloutReconciler is the OTA/Model Update Manager for ModelRollout
// (RFC-0001 §9.3.6). It selects target robots by safety constraints, marks the
// model Updating on batch entry (which suspends the model-driven capabilities the
// Capability Controller derives — the effective §9.3.6 suspension without writing
// status.capabilities directly), and projects the granted capabilities into
// Robot.status.modelGrantedCapabilities[] once a robot reports the model Active at
// the new version. It never writes Robot.spec.
//
// The model_update Command push, the adapter-side artifact signature/checksum
// verification (§9.2.8), and the Active/Failed report via telemetry ride the
// ControlStream wire (not yet built); this drives the control-plane state machine
// over the observable Robot status.
type ModelRolloutReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Audit seals the §9.6.5.1 model-update and artifact-verification entries into the
	// tamper-evident chain. Nil disables recording; the rollout is unaffected.
	Audit audit.Recorder
	// Pusher pushes the model_update Command to a robot's adapter over ControlStream
	// (§9.3.6). When set, batch entry is gated on the adapter acknowledging the
	// update, so capabilities are never suspended for an update that cannot be
	// delivered. Nil (ControlStream disabled) falls back to observe-only: the model
	// is marked Updating and the controller awaits the adapter-reported Active.
	Pusher command.ModelUpdatePusher
}

// +kubebuilder:rbac:groups=swarmada.io,resources=modelrollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=modelrollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots/status,verbs=get;update;patch

type modelState int

const (
	modelPending modelState = iota
	modelUpdating
	modelDone
	modelFailed
	// modelNewer — the robot already runs a version STRICTLY NEWER than the target. It is
	// not a rollout candidate: dispatching would downgrade it. Distinct from modelDone,
	// which means "already at the target", because the two need different reporting — an
	// operator reading `robotsUpdated` should not be told a downgrade-refusal succeeded.
	modelNewer
)

// Reconcile drives one ModelRollout.
func (r *ModelRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("modelrollout", req.NamespacedName)

	rollout := &fleetv1.ModelRollout{}
	if err := r.Get(ctx, req.NamespacedName, rollout); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	sel, err := metav1.LabelSelectorAsSelector(&rollout.Spec.TargetSelector)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid targetSelector: %w", err)
	}
	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(req.Namespace),
		client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing target robots: %w", err)
	}

	now := time.Now()
	windowOnly := rollout.Spec.SafetyConstraints.MaintenanceWindowOnly

	// Carry forward the rollback bookkeeping — the previous version survives ONLY
	// here (once a robot reports newVersion, its running version is the new one), and
	// a robot this rollout already reverted must never be pulled back into an update
	// loop. Both are seeded from the persisted status and mutated below.
	rollbackVersions := copyStringMap(rollout.Status.RollbackVersions)
	rolledBackSet := make(map[string]bool, len(rollout.Status.RolledBackRobots))
	for _, n := range rollout.Status.RolledBackRobots {
		rolledBackSet[n] = true
	}

	// An operator resume converts the robots that are currently failed into this
	// rollout's excluded set (ADR-0041), so the pause cannot re-latch off the same
	// robots on the very next reconcile. Applied BEFORE classification so this pass
	// already sees them excluded.
	if done, err := r.applyResume(ctx, rollout); err != nil {
		return ctrl.Result{}, err
	} else if done {
		return ctrl.Result{Requeue: true}, nil
	}
	excludedSet := make(map[string]bool, len(rollout.Status.ExcludedRobots))
	for _, n := range rollout.Status.ExcludedRobots {
		excludedSet[n] = true
	}

	var done, updating, failed, eligible, rolledBack, newer, excluded []*fleetv1.Robot
	for i := range robots.Items {
		rob := &robots.Items[i]
		if rolledBackSet[rob.Name] {
			// Already auto-reverted by this rollout — excluded from every bucket so it
			// is neither re-updated nor counted as pending/failed.
			rolledBack = append(rolledBack, rob)
			continue
		}
		if excludedSet[rob.Name] {
			// An operator resumed past this robot. Same treatment as a rolled-back one:
			// out of every bucket, so it neither re-pauses the rollout (it is no longer
			// in `failed`) nor is re-dispatched to (it is no longer in `eligible`) nor
			// holds the rollout short of terminal (it counts as settled below).
			excluded = append(excluded, rob)
			continue
		}
		switch classifyModel(rob, rollout.Spec.ModelName, rollout.Spec.NewVersion) {
		case modelDone:
			done = append(done, rob)
			// Succeeded only for a robot this rollout actually updated: one already running
			// the target version before the rollout started installed nothing.
			if batchEntryFor(rollout.Status.CurrentBatch, rob.Name) != nil {
				r.sealModelEvent(ctx, rollout, rob.Name, audit.EventModelUpdateSucceeded, "update",
					audit.OutcomeAllowed, map[string]string{
						"model_name":  rollout.Spec.ModelName,
						"new_version": rollout.Spec.NewVersion,
					})
			}
		case modelUpdating:
			updating = append(updating, rob)
		case modelFailed:
			failed = append(failed, rob)
			// A failed model STAYS failed across reconciles, so the edge is the first
			// appearance: a robot not already recorded in status.failedRobots.
			if !wasAlreadyFailed(rollout.Status.FailedRobots, rob.Name) {
				reason := modelFailureReason(rob, rollout.Spec.ModelName)
				r.sealModelEvent(ctx, rollout, rob.Name, audit.EventModelUpdateFailed, "update",
					audit.OutcomeError, map[string]string{
						"model_name": rollout.Spec.ModelName,
						"reason":     reason,
					})
			}
		case modelNewer:
			newer = append(newer, rob)
		default:
			if eligibleForModelUpdate(rob, rollout) && withinRolloutWindow(rob, windowOnly, now) {
				eligible = append(eligible, rob)
			}
		}
	}

	total := len(robots.Items)
	deferred := false

	// ── Auto rollback (§6.7) ─────────────────────────────────────────────────────
	// Under rollbackPolicy=Auto, a failed robot with a known previous version is
	// reverted (the adapter reactivates the retained model) instead of pausing the
	// rollout. A robot we cannot safely revert — unknown previous version, or an
	// undeliverable rollback push — stays failed (fail-safe, surfaced), matching
	// Manual. Manual (the default) reverts nothing and pauses on failure.
	if rollout.Spec.RollbackPolicy == fleetv1.ModelRollbackAuto {
		var stillFailed []*fleetv1.Robot
		sort.Slice(failed, func(i, j int) bool { return failed[i].Name < failed[j].Name })
		for _, rob := range failed {
			prev := rollbackVersions[rob.Name]
			if prev == "" {
				stillFailed = append(stillFailed, rob) // nothing to revert to
				continue
			}
			reverted, err := r.rollbackRobot(ctx, rob, rollout, prev)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !reverted {
				stillFailed = append(stillFailed, rob) // push not delivered — retry
				deferred = true
				continue
			}
			rolledBackSet[rob.Name] = true
			delete(rollbackVersions, rob.Name)
			rolledBack = append(rolledBack, rob)
		}
		failed = stillFailed
	}

	paused := len(failed) > 0 && rollout.Spec.Strategy.RollingUpdateOrDefault().PauseOnError

	// ── Batch entry ────────────────────────────────────────────────────────────
	// Fill up to maxUnavailable (updaters already in the batch count against it).
	// A Paused rollout starts no new robots; in-progress updaters continue (§9.3.6).
	if !paused {
		slots := maxUnavailable(rollout.Spec.Strategy.RollingUpdateOrDefault().MaxUnavailable, total) - len(updating)
		sort.Slice(eligible, func(i, j int) bool { return eligible[i].Name < eligible[j].Name })
		for _, rob := range eligible {
			if slots <= 0 {
				break
			}
			entered, prev, err := r.enterBatch(ctx, rob, rollout)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !entered {
				// Undeliverable model_update push: the robot did not enter the batch
				// and must NOT consume a slot; retry it on the next reconcile.
				deferred = true
				continue
			}
			// Capture the version the robot was on — the Auto rollback revert target
			// if this update later fails.
			if prev != "" {
				rollbackVersions[rob.Name] = prev
			}
			updating = append(updating, rob)
			slots--
		}
	}

	// ── Projection: grant capabilities for robots that finished successfully ────
	for _, rob := range done {
		if err := r.projectGrant(ctx, rob, rollout); err != nil {
			return ctrl.Result{}, err
		}
	}

	// ── Rollout status ─────────────────────────────────────────────────────────
	newStatus := computeRolloutStatus(rollout.Spec.ModelName, total,
		done, updating, failed, rolledBack, newer, excluded, paused, rollbackVersions, rollout.Status.CurrentBatch)

	// Announce the refusal once per reconcile rather than per robot: on a large fleet a
	// per-robot event would bury the signal it exists to give. A silent skip is what makes
	// "the rollout did nothing" indistinguishable from "there was nothing to do".
	if len(newer) > 0 && !equality.Semantic.DeepEqual(rollout.Status.RobotsIneligible, newStatus.RobotsIneligible) {
		names := make([]string, 0, len(newer))
		for _, rob := range newer {
			names = append(names, rob.Name)
		}
		sort.Strings(names)
		r.event(rollout, corev1.EventTypeWarning, "ModelDowngradeRefused", fmt.Sprintf(
			"%d robot(s) already run a newer %s than %s and were not updated: %s",
			len(newer), rollout.Spec.ModelName, rollout.Spec.NewVersion, strings.Join(names, ", ")))
	}
	// PausedAt anchors the halt for an operator and for the audit entry. Stamped on the
	// EDGE into Paused so it records when the rollout stopped, not when it was last
	// reconciled while stopped — and so it rides the material-change patch below.
	if newStatus.Phase == fleetv1.RolloutPhasePaused {
		if rollout.Status.PausedAt != nil {
			newStatus.PausedAt = rollout.Status.PausedAt
		} else {
			pausedAt := metav1.Now()
			newStatus.PausedAt = &pausedAt
		}
	}
	pauseEdge := newStatus.Phase == fleetv1.RolloutPhasePaused && rollout.Status.Phase != fleetv1.RolloutPhasePaused
	if !equality.Semantic.DeepEqual(rollout.Status, newStatus) {
		orig := rollout.DeepCopy()
		rollout.Status = newStatus
		if err := r.Status().Patch(ctx, rollout, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching rollout status: %w", err)
		}
		logger.V(1).Info("model rollout progress", "phase", newStatus.Phase,
			"updated", newStatus.RobotsUpdated, "total", newStatus.RobotsTotal)
		// §9.5.4 requires MODEL_ROLLOUT_PAUSED. Sealed on the edge, so a rollout that sits
		// Paused across many reconciles seals one entry rather than one per pass.
		if pauseEdge {
			failedNames := make([]string, 0, len(newStatus.FailedRobots))
			for _, f := range newStatus.FailedRobots {
				failedNames = append(failedNames, f.RobotName)
			}
			r.sealModelEvent(ctx, rollout, "", audit.EventModelRolloutPaused, "pause", audit.OutcomeError,
				map[string]string{
					"model_name":    rollout.Spec.ModelName,
					"failed_robots": strings.Join(failedNames, ","),
				})
		}
	}

	// While updaters are outstanding, requeue to pick up the adapter-reported
	// Active transition (telemetry-driven). Also requeue when a robot's model_update
	// push was deferred (adapter not yet reachable) so it is retried.
	if newStatus.Phase == fleetv1.RolloutPhaseInProgress || deferred {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// enterBatch tries to bring a robot into the update batch (§9.3.6). When a Pusher
// is configured it PUSHES the model_update Command FIRST and only enters the robot
// (marking the model Updating, which suspends model-driven capabilities) once the
// adapter ACKNOWLEDGES — so an undeliverable update never suspends capabilities.
// A push that is unreachable/timed-out/un-acked returns entered=false (the robot
// stays Pending and is retried), NOT an error. With no Pusher (ControlStream
// disabled) it falls back to observe-only: mark Updating and await the reported
// Active. Returns (entered, err); err is only for a real control-plane failure.
// enterBatch returns (entered, previousVersion, err). previousVersion is the model
// version the robot was running at entry — captured BEFORE the model is marked
// Updating, since it is the Auto rollback revert target if this update later fails.
func (r *ModelRolloutReconciler) enterBatch(ctx context.Context, rob *fleetv1.Robot, rollout *fleetv1.ModelRollout) (bool, string, error) {
	var prevVersion string
	if e := modelEntry(rob, rollout.Spec.ModelName); e != nil {
		prevVersion = e.RunningVersion
	}
	if r.Pusher != nil {
		outcome, err := r.Pusher.PushModelUpdate(ctx, rollout.Namespace, rob.Name, modelUpdatePayload(rob, rollout))
		if err != nil {
			r.event(rollout, corev1.EventTypeWarning, "ModelRolloutPushDeferred",
				fmt.Sprintf("robot %s: model_update not delivered (%v); will retry", rob.Name, err))
			return false, "", nil
		}
		if !outcome.Acknowledged {
			r.event(rollout, corev1.EventTypeWarning, "ModelRolloutPushDeclined",
				fmt.Sprintf("robot %s: adapter did not acknowledge model_update: %s", rob.Name, outcome.Message))
			// Only a decline the adapter attributes to signature verification is sealed as
			// MODEL_SIGNATURE_FAILED. §9.2.8 mandates that exact reason for that case, so
			// matching it follows the contract rather than guessing; a decline for any
			// other cause is a delivery problem, already surfaced by the event above, and
			// recording it as a signature failure would put a false rejection in the chain.
			if isSignatureFailure(outcome.Message) {
				r.sealModelEvent(ctx, rollout, rob.Name, audit.EventModelSignatureFailed, "verify",
					audit.OutcomeDenied, map[string]string{
						"model_name":      rollout.Spec.ModelName,
						"artifact_digest": rollout.Spec.ModelChecksum,
						"reason":          outcome.Message,
					})
			}
			return false, "", nil
		}
		// Acknowledged means the adapter verified the artifact before installing it
		// (§9.2.8). verified_signer is the adapter's attestation of which trust root it
		// checked against — the robot-side counterpart of the control plane's own
		// FIRMWARE_SIGNATURE_VERIFIED, and the only evidence available for a model, whose
		// bytes this process never sees.
		r.sealModelEvent(ctx, rollout, rob.Name, audit.EventModelSignatureVerified, "verify",
			audit.OutcomeAllowed, map[string]string{
				"model_name":      rollout.Spec.ModelName,
				"artifact_digest": rollout.Spec.ModelChecksum,
				"verified_signer": outcome.VerifiedSigner,
			})
	}

	orig := rob.DeepCopy()
	upsertModelStatus(rob, rollout.Spec.ModelName, fleetv1.ModelStatusUpdating)
	if err := r.Status().Patch(ctx, rob, client.MergeFrom(orig)); err != nil {
		return false, "", fmt.Errorf("marking model updating on %s: %w", rob.Name, err)
	}
	r.event(rollout, corev1.EventTypeNormal, "ModelRolloutBatchStarted",
		fmt.Sprintf("robot %s entered ModelRollout batch for %s@%s", rob.Name, rollout.Spec.ModelName, rollout.Spec.NewVersion))
	// After the mark, not before: the update has begun only once the model is Updating,
	// and that status is what keeps the robot out of a later batch — so this cannot repeat.
	r.sealModelEvent(ctx, rollout, rob.Name, audit.EventModelUpdateStarted, "update",
		audit.OutcomeAllowed, map[string]string{
			"model_name":  rollout.Spec.ModelName,
			"old_version": prevVersion,
			"new_version": rollout.Spec.NewVersion,
		})
	return true, prevVersion, nil
}

// rollbackRobot performs an Auto rollback (§6.7) for a robot whose update failed:
// it pushes a rollback model_update (the adapter reactivates the retained
// prevVersion — no download) and, on acknowledgement, marks the model Updating
// (reverting; capabilities stay suspended until the adapter reports the previous
// model Active). Push-then-mark: an undeliverable/declined rollback returns
// (false, nil) so the robot stays failed and is retried — it never silently drops.
func (r *ModelRolloutReconciler) rollbackRobot(ctx context.Context, rob *fleetv1.Robot, rollout *fleetv1.ModelRollout, prevVersion string) (bool, error) {
	failedVersion := rollout.Spec.NewVersion
	if r.Pusher != nil {
		outcome, err := r.Pusher.PushModelUpdate(ctx, rollout.Namespace, rob.Name, command.ModelUpdate{
			ModelName:  rollout.Spec.ModelName,
			OldVersion: failedVersion,
			NewVersion: prevVersion,
			Rollback:   true,
		})
		if err != nil {
			r.event(rollout, corev1.EventTypeWarning, "ModelRollbackDeferred",
				fmt.Sprintf("robot %s: rollback to %s not delivered (%v); will retry", rob.Name, prevVersion, err))
			return false, nil
		}
		if !outcome.Acknowledged {
			r.event(rollout, corev1.EventTypeWarning, "ModelRollbackDeclined",
				fmt.Sprintf("robot %s: adapter did not acknowledge rollback: %s", rob.Name, outcome.Message))
			return false, nil
		}
	}

	orig := rob.DeepCopy()
	upsertModelStatus(rob, rollout.Spec.ModelName, fleetv1.ModelStatusUpdating)
	if err := r.Status().Patch(ctx, rob, client.MergeFrom(orig)); err != nil {
		return false, fmt.Errorf("marking model reverting on %s: %w", rob.Name, err)
	}
	r.event(rollout, corev1.EventTypeWarning, "ModelRolledBack",
		fmt.Sprintf("robot %s: model %s update to %s failed — auto-reverting to %s (§6.7); fleet now version-fragmented",
			rob.Name, rollout.Spec.ModelName, failedVersion, prevVersion))
	return true, nil
}

// copyStringMap returns a shallow copy so the persisted status map is not mutated
// in place while the reconcile accumulates rollback bookkeeping.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// modelUpdatePayload builds the model_update Command payload from the rollout spec
// and the robot's currently-installed version. modelChecksum and modelSignatureRef
// travel to the adapter so the robot re-verifies the downloaded bytes and the
// detached signature against the checksum before install (§9.2.8, ADR-0020).
func modelUpdatePayload(rob *fleetv1.Robot, rollout *fleetv1.ModelRollout) command.ModelUpdate {
	var oldVersion string
	if e := modelEntry(rob, rollout.Spec.ModelName); e != nil {
		oldVersion = e.RunningVersion
	}
	return command.ModelUpdate{
		ModelName:         rollout.Spec.ModelName,
		OldVersion:        oldVersion,
		NewVersion:        rollout.Spec.NewVersion,
		ModelURI:          rollout.Spec.ModelURI,
		ModelChecksum:     rollout.Spec.ModelChecksum,
		ModelSignatureRef: rollout.Spec.ModelSignatureRef,
	}
}

// projectGrant upserts the model's granted capabilities into
// status.modelGrantedCapabilities[] (grants − revokes, §9.3.6). Idempotent.
func (r *ModelRolloutReconciler) projectGrant(ctx context.Context, rob *fleetv1.Robot, rollout *fleetv1.ModelRollout) error {
	grants := subtractCaps(rollout.Spec.GrantsCapabilities, rollout.Spec.RevokesCapabilities)
	if modelGrantMatches(rob, rollout.Spec.ModelName, rollout.Name, grants) {
		return nil // already projected; no-op write avoided
	}
	orig := rob.DeepCopy()
	upsertModelGrant(rob, rollout.Spec.ModelName, rollout.Name, grants)
	if err := r.Status().Patch(ctx, rob, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("projecting model grant on %s: %w", rob.Name, err)
	}
	r.event(rollout, corev1.EventTypeNormal, "CapabilitiesRestored",
		fmt.Sprintf("robot %s: model %s@%s active; granted %v", rob.Name, rollout.Spec.ModelName, rollout.Spec.NewVersion, grants))
	return nil
}

func (r *ModelRolloutReconciler) event(obj client.Object, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, msg)
	}
}

// ── Pure helpers ──────────────────────────────────────────────────────────────

// classifyModel reports a robot's state for the rolled-out model.
func classifyModel(rob *fleetv1.Robot, modelName, newVersion string) modelState {
	e := modelEntry(rob, modelName)
	if e == nil {
		return modelPending
	}
	switch e.Status {
	case fleetv1.ModelStatusActive:
		if e.RunningVersion == newVersion {
			return modelDone
		}
		// "Not equal" is not "older". Before this check, a rollout naming an older version
		// silently downgraded every robot already past it — the controller compared for
		// equality only, so a newer robot looked exactly like a stale one.
		//
		// Refuse only on PROOF: both versions must parse and the installed one must be
		// strictly greater. An unparseable RunningVersion (it is reported by the adapter,
		// so it is whatever the vendor sends) is NOT evidence of a downgrade, and treating
		// it as one would silently empty the batch — the failure mode where a rollout that
		// did nothing reads exactly like a rollout with nothing to do.
		if newer, ok := versionIsNewer(e.RunningVersion, newVersion); ok && newer {
			return modelNewer
		}
		return modelPending // active but on an older/unorderable version → still needs updating
	case fleetv1.ModelStatusUpdating:
		return modelUpdating
	case fleetv1.ModelStatusFailed:
		return modelFailed
	default:
		return modelPending
	}
}

// sealModelEvent appends one §9.6.5.1 entry about a model rollout. Best-effort and
// nil-safe: the push, the mark and the classification have all already happened, and an
// audit sink must never be able to hold up a model update or a rollback.
// applyResume consumes a pending swarmada.io/rollout-resume annotation (ADR-0041).
//
// It moves the robots this rollout currently records as failed into status.excludedRobots and
// clears status.pausedAt. Those robots are then out of every progress bucket, so `paused` —
// which is derived per reconcile from len(failed), not latched — cannot immediately re-latch
// off the same robots, and the rollout can reach a terminal phase and become deletable.
//
// Returns done=true when it wrote status; the caller ends the reconcile and the write requeues.
// Idempotent: a request already applied writes nothing (RA-1). A rollout that is NOT paused
// still records the request as processed, so a stale annotation cannot silently resume a
// FUTURE pause.
func (r *ModelRolloutReconciler) applyResume(ctx context.Context, rollout *fleetv1.ModelRollout) (bool, error) {
	req, pending := pendingResume(rollout.Annotations)
	if !pending {
		return false, nil
	}

	resumed := false
	if rollout.Status.Phase == fleetv1.RolloutPhasePaused {
		newlyExcluded := make([]string, 0, len(rollout.Status.FailedRobots))
		for _, f := range rollout.Status.FailedRobots {
			newlyExcluded = append(newlyExcluded, f.RobotName)
		}

		base := rollout.DeepCopy()
		rollout.Status.ExcludedRobots = mergeExcluded(rollout.Status.ExcludedRobots, newlyExcluded)
		// The failure list is what re-derives `paused`; leaving it would re-pause on the very
		// next reconcile and make the resume look like it did nothing — the same trap
		// policy-reset avoids by zeroing ConsecutiveRejections.
		rollout.Status.FailedRobots = nil
		rollout.Status.PausedAt = nil
		if err := r.Status().Patch(ctx, rollout, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("resuming model rollout: %w", err)
		}
		r.sealModelEventAs(ctx, rollout,
			estopActor(rollout.Annotations, "modelrollout-controller"),
			"", audit.EventRolloutResumed, "resume", audit.OutcomeAllowed,
			map[string]string{
				"reason":          req,
				"excluded_robots": strings.Join(newlyExcluded, ","),
			})
		resumed = true
	}

	if err := markResumeProcessed(ctx, r.Client, rollout, req); err != nil {
		return false, err
	}
	return resumed, nil
}

func (r *ModelRolloutReconciler) sealModelEvent(ctx context.Context, rollout *fleetv1.ModelRollout,
	robotName, eventType, action string, outcome audit.Outcome, detail map[string]string) {
	r.sealModelEventAs(ctx, rollout,
		audit.Actor{Type: audit.ActorServiceAccount, Identity: "modelrollout-controller"},
		robotName, eventType, action, outcome, detail)
}

// sealModelEventAs is sealModelEvent with an explicit actor — see the FirmwareRollout twin
// for why only ROLLOUT_RESUMED uses it (ADR-0046).
func (r *ModelRolloutReconciler) sealModelEventAs(ctx context.Context, rollout *fleetv1.ModelRollout,
	actor audit.Actor, robotName, eventType, action string, outcome audit.Outcome, detail map[string]string) {
	if r.Audit == nil {
		return
	}
	if detail == nil {
		detail = map[string]string{}
	}
	// Rollout-scope events (pause, resume) concern the whole rollout, not one robot;
	// an empty "robot" key would claim a subject that does not exist.
	if robotName != "" {
		detail["robot"] = robotName
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: eventType,
		Namespace: rollout.Namespace,
		Actor:     actor,
		Resource:  audit.Resource{Kind: "ModelRollout", Namespace: rollout.Namespace, Name: rollout.Name},
		Action:    action,
		Outcome:   outcome,
		Detail:    detail,
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording audit entry", "event", eventType, "rollout", rollout.Name)
	}
}

// isSignatureFailure reports whether an adapter's decline names signature verification as
// the cause. §9.2.8 requires exactly this reason for a fail-closed signature refusal
// ("SignatureVerificationFailed"), so matching it reads the contract rather than guessing;
// the check is case-insensitive and substring-based because the reason is carried in a
// free-text message field alongside whatever context the adapter adds.
func isSignatureFailure(message string) bool {
	return strings.Contains(strings.ToLower(message), "signature")
}

// wasAlreadyFailed reports whether a robot is already recorded in the rollout's failed set.
// The failure classification persists across reconciles, so this is what turns it into an
// edge — without it the chain would gain one MODEL_UPDATE_FAILED per reconcile per robot.
func wasAlreadyFailed(failed []fleetv1.RolloutRobotResult, robotName string) bool {
	for i := range failed {
		if failed[i].RobotName == robotName {
			return true
		}
	}
	return false
}

// modelFailureReason pulls the adapter's reported reason for a failed model, falling back
// to a plain statement of the fact. An empty reason is recorded as such rather than left
// blank: "the adapter reported no reason" is itself worth knowing in a review.
func modelFailureReason(rob *fleetv1.Robot, modelName string) string {
	if e := modelEntry(rob, modelName); e != nil && e.FailureReason != "" {
		return e.FailureReason
	}
	return "adapter reported the model Failed with no reason"
}

// versionIsNewer reports whether `have` is strictly greater than `want`, and whether the
// comparison could be made at all. Both must be plain major.minor.patch — the same shape
// ModelRollout.spec.newVersion is constrained to at admission, and the same exclusion of
// prerelease/build metadata the contract-version parser makes (RFC-0001 §9.2.2): ordering
// those would require this controller to define their precedence, and it does not.
//
// ok=false means "cannot be ordered", which callers must treat as "unknown", never as
// "not newer" — the distinction between a negative answer and no answer is the whole point.
func versionIsNewer(have, want string) (newer, ok bool) {
	h, hok := parseSemver(have)
	w, wok := parseSemver(want)
	if !hok || !wok {
		return false, false
	}
	for i := range h {
		if h[i] != w[i] {
			return h[i] > w[i], true
		}
	}
	return false, true // equal
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func modelEntry(rob *fleetv1.Robot, modelName string) *fleetv1.InstalledModelStatusEntry {
	for i := range rob.Status.InstalledModels {
		if rob.Status.InstalledModels[i].Name == modelName {
			return &rob.Status.InstalledModels[i]
		}
	}
	return nil
}

// eligibleForModelUpdate checks the §9.3.6 safety constraints before batch entry.
func eligibleForModelUpdate(rob *fleetv1.Robot, rollout *fleetv1.ModelRollout) bool {
	sc := rollout.Spec.SafetyConstraints
	if sc.RequireIdleState && rob.Status.Phase != fleetv1.RobotPhaseIdle {
		return false
	}
	// Battery must be known and at/above the floor — an unknown battery is not a
	// safe basis for starting an update.
	if rob.Status.BatteryPercent == nil || *rob.Status.BatteryPercent < sc.MinBatteryPct {
		return false
	}
	for _, reqType := range rollout.Spec.RequiredHardware {
		if !hasHealthyHardwareType(rob, reqType) {
			return false
		}
	}
	return true
}

// hasHealthyHardwareType reports whether the robot has at least one hardware
// component of the given type reporting Healthy.
func hasHealthyHardwareType(rob *fleetv1.Robot, t fleetv1.HardwareComponentType) bool {
	statusByName := make(map[string]fleetv1.HardwareStatus, len(rob.Status.Hardware))
	for _, h := range rob.Status.Hardware {
		statusByName[h.Name] = h.Status
	}
	for i := range rob.Spec.Hardware {
		hw := &rob.Spec.Hardware[i]
		if hw.Type == t && statusByName[hw.Name] == fleetv1.HardwareHealthy {
			return true
		}
	}
	return false
}

func upsertModelStatus(rob *fleetv1.Robot, modelName string, status fleetv1.ModelStatus) {
	for i := range rob.Status.InstalledModels {
		if rob.Status.InstalledModels[i].Name == modelName {
			rob.Status.InstalledModels[i].Status = status
			return
		}
	}
	rob.Status.InstalledModels = append(rob.Status.InstalledModels,
		fleetv1.InstalledModelStatusEntry{Name: modelName, Status: status})
}

func upsertModelGrant(rob *fleetv1.Robot, modelName, grantedBy string, caps []string) {
	for i := range rob.Status.ModelGrantedCapabilities {
		if rob.Status.ModelGrantedCapabilities[i].ModelName == modelName {
			rob.Status.ModelGrantedCapabilities[i].GrantedBy = grantedBy
			rob.Status.ModelGrantedCapabilities[i].Capabilities = caps
			return
		}
	}
	rob.Status.ModelGrantedCapabilities = append(rob.Status.ModelGrantedCapabilities,
		fleetv1.ModelGrantedCapabilityEntry{ModelName: modelName, GrantedBy: grantedBy, Capabilities: caps})
}

func modelGrantMatches(rob *fleetv1.Robot, modelName, grantedBy string, caps []string) bool {
	for i := range rob.Status.ModelGrantedCapabilities {
		e := &rob.Status.ModelGrantedCapabilities[i]
		if e.ModelName == modelName {
			return e.GrantedBy == grantedBy && equalStringSets(e.Capabilities, caps)
		}
	}
	return false
}

// subtractCaps returns grants with any name in revokes removed, sorted.
func subtractCaps(grants, revokes []string) []string {
	rev := make(map[string]bool, len(revokes))
	for _, r := range revokes {
		rev[r] = true
	}
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		if !rev[g] {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// maxUnavailable resolves the strategy value ("10%" or "5") to an absolute count,
// bounded below by 1.
func maxUnavailable(spec string, total int) int {
	if spec == "" {
		spec = "10%"
	}
	if strings.HasSuffix(spec, "%") {
		pct, err := strconv.Atoi(strings.TrimSuffix(spec, "%"))
		if err != nil || pct <= 0 {
			return 1
		}
		n := total * pct / 100
		if n < 1 {
			return 1
		}
		return n
	}
	n, err := strconv.Atoi(spec)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func computeRolloutStatus(modelName string, total int, done, updating, failed, rolledBack, newer, excluded []*fleetv1.Robot, paused bool, rollbackVersions map[string]string, prior []fleetv1.RolloutBatchRobot) fleetv1.ModelRolloutStatus {
	batch := buildRolloutBatch(updating, prior, modelInitialPhase, func(*fleetv1.Robot) string { return "" }, true)

	failedResults := make([]fleetv1.RolloutRobotResult, 0, len(failed))
	for _, r := range failed {
		reason := ""
		if e := modelEntry(r, modelName); e != nil {
			reason = e.FailureReason
		}
		failedResults = append(failedResults, fleetv1.RolloutRobotResult{RobotName: r.Name, Namespace: r.Namespace, Reason: reason})
	}
	sort.Slice(failedResults, func(i, j int) bool { return failedResults[i].RobotName < failedResults[j].RobotName })

	rolledBackNames := make([]string, 0, len(rolledBack))
	for _, r := range rolledBack {
		rolledBackNames = append(rolledBackNames, r.Name)
	}
	sort.Strings(rolledBackNames)

	// Persist revert targets only for robots that could still need one — those
	// updating now or currently failed. done/rolled-back/pending robots need none,
	// keeping the status map bounded and meaningful.
	keep := make(map[string]bool, len(updating)+len(failed))
	for _, r := range updating {
		keep[r.Name] = true
	}
	for _, r := range failed {
		keep[r.Name] = true
	}
	prunedVersions := map[string]string{}
	for name, ver := range rollbackVersions {
		if keep[name] {
			prunedVersions[name] = ver
		}
	}
	if len(prunedVersions) == 0 {
		prunedVersions = nil
	}

	st := fleetv1.ModelRolloutStatus{
		//nolint:gosec // small fleet counts
		RobotsTotal: int32(total),
		//nolint:gosec // small fleet counts
		RobotsUpdated: int32(len(done)),
		//nolint:gosec // small fleet counts
		RobotsFailed: int32(len(failed)),
		//nolint:gosec // small fleet counts
		RobotsRolledBack: int32(len(rolledBack)),
		//nolint:gosec // small fleet counts
		// A downgrade-refused robot is not pending: it will never enter a batch, and
		// counting it as pending would leave the rollout reporting work outstanding that
		// it has already decided not to do — a progress bar that can never reach the end.
		RobotsPending: int32(total - len(done) - len(failed) - len(rolledBack) - len(newer)),
		// Reported as ineligible, alongside robots excluded for missing hardware: both are
		// selector-matched robots this rollout will not touch. Counting them somewhere is
		// what stops "did nothing" from being indistinguishable from "had nothing to do".
		//nolint:gosec // small fleet counts
		RobotsIneligible: int32(len(newer)),
		//nolint:gosec // small fleet counts
		CapabilitiesSuspendedOn: int32(len(updating)),
		CurrentBatch:            batch,
		FailedRobots:            failedResults,
		RollbackVersions:        prunedVersions,
	}
	if len(rolledBackNames) > 0 {
		st.RolledBackRobots = rolledBackNames
	}
	if len(excluded) > 0 {
		excludedNames := make([]string, 0, len(excluded))
		for _, r := range excluded {
			excludedNames = append(excludedNames, r.Name)
		}
		sort.Strings(excludedNames)
		st.ExcludedRobots = excludedNames
	}
	switch {
	case len(done)+len(rolledBack)+len(newer)+len(excluded) == total && total > 0:
		// Every robot is on newVersion, was reverted, or was excluded by an operator
		// resume. Succeeded, but a non-zero RobotsRolledBack / ExcludedRobots surfaces the
		// version fragmentation (§6.7, ADR-0041). Counting excluded robots as settled is
		// what lets a resumed rollout reach a terminal phase at all — without it the
		// record could never be deleted, which is the other half of the wedge resume fixes.
		st.Phase = fleetv1.RolloutPhaseSucceeded
	case paused:
		st.Phase = fleetv1.RolloutPhasePaused
	case len(updating) > 0 || len(done) > 0 || len(failed) > 0 || len(rolledBack) > 0:
		st.Phase = fleetv1.RolloutPhaseInProgress
	default:
		st.Phase = fleetv1.RolloutPhasePending
	}
	return st
}

// SetupWithManager registers the ModelRollout controller and re-reconciles a
// namespace's rollouts when any Robot in it changes (its model progress may have
// advanced).
func (r *ModelRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	robotToRollouts := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var rollouts fleetv1.ModelRolloutList
		if err := r.List(ctx, &rollouts, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(rollouts.Items))
		for i := range rollouts.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Name: rollouts.Items[i].Name, Namespace: rollouts.Items[i].Namespace,
			}})
		}
		return reqs
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.ModelRollout{}).
		Watches(&fleetv1.Robot{}, robotToRollouts).
		Complete(r)
}
