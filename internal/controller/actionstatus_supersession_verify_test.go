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
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/scheduler"
	"github.com/swarmada/swarmada/internal/tde"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// An adapter's report that an action was superseded in place (RFC-0001
// #fleetaction-directive-supersession, D1) is a claim, and it may end live work only if
// it checks out: the successor must exist in the same namespace, be live, and be bound to
// the same robot. A claim that does not verify leaves the action live, frees nothing,
// writes no status, and is recorded as refused.
//
// Every fixture below binds the superseded action "x" to robot "r1" in a zone with room
// for one robot, and holds that robot's slot in the TDE. A second robot asking for the
// zone is the observable: while x holds the slot it is Denied, and only a release lets it
// in. The assertions are on that reservation result, not on an internal count.

const supersessionZone = "z"

type supersessionFixture struct {
	c            client.Client
	r            *FleetActionReconciler
	ing          *ActionStatusIngestor
	engine       *tde.Engine
	events       *record.FakeRecorder
	audit        *auditSpy
	statusWrites *atomic.Int64
}

func liveAction(name, ns, robot string) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate, Zone: supersessionZone},
		Status: fleetv1.FleetActionStatus{
			Phase:                fleetv1.ActionPhaseInProgress,
			AssignedRobot:        robot,
			AssignmentGeneration: 4,
			LeaseExpiresAt:       &metav1.Time{Time: time.Now().Add(time.Minute)},
		},
	}
}

func newSupersessionFixture(t *testing.T, extra ...client.Object) *supersessionFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	objs := append([]client.Object{
		readyActionAdapter(),
		&fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Name: supersessionZone, Namespace: actionNS},
			Spec:       fleetv1.FleetZoneSpec{MaxConcurrentRobots: 1},
		},
		liveAction("x", actionNS, "r1"),
		&fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: actionNS},
			Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseInProgress, AssignedAction: "x"},
		},
	}, extra...)
	var writes atomic.Int64
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAction{}, &fleetv1.Robot{}, &fleetv1.FleetZone{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if _, ok := obj.(*fleetv1.FleetAction); ok {
					writes.Add(1)
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	engine := tde.New(c, tde.DefaultConfig())
	engine.SetRecovered(true)
	if res, err := engine.RequestReservation(context.Background(), tde.ReservationRequest{
		ActionID: "x", RobotID: "r1", Namespace: actionNS, TargetZone: supersessionZone,
		Priority: fleetv1.ActionPriorityNormal,
	}); err != nil || res.Status != tde.Granted {
		t.Fatalf("fixture: reserving x's slot: status=%s err=%v", res.Status, err)
	}

	events := record.NewFakeRecorder(16)
	spy := &auditSpy{}
	return &supersessionFixture{
		c:            c,
		r:            &FleetActionReconciler{Client: c, Scheme: scheme, Scheduler: scheduler.NewDefaultScheduler(), TDE: engine},
		ing:          &ActionStatusIngestor{Client: c, Recorder: events, Audit: spy},
		engine:       engine,
		events:       events,
		audit:        spy,
		statusWrites: &writes,
	}
}

func (f *supersessionFixture) report(t *testing.T, supersededBy string) {
	t.Helper()
	token := uint64(4)
	if err := f.ing.IngestActionStatus(context.Background(), actionNS, allowAnyRobot, &fav1.ActionStatusUpdate{
		ActionId:            "x",
		State:               fav1.ActionState_ACTION_STATE_CANCELLED,
		EndCause:            fav1.ActionEndCause_ACTION_END_CAUSE_SUPERSEDED,
		SupersedingActionId: supersededBy,
		FencingToken:        &token,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

// secondRobotReservation asks for x's zone on behalf of another robot. It is Granted only
// if the zone has a free slot.
func (f *supersessionFixture) secondRobotReservation(t *testing.T) tde.ReservationStatus {
	t.Helper()
	res, err := f.engine.RequestReservation(context.Background(), tde.ReservationRequest{
		ActionID: "other", RobotID: "r9", Namespace: actionNS, TargetZone: supersessionZone,
		Priority: fleetv1.ActionPriorityNormal,
	})
	if err != nil {
		t.Fatalf("second robot reservation: %v", err)
	}
	return res.Status
}

// assertRefusedAndNothingFreed is the whole contract for a claim that does not verify.
func (f *supersessionFixture) assertRefusedAndNothingFreed(t *testing.T, claimed string) {
	t.Helper()
	if n := f.statusWrites.Load(); n != 0 {
		t.Errorf("refused report wrote FleetAction status %d time(s), want 0: a report that "+
			"changes nothing must not write", n)
	}

	// One reconcile, as the watch on the action would run if anything had been written.
	reconcileAction(t, f.r, "x")

	x := getAction(t, f.c, "x")
	if x.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s, want InProgress: an unverified supersession claim must not "+
			"end live work", x.Status.Phase)
	}
	if x.Status.SupersededBy != "" || x.Status.CancellationReason != "" {
		t.Errorf("supersededBy=%q cancellationReason=%q, want both empty on a refused claim",
			x.Status.SupersededBy, x.Status.CancellationReason)
	}
	if got := getRobot(t, f.c, "r1", actionNS); got.Status.AssignedAction != "x" {
		t.Errorf("robot r1 assignedAction = %q, want x: the robot is still executing", got.Status.AssignedAction)
	}
	if got := f.secondRobotReservation(t); got == tde.Granted {
		t.Errorf("a second robot was granted x's zone after a refused claim: the slot of a " +
			"still-running robot was released")
	}

	// Recorded, not dropped: a Denied audit entry naming the claim, and a Warning Event.
	var denied *audit.Entry
	for i := range f.audit.entries {
		e := f.audit.entries[i]
		if e.EventType == audit.EventFleetActionSuperseded && e.Outcome == audit.OutcomeDenied {
			denied = &e
		}
		if e.Outcome == audit.OutcomeAllowed {
			t.Errorf("audit recorded an Allowed %s for a refused claim", e.EventType)
		}
	}
	if denied == nil {
		t.Fatalf("no Denied %s audit entry: a refused claim must be recorded, not dropped (got %v)",
			audit.EventFleetActionSuperseded, f.audit.typesRecorded())
	}
	if denied.Resource.Name != "x" || denied.Detail["claimedSupersededBy"] != claimed || denied.Detail["reason"] == "" {
		t.Errorf("audit entry = %+v, want resource x, claimedSupersededBy %q and a reason", *denied, claimed)
	}
	sawWarning := false
	for {
		select {
		case e := <-f.events.Events:
			if strings.HasPrefix(e, "Warning "+EventActionSupersessionRefused) && strings.Contains(e, claimed) {
				sawWarning = true
			}
			continue
		default:
		}
		break
	}
	if !sawWarning {
		t.Errorf("no Warning %s Event naming %q", EventActionSupersessionRefused, claimed)
	}
}

func TestSupersessionClaim_NonexistentSuccessorIsRefused(t *testing.T) {
	f := newSupersessionFixture(t)
	f.report(t, "no-such-action")
	f.assertRefusedAndNothingFreed(t, "no-such-action")
}

// The successor lookup is scoped to the superseded action's namespace, so an action of
// the same name elsewhere does not satisfy it.
func TestSupersessionClaim_SuccessorInAnotherNamespaceIsRefused(t *testing.T) {
	f := newSupersessionFixture(t, liveAction("y", "elsewhere", "r1"))
	f.report(t, "y")
	f.assertRefusedAndNothingFreed(t, "y")
}

func TestSupersessionClaim_SuccessorOnAnotherRobotIsRefused(t *testing.T) {
	f := newSupersessionFixture(t, liveAction("y", actionNS, "r2"))
	f.report(t, "y")
	f.assertRefusedAndNothingFreed(t, "y")
}

// A terminal action keeps its assignedRobot, so without the liveness check any finished
// action of the same robot would verify.
func TestSupersessionClaim_FinishedSuccessorIsRefused(t *testing.T) {
	done := liveAction("y", actionNS, "r1")
	done.Status.Phase = fleetv1.ActionPhaseSucceeded
	f := newSupersessionFixture(t, done)
	f.report(t, "y")
	f.assertRefusedAndNothingFreed(t, "y")
}

func TestSupersessionClaim_SelfNamedSuccessorIsRefused(t *testing.T) {
	f := newSupersessionFixture(t)
	f.report(t, "x")
	f.assertRefusedAndNothingFreed(t, "x")
}

// A report from an adapter that does not serve r1. The ingestor is not told who sent a
// report, so what such an adapter can do is name an action it does know — here "y", the
// live action on its own robot r2 — and the claim fails on the robot binding. Whatever it
// names, x's robot stays bound and x's slot stays held.
func TestSupersessionClaim_ReportFromAnotherAdapterFreesNothing(t *testing.T) {
	f := newSupersessionFixture(t,
		liveAction("y", actionNS, "r2"),
		&fleetv1.Robot{
			ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: actionNS},
			Status:     fleetv1.RobotStatus{Phase: fleetv1.RobotPhaseInProgress, AssignedAction: "y"},
		})
	f.report(t, "y")
	f.assertRefusedAndNothingFreed(t, "y")
	if got := getAction(t, f.c, "y"); got.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Errorf("the named action y moved to %s; a claim about x must not touch y", got.Status.Phase)
	}
}

