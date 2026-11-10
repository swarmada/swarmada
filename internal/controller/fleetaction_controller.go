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
	stderrors "errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/executorguard"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

const (
	// defaultLeaseDuration is the fallback task-lease horizon: how long a robot may
	// execute an assigned action without a renewal before it MUST self-stop
	// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee). The control plane renews within this window while the
	// assignment stands. The live value is resolved per-namespace from SwarmadaConfig
	// (spec.scheduling.leaseDurationSeconds) by leaseTimings; this default matches
	// that field's CRD default and applies only when no config is readable or it
	// carries no positive value (ADR-0044).
	defaultLeaseDuration = 30 * time.Second
	// defaultLeaseRenewInterval is the fallback for how often the control plane
	// refreshes the lease while the robot is reachable — well below the horizon
	// (always duration/3, RFC-0001 #control-plane-scheduler). It is DERIVED, never separately configurable.
	defaultLeaseRenewInterval = defaultLeaseDuration / 3
	// defaultLeaseClockSkew is the fallback safety margin added before a lease is
	// considered provably expired (assignment-lease condition 3: now ≥ lastRenewal +
	// duration + skew). The live value comes from
	// spec.scheduling.clockSkewMarginSeconds; this default matches that field's CRD
	// default and applies only when no config is readable.
	defaultLeaseClockSkew = 5 * time.Second

	// tdeMinRetryAfter / tdeMaxRetryAfter are the fail-safe bounds on the requeue
	// backoff after a TDE Denied (RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration). They match the CRD defaults for
	// spec.trafficDeconfliction.{min,max}RetryAfterSeconds; the namespace config
	// overrides them via tdeRetryBounds when a SwarmadaConfig is readable.
	tdeMinRetryAfter = 10 * time.Second
	tdeMaxRetryAfter = 120 * time.Second

	// pendingRecheckInterval is how soon a Pending action that found no candidate
	// re-runs selection. It is a controller-runtime requeue, not a FleetAction
	// lifecycle event: nothing about the action changed, and the next attempt may
	// succeed only because a robot elsewhere went Idle.
	//
	// Deliberately NOT SwarmadaConfig.spec.scheduling.actionRequeueBackoffSeconds,
	// despite the shared word. That field is the namespace default for
	// spec.retryPolicy.backoffSeconds — the wait after a FAILED action before it is
	// requeued to Pending (see requeueBackoff) — and RFC-0001 declares no field for
	// this interval at all. Pointing one field at both would give a single number two
	// unrelated meanings and two different fail-safes.
	pendingRecheckInterval = 15 * time.Second
)

// RBAC for the FleetAction controller. Declared as a standalone, markers-only
// comment group (blank lines both sides): controller-gen silently drops
// +kubebuilder:rbac markers that share a comment group with prose, so these
// MUST NOT be folded into the type's doc comment below (observed with
// controller-gen v0.16.5 — the fleetactions/fleetactions-status/fleetactions-finalizers
// markers vanished while they lived inside the FleetActionReconciler doc comment).

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetadapters,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleettasks,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions/finalizers,verbs=update
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=zonemaintenances,verbs=get;list;watch

// FleetActionReconciler reconciles FleetAction objects.
type FleetActionReconciler struct {
	client.Client
	// APIReader is the uncached reader the dispatch choke point lists Robots through, so
	// a Robot admitted a moment ago is counted when deciding whether two carry one wire
	// robot-id (dispatch_holds.go). Nil falls back to Client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Scheduler scheduler.Scheduler
	// TDE is the deconfliction gate the reconciler MUST pass before committing an
	// assignment (RFC-0001 #tde-normative-invariants, TDE-1). Defaulted in SetupWithManager; a nil TDE (only via
	// direct construction in unit tests) skips the zone-capacity gate.
	TDE tde.TrafficDeconflictionEngine
	// Commander pushes assign_action / renew_lease Commands to a robot's adapter over
	// ControlStream (RFC-0001 #safety-connectivity-loss-behaviour). It is BEST-EFFORT and NON-GATING: the authoritative
	// single-executor lease/generation/phase state is written first and is never
	// altered by the wire result. Nil (ControlStream disabled) means no wire push —
	// the control-plane state machine is unchanged.
	Commander command.ActionCommander
	// Validator asks the chosen robot's adapter, at assignment, whether it can serve the
	// CONCRETE action instance (type + opaque payload) — the per-instance confirmation the
	// supported-action catalog cannot express (§9.2.3). Nil (ControlStream disabled, or an
	// operator who does not want the extra round trip) falls back to the catalog gate
	// alone, which is the v0.2 behaviour.
	Validator command.ActionValidator
	// Audit records FLEETACTION_CANCELLED into the RFC-0001 #security-audit-logging chain when a cancel is
	// finalized. Nil disables audit recording. The actor is this controller (the
	// operator who requested the cancel is captured by the API-server audit; the
	// annotation write was SAR-gated by the FleetAction cancel webhook).
	Audit audit.Recorder
	// Recorder emits Kubernetes Events (e.g. FleetActionFailed for onFailure: Alert
	// and exhausted-retry terminal failures). Nil disables event emission.
	Recorder record.EventRecorder

	// declines remembers which robots refused which action under
	// ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET, so FA-R5's "the Scheduler
	// tries the NEXT candidate" is true rather than asserted. It is a process-local
	// CACHE, never API state: losing it costs one wasted round trip and is relearned
	// from the next decline. Zero value is usable, so every existing construction of
	// this reconciler — including every test that builds one by hand — needs no change.
	// See dispatch_operating_declines.go for why this is scoped to that one rejection.
	declines operatingDeclineMemo

	// orphans remembers when this process first saw a robot record an action that no longer
	// exists; the record is released once the longest lease plus the skew margin has passed
	// since then (releaseOrphanedRecords). Process-local: a restart starts the wait again,
	// which only lengthens it. Zero value is usable.
	orphans orphanClock

	// GenerationStoreNamespace is the control plane's own namespace, where each namespace's
	// generation store is kept (fencing_store.go), out of reach of the users of the namespace it
	// serves. The manager sets it from POD_NAMESPACE. Empty keeps each store in the namespace it
	// serves, which only tests that run no manager do.
	GenerationStoreNamespace string
	// RequireLatchContract applies candidate filter 9's version condition (adapterLatchContract).
	RequireLatchContract bool

	// minted is the highest generation this process has issued per namespace, a floor the store
	// must not read below. Zero value is usable.
	minted generationMemory
}

// wireTimeout bounds a best-effort assign/renew push so a silent adapter never
// stalls the reconcile loop; a missing stream returns immediately regardless.
const wireTimeout = 2 * time.Second

// annCancelRequested marks a FleetAction for operator cancellation (set by the
// `swarmctl cancel task` verb, RFC-0001 #security-authorization-swarmada-rbac-roles). Its value is an optional reason. The
// reconciler drives a CONFIRMED cancel (assignment-lease single-executor discipline): a
// bound robot is freed only once it provably stopped — the adapter acknowledges
// cancel_action, or the lease is provably dead — never on an unconfirmed cancel.
const annCancelRequested = "swarmada.io/cancel-requested"

// revokingHeldMessage is the status message for a Revoking action held under
// onDisconnect=Never (or an un-consumable policy): the lease is provably dead but
// the control plane does not auto-reassign — an operator must cancel (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect).
const revokingHeldMessage = "assigned robot lost, lease provably dead — awaiting operator cancel (onDisconnect=Never)"

// annRequeueRequested marks a FleetAction for a forcible requeue (set by the
// ZoneMaintenance controller in Immediate mode, RFC-0001 #zonemaintenance-immediate-mode-step-by-step). It reuses the SAME
// confirmed-stop discipline as cancellation — the bound robot is freed only once
// it provably stopped — but the action returns to Pending (re-schedulable) instead
// of Cancelled. The value is an optional reason.
const annRequeueRequested = "swarmada.io/requeue-requested"

// annFailureAlerted marks that a FleetActionFailed operator alert has already been
// emitted for the current terminal failure, so a Failed action reconciled again
// (resync) does not re-emit. Cleared on requeue so a later attempt can re-alert.
const annFailureAlerted = "swarmada.io/failure-alerted"

// defaultMaxRetries / defaultBackoffSeconds mirror the RetryPolicy CRD defaults;
// they are applied when spec.retryPolicy is omitted entirely (nil pointer, so no
// server-side field defaulting ran).
const (
	defaultMaxRetries     int32 = 3
	defaultBackoffSeconds int32 = 30
)

// cancellingMessage marks a action awaiting confirmed cancellation.
const cancellingMessage = "cancel requested — awaiting confirmed stop"

// requeuingMessage marks a action awaiting confirmed stop before requeue.
const requeuingMessage = "requeue requested — awaiting confirmed stop"

// Hold messages. Every one of these lands on the SAME Paused phase, whose resume rule is
// identical whatever produced it (ADR-0045). They differ only in naming the intake, so an
// operator reading the object can tell an emergency stop from a declarative hold without
// consulting events — the phase alone cannot say which, and the two call for different
// investigation before a human decides to resume.
const (
	estopPausedAssignedMessage   = "paused by estop — operator resume/requeue/cancel required"
	estopPausedInProgressMessage = "paused by estop — operator resume/cancel required"
	// estopPausedUnconfirmedMessage names an Assigned action paused while its robot's stop is
	// not confirmed (the robot is not Stopped): the robot may have received the assignment and
	// still be moving, so it stays bound, and its lease is not renewed; once the lease has
	// provably expired the action waits for an operator (waitForAPerson), and never moves alone.
	estopPausedUnconfirmedMessage = "paused by estop — the robot's stop is not confirmed, so it stays bound to " +
		"this action and its lease is not renewed; once the lease has provably expired it waits for an operator, " +
		"who requeues it, cancels it, or clears the stop and resumes the robot"
	// estopHeldRevokingMessage names a Revoking action held at its phase because its robot
	// is under a stop: no re-adoption, no lease renewal, no release to Pending while the stop is
	// confirmed.
	estopHeldRevokingMessage = "held by estop — robot under an emergency stop, lease not renewed; " +
		"released after the stop is cleared and resumed, or by an operator cancel; if the robot never confirms " +
		"its stop and never reported the action running, the action returns to Pending once its lease has provably expired"

	desiredPausedAssignedMessage   = "held by desiredState=Paused — operator resume/requeue/cancel required"
	desiredPausedInProgressMessage = "held by desiredState=Paused — operator resume/cancel required"

	// returningReason is the cancel_action reason carried on the wire for a Returning
	// recovery, so an adapter log names the intent rather than a bare cancel.
	returningReason = "desiredState=Returning"
	// returningMessage marks a Returning action still awaiting its confirmed stop.
	returningMessage = "return requested — awaiting confirmed recovery"
)

// returningHeldMessage names how the adapter resolved a Returning recovery. The
// disposition is the adapter's own account of how far the robot got (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee), and it is
// what an operator needs before deciding whether the action can be resumed at all: a robot
// that COMPLETED the work needs a different decision from one recovered mid-commitment.
func returningHeldMessage(disp command.CancelDisposition) string {
	switch disp {
	case command.CancelCompleted:
		return "held after desiredState=Returning — robot reported the action complete; operator resume/cancel required"
	case command.CancelRecovered:
		return "held after desiredState=Returning — robot recovered mid-commitment; operator resume/cancel required"
	case command.CancelStopUnconfirmed:
		return "held after desiredState=Returning — robot's stop was commanded and its rest was not witnessed; " +
			"operator resume/cancel required"
	default:
		return "held after desiredState=Returning — robot stopped safely; operator resume/cancel required"
	}
}

// reasonCapabilityLost is the requeue reason recorded when a reachable robot's
// in-flight action is reassigned because the robot no longer satisfies the action's
// required capabilities (RFC-0001 Capability-loss reassignment).
const reasonCapabilityLost = "CapabilityLost"

// reasonCapabilityLostDuringExecution is the FleetAction failureReason set when the
// adapter could not safely hand off a mid-commitment robot and recovered it
// (RFC-0001 Capability-loss reassignment, Recovery outcome).
const reasonCapabilityLostDuringExecution = "CapabilityLostDuringExecution"

// reasonStopUnconfirmed is the failureReason of an action whose requeue cancel the adapter
// acknowledged CANCEL_DISPOSITION_STOP_UNCONFIRMED: the stop was commanded and the robot's
// rest could not be witnessed. The acknowledgement does not prove the lease dead
// (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee), so the action fails
// at once and its robot stays bound until the lease has provably expired (handleFailedAction).
//
// It is read back from status, so the hold survives a manager restart with nothing held in
// memory. The field also carries an adapter's FAILED message; an adapter that reports FAILED
// with exactly this text holds its own robot bound for up to one lease duration plus the
// clock-skew margin, and affects no other robot or action.
const reasonStopUnconfirmed = "StopUnconfirmed"

// Reconcile drives the FleetAction state machine.
//
//	Pending              → Assigned   (scheduler finds a robot; mints a lease)
//	Assigned/InProgress  → Revoking   (assigned robot lost — lease outstanding)
//	Revoking             → InProgress (robot returned on a live lease — re-adopt)
//	Revoking             → Pending    (prior lease PROVABLY dead — safe to reassign)
//	InProgress           → Succeeded/Failed (Fleet Adapter reports completion)
//
// The single-executor guarantee (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee, RA-4): a lost robot's action is
// NEVER reassigned on unreachability alone. It is held in Revoking until the lease
// is provably dead — the robot confirms it is not running the action, or the lease
// horizon (last renewal + the namespace lease duration + skew) passes, by which point the robot
// has self-stopped. Unreachable is not stopped; stopped is confirmed, never
// inferred.
//
// Every status write carries the resourceVersion it was computed from (patchActionStatus). A
// write that loses to a newer object, the ingestor's report of the action's end among them, is
// not an error: the action is read again and reconciled from what the API server holds.
func (r *FleetActionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := r.reconcileAction(ctx, req)
	if err != nil && errors.IsConflict(err) {
		log.FromContext(ctx).V(1).Info("a write lost to a newer object; reading it again",
			"fleetaction", req.NamespacedName, "err", err.Error())
		return ctrl.Result{Requeue: true}, nil
	}
	return res, err
}

// patchActionStatus writes action's status as a merge patch from original, on the condition
// that the object is still at original's resourceVersion. A merge patch carries only the fields
// it changes, so without the condition a phase computed from a stale copy would land on
// whatever the API server holds, a terminal phase included.
func (r *FleetActionReconciler) patchActionStatus(ctx context.Context, action, original *fleetv1.FleetAction) error {
	return r.Status().Patch(ctx, action, executorguard.StatusPatch(original))
}

