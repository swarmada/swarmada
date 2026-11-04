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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A robot's report and the FleetAction controller write the same status, and the ingestor writes
// with the resourceVersion it read, so a controller write in between (a lease renewal, a hold)
// makes the ingestor's write conflict. These rows put that write there, on a real API server,
// inside the ingestor's own status patch just before it is sent, and check what the report does
// then. An adapter reports a terminal state once, so a terminal report the fresh object still
// allows must be applied, or the action stays InProgress with its lease renewed. A report the
// fresh object refuses stays refused: a superseded fencing token, or a phase the report may not
// leave (RFC-0001 #safety-action-table). A report that is not applied
// says so.

const wcGeneration = 7

// wcReportNotApplied is the Event reason a report that lost its write and was not applied
// carries on the action.
const wcReportNotApplied = "ActionReportNotApplied"

// wcRig is one action a1, bound to robot r1 at generation wcGeneration, and an ingestor whose
// status patch of a1 first runs race: the controller's write between the ingestor's read and
// its write.
type wcRig struct {
	t   *testing.T
	ctx context.Context
	ns  string
	ing *ActionStatusIngestor
	ev  *eventTargets
	aud *lockedAudit

	// race is the controller's write, run inside the ingestor's n-th status patch of a1 before
	// it is sent. nil writes nothing.
	race func(n int)
	// patches counts the ingestor's status patches of a1, and conflicts those the API server
	// refused as written against an older object.
	patches, conflicts int
	// behind, when set, is what the ingestor's cached client returns for a1: a manager cache
	// that has not seen the controller's write.
	behind *fleetv1.FleetAction
}

