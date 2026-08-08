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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
)

const (
	// leaseDuration is how long a robot may execute an assigned action without a
	// renewal before it MUST self-stop (RFC-0001 §9.6.3.5). The control plane
	// renews within this window while the assignment stands. TODO: source from
	// SwarmadaConfig.spec.scheduling once that surface consumes it (as
	// robot_controller.go now sources its offline threshold from spec.health).
	leaseDuration = 30 * time.Second
	// leaseRenewInterval is how often the control plane refreshes the lease while
	// the robot is reachable — well below leaseDuration (default duration/3, §9.3.2).
	leaseRenewInterval = leaseDuration / 3
	// leaseClockSkew is the safety margin added before a lease is considered
	// provably expired (§9.6.3.5 condition 3: now ≥ lastRenewal + duration + skew).
	leaseClockSkew = 5 * time.Second

	// tdeMinRetryAfter / tdeMaxRetryAfter are the fail-safe bounds on the requeue
	// backoff after a TDE Denied (§9.1.11.10). They match the CRD defaults for
	// spec.trafficDeconfliction.{min,max}RetryAfterSeconds; the namespace config
	// overrides them via tdeRetryBounds when a SwarmadaConfig is readable.
	tdeMinRetryAfter = 10 * time.Second
	tdeMaxRetryAfter = 120 * time.Second
)

// RBAC for the FleetAction controller. Declared as a standalone, markers-only
// comment group (blank lines both sides): controller-gen silently drops
// +kubebuilder:rbac markers that share a comment group with prose, so these
// MUST NOT be folded into the type's doc comment below (observed with
// controller-gen v0.16.5 — the fleetactions/fleetactions-status/fleetactions-finalizers
// markers vanished while they lived inside the FleetActionReconciler doc comment).

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetadapters,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetactions/finalizers,verbs=update
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch

// FleetActionReconciler reconciles FleetAction objects.
type FleetActionReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Scheduler scheduler.Scheduler
	// TDE is the deconfliction gate the reconciler MUST pass before committing an
	// assignment (§9.4, TDE-1). Defaulted in SetupWithManager; a nil TDE (only via
	// direct construction in unit tests) skips the zone-capacity gate.
	TDE tde.TrafficDeconflictionEngine
	// Commander pushes assign_action / renew_lease Commands to a robot's adapter over
	// ControlStream (§9.6.3). It is BEST-EFFORT and NON-GATING: the authoritative
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
	// Audit records FLEETACTION_CANCELLED into the §9.5.4 chain when a cancel is
	// finalized. Nil disables audit recording. The actor is this controller (the
	// operator who requested the cancel is captured by the API-server audit; the
	// annotation write was SAR-gated by the FleetAction cancel webhook).
	Audit audit.Recorder
	// Recorder emits Kubernetes Events (e.g. FleetActionFailed for onFailure: Alert
	// and exhausted-retry terminal failures). Nil disables event emission.
	Recorder record.EventRecorder
}

// wireTimeout bounds a best-effort assign/renew push so a silent adapter never
// stalls the reconcile loop; a missing stream returns immediately regardless.
const wireTimeout = 2 * time.Second

// annCancelRequested marks a FleetAction for operator cancellation (set by the
// `swarmctl cancel task` verb, §9.5.3). Its value is an optional reason. The
// reconciler drives a CONFIRMED cancel (§9.6.3.5 single-executor discipline): a
// bound robot is freed only once it provably stopped — the adapter acknowledges
// cancel_action, or the lease is provably dead — never on an unconfirmed cancel.
const annCancelRequested = "swarmada.io/cancel-requested"

// revokingHeldMessage is the status message for a Revoking action held under
// onDisconnect=Never (or an un-consumable policy): the lease is provably dead but
// the control plane does not auto-reassign — an operator must cancel (§9.1.11.9).
const revokingHeldMessage = "assigned robot lost, lease provably dead — awaiting operator cancel (onDisconnect=Never)"

// annRequeueRequested marks a FleetAction for a forcible requeue (set by the
// ZoneMaintenance controller in Immediate mode, §9.1.11). It reuses the SAME
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

// reasonCapabilityLost is the requeue reason recorded when a reachable robot's
// in-flight action is reassigned because the robot no longer satisfies the action's
// required capabilities (RFC-0001 Capability-loss reassignment).
const reasonCapabilityLost = "CapabilityLost"

// reasonCapabilityLostDuringExecution is the FleetAction failureReason set when the
// adapter could not safely hand off a mid-commitment robot and recovered it
// (RFC-0001 Capability-loss reassignment, Recovery outcome).
const reasonCapabilityLostDuringExecution = "CapabilityLostDuringExecution"