// reconcileAction is one pass of Reconcile.
func (r *FleetActionReconciler) reconcileAction(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("fleetaction", req.NamespacedName)

	action := &fleetv1.FleetAction{}
	if err := r.Get(ctx, req.NamespacedName, action); err != nil {
		if errors.IsNotFound(err) {
			return r.releaseOrphanedRecords(ctx, req)
		}
		return ctrl.Result{}, fmt.Errorf("fetching fleetaction: %w", err)
	}
	r.orphans.forget(req.NamespacedName)

	// Work a stop holds keeps the stop finalizer; a deleted one waits, with its record, until the
	// stop no longer holds the robot (held_finalizer.go).
	holders, err := r.syncHeldFinalizer(ctx, action)
	if err != nil {
		return ctrl.Result{}, err
	}
	if action.DeletionTimestamp != nil {
		if len(holders) > 0 {
			return r.awaitDeletedHeld(ctx, action, holders)
		}
		return ctrl.Result{}, nil
	}

	// Terminal states retire the action: release its TDE zone reservation and free
	// the robot that held it so the fleet can reuse it. All steps are idempotent,
	// so a terminal action settles to a fixed point after one effective reconcile.
	// Failed additionally consults the onFailure/retryPolicy contract, which may
	// return the action to Pending for another attempt rather than retire it.
	switch action.Status.Phase {
	case fleetv1.ActionPhaseSucceeded, fleetv1.ActionPhaseCancelled:
		return r.retireTerminalAction(ctx, req, action)
	case fleetv1.ActionPhaseFailed:
		return r.handleFailedAction(ctx, req, action)
	}

	original := action.DeepCopy()

	// Resolve the namespace's lease horizon ONCE, and use it for every lease decision
	// below — both the server-side horizon written to status.leaseExpiresAt and the
	// value pushed to the robot as lease_duration_ms. Mixing a resolved horizon with
	// the bare default constant would let the robot's self-stop timer and the control
	// plane's reassignment horizon disagree (ADR-0044).
	leaseDur, leaseRenew, leaseSkew := leaseTimings(ctx, r.Client, req.Namespace)

	// ── Operator cancellation (RFC-0001 #safety-connectivity-loss-behaviour, confirmed-cancel) ───────────────────────
	// A cancel request frees the robot only once it provably stopped, so a cancel
	// never strands a still-executing robot (single-executor safety).
	if _, requested := action.Annotations[annCancelRequested]; requested {
		return r.handleCancel(ctx, action, original, operatorCancelIntent(action.Annotations[annCancelRequested]))
	}

	// ── Declarative cancel intent (spec.desiredState: Cancelled) ───────────────
	// The level-triggered half of the same intent, and the enactment FleetTask
	// depends on: FailFast and the Compensate path cancel a child by writing
	// Cancelled onto its spec.desiredState and doing nothing else, so while this
	// field went unread a composite could not actually stop its children.
	//
	// Routed into the SAME confirmed-cancel path as the operator annotation, so a
	// declarative cancel carries the identical single-executor guarantee — the robot
	// is freed only once it provably stopped, never on the write alone.
	//
	// Checked after the annotation so an explicit operator cancel keeps its reason.
	// Paused and Returning are NOT enacted here: resume from a safe hold is
	// operator-gated by RFC-0001 #safety-fleetaction-behaviour-during-estop, and reconciling them from this field would have to
	// settle who may lift a hold. They stay disclosed in the RFC.
	if action.Spec.DesiredState == fleetv1.DesiredStateCancelled {
		return r.handleCancel(ctx, action, original,
			unattributedCancelIntent("desiredState=Cancelled", command.CancelCauseUnspecified))
	}

	// ── Forcible requeue (RFC-0001 #zonemaintenance-immediate-mode-step-by-step ZoneMaintenance Immediate) ───────────────────
	// Same confirmed-stop discipline as cancel, but the action returns to Pending
	// (re-schedulable elsewhere) rather than terminating. Checked before scheduling
	// so a requeue-pending action is never re-assigned mid-flight.
	//
	// A stop on the bound robot comes first (RFC-0001 #safety-zonemaintenance-vs-estop-explicit-separation:
	// "estop takes precedence"). A request still standing on an Assigned or InProgress action
	// was made before the stop reached it: it is dropped, and the estop pause runs, because
	// whether the interrupted work may move on is now an operator's decision
	// (RFC-0001 #safety-fleetaction-behaviour-during-estop). On a Revoking action it waits for
	// the hold below to be released. On a Paused action it is that operator's decision, and runs,
	// if it was asked while the stop that covers the robot stood; one asked before that stop is
	// held while it stands (the stop wins), and carried out once it is cleared.
	if _, requested := action.Annotations[annRequeueRequested]; requested {
		switch overtaken, waitMsg, err := r.requeueOvertakenByEstop(ctx, action); {
		case err != nil:
			return ctrl.Result{}, err
		case overtaken == requeueDropped:
			return ctrl.Result{Requeue: true}, nil
		case overtaken == requeueRuns:
			return r.handleRequeue(ctx, action, original, waitMsg)
		case overtaken == requeueHeld:
			return r.holdRequeueUnderEstop(ctx, action, original, waitMsg)
		}
		// requeueWaits: the Revoking hold, or the Paused hold, below applies.
	} else if action.Status.Phase == fleetv1.ActionPhasePaused && isRequeueWaitMessage(action.Status.Message) {
		// A requeue request removed while it waited: the message no longer says what holds the action.
		action.Status.Message = requeueWithdrawnMessage
		return ctrl.Result{Requeue: true}, r.patchActionStatus(ctx, action, original)
	}

	// ── Deadline check ────────────────────────────────────────────────────────
	// Only an UNSTARTED action fails on a missed deadline. A started action (Assigned/
	// InProgress/Revoking) must NOT be terminalized here: its robot may still hold
	// a live lease and be physically executing, so its fate is governed by the
	// lease lifecycle below, not a wall-clock Failed transition.
	if (action.Status.Phase == "" || action.Status.Phase == fleetv1.ActionPhasePending) &&
		action.Spec.Deadline != nil && time.Now().After(action.Spec.Deadline.Time) {
		logger.Info("task deadline exceeded, marking Failed")
		now := metav1.Time{Time: time.Now()}
		action.Status.Phase = fleetv1.ActionPhaseFailed
		action.Status.Message = "deadline exceeded before scheduling"
		action.Status.CompletionTime = &now
		action.Status.FailedAt = &now
		action.Status.FailureReason = "deadline exceeded before scheduling"
		metrics.IncAssignmentFailure(action.Namespace, metrics.FailureDeadlineExceeded)
		return ctrl.Result{}, r.patchActionStatus(ctx, action, original)
	}

	// ── Assignment-lease lifecycle (single-executor guarantee, RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee) ────────
	// Replaces the old "requeue immediately when the robot is Offline/Error/gone"
	// behaviour, which reassigned on unreachability alone — a double-execution
	// hazard (RA-4). Now a lost robot's action enters Revoking and is reassigned only
	// once the lease is PROVABLY dead.
	now := time.Now()
	if action.Status.AssignedRobot != "" &&
		(action.Status.Phase == fleetv1.ActionPhaseAssigned ||
			action.Status.Phase == fleetv1.ActionPhaseInProgress ||
			action.Status.Phase == fleetv1.ActionPhaseRevoking ||
			action.Status.Phase == fleetv1.ActionPhasePaused ||
			action.Status.Phase == fleetv1.ActionPhasePreempted) {
		robot := &fleetv1.Robot{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      action.Status.AssignedRobot,
			Namespace: req.Namespace,
		}, robot)
		if err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("fetching assigned robot: %w", err)
		}
		found := err == nil

		// ── Estop-pause (RFC-0001 #safety-fleetaction-behaviour-during-estop): estop takes precedence over the lease ───────
		// An active estop on the assigned robot pauses its action. Estop is never
		// fenced — a safe stop is always honoured. Paused is operator-gated: the
		// control plane NEVER auto-requeues or auto-resumes it.
		if found && robotUnderEstop(robot) {
			switch action.Status.Phase {
			case fleetv1.ActionPhaseAssigned:
				return r.pauseAssignedForEstop(ctx, action, original, robot, now, leaseDur, leaseRenew, leaseSkew)
			case fleetv1.ActionPhaseInProgress:
				logger.Info("estop while InProgress — pausing, keeping robot bound",
					"robot", action.Status.AssignedRobot)
				r.recordActionPausedByEstop(ctx, action, fleetv1.ActionPhaseInProgress)
				return r.applyPauseTransition(ctx, action, original, robot, found,
					estopPausedInProgressMessage, now, leaseDur, leaseRenew, pauseStopConfirmed)
			}
			// Already Paused under estop → fall through to Paused handling.
		}

		// ── A Revoking action whose robot a stop holds is held at Revoking ──
		// The stop takes precedence over the lease evaluation (RFC-0001 #fleetaction-state-machine,
		// E-stop behaviour), and the table has no Revoking → Paused edge, so the action is held
		// at its phase: it is not re-adopted, its lease is not renewed on the wire, and it is not
		// released to Pending. Re-adopting it would push a fresh lease to a robot whose stop may
		// never have reached it (a Failed stop). The hold is released when the robot is out of
		// the stop and past the operator's resume; an operator cancel or requeue is the other exit.
		// The stops are read live (stopsOverBoundRobot), so a stop issued and not yet written onto
		// the robot holds the work as a recorded one does, and so does every stop in the namespace
		// once the Robot object is gone.
		if action.Status.Phase == fleetv1.ActionPhaseRevoking {
			stops, confirmed, _, err := r.stopsOverBoundRobot(ctx, action)
			if err != nil {
				return ctrl.Result{}, err
			}
			if len(stops) > 0 || (found && estopHoldsWork(robot)) {
				if dead := leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), now, leaseSkew); executorguard.WaitsForAPerson(
					stops, confirmed, dead, heldByOperator(action)) {
					// Under a stop no automatic step moves the work (invariant I1): it waits for a person.
					return r.waitForAPerson(ctx, action, original, action.Status.AssignedRobot, stops, leaseRenew)
				}
				return r.holdRevokingUnderEstop(ctx, action, original, leaseRenew)
			}
		}

		// ── Preempted work whose robot a stop holds stays held, started or not (A12, A6): holdPreemptedUnderStop ──
		if res, held, err := r.holdPreemptedUnderStop(ctx, action, original, robot, found, leaseRenew); held || err != nil {
			return res, err
		}

		// ── Declarative hold (spec.desiredState: Paused / Returning, ADR-0045) ──
		//
		// ORDERING. This runs AFTER the estop-pause check above, deliberately. Both
		// intakes reach the same Paused phase through the same transitions, so when an
		// estop is live it is the estop that names the hold — the more urgent cause wins
		// the status message. Neither re-enters once the phase IS Paused (both switches
		// only match Assigned/InProgress), so the two intents cannot oscillate on
		// successive reconciles.
		//
		// Writing Running back does NOT appear here, and that is the decision, not an
		// omission: a held action is lifted only by an operator through the verb-gated
		// requeue/cancel intake (ADR-0045). Running therefore never contends with the
		// estop-pause check above.
		if action.Status.Phase == fleetv1.ActionPhaseAssigned ||
			action.Status.Phase == fleetv1.ActionPhaseInProgress {
			switch action.Spec.DesiredState {
			case fleetv1.DesiredStatePaused:
				logger.Info("desiredState=Paused — entering safe hold",
					"phase", action.Status.Phase, "robot", action.Status.AssignedRobot)
				msg := desiredPausedAssignedMessage
				if action.Status.Phase == fleetv1.ActionPhaseInProgress {
					msg = desiredPausedInProgressMessage
				}
				return r.applyPauseTransition(ctx, action, original, robot, found,
					msg, now, leaseDur, leaseRenew, pauseCancelRobot)

			case fleetv1.DesiredStateReturning:
				// Adapter-CONFIRMED recovery, over the existing cancel_action wire path —
				// no new message. An unconfirmed stop holds the action and keeps the robot
				// bound: unreachable is not stopped (RA-4), and freeing here would be the
				// double-execution the single-executor guarantee exists to prevent.
				confirmed, disp := r.confirmedStopWithDisposition(
					ctx, action, action.Status.AssignedRobot,
					unattributedCancelIntent(returningReason, command.CancelCauseUnspecified))
				if !confirmed {
					return r.holdStop(ctx, action, original, returningMessage)
				}
				logger.Info("desiredState=Returning — recovery confirmed, holding",
					"robot", action.Status.AssignedRobot, "disposition", disp)
				return r.applyPauseTransition(ctx, action, original, robot, found,
					returningHeldMessage(disp), now, leaseDur, leaseRenew, pauseStopConfirmed)
			}
		}

		// ── Paused is operator-gated (RFC-0001 #safety-fleetaction-behaviour-during-estop): NEVER auto-resume ─────────────
		// While the action stays bound to a reachable robot, keep renewing the lease
		// so it cannot be reassigned; otherwise hold. The phase never changes here —
		// only an operator moves a Paused action out.
		if action.Status.Phase == fleetv1.ActionPhasePaused {
			// A stop the robot has not confirmed: the lease is not renewed, and once it is
			// provably dead the work waits for a person; it never moves alone (invariant I1).
			if found {
				if stops, confirmed, err := r.stopOver(ctx, robot); err != nil {
					return ctrl.Result{}, err
				} else if len(stops) > 0 && !confirmed {
					return r.holdUnderUnconfirmedStop(ctx, action, original, robot.Name, stops, now, leaseDur, leaseRenew, leaseSkew)
				}
			}
			if action.Status.Message == estopPausedUnconfirmedMessage {
				return r.pauseAssignedUnconfirmed(ctx, action, original, now, leaseDur, leaseRenew, leaseSkew)
			}
			// The lease advances, with no wire renewal, only while the robot has confirmed its stop
			// (Stopped) and is connected, and only until its horizon has passed. Otherwise it is
			// frozen: under a stop not confirmed, while the robot is lost, after the clear (a
			// Resuming robot has not confirmed its stop, and neither has one released to Normal),
			// and for good once the horizon has passed (RFC-0001 #safety-action-table, The lease).
			// Once a clear has reached the work it stays frozen until the work is settled, under a
			// stop confirmed after the clear too (holdPausedLeaseAfterClear).
			if action.Status.AssignedRobot != "" && found {
				if pausedLeaseHeldAfterClear(action, robot) {
					return r.holdPausedLeaseAfterClear(ctx, action, original, robot, now, leaseRenew, leaseSkew)
				}
				if classifyRobot(action, robot, found) != robotLost && stopConfirmed(robot) &&
					!leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), now, leaseSkew) {
					action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDur)}
					if err := r.patchActionStatus(ctx, action, original); err != nil {
						return ctrl.Result{}, fmt.Errorf("renewing lease for paused task: %w", err)
					}
					return ctrl.Result{RequeueAfter: leaseRenew}, nil
				}
				return ctrl.Result{RequeueAfter: min(leaseRenew,
					untilLeaseHorizon(action.Status.LeaseExpiresAt, now, leaseRenew, leaseSkew))}, nil
			}
			return ctrl.Result{}, nil
		}

		// ── Completion timeout (spec.timeoutSeconds, RFC-0001 #fleetaction-state-machine) ──
		// A reachable InProgress action past its completion window fails, once its stop is
		// confirmed. A lost robot is left to the lease decision below: its action enters
		// Revoking, where onDisconnect decides what expiry means.
		// Not while a stop covers the robot, issued and not yet written onto it: the estop
		// pause above takes the action once the stop reaches the robot's record. Nor while
		// the robot is Resuming: a stop still holds its work until the operator's resume (I1).
		reach := classifyRobot(action, robot, found)
		if action.Status.Phase == fleetv1.ActionPhaseInProgress && reach != robotLost &&
			actionCompletionExpired(action, now) {
			if stops, _, err := r.stopOver(ctx, robot); err != nil {
				return ctrl.Result{}, err
			} else if len(stops) == 0 && !estopHoldsWork(robot) {
				return r.handleCompletionTimeout(ctx, action, original)
			}
		}

		decision := evaluateLease(action.Status.Phase, reach,
			leaseTime(action.Status.LeaseExpiresAt), now, leaseSkew)
		if decision == actionConfirmStop {
			// A Preempted action whose robot is reachable. The plane's own record of the
			// robot is not proof it stopped: the preemptor's commit rewrites the robot's
			// status.assignedAction before any adapter hears of it. So the robot is told to
			// stop, and the action is released only on the adapter's confirmation or once
			// the frozen lease is provably dead.
			released, disp, err := r.confirmPreemptedStop(ctx, action, reach)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !released {
				return ctrl.Result{RequeueAfter: min(leaseRenew,
					untilLeaseHorizon(action.Status.LeaseExpiresAt, now, leaseRenew, leaseSkew))}, nil
			}
			// A robot that answers COMPLETED finished the work: it ends Succeeded rather
			// than going back to the queue to be run again.
			if disp == command.CancelCompleted {
				return ctrl.Result{}, r.succeedOnCompletedCancel(ctx, action, original)
			}
			decision = actionReassign
		}
		switch decision {
		case actionRenew:
			// Row F12: a stop cleared before row A1 paused this work. Nothing about it resumes on
			// its own after the clear, the lease included (holdRenewalAfterClear).
			if renewalHeldAfterClear(action, robot, found) {
				return r.holdRenewalAfterClear(ctx, action, original, robot, now, leaseRenew, leaseSkew)
			}
			// Capability-loss reassignment (RFC-0001 control-plane): a reachable robot
			// that no longer satisfies the action's required capabilities (filter 3, on
			// the already-debounced status.capabilities) is stopped and the action
			// reassigned via the same confirmed-stop requeue path as ZoneMaintenance
			// Immediate — never a bare status flip (single-executor, RA-4). Estop and
			// Offline/Revoking are handled above and take precedence; this fires only
			// for a reachable robot actively holding the action.
			if found &&
				(action.Status.Phase == fleetv1.ActionPhaseAssigned || action.Status.Phase == fleetv1.ActionPhaseInProgress) &&
				!scheduler.RobotSatisfiesActionCapabilities(robot, action, r.acceptDegraded(ctx, action)) {
				return r.beginCapabilityLossReassignment(ctx, action, robot.Name)
			}

			// Reachable and executing T: refresh the lease. A Revoking action whose
			// robot returned on a live lease is RE-ADOPTED at the SAME generation —
			// never a new one (RFC-0001 #safety-reconnection-flow); reassigning here would double-execute.
			action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDur)}
			if action.Status.Phase == fleetv1.ActionPhaseRevoking {
				logger.Info("re-adopting task on live lease", "robot", action.Status.AssignedRobot,
					"generation", action.Status.AssignmentGeneration)
				action.Status.Phase = fleetv1.ActionPhaseInProgress
				action.Status.Message = "re-adopted after connectivity recovered"
				// The disconnect resolved without reassignment; drop the AfterTimeout anchor.
				action.Status.DisconnectedAt = nil
			}
			if err := r.patchActionStatus(ctx, action, original); err != nil {
				return ctrl.Result{}, fmt.Errorf("renewing lease: %w", err)
			}
			metrics.IncLeaseRenewal(action.Namespace)
			// Refresh the robot's self-stop deadline over the wire (best-effort, AFTER
			// the authoritative renewal). A failed push never changes the server-side
			// lease — the robot will self-stop on its own timer if renewals stop.
			r.pushRenewLease(ctx, action, leaseDur)
			requeue := leaseRenew
			if deadline, ok := completionDeadline(action); ok && action.Status.Phase == fleetv1.ActionPhaseInProgress {
				requeue = min(requeue, max(time.Until(deadline), time.Second))
			}
			return ctrl.Result{RequeueAfter: requeue}, nil

		case actionRevoke:
			// Connectivity loss / fault with a lease outstanding: stop renewing,
			// enter Revoking, keep the robot bound. The lease is FROZEN; the robot
			// self-stops at expiry. We do NOT reassign — unreachable ≠ stopped (RA-4).
			logger.Info("assigned robot lost — Revoking, gating reassignment on lease expiry",
				"robot", action.Status.AssignedRobot, "leaseExpiresAt", action.Status.LeaseExpiresAt)
			action.Status.Phase = fleetv1.ActionPhaseRevoking
			action.Status.Message = "assigned robot unreachable — awaiting provable lease expiry"
			if action.Status.LeaseExpiresAt == nil {
				// Never reassign without a provable-death horizon; establish one.
				action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDur)}
			}
			// Anchor the disconnect wall-clock for onDisconnect=AfterTimeout. Set once
			// on the FIRST entry into Revoking-by-disconnect so the ceiling measures from
			// the loss, not from each requeue.
			if action.Status.DisconnectedAt == nil {
				action.Status.DisconnectedAt = &metav1.Time{Time: now}
			}
			// Hold the zone slot through the disconnect window (extends the Reserved
			// TTL to disconnectedReservationTTL) so it is not released before the
			// robot's reconnect chance (RFC-0001 #tde-reservation-state).
			if r.TDE != nil && action.Spec.Zone != "" {
				_ = r.TDE.OnActionPhaseChanged(ctx, req.Namespace, action.Spec.Zone, action.Name, fleetv1.ActionPhaseRevoking)
			}
			if err := r.patchActionStatus(ctx, action, original); err != nil {
				return ctrl.Result{}, fmt.Errorf("entering revoking: %w", err)
			}
			return ctrl.Result{RequeueAfter: untilLeaseHorizon(action.Status.LeaseExpiresAt, now, leaseRenew, leaseSkew)}, nil

		case actionHold:
			// Not yet safe to requeue. Never reassign on a guess. A Preempted action
			// whose robot is lost polls promptly for its robot's return; a Revoking
			// action waits for the lease horizon.
			if action.Status.Phase == fleetv1.ActionPhasePreempted {
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			return ctrl.Result{RequeueAfter: untilLeaseHorizon(action.Status.LeaseExpiresAt, now, leaseRenew, leaseSkew)}, nil

		case actionReassign:
			// Lease PROVABLY dead (robot reachable & confirmed not running T, or the
			// lease expired past the horizon). A Revoking action here is in disconnect
			// recovery: SwarmadaConfig.actionCancellation.onDisconnect governs whether the
			// control plane AUTO-recovers it (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect). The lease horizon stays the
			// single-executor safety floor under every disposition — the policy only
			// decides what happens once that floor is crossed. A non-Revoking
			// actionReassign (a clean release) is never policy-gated.
			if action.Status.Phase == fleetv1.ActionPhaseRevoking {
				switch policy, timeout := r.onDisconnectPolicy(ctx, req.Namespace); policy {
				case fleetv1.ActionCancellationAfterTimeout:
					// Wall-clock ceiling on top of the lease: auto-reassign only once the
					// disconnect has ALSO outlasted disconnectTimeoutSeconds. Until then
					// hold Revoking and requeue at the remaining time.
					if action.Status.DisconnectedAt == nil {
						action.Status.DisconnectedAt = &metav1.Time{Time: now}
					}
					ceiling := action.Status.DisconnectedAt.Add(time.Duration(timeout) * time.Second)
					if now.Before(ceiling) {
						action.Status.Message = fmt.Sprintf(
							"assigned robot lost — holding until AfterTimeout ceiling (%ds)", timeout)
						if err := r.patchActionStatus(ctx, action, original); err != nil {
							return ctrl.Result{}, fmt.Errorf("holding revoking task: %w", err)
						}
						return ctrl.Result{RequeueAfter: ceiling.Sub(now)}, nil
					}
					logger.Info("onDisconnect=AfterTimeout ceiling crossed — reassigning",
						"task", action.Name, "disconnectTimeoutSeconds", timeout)
					// ceiling crossed → fall through to the reassignment below.
				case fleetv1.ActionCancellationWhenActionExpired:
					// WhenActionExpired: a Revoking action whose per-action completion window
					// (spec.timeoutSeconds from status.startedAt) has passed is TERMINATED
					// rather than reassigned — expiry renders completion moot (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect).
					// The lease is provably dead here, so the robot provably stopped:
					// finalize to Cancelled via the confirmed-stop path (frees the robot,
					// lease, and TDE slot). A action with no timeout, or one that never
					// started, cannot expire → hold like Never below.
					if actionCompletionExpired(action, now) {
						logger.Info("onDisconnect=WhenActionExpired: completion window elapsed while Revoking — cancelling",
							"task", action.Name, "timeoutSeconds", *action.Spec.TimeoutSeconds)
						action.Status.DisconnectedAt = nil
						return ctrl.Result{}, r.finalizeCancel(ctx, action, original, action.Status.AssignedRobot,
							unattributedCancelIntent("expired while disconnected (onDisconnect=WhenActionExpired)",
								command.CancelCauseRevocation))
					}
					fallthrough
				case fleetv1.ActionCancellationNever, fleetv1.ActionCancellationPolicy(""):
					// Never (default): hold Revoking until an operator cancels — the safest
					// disposition for actions with physical side effects (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect). Keep the
					// robot bound; the operator's `swarmctl cancel task` drives handleCancel,
					// which finalizes once the robot provably stopped. Event-driven: the
					// robot's return or the cancel annotation re-triggers this reconcile.
					if action.Status.Message != revokingHeldMessage {
						action.Status.Message = revokingHeldMessage
						if err := r.patchActionStatus(ctx, action, original); err != nil {
							return ctrl.Result{}, fmt.Errorf("holding revoking task: %w", err)
						}
					}
					return ctrl.Result{}, nil
				}
			}
			// Release the robot and requeue. The generation is retained; the next
			// assignment mints a strictly-greater one.
			logger.Info("prior lease provably dead — reassigning", "robot", action.Status.AssignedRobot,
				"generation", action.Status.AssignmentGeneration)
			if found && robot.Status.AssignedAction == action.Name {
				robotOriginal := robot.DeepCopy()
				robot.Status.AssignedAction = ""
				_ = r.Status().Patch(ctx, robot, client.MergeFrom(robotOriginal))
			}
			// The dead assignment's zone slot is freed so it does not block the
			// reassignment target (RFC-0001 #tde-reservation-state).
			r.releaseReservation(ctx, req.Namespace, action.Spec.Zone, action.Name)
			// A Revoking action reassigning here is a genuine lease EXPIRY (the robot
			// self-stopped, RFC-0001 #control-plane-observability-prometheus-metrics-contract); a non-Revoking clean release (robot present &
			// confirmed not running T) is not counted.
			if action.Status.Phase == fleetv1.ActionPhaseRevoking {
				metrics.IncLeaseExpiry(action.Namespace)
			}
			action.Status.Phase = fleetv1.ActionPhasePending
			action.Status.AssignedRobot = ""
			action.Status.LeaseExpiresAt = nil
			action.Status.DisconnectedAt = nil
			action.Status.Message = "prior lease provably dead — requeued for reassignment"
			return ctrl.Result{RequeueAfter: 2 * time.Second},
				r.patchActionStatus(ctx, action, original)
		}
	}

	// ── A Paused action no robot is bound to ──
	// A hold on an Assigned action released the binding. The robot that ran it keeps its record
	// while a stop covers it: the record is the one link from this work to the stop that paused
	// it, and the requeue check and the executor guard both read it. Once no stop covers the
	// robot, the record is released through the executor guard and the robot is free for other
	// work; the action stays Paused for an operator.
	if action.Status.Phase == fleetv1.ActionPhasePaused && action.Status.AssignedRobot == "" {
		wait, _, _, err := r.releaseRecorders(ctx, action, now, leaseSkew)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	// ── Scheduling ────────────────────────────────────────────────────────────
	if action.Status.Phase == "" || action.Status.Phase == fleetv1.ActionPhasePending {
		// Anchor the assignment-latency clock the first reconcile the action is seen
		// Pending (RFC-0001 #control-plane-observability-prometheus-metrics-contract). It persists via the no-robot / TDE-denied patches below,
		// so a action that waits accumulates its true queue time; it is cleared and
		// observed on the Assigned transition. Already-set (an earlier Pending
		// episode) is left untouched so the measured window is entering-Pending→Assigned.
		if action.Status.PendingSince == nil {
			n := metav1.Now()
			action.Status.PendingSince = &n
		}
		robotList := &fleetv1.RobotList{}
		if err := r.List(ctx, robotList, client.InNamespace(req.Namespace)); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing robots: %w", err)
		}
		// The executor guard: a robot that still records this action (a deleted predecessor of
		// the same name, or the robot a pause released the binding from) may still be running
		// it. No robot is given the action until that robot is provably off it.
		gate, err := r.executorGate(ctx, action, robotList.Items, now, leaseSkew, false)
		if err != nil {
			return ctrl.Result{}, err
		}
		if wait := gate.Wait; wait > 0 {
			msg := executorGateMessage(gate.Blocked, action.Name, now.Add(wait), gate.Why)
			if gate.Stop != "" {
				msg = executorStopMessage(action, gate.Blocked, gate.Stop)
			}
			if action.Status.Message != msg {
				action.Status.Message = msg
				if err := r.patchActionStatus(ctx, action, original); err != nil {
					return ctrl.Result{}, fmt.Errorf("holding task behind a previous executor: %w", err)
				}
				if gate.Stop != "" {
					warnActionAndTask(r.Recorder, action, EventActionAwaitingOperatorDecision, msg)
				}
			}
			return ctrl.Result{RequeueAfter: wait}, nil
		}

		// Row A15: no dispatch toward a destination in a zone under a stop (dispatch_zone_stopped.go).
		if res, held, err := r.holdForStoppedDestination(ctx, action, original); held || err != nil {
			return res, err
		}

		// ADR-0032 assignment gate: withhold any robot whose bound FleetAdapter is not fit to
		// receive work (Connected + conformance Passed), FAIL CLOSED. The admission gate checks this
		// once, at admission; readiness can flip afterwards (heartbeats lapse → Degraded/Disconnected;
		// a report digest/ConfigMap change → Failed) and nothing re-examined it, so those robots kept
		// getting work. Filtering the candidate list HERE covers both the normal selection below and
		// the preemption search, which reads the same slice. Dispatch only: telemetry, heartbeats and
		// estop are untouched, and work already in flight is never revoked by this gate.
		eligible, withheld, filtersDegraded, restrictions := r.filterDispatchEligible(ctx, action, robotList.Items)
		// FA-R5's plane-side half: a robot whose adapter has already DECLINED this
		// assignment on an operating constraint is not offered it again. Applied after the
		// numbered filters and outside them — the chain is RFC-0001's roster and this is a
		// process-local scheduling memo — but reported as filter 12, because it is the same
		// rule reached from stronger evidence. See dispatch_operating_declines.go.
		eligible, withheld = dropDeclinedCandidates(action, r.declines.declined(action), eligible, withheld)
		logDispatchExclusions(ctx, action, withheld)
		// Kept for filter 12's refusal sentence, which names the zones containing each
		// robot it withheld (operatingConstraintMessage); eligible is a new slice, so this
		// still holds every robot that was listed.
		listed := robotList.Items
		robotList.Items = eligible

		// ── Candidate filter 11 — zone restriction admissibility (ADR-0058 §5) ──
		//
		// Applied BEFORE selection because it does not eliminate a candidate, it
		// eliminates the ACTION: its answer does not vary by robot, so running it per
		// candidate would ask the same question once per robot and report a standing
		// refusal as "no eligible robot".
		//
		// The terminal/Pending split is the operator-facing half of the design.
		// ZoneEntryDenied and ZoneCapabilityDenied are standing decisions that no retry
		// and no fleet change will satisfy, so they go to Failed. Everything else is a
		// fact about today — a FleetZone that becomes readable makes the same action
		// dispatchable — so the action waits like any other unsatisfied Pending.
		if restrictions != nil && restrictions.refuse {
			action.Status.Message = restrictions.message
			if !restrictions.terminal {
				logger.Info("zone restrictions hold this action", "reason", restrictions.reason)
				action.Status.Phase = fleetv1.ActionPhasePending
				_ = r.patchActionStatus(ctx, action, original)
				return ctrl.Result{RequeueAfter: pendingRecheckInterval}, nil
			}
			// retryCount is NOT incremented: nothing about retrying changes a standing
			// rule, and inflating the count would make an operator read a policy refusal
			// as a flaky robot.
			logger.Info("zone restrictions refuse this action", "reason", restrictions.reason)
			action.Status.Phase = fleetv1.ActionPhaseFailed
			action.Status.FailureReason = restrictions.reason
			now := metav1.Now()
			action.Status.FailedAt = &now
			if r.Recorder != nil {
				r.Recorder.Event(action, corev1.EventTypeWarning, restrictions.reason, restrictions.message)
			}
			_ = r.patchActionStatus(ctx, action, original)
			return ctrl.Result{}, nil
		}

		// The narrowed subtree is NAMED, never silent. Trimming a denied descendant
		// quietly is how an operator comes to believe a region was serviced that was
		// never entered — on a fleet where a truncated run and a complete one can be
		// indistinguishable in the outcome report.
		narrowNote := ""
		if restrictions != nil && len(restrictions.narrowedOut) > 0 {
			narrowNote = fmt.Sprintf(
				" zone %q was narrowed: %s excluded by an effective entry denial; the action "+
					"proceeds against the remainder.",
				action.Spec.Zone, strings.Join(restrictions.narrowedOut, ", "))
			action.Status.Message = strings.TrimSpace(narrowNote)
		}

		// ── G2 · Scheduling — external dependencies: sensor streams ──────────────
		//
		// Applied HERE, after the candidate filters and BEFORE selection, for the same
		// reason filter 11 is: the answer does not vary by robot, so asking it per
		// candidate would report one missing camera as "no eligible robot". It reads
		// the ALREADY-FILTERED set, so a robot withheld by adapter readiness or a zone
		// rule cannot satisfy a stream requirement it could not actually serve.
		//
		// The refusal is NOT terminal and does NOT increment retryCount: a robot that
		// reconnects and re-declares its streams, a capability that returns to Active,
		// or a sibling member that gets assigned all make the same action dispatchable
		// with no edit at all. Inflating the retry count would make an operator read a
		// missing declaration as a flaky robot.
		//
		// NO ROBOT IS COMMITTED. That is the whole point of gating at scheduling rather
		// than at dispatch, and the reason the return is here rather than after
		// selectServableRobot: by the time a robot is chosen it has been offered work.
		if refusal := r.sensorStreamGate(ctx, action, robotList.Items); refusal != nil {
			logger.Info("sensor-stream dependency holds this action",
				"reason", refusal.Reason, "message", refusal.Message)
			r.applySensorStreamRefusal(action, refusal)
			// RA-1, enforced rather than intended. This branch repeats every
			// pendingRecheckInterval for as long as the dependency is absent — which can
			// be hours — so the write is issued only when the status actually CHANGED.
			// A held action must cost the API server one write on the transition and
			// nothing per pass; a PATCH per recheck per blocked action is a telemetry
			// tick by another name.
			if !equality.Semantic.DeepEqual(action.Status, original.Status) {
				_ = r.patchActionStatus(ctx, action, original)
			}
			return ctrl.Result{RequeueAfter: pendingRecheckInterval}, nil
		}
		// Every dependency this action names is satisfied. Clear a stale False so an
		// action that was held and is now schedulable does not carry the old reason
		// into its assignment; a no-op for the overwhelming majority of actions, which
		// never had the condition written at all.
		clearSensorStreamRefusal(action)

		acceptDegraded := r.acceptDegraded(ctx, action)
		robot, declined, err := r.selectServable(ctx, action, robotList.Items,
			acceptDegraded, r.preferSameManufacturer(ctx, req.Namespace),
			r.honorPreferredRobot(ctx, req.Namespace))
		// preemptVictim is a lower-band action whose robot a preemptor-band (Critical or High) action will take.
		// It is FOUND here but marked Preempted only AFTER the TDE gate grants, so a
		// denied/failed assignment never strands a Preempted victim.
		var preemptVictim *fleetv1.FleetAction
		if err != nil {
			// No Idle robot. A preemptor band (Critical or High) may preempt a
			// lower-band (Normal/Low) action on an otherwise-eligible robot (§9.1.4.3) —
			// same band rule as the TDE reservation preemption. It displaces one robot
			// from its action to this one; the displaced action is evicted safely via its
			// Preempted phase.
			if action.Spec.Priority.CanPreempt() {
				cand, victim, perr := r.findPreemptionCandidate(ctx, action, robotList.Items)
				if perr != nil {
					return ctrl.Result{}, perr
				}
				if cand != nil {
					if ok, d := r.actionServable(ctx, action, cand); ok {
						robot = *cand
						preemptVictim = victim
						err = nil
					} else if d != nil {
						declined = d
					}
				}
			}
			if err != nil {
				// No eligible robot and no preemption possible — requeue after a
				// short backoff.
				//
				// The message NAMES THE FILTERS that eliminated the candidates. It
				// used to be err.Error() — the scheduler's single "no eligible robot
				// available" sentinel, identical whether the cause was a flat
				// battery, the wrong zone, or an estop — and the cause lives on a
				// different object from the action, so an operator reading only the
				// action had nothing to go on. noCandidateMessage builds a stable
				// tally (RA-1: this patch repeats every requeue, so an unchanged
				// fleet must produce an unchanged string).
				// The narrowing note is APPENDED rather than replaced. An action that was
				// both narrowed and has no candidate must report both: the operator needs
				// to know which zones were excluded even while the reason nothing was
				// dispatched is something else entirely. It is a stable string over a
				// stable exclusion set, so this patch still repeats unchanged (RA-1).
				msg := noCandidateMessage(action, withheld, robotList.Items, acceptDegraded, filtersDegraded) + narrowNote

				// ── Candidate filter 12 — adapter operating-constraint support ──
				//
				// Filter 12 is ROBOT-SCOPED, so unlike filter 11 it produces ordinary
				// candidate exclusions and reaches the action only here, when those
				// exclusions left nothing to dispatch. When they did, the refusal is NAMED:
				// the generic "no eligible robot" tally is true but it is not the sentence
				// an operator needs, and "no robot here can honour the rule" is one of the
				// five ADR-0058 §9 says collapsing costs them the difference.
				//
				// PENDING, NOT FAILED, AND THE REASON IS CARRIED WHILE PENDING. This is a
				// fact about today's fleet — a conforming adapter deployed tomorrow makes
				// the same action dispatchable — so the action waits and fails on its
				// deadline like any other unsatisfied Pending. retryCount is NOT
				// incremented, matching filter 11: nothing about retrying changes what the
				// fleet can honour, and inflating the count makes an operator read a policy
				// hold as a flaky robot.
				if withheldByOperatingConstraint(withheld) {
					action.Status.FailureReason = fleetv1.ActionFailureZoneOperatingConstraintUnmet
					msg = operatingConstraintMessage(action, restrictions.operating, withheld, listed) + ". " + msg
				}

				// EVERY CANDIDATE REFUSED THIS ASSIGNMENT AS INVALID_ACTION — the one
				// conclusion the plane can draw from a value it cannot trust individually.
				//
				// No failureReason is set, and that is deliberate rather than an omission:
				// the five zone-restriction reasons are a closed vocabulary about zone rules
				// (ADR-0058 §9), and this is not one — it is about the action's own type and
				// payload. Borrowing one of the five would send an operator to a FleetZone
				// when the answer is in the FleetAction, which is the exact confusion those
				// five reasons exist to prevent. The message carries it instead.
				//
				// It stays PENDING. A conforming adapter deployed tomorrow makes the same
				// action dispatchable, so this is a fact about today's fleet on the same
				// footing as ZoneOperatingConstraintUnmet — and terminating on a value that
				// conflates "this adapter cannot" with "the payload is bad" would refuse an
				// action a different adapter could serve.
				if everyCandidateRefusedTheAction(withheld, len(robotList.Items)) {
					msg = fmt.Sprintf(
						"every candidate robot's adapter REFUSED this assignment as invalid "+
							"(%d of %d asked, each once). The refusal names no cause: the wire "+
							"value covers BOTH \"this adapter does not implement spec.type %q\" "+
							"and \"spec.payload cannot be parsed\", so check the FleetAction "+
							"first — spec.type against each adapter's advertised catalog, then "+
							"spec.payload. THIS IS NOT A ROBOT-SIDE REFUSAL: no robot was busy, "+
							"flat, estopped or out of zone, and waiting will not change it. The "+
							"action is HELD, not judged — an adapter that implements this type "+
							"makes it dispatchable, which is why it is not failed",
						len(withheld), len(withheld), action.Spec.Type) + ". " + msg
				}

				logger.Info("no eligible robot, requeuing", "reason", msg)
				action.Status.Phase = fleetv1.ActionPhasePending
				action.Status.Message = msg
				if !setAdapterDeclined(action, declined) {
					_ = r.patchActionStatus(ctx, action, original)
					return ctrl.Result{RequeueAfter: pendingRecheckInterval}, nil
				}
				// A NEW decline reason: written under the resourceVersion this reconcile
				// read, and announced only once it has landed. A write that loses to a
				// concurrent one is not announced; the next pass reads the winner and
				// writes the reason again if it is still new.
				if err := r.Status().Patch(ctx, action, client.MergeFromWithOptions(original,
					client.MergeFromWithOptimisticLock{})); err != nil {
					logger.Info("the adapter's decline was not recorded; the next pass retries",
						"err", err.Error())
				} else if r.Recorder != nil {
					r.Recorder.Event(action, corev1.EventTypeWarning, conditionAdapterDeclined,
						declineMessage(declined))
				}
				return ctrl.Result{RequeueAfter: pendingRecheckInterval}, nil
			}
		}

		// ── TDE deconfliction gate (RFC-0001 #tde-normative-invariants, TDE-1) ───────────────────────────────
		// A robot is selected, but the assignment MUST NOT be committed until the
		// Traffic Deconfliction Engine grants a zone-capacity slot. This runs on
		// EVERY assignment path (normal and preemption) — an assignment never
		// bypasses deconfliction.
		reserved := false // true once a TDE slot is held for this assignment
		if r.TDE != nil && action.Spec.Zone != "" {
			res, terr := r.TDE.RequestReservation(ctx, tde.ReservationRequest{
				ActionID:   action.Name,
				RobotID:    robot.Name,
				Namespace:  req.Namespace,
				TargetZone: action.Spec.Zone,
				Priority:   action.Spec.Priority,
				MayEvict:   r.mayEvict(ctx, req.Namespace),
			})
			if terr != nil || res.Status == tde.Denied {
				// No slot — do NOT commit. Leave the action Pending and requeue with
				// the TDE's RetryAfter hint, bounded by the deconfliction backoff.
				logger.Info("TDE denied assignment; requeuing", "reason", res.DeniedReason, "err", terr)
				action.Status.Phase = fleetv1.ActionPhasePending
				action.Status.Message = "awaiting zone capacity (traffic deconfliction)"
				_ = r.patchActionStatus(ctx, action, original)
				minRetry, maxRetry := r.tdeRetryBounds(ctx, req.Namespace)
				return ctrl.Result{RequeueAfter: clampRetryAfter(res.RetryAfter, minRetry, maxRetry)}, nil
			}
			reserved = true
			if res.Status == tde.PreemptedGranted {
				// Reservations the TDE evicted to make room: drive those actions to
				// Preempted (their safe eviction is gated on the robot switching off
				// them; the physical stop rides the cancel_action wire).
				for _, pid := range res.PreemptedActionIDs {
					r.markActionPreempted(ctx, req.Namespace, pid, action.Name)
				}
			}
		}

		// The TDE gate granted (or there is no zone gate): NOW commit the §C robot
		// preemption. Deferring the victim mark to here means a Denied/failed
		// reservation above never strands a Preempted victim.
		if preemptVictim != nil {
			// THE HANDOVER STILL OBEYS THE DISPOSITION RULE (RFC-0001
			// #fleetaction-directive-supersession). Priority decides WHETHER this
			// preemptor may take the robot; it does not decide HOW the robot is handed
			// over, and the two are independent questions that were previously answered
			// by one branch.
			//
			// A same-type handover goes in place: the robot's physical commitment
			// carries over and the higher fencing token retires the victim's lease.
			// A TYPE CHANGE does not — a different type means a different executor, and
			// committing the robot to one while the victim's lease is still live is the
			// dual-execution hazard the single-executor guarantee exists to exclude. So
			// the victim must be CONFIRMED stopped first, and the assignment waits.
			//
			// The victim is still REQUEUED, never marked superseded: it is work the
			// fleet still owes, and that is what separates priority preemption from a
			// directive supersession. The one exception is a robot that answers the cancel
			// COMPLETED: the fleet no longer owes that work, so it ends Succeeded instead.
			completed := false
			if DispositionFor(preemptVictim, action) == SupersedeCancelThenDispatch {
				stopped, disp := r.confirmedStopWithDisposition(ctx, preemptVictim, robot.Name,
					unattributedCancelIntent(
						fmt.Sprintf("preempted by %s (type change requires a confirmed stop)", action.Name),
						command.CancelCauseSuperseded))
				completed = stopped && disp == command.CancelCompleted
				if !stopped {
					// Not stopped is not a failure — it is "not yet". Release the slot
					// and come back; holding the reservation while waiting would let a
					// slow stop block the zone.
					logger.Info("preemption handover awaiting confirmed stop of the victim (type change)",
						"victim", preemptVictim.Name, "victimType", preemptVictim.Spec.Type,
						"preemptorType", action.Spec.Type, "robot", robot.Name)
					r.unreserveOnFailure(ctx, reserved, req.Namespace, action.Spec.Zone, action.Name)
					action.Status.Message = "awaiting confirmed stop of " + preemptVictim.Name +
						" before a type-changing handover"
					_ = r.patchActionStatus(ctx, action, original)
					return ctrl.Result{RequeueAfter: pendingRecheckInterval}, nil
				}
			}

			if completed {
				if err := r.succeedOnCompletedCancel(ctx, preemptVictim, preemptVictim.DeepCopy()); err != nil {
					r.unreserveOnFailure(ctx, reserved, req.Namespace, action.Spec.Zone, action.Name)
					return ctrl.Result{}, err
				}
			} else {
				logger.Info("preempting lower-band task for "+string(action.Spec.Priority)+" task",
					"victim", preemptVictim.Name, "robot", robot.Name, "priority", action.Spec.Priority)
				if err := r.markVictimPreempted(ctx, preemptVictim, action.Name); err != nil {
					r.unreserveOnFailure(ctx, reserved, req.Namespace, action.Spec.Zone, action.Name)
					return ctrl.Result{}, err
				}
			}
		}

		logger.Info("scheduling task", "robot", robot.Name)
		// Observe the completed Pending→Assigned latency and release the anchor
		// (RFC-0001 #control-plane-observability-prometheus-metrics-contract). Cleared in the same write that sets Assigned.
		if action.Status.PendingSince != nil {
			metrics.ObserveAssignmentLatency(action.Namespace, priorityLabel(action.Spec.Priority),
				time.Since(action.Status.PendingSince.Time))
			action.Status.PendingSince = nil
		}
		action.Status.Phase = fleetv1.ActionPhaseAssigned
		action.Status.AssignedRobot = robot.Name
		action.Status.Message = ""
		// A renewal hold belongs to the assignment a cleared stop interrupted, not to this one.
		apimeta.RemoveStatusCondition(&action.Status.Conditions, conditionRenewalHeld)
		if action.Status.ScheduledAt == nil {
			scheduledTS := metav1.Now()
			action.Status.ScheduledAt = &scheduledTS
		}

		// Mint a new lease from the PERSISTED high-water mark (read-before-issue): the
		// namespace's generation store, reserved before this commit, so it is strictly
		// monotonic across a control-plane restart/failover, across every action a robot is
		// given, and across an action created again under its name; never reused. Establish
		// the lease horizon at assignment time.
		gen, err := r.mintGeneration(ctx, action)
		if err != nil {
			r.unreserveOnFailure(ctx, reserved, req.Namespace, action.Spec.Zone, action.Name)
			var behind errStoreBehind
			var ceiling errAtCeiling
			switch {
			case errors.IsConflict(err):
				return ctrl.Result{Requeue: true}, nil
			case stderrors.As(err, &behind):
				warnActionAndTask(r.Recorder, action, EventGenerationStoreBehind, err.Error())
				return ctrl.Result{RequeueAfter: time.Second}, nil
			case stderrors.As(err, &ceiling):
				warnActionAndTask(r.Recorder, action, EventGenerationCeilingReached, err.Error())
				return ctrl.Result{RequeueAfter: pendingRecheckInterval}, nil
			}
			return ctrl.Result{}, err
		}
		action.Status.AssignmentGeneration = gen
		action.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(leaseDur)}

		// ── Commit: TASK FIRST, with optimistic concurrency ────────────────────
		// This is the decisive write, and its order/locking matter for a real
		// double-assignment race: patching the robot (below) sets
		// robot.Status.AssignedAction, which triggers a FRESH reconcile of THIS
		// SAME action via the robotToAction watch (SetupWithManager) — and that
		// second reconcile can start running before this reconcile's own writes
		// have propagated through the separately-watched Robot and FleetAction
		// informer caches (no cross-resource ordering guarantee between the two
		// watch streams). If the action were committed AFTER the robot (the
		// previous order here), that second reconcile could still observe
		// Phase=Pending, re-run SelectRobot, and commit a SECOND robot to the
		// same action — two robots executing one action, exactly the double-
		// execution hazard RA-4 exists to prevent. This was a real, observed,
		// intermittent bug (timing-dependent, so it did not reproduce every
		// run).
		//
		// Committing the action FIRST closes most of the window (the trigger for
		// any second reconcile now fires only after this write lands). The
		// MergeFromWithOptimisticLock precondition closes the rest: if a second
		// reconcile still races in against a stale copy of this action, the API
		// server rejects its patch (409 Conflict) instead of silently accepting
		// a lost update, so it can never commit a second robot.
		if err := r.Status().Patch(ctx, action, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			r.unreserveOnFailure(ctx, reserved, req.Namespace, action.Spec.Zone, action.Name)
			if errors.IsConflict(err) {
				// Lost the race to another reconcile of this same action, which
				// already committed. Nothing was written to any robot from this
				// attempt — just requeue and let the next reconcile read the
				// now-Assigned action and correctly skip scheduling.
				logger.Info("task commit lost a concurrent scheduling race, requeuing",
					"robot", robot.Name)
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("patching task status: %w", err)
		}

		// The action commit above is authoritative and durable: mark the robot
		// InProgress. Requeue within the lease window so the lease is renewed
		// while the assignment stands (Robot watch events also trigger
		// reconciliation).
		robotOriginal := robot.DeepCopy()
		robot.Status.Phase = fleetv1.RobotPhaseInProgress
		robot.Status.AssignedAction = action.Name
		// OPTIMISTIC LOCK, not a plain merge. The optimistic-concurrency guard above is on
		// the ACTION; two reconciles for two different actions each pass their own guard
		// independently, and a plain merge patch here would let the second robot write
		// silently overwrite the first. Robot.status.assignedAction holds one name, so one
		// action would be stranded Assigned to a robot executing the other — a direct
		// violation of the single-executor guarantee ({{ref:safety-assignment-lease-and-the-single-executor-guarantee}}).
		//
		// With the precondition, the loser's write fails with a conflict and falls into the
		// branch below, which already unreserves the TDE slot and reverts the action to
		// Pending. That path was written for "the robot commit failed"; a lost race IS that.
		if err := r.Status().Patch(ctx, &robot, client.MergeFromWithOptions(robotOriginal, client.MergeFromWithOptimisticLock{})); err != nil {
			// The action now says Assigned to a robot that never got the memo —
			// not a double-execution (only one robot's status was ever touched),
			// but an assignment no robot will ever act on. Revert the action back
			// to Pending (best-effort) so the system self-heals on the next
			// reconcile instead of stranding it.
			r.unreserveOnFailure(ctx, reserved, req.Namespace, action.Spec.Zone, action.Name)
			revertOriginal := action.DeepCopy()
			action.Status.Phase = fleetv1.ActionPhasePending
			action.Status.AssignedRobot = ""
			action.Status.LeaseExpiresAt = nil
			n := metav1.Now()
			action.Status.PendingSince = &n
			action.Status.Message = "robot commit failed after task assignment; reverted to Pending"
			if revertErr := r.patchActionStatus(ctx, action, revertOriginal); revertErr != nil {
				logger.Error(revertErr, "failed to revert task to Pending after robot commit failure")
			}
			return ctrl.Result{}, fmt.Errorf("patching robot status: %w", err)
		}
		// Deliver the assignment to the robot's adapter over the wire (AFTER the
		// authoritative commit; fencing token = the freshly-minted generation). An
		// unreachable push leaves the assignment standing (best-effort); an EXPLICIT
		// rejection means the robot won't run it → release and reschedule.
		// FA-R2 — CARRY. The assignment carries the RESOLVED EFFECTIVE operating rules in
		// force AT THE LEAF THIS ROBOT IS BEING SENT TO, folded per FZ-R11 with every zone
		// of its status.containingZones[], which is also the set candidate filter 12
		// judged it against. Reading them off the verdict built earlier in this reconcile
		// means the wire and the filter cannot disagree: one projection, read once, used
		// twice.
		//
		// restrictions is nil when the action names no zone or the namespace declares no
		// restriction, and forRobot is nil-receiver safe, so an unrestricted dispatch sends
		// no constraint at all — which is PERMISSION, not "everything is denied" (§2). Its
		// second return is not read here: a robot with an unreadable containing zone was
		// withheld by filter 12 and never reaches this line.
		var inForce []fleetv1.EffectiveZoneRestriction
		if restrictions != nil {
			inForce, _ = restrictions.operating.forRobot(&robot)
		}
		rejected, rejection, declineMsg, unhonourable := r.pushAssignAction(
			ctx, action, robot.Name, leaseDur, operatingConstraintsForWire(inForce))
		if rejected {
			// The release below clears assignedRobot, and the next assignment clears
			// status.message, so the Events are what keeps the robot that declined.
			r.recordAssignmentDeclined(action, &robot, rejection, declineMsg)
			// TWO OF THE SIX REFUSALS ARE STANDING PROPERTIES OF THE (ROBOT, ACTION) PAIR,
			// and only those two suppress a retry.
			//
			//   OPERATING_CONSTRAINT_UNMET — nothing about waiting changes what an adapter
			//     build can honour.
			//   INVALID_ACTION — nothing about waiting changes whether it can parse this
			//     payload or implements this type.
			//
			// The other four are facts about a MOMENT and their retry-the-same-robot
			// behaviour is deliberately untouched: a busy robot becomes free, an unavailable
			// one recovers, a stale token is superseded by the next generation, and a missing
			// one is a protocol fault that a re-send may not repeat. Suppressing any of those
			// would idle a fleet on a fact that expires.
			switch rejection {
			case rejectionOperatingConstraintUnmet, rejectionInvalidAction:
				r.declines.record(action, robot.Name, rejection, unhonourable)
			}
			return r.releaseRejectedAssignment(ctx, req.Namespace, action, robot.Name, unhonourable)
		}
		// One candidate accepted: the question "who has already refused this" is closed.
		r.declines.forget(action)
		return ctrl.Result{RequeueAfter: leaseRenew}, nil
	}

	return ctrl.Result{}, nil
}

