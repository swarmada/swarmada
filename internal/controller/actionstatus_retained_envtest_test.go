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

	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// The retained outcome on the control-plane side (RFC-0001
// #fleet-adapter-protocol-reconnect-action-state-reconciliation, The retained outcome), read as
// rows A13 and A25 of RFC-0001 #safety-action-table decide it, on a real API server. ack is what
// the stream server turns into an ActionOutcomeAck: given only for an outcome recorded or
// decided, never for one left undecided.

const rtGeneration = 7

type rtRig struct {
	t   *testing.T
	ctx context.Context
	ns  string
	ing *ActionStatusIngestor
	ev  *eventTargets
}

// newRTRig seeds robot r1 (under a confirmed stop when stopped) and action a1 in phase, bound to
// r1 at generation rtGeneration unless unbound.
func newRTRig(t *testing.T, phase fleetv1.ActionPhase, stopped, unbound bool) *rtRig {
	t.Helper()
	requireEnvtest(t)
	r := &rtRig{t: t, ctx: context.Background(), ns: envtestNamespace(t), ev: &eventTargets{}}
	robot := envtestValidRobot(r.ns, "r1", "z")
	r.must("create robot", envK8s.Create(r.ctx, robot))
	robot.Status.Phase = fleetv1.RobotPhaseInProgress
	robot.Status.AssignedAction = "a1"
	if stopped {
		robot.Status.EstopState = fleetv1.RobotEstopStopped
	}
	r.must("seed robot status", envK8s.Status().Update(r.ctx, robot))

	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: r.ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Priority: fleetv1.ActionPriorityNormal},
	}
	r.must("create action", envK8s.Create(r.ctx, a))
	a.Status.Phase = phase
	if !unbound {
		a.Status.AssignedRobot = "r1"
	}
	a.Status.AssignmentGeneration = rtGeneration
	lease := metav1.NewTime(time.Now().Add(30 * time.Second).Truncate(time.Second))
	a.Status.LeaseExpiresAt = &lease
	if phase == fleetv1.ActionPhaseInProgress {
		started := metav1.Now()
		a.Status.StartedAt = &started
	}
	r.must("seed action status", envK8s.Status().Update(r.ctx, a))
	r.ing = &ActionStatusIngestor{Client: envK8s, APIReader: envK8s, Recorder: r.ev, Audit: &lockedAudit{}}
	return r
}

func (r *rtRig) must(what string, err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatalf("%s: %v", what, err)
	}
}

func (r *rtRig) action() *fleetv1.FleetAction {
	r.t.Helper()
	a := &fleetv1.FleetAction{}
	r.must("read action", envK8s.Get(r.ctx, client.ObjectKey{Namespace: r.ns, Name: "a1"}, a))
	return a
}

func allow(context.Context, string) bool { return true }
func deny(context.Context, string) bool  { return false }

func retained(state fav1.ActionState, token uint64, cause fav1.ActionEndCause) *fav1.ActionStatusUpdate {
	return &fav1.ActionStatusUpdate{ActionId: "a1", State: state, FencingToken: proto.Uint64(token),
		EndCause: cause, Retained: true}
}

// ingest reads u and returns whether it was acknowledged, failing on an API error.
func (r *rtRig) ingest(authorize controlstream.RobotAuthorization, u *fav1.ActionStatusUpdate) bool {
	r.t.Helper()
	ack, err := r.ing.IngestRetainedOutcome(r.ctx, r.ns, authorize, u)
	r.must("ingest", err)
	return ack
}

const (
	succeeded = fav1.ActionState_ACTION_STATE_SUCCEEDED
	failed    = fav1.ActionState_ACTION_STATE_FAILED
	noCause   = fav1.ActionEndCause_ACTION_END_CAUSE_UNSPECIFIED
	byEstop   = fav1.ActionEndCause_ACTION_END_CAUSE_ESTOP
)

