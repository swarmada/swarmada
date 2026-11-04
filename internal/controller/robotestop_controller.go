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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

// RobotEstopReconciler drives ROBOT-scope emergency stops (estop-protocol estop
// scopes): a `swarmada.io/estop-triggered` annotation on a single Robot confirmed-
// estops that one robot via the same primitive the zone fan-out uses, and removing
// the annotation (the operator-authorized estop-clear verb) moves it to Resuming. A
// robot leaves Resuming only on an operator's request (fleetv1.EstopResumeAnnotation),
// which this controller hands to the dispatcher once. It
// reuses the annEstopTriggered/annEstopProcessed idempotency markers, so a
// re-reconcile of the same trigger value is a no-op and a new value re-fires. Both
// annotation writes are SAR-gated at admission by the Robot validating webhook
// (RFC-0001 #security-authorization-swarmada-rbac-roles), so an unauthorized user cannot trigger or clear a robot estop.
type RobotEstopReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// now overrides the clock in tests. Nil means time.Now.
	now func() time.Time
	// Estopper issues the confirmed per-robot estop. Nil disables robot estop.
	Estopper ZoneEstopper
	// Resumer releases a robot from the post-clear supervised window when an operator
	// asks and the robot passes its checks (RFC-0001 #safety-estop-state-machine). Nil
	// refuses every request, and a cleared robot stays Resuming — withheld from
	// dispatch — which is the fail-safe direction.
	Resumer ResumeConfirmer
	// Audit records the ESTOP_TRIGGERED / ESTOP_CLEARED / ESTOP_RESUME_* safety events.
	// Nil skips it.
	Audit audit.Recorder
	// Recorder emits the EstopResumeConfirmed / EstopResumeRefused Events that tell the
	// operator what became of a resume request. Nil skips them.
	Recorder record.EventRecorder
	// APIReader reads the API server directly, bypassing the manager's cache. A resume
	// request is re-read through it before it is claimed, so the claim is written against
	// the stored Robot and a second reconcile cannot claim it again. Nil falls back to
	// Client.
	APIReader client.Reader

	// answered holds, per robot, the last resume request this process claimed and answered,
	// so a claim whose drop failed, or that a stale cached read still shows, is not reported
	// as a request left unanswered.
	answered sync.Map
}

// ResumeConfirmer releases a robot from the post-clear estop window on an operator's
// request. [github.com/swarmada/swarmada/internal/safety.Dispatcher] satisfies it, and
// the write stays there rather than here on purpose: RFC-0001
// #safety-estop-state-machine requires Robot.status.estopState to have a SINGLE
// writer — the estop dispatcher on the elected leader — which is what makes the
// per-robot estop epoch total. This controller relays the request; the dispatcher
// writes.
type ResumeConfirmer interface {
	// ConfirmResume moves a Resuming robot to Normal when the robot passes the
	// release checks, and otherwise leaves it in Resuming and returns why. It is a
	// no-op, with the reason, for a robot that is not Resuming.
	ConfirmResume(ctx context.Context, namespace, robotID, confirmedBy string) (fleetv1.RobotEstopState, string, error)
}

// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch;update;patch

