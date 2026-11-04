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
	"sync"

	"google.golang.org/grpc/peer"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/executorguard"
	"github.com/swarmada/swarmada/internal/robotid"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ActionStatusIngestor advances the FleetAction lifecycle from a robot's reported
// action state, delivered over the ControlStream as a ActionStatusUpdate (§9.2.3).
//
// It writes the action-object side of every reported transition: RUNNING →
// InProgress (startedAt), SUCCEEDED → Succeeded (completedAt), and FAILED →
// Failed (failedAt, failureReason), or Paused when a stop covers the robot (a stop
// outcome, not a failure). It deliberately does NOT free the robot,
// drop the assignment lease, or release reservations: freeing a physical robot
// for reassignment is governed by the single-executor lease lifecycle in the
// reconciler and is applied there. Releasing the terminal action's TDE zone
// reservation is handled by the reconciler's idempotent terminal branch.
type ActionStatusIngestor struct {
	client.Client
	// Recorder emits the distinguishable supersession Event. Nil-safe: a nil
	// Recorder simply emits nothing, which is the pre-existing behaviour.
	Recorder record.EventRecorder
	// Audit seals FLEETACTION_SUPERSEDED. Nil disables audit recording.
	Audit audit.Recorder
	// APIReader reads the API server directly, bypassing the manager's cache. Retained outcomes
	// require it (none is acknowledged without it; NewActionStatusIngestor refuses nil). For a
	// live report it is optional: whether a stop covers its robot then falls back to Client.
	APIReader client.Reader

	// tokenless remembers which adapter connections were already warned that they omit the fencing
	// token.
	tokenless connMemory
}

// IngestActionStatus applies one ActionStatusUpdate to the named action. It is a no-op
// (returning nil) when the action is absent, the action is not in an advanceable phase,
// the reporting stream may not act on the action's robot, the update is fenced out, or
// the reported state is one this increment does not yet act on. Errors are returned only
// for unexpected API failures.
//
// A report whose write loses to a newer write of the action (often the controller's own lease
// renewal) is not dropped: an adapter reports a terminal state once. The action is read again
// from the API server and the whole decision is taken again on it, a bounded number of times. A
// report that is then not applied is logged, with a Warning Event on the action when it would
// have ended it.
//
// authorize is the reporting stream's per-robot check. The report names an action, but
// ending the action frees the robot bound to it, so the report may move the action only
// if the stream may act on that robot (RFC-0001
// #security-fleet-adapter-authentication-robot-identity). A nil authorize refuses.
func (i *ActionStatusIngestor) IngestActionStatus(ctx context.Context, namespace string,
	authorize controlstream.RobotAuthorization, u *fav1.ActionStatusUpdate) error {
	if u == nil || u.GetActionId() == "" {
		return nil
	}
	attempt := 0
	var action *fleetv1.FleetAction
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		attempt++
		var err error
		action, err = i.ingestOnce(ctx, namespace, authorize, u, attempt)
		return err
	})
	if apierrors.IsConflict(err) {
		i.recordReportNotApplied(ctx, action, u, fmt.Sprintf(
			"its write lost to a newer write of the action %d times", attempt))
		return nil
	}
	return err
}