// ── Assignment-lease decision core (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee) ────────────────────────
//
// evaluateLease and its helpers are PURE and deterministic so the single-executor
// guarantee is exhaustively testable in isolation, free of the Kubernetes client.

// reachability is the assigned robot's observed relationship to the action.
type reachability int

const (
	// robotExecuting: reachable and reporting it holds action T.
	robotExecuting reachability = iota
	// robotFree: reachable and NOT holding T (provably not running it, assignment-lease cond. 2).
	robotFree
	// robotLost: Offline / Error / deleted — may still be executing; unconfirmed.
	robotLost
)

// leaseAction is the decision for one reconcile of an assigned action.
type leaseAction int

const (
	// actionRenew: refresh the lease (and re-adopt a Revoking action at the same generation).
	actionRenew leaseAction = iota
	// actionRevoke: → Revoking, stop renewing, keep the robot bound.
	actionRevoke
	// actionHold: stay Revoking, wait — the lease is not yet provably dead.
	actionHold
	// actionReassign: lease PROVABLY dead → release and requeue to Pending.
	actionReassign
	// actionConfirmStop: a Preempted action on a reachable robot — tell the robot to
	// stop, and release only on the adapter's confirmation or provable lease death.
	actionConfirmStop
)

// evaluateLease is the single-executor decision core. It never reassigns a action
// away from a robot that might still be executing it: reassignment happens only
// from Revoking, and only when the robot is reachable and confirms it is not
// running T (condition 2) or the lease has provably expired (condition 3).
func evaluateLease(phase fleetv1.ActionPhase, r reachability, lease *time.Time, now time.Time, skew time.Duration) leaseAction {
	switch phase {
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress:
		switch r {
		case robotExecuting:
			return actionRenew
		case robotLost:
			return actionRevoke
		default:
			// robotFree: reachable but not (yet) holding T — a transitional state
			// (not-yet-picked-up or just-completed). Do NOT reassign on this guess.
			return actionHold
		}
	case fleetv1.ActionPhaseRevoking:
		switch r {
		case robotExecuting:
			return actionRenew // robot returned on a live lease → re-adopt (RFC-0001 #safety-reconnection-flow)
		case robotFree:
			return actionReassign // assignment-lease condition 2: reachable & confirmed not running T
		default:
			// robotLost: reassign only once the lease has provably expired.
			if leaseProvablyDead(lease, now, skew) {
				return actionReassign // assignment-lease condition 3
			}
			return actionHold
		}
	case fleetv1.ActionPhasePreempted:
		// A Critical or High action displaced this one (§9.1.4.3). It is being evicted,
		// NOT re-adopted — so unlike Revoking it never renews. Its former robot must be
		// PROVEN off it before it requeues, and the robot's status.assignedAction is not
		// that proof: on robot preemption the preemptor's own commit rewrites it before
		// the adapter hears anything, and on zone-reservation preemption nothing rewrites
		// it at all. So a reachable robot, whatever it records, is asked to stop
		// (actionConfirmStop); a lost one gates on the frozen lease (condition 3).
		switch r {
		case robotExecuting, robotFree:
			return actionConfirmStop
		default:
			if leaseProvablyDead(lease, now, skew) {
				return actionReassign
			}
			return actionHold
		}
	}
	return actionHold
}

