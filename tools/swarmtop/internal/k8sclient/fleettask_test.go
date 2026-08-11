// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sclient

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swarmadav1 "github.com/swarmada/swarmada/api/v1"
)

// TestMapFleetTask pins the composite projection: the controller's actionSummary
// passes through verbatim, every status.actions[] entry becomes a member in the
// recorded order, and an absent completionTime is reported as unknown rather
// than as the zero time (the same trap TestMapFleetZone guards for lastEstopAt).
func TestMapFleetTask(t *testing.T) {
	started := metav1.NewTime(time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC))
	created := metav1.NewTime(time.Date(2026, 8, 9, 8, 55, 0, 0, time.UTC))
	task := &swarmadav1.FleetTask{
		ObjectMeta: metav1.ObjectMeta{Name: "receiving-round-001", CreationTimestamp: created},
		Spec: swarmadav1.FleetTaskSpec{
			CompletionPolicy: swarmadav1.CompletionPolicyAll,
			FailurePolicy:    swarmadav1.FailurePolicyFailFast,
			DesiredState:     swarmadav1.DesiredStateRunning,
		},
		Status: swarmadav1.FleetTaskStatus{
			Phase:         swarmadav1.FleetTaskPhaseRunning,
			ActionSummary: "1/3 Succeeded",
			StartedAt:     &started,
			Actions: []swarmadav1.FleetTaskActionStatus{
				{
					Name:            "approach-dock",
					ActionRef:       "receiving-round-001-approach-dock",
					Phase:           swarmadav1.ActionPhaseSucceeded,
					AssignedRobot:   "sim-robot-002",
					DependenciesMet: true,
					Attempt:         1,
				},
				{
					Name:            "inspect-dock",
					ActionRef:       "receiving-round-001-inspect-dock",
					Phase:           swarmadav1.ActionPhaseInProgress,
					AssignedRobot:   "sim-robot-001",
					DependenciesMet: true,
					Attempt:         2,
				},
				{
					Name:  "return-to-bay",
					Phase: swarmadav1.ActionPhasePending,
				},
			},
		},
	}

	v := mapFleetTask(task)

	if v.Name != "receiving-round-001" || v.Phase != "Running" {
		t.Fatalf("core: %+v", v)
	}
	if v.ActionSummary != "1/3 Succeeded" {
		t.Fatalf("actionSummary: got %q", v.ActionSummary)
	}
	if v.DesiredState != "Running" || v.CompletionPolicy != "All" || v.FailurePolicy != "FailFast" {
		t.Fatalf("policies: %+v", v)
	}
	if len(v.Members) != 3 {
		t.Fatalf("members: got %d, want 3 (%+v)", len(v.Members), v.Members)
	}
	if v.Members[0].Name != "approach-dock" || v.Members[2].Name != "return-to-bay" {
		t.Fatalf("member order not preserved: %+v", v.Members)
	}
	if m := v.Members[1]; m.ActionRef != "receiving-round-001-inspect-dock" ||
		m.AssignedRobot != "sim-robot-001" || m.Attempt != 2 || !m.DependenciesMet {
		t.Fatalf("member fields: %+v", m)
	}
	if v.StartedAtUnknown || !v.StartedAt.Equal(started.Time) {
		t.Fatalf("startedAt: unknown=%v at=%v", v.StartedAtUnknown, v.StartedAt)
	}
	if !v.CompletionTimeUnknown || !v.CompletionTime.IsZero() {
		t.Fatalf("an unset completionTime must read as unknown, got %v/%v",
			v.CompletionTimeUnknown, v.CompletionTime)
	}
	// CreatedAt is what kubectl's Age print column reads.
	if !v.CreatedAt.Equal(created.Time) {
		t.Fatalf("createdAt: got %v, want %v", v.CreatedAt, created.Time)
	}
}

// TestMapFleetTask_AbsentCreationStaysZero pins the mapper half of why CreatedAt
// carries no companion Unknown flag: an absent creation timestamp arrives here as
// the zero time and is passed through untouched, rather than being papered over.
// The rendering half — that a zero time dashes instead of dating the task to the
// epoch — belongs to format.Age and is pinned in format's own TestAge, because
// this package cannot import format (format imports k8sclient).
func TestMapFleetTask_AbsentCreationStaysZero(t *testing.T) {
	task := &swarmadav1.FleetTask{}
	task.Name = "no-creation-stamp"

	if v := mapFleetTask(task); !v.CreatedAt.IsZero() {
		t.Fatalf("expected a zero CreatedAt, got %v", v.CreatedAt)
	}
}