// ingestOnce decides u against one read of its action and writes the outcome, returning the
// action it read. The first attempt reads through the client; a later one follows a write that
// lost to a newer object, so it reads the API server, which holds that object, and says so.
func (i *ActionStatusIngestor) ingestOnce(ctx context.Context, namespace string,
	authorize controlstream.RobotAuthorization, u *fav1.ActionStatusUpdate, attempt int) (*fleetv1.FleetAction, error) {
	action := &fleetv1.FleetAction{}
	var rd client.Reader = i.Client
	if attempt > 1 {
		log.FromContext(ctx).Info("a robot action report's write lost to a newer object; deciding it again on a fresh read",
			"action", u.GetActionId(), "namespace", namespace, "reportedState", u.GetState().String(), "attempt", attempt)
		if i.APIReader != nil {
			rd = i.APIReader
		}
	}
	if err := rd.Get(ctx, client.ObjectKey{Namespace: namespace, Name: u.GetActionId()}, action); err != nil {
		if apierrors.IsNotFound(err) && attempt > 1 {
			log.FromContext(ctx).Info("robot action report not applied: the action is gone",
				"action", u.GetActionId(), "namespace", namespace, "reportedState", u.GetState().String())
		}
		// A status update for an unknown action is stale, not an error.
		return action, client.IgnoreNotFound(err)
	}

	// Only advance a action the control plane still considers actively owned by a
	// robot. Any hold or terminal phase (Revoking/Cancelled/Succeeded/Failed) is
	// authoritative and must not be overwritten by a robot report. This comes first so
	// the checks below run only where a report could change something: a late report
	// for finished or requeued work is dropped as before, and is not sealed as a denial.
	// One exception: start evidence (refusedRunningRecordsStart), checked like any report below.
	recordsStartOnly := false
	switch action.Status.Phase {
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress:
	default:
		if recordsStartOnly = refusedRunningRecordsStart(action, u); recordsStartOnly {
			break
		}
		if attempt > 1 {
			i.recordReportNotApplied(ctx, action, u, fmt.Sprintf(
				"its write lost to a newer write of the action, which is now %s", action.Status.Phase))
		}
		return action, nil
	}

	// The stream must serve the robot this action is bound to. The robot is the one the
	// control plane wrote at assignment, never one the report supplies. A denial is sealed
	// as ROBOT_AUTHZ_DENIED by the check itself, naming the stream; here it is only logged.
	// No Event: this path is driven by whoever holds a stream, and an Event per forged
	// report would let any stream turn its messages into API writes.
	if why, ok := i.streamServesAssignedRobot(ctx, authorize, action); !ok {
		i.recordReportRefused(ctx, action, u, why, nil)
		return action, nil
	}

	// Single-executor fencing: a robot still reporting under a superseded
	// assignment must not move the current one. The report names no robot, so the token is
	// the only tie between a report and the run it came from: a report without one is refused.
	// A token that cannot be represented as a generation cannot match one either — treated as a
	// mismatch (stale) rather than wrapped into a negative that might compare equal by accident.
	// The token is a staleness check, not a credential: the stream was authorized above.
	if u.FencingToken == nil {
		i.refuseTokenless(ctx, action, u)
		return action, nil
	}
	tokenGen, representable := command.GenerationFromToken(u.GetFencingToken())
	if !representable || tokenGen != action.Status.AssignmentGeneration {
		i.recordReportRefused(ctx, action, u, fmt.Sprintf(
			"action %s: fencing token %d does not match assignment generation %d (a report from a superseded assignment)",
			action.Name, u.GetFencingToken(), action.Status.AssignmentGeneration), i.boundRobotRef(ctx, action))
		return action, nil
	}

	if recordsStartOnly {
		return i.recordStartOnly(ctx, action)
	}
	base := action.DeepCopy()
	var pausedUnder []string

	switch u.GetState() {
	case fav1.ActionState_ACTION_STATE_RUNNING:
		pct := clampPct(u.GetProgressPct())
		// Idempotent: already executing with startedAt, and neither the message nor
		// the reported progress changed → no-op (RA-1: no status write per tick).
		if action.Status.Phase == fleetv1.ActionPhaseInProgress &&
			action.Status.StartedAt != nil &&
			action.Status.ProgressPct == pct &&
			(u.GetMessage() == "" || u.GetMessage() == action.Status.Message) {
			return action, nil
		}
		action.Status.Phase = fleetv1.ActionPhaseInProgress
		if action.Status.StartedAt == nil {
			startedTS := metav1.Now()
			action.Status.StartedAt = &startedTS
		}
		action.Status.ProgressPct = pct
		if u.GetMessage() != "" {
			action.Status.Message = u.GetMessage()
		}

	case fav1.ActionState_ACTION_STATE_SUCCEEDED:
		now := metav1.Now()
		action.Status.Phase = fleetv1.ActionPhaseSucceeded
		action.Status.CompletedAt = &now
		action.Status.CompletionTime = &now
		action.Status.ProgressPct = 100
		// A action that completed within a single report may never have stamped
		// startedAt; back-fill it so the lifecycle is well-formed.
		if action.Status.StartedAt == nil {
			action.Status.StartedAt = &now
		}
		if u.GetMessage() != "" {
			action.Status.Message = u.GetMessage()
		}

	case fav1.ActionState_ACTION_STATE_FAILED:
		// A failure reported while a stop covers the robot is the stop's outcome: the stop
		// closed the robot's gate and the adapter said so. It is written Paused, in this one
		// guarded write, so the action's onFailure and its task's failure policy never see a
		// Failed, and nothing is retried on another robot while the stop stands. The binding
		// and the lease horizon are kept, as for any action a stop paused with its robot.
		//
		// A report that carries ACTION_END_CAUSE_ESTOP is the stop's outcome too, under a stop
		// or not (rows A1 and A25): the stop may be one the control plane cannot see, an edge
		// stop or one the adapter took itself, and the report is then its only sign.
		stops, err := i.stopsOver(ctx, action)
		if err != nil {
			return action, err
		}
		byEstop := u.GetEndCause() == fav1.ActionEndCause_ACTION_END_CAUSE_ESTOP
		if executorguard.StopOutcome(stops) || byEstop {
			reason := u.GetMessage()
			if reason == "" {
				reason = "robot reported action failure"
			}
			action.Status.Phase = fleetv1.ActionPhasePaused
			if len(stops) > 0 {
				action.Status.Message = failedUnderStopMessage(reason, stops)
				pausedUnder = stops
			} else {
				action.Status.Message = liveEstopEndCauseMessage
				pausedUnder = []string{"ACTION_END_CAUSE_ESTOP"}
			}
			break
		}
		now := metav1.Now()
		action.Status.Phase = fleetv1.ActionPhaseFailed
		action.Status.FailedAt = &now
		action.Status.CompletionTime = &now
		reason := u.GetMessage()
		if reason == "" {
			reason = "robot reported action failure"
		}
		action.Status.FailureReason = reason
		if u.GetMessage() != "" {
			action.Status.Message = u.GetMessage()
		}

	case fav1.ActionState_ACTION_STATE_CANCELLED:
		// D1 IN-PLACE SUPERSESSION — the one cancellation a robot report may drive.
		//
		// Every other cancellation is control-plane-driven under confirmed-stop
		// semantics and a robot report must not move it; that is still true and is
		// what the default branch below preserves. D1 is the exception BY
		// CONSTRUCTION: RFC-0001 #fleetaction-directive-supersession specifies that an
		// in-place handover sends NO cancel_action at all, so there is no confirmed
		// stop to wait for and no other path on which A could ever reach Cancelled.
		// The adapter picks the safe stopping point and reports A terminal itself.
		//
		// Gated on the adapter SAYING SO. An untyped CANCELLED report — end_cause
		// UNSPECIFIED, as every pre-RFC adapter sends — is NOT read as a supersession
		// and falls through to the default, because UNSPECIFIED is explicitly "no
		// cause stated" and not a claim of any cause.
		if u.GetEndCause() != fav1.ActionEndCause_ACTION_END_CAUSE_SUPERSEDED {
			return action, nil
		}
		// supersededBy is REQUIRED on this path. A supersession that cannot name its
		// successor is malformed (the field is what makes the distinction structural
		// rather than a label), so it is refused rather than written half-formed.
		by := u.GetSupersedingActionId()
		if by == "" {
			log.FromContext(ctx).Info(
				"ignoring ACTION_END_CAUSE_SUPERSEDED with no superseding_action_id — "+
					"a supersession that cannot name its successor is malformed",
				"action", action.Name)
			return action, nil
		}
		// The claim is VERIFIED before it may terminate anything. A report is adapter
		// input, and a terminal Cancelled here is what lets the reconciler's terminal
		// branch free the robot and release its zone slot — so an unverified claim frees
		// a slot a still-moving robot may be using, which breaks the zone-capacity
		// invariant (RFC-0001 #tde-normative-invariants) as surely as freeing a slot
		// early on any other path. A refusal is recorded, never silently dropped.
		refusal, err := i.verifySupersession(ctx, action, by)
		if err != nil {
			return action, err
		}
		if refusal != "" {
			i.recordSupersessionRefused(ctx, action, by, refusal)
			return action, nil
		}
		now := metav1.Now()
		action.Status.Phase = fleetv1.ActionPhaseCancelled
		action.Status.CompletedAt = &now
		action.Status.CompletionTime = &now
		action.Status.CancellationReason = fleetv1.CancellationReasonSupersededByDirective
		action.Status.SupersededBy = by
		action.Status.Message = "superseded in place by directive " + by
		apimeta.SetStatusCondition(&action.Status.Conditions, metav1.Condition{
			Type:    ConditionSuperseded,
			Status:  metav1.ConditionTrue,
			Reason:  ConditionReasonInPlacePreemption,
			Message: action.Status.Message,
		})

	default:
		// PAUSED / UNSPECIFIED, and an untyped CANCELLED, are not driven from a robot
		// report here: pause is a capability concern and cancellation is otherwise
		// control-plane-driven with confirmed-stop semantics.
		return action, nil
	}

	// The write carries the resourceVersion the report was read against: a report that races
	// a hold the controller wrote a moment ago (Paused, as the state table requires) must
	// not write InProgress over it from a stale copy. A conflict is
	// returned, and IngestActionStatus decides the report again on a fresh read.
	if err := i.Status().Patch(ctx, action, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return base, err
	}
	if action.Status.Phase == fleetv1.ActionPhaseCancelled {
		i.recordSupersededInPlace(ctx, action)
	}
	if len(pausedUnder) > 0 {
		log.FromContext(ctx).Info("failure reported under a stop: the action is paused, not failed",
			"action", action.Name, "robot", action.Status.AssignedRobot, "stops", pausedUnder)
		warnActionAndTask(i.Recorder, action, EventActionFailedUnderEstop, action.Status.Message)
		i.recordPausedByEstop(ctx, action, base.Status.Phase)
	}
	return action, nil
}