// leaseProvablyDead is assignment-lease condition 3. A nil horizon is NOT proof of death;
// the controller must never reassign on it (RA-4).
func leaseProvablyDead(lease *time.Time, now time.Time, skew time.Duration) bool {
	return lease != nil && !now.Before(lease.Add(skew))
}

// leaseTimings resolves the namespace's task-lease horizon and clock-skew margin
// (spec.scheduling.{leaseDurationSeconds,clockSkewMarginSeconds}) from a single
// SwarmadaConfig read. Each FAILS SAFE to its built-in default — defaultLeaseDuration /
// defaultLeaseClockSkew — on any problem (no config, list error, or a non-positive
// value), so an unreadable policy never lengthens the window a disconnected robot
// keeps moving (ADR-0044).
//
// This is the SINGLE resolution point for both halves of the single-executor
// guarantee. The same `duration` is written to status.leaseExpiresAt and sent to the
// robot as Command.lease_duration_ms, so the robot's self-stop timer and the control
// plane's reassignment horizon can never be computed from different values. Callers
// MUST resolve once per reconcile and thread the result — never mix a resolved value
// with a bare default constant on the same action.
//
// `renew` is always duration/3 (RFC-0001 #control-plane-scheduler): derived, not configured, so widening the
// horizon cannot also slow the renewals that keep it fresh.
//
// It is a free function rather than a method because the Robot controller resolves the
// same skew for assignedLeaseProvablyDead (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee), and both controllers must agree.
func leaseTimings(ctx context.Context, c client.Client, namespace string) (duration, renew, skew time.Duration) {
	duration, skew = defaultLeaseDuration, defaultLeaseClockSkew
	if cfg, ok := namespaceConfig(ctx, c, namespace); ok {
		if s := cfg.Spec.Scheduling.LeaseDurationSeconds; s > 0 {
			duration = time.Duration(s) * time.Second
		}
		if s := cfg.Spec.Scheduling.ClockSkewMarginSeconds; s > 0 {
			skew = time.Duration(s) * time.Second
		}
	}
	return duration, duration / 3, skew
}

// classifyRobot maps the assigned robot's observed state to a reachability. A
// deleted CRD or an Offline/Error robot is robotLost — none of these PROVE the
// physical robot stopped, so all gate on the lease.
func classifyRobot(action *fleetv1.FleetAction, robot *fleetv1.Robot, found bool) reachability {
	if !found {
		return robotLost
	}
	switch robot.Status.Phase {
	case fleetv1.RobotPhaseOffline, fleetv1.RobotPhaseError:
		return robotLost
	}
	if robot.Status.AssignedAction == action.Name {
		return robotExecuting
	}
	return robotFree
}

// findPreemptionCandidate finds a robot to free for a preemptor-band (Critical or
// High) action by displacing a lower-band (Normal/Low) action it is running
// (§9.1.4.3) — WITHOUT side effects. It considers only reachable
// (Assigned/InProgress), non-estopped robots that match the preemptor action's
// zone/capability constraints (via the same scheduler matcher, so the two paths
// cannot diverge), and prefers the lowest-priority victim (Low before Normal),
// tie-broken deterministically by robot name. It never selects a Critical (FIFO)
// or High victim — the same band rule as the TDE reservation preemption. An
// External action (spec.executor set) is excluded outright: it is dispatched to a
// ComputeEndpoint, never to a robot, so it may not displace one. Returns
// the robot and the victim action, or (nil, nil) when none exists. The caller marks
// the victim Preempted (via markVictimPreempted) only after the TDE gate grants.
func (r *FleetActionReconciler) findPreemptionCandidate(
	ctx context.Context, action *fleetv1.FleetAction, robots []fleetv1.Robot,
) (*fleetv1.Robot, *fleetv1.FleetAction, error) {
	// An external action never takes a robot, so it never frees one either. Stated
	// here as well as in the matcher because this function's contract is "find a
	// robot to displace" and the reader looking for the exclusion looks here first;
	// the caller reaches it precisely BECAUSE SelectRobot returned ErrExternalAction.
	if action.Spec.Executor != nil {
		return nil, nil, nil
	}

	var chosen *fleetv1.Robot
	var victim *fleetv1.FleetAction
	chosenRank := -1

	for i := range robots {
		rob := &robots[i]
		if rob.Status.AssignedAction == "" {
			continue
		}
		if rob.Status.Phase != fleetv1.RobotPhaseInProgress &&
			rob.Status.Phase != fleetv1.RobotPhaseAssigned {
			continue
		}
		if robotUnderEstop(rob) {
			continue
		}
		if !scheduler.RobotMatchesAction(rob, action) {
			continue
		}

		v := &fleetv1.FleetAction{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      rob.Status.AssignedAction,
			Namespace: action.Namespace,
		}, v); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, nil, fmt.Errorf("fetching preemption victim: %w", err)
		}
		if !isPreemptible(v.Spec.Priority) || !r.mayDisplace(ctx, v) {
			continue
		}

		// Prefer the lowest-priority victim (highest rank number); tie-break by
		// robot name for determinism.
		rank := priorityRank(v.Spec.Priority)
		if rank > chosenRank || (rank == chosenRank && rob.Name < chosen.Name) {
			chosen, victim, chosenRank = rob, v, rank
		}
	}

	return chosen, victim, nil
}

// markVictimPreempted displaces a preemption victim to Preempted, KEEPING its
// robot binding and freezing its lease so the eviction is gated (Preempted →
// Pending only once the adapter confirms a cancel_action for it, or its frozen lease
// is provably dead; see confirmPreemptedStop). Called only after the TDE gate grants,
// so a denied/failed assignment never strands a Preempted victim.
func (r *FleetActionReconciler) markVictimPreempted(ctx context.Context, victim *fleetv1.FleetAction, byAction string) error {
	vOrig := victim.DeepCopy()
	victim.Status.Phase = fleetv1.ActionPhasePreempted
	victim.Status.Message = fmt.Sprintf("preempted by higher-priority task %s", byAction)
	if err := r.patchActionStatus(ctx, victim, vOrig); err != nil {
		return fmt.Errorf("marking victim preempted: %w", err)
	}
	return nil
}

// confirmPreemptedStop asks a Preempted action's reachable robot to stop it, through the
// confirmed-stop gate cancel already uses (RFC-0001
// #safety-assignment-lease-and-the-single-executor-guarantee): it reports released once
// the adapter acknowledges cancel_action, or once the frozen lease is provably dead, and
// how the adapter handled the cancel. On release a robot that still records the action is
// returned to Idle — on the zone-reservation path nothing else would free it. It writes
// nothing while it waits.
//
// The wire cause is SUPERSEDED when the robot already records another action (robot
// preemption: a newer directive replaced this one on it) and UNSPECIFIED otherwise.
func (r *FleetActionReconciler) confirmPreemptedStop(ctx context.Context, action *fleetv1.FleetAction, reach reachability) (bool, command.CancelDisposition, error) {
	cause := command.CancelCauseUnspecified
	if reach == robotFree {
		cause = command.CancelCauseSuperseded
	}
	robotName := action.Status.AssignedRobot
	stopped, disp := r.confirmedStopWithDisposition(ctx, action, robotName, unattributedCancelIntent("preempted", cause))
	if !stopped {
		return false, disp, nil
	}
	return true, disp, r.releaseRobotForStop(ctx, action, robotName)
}

// succeedOnCompletedCancel ends a preemption victim whose robot answered the preemption's
// cancel_action CANCEL_DISPOSITION_COMPLETED: the robot finished the work before the cancel
// reached it, so the action is Succeeded and is not requeued, which would run finished work
// a second time. The robot's own terminal report cannot settle it once the action is
// Preempted, because the ingestor drops a report outside Assigned and InProgress. The
// status written is the ingestor's for a SUCCEEDED report; the zone slot and the lease
// horizon are released by retireTerminalAction, as for any Succeeded action.
func (r *FleetActionReconciler) succeedOnCompletedCancel(ctx context.Context, action, original *fleetv1.FleetAction) error {
	now := metav1.Now()
	action.Status.Phase = fleetv1.ActionPhaseSucceeded
	action.Status.CompletedAt = &now
	action.Status.CompletionTime = &now
	action.Status.ProgressPct = 100
	if action.Status.StartedAt == nil {
		action.Status.StartedAt = &now
	}
	action.Status.Message = "robot reported the action complete in answer to its preemption cancel; not requeued"
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return fmt.Errorf("finishing a preempted action its robot completed: %w", err)
	}
	log.FromContext(ctx).Info("preemption cancel: robot completed the task; no requeue",
		"task", action.Name, "robot", action.Status.AssignedRobot)
	return nil
}

// unreserveOnFailure releases a just-made TDE reservation when the assignment
// commit fails, so a failed write never leaves a phantom slot. Idempotent: the
// TDE's Release is a no-op if the reservation is already gone.
func (r *FleetActionReconciler) unreserveOnFailure(ctx context.Context, reserved bool, namespace, zone, actionID string) {
	if !reserved || r.TDE == nil {
		return
	}
	if err := r.TDE.ReleaseReservation(ctx, namespace, zone, actionID); err != nil {
		log.FromContext(ctx).Error(err, "unreserving after commit failure", "task", actionID)
	}
}

// releaseReservation drops a action's TDE zone reservation on a terminal/estop/
// reassignment transition (RFC-0001 #tde-reservation-state). Nil-TDE and empty-zone safe; idempotent (the
// engine's Release is a no-op if the reservation is already gone).
func (r *FleetActionReconciler) releaseReservation(ctx context.Context, namespace, zone, actionID string) {
	if r.TDE == nil || zone == "" {
		return
	}
	if err := r.TDE.ReleaseReservation(ctx, namespace, zone, actionID); err != nil {
		log.FromContext(ctx).Error(err, "releasing TDE reservation", "task", actionID)
	}
}

// pushAssignAction delivers a committed assignment to the robot's adapter over the
// wire (RFC-0001 #safety-connectivity-loss-behaviour). Best-effort and NON-GATING: it runs AFTER the authoritative
// status commit, so a rejection or unreachable adapter is logged/evented but never
// rolls back the control-plane assignment (the lease machinery remains the single
// source of truth). A nil Commander (ControlStream disabled) is a no-op.
// pushAssignAction delivers a committed assignment to the robot's adapter and
// reports whether the adapter EXPLICITLY REJECTED it (robot busy/unavailable/
// invalid/stale-fencing). A rejection means the robot definitively did not accept
// the action — it is not executing — so the caller safely releases the assignment to
// reschedule. An UNREACHABLE push (no wire / send failure / timeout) returns false:
// we cannot tell whether the robot got it, so the assignment stands and the lease
// machinery governs a truly-lost robot (never freed on unconfirmed loss, RA-4).
func (r *FleetActionReconciler) pushAssignAction(
	ctx context.Context, action *fleetv1.FleetAction, robotID string,
	leaseDur time.Duration, operating []*fav1.EffectiveOperatingConstraint,
) (rejected bool, rejection, message string, unhonourable []string) {
	if r.Commander == nil {
		return false, "", "", nil
	}
	gen := command.FencingToken(action.Status.AssignmentGeneration)
	a := command.AssignAction{
		ActionID:   action.Name,
		ActionType: string(action.Spec.Type),
		// The verb rides the assignment (contract 1.4.0). Admission accepts customType
		// only with type Custom, so this is empty for every other type. The payload is
		// not touched: the control plane never writes the verb into it.
		CustomType:      action.Spec.CustomType,
		FencingToken:    gen,
		LeaseGeneration: gen,
		LeaseDurationMs: command.LeaseDurationMs(leaseDur),
		Priority:        int32(priorityRank(action.Spec.Priority)),
	}
	if action.Spec.Deadline != nil {
		a.DeadlineMs = action.Spec.Deadline.UnixMilli()
	}
	// The payload rides the assignment itself — it is how the robot receives the work
	// to execute (RFC-0001 #fleetaction-the-payload-field), and since proto field 3 `destination` is deprecated it is
	// the only channel for one. Nil-handling mirrors actionServableBy's read for the
	// validate path: spec.payload is a pointer, and an absent payload sends no bytes
	// rather than an empty non-nil slice.
	if action.Spec.Payload != nil {
		a.Payload = action.Spec.Payload.Raw
	}
	// FA-R2 — carry. The resolved effective operating rules ride the assignment.
	//
	// WHY THE WIRE STILL CARRIES WHAT CANDIDATE FILTER 12 ALREADY CHECKED, because the
	// next reader will see two enforcements of one rule and delete this one as
	// redundant. It is not redundant, and ADR-0058 §5 names the reason in one sentence:
	// A RULE ENFORCED IN ONE PLACE IS A RULE WITH A BYPASS. That applies to the filter
	// exactly as it applies to the adapter, and there are three concrete ways the filter
	// alone is not enough:
	//
	//  1. FILTER 12 IS BUILT ON THE ADAPTER'S OWN WORD. It reads
	//     Robot.status.honouredOperatingConstraints[], projected from the adapter's
	//     CapabilitiesSnapshot. An attestation is a claim, not a measurement (§12), so
	//     the filter can only ever be as good as the claim. The constraint on the wire
	//     is the adapter being asked to meet the rule ITSELF, at the moment it matters.
	//  2. A DECLARATION GOES STALE. The snapshot that produced the projection was taken
	//     before this assignment; a robot re-flashed, a module swapped, or an adapter
	//     restarted against different hardware between snapshot and dispatch passes
	//     filter 12 on a claim that is no longer true.
	//  3. FA-R6 IS ABOUT A SUBJECT NO DECLARATION COVERS. An adapter that does not
	//     RECOGNISE a subject must decline naming it — an unrecognised subject is not an
	//     absent subject, and treating it as absent would make the namespaced extension
	//     form a universal bypass. No declaration can express "I do not know what this
	//     is", so no filter reading a declaration can catch it.
	//
	// On the happy path the adapter is therefore rarely ASKED to decline, and that is the
	// filter working rather than this being dead weight: the filter turns the common case
	// into a scheduling decision that costs no round trip, and the wire covers the cases a
	// declaration cannot describe.
	a.OperatingConstraints = operating
	wctx, cancel := context.WithTimeout(ctx, wireTimeout)
	defer cancel()
	out, err := r.Commander.PushAssignAction(wctx, action.Namespace, robotID, a)
	logger := log.FromContext(ctx)
	if err != nil {
		logger.V(1).Info("assign_action not delivered (best-effort; assignment stands, lease machinery governs)",
			"task", action.Name, "robot", robotID, "reason", err.Error())
		return false, "", "", nil
	}
	if !out.Accepted {
		logger.Info("adapter rejected assign_action; releasing the assignment to reschedule",
			"task", action.Name, "robot", robotID, "rejection", out.Rejection, "message", out.Message,
			"unhonourableSubjects", out.UnhonourableSubjects)
		// The subjects travel back to the caller, which puts them on the action's status.
		// A decline that does not name the subject is not testable and does not satisfy
		// FA-R5; a decline the plane receives named and then discards is no better to the
		// operator reading the object.
		return true, out.Rejection, out.Message, out.UnhonourableSubjects
	}
	return false, "", "", nil
}

// rejectionOperatingConstraintUnmet is AssignActionRejection's
// ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET, as the command layer renders it.
//
// Compared as a STRING because that is the shape the wire outcome carries
// (command.AssignActionOutcome.Rejection is the enum's String()); the alternative is to
// leak the proto enum into the reconciler for one comparison. It is pinned by
// TestRejectionConstantMatchesTheWireEnum so a rename on the wire cannot leave this
// silently matching nothing — which would fail OPEN, back into the re-offer loop.
const rejectionOperatingConstraintUnmet = "ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET"

// rejectionInvalidAction is AssignActionRejection's ASSIGN_ACTION_REJECTION_INVALID_ACTION.
//
// # IT CONFLATES TWO THINGS, AND THAT DECIDES WHAT THE PLANE MAY DO WITH IT
//
// The wire documents it as "unparseable / unsupported", so one value carries both "this
// adapter does not implement spec.type" — a property of the (robot, action) pair, which
// another adapter may not share — and "spec.payload cannot be parsed", which every
// candidate will refuse. `AssignActionRejection` has no separate
// UNSUPPORTED_ACTION_TYPE; its seven values are UNSPECIFIED, STALE_FENCING_TOKEN,
// MISSING_FENCING_TOKEN, ROBOT_BUSY, ROBOT_UNAVAILABLE, INVALID_ACTION and
// OPERATING_CONSTRAINT_UNMET.
//
// So a TERMINAL failure on this value would be wrong rather than merely expensive: it
// would convert "one adapter cannot do this" into "nobody can", refusing the fleet an
// action a different adapter could have served. What the plane does instead is stop
// re-offering the same assignment to the same adapter — correct for both halves — and,
// only when EVERY candidate has refused with it, say so in a sentence that names the
// action rather than the robots (everyCandidateRefusedTheAction).
//
// Pinned against the wire enum by TestRejectionConstantsMatchTheWireEnum, for the same
// reason the constant above is: a rename that left this matching nothing would fail OPEN,
// back into the re-offer loop, and silently.
const rejectionInvalidAction = "ASSIGN_ACTION_REJECTION_INVALID_ACTION"

// pushRenewLease refreshes the robot's self-stop deadline over the wire (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee).
// Best-effort and NON-GATING: it runs AFTER the authoritative lease renewal, so a
// failed push never changes the server-side lease — if renewals stop reaching the
// robot, its own timer self-stops it. A nil Commander is a no-op.
func (r *FleetActionReconciler) pushRenewLease(ctx context.Context, action *fleetv1.FleetAction, leaseDur time.Duration) {
	if r.Commander == nil || action.Status.AssignedRobot == "" {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, wireTimeout)
	defer cancel()
	out, err := r.Commander.PushRenewLease(wctx, action.Namespace, action.Status.AssignedRobot, command.RenewLease{
		ActionID:        action.Name,
		LeaseGeneration: command.FencingToken(action.Status.AssignmentGeneration),
		LeaseDurationMs: command.LeaseDurationMs(leaseDur),
	})
	if err != nil {
		log.FromContext(ctx).V(1).Info("renew_lease not delivered (best-effort; server-side lease stands)",
			"task", action.Name, "robot", action.Status.AssignedRobot, "reason", err.Error())
		return
	}
	if !out.Renewed {
		log.FromContext(ctx).Info("adapter did not renew lease (server-side lease governs single-executor safety)",
			"task", action.Name, "robot", action.Status.AssignedRobot, "message", out.Message)
	}
}

// onDisconnectPolicy resolves the namespace's actionCancellation.onDisconnect
// disposition and its wall-clock ceiling (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect). It FAILS SAFE to Never (the
// hold-until-operator disposition) on any problem — no SwarmadaConfig, a list
// error, or an AfterTimeout policy that (contra the admission webhook) carries no
// positive disconnectTimeoutSeconds — so an unreadable policy never causes an
// automatic reassignment. The returned timeout (seconds) is meaningful only for
// AfterTimeout.
func (r *FleetActionReconciler) onDisconnectPolicy(ctx context.Context, namespace string) (fleetv1.ActionCancellationPolicy, int32) {
	var configs fleetv1.SwarmadaConfigList
	if err := r.List(ctx, &configs, client.InNamespace(namespace)); err != nil || len(configs.Items) == 0 {
		return fleetv1.ActionCancellationNever, 0
	}
	tc := configs.Items[0].Spec.ActionCancellation
	if tc.OnDisconnect == fleetv1.ActionCancellationAfterTimeout {
		if tc.DisconnectTimeoutSeconds == nil || *tc.DisconnectTimeoutSeconds <= 0 {
			// AfterTimeout without a positive ceiling is unusable — fail safe to hold.
			return fleetv1.ActionCancellationNever, 0
		}
		return fleetv1.ActionCancellationAfterTimeout, *tc.DisconnectTimeoutSeconds
	}
	return tc.OnDisconnect, 0
}

// selectServableRobot picks an eligible robot whose adapter confirms it can serve this
// CONCRETE action, per §9.2.3's assignment-time validation.
//
// The catalog gate is a TYPE-level pre-filter: it answers "can this adapter serve actions
// of this kind at all", and a stale catalog can therefore admit an action the chosen robot
// cannot actually perform. That surfaces today as a failed assignment — the robot is bound,
// the task starts, and it fails at execution. Asking first turns that into a scheduling
// miss, which costs a round trip and no robot-time.
//
// A refused candidate is DROPPED and selection re-runs over the remaining pool, matching
// the specified "tries the next eligible robot". Validation is per-candidate rather than
// over the whole list on purpose: one round trip per attempt, not one per eligible robot.
func (r *FleetActionReconciler) selectServableRobot(ctx context.Context, action *fleetv1.FleetAction,
	candidates []fleetv1.Robot, acceptDegraded, preferSameManufacturer, honorPreferredRobot bool) (fleetv1.Robot, error) {
	robot, _, err := r.selectServable(ctx, action, candidates, acceptDegraded, preferSameManufacturer, honorPreferredRobot)
	return robot, err
}