func TestEnvtest_RetainedOutcome_SucceededUnderAStopEndsTheWorkAndIsAcknowledgedOnce(t *testing.T) {
	// A13: after an abnormal event A1 has paused the work under the end-state stop; the robot's
	// retained SUCCEEDED, carrying the action's report token, ends it (A25: under a stop or not).
	r := newRTRig(t, fleetv1.ActionPhasePaused, true, false)
	if !r.ingest(allow, retained(succeeded, rtGeneration, noCause)) {
		t.Fatal("a retained SUCCEEDED for the bound action was not acknowledged")
	}
	a := r.action()
	if a.Status.Phase != fleetv1.ActionPhaseSucceeded || a.Status.CompletedAt == nil {
		t.Fatalf("phase %s completedAt %v; want Succeeded with completedAt", a.Status.Phase, a.Status.CompletedAt)
	}
	// The acknowledgement was lost and the adapter reports the outcome again on its next
	// reconnect: acknowledged again, and nothing is written a second time.
	rv := a.ResourceVersion
	if !r.ingest(allow, retained(succeeded, rtGeneration, noCause)) {
		t.Fatal("the same retained outcome, already recorded, was not acknowledged")
	}
	if got := r.action().ResourceVersion; got != rv {
		t.Fatalf("an outcome already recorded was written again (resourceVersion %s -> %s)", rv, got)
	}
}

func TestEnvtest_RetainedOutcome_AStaleReportTokenIsNotAcknowledged(t *testing.T) {
	r := newRTRig(t, fleetv1.ActionPhasePaused, true, false)
	rv := r.action().ResourceVersion
	if r.ingest(allow, retained(succeeded, rtGeneration-1, noCause)) {
		t.Fatal("a retained outcome from a superseded assignment was acknowledged: the adapter would drop it")
	}
	a := r.action()
	if a.Status.Phase != fleetv1.ActionPhasePaused || a.ResourceVersion != rv {
		t.Fatalf("a stale retained outcome moved the action: phase %s, resourceVersion %s -> %s",
			a.Status.Phase, rv, a.ResourceVersion)
	}
	// Another run's report: the refusal is raised on the bound robot, which the stream serves,
	// not on the action, which is often a later run under the same name.
	onRobot, onAction := 0, 0
	for _, e := range r.ev.got {
		if e.reason != EventActionReportRefused {
			continue
		}
		switch {
		case e.kind == "*v1.Robot" && e.name == "r1":
			onRobot++
		case e.kind == "*v1.FleetAction":
			onAction++
		}
	}
	if onRobot != 1 || onAction != 0 {
		t.Fatalf("ActionReportRefused on robot r1 %d time(s) and on the action %d time(s); want 1 and 0: %+v",
			onRobot, onAction, r.ev.got)
	}
}

func TestEnvtest_RetainedOutcome_AStaleReportTokenFromAnUnauthorizedStreamRaisesNoEvent(t *testing.T) {
	r := newRTRig(t, fleetv1.ActionPhasePaused, true, false)
	if r.ingest(deny, retained(succeeded, rtGeneration-1, noCause)) {
		t.Fatal("a stale retained outcome from a stream not authorized for r1 was acknowledged")
	}
	for _, e := range r.ev.got {
		if e.reason == EventActionReportRefused {
			t.Fatalf("a stream not authorized for the bound robot raised a Warning Event: %+v", e)
		}
	}
}

func TestEnvtest_RetainedOutcome_NoAPIReaderAcknowledgesNothing(t *testing.T) {
	r := newRTRig(t, fleetv1.ActionPhasePaused, true, false)
	r.ing.APIReader = nil
	rv := r.action().ResourceVersion
	ack, err := r.ing.IngestRetainedOutcome(r.ctx, r.ns, allow, retained(succeeded, rtGeneration, noCause))
	if ack || err == nil {
		t.Fatalf("an ingestor without an API reader answered ack=%v err=%v; want no acknowledgement and an error", ack, err)
	}
	if got := r.action().ResourceVersion; got != rv {
		t.Fatalf("an ingestor without an API reader wrote the action (resourceVersion %s -> %s)", rv, got)
	}
}

func TestEnvtest_RetainedOutcome_FailedUnderAStopIsA1sAndMovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   fleetv1.ActionPhase
		stopped bool
		cause   fav1.ActionEndCause
	}{
		{"paused under a stop", fleetv1.ActionPhasePaused, true, noCause},
		{"preempted, ended by an estop", fleetv1.ActionPhasePreempted, false, byEstop},
		{"revoking under a stop", fleetv1.ActionPhaseRevoking, true, byEstop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRTRig(t, tc.phase, tc.stopped, false)
			rv := r.action().ResourceVersion
			if !r.ingest(allow, retained(failed, rtGeneration, tc.cause)) {
				t.Fatal("a retained FAILED that A1 decides was not acknowledged")
			}
			if a := r.action(); a.Status.Phase != tc.phase || a.ResourceVersion != rv {
				t.Fatalf("A1's report moved the action: phase %s -> %s", tc.phase, a.Status.Phase)
			}
		})
	}
}

