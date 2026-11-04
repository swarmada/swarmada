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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func newActionStatusIngestor(t *testing.T, objs ...client.Object) (*ActionStatusIngestor, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	// The ingestor reads the Robot an action is assigned to, and authorizes the report by
	// that Robot's wire robot_id. A fixture that assigns an action to a robot it does not
	// supply gets a plain Robot of that name, whose robot_id is its name.
	have := map[string]bool{}
	for _, o := range objs {
		if r, ok := o.(*fleetv1.Robot); ok {
			have[r.Name] = true
		}
	}
	for _, o := range objs {
		if a, ok := o.(*fleetv1.FleetAction); ok && a.Status.AssignedRobot != "" && !have[a.Status.AssignedRobot] {
			have[a.Status.AssignedRobot] = true
			objs = append(objs, &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: a.Status.AssignedRobot, Namespace: a.Namespace}})
		}
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&fleetv1.FleetAction{}).
		Build()
	return &ActionStatusIngestor{Client: c}, c
}

func u64(v uint64) *uint64 { return &v }

func ingest(t *testing.T, i *ActionStatusIngestor, u *fav1.ActionStatusUpdate) {
	t.Helper()
	if err := i.IngestActionStatus(context.Background(), actionNS, allowAnyRobot, u); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

// RUNNING advances an assigned action to InProgress and stamps startedAt.
func TestIngest_Running_AssignedToInProgress(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseAssigned, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(5),
	})

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s, want InProgress", ft.Status.Phase)
	}
	if ft.Status.StartedAt == nil {
		t.Fatal("startedAt was not stamped on RUNNING")
	}
}

// A report without a fencing token is refused like a stale one: it names no robot, so nothing ties
// it to the run it came from (conformance C6.4 requires the echo). A Warning Event on the adapter's
// own FleetAdapter says so for every terminal report, and once per connection for running reports.
func TestIngest_NilFencingToken_Refused(t *testing.T) {
	adapter := &fleetv1.FleetAdapter{ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: actionNS}}
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil), adapter)
	rec := &eventTargets{}
	i.Recorder = rec
	defer func(prev func(context.Context) controlstream.TLSIdentity) { streamIdentity = prev }(streamIdentity)
	streamIdentity = func(context.Context) controlstream.TLSIdentity {
		return controlstream.TLSIdentity{AdapterName: "acme", Namespace: actionNS, Verified: true}
	}

	for _, s := range []fav1.ActionState{fav1.ActionState_ACTION_STATE_RUNNING, fav1.ActionState_ACTION_STATE_RUNNING,
		fav1.ActionState_ACTION_STATE_CANCELLED, fav1.ActionState_ACTION_STATE_FAILED, fav1.ActionState_ACTION_STATE_SUCCEEDED} {
		ingest(t, i, &fav1.ActionStatusUpdate{ActionId: "t1", State: s})
	}

	if ft := getAction(t, c, "t1"); ft.Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatalf("phase = %s, want InProgress: a report without a fencing token must not move the action", ft.Status.Phase)
	}
	if len(rec.got) != 4 {
		t.Fatalf("%d Events (%v), want 4: one for the connection's running reports, one per terminal report", len(rec.got), rec.got)
	}
	for _, e := range rec.got {
		if e.kind != "FleetAdapter" || e.name != "acme" || e.uid != adapter.UID || e.reason != EventAdapterOmitsFencingToken ||
			!strings.Contains(e.msg, "C6.4") {
			t.Errorf("Event %+v, want %s on the FleetAdapter acme itself, naming conformance C6.4", e, EventAdapterOmitsFencingToken)
		}
	}
}

// eventTargets records which object each Event was given.
type eventTargets struct {
	got []recordedEvent
}

type recordedEvent struct {
	kind, name, reason, msg string
	uid                     types.UID
}

func (o *eventTargets) Event(obj runtime.Object, _, reason, msg string) {
	m, _ := obj.(client.Object)
	kind := fmt.Sprintf("%T", obj)
	if _, ok := obj.(*fleetv1.FleetAdapter); ok {
		kind = "FleetAdapter"
	}
	o.got = append(o.got, recordedEvent{kind: kind, name: m.GetName(), uid: m.GetUID(), reason: reason, msg: msg})
}

func (o *eventTargets) Eventf(obj runtime.Object, typ, reason, format string, args ...interface{}) {
	o.Event(obj, typ, reason, fmt.Sprintf(format, args...))
}

func (o *eventTargets) AnnotatedEventf(obj runtime.Object, _ map[string]string, typ, reason, format string, args ...interface{}) {
	o.Eventf(obj, typ, reason, format, args...)
}

// A superseded assignment (stale fencing token) must not move the current action.
func TestIngest_StaleFencingToken_Ignored(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseAssigned, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(4),
	})

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseAssigned {
		t.Fatalf("phase = %s, want Assigned (stale report must be ignored)", ft.Status.Phase)
	}
	if ft.Status.StartedAt != nil {
		t.Fatal("stale report stamped startedAt")
	}
}