// recordPausedByEstop seals ACTION_PAUSED_BY_ESTOP for a pause a failure report under a stop
// wrote, as the controller does for the pauses it writes. Best-effort, as there.
func (i *ActionStatusIngestor) recordPausedByEstop(ctx context.Context, action *fleetv1.FleetAction, prior fleetv1.ActionPhase) {
	if i.Audit == nil {
		return
	}
	if _, err := i.Audit.Record(audit.Entry{
		EventType: audit.EventActionPausedByEstop,
		Namespace: action.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "actionstatus-ingestor"},
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

// stopsOver names the stops that cover the robot action is bound to, read from the API server;
// nil when none does or the robot is gone.
func (i *ActionStatusIngestor) stopsOver(ctx context.Context, action *fleetv1.FleetAction) ([]string, error) {
	rd := i.APIReader
	if rd == nil {
		rd = i.Client
	}
	robot := &fleetv1.Robot{}
	if err := rd.Get(ctx, client.ObjectKey{Namespace: action.Namespace, Name: action.Status.AssignedRobot}, robot); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading robot %s for a failure report: %w", action.Status.AssignedRobot, err)
	}
	return stopNames(ctx, rd, robot)
}

// EventActionSupersessionRefused — an adapter reported this action superseded and the
// claim did not verify, so the action was left live.
const EventActionSupersessionRefused = "ActionSupersessionRefused"

// verifySupersession checks an adapter's claim that action was superseded in place by
// the action named by. It returns a non-empty refusal when the claim does not hold, and
// an error only for an unexpected API failure.
//
// The claim holds only if the successor exists in the same namespace, is itself live
// (Assigned or InProgress), and is bound to the same robot through status.assignedRobot.
// assignedRobot is the binding because it is the field the assignment commit writes first
// and the one TDE-1 forbids anything but that commit from writing; robot.status.assignedAction
// is its mirror, written after. Liveness is required because a terminal action keeps its
// assignedRobot, so without it any finished action of the same robot would verify.
//
// No control-plane path commits an in-place handover today, and the single-executor
// commit never binds two live actions to one robot, so no report can pass this check yet.
// That is the intended outcome: the check refuses the claims the control plane never made.
func (i *ActionStatusIngestor) verifySupersession(ctx context.Context, action *fleetv1.FleetAction, by string) (string, error) {
	if by == action.Name {
		return "the report names the action itself as its successor", nil
	}
	if action.Status.AssignedRobot == "" {
		return "the superseded action is bound to no robot", nil
	}
	// The lookup is namespaced, so a successor in another namespace resolves as absent.
	successor := &fleetv1.FleetAction{}
	if err := i.Get(ctx, client.ObjectKey{Namespace: action.Namespace, Name: by}, successor); err != nil {
		if apierrors.IsNotFound(err) {
			return "no FleetAction " + by + " exists in namespace " + action.Namespace, nil
		}
		return "", fmt.Errorf("fetching superseding action %s: %w", by, err)
	}
	switch successor.Status.Phase {
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress:
	default:
		return "superseding action " + by + " is not live (phase " + string(successor.Status.Phase) + ")", nil
	}
	if successor.Status.AssignedRobot != action.Status.AssignedRobot {
		return "superseding action " + by + " is bound to robot " + successor.Status.AssignedRobot +
			", not " + action.Status.AssignedRobot, nil
	}
	return "", nil
}

// recordSupersessionRefused records a refused supersession claim where other refused
// adapter input is recorded: a Denied entry in the audit chain, beside ROBOT_AUTHZ_DENIED,
// and a log line. It also emits a Warning Event on the action, so an operator reading
// `kubectl describe fleetaction` sees that a claim arrived and why it was refused. It
// writes no status: the action is unchanged, so there is nothing to write (RA-1).
//
// The ingestor is not given the reporting stream's identity, only its authorization, so the
// entry cannot name the stream. The stream was authorized for the action's robot before
// this claim was read; a stream that was not is refused earlier and sealed as
// ROBOT_AUTHZ_DENIED by the stream's check.
func (i *ActionStatusIngestor) recordSupersessionRefused(ctx context.Context, action *fleetv1.FleetAction, by, refusal string) {
	msg := "refused adapter report that this action was superseded by " + by + ": " + refusal
	log.FromContext(ctx).Info("refusing ACTION_END_CAUSE_SUPERSEDED report",
		"action", action.Name, "namespace", action.Namespace, "supersedingActionID", by, "reason", refusal)
	if i.Recorder != nil {
		i.Recorder.Event(action, corev1.EventTypeWarning, EventActionSupersessionRefused, msg)
	}
	if i.Audit == nil {
		return
	}
	if _, err := i.Audit.Record(audit.Entry{
		EventType: audit.EventFleetActionSuperseded,
		Namespace: action.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "actionstatus-ingestor"},
		Resource:  audit.Resource{Kind: "FleetAction", Namespace: action.Namespace, Name: action.Name},
		Action:    "supersede",
		Outcome:   audit.OutcomeDenied,
		Detail:    map[string]string{"claimedSupersededBy": by, "reason": refusal},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording refused FLEETACTION_SUPERSEDED audit entry",
			"action", action.Name)
	}
}

