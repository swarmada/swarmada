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
	"k8s.io/client-go/util/retry"
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
// (RFC-0001 #control-plane-ota-model-update-manager). It selects target robots by safety constraints, marks the
// model Updating on batch entry (which suspends the model-driven capabilities the
// Capability Controller derives — the effective model-update-manager suspension without writing
// status.capabilities directly), and projects the granted capabilities into
// Robot.status.modelGrantedCapabilities[] once a robot reports the model Active at
// the new version. It never writes Robot.spec.
//
// The model_update Command push, the adapter-side artifact signature/checksum
// verification (RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist), and the Active/Failed report via telemetry ride the
// ControlStream wire (the push through Pusher, below); this drives the
// control-plane state machine over the observable Robot status.
type ModelRolloutReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Audit seals the RFC-0001 #safety-required-events model-update and artifact-verification entries into the
	// tamper-evident chain. Nil disables recording; the rollout is unaffected.
	Audit audit.Recorder
	// Pusher pushes the model_update Command to a robot's adapter over ControlStream
	// (RFC-0001 #control-plane-ota-model-update-manager). When set, batch entry is gated on the adapter acknowledging the
	// update, so capabilities are never suspended for an update that cannot be
	// delivered. Nil (ControlStream disabled) falls back to observe-only: the model
	// is marked Updating and the controller awaits the adapter-reported Active.
	Pusher command.ModelUpdatePusher
	// APIReader reads the API server directly, bypassing the manager's cache. The rollout
	// is re-read through it before a step that must not repeat — a model_update push, and
	// the audit entry for the pause — because a Robot event can queue a reconcile that
	// reads a cached rollout from before this controller's last status write, and would
	// push an artifact already refused or seal the same pause again. Nil falls back to
	// Client.
	APIReader client.Reader
	// Acks records each acknowledged model_update for the UpdateProgress ingestor, which
	// shares it, so a failure the robot reports after acknowledging is recorded even before
	// the batch is written (ModelAckLedger). Nil records nothing.
	Acks *ModelAckLedger

	// notes keeps the ModelRollbackWithheld Event to one per held robot.
	notes heldNotes
}

