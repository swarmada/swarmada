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
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Directive supersession (RFC-0001 #fleetaction-directive-supersession).
//
// The property under test throughout is the one the RFC states as a MUST and that
// v0.4 could not satisfy: a supersession and an operator cancellation both end at
// phase `Cancelled`, so the phase CANNOT tell them apart and an implementation must
// not let it try. Every assertion below is therefore about what distinguishes them —
// status.cancellationReason, status.supersededBy, the Superseded condition, the
// Kubernetes Event reason, and the cause on the wire — and never about the phase.

// ---------------------------------------------------------------------------
// The disposition rule itself. A pure function, so it needs no cluster, no robot
// and no wire: "same type in place, different type cancel first".
// ---------------------------------------------------------------------------

func TestDispositionFor_SameTypeIsInPlace(t *testing.T) {
	a := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}}
	b := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}}
	if got := DispositionFor(a, b); got != SupersedeInPlace {
		t.Fatalf("Navigate superseded by Navigate = %v, want in-place: a same-type directive "+
			"reaches the same executor, so the robot's physical commitment carries over and "+
			"no cancel_action is sent", got)
	}
	if got := DispositionFor(a, b).String(); got != ConditionReasonInPlacePreemption {
		t.Errorf("condition reason = %q, want %q", got, ConditionReasonInPlacePreemption)
	}
}

func TestDispositionFor_TypeChangeRequiresCancelFirst(t *testing.T) {
	a := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate}}
	b := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{Type: fleetv1.ActionTypePickUp}}
	if got := DispositionFor(a, b); got != SupersedeCancelThenDispatch {
		t.Fatalf("Navigate superseded by PickUp = %v, want cancel-then-dispatch: a type change "+
			"means a different executor, and handing a committed robot to one without a "+
			"confirmed stop is the dual-execution hazard the lease exists to exclude", got)
	}
	if got := DispositionFor(a, b).String(); got != ConditionReasonCancelThenDispatch {
		t.Errorf("condition reason = %q, want %q", got, ConditionReasonCancelThenDispatch)
	}
}

// The rule is on spec.type ALONE. Priority is the input to the OTHER preemption
// (which requeues its victim); reading it here would conflate the two mechanisms.
func TestDispositionFor_IgnoresPriority(t *testing.T) {
	a := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{
		Type: fleetv1.ActionTypePatrol, Priority: fleetv1.ActionPriorityLow}}
	b := &fleetv1.FleetAction{Spec: fleetv1.FleetActionSpec{
		Type: fleetv1.ActionTypePatrol, Priority: fleetv1.ActionPriorityCritical}}
	if got := DispositionFor(a, b); got != SupersedeInPlace {
		t.Errorf("a Critical directive of the SAME type = %v, want in-place — the disposition "+
			"is selected on spec.type alone; priority decides whether a preemptor may take "+
			"the robot, not how the handover reaches it", got)
	}
}

// A nil operand is not a same-type match. Defaulting to in-place would be the
// permissive direction, which is the wrong one: in-place skips the confirmed stop.
func TestDispositionFor_NilDefaultsToTheSafeDisposition(t *testing.T) {
	if got := DispositionFor(nil, nil); got != SupersedeCancelThenDispatch {
		t.Errorf("DispositionFor(nil, nil) = %v, want cancel-then-dispatch: an unknown type "+
			"must not take the path that skips the confirmed stop", got)
	}
}

// ---------------------------------------------------------------------------
// E-STOP IS NOT A PREEMPTION. The guard that stops the fleet silently resolving
// work a human stopped.
// ---------------------------------------------------------------------------

func TestMaySupersede_PausedActionIsNotSupersedable(t *testing.T) {
	paused := &fleetv1.FleetAction{Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePaused}}
	if MaySupersede(paused) {
		t.Fatal("a Paused action was reported supersedable. An action paused by an estop or a " +
			"ZoneMaintenance window is OPERATOR-GATED: superseding it would resolve, " +
			"automatically and silently, an action whose resolution the pause deliberately " +
			"handed to a human. E-stop is not a preemption")
	}
}