// selectServable is selectServableRobot, also returning the last adapter that declined with
// a reason, so the caller can carry it onto the action when no candidate is left; nil when
// none declined.
func (r *FleetActionReconciler) selectServable(ctx context.Context, action *fleetv1.FleetAction,
	candidates []fleetv1.Robot, acceptDegraded, preferSameManufacturer, honorPreferredRobot bool) (fleetv1.Robot, *adapterDecline, error) {
	pool := candidates
	var declined *adapterDecline
	// Bounded by the pool size: every iteration removes exactly one candidate.
	for range candidates {
		robot, err := r.Scheduler.SelectRobot(action, pool, acceptDegraded, preferSameManufacturer, honorPreferredRobot)
		if err != nil {
			return robot, declined, err
		}
		ok, d := r.actionServable(ctx, action, &robot)
		if ok {
			return robot, declined, nil
		}
		if d != nil {
			declined = d
		}
		next := make([]fleetv1.Robot, 0, len(pool)-1)
		for i := range pool {
			if pool[i].Name != robot.Name {
				next = append(next, pool[i])
			}
		}
		pool = next
	}
	// Every candidate refused. Return the scheduler's own no-robot error over the empty
	// pool so the caller's existing Pending path and message shape are unchanged — this is
	// a scheduling miss, not a failure, and retryCount is not incremented (§9.2.3).
	robot, err := r.Scheduler.SelectRobot(action, nil, acceptDegraded, preferSameManufacturer, honorPreferredRobot)
	return robot, declined, err
}

// actionServableBy asks one robot's adapter whether it can serve this action instance.
//
// Three replies, and only one of them withholds the robot:
//   - servable        → yes.
//   - unsupported     → the adapter does not implement validate_action. RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist makes it
//     OPTIONAL, so this MUST NOT withhold work: the control plane dispatches on the catalog
//     gate alone, exactly as it does today. Treating "did not answer the question" as "no"
//     would make an optional command effectively mandatory and strand every adapter that
//     has not implemented it.
//   - unreachable/timeout → the robot is dropped. This is the one case §9.2.3 does not
//     state, and dropping is the safer reading: validate_action is pure inspection, so
//     dropping costs nothing and the action stays Pending and retries. Dispatching to an
//     adapter we have just failed to reach would commit the assignment and then push
//     assign_action best-effort into the same silence — a bound robot that may never have
//     received its task, which is the phantom-assignment case the lease exists to avoid.
func (r *FleetActionReconciler) actionServableBy(ctx context.Context, action *fleetv1.FleetAction, robot *fleetv1.Robot) bool {
	ok, _ := r.actionServable(ctx, action, robot)
	return ok
}

// actionServable is actionServableBy, also returning the adapter's decline when it
// answered servable=false: the one reply that carries a reason the adapter chose to give.
// An unreachable adapter declined nothing, so it returns none.
func (r *FleetActionReconciler) actionServable(ctx context.Context, action *fleetv1.FleetAction, robot *fleetv1.Robot) (bool, *adapterDecline) {
	if r.Validator == nil {
		return true, nil // catalog gate only — the v0.2 behaviour
	}
	logger := log.FromContext(ctx)
	var payload []byte
	if action.Spec.Payload != nil {
		payload = action.Spec.Payload.Raw
	}
	out, err := r.Validator.ValidateAction(ctx, robot.Namespace, robot.Name, string(action.Spec.Type),
		action.Spec.CustomType, payload)
	switch {
	case err != nil:
		logger.V(1).Info("validate_action unreachable; skipping candidate",
			"robot", robot.Name, "action", action.Name, "err", err)
		return false, nil
	case out.Unsupported:
		return true, nil
	case !out.Servable:
		logger.Info("adapter cannot serve this action instance; trying another robot",
			"robot", robot.Name, "action", action.Name, "reason", out.Message)
		return false, &adapterDecline{Adapter: robot.Spec.Adapter.Name, Robot: robot.Name, Reason: out.Message}
	default:
		return true, nil
	}
}

// recordActionPausedByEstop seals ACTION_PAUSED_BY_ESTOP (RFC-0001 #safety-required-events) as an in-flight action
// is halted by an emergency stop.
//
// prior_phase is what makes the entry actionable: the two Paused edges leave the action in
// materially different states. From Assigned the robot never started and the binding is
// released, so the work can go to another robot. From InProgress the robot is physically
// committed and stays bound with its lease alive, so nothing else can take it until an
// operator decides. A reviewer reading "paused" without knowing which one cannot tell
// whether a robot is still holding the task.
//
// Called BEFORE the phase is overwritten — the prior phase is the thing being recorded —
// and best-effort: an estop pause must never wait on an audit sink.
func (r *FleetActionReconciler) recordActionPausedByEstop(ctx context.Context, action *fleetv1.FleetAction, prior fleetv1.ActionPhase) {
	if r.Audit == nil {
		return
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventActionPausedByEstop,
		Namespace: action.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "fleetaction-controller"},
		Resource:  audit.Resource{Kind: "FleetAction", Namespace: action.Namespace, Name: action.Name},
		Action:    "pause",
		Outcome:   audit.OutcomeAllowed,
		Detail: map[string]string{
			"action_name": action.Name,
			"prior_phase": string(prior),
		},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording ACTION_PAUSED_BY_ESTOP", "action", action.Name)
	}
}

// acceptDegraded resolves a action's effective degraded-capability acceptance: an
// explicit spec.acceptDegradedCapabilities wins; otherwise the namespace default
// (SwarmadaConfig.spec.scheduling.defaultAcceptDegradedCapabilities) applies, and
// an unreadable config fails safe to false (Active-only scheduling).
func (r *FleetActionReconciler) acceptDegraded(ctx context.Context, action *fleetv1.FleetAction) bool {
	if action.Spec.AcceptDegradedCapabilities != nil {
		return *action.Spec.AcceptDegradedCapabilities
	}
	if cfg, ok := namespaceConfig(ctx, r.Client, action.Namespace); ok {
		return cfg.Spec.Scheduling.DefaultAcceptDegradedCapabilities
	}
	return false
}

// preferSameManufacturer resolves the namespace's soft manufacturer-preference
// flag (SwarmadaConfig.spec.scheduling.preferSameManufacturer, ADR-0022). It
// fails safe to the field default (true) when the config is unreadable or the
// pointer is unset, so an absent policy still honours a per-action
// spec.preferredManufacturer hint. The hint itself is what actually gates the
// tiebreak inside the scheduler.
func (r *FleetActionReconciler) preferSameManufacturer(ctx context.Context, namespace string) bool {
	if cfg, ok := namespaceConfig(ctx, r.Client, namespace); ok && cfg.Spec.Scheduling.PreferSameManufacturer != nil {
		return *cfg.Spec.Scheduling.PreferSameManufacturer
	}
	return true
}

// honorPreferredRobot resolves the namespace's soft preferred-robot flag
// (SwarmadaConfig.spec.scheduling.honorPreferredRobot, ADR-0034). Same shape and same
// fail-safe as preferSameManufacturer above: an unreadable config or an unset pointer
// falls back to the field default (true), so an absent policy still honours a per-action
// spec.preferredRobot hint. The hint itself is what gates the tiebreak in the scheduler,
// so this being true costs nothing for actions that carry no hint.
func (r *FleetActionReconciler) honorPreferredRobot(ctx context.Context, namespace string) bool {
	if cfg, ok := namespaceConfig(ctx, r.Client, namespace); ok && cfg.Spec.Scheduling.HonorPreferredRobot != nil {
		return *cfg.Spec.Scheduling.HonorPreferredRobot
	}
	return true
}

// completionDeadline is the end of an action's per-action completion window:
// spec.timeoutSeconds measured from status.startedAt, the instant the robot reported
// execution began (RFC-0001 #fleetaction-state-machine). ok is false when no positive
// timeout is set or the action never started — an unbounded or unstarted action has no
// deadline.
//
// status.startTime is NOT read. It is superseded by status.startedAt, and no controller
// writes it, so reading it is what left spec.timeoutSeconds inert.
func completionDeadline(action *fleetv1.FleetAction) (deadline time.Time, ok bool) {
	if action.Spec.TimeoutSeconds == nil || *action.Spec.TimeoutSeconds <= 0 || action.Status.StartedAt == nil {
		return time.Time{}, false
	}
	return action.Status.StartedAt.Add(time.Duration(*action.Spec.TimeoutSeconds) * time.Second), true
}

// actionCompletionExpired reports whether an action's completion window (see
// completionDeadline) has elapsed at now. It is false for an action with no deadline, so
// onDisconnect=WhenActionExpired (RFC-0001 #swarmadaconfig-action-cancellation-on-disconnect)
// holds such an action like Never rather than cancelling it.
func actionCompletionExpired(action *fleetv1.FleetAction, now time.Time) bool {
	deadline, ok := completionDeadline(action)
	return ok && !now.Before(deadline)
}

// timingOutMessage marks an action whose completion window has elapsed and whose robot
// has not yet been proven stopped.
const timingOutMessage = "timeoutSeconds elapsed — awaiting confirmed stop"

// handleCompletionTimeout fails an InProgress action whose completion window has elapsed
// (the InProgress → Failed row of RFC-0001 #fleetaction-state-machine). The robot may
// still be executing, so the transition waits on the same confirmed-stop gate as cancel:
// the adapter acknowledges cancel_action, or the lease is provably dead. While it waits
// the lease is not renewed, so the robot self-stops at the horizon if the adapter never
// answers. Only then is the robot released and the action failed; onFailure applies from
// there (handleFailedAction).
func (r *FleetActionReconciler) handleCompletionTimeout(ctx context.Context, action, original *fleetv1.FleetAction) (ctrl.Result, error) {
	robotName := action.Status.AssignedRobot
	reason := fmt.Sprintf("timeoutSeconds (%ds) elapsed since status.startedAt", *action.Spec.TimeoutSeconds)
	if !r.confirmedStop(ctx, action, robotName, unattributedCancelIntent(reason, command.CancelCauseUnspecified)) {
		return r.holdStop(ctx, action, original, timingOutMessage)
	}
	if err := r.releaseRobotForStop(ctx, action, robotName); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("completion window elapsed and stop confirmed — failing task",
		"task", action.Name, "robot", robotName, "timeoutSeconds", *action.Spec.TimeoutSeconds)
	now := metav1.Now()
	action.Status.Phase = fleetv1.ActionPhaseFailed
	action.Status.FailedAt = &now
	action.Status.CompletionTime = &now
	action.Status.FailureReason = reason
	action.Status.Message = reason
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return ctrl.Result{}, fmt.Errorf("failing timed-out task: %w", err)
	}
	return ctrl.Result{}, nil
}

// handleCancel drives a confirmed cancellation (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee). It finalizes the action
// to Cancelled — releasing the robot binding, lease, and TDE slot — ONLY when the
// assigned robot is provably not executing: it is unbound, the adapter confirms
// the cancel, or the lease is provably dead (the robot self-stopped). Otherwise it
// holds the action "cancelling" and requeues; it NEVER frees the robot/slot while
// the lease is alive, so a cancel cannot cause a double-execution.
//
// The reason is supplied by the caller rather than read from the annotation here: the
// same confirmed-cancel discipline serves both the operator annotation and the
// declarative spec.desiredState: Cancelled intent, and only the caller knows which.
func (r *FleetActionReconciler) handleCancel(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction, intent cancelIntent) (ctrl.Result, error) {
	robotName := action.Status.AssignedRobot

	// Not bound (nothing executes) or the robot is provably stopped → finalize.
	if robotName == "" || r.confirmedStop(ctx, action, robotName, intent) {
		return ctrl.Result{}, r.finalizeCancel(ctx, action, original, robotName, intent)
	}
	// Otherwise hold: the robot may still be executing. Never free it here.
	return r.holdStop(ctx, action, original, cancellingMessage)
}

// pauseIntake says what a hold rests on: how the robot was, or is, brought to a stop.
type pauseIntake int

const (
	// pauseStopConfirmed: the robot's stop is confirmed (estopState Stopped), or its adapter
	// confirmed the recovery (Returning). The robot is provably off the action.
	pauseStopConfirmed pauseIntake = iota
	// pauseCancelRobot: a declarative hold (spec.desiredState: Paused). The robot is sent
	// cancel_action over the existing wire so that it stops; its acknowledgement is the proof.
	pauseCancelRobot
	// pauseUnderStop: a stop the robot confirmed (row A1); see pauseAssignedUnderStop.
	pauseUnderStop
)

// applyPauseTransition performs the RFC-0001 #safety-fleetaction-behaviour-during-estop Paused transitions for a hold, and is the
// ONLY implementation of them. Both intakes call it — the estop path (once the robot's stop is
// confirmed; an unconfirmed stop takes pauseAssignedUnconfirmed) and the declarative
// spec.desiredState: Paused/Returning path (ADR-0045) — because two copies of a
// single-executor rule is how one of them silently stops matching the other.
//
// The split is physical, not procedural:
//
//   - Assigned: under a stop (pauseUnderStop, row A1) everything is kept, as for InProgress.
//     Under a declarative hold the binding, the lease horizon and the zone slot are released,
//     and an operator may re-assign the action (a fresh assignment mints a new generation).
//     The robot's own record of the action (status.assignedAction) is released only with
//     proof that the robot is off it: the acknowledged cancel the hold sends. Without it the
//     record stays, and the executor guard releases it once the robot acknowledges the cancel
//     or its lease is provably dead, so a requeue cannot give the action to a second robot.
//   - InProgress: the robot is physically committed, so it stays bound and its lease keeps
//     being renewed. No other robot may take the action while it is held — that is
//     RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee, and it does not care why the hold happened.
//     A declarative hold sends the robot cancel_action, so that a hold stops the robot.
//
// Any other phase is left untouched: a Pending action is not executing and has nothing to
// hold (estop-behaviour table, `Pending` → no change), and a terminal one is already settled.
func (r *FleetActionReconciler) applyPauseTransition(
	ctx context.Context, action, original *fleetv1.FleetAction,
	robot *fleetv1.Robot, robotFound bool, message string,
	now time.Time, leaseDur, leaseRenew time.Duration, intake pauseIntake,
) (ctrl.Result, error) {
	switch action.Status.Phase {
	case fleetv1.ActionPhaseAssigned:
		if intake == pauseUnderStop {
			return r.pauseAssignedUnderStop(ctx, action, original, message, now, leaseDur, leaseRenew)
		}
		robotOff := intake == pauseStopConfirmed
		if intake == pauseCancelRobot && robotFound && r.Commander != nil {
			ok, disp := r.pushCancel(ctx, action, action.Status.AssignedRobot,
				unattributedCancelIntent("desiredState=Paused", command.CancelCauseUnspecified))
			robotOff = ok && disp != command.CancelStopUnconfirmed
			message = pausedMessageWithStop(message, ok, disp, false)
		}
		// The record of a robot a stop covers is kept even with proof: it links the work to the
		// stop that paused it, so the requeue check finds the robot and no other robot is given
		// the work while that stop stands (executorguard.KeepsRecord).
		if robotOff && robotFound && robot.Status.AssignedAction == action.Name {
			covered, err := r.stopCovering(ctx, robot)
			if err != nil {
				return ctrl.Result{}, err
			}
			robotOff = !executorguard.KeepsRecord(covered, false)
		}
		if robotOff && robotFound && robot.Status.AssignedAction == action.Name {
			robotOriginal := robot.DeepCopy()
			robot.Status.AssignedAction = ""
			if robot.Status.Phase == fleetv1.RobotPhaseInProgress || robot.Status.Phase == fleetv1.RobotPhaseAssigned {
				robot.Status.Phase = fleetv1.RobotPhaseIdle
			}
			if err := r.Status().Patch(ctx, robot, client.MergeFromWithOptions(robotOriginal, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, fmt.Errorf("releasing robot %s from the paused action: %w", robot.Name, err)
			}
		}
		action.Status.Phase = fleetv1.ActionPhasePaused
		action.Status.AssignedRobot = ""
		action.Status.LeaseExpiresAt = nil
		action.Status.Message = message
		// The robot never entered the zone for this action; free its slot. (An
		// InProgress→Paused robot is physically in-zone and keeps its slot.)
		r.releaseReservation(ctx, action.Namespace, action.Spec.Zone, action.Name)
		return ctrl.Result{}, r.patchActionStatus(ctx, action, original)

	case fleetv1.ActionPhaseInProgress:
		if intake == pauseCancelRobot && r.Commander != nil {
			ok, disp := r.pushCancel(ctx, action, action.Status.AssignedRobot,
				unattributedCancelIntent("desiredState=Paused", command.CancelCauseUnspecified))
			message = pausedMessageWithStop(message, ok, disp, true)
		}
		action.Status.Phase = fleetv1.ActionPhasePaused
		// The lease advances only while the robot has confirmed its stop (The lease). Otherwise
		// the horizon the robot was last given is kept: it is the one after which the robot has
		// stopped on its own lease, and the Paused action gets no wire renewal. Work a clear holds
		// (leaseStampHeld) keeps it under a stop confirmed after the clear too.
		if (robotFound && stopConfirmed(robot) && !leaseStampHeld(action)) || action.Status.LeaseExpiresAt == nil {
			action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDur)}
		}
		action.Status.Message = message
		return ctrl.Result{RequeueAfter: leaseRenew},
			r.patchActionStatus(ctx, action, original)
	}
	return ctrl.Result{}, nil
}

// pausedMessageWithStop appends to a declarative hold's message what the robot's adapter said
// to the cancel the hold sent, and what happens next: the stop an operator reading the object
// needs to know about. bound is true for an InProgress action, which stays bound to its robot.
//
// A bound action's cancel is sent once and not repeated, and its lease is renewed on the server
// only, never on the wire, so a robot that did not act on the cancel stops on its own lease; the
// action stays bound until an operator requeues or cancels it. An Assigned action's binding is
// released and the robot keeps its record: the executor guard sends the cancel again and releases
// the record on the adapter's acknowledgement, or, once the action is requeued, when its lease is
// provably dead.
func pausedMessageWithStop(message string, acknowledged bool, disp command.CancelDisposition, bound bool) string {
	unconfirmed := !acknowledged || disp == command.CancelStopUnconfirmed
	if unconfirmed {
		said := "has not acknowledged it"
		if acknowledged {
			said = "its adapter could not witness its rest"
		}
		if bound {
			return message + "; the robot was told to stop and " + said + ": the cancel is sent once and not " +
				"repeated, and the lease is no longer renewed to the robot, so it stops on its own lease; the action " +
				"stays bound to it until an operator requeues or cancels it"
		}
		return message + "; the robot was told to stop and " + said + ": it keeps its record of the action, and " +
			"the cancel is sent again, until its adapter acknowledges it or, once the action is requeued, its " +
			"lease is provably dead"
	}
	switch disp {
	case command.CancelCompleted:
		return message + "; the robot was told to stop and reported the action complete"
	default:
		return message + "; the robot was told to stop and acknowledged it"
	}
}

// pauseAssignedUnconfirmed pauses an Assigned action whose robot is under a stop that is not
// confirmed: the robot's estopState is not Stopped (the stop is in flight, was never
// delivered and ended Failed, or is recorded and not yet written). The assignment was pushed,
// so the robot may have started on it and may still be moving; releasing it now would let a
// requeue give the work to a second robot while the first may still run it (RFC-0001
// #safety-assignment-lease-and-the-single-executor-guarantee). So the binding, the robot's
// status.assignedAction, the zone slot and the lease horizon are kept, and the lease is not
// renewed, as for a requeue on an unconfirmed stop (finalizeStopUnconfirmed). Once the lease
// has provably expired it waits for a person (WaitsForAPerson), or, once the stop is confirmed,
// stays Paused and bound (row A1); only a robot that is gone releases it. A requeue or a cancel
// before then goes through the confirmed-stop gate and waits for the same proof.
func (r *FleetActionReconciler) pauseAssignedUnconfirmed(ctx context.Context, action, original *fleetv1.FleetAction,
	now time.Time, leaseDur, renew, skew time.Duration) (ctrl.Result, error) {
	action.Status.Phase = fleetv1.ActionPhasePaused
	if action.Status.LeaseExpiresAt == nil {
		action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDur)}
	}
	if !leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), now, skew) {
		action.Status.Message = estopPausedUnconfirmedMessage
		if err := r.patchActionStatus(ctx, action, original); err != nil {
			return ctrl.Result{}, fmt.Errorf("pausing task bound to a robot whose stop is not confirmed: %w", err)
		}
		return ctrl.Result{RequeueAfter: min(renew, untilLeaseHorizon(action.Status.LeaseExpiresAt, now, renew, skew))}, nil
	}
	robot := &fleetv1.Robot{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Name: action.Status.AssignedRobot, Namespace: action.Namespace}, robot); err == nil {
		stops, confirmed, err := r.stopOver(ctx, robot)
		if err != nil {
			return ctrl.Result{}, err
		}
		if executorguard.WaitsForAPerson(stops, confirmed, true, heldByOperator(action)) {
			return r.waitForAPerson(ctx, action, original, robot.Name, stops, renew)
		}
		// The stop is confirmed now, or cleared: the action stays Paused and bound, with its
		// slot and the robot's record (row A1). Only a row out of Paused moves it; a requeue or
		// a cancel then has the dead lease as its proof.
		log.FromContext(ctx).Info("Paused action stays bound: its robot's stop is confirmed or cleared",
			"task", action.Name, "robot", robot.Name, "stops", stops)
		action.Status.Message = estopPausedAssignedMessage
		return ctrl.Result{RequeueAfter: renew}, r.patchActionStatus(ctx, action, original)
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("fetching assigned robot: %w", err)
	}
	log.FromContext(ctx).Info("Paused action released from its robot: the robot is gone and the lease has provably expired",
		"task", action.Name, "robot", action.Status.AssignedRobot)
	action.Status.AssignedRobot = ""
	action.Status.LeaseExpiresAt = nil
	action.Status.Message = estopPausedAssignedMessage
	r.releaseReservation(ctx, action.Namespace, action.Spec.Zone, action.Name)
	return ctrl.Result{}, r.patchActionStatus(ctx, action, original)
}

// handleRequeue drives a forcible requeue (RFC-0001 #zonemaintenance-immediate-mode-step-by-step ZoneMaintenance Immediate). It
// applies the SAME confirmed-stop gate as handleCancel, but returns the action to
// Pending (re-schedulable) instead of terminating it.
// beginCapabilityLossReassignment marks a reachable, capability-degraded robot's
// in-flight action for the confirmed-stop requeue and emits ActionReassignedCapabilityLoss.
// The existing handleRequeue path then drives cancel_action → confirmed stop → Pending
// (single-executor preserved). Idempotent: a action already marked for requeue is left
// to that path. The Failed-via-recovery and complete-then-release outcomes arrive with
// the adapter CancelActionResult disposition (a later change); until then a safe-stop
// hand-off requeues, and a robot that cannot confirm a stop is held awaiting provable
// lease death, exactly as the requeue path already does.
func (r *FleetActionReconciler) beginCapabilityLossReassignment(ctx context.Context, action *fleetv1.FleetAction, robotName string) (ctrl.Result, error) {
	if _, already := action.Annotations[annRequeueRequested]; already {
		return ctrl.Result{Requeue: true}, nil
	}
	base := action.DeepCopy()
	if action.Annotations == nil {
		action.Annotations = map[string]string{}
	}
	action.Annotations[annRequeueRequested] = reasonCapabilityLost
	if err := r.Patch(ctx, action, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking task for capability-loss requeue: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(action, corev1.EventTypeWarning, "ActionReassignedCapabilityLoss",
			"assigned robot %s no longer satisfies required capabilities; reassigning", robotName)
	}
	log.FromContext(ctx).Info("capability-loss reassignment initiated", "task", action.Name, "robot", robotName)
	return ctrl.Result{Requeue: true}, nil
}