// Reconcile issues a robot estop when the trigger annotation is (newly) present and
// clears it when the annotation is removed, then handles an operator's resume request.
func (r *RobotEstopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	robot := &fleetv1.Robot{}
	if err := r.Get(ctx, req.NamespacedName, robot); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A new stop, or a clear, comes before any resume request: a request whose handling
	// cannot be written must never stand in the way of a stop (RFC-0001
	// #safety-emergency-stop-protocol). A request is then answered against the stop.
	if r.Estopper != nil {
		trigger, firing := robot.Annotations[annEstopTriggered]
		processed, wasProcessed := robot.Annotations[annEstopProcessed]
		// The stop finalizer on the robot: held from its own trigger until the clear has been
		// applied, and while the robot holds a record of work a stop holds, so the record
		// survives any delete path (estopscope.StopFinalizer, robotKeepsStopFinalizer).
		keep, err := robotKeepsStopFinalizer(ctx, r.reader(), robot)
		if err != nil {
			return ctrl.Result{}, err
		}
		if done, err := holdStopFinalizer(ctx, r.Client, r.reader(), robot, keep); done || err != nil {
			return ctrl.Result{}, err
		}
		switch {
		case firing && (!wasProcessed || processed != trigger):
			// The marker's presence is compared as well as its value: an absent marker
			// reads as "", and an empty trigger is a stop like any other.
			return r.issueRobotEstop(ctx, robot, trigger)
		case !firing && wasProcessed:
			return r.clearRobotEstop(ctx, robot)
		}
	}

	// A request claimed and never answered: the control plane stopped, or a write failed,
	// between the claim and the answer. It is closed; the operator is told to re-issue it.
	if claimed, ok := robot.Annotations[fleetv1.EstopResumeClaimedAnnotation]; ok {
		return r.closeClaim(ctx, robot, claimed)
	}

	// An operator's request to release the robot from the post-clear window. It is
	// the ONLY thing that leaves Resuming: nothing here or in the dispatcher releases a
	// robot on its own (RFC-0001 #safety-estop-state-machine). A request is handled
	// at most once, whatever the robot's state, so one written while the robot was not
	// Resuming, or refused, never releases a later window.
	//
	// This covers all three estop scopes. A zone- or namespace-scope clear fans out to
	// a per-robot ClearEstop, so every cleared robot lands in Resuming on its own
	// status and is released per robot.
	if request, pending := pendingEstopResume(robot.Annotations); pending {
		return r.handleResumeRequest(ctx, robot, request)
	}
	return ctrl.Result{}, nil
}

// issueRobotEstop sends the robot's own stop and records it as issued only once its outcome
// is written to estopState. An outcome that could not be written is audited as such and
// returned, without the marker, so the stop is issued again (RFC-0001
// #safety-estop-state-machine): until then the robot's own trigger withholds it from
// dispatch and pauses its work.
func (r *RobotEstopReconciler) issueRobotEstop(ctx context.Context, robot *fleetv1.Robot, trigger string) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("robot", client.ObjectKeyFromObject(robot))
	reason := estopReason(trigger, "robot emergency stop")
	actor := estopActor(robot.Annotations, "robot-estop:"+robot.Name)
	issuedBy := actor.Identity

	sentAt := r.clock()
	// Robot scope: a single stop, no fan-out — deliberately NOT observed on the fan-out
	// histogram, whose population must stay "episodes that fan out" for its quantiles to
	// mean anything.
	res, terr := r.Estopper.TriggerEstop(ctx, robot.Namespace, robot.Name, reason, issuedBy,
		metrics.ScopeRobot, safety.Carrier{Kind: fleetv1.StopCarrierRobot, Name: robot.Name, UID: string(robot.UID)})
	unwritten := errors.Is(terr, safety.ErrStateUnwritten)
	confirmed := terr == nil && res.State == fleetv1.RobotEstopStopped
	if !confirmed {
		logger.Info("robot estop not confirmed (escalate)", "robot", robot.Name,
			"state", res.State, "err", errString(terr))
	}

	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if !confirmed {
			outcome = audit.OutcomeError
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: robot.Namespace,
			EventType: audit.EventEstopTriggered,
			Action:    "estop-trigger",
			Outcome:   outcome,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
			// The RFC-0001 #safety-timing-requirements SLA is measured on this round trip, so the audit chain — not
			// only Prometheus — carries the evidence a post-incident review needs. A
			// latency of 0 means no ack came back, which is why delivered is recorded
			// separately rather than inferred from the number. state_written says whether
			// the state named here reached Robot.status.
			Detail: estopTimingDetail(map[string]string{
				"reason": reason, "state": string(res.State), "state_written": strconv.FormatBool(!unwritten),
			}, sentAt, res),
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_TRIGGERED audit entry")
		}
	}
	if unwritten {
		return ctrl.Result{}, fmt.Errorf("robot %s estop: %w; issuing it again", robot.Name, terr)
	}

	base := robot.DeepCopy()
	if robot.Annotations == nil {
		robot.Annotations = map[string]string{}
	}
	robot.Annotations[annEstopProcessed] = trigger
	// A pending-firmware instruction does not stand on a stopped robot (the stop guard).
	delete(robot.Annotations, annPendingFirmwareVersion)
	delete(robot.Annotations, annPendingFirmwareURI)
	delete(robot.Annotations, annPendingFirmwareChecksum)
	consumeEstopActor(robot.Annotations)
	if err := r.Patch(ctx, robot, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording robot estop processed: %w", err)
	}
	return ctrl.Result{}, nil
}