// Reconcile drives the FleetAction state machine.
//
//	Pending              → Assigned   (scheduler finds a robot; mints a lease)
//	Assigned/InProgress  → Revoking   (assigned robot lost — lease outstanding)
//	Revoking             → InProgress (robot returned on a live lease — re-adopt)
//	Revoking             → Pending    (prior lease PROVABLY dead — safe to reassign)
//	InProgress           → Succeeded/Failed (Fleet Adapter reports completion)
//
// The single-executor guarantee (RFC-0001 §9.6.3.5, RA-4): a lost robot's action is
// NEVER reassigned on unreachability alone. It is held in Revoking until the lease
// is provably dead — the robot confirms it is not running the action, or the lease
// horizon (last renewal + leaseDuration + skew) passes, by which point the robot
// has self-stopped. Unreachable is not stopped; stopped is confirmed, never
// inferred.
func (r *FleetActionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("fleetaction", req.NamespacedName)

	action := &fleetv1.FleetAction{}
	if err := r.Get(ctx, req.NamespacedName, action); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching fleetaction: %w", err)
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

	// ── Operator cancellation (§9.6.3, confirmed-cancel) ───────────────────────
	// A cancel request frees the robot only once it provably stopped, so a cancel
	// never strands a still-executing robot (single-executor safety).
	if _, requested := action.Annotations[annCancelRequested]; requested {
		return r.handleCancel(ctx, action, original)
	}

	// ── Forcible requeue (§9.1.11 ZoneMaintenance Immediate) ───────────────────
	// Same confirmed-stop discipline as cancel, but the action returns to Pending
	// (re-schedulable elsewhere) rather than terminating. Checked before scheduling
	// so a requeue-pending action is never re-assigned mid-flight.
	if _, requested := action.Annotations[annRequeueRequested]; requested {
		return r.handleRequeue(ctx, action, original)
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
		return ctrl.Result{}, r.Status().Patch(ctx, action, client.MergeFrom(original))
	}

	// ── Assignment-lease lifecycle (single-executor guarantee, §9.6.3.5) ────────
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

		// ── Estop-pause (§9.6.2.4): estop takes precedence over the lease ───────
		// An active estop on the assigned robot pauses its action. Estop is never
		// fenced — a safe stop is always honoured. Paused is operator-gated: the
		// control plane NEVER auto-requeues or auto-resumes it.
		if found && robotUnderEstop(robot) {
			switch action.Status.Phase {
			case fleetv1.ActionPhaseAssigned:
				// Table §9.6.2.4: the robot never started; release the binding so an
				// operator can re-assign (a fresh assignment mints a new generation).
				logger.Info("estop while Assigned — pausing and releasing robot",
					"robot", action.Status.AssignedRobot)
				if robot.Status.AssignedAction == action.Name {
					robotOriginal := robot.DeepCopy()
					robot.Status.AssignedAction = ""
					_ = r.Status().Patch(ctx, robot, client.MergeFrom(robotOriginal))
				}
				r.recordActionPausedByEstop(ctx, action, fleetv1.ActionPhaseAssigned)
				action.Status.Phase = fleetv1.ActionPhasePaused
				action.Status.AssignedRobot = ""
				action.Status.LeaseExpiresAt = nil
				action.Status.Message = "paused by estop — operator resume/requeue/cancel required"
				// The robot never entered the zone for this action; free its slot. (An
				// InProgress→Paused robot is physically in-zone and keeps its slot.)
				r.releaseReservation(ctx, req.Namespace, action.Spec.Zone, action.Name)
				return ctrl.Result{}, r.Status().Patch(ctx, action, client.MergeFrom(original))
			case fleetv1.ActionPhaseInProgress:
				// Robot stops mid-action; KEEP it bound and keep the lease so no other
				// robot can take the action (single-executor, §9.6.3.5).
				logger.Info("estop while InProgress — pausing, keeping robot bound",
					"robot", action.Status.AssignedRobot)
				r.recordActionPausedByEstop(ctx, action, fleetv1.ActionPhaseInProgress)
				action.Status.Phase = fleetv1.ActionPhasePaused
				action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDuration)}
				action.Status.Message = "paused by estop — operator resume/cancel required"
				return ctrl.Result{RequeueAfter: leaseRenewInterval},
					r.Status().Patch(ctx, action, client.MergeFrom(original))
			}
			// Already Paused/Revoking under estop → fall through to Paused handling.
		}

		// ── Paused is operator-gated (§9.6.2.4): NEVER auto-resume ─────────────
		// While the action stays bound to a reachable robot, keep renewing the lease
		// so it cannot be reassigned; otherwise hold. The phase never changes here —
		// only an operator moves a Paused action out.
		if action.Status.Phase == fleetv1.ActionPhasePaused {
			if action.Status.AssignedRobot != "" && found &&
				classifyRobot(action, robot, found) != robotLost {
				action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDuration)}
				if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
					return ctrl.Result{}, fmt.Errorf("renewing lease for paused task: %w", err)
				}
				return ctrl.Result{RequeueAfter: leaseRenewInterval}, nil
			}
			return ctrl.Result{}, nil
		}

		switch evaluateLease(action.Status.Phase, classifyRobot(action, robot, found),
			leaseTime(action.Status.LeaseExpiresAt), now, leaseClockSkew) {
		case actionRenew:
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
			// never a new one (§9.6.3.4); reassigning here would double-execute.
			action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDuration)}
			if action.Status.Phase == fleetv1.ActionPhaseRevoking {
				logger.Info("re-adopting task on live lease", "robot", action.Status.AssignedRobot,
					"generation", action.Status.AssignmentGeneration)
				action.Status.Phase = fleetv1.ActionPhaseInProgress
				action.Status.Message = "re-adopted after connectivity recovered"
				// The disconnect resolved without reassignment; drop the AfterTimeout anchor.
				action.Status.DisconnectedAt = nil
			}
			if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
				return ctrl.Result{}, fmt.Errorf("renewing lease: %w", err)
			}
			metrics.IncLeaseRenewal(action.Namespace)
			// Refresh the robot's self-stop deadline over the wire (best-effort, AFTER
			// the authoritative renewal). A failed push never changes the server-side
			// lease — the robot will self-stop on its own timer if renewals stop.
			r.pushRenewLease(ctx, action)
			return ctrl.Result{RequeueAfter: leaseRenewInterval}, nil

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
				action.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(leaseDuration)}
			}
			// Anchor the disconnect wall-clock for onDisconnect=AfterTimeout. Set once
			// on the FIRST entry into Revoking-by-disconnect so the ceiling measures from
			// the loss, not from each requeue.
			if action.Status.DisconnectedAt == nil {
				action.Status.DisconnectedAt = &metav1.Time{Time: now}
			}
			// Hold the zone slot through the disconnect window (extends the Reserved
			// TTL to disconnectedReservationTTL) so it is not released before the
			// robot's reconnect chance (§9.4.2).
			if r.TDE != nil && action.Spec.Zone != "" {
				_ = r.TDE.OnActionPhaseChanged(ctx, req.Namespace, action.Spec.Zone, action.Name, fleetv1.ActionPhaseRevoking)
			}
			if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
				return ctrl.Result{}, fmt.Errorf("entering revoking: %w", err)
			}
			return ctrl.Result{RequeueAfter: untilLeaseHorizon(action.Status.LeaseExpiresAt, now)}, nil

		case actionHold:
			// Not yet safe to requeue. Never reassign on a guess. A Preempted action
			// polls promptly for its robot to switch to the Critical action; a Revoking
			// action waits for the lease horizon.
			if action.Status.Phase == fleetv1.ActionPhasePreempted {
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			return ctrl.Result{RequeueAfter: untilLeaseHorizon(action.Status.LeaseExpiresAt, now)}, nil

		case actionReassign:
			// Lease PROVABLY dead (robot reachable & confirmed not running T, or the
			// lease expired past the horizon). A Revoking action here is in disconnect
			// recovery: SwarmadaConfig.actionCancellation.onDisconnect governs whether the
			// control plane AUTO-recovers it (§9.1.11.9). The lease horizon stays the
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
						if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
							return ctrl.Result{}, fmt.Errorf("holding revoking task: %w", err)
						}
						return ctrl.Result{RequeueAfter: ceiling.Sub(now)}, nil
					}
					logger.Info("onDisconnect=AfterTimeout ceiling crossed — reassigning",
						"task", action.Name, "disconnectTimeoutSeconds", timeout)
					// ceiling crossed → fall through to the reassignment below.
				case fleetv1.ActionCancellationWhenActionExpired:
					// WhenActionExpired: a Revoking action whose per-action completion window
					// (spec.timeoutSeconds from status.StartTime) has passed is TERMINATED
					// rather than reassigned — expiry renders completion moot (§9.1.11.9).
					// The lease is provably dead here, so the robot provably stopped:
					// finalize to Cancelled via the confirmed-stop path (frees the robot,
					// lease, and TDE slot). A action with no timeout, or one that never
					// started, cannot expire → hold like Never below.
					if actionCompletionExpired(action, now) {
						logger.Info("onDisconnect=WhenActionExpired: completion window elapsed while Revoking — cancelling",
							"task", action.Name, "timeoutSeconds", *action.Spec.TimeoutSeconds)
						action.Status.DisconnectedAt = nil
						return ctrl.Result{}, r.finalizeCancel(ctx, action, original, action.Status.AssignedRobot,
							"expired while disconnected (onDisconnect=WhenActionExpired)")
					}
					fallthrough
				case fleetv1.ActionCancellationNever, fleetv1.ActionCancellationPolicy(""):
					// Never (default): hold Revoking until an operator cancels — the safest
					// disposition for actions with physical side effects (§9.1.11.9). Keep the
					// robot bound; the operator's `swarmctl cancel task` drives handleCancel,
					// which finalizes once the robot provably stopped. Event-driven: the
					// robot's return or the cancel annotation re-triggers this reconcile.
					if action.Status.Message != revokingHeldMessage {
						action.Status.Message = revokingHeldMessage
						if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
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
			// reassignment target (§9.4.2).
			r.releaseReservation(ctx, req.Namespace, action.Spec.Zone, action.Name)
			// A Revoking action reassigning here is a genuine lease EXPIRY (the robot
			// self-stopped, §9.3.8); a non-Revoking clean release (robot present &
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
				r.Status().Patch(ctx, action, client.MergeFrom(original))
		}
	}

	// ── Scheduling ────────────────────────────────────────────────────────────
	if action.Status.Phase == "" || action.Status.Phase == fleetv1.ActionPhasePending {
		// Anchor the assignment-latency clock the first reconcile the action is seen
		// Pending (§9.3.8). It persists via the no-robot / TDE-denied patches below,
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

		// ADR-0032 assignment gate: withhold any robot whose bound FleetAdapter is not fit to
		// receive work (Connected + conformance Passed), FAIL CLOSED. The admission gate checks this
		// once, at admission; readiness can flip afterwards (heartbeats lapse → Degraded/Disconnected;
		// a report digest/ConfigMap change → Failed) and nothing re-examined it, so those robots kept
		// getting work. Filtering the candidate list HERE covers both the normal selection below and
		// the preemption search, which reads the same slice. Dispatch only: telemetry, heartbeats and
		// estop are untouched, and work already in flight is never revoked by this gate.
		eligible, withheld := r.filterDispatchEligible(ctx, robotList.Items)
		logDispatchExclusions(ctx, action, withheld)
		robotList.Items = eligible

		robot, err := r.selectServableRobot(ctx, action, robotList.Items,
			r.acceptDegraded(ctx, action), r.preferSameManufacturer(ctx, req.Namespace),
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
				if cand != nil && r.actionServableBy(ctx, action, cand) {
					robot = *cand
					preemptVictim = victim
					err = nil
				}
			}
			if err != nil {
				// No eligible robot and no preemption possible — requeue after a
				// short backoff.
				logger.Info("no eligible robot, requeuing", "reason", err)
				action.Status.Phase = fleetv1.ActionPhasePending
				action.Status.Message = err.Error()
				_ = r.Status().Patch(ctx, action, client.MergeFrom(original))
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
		}

		// ── TDE deconfliction gate (§9.4, TDE-1) ───────────────────────────────
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
			})
			if terr != nil || res.Status == tde.Denied {
				// No slot — do NOT commit. Leave the action Pending and requeue with
				// the TDE's RetryAfter hint, bounded by the deconfliction backoff.
				logger.Info("TDE denied assignment; requeuing", "reason", res.DeniedReason, "err", terr)
				action.Status.Phase = fleetv1.ActionPhasePending
				action.Status.Message = "awaiting zone capacity (traffic deconfliction)"
				_ = r.Status().Patch(ctx, action, client.MergeFrom(original))
				minRetry, maxRetry := r.tdeRetryBounds(ctx, req.Namespace)
				return ctrl.Result{RequeueAfter: clampRetryAfter(res.RetryAfter, minRetry, maxRetry)}, nil
			}
			reserved = true
			if res.Status == tde.PreemptedGranted {
				// Reservations the TDE evicted to make room: drive those actions to
				// Preempted (their safe eviction is gated on the robot switching off
				// them; the physical stop rides the cancel_action wire, §9.4.6/§F).
				for _, pid := range res.PreemptedActionIDs {
					r.markActionPreempted(ctx, req.Namespace, pid, action.Name)
				}
			}
		}

		// The TDE gate granted (or there is no zone gate): NOW commit the §C robot
		// preemption. Deferring the victim mark to here means a Denied/failed
		// reservation above never strands a Preempted victim.
		if preemptVictim != nil {
			logger.Info("preempting lower-band task for Critical task",
				"victim", preemptVictim.Name, "robot", robot.Name)
			if err := r.markVictimPreempted(ctx, preemptVictim, action.Name); err != nil {
				r.unreserveOnFailure(ctx, reserved, req.Namespace, action.Spec.Zone, action.Name)
				return ctrl.Result{}, err
			}
		}

		logger.Info("scheduling task", "robot", robot.Name)
		// Observe the completed Pending→Assigned latency and release the anchor
		// (§9.3.8). Cleared in the same write that sets Assigned.
		if action.Status.PendingSince != nil {
			metrics.ObserveAssignmentLatency(action.Namespace, priorityLabel(action.Spec.Priority),
				time.Since(action.Status.PendingSince.Time))
			action.Status.PendingSince = nil
		}
		action.Status.Phase = fleetv1.ActionPhaseAssigned
		action.Status.AssignedRobot = robot.Name
		action.Status.Message = ""
		if action.Status.ScheduledAt == nil {
			scheduledTS := metav1.Now()
			action.Status.ScheduledAt = &scheduledTS
		}

		// Mint a new lease from the PERSISTED high-water mark (read-before-issue),
		// so it is strictly monotonic across a control-plane restart/failover and
		// never reused (§9.6.3.5 failover safety). Never reset — a reassignment
		// increments it. Establish the lease horizon at assignment time.
		action.Status.AssignmentGeneration++
		action.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().Add(leaseDuration)}

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
		if err := r.Status().Patch(ctx, &robot, client.MergeFrom(robotOriginal)); err != nil {
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
			if revertErr := r.Status().Patch(ctx, action, client.MergeFrom(revertOriginal)); revertErr != nil {
				logger.Error(revertErr, "failed to revert task to Pending after robot commit failure")
			}
			return ctrl.Result{}, fmt.Errorf("patching robot status: %w", err)
		}
		// Deliver the assignment to the robot's adapter over the wire (AFTER the
		// authoritative commit; fencing token = the freshly-minted generation). An
		// unreachable push leaves the assignment standing (best-effort); an EXPLICIT
		// rejection means the robot won't run it → release and reschedule.
		if r.pushAssignAction(ctx, action, robot.Name) {
			return r.releaseRejectedAssignment(ctx, req.Namespace, action, robot.Name)
		}
		return ctrl.Result{RequeueAfter: leaseRenewInterval}, nil
	}

	return ctrl.Result{}, nil
}