// EventActionReportRefused — a robot's report about this action was refused (the reporting
// stream may not act on the action's robot, or the report was fenced out), so the action
// was left as it was.
const EventActionReportRefused = "ActionReportRefused"

// recordReportRefused makes a refused report visible, in the shape recordSupersessionRefused
// uses: a log line and, when on is set and the report would have ended the action, a Warning
// Event on on. It writes no status (RA-1): the action is unchanged, so there is nothing to write.
//
// A non-terminal report (RUNNING) is logged at V(1) and never gets an Event: an adapter
// reports progress at telemetry cadence, and an Event per refused tick would turn a
// refusal into an API write per tick. The audit entry for a stream-authorization refusal
// is written by the stream's check (ROBOT_AUTHZ_DENIED), which names the stream; the
// ingestor cannot.
func (i *ActionStatusIngestor) recordReportRefused(ctx context.Context, action *fleetv1.FleetAction,
	u *fav1.ActionStatusUpdate, reason string, on client.Object) {
	logger := log.FromContext(ctx).WithValues("action", action.Name, "namespace", action.Namespace,
		"reportedState", u.GetState().String(), "reason", reason)
	if !isTerminalReport(u.GetState()) {
		logger.V(1).Info("refusing robot action report")
		return
	}
	logger.Info("refusing robot action report")
	if on != nil && i.Recorder != nil {
		i.Recorder.Event(on, corev1.EventTypeWarning, EventActionReportRefused,
			"refused a "+u.GetState().String()+" report: "+reason)
	}
}