// requeueVerdict is what a stop on the bound robot makes of a standing requeue request.
type requeueVerdict int

const (
	// requeueRuns: no stop holds the bound robot, or the action is already Paused (the
	// request is the operator's decision on held work); handleRequeue runs as before.
	requeueRuns requeueVerdict = iota
	// requeueDropped: the stop overtook the request on an Assigned or InProgress action; the
	// annotation was removed and the estop pause runs on the next reconcile.
	requeueDropped
	// requeueWaits: a Revoking action held under the stop, or a Paused one whose request is
	// already held; the request stays and is not run until the hold is released.
	requeueWaits
	// requeueHeld: a Paused action whose request was asked before the stop that covers its robot
	// now; the request is held while that stop stands, and the operator is told.
	requeueHeld
)

// Messages of a requeue waiting on a Paused action. They are the record of when the request
// was asked: the first pass that handles it writes whether a stop covered the robot then, and a
// request asked before a stop is held while that stop stands.
const (
	requeueUnderStopPrefix  = "requeue requested while the emergency stop of "
	requeueHeldPrefix       = "requeue requested before the emergency stop of "
	requeueWithdrawnMessage = "paused — the requeue request was withdrawn; operator requeue/cancel required"
)

// requeueUnderStopMessage is the wait message of a requeue asked while stops covered the robot.
func requeueUnderStopMessage(stops []string) string {
	return requeueUnderStopPrefix + strings.Join(stops, ", ") + " stood — awaiting confirmed stop"
}

// requeueHeldMessage is the message of a requeue asked before stops that now cover the robot.
func requeueHeldMessage(stops []string) string {
	return requeueHeldPrefix + strings.Join(stops, ", ") + " reached the robot — held while that stop stands, " +
		"and carried out once it is cleared"
}

// isRequeueWaitMessage reports whether msg is the message of a requeue that waits.
func isRequeueWaitMessage(msg string) bool {
	return msg == requeuingMessage || strings.HasPrefix(msg, requeueUnderStopPrefix) || strings.HasPrefix(msg, requeueHeldPrefix)
}

// requeueOvertakenByEstop decides whether a requeue request may run while a stop holds the
// bound robot, and the message it waits under. A request on an action the stop has not yet paused was made before the stop:
// the window that wrote it, or the operator who did, decided about running work, not about
// work an estop interrupted. Running it would move that work off its stopped robot with no
// operator decision (RFC-0001 #safety-fleetaction-behaviour-during-estop: "NOT automatically
// requeued"), so it is dropped and the action is paused. A Warning Event names the drop, so
// an operator who wanted the requeue knows to ask again once the work is inspected.
func (r *FleetActionReconciler) requeueOvertakenByEstop(ctx context.Context, action *fleetv1.FleetAction) (requeueVerdict, string, error) {
	phase := action.Status.Phase
	if phase == fleetv1.ActionPhasePaused {
		return r.requeueOfPausedWork(ctx, action)
	}
	if action.Status.AssignedRobot == "" ||
		(phase != fleetv1.ActionPhaseAssigned && phase != fleetv1.ActionPhaseInProgress && phase != fleetv1.ActionPhaseRevoking) {
		return requeueRuns, requeuingMessage, nil
	}
	robot := &fleetv1.Robot{}
	if err := r.Get(ctx, types.NamespacedName{Name: action.Status.AssignedRobot, Namespace: action.Namespace}, robot); err != nil {
		if errors.IsNotFound(err) {
			return requeueRuns, requeuingMessage, nil
		}
		return requeueRuns, "", fmt.Errorf("fetching assigned robot: %w", err)
	}
	// The stops are read from the API server through the stop guard, so a stop issued over the
	// robot counts before its fan-out writes it onto the robot.
	stops, confirmed, err := r.stopOver(ctx, robot)
	if err != nil {
		return requeueRuns, "", err
	}
	if phase == fleetv1.ActionPhaseRevoking {
		if len(stops) == 0 && !estopHoldsWork(robot) {
			return requeueRuns, requeuingMessage, nil
		}
		// Work that waits for a person under a stop the robot never confirmed: the request is that
		// person's decision if it was asked under the stop, which the requeue check admitted only to
		// estop-clear on it. One asked before the stop is held while the stop stands.
		_, _, skew := leaseTimings(ctx, r.Client, action.Namespace)
		if executorguard.WaitsForAPerson(stops, confirmed, leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), time.Now(), skew),
			false) {
			verdict, msg := requeueAskedUnder(action, stops)
			return verdict, msg, nil
		}
		return requeueWaits, "", nil
	}
	if len(stops) == 0 && !robotUnderEstop(robot) {
		return requeueRuns, requeuingMessage, nil
	}
	reason := action.Annotations[annRequeueRequested]
	if err := r.removeActionAnnotation(ctx, action, annRequeueRequested); err != nil {
		return requeueRuns, "", fmt.Errorf("dropping a requeue request the estop overtook: %w", err)
	}
	log.FromContext(ctx).Info("requeue request dropped: an emergency stop on the robot overtook it; pausing",
		"task", action.Name, "robot", robot.Name, "reason", reason)
	if r.Recorder != nil {
		r.Recorder.Eventf(action, corev1.EventTypeWarning, "RequeueOvertakenByEstop",
			"requeue request %q not carried out: robot %s is under an emergency stop, so the action is paused; "+
				"an operator requeues or cancels it once the work is inspected", reason, robot.Name)
	}
	return requeueDropped, "", nil
}

// requeueOfPausedWork decides a requeue request on a Paused action. The stops that cover the
// robot holding the work (the bound robot, or every robot that records the action) are read
// from the API server. With none, the request runs. With some, it runs if it was asked while
// those same stops stood: the requeue check admitted it only to their own authority, and the
// first pass that handled it wrote so in the wait message. A request the controller had already
// seen with no stop, or under other stops, was asked before them: it is held while they stand
// (RFC-0001 #safety-fleetaction-behaviour-during-estop). A request seen for the first time
// under a stop is taken as asked under it.
func (r *FleetActionReconciler) requeueOfPausedWork(ctx context.Context, action *fleetv1.FleetAction) (requeueVerdict, string, error) {
	stops, err := r.stopsOverWork(ctx, action)
	if err != nil {
		return requeueRuns, "", err
	}
	if len(stops) == 0 {
		return requeueRuns, requeuingMessage, nil
	}
	verdict, msg := requeueAskedUnder(action, stops)
	return verdict, msg, nil
}

// requeueAskedUnder decides a requeue request on work stops hold from when it was asked, which the
// action's message records: it runs if it was asked while those same stops stood, or is seen for the
// first time under them; one already seen with no stop, or under other stops, is held while they stand.
func requeueAskedUnder(action *fleetv1.FleetAction, stops []string) (requeueVerdict, string) {
	under, msg := requeueUnderStopMessage(stops), action.Status.Message
	switch {
	case executorguard.PausedRequeueRuns(true, isRequeueWaitMessage(msg), msg == under):
		return requeueRuns, under
	case msg == requeueHeldMessage(stops):
		return requeueWaits, ""
	default:
		return requeueHeld, requeueHeldMessage(stops)
	}
}

// keepRequeueWait is msg, or the action's message while that message records when a standing
// requeue request was asked: requeueAskedUnder decides from it, so no other hold rewrites it while
// the request stands.
func keepRequeueWait(action *fleetv1.FleetAction, msg string) string {
	if _, requested := action.Annotations[annRequeueRequested]; requested && isRequeueWaitMessage(action.Status.Message) {
		return action.Status.Message
	}
	return msg
}

// stopsOverWork names the stops that cover the robot action is bound to or, with no binding,
// every robot that records it; sorted, read from the API server.
func (r *FleetActionReconciler) stopsOverWork(ctx context.Context, action *fleetv1.FleetAction) ([]string, error) {
	var robots []fleetv1.Robot
	if action.Status.AssignedRobot != "" {
		rb := fleetv1.Robot{}
		if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: action.Namespace, Name: action.Status.AssignedRobot}, &rb); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		robots = append(robots, rb)
	} else {
		var list fleetv1.RobotList
		if err := r.liveReader().List(ctx, &list, client.InNamespace(action.Namespace)); err != nil {
			return nil, fmt.Errorf("listing robots for a requeue under a stop: %w", err)
		}
		for i := range list.Items {
			if list.Items[i].Status.AssignedAction == action.Name {
				robots = append(robots, list.Items[i])
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for i := range robots {
		names, err := stopNames(ctx, r.liveReader(), &robots[i])
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// holdRequeueUnderEstop holds a requeue asked before the stop that now covers the robot: the
// action keeps its phase, the message says the request is held and why, and a Warning Event
// tells the operator once. The Paused hold applies on the next pass.
func (r *FleetActionReconciler) holdRequeueUnderEstop(ctx context.Context, action, original *fleetv1.FleetAction, msg string) (ctrl.Result, error) {
	action.Status.Message = msg
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return ctrl.Result{}, fmt.Errorf("holding a requeue under a stop: %w", err)
	}
	log.FromContext(ctx).Info("requeue request held: it was asked before the stop that now covers the robot",
		"task", action.Name, "robot", action.Status.AssignedRobot)
	warnActionAndTask(r.Recorder, action, EventRequeueHeldByEstop, msg)
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// stopOver reads robot from the API server and reports the stops that cover it and whether it
// has confirmed its stop (its estop state is Stopped).
func (r *FleetActionReconciler) stopOver(ctx context.Context, robot *fleetv1.Robot) ([]string, bool, error) {
	live := &fleetv1.Robot{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Namespace: robot.Namespace, Name: robot.Name}, live); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading robot %s for its stop: %w", robot.Name, err)
	}
	stops, err := stopNames(ctx, r.liveReader(), live)
	if err != nil {
		return nil, false, err
	}
	return stops, stopConfirmed(live), nil
}

// stopsOverBoundRobot reads from the API server the stops that hold the work of the robot action is
// bound to, and whether that robot has confirmed its stop. While the robot's Robot object exists
// they are the stops over it (stopNames), a stop issued and not yet written onto it included. Once
// the object is gone (gone), nothing records where the robot stands: its zone and its recorded
// holds went with the object, and the action records neither. Every zone in the namespace that
// carries a stop, issued or in force, and every namespace stop then holds the work, since the robot
// may stand under any of them (RFC-0001 #safety-action-table, invariant I1).
func (r *FleetActionReconciler) stopsOverBoundRobot(ctx context.Context, action *fleetv1.FleetAction) (stops []string, confirmed, gone bool, err error) {
	live := &fleetv1.Robot{}
	err = r.liveReader().Get(ctx, types.NamespacedName{Namespace: action.Namespace, Name: action.Status.AssignedRobot}, live)
	switch {
	case err == nil:
		if stops, err = stopNames(ctx, r.liveReader(), live); err != nil {
			return nil, false, false, err
		}
		return stops, stopConfirmed(live), false, nil
	case !errors.IsNotFound(err):
		return nil, false, false, fmt.Errorf("reading robot %s for its stop: %w", action.Status.AssignedRobot, err)
	}
	t, configs, err := estopscope.ReadWorld(ctx, r.liveReader(), action.Namespace)
	if err != nil {
		return nil, false, true, err
	}
	for _, name := range t.Names() {
		if estopscope.Carries(t.Zone(name)) {
			stops = append(stops, string(estopscope.KindZone)+"/"+name)
		}
	}
	for i := range configs {
		if estopscope.InForce(configs[i].Annotations) {
			stops = append(stops, string(estopscope.KindNamespace)+"/"+configs[i].Name)
		}
	}
	sort.Strings(stops)
	return stops, false, true, nil
}

// holdUnderUnconfirmedStop keeps a Paused action bound to a robot whose stop is not confirmed:
// the lease is not renewed, so the horizon the robot was last given stands, and once it is
// provably dead the work waits for a person (waitForAPerson). A missing horizon is set one lease
// duration ahead, which bounds any lease the robot could hold.
func (r *FleetActionReconciler) holdUnderUnconfirmedStop(ctx context.Context, action, original *fleetv1.FleetAction,
	robot string, stops []string, now time.Time, leaseDur, renew, skew time.Duration) (ctrl.Result, error) {
	if action.Status.LeaseExpiresAt == nil {
		action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDur)}
		if err := r.patchActionStatus(ctx, action, original); err != nil {
			return ctrl.Result{}, fmt.Errorf("bounding the lease of a robot whose stop is not confirmed: %w", err)
		}
		return ctrl.Result{RequeueAfter: renew}, nil
	}
	if executorguard.WaitsForAPerson(stops, false, leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), now, skew),
		heldByOperator(action)) {
		return r.waitForAPerson(ctx, action, original, robot, stops, renew)
	}
	return ctrl.Result{RequeueAfter: min(renew, untilLeaseHorizon(action.Status.LeaseExpiresAt, now, renew, skew))}, nil
}

// waitForAPerson keeps a robot's work with that robot when the robot is under a stop it has not
// confirmed and its lease is provably dead. Under a stop the work moves only on the robot's own end
// report or an operator's decision (RFC-0001 #safety-how-the-two-interact, invariant I1): the robot
// may still be moving, and it may hold the work's payload, which a robot given the work afresh would
// not find. That holds for work it never reported started too, since a running report can be lost.
// The action keeps its phase and binding; the message says what an operator decides, and a Warning
// Event on the action and its FleetTask says it once. The lease is not renewed, so a requeue an
// operator asks completes on the dead lease.
func (r *FleetActionReconciler) waitForAPerson(ctx context.Context, action, original *fleetv1.FleetAction,
	robot string, stops []string, renew time.Duration) (ctrl.Result, error) {
	why := "it had started this action and may hold its payload"
	if action.Status.StartedAt == nil {
		why = "it never reported this action started; its running report may have been lost, and it may hold its payload"
	}
	msg := keepRequeueWait(action, awaitingDecision(action, stops, fmt.Sprintf("robot %s did not confirm the stop and "+
		"its lease is provably dead, but %s; inspect the robot first", robot, why)))
	if action.Status.Message == msg {
		return ctrl.Result{RequeueAfter: renew}, nil
	}
	action.Status.Message = msg
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return ctrl.Result{}, fmt.Errorf("holding work under an unconfirmed stop: %w", err)
	}
	log.FromContext(ctx).Info("work held for an operator: the robot's stop is not confirmed and its lease is dead",
		"task", action.Name, "robot", robot, "stops", stops, "started", action.Status.StartedAt != nil)
	warnActionAndTask(r.Recorder, action, EventActionAwaitingOperatorDecision, msg)
	return ctrl.Result{RequeueAfter: renew}, nil
}

// heldByOperator reports whether an operator holds action through spec.desiredState (Paused or
// Returning) and has asked no requeue: the hold is the operator's decision, and the work stays.
func heldByOperator(action *fleetv1.FleetAction) bool {
	_, requeue := action.Annotations[annRequeueRequested]
	return !requeue && (action.Spec.DesiredState == fleetv1.DesiredStatePaused ||
		action.Spec.DesiredState == fleetv1.DesiredStateReturning)
}

// holdRevokingUnderEstop keeps a Revoking action at Revoking while its robot is under a stop
// (estopHoldsWork). The lease is neither renewed nor evaluated, so the action is not
// re-adopted and not released to Pending; the robot self-stops on the lease it already holds.
func (r *FleetActionReconciler) holdRevokingUnderEstop(ctx context.Context, action, original *fleetv1.FleetAction, renew time.Duration) (ctrl.Result, error) {
	if msg := keepRequeueWait(action, estopHeldRevokingMessage); action.Status.Message != msg {
		log.FromContext(ctx).Info("Revoking action held: its robot is under an emergency stop",
			"task", action.Name, "robot", action.Status.AssignedRobot)
		action.Status.Message = msg
		if err := r.patchActionStatus(ctx, action, original); err != nil {
			return ctrl.Result{}, fmt.Errorf("holding revoking task under estop: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: renew}, nil
}

func (r *FleetActionReconciler) handleRequeue(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction, waitMsg string) (ctrl.Result, error) {
	reason := action.Annotations[annRequeueRequested]
	robotName := action.Status.AssignedRobot

	if robotName == "" {
		return ctrl.Result{}, r.finalizeRequeue(ctx, action, original, robotName, reason)
	}

	// A capability-loss requeue cancels on the wire with its own cause; other requeue
	// reasons (ZoneMaintenance Immediate) state none. Neither is a supersession: a
	// requeued action returns to Pending and is still work the fleet owes.
	requeueCause := command.CancelCauseUnspecified
	if reason == reasonCapabilityLost {
		requeueCause = command.CancelCauseCapabilityLoss
	}
	confirmed, disp := r.confirmedStopWithDisposition(ctx, action, robotName,
		unattributedCancelIntent(reason, requeueCause))
	if disp == command.CancelStopUnconfirmed {
		// Acknowledged, rest unwitnessed: the requeue is not carried out. The action fails
		// now and keeps its robot until the lease has provably expired.
		return ctrl.Result{}, r.finalizeStopUnconfirmed(ctx, action, original, robotName, reason)
	}
	if !confirmed {
		return r.holdStop(ctx, action, original, waitMsg)
	}

	// Capability-loss reassignment: the adapter's disposition selects the outcome
	// once the stop is confirmed. Other requeue reasons (ZoneMaintenance Immediate)
	// always requeue to Pending, exactly as before.
	if reason == reasonCapabilityLost {
		switch disp {
		case command.CancelRecovered:
			// Robot could not safely hand off; it recovered (load returned / to base).
			return ctrl.Result{}, r.finalizeCapabilityLossFailure(ctx, action, original, robotName)
		case command.CancelCompleted:
			// Robot finished the action; the cancel is moot — drop the annotation and let
			// the normal ActionStatusUpdate → Succeeded path settle it.
			return ctrl.Result{}, r.clearRequeueAnnotation(ctx, action)
		}
	}
	return ctrl.Result{}, r.finalizeRequeue(ctx, action, original, robotName, reason)
}

// confirmedStop reports whether the robot bound to action is provably not executing.
// Thin bool wrapper over confirmedStopWithDisposition for the cancel path (which
// does not branch on disposition).
func (r *FleetActionReconciler) confirmedStop(ctx context.Context, action *fleetv1.FleetAction, robotName string, intent cancelIntent) bool {
	ok, _ := r.confirmedStopWithDisposition(ctx, action, robotName, intent)
	return ok
}

// confirmedStopWithDisposition reports whether the robot is provably not executing
// (adapter acknowledged a cancel_action, OR the lease is provably dead — a nil horizon
// is NOT proof, RA-4), and, when confirmed via the adapter, how it handled the
// cancel. This is the single-executor gate shared by cancel and requeue — a robot is
// freed only when confirmed is true. The lease-death path reports StoppedSafely.
//
// An acknowledgement carrying CancelStopUnconfirmed is NOT proof: the adapter commanded the
// stop and could not witness rest, so it does not satisfy condition 1 of the
// safe-reassignment invariant. It is reported (false, CancelStopUnconfirmed) while the lease
// lives, and (true, CancelStopUnconfirmed) once the lease has provably expired, so every
// caller that waits for a confirmed stop waits for that expiry, and the requeue path can
// still tell an unwitnessed stop from a safe one.
func (r *FleetActionReconciler) confirmedStopWithDisposition(ctx context.Context, action *fleetv1.FleetAction, robotName string, intent cancelIntent) (bool, command.CancelDisposition) {
	unconfirmed := false
	if r.Commander != nil {
		ok, disp := r.pushCancel(ctx, action, robotName, intent)
		if ok && disp != command.CancelStopUnconfirmed {
			return true, disp
		}
		unconfirmed = ok
	}
	_, _, skew := leaseTimings(ctx, r.Client, action.Namespace)
	dead := leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), time.Now(), skew)
	if unconfirmed {
		return dead, command.CancelStopUnconfirmed
	}
	return dead, command.CancelStoppedSafely
}

// holdStop keeps a not-yet-stopped action in its current phase with a "awaiting
// confirmed stop" message and requeues — it never frees the robot.
func (r *FleetActionReconciler) holdStop(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction, msg string) (ctrl.Result, error) {
	if action.Status.Message != msg {
		action.Status.Message = msg
		if err := r.patchActionStatus(ctx, action, original); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking task awaiting stop: %w", err)
		}
	}
	_, renew, _ := leaseTimings(ctx, r.Client, action.Namespace)
	return ctrl.Result{RequeueAfter: renew}, nil
}

// pushCancel pushes cancel_action and reports whether the adapter CONFIRMED the
// cancellation (a provable stop) and, if so, its disposition. Unreachable/declined/
// stale is not confirmed.
func (r *FleetActionReconciler) pushCancel(ctx context.Context, action *fleetv1.FleetAction, robotName string, intent cancelIntent) (bool, command.CancelDisposition) {
	wctx, cancel := context.WithTimeout(ctx, wireTimeout)
	defer cancel()
	out, err := r.Commander.PushCancelAction(wctx, action.Namespace, robotName, command.CancelAction{
		ActionID:     action.Name,
		Reason:       intent.reason,
		FencingToken: command.FencingToken(action.Status.AssignmentGeneration),
		// The machine-readable cause travels with the cancel so the adapter can echo
		// the matching ActionEndCause on its terminal report. Without it every cancel
		// looked identical on the wire and a supersession was indistinguishable from
		// an operator stop at the far end.
		Cause: intent.cause,
	})
	if err != nil {
		log.FromContext(ctx).V(1).Info("cancel_action not confirmed via wire (holding until provable lease death)",
			"task", action.Name, "robot", robotName, "reason", err.Error())
		return false, command.CancelStoppedSafely
	}
	return out.Acknowledged, out.Disposition
}

// finalizeCancel terminalises a action to Cancelled: it releases the robot binding
// (→ Idle) if the robot still claims this action, clears the lease, releases the TDE
// slot, and records the completion. Called ONLY once the robot is provably stopped.
func (r *FleetActionReconciler) finalizeCancel(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction, robotName string, intent cancelIntent) error {
	if err := r.releaseBoundUnlessCovered(ctx, action, robotName); err != nil {
		return err
	}
	msg := "cancelled by operator"
	if intent.reason != "" && intent.reason != "true" {
		msg = "cancelled: " + intent.reason
	}
	action.Status.Phase = fleetv1.ActionPhaseCancelled
	action.Status.AssignedRobot = ""
	action.Status.LeaseExpiresAt = nil
	action.Status.Message = msg
	action.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	applyCancelAttribution(action, intent)
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return fmt.Errorf("finalising cancel: %w", err)
	}
	r.releaseReservation(ctx, action.Namespace, action.Spec.Zone, action.Name)
	r.emitCancelEvent(action, intent)
	r.recordActionCancelled(ctx, action, intent)
	return nil
}

// applyCancelAttribution writes the structural half of "which of the two events was
// this" onto status: the reason, the superseding action, and the Superseded condition.
//
// Phase is `Cancelled` for both a supersession and an operator cancellation, and
// RFC-0001 #fleetaction-directive-supersession is explicit that an implementation MUST
// NOT let phase try to tell them apart. These fields are what does. supersededBy in
// particular is what makes the distinction STRUCTURAL rather than a matter of
// labelling: an operator cancellation has no superseding action and can never populate
// it, so a supersession that left it empty would be malformed.
func applyCancelAttribution(action *fleetv1.FleetAction, intent cancelIntent) {
	action.Status.CancellationReason = intent.cancellationReason
	action.Status.SupersededBy = intent.supersededBy
	if intent.conditionReason == "" {
		return
	}
	apimeta.SetStatusCondition(&action.Status.Conditions, metav1.Condition{
		Type:    ConditionSuperseded,
		Status:  metav1.ConditionTrue,
		Reason:  intent.conditionReason,
		Message: intent.reason,
	})
}

