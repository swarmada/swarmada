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
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// A running report for work that has already left Assigned is refused (it moves no phase), but
// it is the evidence that the robot began the work: it records startedAt (RFC-0001 safety.md,
// the terms, "started"). Without it a stop's later rows (A5, A12) read the work as not started
// and may move work a robot holds.
func TestIngest_RefusedRunning_RecordsTheStart(t *testing.T) {
	stopUnconfirmed := func(a *fleetv1.FleetAction) *fleetv1.FleetAction {
		a.Status.FailureReason = reasonStopUnconfirmed
		return a
	}
	for _, tc := range []struct {
		name   string
		action *fleetv1.FleetAction
		token  uint64
		want   bool
	}{
		{"paused", assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 5, nil), 5, true},
		{"preempted", assignedAction("t1", "r1", fleetv1.ActionPhasePreempted, 5, nil), 5, true},
		{"revoking", assignedAction("t1", "r1", fleetv1.ActionPhaseRevoking, 5, nil), 5, true},
		{"failed-stop-unconfirmed", stopUnconfirmed(assignedAction("t1", "r1", fleetv1.ActionPhaseFailed, 5, nil)), 5, true},
		{"paused-stale-token", assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 5, nil), 4, false},
		{"paused-unbound", assignedAction("t1", "", fleetv1.ActionPhasePaused, 5, nil), 5, false},
		{"failed-other-reason", assignedAction("t1", "r1", fleetv1.ActionPhaseFailed, 5, nil), 5, false},
		{"succeeded", assignedAction("t1", "r1", fleetv1.ActionPhaseSucceeded, 5, nil), 5, false},
		{"cancelled", assignedAction("t1", "r1", fleetv1.ActionPhaseCancelled, 5, nil), 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase := tc.action.Status.Phase
			i, c := newActionStatusIngestor(t, tc.action)
			ingest(t, i, &fav1.ActionStatusUpdate{
				ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(tc.token),
			})
			ft := getAction(t, c, "t1")
			if ft.Status.Phase != phase {
				t.Fatalf("phase = %s, want %s: a refused running report moves no phase", ft.Status.Phase, phase)
			}
			if got := ft.Status.StartedAt != nil; got != tc.want {
				t.Fatalf("startedAt recorded = %v, want %v", got, tc.want)
			}
		})
	}
}

// The start is recorded once: a second refused running report writes nothing (RA-1).
func TestIngest_RefusedRunning_StartRecordedOnce(t *testing.T) {
	i, c := newActionStatusIngestor(t, assignedAction("t1", "r1", fleetv1.ActionPhasePaused, 5, nil))
	u := &fav1.ActionStatusUpdate{ActionId: "t1", State: fav1.ActionState_ACTION_STATE_RUNNING, FencingToken: u64(5)}
	ingest(t, i, u)
	first := getAction(t, c, "t1")
	ingest(t, i, u)
	second := getAction(t, c, "t1")
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("a second refused running report wrote the action (rv %s -> %s)", first.ResourceVersion, second.ResourceVersion)
	}
}