// clearRobotEstop moves the robot to Resuming after the operator removes the trigger
// annotation (estop-clear). Idempotent — a robot not under an estop is a no-op; a
// action Paused by the estop stays operator-gated (RFC-0001 #safety-fleetaction-behaviour-during-estop).
//
// A robot that a zone's stop or its namespace's stop still holds is not moved: the robot's
// own stop is cleared (its marker goes, and the clear is recorded), and the robot stays as
// it is until the clear of the stop that holds it, which then reaches it (RFC-0001
// #safety-estop-scopes). A recorded stop whose carrier no longer exists cannot be cleared
// by its own clear; this clear releases it (estopscope.OrphanedRelease), and the audit entry
// names it.
func (r *RobotEstopReconciler) clearRobotEstop(ctx context.Context, robot *fleetv1.Robot) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	actor := estopActor(robot.Annotations, "robot-estop:"+robot.Name)
	clearedBy := actor.Identity
	zones, configs, err := readStandingStops(ctx, r.reader(), robot.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	var live []estopscope.Holder
	var orphaned []string
	keep := map[string]bool{}
	for _, h := range estopscope.Holders(robot, zones, configs) {
		switch {
		case h.Orphaned:
			orphaned = append(orphaned, string(h.Kind)+"/"+h.Name)
		default:
			live = append(live, h)
			if h.Recorded {
				keep[string(h.Kind)+"/"+h.Name] = true
			}
		}
	}
	detail := map[string]string{} // none on a robot-scope clear, unless another stop holds the robot
	if len(live) > 0 {
		detail["held_by"] = live[0].String()
		heldByStandingStop(r.Recorder, robot, "robot", live[0])
	} else if _, cerr := r.Estopper.ClearEstop(ctx, robot.Namespace, robot.Name, clearedBy,
		clearEnds(nil, func(e fleetv1.EstopHold) bool { return !keep[string(estopscope.HoldKind(e.Kind))+"/"+e.Name] },
			zones, configs)); cerr != nil {
		return ctrl.Result{}, fmt.Errorf("clearing robot %s estop: %w", robot.Name, cerr)
	}
	// Every record this clear may end: a stop whose carrier no longer exists, and a record
	// whose carrier has been cleared. A record of a stop that still holds is kept.
	if err := dropHolds(ctx, r.Client, r.reader(), robot.Namespace, robot.Name, func(e fleetv1.EstopHold) bool {
		return !keep[string(estopscope.HoldKind(e.Kind))+"/"+e.Name]
	}); err != nil {
		return ctrl.Result{}, err
	}
	if len(orphaned) > 0 {
		detail["released_orphaned_holds"] = strings.Join(orphaned, ",")
	}
	if len(detail) == 0 {
		detail = nil
	}

	if r.Audit != nil {
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: robot.Namespace,
			EventType: audit.EventEstopCleared,
			Action:    "estop-clear",
			Outcome:   audit.OutcomeAllowed,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
			Detail:    detail,
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_CLEARED audit entry")
		}
	}

	base := robot.DeepCopy()
	delete(robot.Annotations, annEstopProcessed)
	consumeEstopActor(robot.Annotations)
	keepFinalizer, ferr := robotKeepsStopFinalizer(ctx, r.reader(), robot)
	if ferr != nil {
		return ctrl.Result{}, ferr
	}
	if !keepFinalizer {
		controllerutil.RemoveFinalizer(robot, estopscope.StopFinalizer)
	}
	if err := r.Patch(ctx, robot, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing robot estop processed marker: %w", err)
	}
	return ctrl.Result{}, nil
}