// A status update for an unknown action is stale, not an error.
func TestIngest_UnknownAction_NoError(t *testing.T) {
	i, _ := newActionStatusIngestor(t)
	if err := i.IngestActionStatus(context.Background(), actionNS, allowAnyRobot,
		&fav1.ActionStatusUpdate{ActionId: "ghost", State: fav1.ActionState_ACTION_STATE_RUNNING}); err != nil {
		t.Fatalf("unknown task should be a no-op, got error: %v", err)
	}
}

// RUNNING is idempotent: a re-report on an already-started action rewrites nothing.
func TestIngest_Running_Idempotent(t *testing.T) {
	action := assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil)
	started := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	action.Status.StartedAt = &started
	i, c := newActionStatusIngestor(t, action)

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(5),
	})

	ft := getAction(t, c, "t1")
	if ft.Status.StartedAt == nil || !ft.Status.StartedAt.Time.Equal(started.Time) {
		t.Fatalf("startedAt was rewritten on an idempotent RUNNING re-report: got %v, want %v",
			ft.Status.StartedAt, started)
	}
}

// SUCCEEDED drives Succeeded, stamps completedAt/completionTime, and back-fills
// startedAt for a action that completed within a single report.
func TestIngest_Succeeded_SetsCompletedAtAndBackfillsStarted(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseAssigned, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_SUCCEEDED, FencingToken: u64(5), Message: "done",
	})

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", ft.Status.Phase)
	}
	if ft.Status.CompletedAt == nil || ft.Status.CompletionTime == nil {
		t.Fatal("completedAt/completionTime not stamped on SUCCEEDED")
	}
	if ft.Status.StartedAt == nil {
		t.Fatal("startedAt not back-filled for a single-report completion")
	}
	if ft.Status.Message != "done" {
		t.Fatalf("message = %q, want %q", ft.Status.Message, "done")
	}
}

// FAILED drives Failed and records the reported reason.
func TestIngest_Failed_SetsFailedAtAndReason(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_FAILED, FencingToken: u64(5), Message: "gripper jam",
	})

	ft := getAction(t, c, "t1")
	if ft.Status.Phase != fleetv1.ActionPhaseFailed {
		t.Fatalf("phase = %s, want Failed", ft.Status.Phase)
	}
	if ft.Status.FailedAt == nil {
		t.Fatal("failedAt not stamped on FAILED")
	}
	if ft.Status.FailureReason != "gripper jam" {
		t.Fatalf("failureReason = %q, want %q", ft.Status.FailureReason, "gripper jam")
	}
}

// FAILED with no message falls back to a default reason.
func TestIngest_Failed_DefaultReasonWhenEmpty(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_FAILED, FencingToken: u64(5),
	})

	if r := getAction(t, c, "t1").Status.FailureReason; r == "" {
		t.Fatal("failureReason should default to a non-empty string when the report omits a message")
	}
}

// A action already in a terminal phase is authoritative and not overwritten.
func TestIngest_TerminalAction_NotOverwritten(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseSucceeded, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_FAILED, FencingToken: u64(5),
	})

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseSucceeded {
		t.Fatal("a terminal task was overwritten by a later robot report")
	}
}

// RUNNING surfaces the reported progress onto status.progressPct.
func TestIngest_Running_StoresProgress(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseAssigned, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(5), ProgressPct: 42,
	})

	if got := getAction(t, c, "t1").Status.ProgressPct; got != 42 {
		t.Fatalf("progressPct = %d, want 42", got)
	}
}

// An out-of-range progress report is clamped into [0,100].
func TestIngest_Running_ProgressClamped(t *testing.T) {
	action := assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil)
	started := metav1.NewTime(time.Now().Add(-time.Minute))
	action.Status.StartedAt = &started
	i, c := newActionStatusIngestor(t, action)

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(5), ProgressPct: 150,
	})

	if got := getAction(t, c, "t1").Status.ProgressPct; got != 100 {
		t.Fatalf("progressPct = %d, want 100 (clamped)", got)
	}
}

// SUCCEEDED reports full progress.
func TestIngest_Succeeded_ProgressFull(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_SUCCEEDED, FencingToken: u64(5),
	})

	if got := getAction(t, c, "t1").Status.ProgressPct; got != 100 {
		t.Fatalf("progressPct = %d, want 100 on Succeeded", got)
	}
}

// PAUSED is not acted on from a robot report (pause is a capability concern).
func TestIngest_Paused_Ignored(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseInProgress, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{
		ActionId: "t1", State: fav1.ActionState_ACTION_STATE_PAUSED, FencingToken: u64(5),
	})

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatal("PAUSED report should not change the task phase")
	}
}