// A claim that verifies ends x, and the reconciler's terminal branch — the same one
// every Succeeded and Cancelled action retires through — frees the robot, clears the
// lease and releases the zone slot. The successor here is constructed by the test; the
// control plane itself has no path that binds one today.
func TestSupersessionClaim_VerifiedClaimEndsTheActionAndReleasesIt(t *testing.T) {
	f := newSupersessionFixture(t, liveAction("y", actionNS, "r1"))
	if got := f.secondRobotReservation(t); got == tde.Granted {
		t.Fatalf("precondition: a second robot was granted the zone while x holds it")
	}

	f.report(t, "y")
	x := getAction(t, f.c, "x")
	if x.Status.Phase != fleetv1.ActionPhaseCancelled ||
		x.Status.CancellationReason != fleetv1.CancellationReasonSupersededByDirective ||
		x.Status.SupersededBy != "y" {
		t.Fatalf("after a verified claim: phase=%s reason=%q supersededBy=%q, want Cancelled, "+
			"SupersededByDirective, y", x.Status.Phase, x.Status.CancellationReason, x.Status.SupersededBy)
	}
	for _, e := range f.audit.entries {
		if e.Outcome == audit.OutcomeDenied {
			t.Errorf("a verified claim recorded a Denied %s entry", e.EventType)
		}
	}

	reconcileAction(t, f.r, "x")

	x = getAction(t, f.c, "x")
	if x.Status.LeaseExpiresAt != nil {
		t.Errorf("lease horizon = %v after retirement, want cleared", x.Status.LeaseExpiresAt)
	}
	r1 := getRobot(t, f.c, "r1", actionNS)
	if r1.Status.AssignedAction != "" || r1.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Errorf("robot r1 = {phase %s, assignedAction %q}, want Idle and unbound",
			r1.Status.Phase, r1.Status.AssignedAction)
	}
	if got := f.secondRobotReservation(t); got != tde.Granted {
		t.Errorf("second robot reservation = %s, want Granted: x's slot was not released", got)
	}
}