// reader is the uncached reader when one is wired, else the client.
func (r *RobotEstopReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// pendingEstopResume returns the operator's resume request awaiting action, and
// whether there is one. A request whose value is already in the processed marker has
// been handled.
func pendingEstopResume(annotations map[string]string) (string, bool) {
	req, requested := annotations[fleetv1.EstopResumeAnnotation]
	if !requested {
		return "", false
	}
	if annotations[fleetv1.EstopResumeProcessedAnnotation] == req {
		return "", false
	}
	return req, true
}

// handleResumeRequest claims one operator resume request, hands it to the dispatcher, tells
// the operator the outcome (an Event, and the safety audit log), and marks it handled. It
// writes no status: the estopState write belongs to the dispatcher's single-writer path.
//
// The request is claimed first: the claim is written against the stored Robot, under an
// optimistic lock, before anything is evaluated. A claim that cannot be written is
// returned and retried, and nothing has been evaluated. A claimed request is evaluated
// once, and never again: a refused release is closed, not queued (RFC-0001
// #safety-estop-state-machine), so a retry can never release the robot later, at a moment
// nobody chose. If the answer is lost (the control plane stops between the claim and the
// answer), the claim is left on the Robot and the next reconcile reports it (closeClaim).
//
// The processed marker is written last, after the dispatcher has written the outcome, so a
// reader that sees the request handled reads the estopState that answered it (swarmctl
// estop resume judges on that read).
func (r *RobotEstopReconciler) handleResumeRequest(ctx context.Context, robot *fleetv1.Robot, request string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	reader := r.reader()
	fresh := &fleetv1.Robot{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(robot), fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// A claim the API server holds and the cached copy did not show yet: it is closed,
	// never evaluated again.
	if claimed, ok := fresh.Annotations[fleetv1.EstopResumeClaimedAnnotation]; ok {
		return r.closeClaim(ctx, fresh, claimed)
	}
	if req, pending := pendingEstopResume(fresh.Annotations); !pending || req != request {
		return ctrl.Result{}, nil
	}
	actor := estopActor(fresh.Annotations, "robot-estop-resume:"+fresh.Name)

	base := fresh.DeepCopy()
	fresh.Annotations[fleetv1.EstopResumeClaimedAnnotation] = request
	consumeEstopActor(fresh.Annotations)
	if err := r.Patch(ctx, fresh, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, fmt.Errorf("claiming estop resume request: %w", err)
	}
	robot = fresh

	// A robot another stop still holds is not released, whatever moved it to Resuming:
	// its own robot-scope trigger, a zone's or the namespace's stop it records or the tree
	// puts it under (RFC-0001 #safety-estop-scopes). The request is answered and closed
	// like any refusal.
	state, why := robot.Status.EstopState, "resume confirmation is not configured on this control plane"
	zones, configs, serr := readStandingStops(ctx, reader, robot.Namespace)
	by, held := estopscope.StandingStop(robot, zones, configs)
	switch {
	case serr != nil:
		why = "the request could not be evaluated: " + serr.Error()
	case held && by.Orphaned:
		why = "it is still held by " + by.String() + "; " + estopscope.OrphanedRelease(robot.Name, robot.Namespace)
	case held:
		why = "it is still held by " + by.String()
	case r.Resumer != nil:
		var err error
		state, why, err = r.Resumer.ConfirmResume(ctx, robot.Namespace, robot.Name, actor.Identity)
		if err != nil {
			state, why = robot.Status.EstopState, "the request could not be evaluated: "+err.Error()
		}
	}
	released := state == fleetv1.RobotEstopNormal && robot.Status.EstopState == fleetv1.RobotEstopResuming

	eventType, event, reason := corev1.EventTypeWarning, audit.EventEstopResumeRefused, "EstopResumeRefused"
	msg := fmt.Sprintf("resume refused; the robot stays %s: %s", stateOrNormal(state), why)
	if released {
		eventType, event, reason = corev1.EventTypeNormal, audit.EventEstopResumeConfirmed, "EstopResumeConfirmed"
		msg = "resume confirmed; the robot is Normal: " + why
	}
	if r.Recorder != nil {
		r.Recorder.Event(robot, eventType, reason, msg)
	}
	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if !released {
			outcome = audit.OutcomeDenied
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: robot.Namespace,
			EventType: event,
			Action:    "estop-resume",
			Outcome:   outcome,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "Robot", Namespace: robot.Namespace, Name: robot.Name},
			Detail: map[string]string{
				"reason":   request,
				"evidence": why,
			},
		}); aerr != nil {
			logger.Error(aerr, "recording estop resume audit entry")
		}
	}

	r.answered.Store(answeredKey(robot), request)
	if err := r.closeRequest(ctx, robot, request); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// closeClaim closes a claim left on the Robot. It acts only on the claim the API server