func TestEnvtest_RetainedOutcome_FailedFromLiveWorkUnderAStopPausesIt(t *testing.T) {
	r := newRTRig(t, fleetv1.ActionPhaseInProgress, true, false)
	if !r.ingest(allow, retained(failed, rtGeneration, noCause)) {
		t.Fatal("not acknowledged")
	}
	a := r.action()
	if a.Status.Phase != fleetv1.ActionPhasePaused || a.Status.FailedAt != nil {
		t.Fatalf("phase %s failedAt %v; want Paused, not Failed: a failure under a stop is a stop outcome",
			a.Status.Phase, a.Status.FailedAt)
	}
	if a.Status.AssignedRobot != "r1" {
		t.Fatal("the pause released the robot binding")
	}
}

func TestEnvtest_RetainedOutcome_EstopEndCauseWithNoRecordedStopPausesLiveWork(t *testing.T) {
	r := newRTRig(t, fleetv1.ActionPhaseInProgress, false, false)
	if !r.ingest(allow, retained(failed, rtGeneration, byEstop)) {
		t.Fatal("not acknowledged")
	}
	a := r.action()
	if a.Status.Phase != fleetv1.ActionPhasePaused || !strings.Contains(a.Status.Message, "ACTION_END_CAUSE_ESTOP") {
		t.Fatalf("phase %s message %q; want Paused naming the end cause", a.Status.Phase, a.Status.Message)
	}
}

func TestEnvtest_RetainedOutcome_TheRobotsOwnFailureFailsTheWork(t *testing.T) {
	r := newRTRig(t, fleetv1.ActionPhaseInProgress, false, false)
	if !r.ingest(allow, retained(failed, rtGeneration, fav1.ActionEndCause_ACTION_END_CAUSE_EXECUTION_FAILURE)) {
		t.Fatal("not acknowledged")
	}
	if a := r.action(); a.Status.Phase != fleetv1.ActionPhaseFailed || a.Status.FailedAt == nil {
		t.Fatalf("phase %s; want Failed", a.Status.Phase)
	}
}

func TestEnvtest_RetainedOutcome_UndecidedOutcomesAreNotAcknowledged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		phase     fleetv1.ActionPhase
		unbound   bool
		authorize controlstream.RobotAuthorization
		report    *fav1.ActionStatusUpdate
	}{
		{"not terminal", fleetv1.ActionPhaseInProgress, false, allow,
			retained(fav1.ActionState_ACTION_STATE_RUNNING, rtGeneration, noCause)},
		{"no report token", fleetv1.ActionPhaseInProgress, false, allow,
			&fav1.ActionStatusUpdate{ActionId: "a1", State: succeeded, Retained: true}},
		{"a stream that may not act on the robot", fleetv1.ActionPhaseInProgress, false, deny,
			retained(succeeded, rtGeneration, noCause)},
		{"work another row moved first (Pending)", fleetv1.ActionPhasePending, false, allow,
			retained(succeeded, rtGeneration, noCause)},
		{"work no longer bound to a robot", fleetv1.ActionPhasePaused, true, allow,
			retained(succeeded, rtGeneration, noCause)},
		{"a CANCELLED outcome for live work", fleetv1.ActionPhaseInProgress, false, allow,
			retained(fav1.ActionState_ACTION_STATE_CANCELLED, rtGeneration, noCause)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRTRig(t, tc.phase, false, tc.unbound)
			rv := r.action().ResourceVersion
			if r.ingest(tc.authorize, tc.report) {
				t.Fatal("an undecided retained outcome was acknowledged: the adapter would drop it")
			}
			if a := r.action(); a.ResourceVersion != rv {
				t.Fatalf("an undecided retained outcome was written: phase %s", a.Status.Phase)
			}
		})
	}
}

func TestEnvtest_RetainedOutcome_ForAnActionTheAPIServerNoLongerHoldsIsAcknowledged(t *testing.T) {
	r := newRTRig(t, fleetv1.ActionPhaseInProgress, false, false)
	r.must("delete action", envK8s.Delete(r.ctx, r.action()))
	if !r.ingest(allow, retained(succeeded, rtGeneration, noCause)) {
		t.Fatal("a retained outcome for a deleted action was not acknowledged: the adapter would report it forever")
	}
}