// EventActionReportNotApplied — a robot's report about this action lost its write to a newer
// write of the action and was not applied: read again, the action was in a phase the report may
// not leave, or every retry lost again.
const EventActionReportNotApplied = "ActionReportNotApplied"

// recordReportNotApplied makes a report that lost its write and was then not applied visible: a
// log line at info and, when the report would have ended the action, a Warning Event on it. A
// running report gets no Event, as in recordReportRefused. A phase that is already the report's
// own outcome (another delivery of the same report was written first) is logged only.
func (i *ActionStatusIngestor) recordReportNotApplied(ctx context.Context, action *fleetv1.FleetAction,
	u *fav1.ActionStatusUpdate, reason string) {
	log.FromContext(ctx).Info("robot action report not applied", "action", action.Name,
		"namespace", action.Namespace, "phase", action.Status.Phase, "reportedState", u.GetState().String(),
		"reason", reason)
	if i.Recorder == nil || !isTerminalReport(u.GetState()) || action.Status.Phase == reportedPhase(u.GetState()) {
		return
	}
	i.Recorder.Event(action, corev1.EventTypeWarning, EventActionReportNotApplied,
		"the robot's "+u.GetState().String()+" report was not applied: "+reason)
}

// reportedPhase is the phase a terminal report ends the action in when no stop covers its robot.
func reportedPhase(state fav1.ActionState) fleetv1.ActionPhase {
	switch state {
	case fav1.ActionState_ACTION_STATE_SUCCEEDED:
		return fleetv1.ActionPhaseSucceeded
	case fav1.ActionState_ACTION_STATE_FAILED:
		return fleetv1.ActionPhaseFailed
	case fav1.ActionState_ACTION_STATE_CANCELLED:
		return fleetv1.ActionPhaseCancelled
	default:
		return ""
	}
}