// emitCancelEvent emits the distinguishable Kubernetes Event. An operator watching a
// namespace sees `ActionCancelledByOperator` or one of the two supersession reasons —
// never a single undifferentiated "cancelled", which is what the phase alone would
// have given them.
func (r *FleetActionReconciler) emitCancelEvent(action *fleetv1.FleetAction, intent cancelIntent) {
	if r.Recorder == nil || intent.eventReason == "" {
		return
	}
	// Normal, not Warning: a supersession is the fleet working as designed, and an
	// operator cancellation is a deliberate act. Neither is a fault, and grading them
	// Warning would train operators to ignore the channel that carries real faults.
	r.Recorder.Event(action, corev1.EventTypeNormal, intent.eventReason, intent.reason)
}

// recordActionCancelled seals a FLEETACTION_CANCELLED entry into the RFC-0001 #security-audit-logging chain.
// Best-effort: a record failure is logged, never blocking the (already-finalized)
// cancellation.
func (r *FleetActionReconciler) recordActionCancelled(ctx context.Context, action *fleetv1.FleetAction, intent cancelIntent) {
	if r.Audit == nil {
		return
	}
	detail := map[string]string{}
	if intent.reason != "" && intent.reason != "true" {
		detail["reason"] = intent.reason
	}
	// A supersession is a different audit event from a cancellation, not the same
	// event with different text: an auditor reconstructing "who stopped this work"
	// must be able to separate the fleet replacing it from a person halting it.
	eventType := audit.EventFleetActionCancelled
	if intent.supersededAudit {
		eventType = audit.EventFleetActionSuperseded
		if intent.supersededBy != "" {
			detail["supersededBy"] = intent.supersededBy
		}
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: eventType,
		Namespace: action.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "fleetaction-controller"},
		Resource:  audit.Resource{Kind: "FleetAction", Namespace: action.Namespace, Name: action.Name},
		Action:    "cancel",
		Outcome:   audit.OutcomeAllowed,
		Detail:    detail,
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording FLEETACTION_CANCELLED audit entry", "task", action.Name)
	}
}

// finalizeRequeue returns a action to Pending (re-schedulable) once its robot is
// provably stopped. It releases the robot binding, lease, and TDE slot — exactly
// like a cancel — but keeps the action alive: no CompletionTime, and
// assignmentGeneration is PRESERVED so the next assignment is strictly greater
// (never reused). The requeue annotation is cleared so the re-scheduled action is
// not requeued again.
func (r *FleetActionReconciler) finalizeRequeue(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction, robotName, reason string) error {
	if err := r.releaseRobotForStop(ctx, action, robotName); err != nil {
		return err
	}
	msg := "requeued for zone maintenance"
	if reason != "" && reason != "true" {
		msg = "requeued: " + reason
	}
	action.Status.Phase = fleetv1.ActionPhasePending
	action.Status.AssignedRobot = ""
	action.Status.LeaseExpiresAt = nil
	action.Status.Message = msg
	// The re-entry into Pending is the executor guard's reference for a horizon it does not know;
	// without it the reference falls back to the action's creation, which can be long before the
	// last lease the previous robot was granted.
	reentered := metav1.Now()
	action.Status.PendingSince = &reentered
	// assignmentGeneration deliberately preserved (read-before-issue at next commit).
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return fmt.Errorf("finalising requeue: %w", err)
	}
	// A requeue is the one edit that moves work a stop paused, and the requeue check admitted it
	// only to the stop's own authority: a robot the stop covers that still records the action is
	// released here on proof (an acknowledged cancel, or a lease provably dead; a confirmed stop is
	// not proof). One that cannot be proved off it now keeps its record, and dispatch waits for the
	// stop's clear.
	_, _, skew := leaseTimings(ctx, r.Client, action.Namespace)
	if _, err := r.recordersDecision(ctx, action, time.Now(), skew, true); err != nil {
		return err
	}
	r.releaseReservation(ctx, action.Namespace, action.Spec.Zone, action.Name)

	// Clear the requeue annotation so the re-scheduled action is not requeued again.
	if err := r.removeActionAnnotation(ctx, action, annRequeueRequested); err != nil {
		return fmt.Errorf("clearing requeue annotation: %w", err)
	}
	return nil
}

// finalizeCapabilityLossFailure fails a action whose mid-commitment robot could not
// safely hand off and was recovered by the adapter (RFC-0001 Capability-loss
// reassignment, Recovery outcome). It transitions to Failed with reason
// CapabilityLostDuringExecution and stamps FailedAt, then clears the requeue
// annotation so the onFailure contract (handleFailedAction) — not the requeue path —
// governs the next step (default Requeue reschedules to a capable robot). The robot
// binding and TDE slot are freed by handleFailedAction on the next reconcile.
func (r *FleetActionReconciler) finalizeCapabilityLossFailure(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction, robotName string) error {
	now := time.Now()
	action.Status.Phase = fleetv1.ActionPhaseFailed
	action.Status.FailureReason = reasonCapabilityLostDuringExecution
	action.Status.FailedAt = &metav1.Time{Time: now}
	action.Status.Message = "adapter recovered the robot after capability loss; task failed"
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return fmt.Errorf("finalising capability-loss failure: %w", err)
	}

	if err := r.removeActionAnnotation(ctx, action, annRequeueRequested); err != nil {
		return fmt.Errorf("clearing requeue annotation: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(action, corev1.EventTypeWarning, "ActionRecoveredCapabilityLoss",
			"robot %s recovered after capability loss; task failed (%s)", robotName, reasonCapabilityLostDuringExecution)
	}
	return nil
}

// finalizeStopUnconfirmed fails an action whose requeue cancel the adapter acknowledged
// CANCEL_DISPOSITION_STOP_UNCONFIRMED, and keeps its robot bound. The binding, the zone slot
// and status.leaseExpiresAt are left as they are, so handleFailedAction holds them until
// the lease has provably expired; a nil horizon is stamped one lease duration from now,
// which bounds any lease the robot could hold (every grant and renewal was sent before
// now). The requeue annotation is cleared: the requeue was not carried out.
func (r *FleetActionReconciler) finalizeStopUnconfirmed(ctx context.Context, action, original *fleetv1.FleetAction, robotName, reason string) error {
	now := time.Now()
	if action.Status.LeaseExpiresAt == nil {
		leaseDur, _, _ := leaseTimings(ctx, r.Client, action.Namespace)
		action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDur)}
	}
	action.Status.Phase = fleetv1.ActionPhaseFailed
	action.Status.FailureReason = reasonStopUnconfirmed
	action.Status.FailedAt = &metav1.Time{Time: now}
	action.Status.Message = fmt.Sprintf("requeue (%s) not carried out: the robot's stop was commanded and its rest "+
		"was not witnessed; %s stays bound until its lease has provably expired", reason, robotName)
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return fmt.Errorf("failing task on an unconfirmed stop: %w", err)
	}
	if err := r.removeActionAnnotation(ctx, action, annRequeueRequested); err != nil {
		return fmt.Errorf("clearing requeue annotation: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(action, corev1.EventTypeWarning, "ActionStopUnconfirmed",
			"robot %s acknowledged the cancel but could not witness its rest; the task failed and the robot "+
				"stays bound until its lease has provably expired (%s)", robotName, action.Status.LeaseExpiresAt.UTC().Format(time.RFC3339))
	}
	log.FromContext(ctx).Info("requeue cancel acknowledged with the stop unconfirmed; task failed, robot held",
		"task", action.Name, "robot", robotName, "leaseExpiresAt", action.Status.LeaseExpiresAt)
	return nil
}

// clearRequeueAnnotation drops the requeue annotation without changing phase: used
// when a capability-loss cancel found the robot had already COMPLETED the action, so
// the cancel is moot and the normal completion path settles the action.
func (r *FleetActionReconciler) clearRequeueAnnotation(ctx context.Context, action *fleetv1.FleetAction) error {
	if _, ok := action.Annotations[annRequeueRequested]; !ok {
		return nil
	}
	if err := r.removeActionAnnotation(ctx, action, annRequeueRequested); err != nil {
		return fmt.Errorf("clearing requeue annotation: %w", err)
	}
	log.FromContext(ctx).Info("capability-loss cancel: robot completed the task; no reassignment", "task", action.Name)
	return nil
}

// eventAssignmentDeclined is the Event reason for an assignment an adapter declined.
const eventAssignmentDeclined = "AssignmentDeclined"

// declineReasonLimit caps the adapter's text in an assignment-declined Event. The text is
// the adapter's, and an Event is not the place for an unbounded one.
const declineReasonLimit = 512

// recordAssignmentDeclined writes a Warning Event on the FleetAction and one on the Robot,
// naming the robot that declined, the rejection and the adapter's reason. A declined
// assignment is released, and status.message is cleared when another robot takes the
// action, so without these the robot that refused is visible only in the manager's log.
// Events expire with the API server's event TTL; they are for an operator looking soon
// after, not a history.
func (r *FleetActionReconciler) recordAssignmentDeclined(action *fleetv1.FleetAction, robot *fleetv1.Robot,
	rejection, adapterMessage string) {
	if r.Recorder == nil {
		return
	}
	reason := adapterMessage
	if reason == "" {
		reason = "the adapter gave no reason"
	} else if runes := []rune(reason); len(runes) > declineReasonLimit {
		reason = string(runes[:declineReasonLimit]) + "…"
	}
	msg := fmt.Sprintf("robot %q declined the assignment of FleetAction %q (%s): %s",
		robot.Name, action.Name, rejection, reason)
	r.Recorder.Event(action, corev1.EventTypeWarning, eventAssignmentDeclined, msg)
	r.Recorder.Event(robot, corev1.EventTypeWarning, eventAssignmentDeclined, msg)
}

// releaseRejectedAssignment rolls back a just-committed assignment the adapter
// rejected: the robot did not accept the action (so it is not executing), so the
// action returns to Pending — assignmentGeneration PRESERVED (next commit issues
// strictly-greater) — the robot is freed to Idle (guarded), the TDE slot is
// released, and a short backoff requeue lets the scheduler retry (possibly on
// another robot). Safe: an explicit rejection is a provable not-executing.
func (r *FleetActionReconciler) releaseRejectedAssignment(
	ctx context.Context, namespace string, action *fleetv1.FleetAction,
	robotName string, unhonourable []string,
) (ctrl.Result, error) {
	if res, held, err := r.holdRefusalUnderStop(ctx, action, robotName); held || err != nil {
		return res, err
	}
	base := action.DeepCopy() // the committed Assigned state
	action.Status.Phase = fleetv1.ActionPhasePending
	action.Status.AssignedRobot = ""
	action.Status.LeaseExpiresAt = nil
	action.Status.Message = "adapter rejected the assignment; rescheduling"
	// FA-R5's decline is the one rejection that names something, and naming it is the
	// whole of what makes the obligation observable rather than a claim. The robot is
	// named alongside the subject because the two send an operator to different places:
	// the SUBJECT says which zone rule is in play, the ROBOT says whose adapter
	// contradicted its own declaration — filter 12 admitted this candidate on that
	// declaration, so a decline here is the claim failing, not the rule changing.
	if len(unhonourable) > 0 {
		sorted := append([]string(nil), unhonourable...)
		sort.Strings(sorted)
		action.Status.Message = fmt.Sprintf(
			"robot %q declined the assignment: its adapter cannot honour the operating "+
				"subject(s) %s in force where it stands, though its declaration says it can. "+
				"The action is rescheduling to another candidate; declining costs the adapter "+
				"nothing", robotName, strings.Join(sorted, ", "))
	}
	if err := r.patchActionStatus(ctx, action, base); err != nil {
		return ctrl.Result{}, fmt.Errorf("releasing rejected assignment: %w", err)
	}
	if err := r.releaseRobotForStop(ctx, action, robotName); err != nil {
		return ctrl.Result{}, err
	}
	r.releaseReservation(ctx, namespace, action.Spec.Zone, action.Name)
	minRetry, _ := r.tdeRetryBounds(ctx, namespace)
	return ctrl.Result{RequeueAfter: minRetry}, nil
}

// holdRefusalUnderStop is row A24's rule for a refusal under a stop, a refusal under the adapter's
// latch (I6) included: it moves nothing. The stops are read from the API server through the stop
// guard, as requeueOvertakenByEstop reads them, so a stop issued after the candidate passed
// filter 10 counts before its fan-out writes it onto the robot. While one covers the robot,
// or the robot is Resuming (estopHoldsWork), the assignment is not released: row A1 pauses it
// (pauseAssignedForEstop), and it stays with the robot until an operator decides (I1). held is
// false when no stop holds the robot and the release goes ahead. A question the guard cannot answer
// is an error, and nothing is released.
func (r *FleetActionReconciler) holdRefusalUnderStop(ctx context.Context, action *fleetv1.FleetAction,
	robotName string) (ctrl.Result, bool, error) {
	robot := &fleetv1.Robot{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Name: robotName, Namespace: action.Namespace}, robot); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, false, nil
		}
		return ctrl.Result{}, true, fmt.Errorf("reading robot %s for its stop before releasing a refused assignment: %w", robotName, err)
	}
	stops, err := stopNames(ctx, r.liveReader(), robot)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if len(stops) == 0 && !estopHoldsWork(robot) {
		return ctrl.Result{}, false, nil
	}
	log.FromContext(ctx).Info("refused assignment not released: a stop holds the robot, so the action is paused on it",
		"task", action.Name, "robot", robotName, "stops", stops, "estopState", robot.Status.EstopState)
	leaseDur, renew, skew := leaseTimings(ctx, r.Client, action.Namespace)
	res, err := r.pauseAssignedForEstop(ctx, action, action.DeepCopy(), robot, time.Now(), leaseDur, renew, skew)
	return res, true, err
}

// releaseBoundUnlessCovered is releaseRobotForStop for a cancel: the bound robot's record is
// kept while a stop covers it (executorguard.KeepsRecord). The cancelled work is then still
// linked to the stop that paused it, so a member deleted and created again under its name is
// not given to another robot while that stop stands; the record is released through the
// executor guard once the stop is cleared.
func (r *FleetActionReconciler) releaseBoundUnlessCovered(ctx context.Context, action *fleetv1.FleetAction, robotName string) error {
	if robotName == "" {
		return nil
	}
	robot := &fleetv1.Robot{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Name: robotName, Namespace: action.Namespace}, robot); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("fetching robot for the cancel: %w", err)
	}
	if robot.Status.AssignedAction != action.Name {
		return nil
	}
	covered, err := r.stopCovering(ctx, robot)
	if err != nil {
		return err
	}
	if executorguard.KeepsRecord(covered, false) {
		log.FromContext(ctx).Info("cancelled action: the robot keeps its record while a stop covers it",
			"task", action.Name, "robot", robotName, "stop", covered)
		return nil
	}
	return r.releaseRobotForStop(ctx, action, robotName)
}

// releaseRobotForStop frees a robot from a action being cancelled/requeued: it
// clears the robot's AssignedAction and returns it to Idle (from InProgress/Assigned)
// — but only if the robot still claims THIS action. Missing robot is a no-op. Called
// only once the robot is provably stopped (single-executor safety).
//
// The write carries the resourceVersion it read. The robot is read from the cache, and
// the action is usually stopped because the robot has just faulted: a view taken before
// the status sink wrote the robot's reported Error still says InProgress, and a plain
// merge from it would write Idle over the Error. The projector does not report an
// unchanged Error again, so nothing would restore it. A conflict is returned, and the
// requeue re-reads.
func (r *FleetActionReconciler) releaseRobotForStop(ctx context.Context, action *fleetv1.FleetAction, robotName string) error {
	if robotName == "" {
		return nil
	}
	robot := &fleetv1.Robot{}
	err := r.Get(ctx, types.NamespacedName{Name: robotName, Namespace: action.Namespace}, robot)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetching robot for stop: %w", err)
	}
	if robot.Status.AssignedAction != action.Name {
		return nil
	}
	robotOriginal := robot.DeepCopy()
	robot.Status.AssignedAction = ""
	if robot.Status.Phase == fleetv1.RobotPhaseInProgress || robot.Status.Phase == fleetv1.RobotPhaseAssigned {
		robot.Status.Phase = fleetv1.RobotPhaseIdle
	}
	if err := r.Status().Patch(ctx, robot, client.MergeFromWithOptions(robotOriginal, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("releasing robot: %w", err)
	}
	return nil
}

// retireTerminalAction settles a Succeeded/Cancelled action to its fixed point:
// release the TDE zone reservation, free the (already-stopped) robot, and clear
// the inert lease horizon once. Every step is idempotent, so repeat reconciles
// of a retired action write nothing.
func (r *FleetActionReconciler) retireTerminalAction(ctx context.Context, req ctrl.Request, action *fleetv1.FleetAction) (ctrl.Result, error) {
	r.releaseReservation(ctx, req.Namespace, action.Spec.Zone, action.Name)
	// The bound robot reported the end, or its stop was confirmed before the cancel finalized:
	// it is released on that proof.
	if err := r.releaseRobotForStop(ctx, action, action.Status.AssignedRobot); err != nil {
		return ctrl.Result{}, err
	}
	// A robot that still records the action without being bound to it (a hold released the
	// binding before the cancel) is released only through the executor guard; until then the
	// horizon is kept, and the action is looked at again.
	_, _, skew := leaseTimings(ctx, r.Client, action.Namespace)
	wait, blocked, why, err := r.releaseRecorders(ctx, action, time.Now(), skew)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait > 0 {
		log.FromContext(ctx).Info("terminal action keeps a robot's record: the robot may still be running it",
			"task", action.Name, "robot", blocked, "why", why, "wait", wait)
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{}, r.clearLeaseHorizon(ctx, action)
}

// handleFailedAction applies the onFailure contract to a action in the Failed phase.
// The robot has already stopped (it self-reported FAILED, or the deadline passed
// before it ever started), so its slot and binding are freed regardless of the
// retry outcome. When onFailure is Requeue and retries remain, the action returns
// to Pending after backoffSeconds — assignmentGeneration preserved so the next
// assignment is strictly greater. Otherwise it stays Failed; Alert (and the
// exhausted-Requeue fall-through) emit a one-shot operator alert, Abandon is
// silent.
func (r *FleetActionReconciler) handleFailedAction(ctx context.Context, req ctrl.Request, action *fleetv1.FleetAction) (ctrl.Result, error) {
	// An action failed on an unconfirmed stop keeps its robot, its zone slot and its lease
	// horizon until the lease has provably expired (condition 3 of the safe-reassignment
	// invariant; an adapter's own report is not proof here). Everything the hold reads is on
	// the action, so it is re-derived after a restart. Clearing the horizon at release ends it.
	if action.Status.FailureReason == reasonStopUnconfirmed && action.Status.AssignedRobot != "" {
		if res, done, err := r.heldFailedAction(ctx, action); done || err != nil {
			return res, err
		}
	}
	if action.Status.FailureReason == reasonStopUnconfirmed && action.Status.LeaseExpiresAt != nil {
		_, renew, skew := leaseTimings(ctx, r.Client, action.Namespace)
		now := time.Now()
		if !leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), now, skew) {
			return ctrl.Result{RequeueAfter: untilLeaseHorizon(action.Status.LeaseExpiresAt, now, renew, skew)}, nil
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(action, corev1.EventTypeNormal, "ActionStopUnconfirmedReleased",
				"lease provably expired (%s); robot %s released",
				action.Status.LeaseExpiresAt.UTC().Format(time.RFC3339), action.Status.AssignedRobot)
		}
	}
	// Free the robot and its zone slot first — a stopped robot must never stay
	// bound to a dead action, whether or not the action will be retried.
	r.releaseReservation(ctx, req.Namespace, action.Spec.Zone, action.Name)
	if err := r.releaseRobotForStop(ctx, action, action.Status.AssignedRobot); err != nil {
		return ctrl.Result{}, err
	}
	// A robot that records the action without being bound to it is released only through the
	// executor guard, and the action is not retried before then.
	if _, _, skew := leaseTimings(ctx, r.Client, action.Namespace); true {
		wait, blocked, why, err := r.releaseRecorders(ctx, action, time.Now(), skew)
		if err != nil {
			return ctrl.Result{}, err
		}
		if wait > 0 {
			log.FromContext(ctx).Info("failed action keeps a robot's record: the robot may still be running it",
				"task", action.Name, "robot", blocked, "why", why, "wait", wait)
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	policy := action.Spec.OnFailure
	if policy == "" {
		policy = fleetv1.ActionFailureRequeue // CRD default; guards un-defaulted objects.
	}
	maxRetries, backoff := r.retryBounds(ctx, action)

	if awaitsRetry(action) {
		// Honour the backoff window measured from the failure instant. The robot is
		// already free, so waiting only delays re-scheduling, never holds a robot.
		if action.Status.FailedAt != nil {
			if wait := backoff - time.Since(action.Status.FailedAt.Time); wait > 0 {
				if err := r.clearLeaseHorizon(ctx, action); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: wait}, nil
			}
		}
		return r.requeueFailedAction(ctx, action, maxRetries)
	}

	// Terminal failure. Alert unless explicitly Abandon (exhausted Requeue alerts).
	if policy != fleetv1.ActionFailureAbandon {
		r.alertFailedOnce(ctx, action)
	}
	return ctrl.Result{}, r.clearLeaseHorizon(ctx, action)
}

// clearLeaseHorizon clears the informational leaseExpiresAt once a action holds no
// live lease (RFC: empty when phase is not Assigned/InProgress). Guarded so it
// does not re-write status on every reconcile of a settled action.
func (r *FleetActionReconciler) clearLeaseHorizon(ctx context.Context, action *fleetv1.FleetAction) error {
	if action.Status.LeaseExpiresAt == nil {
		return nil
	}
	leaseCleared := action.DeepCopy()
	action.Status.LeaseExpiresAt = nil
	if err := r.patchActionStatus(ctx, action, leaseCleared); err != nil {
		return fmt.Errorf("clearing lease horizon: %w", err)
	}
	return nil
}