func TestMaySupersede_OnlyLiveAssignedOrInProgress(t *testing.T) {
	for phase, want := range map[fleetv1.ActionPhase]bool{
		fleetv1.ActionPhaseAssigned:   true,
		fleetv1.ActionPhaseInProgress: true,
		fleetv1.ActionPhasePaused:     false, // operator-gated
		fleetv1.ActionPhasePending:    false, // not executing; nothing to hand over
		fleetv1.ActionPhasePreempted:  false, // already displaced by the OTHER mechanism
		fleetv1.ActionPhaseCancelled:  false, // terminal
		fleetv1.ActionPhaseSucceeded:  false, // terminal
		fleetv1.ActionPhaseFailed:     false, // terminal
	} {
		a := &fleetv1.FleetAction{Status: fleetv1.FleetActionStatus{Phase: phase}}
		if got := MaySupersede(a); got != want {
			t.Errorf("MaySupersede(%s) = %v, want %v", phase, got, want)
		}
	}
	if MaySupersede(nil) {
		t.Error("MaySupersede(nil) must be false")
	}
}

// ---------------------------------------------------------------------------
// An operator cancellation, end to end: the reason, the empty supersededBy, the
// Event, and the cause on the wire.
// ---------------------------------------------------------------------------

func TestOperatorCancel_IsAttributedAndDistinguishable(t *testing.T) {
	live := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	r, c := newActionReconciler(t,
		cancelAction("t1", "r1", fleetv1.ActionPhaseInProgress, 4, live, "true"),
		robotInPhase("r1", fleetv1.RobotPhaseInProgress, "t1"),
	)
	fc := &fakeCommander{cancelAck: true}
	r.Commander = fc
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec

	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %s, want Cancelled", ft.Status.Phase)
	}
	if ft.Status.CancellationReason != fleetv1.CancellationReasonOperator {
		t.Errorf("cancellationReason = %q, want Operator. The annotation intake is SAR-gated on "+
			"the `cancel` verb, so it is definitionally a person", ft.Status.CancellationReason)
	}
	// The structural half: an operator cancellation can NEVER name a superseding action.
	if ft.Status.SupersededBy != "" {
		t.Errorf("supersededBy = %q on an operator cancel, want empty — this field is what makes "+
			"the distinction structural rather than a label", ft.Status.SupersededBy)
	}
	if cond := apimeta.FindStatusCondition(ft.Status.Conditions, ConditionSuperseded); cond != nil &&
		cond.Status == metav1.ConditionTrue {
		t.Error("Superseded=True on an operator cancellation")
	}
	assertEventReason(t, rec, EventActionCancelledByOperator)

	if len(fc.cancels) != 1 {
		t.Fatalf("expected one cancel_action, got %d", len(fc.cancels))
	}
	if fc.cancels[0].Cause != command.CancelCauseOperator {
		t.Errorf("wire cause = %v, want CancelCauseOperator. Free text is not a cause; the "+
			"adapter branches on this and echoes the matching ActionEndCause back",
			fc.cancels[0].Cause)
	}
}

// ---------------------------------------------------------------------------
// D2 — cancel-then-dispatch. The same finalize path, attributed as a supersession.
// ---------------------------------------------------------------------------