// EventAdapterOmitsFencingToken is the Event reason, on the FleetAdapter, when a report it sent
// carried no fencing token (conformance C6.4). It is raised for every terminal report without one,
// and once per adapter connection for a running report without one.
const EventAdapterOmitsFencingToken = "AdapterOmitsFencingToken"

// refuseTokenless refuses a report that carries no fencing token: the report names no robot, so
// the token is the only tie between it and the run it came from. Every refusal is logged. A
// Warning Event on the adapter's FleetAdapter names it: for every refused terminal report, which
// would have ended the action, and for the first refused running report on each connection, so an
// adapter that never echoes the token on progress is visible without an Event per report.
func (i *ActionStatusIngestor) refuseTokenless(ctx context.Context, action *fleetv1.FleetAction, u *fav1.ActionStatusUpdate) {
	id := streamIdentity(ctx)
	conn := ""
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		conn = p.Addr.String()
	}
	log.FromContext(ctx).Info("refusing robot action report: no fencing token", "action", action.Name,
		"namespace", action.Namespace, "reportedState", u.GetState().String(), "adapter", id.AdapterName, "peer", conn)
	if i.Recorder == nil {
		return
	}
	if !isTerminalReport(u.GetState()) && !i.tokenless.first(action.Namespace+"/"+id.AdapterName, conn) {
		return
	}
	var on client.Object = action
	name := id.AdapterName
	adapter := &fleetv1.FleetAdapter{}
	switch {
	case name == "":
		name = "(unidentified)"
	case i.Get(ctx, client.ObjectKey{Namespace: action.Namespace, Name: name}, adapter) == nil:
		on = adapter
	}
	i.Recorder.Eventf(on, corev1.EventTypeWarning, EventAdapterOmitsFencingToken,
		"adapter %s sent a %s report for action %s without a fencing token; reports without one are refused. "+
			"Conformance C6.4 requires an adapter to echo the fencing token on every report", name,
		u.GetState().String(), action.Name)
}