// ── Assignment-lease decision core (RFC-0001 §9.6.3.5) ────────────────────────
//
// evaluateLease and its helpers are PURE and deterministic so the single-executor
// guarantee is exhaustively testable in isolation, free of the Kubernetes client.

// reachability is the assigned robot's observed relationship to the action.
type reachability int

const (
	// robotExecuting: reachable and reporting it holds action T.
	robotExecuting reachability = iota
	// robotFree: reachable and NOT holding T (provably not running it, §9.6.3.5 cond. 2).
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
			return actionRenew // robot returned on a live lease → re-adopt (§9.6.3.4)
		case robotFree:
			return actionReassign // §9.6.3.5 condition 2: reachable & confirmed not running T
		default:
			// robotLost: reassign only once the lease has provably expired.
			if leaseProvablyDead(lease, now, skew) {
				return actionReassign // §9.6.3.5 condition 3
			}
			return actionHold
		}
	case fleetv1.ActionPhasePreempted:
		// A Critical action displaced this one (§9.1.4.3). It is being evicted, NOT
		// re-adopted — so unlike Revoking it never renews. It requeues only once
		// its former robot has provably switched off it (the robot now holds the
		// Critical action → robotFree — §9.6.3.5 condition 2), or its frozen lease
		// dies (condition 3). While the robot still claims it, hold.
		switch r {
		case robotExecuting:
			return actionHold
		case robotFree:
			return actionReassign
		default:
			if leaseProvablyDead(lease, now, skew) {
				return actionReassign
			}
			return actionHold
		}
	}
	return actionHold
}