func TestSupersededCancelIntent_IsADistinctEventFromAnOperatorCancel(t *testing.T) {
	op := operatorCancelIntent("true")
	sup := supersededCancelIntent("action-b")

	if op.eventReason == sup.eventReason {
		t.Fatal("an operator cancellation and a supersession emit the SAME Event reason — " +
			"they are different events with different actors and different operational " +
			"meanings, and phase alone already cannot separate them")
	}
	if sup.cancellationReason != fleetv1.CancellationReasonSupersededByDirective {
		t.Errorf("cancellationReason = %q, want SupersededByDirective", sup.cancellationReason)
	}
	if sup.supersededBy != "action-b" {
		t.Errorf("supersededBy = %q, want action-b — REQUIRED on a supersession", sup.supersededBy)
	}
	if sup.cause != command.CancelCauseSuperseded {
		t.Errorf("wire cause = %v, want CancelCauseSuperseded", sup.cause)
	}
	if !sup.supersededAudit {
		t.Error("a supersession must seal FLEETACTION_SUPERSEDED, not FLEETACTION_CANCELLED: " +
			"an auditor must be able to separate the fleet replacing work from a person " +
			"halting it")
	}
	if op.supersededAudit {
		t.Error("an operator cancellation must seal FLEETACTION_CANCELLED")
	}
	if op.supersededBy != "" {
		t.Error("an operator cancellation must never populate supersededBy")
	}
}

// A composite cancelling its child through spec.desiredState is NOT claimed as an
// operator act. Guessing would put a false actor in the audit trail.
func TestDesiredStateCancel_IsNotAttributedToAnOperator(t *testing.T) {
	act := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: actionNS},
		Spec: fleetv1.FleetActionSpec{
			Type:         fleetv1.ActionTypeNavigate,
			DesiredState: fleetv1.DesiredStateCancelled,
		},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhasePending},
	}
	r, c := newActionReconciler(t, act)
	reconcileAction(t, r, "t1")

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %s, want Cancelled", ft.Status.Phase)
	}
	if ft.Status.CancellationReason != "" {
		t.Errorf("cancellationReason = %q, want EMPTY. spec.desiredState: Cancelled is written "+
			"by a person OR by the FleetTask FailFast/Compensate path and nothing here can "+
			"tell which; RFC-0001 provides for exactly this — empty means no controller "+
			"stated a cause, and a reader must not infer one", ft.Status.CancellationReason)
	}
	if ft.Status.SupersededBy != "" {
		t.Errorf("supersededBy = %q, want empty", ft.Status.SupersededBy)
	}
}

// ---------------------------------------------------------------------------
// D1 — in-place supersession, driven by the adapter's terminal report. This is the
// only cancellation a robot report may drive, and only when it SAYS it is one.
// ---------------------------------------------------------------------------

func supersedableAction(name, robot string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: actionNS},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
		Status: fleetv1.FleetActionStatus{
			Phase:                fleetv1.ActionPhaseInProgress,
			AssignedRobot:        robot,
			AssignmentGeneration: 4,
		},
	}
}