// requeueFailedAction returns a failed action to Pending for another attempt: it
// increments retryCount, clears the current-attempt lifecycle fields (which the
// next attempt re-stamps), and preserves assignmentGeneration so the next
// assignment is strictly greater. The failure-alerted annotation is cleared so a
// subsequent terminal failure can alert again.
func (r *FleetActionReconciler) requeueFailedAction(ctx context.Context, action *fleetv1.FleetAction, maxRetries int32) (ctrl.Result, error) {
	original := action.DeepCopy()
	action.Status.RetryCount++
	action.Status.Phase = fleetv1.ActionPhasePending
	action.Status.AssignedRobot = ""
	action.Status.LeaseExpiresAt = nil
	action.Status.Message = fmt.Sprintf("requeued after failure (retry %d/%d)", action.Status.RetryCount, maxRetries)
	// Current-attempt timestamps reflect only the attempt in flight (RFC §status),
	// so clear them; the re-scheduled attempt writes fresh values.
	action.Status.ScheduledAt = nil
	action.Status.StartedAt = nil
	action.Status.CompletedAt = nil
	action.Status.FailedAt = nil
	action.Status.CompletionTime = nil
	action.Status.FailureReason = ""
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return ctrl.Result{}, fmt.Errorf("requeueing failed task: %w", err)
	}

	if _, alerted := action.Annotations[annFailureAlerted]; alerted {
		if err := r.removeActionAnnotation(ctx, action, annFailureAlerted); err != nil {
			return ctrl.Result{}, fmt.Errorf("clearing failure-alerted annotation: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

// alertFailedOnce emits a single FleetActionFailed operator event for a terminal
// failure, guarded by an annotation so a resynced Failed action does not re-alert.
func (r *FleetActionReconciler) alertFailedOnce(ctx context.Context, action *fleetv1.FleetAction) {
	if r.Recorder == nil {
		return
	}
	if _, done := action.Annotations[annFailureAlerted]; done {
		return
	}
	reason := action.Status.FailureReason
	if reason == "" {
		reason = "task failed"
	}
	r.Recorder.Event(action, corev1.EventTypeWarning, "FleetActionFailed", reason)

	base := action.DeepCopy()
	if action.Annotations == nil {
		action.Annotations = map[string]string{}
	}
	action.Annotations[annFailureAlerted] = "true"
	if err := r.Patch(ctx, action, client.MergeFrom(base)); err != nil {
		log.FromContext(ctx).Error(err, "marking failure alerted", "task", action.Name)
	}
}

// retryBounds resolves the effective maxRetries and backoff for a action's retry
// policy, falling back to the CRD defaults when spec.retryPolicy is omitted (nil,
// so no server-side field defaulting applied).
func (r *FleetActionReconciler) retryBounds(
	ctx context.Context, action *fleetv1.FleetAction,
) (maxRetries int32, backoff time.Duration) {
	if p := action.Spec.RetryPolicy; p != nil {
		return p.MaxRetries, time.Duration(p.BackoffSeconds) * time.Second
	}
	return defaultMaxRetries, r.requeueBackoff(ctx, action.Namespace)
}

// maxRetriesOf is retryBounds' maxRetries, which needs no namespace read.
func maxRetriesOf(action *fleetv1.FleetAction) int32 {
	if p := action.Spec.RetryPolicy; p != nil {
		return p.MaxRetries
	}
	return defaultMaxRetries
}

// awaitsRetry reports whether a Failed action is one that row A23 returns to Pending once its
// backoff (and, for StopUnconfirmed, A3's release) has run: onFailure Requeue, the CRD default,
// with retries left, on an action that was actually assigned to a robot (an execution attempt).
// A pre-scheduling failure — deadline exceeded while still Pending — is permanently unstartable
// (its deadline will not move), so it is terminal regardless of onFailure rather than flapping
// Pending↔Failed until retries drain. scheduledAt is set at the Assigned transition and is
// cleared on requeue, so it is nil only for a never-assigned action.
//
// handleFailedAction retries on it, and the FleetTask controller reads it to tell a child that is
// Failed only for its backoff from one that is settled (takesWrite), so the two cannot disagree
// about which Failed child will run again.
func awaitsRetry(action *fleetv1.FleetAction) bool {
	if action.Status.Phase != fleetv1.ActionPhaseFailed {
		return false
	}
	policy := action.Spec.OnFailure
	if policy == "" {
		policy = fleetv1.ActionFailureRequeue // CRD default; guards un-defaulted objects.
	}
	return policy == fleetv1.ActionFailureRequeue && action.Status.ScheduledAt != nil &&
		action.Status.RetryCount < maxRetriesOf(action)
}

// requeueBackoff resolves the namespace default for the failure-requeue backoff from
// SwarmadaConfig.spec.scheduling.actionRequeueBackoffSeconds.
//
// This is the field's specified meaning and its only one. RFC-0001's override map
// ({{ref:swarmadaconfig}}, "The full override map") pairs it with
// FleetAction.spec.retryPolicy.backoffSeconds — "default backoff before a failed
// FleetAction is requeued to Pending, applied to FleetAction resources that do not set
// spec.retryPolicy.backoffSeconds explicitly" — so the per-action policy overrides it
// and this fills in when there is none. It is NOT the Pending re-check interval; that
// is pendingRecheckInterval, a controller-runtime requeue that RFC-0001 binds to no
// field (see the constant's comment).
//
// Reachable, unlike some namespace defaults: spec.retryPolicy is a pointer with no CRD
// default, so "the action set no policy" is a state that actually occurs rather than one
// the API server defaults away before any controller sees it.
//
// Fails safe to the constant that mirrors the CRD default when no config is readable or
// the value is negative. Zero is honoured as written — an operator asking for no backoff
// is asking for an immediate retry, not for the default.
func (r *FleetActionReconciler) requeueBackoff(ctx context.Context, namespace string) time.Duration {
	if cfg, ok := namespaceConfig(ctx, r.Client, namespace); ok {
		if secs := cfg.Spec.Scheduling.ActionRequeueBackoffSeconds; secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return time.Duration(defaultBackoffSeconds) * time.Second
}

// clampRetryAfter bounds a TDE RetryAfter hint into the [lo, hi] deconfliction
// backoff window (RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration).
func clampRetryAfter(hint, lo, hi time.Duration) time.Duration {
	if hint < lo {
		return lo
	}
	if hint > hi {
		return hi
	}
	return hint
}

// tdeRetryBounds resolves the deconfliction backoff floor/ceiling for a namespace
// from SwarmadaConfig.spec.trafficDeconfliction.{min,max}RetryAfterSeconds, failing
// safe to the tdeMinRetryAfter / tdeMaxRetryAfter constants when no config is
// readable or a value is non-positive. An inverted config (max < min) is corrected
// to max = min so the window is never empty.
func (r *FleetActionReconciler) tdeRetryBounds(ctx context.Context, namespace string) (lo, hi time.Duration) {
	lo, hi = tdeMinRetryAfter, tdeMaxRetryAfter
	if cfg, ok := namespaceConfig(ctx, r.Client, namespace); ok {
		td := cfg.Spec.TrafficDeconfliction
		if td.MinRetryAfterSeconds > 0 {
			lo = time.Duration(td.MinRetryAfterSeconds) * time.Second
		}
		if td.MaxRetryAfterSeconds > 0 {
			hi = time.Duration(td.MaxRetryAfterSeconds) * time.Second
		}
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// markActionPreempted drives a FleetAction (evicted from a zone reservation by the
// TDE) to the Preempted phase, keeping its robot binding and lease frozen. The
// preemptor runs on a different robot, so nothing else tells this action's robot to
// stop: its eviction to Pending sends cancel_action and waits for the adapter's
// confirmation or provable lease death (see confirmPreemptedStop). Best-effort: a
// missing action or patch error is logged, not fatal.
func (r *FleetActionReconciler) markActionPreempted(ctx context.Context, namespace, actionID, byAction string) {
	logger := log.FromContext(ctx)
	victim := &fleetv1.FleetAction{}
	if err := r.Get(ctx, types.NamespacedName{Name: actionID, Namespace: namespace}, victim); err != nil {
		logger.Error(err, "fetching TDE-preempted task", "task", actionID)
		return
	}
	if victim.Status.Phase == fleetv1.ActionPhasePreempted {
		return
	}
	if !r.mayDisplace(ctx, victim) {
		// Row A11 no longer admits it (the TDE checked before it evicted): it keeps its phase.
		logger.Info("TDE-preempted task not marked Preempted: row A11 does not admit it", "task", actionID,
			"phase", victim.Status.Phase)
		return
	}
	original := victim.DeepCopy()
	victim.Status.Phase = fleetv1.ActionPhasePreempted
	victim.Status.Message = fmt.Sprintf("preempted by higher-priority task %s (zone reservation)", byAction)
	if err := r.patchActionStatus(ctx, victim, original); err != nil {
		logger.Error(err, "marking TDE-preempted task", "task", actionID)
	}
}

// isPreemptible reports whether a action's priority band may be displaced by a
// preemptor band (Critical or High) without further conditions (§9.1.4.3).
// Neither Critical nor High is preemptible (FIFO within the preemptor bands);
// only Normal/Low may be evicted.
func isPreemptible(p fleetv1.ActionPriority) bool {
	return p == fleetv1.ActionPriorityNormal || p == fleetv1.ActionPriorityLow
}

// priorityLabel is the metrics-contract assignment-latency `priority` label; an unset
// priority reports as the Normal default (the CRD default band).
func priorityLabel(p fleetv1.ActionPriority) string {
	if p == "" {
		return string(fleetv1.ActionPriorityNormal)
	}
	return string(p)
}

// priorityRank maps a band to its numeric rank (Critical=1 … Low=4, §9.1.4.3); a
// higher number is a lower priority. An unset band defaults to Normal.
func priorityRank(p fleetv1.ActionPriority) int {
	switch p {
	case fleetv1.ActionPriorityCritical:
		return 1
	case fleetv1.ActionPriorityHigh:
		return 2
	case fleetv1.ActionPriorityLow:
		return 4
	default: // Normal (and the empty default)
		return 3
	}
}

// robotUnderEstop reports whether the robot has an ACTIVE emergency stop:
// RFC-0001 candidate filter 10 — Stopping, Stopped, or Failed. Normal and
// Resuming are not active; an empty state is treated as Normal.
//
// Resuming is deliberately absent. It is the post-clear window, not a live stop:
// the hazard has been cleared by an operator, so an in-flight action must not be
// estop-paused. Its work is still held, though: it is no preemption victim until the
// operator's resume (estopHoldsWork, row A11). It IS withheld from dispatch, by a separate gate in
// adapter_readiness.go — a robot mid-resume has nothing to pause, but it is also
// not ready for new work.
//
// Failed is included because it is NOT "the stop was refused" — it is "a stop
// was commanded and never confirmed" (dropped estop, silence, or STOPPING with
// no STOPPED). The robot's physical state is unknown, it must never be treated
// as at rest, and it already carries an operator obligation to escalate to an
// edge or manual path. Excluding only the robots whose stop SUCCEEDED would
// leave the fleet dispatching to the one robot whose stop did not.
//
// A stop the robot records is active whatever its estopState reads: its own
// swarmada.io/estop-triggered annotation, or a zone's or the namespace's stop in
// status.estopHeldBy (estopscope.HeldOnRecord). The stop holds the robot from the
// moment it is issued, so its work is paused even when the estopState write did not
// land, and the hold does not wait on a status write the robot-scope trigger needs
// none of (RFC-0001 #safety-estop-scopes).
//
// This predicate governs three paths, and Failed now reaches all three: the
// dispatch candidate filter (adapter_readiness.go), the estop-pause of an
// in-flight action (below), and preemption-victim selection, where it is a first
// filter: the victim must then pass the wider estopHoldsWork (mayDisplace, row A11).
func robotUnderEstop(robot *fleetv1.Robot) bool {
	switch robot.Status.EstopState {
	case fleetv1.RobotEstopStopping, fleetv1.RobotEstopStopped, fleetv1.RobotEstopFailed:
		return true
	}
	_, held := estopscope.HeldOnRecord(robot)
	return held
}

// estopHoldsWork reports whether a stop still governs the work a robot holds: the robot is
// under a stop (robotUnderEstop), or the stop is cleared and the operator has not yet
// confirmed the resume (Resuming). Work the stop interrupted is not re-adopted, renewed or
// requeued before that confirmation (RFC-0001 #safety-estop-state-machine).
func estopHoldsWork(robot *fleetv1.Robot) bool {
	return robotUnderEstop(robot) || robot.Status.EstopState == fleetv1.RobotEstopResuming
}

// leaseTime unwraps an optional metav1.Time for the pure decision core.
func leaseTime(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	tt := t.Time
	return &tt
}

// untilLeaseHorizon returns how long to wait before re-checking a lease, bounded
// below by 1s. A nil horizon falls back to the renew interval. `renew` and `skew`
// are the namespace-resolved values from leaseTimings — a pure function over them,
// so it can never read a horizon the caller did not resolve (ADR-0044).
func untilLeaseHorizon(expires *metav1.Time, now time.Time, renew, skew time.Duration) time.Duration {
	if expires == nil {
		return renew
	}
	if d := expires.Time.Add(skew).Sub(now); d > time.Second {
		return d
	}
	return time.Second
}

// SetupWithManager registers the FleetAction controller and watches Robot objects
// so that a Robot phase change (e.g. → Degraded) triggers reconciliation of
// any FleetAction assigned to that robot.
func (r *FleetActionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Scheduler == nil {
		r.Scheduler = scheduler.NewDefaultScheduler()
	}
	if r.TDE == nil {
		// Production always gates through the TDE (RFC-0001 #tde-normative-invariants, TDE-1); it shares the
		// manager's client and mirrors reservation state to FleetZone.status.
		r.TDE = tde.New(r.Client, tde.DefaultConfig())
	}

	// Map a Robot event to the FleetAction that references it.
	robotToAction := handler.MapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		robot, ok := obj.(*fleetv1.Robot)
		if !ok || robot.Status.AssignedAction == "" {
			return nil
		}
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{
				Name:      robot.Status.AssignedAction,
				Namespace: robot.Namespace,
			}},
		}
	})

	// Map a FleetAdapter event to the FleetActions whose dispatch eligibility it governs
	// (ADR-0032 assignment gate). Without this the gate would only take effect on the next
	// unrelated reconcile: an adapter that recovers would leave its robots' work Pending, and one
	// that degrades would keep looking eligible until something else woke the action.
	//
	// Two sets are enqueued, both namespace-scoped and bounded:
	//   1. actions currently bound to a robot of this adapter — so a degrade is observed; and
	//   2. actions still Pending — so a RECOVERY immediately re-offers them a candidate (they have no
	//      robot yet, so set 1 cannot reach them).
	adapterToActions := handler.MapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		adapter, ok := obj.(*fleetv1.FleetAdapter)
		if !ok {
			return nil
		}
		seen := make(map[string]struct{})
		var reqs []reconcile.Request
		add := func(name string) {
			if name == "" {
				return
			}
			if _, dup := seen[name]; dup {
				return
			}
			seen[name] = struct{}{}
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: adapter.Namespace},
			})
		}

		var robots fleetv1.RobotList
		if err := r.List(ctx, &robots, client.InNamespace(adapter.Namespace)); err == nil {
			for i := range robots.Items {
				if robots.Items[i].Spec.Adapter.Name == adapter.Name {
					add(robots.Items[i].Status.AssignedAction)
				}
			}
		}
		var actions fleetv1.FleetActionList
		if err := r.List(ctx, &actions, client.InNamespace(adapter.Namespace)); err == nil {
			for i := range actions.Items {
				if actions.Items[i].Status.Phase == fleetv1.ActionPhasePending {
					add(actions.Items[i].Name)
				}
			}
		}
		return reqs
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetAction{}).
		Watches(&fleetv1.Robot{}, handler.EnqueueRequestsFromMapFunc(robotToAction)).
		Watches(&fleetv1.FleetAdapter{}, handler.EnqueueRequestsFromMapFunc(adapterToActions)).
		Complete(r)
}

// pauseAssignedForEstop is row A1 for an Assigned action whose robot a stop holds: a stop the
// robot has not confirmed takes pauseAssignedUnconfirmed, a confirmed one pauseAssignedUnderStop
// (through applyPauseTransition). The estop pause of a dispatched action and a refusal under a stop
// (holdRefusalUnderStop) both take it.
func (r *FleetActionReconciler) pauseAssignedForEstop(ctx context.Context, action, original *fleetv1.FleetAction,
	robot *fleetv1.Robot, now time.Time, leaseDur, renew, skew time.Duration) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("fleetaction", types.NamespacedName{Namespace: action.Namespace, Name: action.Name})
	if robot.Status.EstopState != fleetv1.RobotEstopStopped {
		logger.Info("estop while Assigned, stop not confirmed — pausing, keeping robot bound until its lease has provably expired",
			"robot", action.Status.AssignedRobot, "estopState", robot.Status.EstopState)
		r.recordActionPausedByEstop(ctx, action, fleetv1.ActionPhaseAssigned)
		return r.pauseAssignedUnconfirmed(ctx, action, original, now, leaseDur, renew, skew)
	}
	logger.Info("estop while Assigned — pausing, keeping robot bound",
		"robot", action.Status.AssignedRobot)
	r.recordActionPausedByEstop(ctx, action, fleetv1.ActionPhaseAssigned)
	return r.applyPauseTransition(ctx, action, original, robot, true,
		estopPausedAssignedMessage, now, leaseDur, renew, pauseUnderStop)
}

// pauseAssignedUnderStop is row A1 for an Assigned action whose robot confirmed its stop. A
// confirmed stop is not proof the robot is off the action, so the action keeps its binding, its
// zone slot, its lease and the robot's record, as an InProgress one does; only a row out of Paused
// (A2, A4, A5, A24, A25) releases them. Work a clear holds (leaseStampHeld) keeps the lease it has.
func (r *FleetActionReconciler) pauseAssignedUnderStop(ctx context.Context, action, original *fleetv1.FleetAction,
	message string, now time.Time, leaseDur, leaseRenew time.Duration) (ctrl.Result, error) {
	action.Status.Phase = fleetv1.ActionPhasePaused
	if !leaseStampHeld(action) || action.Status.LeaseExpiresAt == nil {
		action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDur)}
	}
	action.Status.Message = message
	return ctrl.Result{RequeueAfter: leaseRenew}, r.patchActionStatus(ctx, action, original)
}

// holdPreemptedUnderStop holds a Preempted action while a stop holds its robot, started or not
// (A12 moves work only while no stop holds the robot; under a stop A6 holds it until A2 or A21).
// held is false when the rule does not apply and the lease evaluation decides. The stops are read
// live (stopsOverBoundRobot): a victim whose Robot object is gone is held while any stop in the
// namespace stands, since nothing then records where its robot stands.
func (r *FleetActionReconciler) holdPreemptedUnderStop(ctx context.Context, action, original *fleetv1.FleetAction,
	robot *fleetv1.Robot, found bool, renew time.Duration) (ctrl.Result, bool, error) {
	if action.Status.Phase != fleetv1.ActionPhasePreempted {
		return ctrl.Result{}, false, nil
	}
	stops, _, gone, err := r.stopsOverBoundRobot(ctx, action)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if len(stops) == 0 && (!found || !estopHoldsWork(robot)) {
		return ctrl.Result{}, false, nil
	}
	given := "had started"
	if action.Status.StartedAt == nil {
		given = "was given"
	}
	why := fmt.Sprintf("robot %s %s this action and a stop holds it, so the work is not released on its own",
		action.Status.AssignedRobot, given)
	if gone {
		why = fmt.Sprintf("robot %s %s this action and its Robot object is gone, so nothing records where it stands "+
			"and any stop in the namespace may hold it; the work is not released on its own", action.Status.AssignedRobot, given)
	}
	if !found {
		robot = &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: action.Namespace, Name: action.Status.AssignedRobot}}
	}
	res, err := r.holdUnderStop(ctx, action, original, robot, stops, renew, why)
	return res, true, err
}

// holdUnderStop holds work whose robot a stop holds, in its phase (A6): nothing is released and
// nothing is sent, and only a holder of estop-clear on the stop decides where it goes. why says
// what the hold rests on.
func (r *FleetActionReconciler) holdUnderStop(ctx context.Context, action, original *fleetv1.FleetAction,
	robot *fleetv1.Robot, stops []string, renew time.Duration, why string) (ctrl.Result, error) {
	if len(stops) == 0 {
		stops = []string{"robot/" + robot.Name + " (estopState " + string(robot.Status.EstopState) + ")"}
	}
	msg := keepRequeueWait(action, awaitingDecision(action, stops, why))
	if action.Status.Message == msg {
		return ctrl.Result{RequeueAfter: renew}, nil
	}
	action.Status.Message = msg
	if err := r.patchActionStatus(ctx, action, original); err != nil {
		return ctrl.Result{}, fmt.Errorf("holding started work under a stop: %w", err)
	}
	log.FromContext(ctx).Info("started work held under a stop", "task", action.Name, "phase", action.Status.Phase,
		"robot", robot.Name, "stops", stops)
	warnActionAndTask(r.Recorder, action, EventActionAwaitingOperatorDecision, msg)
	return ctrl.Result{RequeueAfter: renew}, nil
}

// heldFailedAction decides a Failed (StopUnconfirmed) action that still holds its robot, before
// A3's release: a cancel asked on it is carried out once the lease is provably dead (A4); while a
// stop holds the robot, a requeue asked on it is carried out on A2's proof, and the release on
// condition 3 alone does not happen (A6 holds it). done is false when none applies and A3's
// release decides. The stops are read live (stopsOverBoundRobot): an action whose Robot object is
// gone is held while any stop in the namespace stands, since nothing then records where its robot
// stands.
func (r *FleetActionReconciler) heldFailedAction(ctx context.Context, action *fleetv1.FleetAction) (ctrl.Result, bool, error) {
	original := action.DeepCopy()
	_, renew, skew := leaseTimings(ctx, r.Client, action.Namespace)
	dead := leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), time.Now(), skew)
	robotName := action.Status.AssignedRobot

	// A4: the one condition that releases a Failed (StopUnconfirmed) action is a dead lease.
	var intent *cancelIntent
	if v, ok := action.Annotations[annCancelRequested]; ok {
		in := operatorCancelIntent(v)
		intent = &in
	} else if action.Spec.DesiredState == fleetv1.DesiredStateCancelled {
		in := unattributedCancelIntent("desiredState=Cancelled", command.CancelCauseUnspecified)
		intent = &in
	}
	if intent != nil {
		if !dead {
			res, err := r.holdStop(ctx, action, original, cancellingMessage)
			return res, true, err
		}
		return ctrl.Result{}, true, r.finalizeCancel(ctx, action, original, robotName, *intent)
	}

	robot := &fleetv1.Robot{}
	if err := r.liveReader().Get(ctx, types.NamespacedName{Name: robotName, Namespace: action.Namespace}, robot); err != nil &&
		!errors.IsNotFound(err) {
		return ctrl.Result{}, true, fmt.Errorf("fetching the robot of a failed action: %w", err)
	}
	stops, _, gone, err := r.stopsOverBoundRobot(ctx, action)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if gone || robot.Name == "" {
		robot = &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: action.Namespace, Name: robotName}}
	}
	if len(stops) == 0 && !estopHoldsWork(robot) {
		return ctrl.Result{}, false, nil
	}
	// A2: a requeue asked while a stop holds the robot (the requeue check admitted it only to the
	// stop's authority) runs on the same proof as from Paused.
	if _, requested := action.Annotations[annRequeueRequested]; requested {
		waitMsg := requeuingMessage
		if len(stops) > 0 {
			waitMsg = requeueUnderStopMessage(stops)
		}
		res, err := r.handleRequeue(ctx, action, original, waitMsg)
		return res, true, err
	}
	// A6: held while the stop holds the robot, the lease's death included.
	if !dead {
		return ctrl.Result{RequeueAfter: untilLeaseHorizon(action.Status.LeaseExpiresAt, time.Now(), renew, skew)}, true, nil
	}
	why := fmt.Sprintf("robot %s did not confirm the stop that failed this action, and a stop holds it, so its dead lease "+
		"does not release the work", robot.Name)
	if gone {
		why = fmt.Sprintf("robot %s did not confirm the stop that failed this action and its Robot object is gone, so "+
			"nothing records where it stands and any stop in the namespace may hold it; its dead lease does not "+
			"release the work", robot.Name)
	}
	res, err := r.holdUnderStop(ctx, action, original, robot, stops, renew, why)
	return res, true, err
}
