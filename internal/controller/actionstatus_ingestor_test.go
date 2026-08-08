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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

func newActionStatusIngestor(t *testing.T, objs ...client.Object) (*ActionStatusIngestor, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fleetv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
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
	if err := i.IngestActionStatus(context.Background(), actionNS, u); err != nil {
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

// A nil fencing token is treated as current (adapters may omit it).
func TestIngest_NilFencingToken_TreatedAsCurrent(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhaseAssigned, 5, nil))

	ingest(t, i, &fav1.ActionStatusUpdate{ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING})

	if getAction(t, c, "t1").Status.Phase != fleetv1.ActionPhaseInProgress {
		t.Fatal("nil fencing token should be treated as current and advance the task")
	}
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
	if err := i.IngestActionStatus(context.Background(), actionNS,
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