func TestIngest_InPlaceSupersessionIsAttributed(t *testing.T) {
	// The successor must exist, be live and be bound to the same robot for the claim to
	// verify; see actionstatus_supersession_verify_test.go for the refusals.
	_, c := newActionReconciler(t, supersedableAction("t1", "r1"), supersedableAction("t2", "r1"), robotAt("r1", "r1", ""))
	rec := record.NewFakeRecorder(10)
	ing := &ActionStatusIngestor{Client: c, Recorder: rec}

	if err := ing.IngestActionStatus(context.Background(), actionNS, allowAnyRobot, &fav1.ActionStatusUpdate{
		ActionId:            "t1",
		State:               fav1.ActionState_ACTION_STATE_CANCELLED,
		EndCause:            fav1.ActionEndCause_ACTION_END_CAUSE_SUPERSEDED,
		SupersedingActionId: "t2",
		FencingToken:        u64(4),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseCancelled {
		t.Fatalf("phase = %s, want Cancelled — under D1 no cancel_action is sent, so this is "+
			"the only path on which A can reach a terminal phase", ft.Status.Phase)
	}
	if ft.Status.CancellationReason != fleetv1.CancellationReasonSupersededByDirective {
		t.Errorf("cancellationReason = %q, want SupersededByDirective", ft.Status.CancellationReason)
	}
	if ft.Status.SupersededBy != "t2" {
		t.Errorf("supersededBy = %q, want t2", ft.Status.SupersededBy)
	}
	cond := apimeta.FindStatusCondition(ft.Status.Conditions, ConditionSuperseded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Superseded condition = %v, want True", cond)
	}
	if cond.Reason != ConditionReasonInPlacePreemption {
		t.Errorf("condition reason = %q, want %q — D1 and D2 must stay distinguishable on the "+
			"object, not merely both be 'superseded'", cond.Reason, ConditionReasonInPlacePreemption)
	}
	assertEventReason(t, rec, EventActionSupersededInPlace)
}

// An UNTYPED cancelled report is not a supersession claim. Every pre-RFC adapter
// sends end_cause UNSPECIFIED, and UNSPECIFIED is explicitly "no cause stated".
func TestIngest_UntypedCancelledReportIsNotASupersession(t *testing.T) {
	_, c := newActionReconciler(t, supersedableAction("t1", "r1"), robotAt("r1", "r1", ""))
	ing := &ActionStatusIngestor{Client: c}

	if err := ing.IngestActionStatus(context.Background(), actionNS, allowAnyRobot, &fav1.ActionStatusUpdate{
		ActionId: "t1",
		State:    fav1.ActionState_ACTION_STATE_CANCELLED,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s, want InProgress unchanged. An untyped CANCELLED report must not "+
			"terminalise an action: cancellation is otherwise control-plane-driven under "+
			"confirmed-stop semantics, and reading UNSPECIFIED as a supersession would let a "+
			"legacy adapter free a robot the control plane still considers committed",
			ft.Status.Phase)
	}
}

// A supersession that cannot name its successor is malformed, and is refused rather
// than written half-formed.
func TestIngest_SupersededWithNoSuccessorIsRefused(t *testing.T) {
	_, c := newActionReconciler(t, supersedableAction("t1", "r1"), robotAt("r1", "r1", ""))
	ing := &ActionStatusIngestor{Client: c}

	if err := ing.IngestActionStatus(context.Background(), actionNS, allowAnyRobot, &fav1.ActionStatusUpdate{
		ActionId: "t1",
		State:    fav1.ActionState_ACTION_STATE_CANCELLED,
		EndCause: fav1.ActionEndCause_ACTION_END_CAUSE_SUPERSEDED,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	ft := getAction(t, c, "t1")
	if ft.Status.Phase == fleetv1.ActionPhaseCancelled {
		t.Fatal("an ACTION_END_CAUSE_SUPERSEDED report with no superseding_action_id was " +
			"accepted. supersededBy is REQUIRED on this path — it is what makes the " +
			"distinction structural, so a supersession that leaves it empty is malformed")
	}
}

// THE ESTOP GUARD, AT THE INGEST BOUNDARY. A Paused action is not advanceable, so a
// robot report claiming a supersession cannot resolve a human's hold.
func TestIngest_PausedActionIsNotSupersededByARobotReport(t *testing.T) {
	paused := supersedableAction("t1", "r1")
	paused.Status.Phase = fleetv1.ActionPhasePaused
	_, c := newActionReconciler(t, paused)
	ing := &ActionStatusIngestor{Client: c}

	if err := ing.IngestActionStatus(context.Background(), actionNS, allowAnyRobot, &fav1.ActionStatusUpdate{
		ActionId:            "t1",
		State:               fav1.ActionState_ACTION_STATE_CANCELLED,
		EndCause:            fav1.ActionEndCause_ACTION_END_CAUSE_SUPERSEDED,
		SupersedingActionId: "t2",
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if got := getAction(t, c, "t1").Status.Phase; got != fleetv1.ActionPhasePaused {
		t.Fatalf("phase = %s, want Paused. E-stop is not a preemption: a paused action is "+
			"operator-gated and neither disposition is available while it is held", got)
	}
}

// assertEventReason fails unless one buffered Event carries the given reason.
func assertEventReason(t *testing.T, rec *record.FakeRecorder, want string) {
	t.Helper()
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, want) {
				return
			}
		default:
			t.Fatalf("no Event with reason %q was emitted", want)
		}
	}
}

// ---------------------------------------------------------------------------
// The handover inside a PRIORITY preemption still obeys the disposition rule.
//
// Priority decides WHETHER this preemptor may take the robot. It does not decide
// HOW the robot is handed over — and the two were previously answered by one
// branch, so a Critical action of a different type took a committed robot with no
// confirmed stop.
//
// The victim is REQUEUED either way, never marked superseded: it is work the fleet
// still owes, and that is exactly what separates priority preemption from a
// directive supersession.
// ---------------------------------------------------------------------------

func TestPreempt_TypeChangeWaitsForAConfirmedStop(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	crit := criticalPending("crit")
	crit.Spec.Type = fleetv1.ActionTypePickUp // DIFFERENT type from the victim's Navigate

	victim := bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease)
	victim.Spec.Type = fleetv1.ActionTypeNavigate

	r, c := newActionReconciler(t, crit, victim, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"))
	// The adapter is unreachable, so no stop can be confirmed and the lease is alive.
	r.Commander = &fakeCommander{cancelErr: command.ErrUnreachable}

	reconcileAction(t, r, "crit")

	got := getAction(t, c, "crit")
	if got.Status.AssignedRobot == "r1" {
		t.Fatal("a type-changing preemptor was committed to a robot whose victim had NOT " +
			"confirmed a stop. A different spec.type means a different executor; handing a " +
			"committed robot to one while the victim's lease is live is the dual-execution " +
			"hazard the single-executor guarantee exists to exclude")
	}
	if v := getAction(t, c, "norm"); v.Status.Phase == fleetv1.ActionPhasePreempted {
		t.Error("the victim was displaced before its stop was confirmed")
	}
	// And the victim must never be labelled superseded — it is requeued work.
	if v := getAction(t, c, "norm"); v.Status.CancellationReason != "" || v.Status.SupersededBy != "" {
		t.Errorf("a priority-preemption victim was marked superseded (reason=%q supersededBy=%q). "+
			"Its fate is Preempted → Pending with retryCount unchanged, because the fleet still "+
			"owes the work", v.Status.CancellationReason, v.Status.SupersededBy)
	}
}

func TestPreempt_SameTypeHandsOverInPlace(t *testing.T) {
	lease := &metav1.Time{Time: time.Now().Add(defaultLeaseDuration)}
	crit := criticalPending("crit") // Navigate
	victim := bandAction("norm", "r1", fleetv1.ActionPhaseInProgress, fleetv1.ActionPriorityNormal, 2, lease)
	victim.Spec.Type = fleetv1.ActionTypeNavigate // SAME type

	r, c := newActionReconciler(t, crit, victim, robotInPhase("r1", fleetv1.RobotPhaseInProgress, "norm"))
	// Unreachable adapter: an in-place handover must NOT depend on a confirmed stop,
	// so this must still commit. If it waits, the same-type fast path has been lost.
	r.Commander = &fakeCommander{cancelErr: command.ErrUnreachable}

	reconcileAction(t, r, "crit")

	got := getAction(t, c, "crit")
	if got.Status.AssignedRobot != "r1" || got.Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatalf("a SAME-type preemptor did not take the robot in place: phase=%s robot=%q. "+
			"D1 sends no cancel_action and does not wait for the victim to terminalise — "+
			"the robot's physical commitment carries over",
			got.Status.Phase, got.Status.AssignedRobot)
	}
	if v := getAction(t, c, "norm"); v.Status.Phase != fleetv1.ActionPhasePreempted {
		t.Errorf("victim phase = %s, want Preempted", v.Status.Phase)
	}
	if v := getAction(t, c, "norm"); v.Status.SupersededBy != "" {
		t.Errorf("a priority-preemption victim was marked superseded: %q", v.Status.SupersededBy)
	}
}