// +kubebuilder:rbac:groups=swarmada.io,resources=modelrollouts,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=modelrollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch

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

	// An EdgeNode-targeted rollout is SPECIFIED but NOT IMPLEMENTED at v0.4: no
	// controller resolves targetSelector against EdgeNodes and no ModelUpdate is
	// dispatched over an edge node's uplink session. Refuse it explicitly rather
	// than fall through — the code below resolves the selector against ROBOTS, so
	// falling through would push an edge model onto whatever robots happen to carry
	// the node labels. Silence is the failure mode worth spending an event on.
	if rollout.Spec.TargetKind == fleetv1.RolloutTargetEdgeNode {
		if r.Recorder != nil {
			r.Recorder.Eventf(rollout, corev1.EventTypeWarning, "TargetKindNotImplemented",
				"spec.targetKind: EdgeNode is specified but not implemented at v0.4; no node is updated by this rollout")
		}
		logger.Info("skipping EdgeNode-targeted ModelRollout: not implemented at v0.4")
		return ctrl.Result{}, nil
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

	holds, err := readRolloutHolds(ctx, r.Client, req.Namespace)
	if err != nil {
		return ctrl.Result{}, err
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
	// Robots whose adapter refused this rollout's artifact on its signature. The refusal
	// leaves no trace on the Robot (nothing was installed), so status.failedRobots is where
	// it is kept, and it is what stops the refused artifact from being pushed again.
	refused := signatureRefusals(rollout.Status.FailedRobots)

	var done, updating, failed, eligible, rolledBack, reverting, revertFailed, newer, excluded []*fleetv1.Robot
	for i := range robots.Items {
		rob := &robots.Items[i]
		if rolledBackSet[rob.Name] {
			// This rollout has pushed a rollback to this robot, so it is never re-updated —
			// a fixed model needs a fresh rollout. Whether it has SETTLED is a separate
			// question, and answering it with the acknowledgement was the defect: an ack is
			// a promise to revert, and only the robot reporting the previous version Active
			// is evidence that it did. Until then the model entry is still Updating and the
			// capabilities it grants are still suspended, so counting it settled let the
			// rollout report Succeeded over robots that had lost their capabilities.
			//
			// An empty revert target means the revert has already been observed (the target
			// is dropped when it lands) or that the entry predates this tracking, so the
			// robot is taken as settled rather than stranded reverting for ever.
			switch target := rollbackVersions[rob.Name]; {
			case target == "" || modelActiveAt(rob, rollout.Spec.ModelName, target):
				rolledBack = append(rolledBack, rob)
				delete(rollbackVersions, rob.Name)
			case classifyModel(rob, rollout.Spec.ModelName, target) == modelFailed:
				// The revert itself failed. Counted failed, not reverting: it is the one
				// classification that reaches an operator (or terminates the rollout under
				// pauseOnError: false) instead of waiting for a report that is not coming.
				// Merged into `failed` AFTER the rollback pass below, so this robot is never
				// pushed a second rollback.
				revertFailed = append(revertFailed, rob)
			default:
				reverting = append(reverting, rob)
			}
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
			if _, ok := refused[rob.Name]; ok {
				failed = append(failed, rob)
				continue
			}
			if holds.why(rob) == "" && eligibleForModelUpdate(rob, rollout) && withinRolloutWindow(rob, windowOnly, now) {
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
			// A rollback is a model push like any other: a robot a stop holds is not pushed
			// one. It stays failed and is retried, as an undeliverable rollback push is.
			held := holds.why(rob)
			if held == "" && r.Pusher != nil {
				held = heldNow(ctx, r.liveReader(), rob)
			}
			if held != "" {
				if r.notes.first(heldNoteKey(rollout, rob.Name, "rollback")) {
					r.event(rollout, corev1.EventTypeWarning, "ModelRollbackWithheld",
						fmt.Sprintf("robot %s is under an emergency stop (%s): its rollback to %s is not pushed while "+
							"the stop holds it, and is retried once nothing holds the robot", rob.Name, held, prev))
				}
				stillFailed = append(stillFailed, rob)
				deferred = true
				continue
			}
			r.notes.forget(heldNoteKey(rollout, rob.Name, "rollback"))
			reverted, err := r.rollbackRobot(ctx, rob, rollout, prev)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !reverted {
				stillFailed = append(stillFailed, rob) // push not delivered — retry
				deferred = true
				continue
			}
			// The revert target is deliberately NOT dropped here: it is what recognises the
			// revert when the robot reports it, and dropping it on the push left nothing
			// able to tell a completed revert from an outstanding one.
			rolledBackSet[rob.Name] = true
			reverting = append(reverting, rob)
		}
		failed = stillFailed
	}
	// A failed revert joins the failed set only now, past the rollback pass, so it cannot be
	// pushed a second rollback on the same reconcile.
	failed = append(failed, revertFailed...)
	sort.Slice(failed, func(i, j int) bool { return failed[i].Name < failed[j].Name })

	// A revert is an error for pauseOnError. Under rollbackPolicy Auto a robot whose update
	// failed is reverted, and the rollback pass above takes it out of `failed`, so judging the
	// halt on `failed` alone let a model that fails on every robot be pushed to every robot in
	// turn. The model failed on the reverted robot; the revert only repaired it. So a reverted
	// robot halts the rollout after the batch in which it happened, until an operator resume
	// acknowledges it by adding it to status.excludedRobots, exactly as a resume past a failed
	// robot does (ADR-0041). What a revert counts toward is unchanged: a landed revert is
	// settled toward Succeeded, so the terminal phase is the same as before.
	var unackedReverts, ackedReverts []string
	for _, group := range [][]*fleetv1.Robot{rolledBack, reverting} {
		for _, rob := range group {
			if excludedSet[rob.Name] {
				ackedReverts = append(ackedReverts, rob.Name)
			} else {
				unackedReverts = append(unackedReverts, rob.Name)
			}
		}
	}
	sort.Strings(unackedReverts)
	pausedOnError := (len(failed) > 0 || len(unackedReverts) > 0) &&
		rollout.Spec.Strategy.RollingUpdateOrDefault().PausesOnError()

	// The OPERATOR hold, composed with — never substituted for — the error halt.
	// spec.desiredState: Paused withholds dispatch of any further robot. The two
	// holds are independent and the rollout dispatches only when NEITHER is in
	// force, which is the property that makes the operator's field safe: writing
	// `Running` withdraws the operator's own hold and cannot touch a pauseOnError
	// halt, whose exit is the rollout-resume verb because that verb also decides
	// what happens to the robots that failed (ADR-0041). An operator therefore
	// cannot use a spec field to push a rollout past a failure the halt exists to
	// surface.
	//
	// In-flight updaters are NOT interrupted, and on this kind that matters more
	// than on firmware: a robot in the batch has its models marked Updating, which
	// suspends every capability those models grant, and the rollout's own status is
	// what un-suspends them. Yanking a robot out of the batch mid-update would
	// strand exactly those capabilities. A hold stops the NEXT robot.
	heldByOperator := rollout.Spec.DesiredState == fleetv1.RolloutDesiredStatePaused
	paused := pausedOnError || heldByOperator

	// current is set once this pass has confirmed, uncached, that it is not working from a
	// stale rollout (see APIReader).
	current := false

	// ── Batch entry ────────────────────────────────────────────────────────────
	// Fill up to maxUnavailable (updaters already in the batch count against it).
	// A Paused rollout starts no new robots; in-progress updaters continue (RFC-0001 #control-plane-ota-model-update-manager).
	if !paused {
		slots := maxUnavailable(rollout.Spec.Strategy.RollingUpdateOrDefault().MaxUnavailable, total) - len(updating)
		if r.Pusher != nil && slots > 0 && len(eligible) > 0 {
			if stale, err := r.staleRollout(ctx, rollout); err != nil {
				return ctrl.Result{}, err
			} else if stale {
				return ctrl.Result{Requeue: true}, nil
			}
			current = true
		}
		sort.Slice(eligible, func(i, j int) bool { return eligible[i].Name < eligible[j].Name })
		for _, rob := range eligible {
			if slots <= 0 {
				break
			}
			entered, prev, refusal, err := r.enterBatch(ctx, rob, rollout)
			if err != nil {
				return ctrl.Result{}, err
			}
			if refusal != "" {
				// Refused on its signature: the robot fails in this rollout, and under
				// pauseOnError the next robot is not pushed an artifact already refused.
				refused[rob.Name] = refusal
				failed = append(failed, rob)
				if rollout.Spec.Strategy.RollingUpdateOrDefault().PausesOnError() {
					paused = true
					break
				}
				continue
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
		done, updating, failed, rolledBack, reverting, newer, excluded, paused, rollbackVersions,
		rollout.Status.CurrentBatch)
	// Conditions start from the stored ones, so a condition whose status did not change
	// keeps its lastTransitionTime and an unchanged pass compares equal and writes nothing
	// (RA-1).
	newStatus.Conditions = rollout.Status.DeepCopy().Conditions
	for i := range newStatus.FailedRobots {
		if reason, ok := refused[newStatus.FailedRobots[i].RobotName]; ok && newStatus.FailedRobots[i].Reason == "" {
			newStatus.FailedRobots[i].Reason = reason
		}
	}
	// A reverted robot a resume acknowledged stays acknowledged. It is classified as rolled
	// back, not excluded, so computeRolloutStatus does not carry it in excludedRobots, and
	// dropping it there would re-latch the pause on the next pass.
	if len(ackedReverts) > 0 {
		newStatus.ExcludedRobots = mergeExcluded(newStatus.ExcludedRobots, ackedReverts)
	}

	// Which hold is in force — see conditionOperatorHold. `phase: Paused` alone
	// cannot answer "will this move on its own, or is it waiting for me?" now that
	// an operator hold and the pauseOnError halt both produce it.
	if heldByOperator {
		upsertCondition(&newStatus.Conditions, conditionOperatorHold, metav1.ConditionTrue,
			"OperatorPaused", "spec.desiredState is Paused: no further robot enters a batch until it is set back to Running")
	} else {
		upsertCondition(&newStatus.Conditions, conditionOperatorHold, metav1.ConditionFalse,
			"NoOperatorHold", "spec.desiredState is Running; any Paused phase is the pauseOnError halt, exited with the rollout-resume verb")
	}

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
	if pauseEdge && !current {
		if stale, err := r.staleRollout(ctx, rollout); err != nil {
			return ctrl.Result{}, err
		} else if stale {
			return ctrl.Result{Requeue: true}, nil
		}
	}
	if !equality.Semantic.DeepEqual(rollout.Status, newStatus) {
		orig := rollout.DeepCopy()
		rollout.Status = newStatus
		if err := r.Status().Patch(ctx, rollout, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching rollout status: %w", err)
		}
		logger.V(1).Info("model rollout progress", "phase", newStatus.Phase,
			"updated", newStatus.RobotsUpdated, "total", newStatus.RobotsTotal)
		// RFC-0001 #security-audit-logging requires MODEL_ROLLOUT_PAUSED. Sealed on the edge, so a rollout that sits
		// Paused across many reconciles seals one entry rather than one per pass.
		if pauseEdge {
			failedNames := make([]string, 0, len(newStatus.FailedRobots))
			for _, f := range newStatus.FailedRobots {
				failedNames = append(failedNames, f.RobotName)
			}
			detail := map[string]string{
				"model_name":    rollout.Spec.ModelName,
				"failed_robots": strings.Join(failedNames, ","),
			}
			if len(unackedReverts) > 0 {
				detail["rolled_back_robots"] = strings.Join(unackedReverts, ",")
			}
			r.sealModelEvent(ctx, rollout, "", audit.EventModelRolloutPaused, "pause", audit.OutcomeError, detail)
		}
	}

	// While updaters are outstanding, requeue to pick up the adapter-reported
	// Active transition (telemetry-driven). Also requeue when a robot's model_update
	// push was deferred (adapter not yet reachable) so it is retried.
	if newStatus.Phase == fleetv1.RolloutPhaseInProgress || deferred || len(reverting) > 0 {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// enterBatch tries to bring a robot into the update batch (RFC-0001 #control-plane-ota-model-update-manager). When a Pusher
// is configured it PUSHES the model_update Command FIRST and only enters the robot
// (marking the model Updating, which suspends model-driven capabilities) once the
// adapter ACKNOWLEDGES — so an undeliverable update never suspends capabilities.
// A push that is unreachable/timed-out/un-acked returns entered=false (the robot
// stays Pending and is retried), NOT an error. With no Pusher (ControlStream
// disabled) it falls back to observe-only: mark Updating and await the reported
// Active. Returns (entered, err); err is only for a real control-plane failure.
// enterBatch returns (entered, previousVersion, refusal, err). previousVersion is the model
// version the robot was running at entry — captured BEFORE the model is marked
// Updating, since it is the Auto rollback revert target if this update later fails.
// refusal is non-empty only when the adapter refused the artifact on its signature: the
// failure reason the caller records for the robot.
func (r *ModelRolloutReconciler) enterBatch(ctx context.Context, rob *fleetv1.Robot, rollout *fleetv1.ModelRollout) (bool, string, string, error) {
	var prevVersion string
	if e := modelEntry(rob, rollout.Spec.ModelName); e != nil {
		prevVersion = e.RunningVersion
	}
	if r.Pusher != nil {
		// The robot was found eligible on the copy this pass listed from the cache. A push
		// cannot be taken back, so a stop that reached the robot since is read first.
		if held := heldNow(ctx, r.liveReader(), rob); held != "" {
			log.FromContext(ctx).Info("model_update not pushed: the robot is under an emergency stop",
				"robot", rob.Name, "held", held)
			return false, "", "", nil
		}
		r.Acks.forget(rollout.Namespace, rob.Name, rollout.Spec.ModelName)
		outcome, err := r.Pusher.PushModelUpdate(ctx, rollout.Namespace, rob.Name, modelUpdatePayload(rob, rollout))
		if err != nil {
			r.event(rollout, corev1.EventTypeWarning, "ModelRolloutPushDeferred",
				fmt.Sprintf("robot %s: model_update not delivered (%v); will retry", rob.Name, err))
			return false, "", "", nil
		}
		if !outcome.Acknowledged {
			if isSignatureFailure(outcome.Message) {
				r.event(rollout, corev1.EventTypeWarning, "ModelRolloutPushDeclined",
					fmt.Sprintf("robot %s: adapter refused the model's signature: %s; the robot is failed in this rollout and the model is not pushed to it again",
						rob.Name, outcome.Message))
			} else {
				r.event(rollout, corev1.EventTypeWarning, "ModelRolloutPushDeclined",
					fmt.Sprintf("robot %s: adapter did not acknowledge model_update: %s", rob.Name, outcome.Message))
			}
			// Only a decline the adapter attributes to signature verification is sealed as
			// MODEL_SIGNATURE_FAILED. RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist mandates that exact reason for that case, so
			// matching it follows the contract rather than guessing; a decline for any
			// other cause is a delivery problem, already surfaced by the event above, and
			// recording it as a signature failure would put a false rejection in the chain.
			// A signature refusal is sealed once: the caller records the robot failed, and a
			// failed robot is never pushed again by this rollout.
			if isSignatureFailure(outcome.Message) {
				r.sealModelEvent(ctx, rollout, rob.Name, audit.EventModelSignatureFailed, "verify",
					audit.OutcomeDenied, map[string]string{
						"model_name":      rollout.Spec.ModelName,
						"artifact_digest": rollout.Spec.ModelChecksum,
						"reason":          outcome.Message,
					})
				return false, "", signatureRefusalPrefix + outcome.Message, nil
			}
			return false, "", "", nil
		}
		r.Acks.ack(rollout.Namespace, rob.Name, rollout.Spec.ModelName)
		// Acknowledged means the adapter verified the artifact before installing it
		// (RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist). verified_signer is the adapter's attestation of which trust root it
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

	// Re-read before marking. `rob` is the copy listed at the top of this reconcile, from
	// BEFORE the push — and the push above blocked on an acknowledgement that the reference
	// adapter sends AFTER its terminal install outcome. So by the time we get
	// here the robot may already have reported the install finished, and marking Updating
	// off the stale copy would overwrite that report and pin the robot mid-update for ever:
	// classifyModel would read Updating on every subsequent reconcile and the rollout would
	// never complete. Re-reading costs one Get on a path that has just done a network round
	// trip to a robot.
	//
	// Already settled for THIS target version — the install outcome beat the ack. Enter the
	// batch (so the success is attributed to this rollout and MODEL_UPDATE_SUCCEEDED is
	// sealed on the next pass) but write no status: there is nothing to suspend, and the
	// robot has already told us what it is running.
	settled, err := r.markModelUpdating(ctx, rob, rollout.Spec.ModelName, func(fresh *fleetv1.Robot) bool {
		st := classifyModel(fresh, rollout.Spec.ModelName, rollout.Spec.NewVersion)
		return st == modelDone || st == modelFailed
	})
	if err != nil {
		return false, "", "", fmt.Errorf("marking model updating on %s: %w", rob.Name, err)
	}
	if settled {
		return true, prevVersion, "", nil
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
	return true, prevVersion, "", nil
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
		r.Acks.forget(rollout.Namespace, rob.Name, rollout.Spec.ModelName)
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
		r.Acks.ack(rollout.Namespace, rob.Name, rollout.Spec.ModelName)
	}

	// Re-read before marking, for the SAME reason enterBatch does — and this path needs it
	// more, because it is the path that GIVES CAPABILITIES BACK. `rob` is the copy listed at
	// the top of this reconcile, from before the push; the push blocked on an acknowledgement
	// the reference adapter sends AFTER both its terminal outcome and its ADR-0033
	// CapabilitiesSnapshot. So by the time we get here the robot has usually already reported
	// the revert landed, and marking Updating off the stale copy overwrites that report.
	//
	// Nothing repairs it afterwards. The forward path is rescued by the snapshot the adapter
	// re-sends on its next install transition; a reverted robot has no next transition — the
	// revert WAS the last one — so the overwrite is permanent. Reproduced on a live
	// three-robot cluster: every robot pinned at the previous version with status Updating,
	// the model-driven capability suspended and the rollout stuck InProgress, while all
	// three were in fact running that version and had reported it.
	//
	// The revert already landed and the robot is back on the retained version. Report it
	// rolled back and write no status: there is nothing left to suspend, and marking
	// Updating here would take the capability away again after the robot restored it.
	//
	// A revert the robot has already reported FAILED is settled too, and must not be marked
	// over either: the next pass counts it failed (revertFailed). The entry was Failed before
	// this push as well, from the update that failed, so only an entry that changed since the
	// pre-push copy is the robot's report of this revert.
	var reportedFailed bool
	settled, err := r.markModelUpdating(ctx, rob, rollout.Spec.ModelName, func(fresh *fleetv1.Robot) bool {
		if modelActiveAt(fresh, rollout.Spec.ModelName, prevVersion) {
			return true
		}
		reportedFailed = revertReportedFailed(rob, fresh, rollout.Spec.ModelName)
		return reportedFailed
	})
	if err != nil {
		return false, fmt.Errorf("marking model reverting on %s: %w", rob.Name, err)
	}
	if settled && reportedFailed {
		r.event(rollout, corev1.EventTypeWarning, "ModelRollbackFailed",
			fmt.Sprintf("robot %s: model %s revert to %s was reported failed", rob.Name, rollout.Spec.ModelName, prevVersion))
		return true, nil
	}
	if settled {
		r.event(rollout, corev1.EventTypeWarning, "ModelRolledBack",
			fmt.Sprintf("robot %s: model %s update to %s failed — auto-reverted to %s (§6.7); fleet now version-fragmented",
				rob.Name, rollout.Spec.ModelName, failedVersion, prevVersion))
		return true, nil
	}
	r.event(rollout, corev1.EventTypeWarning, "ModelRolledBack",
		fmt.Sprintf("robot %s: model %s update to %s failed — auto-reverting to %s (§6.7); fleet now version-fragmented",
			rob.Name, rollout.Spec.ModelName, failedVersion, prevVersion))
	return true, nil
}

// markModelUpdating re-reads the robot after a push and marks the model Updating on it,
// unless settled reports that the robot has already said how the push ended; it returns
// true, having written nothing, in that case.
//
// The re-read covers a report that arrives before the acknowledgement. A report can also
// arrive after the acknowledgement and before the mark is written, and a plain merge patch
// computed from the re-read copy would then replace installedModels with that copy and
// erase the report; nothing reports again, so the robot would read Updating for ever. So
// the mark carries the re-read's resourceVersion: a report written in between makes the
// patch conflict, and the robot is read and judged again.
func (r *ModelRolloutReconciler) markModelUpdating(ctx context.Context, rob *fleetv1.Robot, modelName string, settled func(*fleetv1.Robot) bool) (bool, error) {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	already := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &fleetv1.Robot{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(rob), fresh); err != nil {
			return fmt.Errorf("re-reading %s after push: %w", rob.Name, err)
		}
		if settled(fresh) {
			already = true
			return nil
		}
		orig := fresh.DeepCopy()
		upsertModelStatus(fresh, modelName, fleetv1.ModelStatusUpdating)
		return r.Status().Patch(ctx, fresh, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
	})
	return already, err
}

// revertReportedFailed reports whether fresh carries a Failed entry for modelName that
// differs from the entry in before, the copy read before the rollback push: a failure the
// robot reported for the revert, not the failure the revert answers.
func revertReportedFailed(before, fresh *fleetv1.Robot, modelName string) bool {
	now := modelEntry(fresh, modelName)
	if now == nil || now.Status != fleetv1.ModelStatusFailed {
		return false
	}
	was := modelEntry(before, modelName)
	return was == nil || was.Status != now.Status || was.RunningVersion != now.RunningVersion ||
		was.FailureReason != now.FailureReason
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
// detached signature against the checksum before install (RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist, ADR-0020).
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
// status.modelGrantedCapabilities[] (grants − revokes, RFC-0001 #control-plane-ota-model-update-manager). Idempotent.
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

// sealModelEvent appends one RFC-0001 #safety-required-events entry about a model rollout. Best-effort and
// nil-safe: the push, the mark and the classification have all already happened, and an
// audit sink must never be able to hold up a model update or a rollback.
// applyResume consumes a pending swarmada.io/rollout-resume annotation (ADR-0041).
//
// It moves the robots this rollout currently records as failed, and the robots it has reverted,
// into status.excludedRobots and clears status.pausedAt. `paused` is derived per reconcile from
// the failed robots and the reverted robots not yet excluded, not latched, so it cannot
// immediately re-latch off the same robots, and the rollout can reach a terminal phase and
// become deletable.
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
		newlyExcluded := make([]string, 0, len(rollout.Status.FailedRobots)+len(rollout.Status.RolledBackRobots))
		for _, f := range rollout.Status.FailedRobots {
			newlyExcluded = append(newlyExcluded, f.RobotName)
		}
		// A reverted robot halts the rollout as a failed one does, and is acknowledged the
		// same way; it stays rolled back, so this changes only what holds the rollout.
		excluded := make(map[string]bool, len(rollout.Status.ExcludedRobots))
		for _, n := range rollout.Status.ExcludedRobots {
			excluded[n] = true
		}
		for _, n := range rollout.Status.RolledBackRobots {
			if !excluded[n] {
				newlyExcluded = append(newlyExcluded, n)
			}
		}

		base := rollout.DeepCopy()
		rollout.Status.ExcludedRobots = mergeExcluded(rollout.Status.ExcludedRobots, newlyExcluded)
		// The failure list is what re-derives `paused`; leaving it would re-pause on the very
		// next reconcile and make the resume look like it did nothing — the same trap
		// policy-reset avoids by zeroing ConsecutiveRejections.
		rollout.Status.FailedRobots = nil
		rollout.Status.PausedAt = nil
		// Written and sealed only when it changes status. A pass that applied this request
		// but could not record it as processed finds the status already resumed on its retry:
		// it writes nothing, seals nothing, and goes on to the marker (RA-1).
		if !equality.Semantic.DeepEqual(base.Status, rollout.Status) {
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
// the cause. RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist requires exactly this reason for a fail-closed signature refusal
// ("SignatureVerificationFailed"), so matching it reads the contract rather than guessing;
// the check is case-insensitive and substring-based because the reason is carried in a
// free-text message field alongside whatever context the adapter adds.
func isSignatureFailure(message string) bool {
	return strings.Contains(strings.ToLower(message), "signature")
}

// staleRollout reports whether the cached rollout this pass read is older than the stored
// one. A stale pass is abandoned and requeued: the newer rollout's own event follows.
func (r *ModelRolloutReconciler) staleRollout(ctx context.Context, rollout *fleetv1.ModelRollout) (bool, error) {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	stored := &fleetv1.ModelRollout{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(rollout), stored); err != nil {
		return false, fmt.Errorf("re-reading rollout: %w", err)
	}
	return stored.ResourceVersion != rollout.ResourceVersion, nil
}

// signatureRefusalPrefix begins the status.failedRobots reason of a robot whose adapter
// refused the rollout's artifact on its signature. It is how a later reconcile tells that
// failure, which leaves nothing on the Robot, from one the robot reported itself.
const signatureRefusalPrefix = "adapter refused the model's signature: "

// signatureRefusals returns the robots recorded failed on a signature refusal, with the
// recorded reason.
func signatureRefusals(failed []fleetv1.RolloutRobotResult) map[string]string {
	out := map[string]string{}
	for _, f := range failed {
		if strings.HasPrefix(f.Reason, signatureRefusalPrefix) {
			out[f.RobotName] = f.Reason
		}
	}
	return out
}

// wasAlreadyFailed reports whether a robot is already recorded in the rollout's failed set.
// The failure classification persists across reconciles, so this is what turns it into an
// edge — without it the chain would gain one MODEL_UPDATE_FAILED per reconcile per robot.
// On the firmware path it does more: it is what carries the failure across the pass on which
// the robot leaves status.currentBatch.
func wasAlreadyFailed(failed []fleetv1.RolloutRobotResult, robotName string) bool {
	return failedResultFor(failed, robotName) != nil
}

// failedResultFor returns a robot's recorded failure result, or nil when it has none.
func failedResultFor(failed []fleetv1.RolloutRobotResult, robotName string) *fleetv1.RolloutRobotResult {
	for i := range failed {
		if failed[i].RobotName == robotName {
			return &failed[i]
		}
	}
	return nil
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

// eligibleForModelUpdate checks the model-update-manager safety constraints before batch
// entry; a robot under an emergency stop, or Resuming, is held back (heldFromUpdate).
func eligibleForModelUpdate(rob *fleetv1.Robot, rollout *fleetv1.ModelRollout) bool {
	if heldFromUpdate(rob) {
		return false
	}
	sc := rollout.Spec.SafetyConstraints
	if sc.RequiresIdleState() && rob.Status.Phase != fleetv1.RobotPhaseIdle {
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

func computeRolloutStatus(modelName string, total int, done, updating, failed, rolledBack, reverting, newer, excluded []*fleetv1.Robot, paused bool, rollbackVersions map[string]string, prior []fleetv1.RolloutBatchRobot) fleetv1.ModelRolloutStatus {
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

	// Every robot this rollout has pushed a rollback to, settled or not. This list is what
	// seeds the exclusion set on the next reconcile, so a robot still reverting MUST appear
	// in it — dropping it would push the failed model onto the robot a second time.
	// status.robotsRolledBack, below, counts only the reverts actually observed to land.
	rolledBackNames := make([]string, 0, len(rolledBack)+len(reverting))
	for _, r := range rolledBack {
		rolledBackNames = append(rolledBackNames, r.Name)
	}
	for _, r := range reverting {
		rolledBackNames = append(rolledBackNames, r.Name)
	}
	sort.Strings(rolledBackNames)

	// Persist revert targets only for robots that could still need one — those updating now,
	// currently failed, or reverting. A reverting robot needs its target most of all: it is
	// the version the controller compares the robot's next report against to decide the
	// revert has landed. done/settled/pending robots need none, keeping the map bounded.
	keep := make(map[string]bool, len(updating)+len(failed)+len(reverting))
	for _, r := range updating {
		keep[r.Name] = true
	}
	for _, r := range failed {
		keep[r.Name] = true
	}
	for _, r := range reverting {
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
		// Reverts OBSERVED to land. A robot whose rollback was acknowledged but not yet
		// reported back is not counted here: it has not returned to service on the previous
		// version, and its granted capabilities are still suspended. It is counted in
		// robotsPending and in capabilitiesSuspendedOn instead, which is what stops a
		// rollout from reading as settled over robots that have lost their capabilities.
		//nolint:gosec // small fleet counts
		RobotsRolledBack: int32(len(rolledBack)),
		//nolint:gosec // small fleet counts
		// A downgrade-refused robot is not pending: it will never enter a batch, and
		// counting it as pending would leave the rollout reporting work outstanding that
		// it has already decided not to do — a progress bar that can never reach the end.
		RobotsPending: int32(total - len(done) - len(failed) - len(rolledBack) - len(newer)),
		// Robots refused as a downgrade only. A robot lacking a required hardware type is
		// never dispatched either, but it is not counted here: it stays in robotsPending
		// (RequiredHardware, api/v1/rollout_types.go). Counting the downgrade refusals is
		// what stops "did nothing" from being indistinguishable from "had nothing to do".
		//nolint:gosec // small fleet counts
		RobotsIneligible: int32(len(newer)),
		// Reverting robots are included: their model entry is Updating while the adapter
		// reactivates the retained version, so the capabilities that model grants are
		// suspended on exactly the same terms as a forward update's.
		//nolint:gosec // small fleet counts
		CapabilitiesSuspendedOn: int32(len(updating) + len(reverting)),
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
	settled := len(done) + len(rolledBack) + len(newer) + len(excluded)
	switch {
	case settled == total && total > 0:
		// Every robot is on newVersion, was reverted AND observed back on the previous
		// version, was refused as a downgrade, or was excluded by an operator resume.
		// Succeeded, but a non-zero RobotsRolledBack / ExcludedRobots surfaces the version
		// fragmentation (§6.7, ADR-0041). Counting excluded robots as settled is what lets a
		// resumed rollout reach a terminal phase at all — without it the record could never
		// be deleted, which is the other half of the wedge resume fixes. A robot still
		// reverting is deliberately NOT settled: its capabilities are suspended and the
		// rollout still owns the state that restores them.
		st.Phase = fleetv1.RolloutPhaseSucceeded
	// Paused is checked before Failed and is not terminal: pauseOnError halted the rollout so
	// an operator could decide, and `swarmctl rollout resume` is the exit (ADR-0041).
	case paused:
		st.Phase = fleetv1.RolloutPhasePaused
	// Terminal failure. Every robot has settled or failed, and at least one failed without
	// being reverted, repaired or excluded — so there is no further work this rollout can do.
	// Before this branch existed such a rollout sat InProgress for ever and could never be
	// deleted, which on this kind means model-granted capabilities left suspended with no
	// owner to restore them. Reachable only with pauseOnError: false; derived per reconcile
	// rather than latched, like `paused`.
	case total > 0 && len(failed) > 0 && settled+len(failed) == total:
		st.Phase = fleetv1.RolloutPhaseFailed
	case len(updating) > 0 || len(done) > 0 || len(failed) > 0 || len(rolledBack) > 0 || len(reverting) > 0:
		st.Phase = fleetv1.RolloutPhaseInProgress
	default:
		st.Phase = fleetv1.RolloutPhasePending
	}
	return st
}

// modelActiveAt reports whether a robot is running the named model, Active, at version want.
// It is the evidence a rollback has actually landed — as opposed to the adapter having
// acknowledged that it would.
func modelActiveAt(rob *fleetv1.Robot, modelName, want string) bool {
	e := modelEntry(rob, modelName)
	return e != nil && e.Status == fleetv1.ModelStatusActive && e.RunningVersion == want
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

	inNamespace := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return rolloutsOfNamespace(ctx, r, &fleetv1.ModelRolloutList{}, obj)
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.ModelRollout{}).
		Watches(&fleetv1.Robot{}, robotToRollouts).
		Watches(&fleetv1.FleetZone{}, inNamespace).
		Watches(&fleetv1.SwarmadaConfig{}, inNamespace).
		Complete(r)
}