// streamIdentity is the reporting adapter's authenticated identity (a variable so a test can give one).
var streamIdentity = controlstream.IdentityFromContext

// connMemory remembers, per adapter, the connection it was last warned about.
type connMemory struct {
	mu   sync.Mutex
	seen map[string]string
}

// first reports whether conn is a connection of adapter not yet warned about, and records it.
func (m *connMemory) first(adapter, conn string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.seen[adapter]; ok && prev == conn {
		return false
	}
	if m.seen == nil {
		m.seen = map[string]string{}
	}
	m.seen[adapter] = conn
	return true
}

// isTerminalReport reports whether state would end the action if it were applied.
func isTerminalReport(state fav1.ActionState) bool {
	switch state {
	case fav1.ActionState_ACTION_STATE_SUCCEEDED, fav1.ActionState_ACTION_STATE_FAILED,
		fav1.ActionState_ACTION_STATE_CANCELLED:
		return true
	default:
		return false
	}
}

// streamServesAssignedRobot asks authorize whether the reporting stream may act on the
// robot the action is assigned to, and says why not when it may not. The assignment names
// the Robot object; the stream authorizer judges the robot_id the adapter announced
// (robotid.Wire), and the two differ for a robot admitted under another name
// (RFC-0001 #security-fleet-adapter-authentication-robot-identity). A Robot that cannot be
// read is refused: there is no robot_id to ask about, and the name is not one.
func (i *ActionStatusIngestor) streamServesAssignedRobot(ctx context.Context,
	authorize controlstream.RobotAuthorization, action *fleetv1.FleetAction) (string, bool) {
	name := action.Status.AssignedRobot
	if authorize == nil {
		return "the reporting stream is not authorized for robot " + name, false
	}
	robot := &fleetv1.Robot{}
	if err := i.Get(ctx, client.ObjectKey{Namespace: action.Namespace, Name: name}, robot); err != nil {
		return fmt.Sprintf("the action's robot %s could not be read to authorize the report: %v", name, err), false
	}
	if !authorize(ctx, robotid.Wire(robot)) {
		return "the reporting stream is not authorized for robot " + name, false
	}
	return "", true
}

// clampPct bounds a reported progress percentage into [0,100]; a malformed
// out-of-range report is clamped rather than rejected.
func clampPct(p int32) int32 {
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	default:
		return p
	}
}