// leaseProvablyDead is §9.6.3.5 condition 3. A nil horizon is NOT proof of death;
// the controller must never reassign on it (RA-4).
func leaseProvablyDead(lease *time.Time, now time.Time, skew time.Duration) bool {
	return lease != nil && !now.Before(lease.Add(skew))
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
// or High victim — the same band rule as the TDE reservation preemption. Returns
// the robot and the victim action, or (nil, nil) when none exists. The caller marks
// the victim Preempted (via markVictimPreempted) only after the TDE gate grants.
func (r *FleetActionReconciler) findPreemptionCandidate(
	ctx context.Context, action *fleetv1.FleetAction, robots []fleetv1.Robot,
) (*fleetv1.Robot, *fleetv1.FleetAction, error) {
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
		if !isPreemptible(v.Spec.Priority) {
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
// Pending only once its robot switches to the Critical action, or its lease dies;
// see the Preempted case of evaluateLease). Called only after the TDE gate grants,
// so a denied/failed assignment never strands a Preempted victim.
func (r *FleetActionReconciler) markVictimPreempted(ctx context.Context, victim *fleetv1.FleetAction, byAction string) error {
	vOrig := victim.DeepCopy()
	victim.Status.Phase = fleetv1.ActionPhasePreempted
	victim.Status.Message = fmt.Sprintf("preempted by higher-priority task %s", byAction)
	if err := r.Status().Patch(ctx, victim, client.MergeFrom(vOrig)); err != nil {
		return fmt.Errorf("marking victim preempted: %w", err)
	}
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
// reassignment transition (§9.4.2). Nil-TDE and empty-zone safe; idempotent (the
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
// wire (§9.6.3). Best-effort and NON-GATING: it runs AFTER the authoritative
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
func (r *FleetActionReconciler) pushAssignAction(ctx context.Context, action *fleetv1.FleetAction, robotID string) (rejected bool) {
	if r.Commander == nil {
		return false
	}
	gen := command.FencingToken(action.Status.AssignmentGeneration)
	a := command.AssignAction{
		ActionID:        action.Name,
		ActionType:      string(action.Spec.Type),
		FencingToken:    gen,
		LeaseGeneration: gen,
		LeaseDurationMs: command.LeaseDurationMs(leaseDuration),
		Priority:        int32(priorityRank(action.Spec.Priority)),
	}
	if action.Spec.Deadline != nil {
		a.DeadlineMs = action.Spec.Deadline.UnixMilli()
	}
	wctx, cancel := context.WithTimeout(ctx, wireTimeout)
	defer cancel()
	out, err := r.Commander.PushAssignAction(wctx, action.Namespace, robotID, a)
	logger := log.FromContext(ctx)
	if err != nil {
		logger.V(1).Info("assign_action not delivered (best-effort; assignment stands, lease machinery governs)",
			"task", action.Name, "robot", robotID, "reason", err.Error())
		return false
	}
	if !out.Accepted {
		logger.Info("adapter rejected assign_action; releasing the assignment to reschedule",
			"task", action.Name, "robot", robotID, "rejection", out.Rejection, "message", out.Message)
		return true
	}
	return false
}

// pushRenewLease refreshes the robot's self-stop deadline over the wire (§9.6.3.5).
// Best-effort and NON-GATING: it runs AFTER the authoritative lease renewal, so a
// failed push never changes the server-side lease — if renewals stop reaching the
// robot, its own timer self-stops it. A nil Commander is a no-op.
func (r *FleetActionReconciler) pushRenewLease(ctx context.Context, action *fleetv1.FleetAction) {
	if r.Commander == nil || action.Status.AssignedRobot == "" {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, wireTimeout)
	defer cancel()
	out, err := r.Commander.PushRenewLease(wctx, action.Namespace, action.Status.AssignedRobot, command.RenewLease{
		ActionID:        action.Name,
		LeaseGeneration: command.FencingToken(action.Status.AssignmentGeneration),
		LeaseDurationMs: command.LeaseDurationMs(leaseDuration),
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
// disposition and its wall-clock ceiling (§9.1.11.9). It FAILS SAFE to Never (the
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
	pool := candidates
	// Bounded by the pool size: every iteration removes exactly one candidate.
	for range candidates {
		robot, err := r.Scheduler.SelectRobot(action, pool, acceptDegraded, preferSameManufacturer, honorPreferredRobot)
		if err != nil {
			return robot, err
		}
		if r.actionServableBy(ctx, action, &robot) {
			return robot, nil
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
	return r.Scheduler.SelectRobot(action, nil, acceptDegraded, preferSameManufacturer, honorPreferredRobot)
}

// actionServableBy asks one robot's adapter whether it can serve this action instance.
//
// Three replies, and only one of them withholds the robot:
//   - servable        → yes.
//   - unsupported     → the adapter does not implement validate_action. §9.2.8 makes it
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
	if r.Validator == nil {
		return true // catalog gate only — the v0.2 behaviour
	}
	logger := log.FromContext(ctx)
	var payload []byte
	if action.Spec.Payload != nil {
		payload = action.Spec.Payload.Raw
	}
	out, err := r.Validator.ValidateAction(ctx, robot.Namespace, robot.Name, string(action.Spec.Type), payload)
	switch {
	case err != nil:
		logger.V(1).Info("validate_action unreachable; skipping candidate",
			"robot", robot.Name, "action", action.Name, "err", err)
		return false
	case out.Unsupported:
		return true
	case !out.Servable:
		logger.Info("adapter cannot serve this action instance; trying another robot",
			"robot", robot.Name, "action", action.Name, "reason", out.Message)
		return false
	default:
		return true
	}
}

// recordActionPausedByEstop seals ACTION_PAUSED_BY_ESTOP (§9.6.5.1) as an in-flight action
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

// actionCompletionExpired reports whether a action's per-action completion window
// (spec.timeoutSeconds measured from status.StartTime, §9.1.11.9) has elapsed at
// now. It is false when no positive timeout is set or the action never started — an
// unbounded or unstarted action cannot expire, so onDisconnect=WhenActionExpired holds
// such a action like Never rather than cancelling it.
func actionCompletionExpired(action *fleetv1.FleetAction, now time.Time) bool {
	if action.Spec.TimeoutSeconds == nil || *action.Spec.TimeoutSeconds <= 0 || action.Status.StartTime == nil {
		return false
	}
	deadline := action.Status.StartTime.Add(time.Duration(*action.Spec.TimeoutSeconds) * time.Second)
	return !now.Before(deadline)
}

// handleCancel drives a confirmed cancellation (§9.6.3.5). It finalizes the action
// to Cancelled — releasing the robot binding, lease, and TDE slot — ONLY when the
// assigned robot is provably not executing: it is unbound, the adapter confirms
// the cancel, or the lease is provably dead (the robot self-stopped). Otherwise it
// holds the action "cancelling" and requeues; it NEVER frees the robot/slot while
// the lease is alive, so a cancel cannot cause a double-execution.
func (r *FleetActionReconciler) handleCancel(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction) (ctrl.Result, error) {
	reason := action.Annotations[annCancelRequested]
	robotName := action.Status.AssignedRobot

	// Not bound (nothing executes) or the robot is provably stopped → finalize.
	if robotName == "" || r.confirmedStop(ctx, action, robotName, reason) {
		return ctrl.Result{}, r.finalizeCancel(ctx, action, original, robotName, reason)
	}
	// Otherwise hold: the robot may still be executing. Never free it here.
	return r.holdStop(ctx, action, original, cancellingMessage)
}

// handleRequeue drives a forcible requeue (§9.1.11 ZoneMaintenance Immediate). It
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

func (r *FleetActionReconciler) handleRequeue(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction) (ctrl.Result, error) {
	reason := action.Annotations[annRequeueRequested]
	robotName := action.Status.AssignedRobot

	if robotName == "" {
		return ctrl.Result{}, r.finalizeRequeue(ctx, action, original, robotName, reason)
	}

	confirmed, disp := r.confirmedStopWithDisposition(ctx, action, robotName, reason)
	if !confirmed {
		return r.holdStop(ctx, action, original, requeuingMessage)
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
func (r *FleetActionReconciler) confirmedStop(ctx context.Context, action *fleetv1.FleetAction, robotName, reason string) bool {
	ok, _ := r.confirmedStopWithDisposition(ctx, action, robotName, reason)
	return ok
}

// confirmedStopWithDisposition reports whether the robot is provably not executing
// (adapter acknowledged a cancel_action, OR the lease is provably dead — a nil horizon
// is NOT proof, RA-4), and, when confirmed via the adapter, how it handled the
// cancel. This is the single-executor gate shared by cancel and requeue — a robot is
// freed only when confirmed is true. The lease-death path reports StoppedSafely.
func (r *FleetActionReconciler) confirmedStopWithDisposition(ctx context.Context, action *fleetv1.FleetAction, robotName, reason string) (bool, command.CancelDisposition) {
	if r.Commander != nil {
		if ok, disp := r.pushCancel(ctx, action, robotName, reason); ok {
			return true, disp
		}
	}
	if leaseProvablyDead(leaseTime(action.Status.LeaseExpiresAt), time.Now(), leaseClockSkew) {
		return true, command.CancelStoppedSafely
	}
	return false, command.CancelStoppedSafely
}

// holdStop keeps a not-yet-stopped action in its current phase with a "awaiting
// confirmed stop" message and requeues — it never frees the robot.
func (r *FleetActionReconciler) holdStop(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction, msg string) (ctrl.Result, error) {
	if action.Status.Message != msg {
		action.Status.Message = msg
		if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking task awaiting stop: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: leaseRenewInterval}, nil
}

// pushCancel pushes cancel_action and reports whether the adapter CONFIRMED the
// cancellation (a provable stop) and, if so, its disposition. Unreachable/declined/
// stale is not confirmed.
func (r *FleetActionReconciler) pushCancel(ctx context.Context, action *fleetv1.FleetAction, robotName, reason string) (bool, command.CancelDisposition) {
	wctx, cancel := context.WithTimeout(ctx, wireTimeout)
	defer cancel()
	out, err := r.Commander.PushCancelAction(wctx, action.Namespace, robotName, command.CancelAction{
		ActionID:     action.Name,
		Reason:       reason,
		FencingToken: command.FencingToken(action.Status.AssignmentGeneration),
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
func (r *FleetActionReconciler) finalizeCancel(ctx context.Context, action *fleetv1.FleetAction, original *fleetv1.FleetAction, robotName, reason string) error {
	if err := r.releaseRobotForStop(ctx, action, robotName); err != nil {
		return err
	}
	msg := "cancelled by operator"
	if reason != "" && reason != "true" {
		msg = "cancelled: " + reason
	}
	action.Status.Phase = fleetv1.ActionPhaseCancelled
	action.Status.AssignedRobot = ""
	action.Status.LeaseExpiresAt = nil
	action.Status.Message = msg
	action.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("finalising cancel: %w", err)
	}
	r.releaseReservation(ctx, action.Namespace, action.Spec.Zone, action.Name)
	r.recordActionCancelled(ctx, action, reason)
	return nil
}

// recordActionCancelled seals a FLEETACTION_CANCELLED entry into the §9.5.4 chain.
// Best-effort: a record failure is logged, never blocking the (already-finalized)
// cancellation.
func (r *FleetActionReconciler) recordActionCancelled(ctx context.Context, action *fleetv1.FleetAction, reason string) {
	if r.Audit == nil {
		return
	}
	detail := map[string]string{}
	if reason != "" && reason != "true" {
		detail["reason"] = reason
	}
	if _, err := r.Audit.Record(audit.Entry{
		EventType: audit.EventFleetActionCancelled,
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
	// assignmentGeneration deliberately preserved (read-before-issue at next commit).
	if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("finalising requeue: %w", err)
	}
	r.releaseReservation(ctx, action.Namespace, action.Spec.Zone, action.Name)

	// Clear the requeue annotation so the re-scheduled action is not requeued again.
	base := action.DeepCopy()
	delete(action.Annotations, annRequeueRequested)
	if err := r.Patch(ctx, action, client.MergeFrom(base)); err != nil {
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
	if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
		return fmt.Errorf("finalising capability-loss failure: %w", err)
	}

	base := action.DeepCopy()
	delete(action.Annotations, annRequeueRequested)
	if err := r.Patch(ctx, action, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("clearing requeue annotation: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(action, corev1.EventTypeWarning, "ActionRecoveredCapabilityLoss",
			"robot %s recovered after capability loss; task failed (%s)", robotName, reasonCapabilityLostDuringExecution)
	}
	return nil
}

// clearRequeueAnnotation drops the requeue annotation without changing phase: used
// when a capability-loss cancel found the robot had already COMPLETED the action, so
// the cancel is moot and the normal completion path settles the action.
func (r *FleetActionReconciler) clearRequeueAnnotation(ctx context.Context, action *fleetv1.FleetAction) error {
	if _, ok := action.Annotations[annRequeueRequested]; !ok {
		return nil
	}
	base := action.DeepCopy()
	delete(action.Annotations, annRequeueRequested)
	if err := r.Patch(ctx, action, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("clearing requeue annotation: %w", err)
	}
	log.FromContext(ctx).Info("capability-loss cancel: robot completed the task; no reassignment", "task", action.Name)
	return nil
}

// releaseRejectedAssignment rolls back a just-committed assignment the adapter
// rejected: the robot did not accept the action (so it is not executing), so the
// action returns to Pending — assignmentGeneration PRESERVED (next commit issues
// strictly-greater) — the robot is freed to Idle (guarded), the TDE slot is
// released, and a short backoff requeue lets the scheduler retry (possibly on
// another robot). Safe: an explicit rejection is a provable not-executing.
func (r *FleetActionReconciler) releaseRejectedAssignment(ctx context.Context, namespace string, action *fleetv1.FleetAction, robotName string) (ctrl.Result, error) {
	base := action.DeepCopy() // the committed Assigned state
	action.Status.Phase = fleetv1.ActionPhasePending
	action.Status.AssignedRobot = ""
	action.Status.LeaseExpiresAt = nil
	action.Status.Message = "adapter rejected the assignment; rescheduling"
	if err := r.Status().Patch(ctx, action, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("releasing rejected assignment: %w", err)
	}
	if err := r.releaseRobotForStop(ctx, action, robotName); err != nil {
		return ctrl.Result{}, err
	}
	r.releaseReservation(ctx, namespace, action.Spec.Zone, action.Name)
	minRetry, _ := r.tdeRetryBounds(ctx, namespace)
	return ctrl.Result{RequeueAfter: minRetry}, nil
}

// releaseRobotForStop frees a robot from a action being cancelled/requeued: it
// clears the robot's AssignedAction and returns it to Idle (from InProgress/Assigned)
// — but only if the robot still claims THIS action. Missing robot is a no-op. Called
// only once the robot is provably stopped (single-executor safety).
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
	if err := r.Status().Patch(ctx, robot, client.MergeFrom(robotOriginal)); err != nil {
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
	if err := r.releaseRobotForStop(ctx, action, action.Status.AssignedRobot); err != nil {
		return ctrl.Result{}, err
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
	// Free the robot and its zone slot first — a stopped robot must never stay
	// bound to a dead action, whether or not the action will be retried.
	r.releaseReservation(ctx, req.Namespace, action.Spec.Zone, action.Name)
	if err := r.releaseRobotForStop(ctx, action, action.Status.AssignedRobot); err != nil {
		return ctrl.Result{}, err
	}

	policy := action.Spec.OnFailure
	if policy == "" {
		policy = fleetv1.ActionFailureRequeue // CRD default; guards un-defaulted objects.
	}
	maxRetries, backoff := retryBounds(action.Spec.RetryPolicy)

	// Retry applies only to a action that was actually assigned to a robot (an
	// execution attempt). A pre-scheduling failure — deadline exceeded while still
	// Pending — is permanently unstartable (its deadline will not move), so it is
	// terminal regardless of onFailure rather than flapping Pending↔Failed until
	// retries drain. scheduledAt is set at the Assigned transition and is cleared
	// on requeue, so it is nil only for a never-assigned action.
	attempted := action.Status.ScheduledAt != nil

	if policy == fleetv1.ActionFailureRequeue && attempted && action.Status.RetryCount < maxRetries {
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
	if err := r.Status().Patch(ctx, action, client.MergeFrom(leaseCleared)); err != nil {
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
	if err := r.Status().Patch(ctx, action, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("requeueing failed task: %w", err)
	}

	if _, alerted := action.Annotations[annFailureAlerted]; alerted {
		base := action.DeepCopy()
		delete(action.Annotations, annFailureAlerted)
		if err := r.Patch(ctx, action, client.MergeFrom(base)); err != nil {
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
func retryBounds(p *fleetv1.ActionRetryPolicy) (maxRetries int32, backoff time.Duration) {
	if p == nil {
		return defaultMaxRetries, time.Duration(defaultBackoffSeconds) * time.Second
	}
	return p.MaxRetries, time.Duration(p.BackoffSeconds) * time.Second
}

// clampRetryAfter bounds a TDE RetryAfter hint into the [lo, hi] deconfliction
// backoff window (§9.1.11.10).
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
// TDE) to the Preempted phase, keeping its robot binding and lease frozen. Its
// safe eviction to Pending is then gated on the robot switching off it (see the
// Preempted case of evaluateLease). Best-effort: a missing action or patch error is
// logged, not fatal.
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
	original := victim.DeepCopy()
	victim.Status.Phase = fleetv1.ActionPhasePreempted
	victim.Status.Message = fmt.Sprintf("preempted by higher-priority task %s (zone reservation)", byAction)
	if err := r.Status().Patch(ctx, victim, client.MergeFrom(original)); err != nil {
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

// priorityLabel is the §9.3.8 assignment-latency `priority` label; an unset
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

// robotUnderEstop reports whether the robot has an ACTIVE emergency stop
// (§9.6.2.3 states Stopping or Stopped). Normal and Resuming are not active; an
// empty state is treated as Normal.
func robotUnderEstop(robot *fleetv1.Robot) bool {
	return robot.Status.EstopState == fleetv1.RobotEstopStopping ||
		robot.Status.EstopState == fleetv1.RobotEstopStopped
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
// below by 1s. A nil horizon falls back to the renew interval.
func untilLeaseHorizon(expires *metav1.Time, now time.Time) time.Duration {
	if expires == nil {
		return leaseRenewInterval
	}
	if d := expires.Time.Add(leaseClockSkew).Sub(now); d > time.Second {
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
		// Production always gates through the TDE (§9.4, TDE-1); it shares the
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