func newWCRig(t *testing.T, phase fleetv1.ActionPhase) *wcRig {
	t.Helper()
	requireEnvtest(t)
	r := &wcRig{t: t, ctx: context.Background(), ns: envtestNamespace(t), ev: &eventTargets{}, aud: &lockedAudit{}}

	robot := envtestValidRobot(r.ns, "r1", "z")
	r.must("create robot", envK8s.Create(r.ctx, robot))
	robot.Status.Phase = fleetv1.RobotPhaseInProgress
	robot.Status.AssignedAction = "a1"
	r.must("seed robot status", envK8s.Status().Update(r.ctx, robot))

	a := &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: r.ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Priority: fleetv1.ActionPriorityNormal},
	}
	r.must("create action", envK8s.Create(r.ctx, a))
	a.Status.Phase = phase
	a.Status.AssignedRobot = "r1"
	a.Status.AssignmentGeneration = wcGeneration
	lease := metav1.NewTime(time.Now().Add(30 * time.Second).Truncate(time.Second))
	a.Status.LeaseExpiresAt = &lease
	if phase == fleetv1.ActionPhaseInProgress {
		started := metav1.Now()
		a.Status.StartedAt = &started
		a.Status.ProgressPct = 40
	}
	r.must("seed action status", envK8s.Status().Update(r.ctx, a))

	direct, err := client.NewWithWatch(envCfg, client.Options{Scheme: envScheme})
	r.must("direct client", err)
	c := interceptor.NewClient(direct, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if fa, ok := obj.(*fleetv1.FleetAction); ok && key.Name == "a1" && r.behind != nil {
				r.behind.DeepCopyInto(fa)
				return nil
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := obj.(*fleetv1.FleetAction); ok && sub == "status" && obj.GetName() == "a1" {
				r.patches++
				if r.race != nil {
					r.race(r.patches)
				}
				err := cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
				if apierrors.IsConflict(err) {
					r.conflicts++
				}
				return err
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	r.ing = &ActionStatusIngestor{Client: c, APIReader: direct, Recorder: r.ev, Audit: r.aud}
	return r
}

func (r *wcRig) must(what string, err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatalf("%s: %v", what, err)
	}
}

// raced fails the row unless the controller's write made the report's write conflict.
func (r *wcRig) raced() {
	r.t.Helper()
	if r.conflicts == 0 {
		r.t.Fatalf("the report's write never conflicted: the controller's write did not race it")
	}
}

func (r *wcRig) action() *fleetv1.FleetAction {
	r.t.Helper()
	a := &fleetv1.FleetAction{}
	r.must("get action", envK8s.Get(r.ctx, client.ObjectKey{Namespace: r.ns, Name: "a1"}, a))
	return a
}

// write is a controller write of a1's status.
func (r *wcRig) write(what string, mut func(*fleetv1.FleetAction)) {
	r.t.Helper()
	a := r.action()
	mut(a)
	r.must("the controller's write: "+what, envK8s.Status().Update(r.ctx, a))
}

// renewLease is the controller's lease renewal: a status write that leaves the phase alone.
func (r *wcRig) renewLease() {
	r.write("lease renewal", func(a *fleetv1.FleetAction) {
		next := metav1.NewTime(a.Status.LeaseExpiresAt.Add(time.Second))
		a.Status.LeaseExpiresAt = &next
	})
}

// stopRobot puts r1 under a stop.
func (r *wcRig) stopRobot() {
	r.t.Helper()
	robot := &fleetv1.Robot{}
	r.must("get robot", envK8s.Get(r.ctx, client.ObjectKey{Namespace: r.ns, Name: "r1"}, robot))
	robot.Status.EstopState = fleetv1.RobotEstopStopping
	r.must("r1 comes under a stop", envK8s.Status().Update(r.ctx, robot))
}

// hold is the controller writing a1 into phase: Paused under a stop on r1, Revoking, Preempted
// or Cancelled.
func (r *wcRig) hold(phase fleetv1.ActionPhase) {
	r.t.Helper()
	if phase == fleetv1.ActionPhasePaused {
		r.stopRobot()
	}
	r.write("hold "+string(phase), func(a *fleetv1.FleetAction) {
		a.Status.Phase = phase
		a.Status.Message = "held by the controller: " + string(phase)
		if phase == fleetv1.ActionPhaseCancelled {
			now := metav1.Now()
			a.Status.CompletionTime = &now
		}
	})
}

func (r *wcRig) report(on *ActionStatusIngestor, state fav1.ActionState, token *uint64, msg string) {
	r.t.Helper()
	r.must("ingest "+state.String(), on.IngestActionStatus(r.ctx, r.ns, allowAnyRobot,
		&fav1.ActionStatusUpdate{ActionId: "a1", State: state, FencingToken: token, Message: msg}))
}

func (r *wcRig) events(reason string) []recordedEvent {
	var out []recordedEvent
	for _, e := range r.ev.got {
		if e.reason == reason {
			out = append(out, e)
		}
	}
	return out
}

func (r *wcRig) audits(eventType string) int {
	r.aud.mu.Lock()
	defer r.aud.mu.Unlock()
	n := 0
	for _, e := range r.aud.entries {
		if e.EventType == eventType {
			n++
		}
	}
	return n
}

func (r *wcRig) result(a *fleetv1.FleetAction) {
	r.t.Helper()
	var reasons []string
	for _, e := range r.ev.got {
		reasons = append(reasons, e.reason)
	}
	r.t.Logf("RESULT phase=%s generation=%d patches=%d conflicts=%d events=%v message=%q",
		a.Status.Phase, a.Status.AssignmentGeneration, r.patches, r.conflicts, reasons, a.Status.Message)
}

var (
	wcSucceeded = fav1.ActionState_ACTION_STATE_SUCCEEDED
	wcFailed    = fav1.ActionState_ACTION_STATE_FAILED
	wcRunning   = fav1.ActionState_ACTION_STATE_RUNNING
)

// A report the fresh object still allows is applied after the conflict.
func TestEnvtest_ActionReportWriteConflict_Applied(t *testing.T) {
	once := func(r *wcRig, write func()) {
		r.race = func(n int) {
			if n == 1 {
				write()
			}
		}
	}

	for _, behind := range []bool{false, true} {
		name := "terminal-after-a-lease-renewal/succeeded"
		if behind {
			// The manager's cache still holds the copy the first attempt read: the decision
			// is taken again on what the API server holds.
			name += "/cache-behind"
		}
		t.Run(name, func(t *testing.T) {
			r := newWCRig(t, fleetv1.ActionPhaseInProgress)
			if behind {
				r.behind = r.action()
			}
			once(r, r.renewLease)
			r.report(r.ing, wcSucceeded, u64(wcGeneration), "arrived")
			a := r.action()
			r.result(a)
			r.raced()
			if a.Status.Phase != fleetv1.ActionPhaseSucceeded || a.Status.CompletedAt == nil || a.Status.ProgressPct != 100 {
				t.Fatalf("phase %s completedAt %v progress %d after a SUCCEEDED report that lost a write to a lease "+
					"renewal, want Succeeded with completedAt and 100: the adapter reports a terminal state once",
					a.Status.Phase, a.Status.CompletedAt, a.Status.ProgressPct)
			}
		})
	}

	t.Run("terminal-after-a-lease-renewal/failed", func(t *testing.T) {
		r := newWCRig(t, fleetv1.ActionPhaseInProgress)
		once(r, r.renewLease)
		r.report(r.ing, wcFailed, u64(wcGeneration), "path blocked")
		a := r.action()
		r.result(a)
		r.raced()
		if a.Status.Phase != fleetv1.ActionPhaseFailed || a.Status.FailedAt == nil || a.Status.FailureReason != "path blocked" {
			t.Fatalf("phase %s failedAt %v reason %q after a FAILED report that lost a write to a lease renewal, "+
				"want Failed, failedAt, \"path blocked\"", a.Status.Phase, a.Status.FailedAt, a.Status.FailureReason)
		}
	})

	t.Run("running-after-a-lease-renewal", func(t *testing.T) {
		r := newWCRig(t, fleetv1.ActionPhaseAssigned)
		once(r, r.renewLease)
		r.report(r.ing, wcRunning, u64(wcGeneration), "")
		a := r.action()
		r.result(a)
		r.raced()
		if a.Status.Phase != fleetv1.ActionPhaseInProgress || a.Status.StartedAt == nil {
			t.Fatalf("phase %s startedAt %v after a RUNNING report that lost a write, want InProgress with startedAt",
				a.Status.Phase, a.Status.StartedAt)
		}
	})

	// The stop guard is asked again: a stop that reached the robot between the read and the
	// write makes the failure the stop's outcome.
	t.Run("failed-after-a-stop-arrives", func(t *testing.T) {
		r := newWCRig(t, fleetv1.ActionPhaseInProgress)
		once(r, func() { r.stopRobot(); r.renewLease() })
		r.report(r.ing, wcFailed, u64(wcGeneration), "motor halted")
		a := r.action()
		r.result(a)
		r.raced()
		if a.Status.Phase != fleetv1.ActionPhasePaused {
			t.Fatalf("phase %s, want Paused: a failure reported while a stop covers the robot is the stop's outcome, "+
				"and the stop arrived before the report was written", a.Status.Phase)
		}
		if n := len(r.events(EventActionFailedUnderEstop)); n != 1 {
			t.Errorf("%d %s Events, want 1", n, EventActionFailedUnderEstop)
		}
		if n := r.audits(audit.EventActionPausedByEstop); n != 1 {
			t.Errorf("%d %s audit entries, want 1", n, audit.EventActionPausedByEstop)
		}
	})
}

// A report that is not applied says so: a named Event on the action, never a silent drop.
func TestEnvtest_ActionReportWriteConflict_NotAppliedIsNamed(t *testing.T) {
	t.Run("retries-run-out", func(t *testing.T) {
		r := newWCRig(t, fleetv1.ActionPhaseInProgress)
		r.race = func(int) { r.renewLease() }
		r.report(r.ing, wcSucceeded, u64(wcGeneration), "")
		a := r.action()
		r.result(a)
		r.raced()
		if a.Status.Phase != fleetv1.ActionPhaseInProgress {
			t.Fatalf("phase %s, want InProgress: every write lost", a.Status.Phase)
		}
		if r.patches < 2 || r.patches > 5 {
			t.Errorf("%d status patches, want a bounded retry (2 to 5)", r.patches)
		}
		ev := r.events(wcReportNotApplied)
		if len(ev) != 1 || ev[0].name != "a1" || !strings.Contains(ev[0].msg, "SUCCEEDED") {
			t.Fatalf("%s Events %+v, want one on a1 naming the SUCCEEDED report", wcReportNotApplied, ev)
		}
	})

	t.Run("the-fresh-phase-refuses", func(t *testing.T) {
		r := newWCRig(t, fleetv1.ActionPhaseInProgress)
		r.race = func(n int) {
			if n == 1 {
				r.hold(fleetv1.ActionPhasePaused)
			}
		}
		r.report(r.ing, wcSucceeded, u64(wcGeneration), "")
		a := r.action()
		r.result(a)
		r.raced()
		if a.Status.Phase != fleetv1.ActionPhasePaused {
			t.Fatalf("phase %s, want Paused", a.Status.Phase)
		}
		ev := r.events(wcReportNotApplied)
		if len(ev) != 1 || ev[0].name != "a1" || !strings.Contains(ev[0].msg, "SUCCEEDED") || !strings.Contains(ev[0].msg, "Paused") {
			t.Fatalf("%s Events %+v, want one on a1 naming the SUCCEEDED report and the phase Paused", wcReportNotApplied, ev)
		}
	})
}

// A report the fresh object refuses stays refused, and a duplicate terminal report is applied
// once.
func TestEnvtest_ActionReportWriteConflict_StaysRefused(t *testing.T) {
	terminal := []fav1.ActionState{wcSucceeded, wcFailed}

	// unchanged fails the row if the report wrote anything after rv.
	unchanged := func(r *wcRig, rv string, want fleetv1.ActionPhase) *fleetv1.FleetAction {
		r.t.Helper()
		a := r.action()
		r.result(a)
		if a.Status.Phase != want || a.ResourceVersion != rv {
			r.t.Fatalf("phase %s resourceVersion %s -> %s, want %s and no write: the report must stay refused",
				a.Status.Phase, rv, a.ResourceVersion, want)
		}
		return a
	}

	for _, s := range terminal {
		s := s
		t.Run("stale-token/"+s.String(), func(t *testing.T) {
			r := newWCRig(t, fleetv1.ActionPhaseInProgress)
			rv := r.action().ResourceVersion
			r.report(r.ing, s, u64(wcGeneration-1), "")
			unchanged(r, rv, fleetv1.ActionPhaseInProgress)
		})
		t.Run("missing-token/"+s.String(), func(t *testing.T) {
			r := newWCRig(t, fleetv1.ActionPhaseInProgress)
			rv := r.action().ResourceVersion
			r.report(r.ing, s, nil, "")
			unchanged(r, rv, fleetv1.ActionPhaseInProgress)
		})

		// The controller re-assigned the action between the read and the write: the report's
		// token, right when it was read, names a superseded run now.
		t.Run("token-superseded-in-the-race/"+s.String(), func(t *testing.T) {
			r := newWCRig(t, fleetv1.ActionPhaseInProgress)
			var rv string
			r.race = func(n int) {
				if n == 1 {
					r.write("re-assignment", func(a *fleetv1.FleetAction) {
						a.Status.Phase = fleetv1.ActionPhaseAssigned
						a.Status.AssignmentGeneration = wcGeneration + 1
						a.Status.StartedAt = nil
						a.Status.ProgressPct = 0
					})
					rv = r.action().ResourceVersion
				}
			}
			r.report(r.ing, s, u64(wcGeneration), "")
			r.raced()
			a := unchanged(r, rv, fleetv1.ActionPhaseAssigned)
			if a.Status.AssignmentGeneration != wcGeneration+1 {
				t.Fatalf("generation %d, want %d", a.Status.AssignmentGeneration, wcGeneration+1)
			}
		})

		for _, phase := range []fleetv1.ActionPhase{fleetv1.ActionPhasePaused, fleetv1.ActionPhaseRevoking,
			fleetv1.ActionPhasePreempted, fleetv1.ActionPhaseCancelled} {
			phase := phase
			t.Run("hold-written-in-the-race/"+string(phase)+"/"+s.String(), func(t *testing.T) {
				r := newWCRig(t, fleetv1.ActionPhaseInProgress)
				var rv string
				r.race = func(n int) {
					if n == 1 {
						r.hold(phase)
						rv = r.action().ResourceVersion
					}
				}
				r.report(r.ing, s, u64(wcGeneration), "")
				r.raced()
				unchanged(r, rv, phase)
				if n := r.audits(audit.EventActionPausedByEstop); n != 0 {
					t.Errorf("%d %s audit entries for a report that was not applied", n, audit.EventActionPausedByEstop)
				}
				if n := len(r.events(EventActionFailedUnderEstop)); n != 0 {
					t.Errorf("%d %s Events for a report that was not applied", n, EventActionFailedUnderEstop)
				}
			})
		}
	}

	t.Run("duplicate/SUCCEEDED", func(t *testing.T) {
		r := newWCRig(t, fleetv1.ActionPhaseInProgress)
		r.report(r.ing, wcSucceeded, u64(wcGeneration), "")
		first := r.action()
		r.report(r.ing, wcSucceeded, u64(wcGeneration), "")
		unchanged(r, first.ResourceVersion, fleetv1.ActionPhaseSucceeded)
		if len(r.ev.got) != 0 {
			t.Errorf("Events %+v for a SUCCEEDED report applied and then repeated, want none", r.ev.got)
		}
	})

	t.Run("duplicate/FAILED-under-a-stop", func(t *testing.T) {
		r := newWCRig(t, fleetv1.ActionPhaseInProgress)
		r.stopRobot()
		r.report(r.ing, wcFailed, u64(wcGeneration), "")
		first := r.action()
		r.report(r.ing, wcFailed, u64(wcGeneration), "")
		unchanged(r, first.ResourceVersion, fleetv1.ActionPhasePaused)
		if n := r.audits(audit.EventActionPausedByEstop); n != 1 {
			t.Errorf("%d %s audit entries, want 1", n, audit.EventActionPausedByEstop)
		}
		if n := len(r.events(EventActionFailedUnderEstop)); n != 1 {
			t.Errorf("%d %s Events, want 1", n, EventActionFailedUnderEstop)
		}
	})

	// The same report, delivered twice, is applied by the other delivery between this one's
	// read and its write.
	for _, s := range terminal {
		s := s
		t.Run("duplicate-in-the-race/"+s.String(), func(t *testing.T) {
			r := newWCRig(t, fleetv1.ActionPhaseInProgress)
			want := fleetv1.ActionPhaseSucceeded
			if s == wcFailed {
				r.stopRobot()
				want = fleetv1.ActionPhasePaused
			}
			other := &ActionStatusIngestor{Client: envK8s, Recorder: r.ev, Audit: r.aud}
			var rv string
			r.race = func(n int) {
				if n == 1 {
					r.report(other, s, u64(wcGeneration), "")
					rv = r.action().ResourceVersion
				}
			}
			r.report(r.ing, s, u64(wcGeneration), "")
			r.raced()
			unchanged(r, rv, want)
			if s == wcSucceeded && len(r.events(wcReportNotApplied)) != 0 {
				t.Errorf("%s Events %+v: the report's own outcome was already written", wcReportNotApplied, r.ev.got)
			}
			if s == wcFailed {
				if n := r.audits(audit.EventActionPausedByEstop); n != 1 {
					t.Errorf("%d %s audit entries, want 1", n, audit.EventActionPausedByEstop)
				}
				if n := len(r.events(EventActionFailedUnderEstop)); n != 1 {
					t.Errorf("%d %s Events, want 1", n, EventActionFailedUnderEstop)
				}
			}
		})
	}
}