// still stores: a cached copy can show a claim already dropped. One this process answered
// is closed silently; any other was claimed and never answered, and the operator is told
// so, once.
func (r *RobotEstopReconciler) closeClaim(ctx context.Context, robot *fleetv1.Robot, request string) (ctrl.Result, error) {
	fresh := &fleetv1.Robot{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(robot), fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if fresh.Annotations[fleetv1.EstopResumeClaimedAnnotation] != request {
		return ctrl.Result{}, nil
	}
	robot = fresh
	if done, _ := r.answered.Load(answeredKey(robot)); done != request && r.Recorder != nil {
		r.Recorder.Event(robot, corev1.EventTypeWarning, "EstopResumeUnanswered", fmt.Sprintf(
			"the resume request %q was claimed but not answered (the control plane stopped, or a write failed, before "+
				"it was evaluated); it is closed and the robot stays %s: re-issue it with swarmctl estop resume",
			request, stateOrNormal(robot.Status.EstopState)))
	}
	if err := r.closeRequest(ctx, robot, request); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// closeRequest marks request handled and drops its claim, in one write, when the claim
// still names it. It is the request's last write: a reader that sees the processed marker
// reads whatever estopState answered the request.
func (r *RobotEstopReconciler) closeRequest(ctx context.Context, robot *fleetv1.Robot, request string) error {
	if robot.Annotations[fleetv1.EstopResumeClaimedAnnotation] != request {
		return nil
	}
	base := robot.DeepCopy()
	robot.Annotations[fleetv1.EstopResumeProcessedAnnotation] = request
	delete(robot.Annotations, fleetv1.EstopResumeClaimedAnnotation)
	if err := r.Patch(ctx, robot, client.MergeFrom(base)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("closing estop resume request: %w", err)
	}
	return nil
}

func answeredKey(robot *fleetv1.Robot) string { return robot.Namespace + "/" + robot.Name }

// stateOrNormal names an unset estopState as the Normal it means.
func stateOrNormal(s fleetv1.RobotEstopState) fleetv1.RobotEstopState {
	if s == "" {
		return fleetv1.RobotEstopNormal
	}
	return s
}

// SetupWithManager registers the robot-estop controller under a distinct name so it
// coexists with the capability-deriving Robot reconciler on the same resource.
func (r *RobotEstopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.Robot{}).
		Named("robotestop").
		Complete(r)
}

// estopTimingDetail adds the RFC-0001 #safety-required-events estop timing fields to an audit detail map.
//
// Values are strings because audit.Entry.Detail is map[string]string — a flat string
// map keeps the hash chain free of number-formatting ambiguity (RFC-0001 #safety-log-entry-format). Timestamps
// use RFC3339 with nanoseconds so a sub-millisecond round trip is not rounded away.
func estopTimingDetail(detail map[string]string, sentAt time.Time, res safety.Result) map[string]string {
	detail["rpc_sent_at"] = sentAt.UTC().Format(time.RFC3339Nano)
	detail["delivered"] = strconv.FormatBool(res.Delivered)
	if res.Delivered {
		detail["ack_received_at"] = sentAt.Add(res.Latency).UTC().Format(time.RFC3339Nano)
		detail["ack_latency_ms"] = strconv.FormatInt(res.Latency.Milliseconds(), 10)
		detail["latency_violation"] = strconv.FormatBool(res.LatencyViolation)
	}
	return detail
}

// clock is the reconciler's time source; tests substitute it so an audit entry's
// timestamps are deterministic.
func (r *RobotEstopReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