// TestCurrentMemberSelection is acceptance criterion 4: the member the task row
// headlines is the most advanced non-terminal one, and a settled task headlines
// nothing.
func TestCurrentMemberSelection(t *testing.T) {
	member := func(name string, phase swarmadav1.ActionPhase) swarmadav1.FleetTaskActionStatus {
		return swarmadav1.FleetTaskActionStatus{Name: name, Phase: phase}
	}

	cases := []struct {
		name    string
		phase   swarmadav1.FleetTaskPhase
		members []swarmadav1.FleetTaskActionStatus
		want    string
	}{
		{
			name:  "InProgress outranks Assigned and Pending",
			phase: swarmadav1.FleetTaskPhaseRunning,
			members: []swarmadav1.FleetTaskActionStatus{
				member("a", swarmadav1.ActionPhaseSucceeded),
				member("b", swarmadav1.ActionPhasePending),
				member("c", swarmadav1.ActionPhaseInProgress),
				member("d", swarmadav1.ActionPhaseAssigned),
			},
			want: "c",
		},
		{
			name:  "Assigned outranks Pending",
			phase: swarmadav1.FleetTaskPhaseRunning,
			members: []swarmadav1.FleetTaskActionStatus{
				member("a", swarmadav1.ActionPhasePending),
				member("b", swarmadav1.ActionPhaseAssigned),
			},
			want: "b",
		},
		{
			name:  "ties resolve to the earliest recorded member",
			phase: swarmadav1.FleetTaskPhaseRunning,
			members: []swarmadav1.FleetTaskActionStatus{
				member("first", swarmadav1.ActionPhaseInProgress),
				member("second", swarmadav1.ActionPhaseInProgress),
			},
			want: "first",
		},
		{
			name:  "a terminal task headlines nothing",
			phase: swarmadav1.FleetTaskPhaseSucceeded,
			members: []swarmadav1.FleetTaskActionStatus{
				member("a", swarmadav1.ActionPhaseInProgress),
			},
			want: "",
		},
		{
			name:  "a cancelled task headlines nothing",
			phase: swarmadav1.FleetTaskPhaseCancelled,
			members: []swarmadav1.FleetTaskActionStatus{
				member("a", swarmadav1.ActionPhaseAssigned),
			},
			want: "",
		},
		{
			name:    "no members at all",
			phase:   swarmadav1.FleetTaskPhasePending,
			members: nil,
			want:    "",
		},
		{
			name:  "only terminal members leaves no candidate",
			phase: swarmadav1.FleetTaskPhaseRunning,
			members: []swarmadav1.FleetTaskActionStatus{
				member("a", swarmadav1.ActionPhaseSucceeded),
				member("b", swarmadav1.ActionPhaseFailed),
			},
			want: "",
		},
		{
			name:  "an unranked phase is not promoted to the headline",
			phase: swarmadav1.FleetTaskPhaseRunning,
			members: []swarmadav1.FleetTaskActionStatus{
				member("a", swarmadav1.ActionPhasePaused),
				member("b", swarmadav1.ActionPhasePreempted),
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &swarmadav1.FleetTask{}
			task.Name = "task"
			task.Status.Phase = tc.phase
			task.Status.Actions = tc.members

			if got := mapFleetTask(task).CurrentMember; got != tc.want {
				t.Fatalf("current member: got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMapFleetAction_OwnerTaskFromLabel covers the data source acceptance
// criterion 3 rests on: membership is read from the label the FleetTask
// controller stamps, never from ownerReferences.
func TestMapFleetAction_OwnerTaskFromLabel(t *testing.T) {
	member := &swarmadav1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "equiv-task-probe",
			Labels: map[string]string{"swarmada.io/fleettask": "equiv-task"},
		},
	}
	if got := mapFleetAction(member).OwnerTask; got != "equiv-task" {
		t.Fatalf("labelled action: got OwnerTask %q, want %q", got, "equiv-task")
	}

	standalone := &swarmadav1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Name: "equiv-standalone"},
	}
	if got := mapFleetAction(standalone).OwnerTask; got != "" {
		t.Fatalf("unlabelled action: got OwnerTask %q, want empty", got)
	}

	// An ownerReference alone must not make an action look like a member — the
	// label is the only signal, so a hand-made child without one stays standalone.
	owned := &swarmadav1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "hand-made-child",
			OwnerReferences: []metav1.OwnerReference{{Kind: "FleetTask", Name: "equiv-task"}},
		},
	}
	if got := mapFleetAction(owned).OwnerTask; got != "" {
		t.Fatalf("ownerReference-only action: got OwnerTask %q, want empty", got)
	}
}

// TestReducer_TasksFlowThroughAndSort exercises the informer-to-snapshot path
// for the new kind: EmitTask lands in the store, updates replace, deletes evict,
// and Snapshot sorts by name like every other collection.
func TestReducer_TasksFlowThroughAndSort(t *testing.T) {
	f, s := startWithFake(t)

	// Emit out of order to prove Snapshot sorts.
	f.EmitTask(EventAdded, FleetTaskView{Name: "task-b", Phase: "Running", ActionSummary: "0/2 Succeeded"})
	waitChanged(t, s)
	f.EmitTask(EventAdded, FleetTaskView{Name: "task-a", Phase: "Pending"})
	waitChanged(t, s)

	snap := s.Snapshot()
	if len(snap.Tasks) != 2 || snap.Tasks[0].Name != "task-a" || snap.Tasks[1].Name != "task-b" {
		t.Fatalf("tasks not sorted: %+v", snap.Tasks)
	}
	if snap.Tasks[1].ActionSummary != "0/2 Succeeded" {
		t.Fatalf("actionSummary did not survive the reducer: %+v", snap.Tasks[1])
	}

	f.EmitTask(EventUpdated, FleetTaskView{Name: "task-b", Phase: "Succeeded", ActionSummary: "2/2 Succeeded"})
	waitChanged(t, s)
	if got := s.Snapshot().Tasks; len(got) != 2 || got[1].Phase != "Succeeded" {
		t.Fatalf("after update: %+v", got)
	}

	f.EmitTask(EventDeleted, FleetTaskView{Name: "task-b"})
	waitChanged(t, s)
	if got := s.Snapshot().Tasks; len(got) != 1 || got[0].Name != "task-a" {
		t.Fatalf("after delete: %+v", got)
	}
}