// recordSupersededInPlace emits the Event and seals the audit entry for a D1 handover.
// Best-effort on both: the transition is already written, and losing a notification
// must not undo it.
func (i *ActionStatusIngestor) recordSupersededInPlace(ctx context.Context, action *fleetv1.FleetAction) {
	if i.Recorder != nil {
		// Normal, not Warning: an in-place handover is the fleet working as designed.
		i.Recorder.Event(action, corev1.EventTypeNormal, EventActionSupersededInPlace,
			action.Status.Message)
	}
	if i.Audit == nil {
		return
	}
	if _, err := i.Audit.Record(audit.Entry{
		EventType: audit.EventFleetActionSuperseded,
		Namespace: action.Namespace,
		Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: "actionstatus-ingestor"},
		Resource:  audit.Resource{Kind: "FleetAction", Namespace: action.Namespace, Name: action.Name},
		Action:    "supersede",
		Outcome:   audit.OutcomeAllowed,
		Detail:    map[string]string{"supersededBy": action.Status.SupersededBy},
	}); err != nil {
		log.FromContext(ctx).Error(err, "recording FLEETACTION_SUPERSEDED audit entry",
			"action", action.Name)
	}
}

// refusedRunningRecordsStart reports whether a report the phase check refuses still records the
// start: a running report, for work not yet recorded as started, that has already left Assigned
// while it is still bound to its robot (Paused, Preempted, Revoking, or Failed with its robot
// kept by StopUnconfirmed). The stream and token checks still apply before anything is written.
func refusedRunningRecordsStart(action *fleetv1.FleetAction, u *fav1.ActionStatusUpdate) bool {
	if u.GetState() != fav1.ActionState_ACTION_STATE_RUNNING || action.Status.StartedAt != nil ||
		action.Status.AssignedRobot == "" {
		return false
	}
	switch action.Status.Phase {
	case fleetv1.ActionPhasePaused, fleetv1.ActionPhasePreempted, fleetv1.ActionPhaseRevoking:
		return true
	case fleetv1.ActionPhaseFailed:
		return action.Status.FailureReason == reasonStopUnconfirmed
	}
	return false
}

// recordStartOnly writes startedAt, and nothing else, for a running report that
// refusedRunningRecordsStart admits. The write carries the resourceVersion the report was read
// against, as every report write does.
func (i *ActionStatusIngestor) recordStartOnly(ctx context.Context, action *fleetv1.FleetAction) (*fleetv1.FleetAction, error) {
	base := action.DeepCopy()
	startedTS := metav1.Now()
	action.Status.StartedAt = &startedTS
	if err := i.Status().Patch(ctx, action, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return base, err
	}
	log.FromContext(ctx).Info("running report refused in this phase: the start is recorded, no phase changes",
		"action", action.Name, "phase", action.Status.Phase, "robot", action.Status.AssignedRobot)
	return action, nil
}

// boundRobotRef is the Robot a fenced-out report's refusal is raised on: the robot the action is
// bound to, which the reporting stream was authorized for. A report from another run of the
// action does not belong on the action, which is often a later run created under the same name;
// the operator looks at the robot first. Read from the cache; a robot that cannot be read is
// named by namespace and name only.
func (i *ActionStatusIngestor) boundRobotRef(ctx context.Context, action *fleetv1.FleetAction) *fleetv1.Robot {
	robot := &fleetv1.Robot{}
	if err := i.Get(ctx, client.ObjectKey{Namespace: action.Namespace, Name: action.Status.AssignedRobot}, robot); err != nil {
		return &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Namespace: action.Namespace, Name: action.Status.AssignedRobot}}
	}
	return robot
}

// NewActionStatusIngestor builds the ingestor the manager runs. apiReader is required: a retained
// outcome is decided only on the API server's copy of its action (IngestRetainedOutcome), and an
// ingestor without one would acknowledge none.
func NewActionStatusIngestor(c client.Client, apiReader client.Reader, rec record.EventRecorder,
	aud audit.Recorder) (*ActionStatusIngestor, error) {
	if apiReader == nil {
		return nil, errors.New("action status ingestor: an API reader is required for retained outcomes")
	}
	return &ActionStatusIngestor{Client: c, APIReader: apiReader, Recorder: rec, Audit: aud}, nil
}
