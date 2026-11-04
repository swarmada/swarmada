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

// ─────────────────────────────────────────────────────────────────────────────
// EVERY ARM OF A fromMember BINDING, AND WHAT EACH MISS HAS TO SAY.
//
// fleettask_params.go states the obligation plainly: "this function's whole
// obligation is the visibility half: every fromMember binding gets a
// status.parameterResolution entry saying what it resolved to or why it did not."
// A miss whose reason does not name what was missing satisfies the letter of that
// and none of its point — an author reading "unresolved" learns nothing, and the
// wrong field name is the most common cause.
//
// fleettask_params_test.go drives the EVENT arm through a full reconcile. These
// tests call the resolver directly instead, because the arms below are decided
// entirely by the binding and the producer's state: a reconcile would add a
// cluster, a child object and a fake client to reach the same branch, and would
// still not let a payload-shape case say which shape it meant.
//
// The RESULT arm had no test of any kind before this file, success path included.
// ─────────────────────────────────────────────────────────────────────────────

import (
	"strings"
	"testing"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

// staticChild answers childFn with one producer state at one iteration, and
// "no such child" everywhere else.
func staticChild(member string, iteration int32, st flow.ChildState) func(string, int32) flow.ChildState {
	return func(m string, i int32) flow.ChildState {
		if m == member && i == iteration {
			return st
		}
		return flow.ChildState{}
	}
}

// noChild answers childFn with "nothing exists anywhere".
func noChild() func(string, int32) flow.ChildState {
	return func(string, int32) flow.ChildState { return flow.ChildState{} }
}

// requireMiss asserts the binding did not resolve and that its recorded reason
// carries every fragment an author would need to find the mistake.
func requireMiss(t *testing.T, got resolvedBinding, mustSay ...string) {
	t.Helper()
	if got.entry.Resolved || got.value != nil {
		t.Fatalf("the binding resolved to %+v; it must not have", got.entry)
	}
	if got.entry.Source != "member" {
		t.Errorf("source = %q, want \"member\"", got.entry.Source)
	}
	for _, frag := range mustSay {
		if !strings.Contains(got.entry.Reason, frag) {
			t.Errorf("the reason does not mention %q, so it does not say what went wrong: %s",
				frag, got.entry.Reason)
		}
	}
}

// A producer with no child at this iteration and none at iteration 0 is not an
// error and not a silent empty parameter: it is a recorded miss naming the member
// and the iteration that was searched.
func TestResolveMemberBinding_AbsentProducerIsARecordedMissNamingIt(t *testing.T) {
	got := resolveOneMemberBinding("zone",
		&fleetv1.MemberBinding{Name: "watch", Event: strPtr("IntruderDetected"), Field: strPtr("zone")},
		4, noChild())

	requireMiss(t, got, "watch", "iteration 4")
	if got.entry.FromMember != "watch" {
		t.Errorf("fromMember = %q; the entry must name the member even on a miss", got.entry.FromMember)
	}
}

// "The producing member is looked up at THIS iteration first and then at
// iteration 0 … a resident watcher beside it. A resident recognition service is
// the common shape for the event arm, and it lives at iteration 0." So a consumer
// on iteration 4 reads the watcher that was started once, outside the loop.
func TestProducerState_FallsBackToTheResidentProducerAtIterationZero(t *testing.T) {
	resident := flow.ChildState{
		Exists: true,
		Phase:  fleetv1.ActionPhaseInProgress,
		Events: []fleetv1.ActionEvent{
			{Type: "IntruderDetected", Seq: 9, Payload: map[string]string{"zone": "dock"}},
		},
	}
	childFn := staticChild("watch", 0, resident)

	st, ok := producerState("watch", 4, childFn)
	if !ok {
		t.Fatal("a resident producer at iteration 0 was not found from iteration 4")
	}
	if len(st.Events) != 1 || st.Events[0].Seq != 9 {
		t.Fatalf("the wrong producer state came back: %+v", st)
	}

	// And the binding resolves through it, rather than reporting the miss above.
	got := resolveOneMemberBinding("zone",
		&fleetv1.MemberBinding{Name: "watch", Event: strPtr("IntruderDetected"), Field: strPtr("zone")},
		4, childFn)
	if !got.entry.Resolved || got.value == nil || *got.value != "dock" {
		t.Fatalf("the binding did not read the resident producer: %+v", got.entry)
	}
	if got.entry.FromEventSeq != 9 {
		t.Errorf("fromEventSeq = %d, want 9 — the resolution must pin the exact observation",
			got.entry.FromEventSeq)
	}
}

// The iteration-0 fallback must not fire when the consumer IS at iteration 0:
// there is nowhere else to look, and a second lookup at the same coordinate
// would be a wasted call that could only ever return the same answer.
func TestProducerState_AtIterationZeroThereIsNoSecondLookup(t *testing.T) {
	calls := 0
	childFn := func(string, int32) flow.ChildState {
		calls++
		return flow.ChildState{}
	}
	if _, ok := producerState("watch", 0, childFn); ok {
		t.Fatal("producerState reported a child that does not exist")
	}
	if calls != 1 {
		t.Errorf("childFn was called %d times at iteration 0, want 1", calls)
	}
}

// MemberBinding.Field's godoc: "Required with Event." The resolver states that
// rather than defaulting it, because "there is no sensible whole-payload encoding,
// and picking one would hand the executor a string the author never wrote."
func TestResolveMemberBinding_EventWithoutFieldIsRefusedNotGuessed(t *testing.T) {
	producer := flow.ChildState{
		Exists: true,
		Events: []fleetv1.ActionEvent{{Type: "IntruderDetected", Seq: 1,
			Payload: map[string]string{"zone": "dock"}}},
	}
	for name, field := range map[string]*string{
		"field unset": nil,
		"field empty": strPtr(""),
	} {
		t.Run(name, func(t *testing.T) {
			got := resolveOneMemberBinding("zone",
				&fleetv1.MemberBinding{Name: "watch", Event: strPtr("IntruderDetected"), Field: field},
				0, staticChild("watch", 0, producer))
			requireMiss(t, got, "IntruderDetected", "field is required")
		})
	}
}

// A misspelled payload field is the mistake this reason exists for, so it lists
// the keys the event actually carried — sorted, and "none" when the executor sent
// an empty payload, which is a different mistake needing a different answer.
func TestResolveMemberBinding_MissingPayloadFieldListsWhatTheEventCarried(t *testing.T) {
	t.Run("the keys that were present, sorted", func(t *testing.T) {
		producer := flow.ChildState{
			Exists: true,
			Events: []fleetv1.ActionEvent{{Type: "IntruderDetected", Seq: 7, Payload: map[string]string{
				"zone": "dock", "confidence": "0.91", "atSeq": "7",
			}}},
		}
		got := resolveOneMemberBinding("zone",
			&fleetv1.MemberBinding{Name: "watch", Event: strPtr("IntruderDetected"), Field: strPtr("zoneName")},
			0, staticChild("watch", 0, producer))

		requireMiss(t, got, "zoneName", "atSeq, confidence, zone")
		// The entry still pins WHICH observation was read, so an author can go and
		// look at that exact event rather than at the type in general.
		if got.entry.FromEvent != "IntruderDetected" || got.entry.FromEventSeq != 7 {
			t.Errorf("the miss does not name the event it read: from=%q seq=%d",
				got.entry.FromEvent, got.entry.FromEventSeq)
		}
	})

	t.Run("an empty payload says none rather than an empty list", func(t *testing.T) {
		producer := flow.ChildState{
			Exists: true,
			Events: []fleetv1.ActionEvent{{Type: "IntruderDetected", Seq: 1}},
		}
		got := resolveOneMemberBinding("zone",
			&fleetv1.MemberBinding{Name: "watch", Event: strPtr("IntruderDetected"), Field: strPtr("zone")},
			0, staticChild("watch", 0, producer))
		requireMiss(t, got, "present: none")
	})
}

// "An App producer's status.result is written once at the terminal transition, so
// a value read before it is a value that can still change." A running producer
// therefore yields a miss, not an early read.
func TestResolveMemberBinding_ResultArmWaitsForTheProducerToSettle(t *testing.T) {
	binding := &fleetv1.MemberBinding{Name: "inspect", Result: strPtr("grade")}
	settled := &fleetv1.ActionResult{Values: map[string]string{"grade": "A"}}

	t.Run("still running, result already written", func(t *testing.T) {
		got := resolveOneMemberBinding("grade", binding, 0, staticChild("inspect", 0,
			flow.ChildState{Exists: true, Phase: fleetv1.ActionPhaseInProgress, Result: settled}))
		requireMiss(t, got, "inspect", "terminal")
	})

	t.Run("terminal, but no result written", func(t *testing.T) {
		got := resolveOneMemberBinding("grade", binding, 0, staticChild("inspect", 0,
			flow.ChildState{Exists: true, Phase: fleetv1.ActionPhaseSucceeded}))
		requireMiss(t, got, "inspect", "terminal")
	})

	t.Run("terminal with the value, resolved", func(t *testing.T) {
		got := resolveOneMemberBinding("grade", binding, 0, staticChild("inspect", 0,
			flow.ChildState{Exists: true, Phase: fleetv1.ActionPhaseSucceeded, Result: settled}))
		if !got.entry.Resolved || got.value == nil || *got.value != "A" {
			t.Fatalf("a settled producer's result did not resolve: %+v", got.entry)
		}
		if got.entry.FromMember != "inspect" {
			t.Errorf("fromMember = %q, want \"inspect\"", got.entry.FromMember)
		}
		// The result arm reads no event, so the event provenance stays empty rather
		// than carrying a stale or zero seq that a reader would take for a real one.
		if got.entry.FromEvent != "" || got.entry.FromEventSeq != 0 {
			t.Errorf("a result binding recorded event provenance: from=%q seq=%d",
				got.entry.FromEvent, got.entry.FromEventSeq)
		}
	})

	t.Run("terminal, result written, but not this value", func(t *testing.T) {
		got := resolveOneMemberBinding("grade",
			&fleetv1.MemberBinding{Name: "inspect", Result: strPtr("score")}, 0,
			staticChild("inspect", 0, flow.ChildState{
				Exists: true, Phase: fleetv1.ActionPhaseSucceeded, Result: settled,
			}))
		requireMiss(t, got, "inspect", "score")
	})
}

// The two arms with no reader are NAMED, not silently skipped: "it is not a
// fromMember-event concern" is a decision an author can read and act on, whereas
// an absent entry is indistinguishable from the resolver not having run.
func TestResolveMemberBinding_ArmsWithNoReaderAreNamedNotSkipped(t *testing.T) {
	producer := flow.ChildState{Exists: true, Phase: fleetv1.ActionPhaseSucceeded}

	t.Run("artifact says why a scalar parameter cannot take one", func(t *testing.T) {
		got := resolveOneMemberBinding("photo",
			&fleetv1.MemberBinding{Name: "inspect", Artifact: strPtr("frame")}, 0,
			staticChild("inspect", 0, producer))
		requireMiss(t, got, "artifact", "not a scalar")
	})

	t.Run("a binding naming no source at all says so", func(t *testing.T) {
		got := resolveOneMemberBinding("photo", &fleetv1.MemberBinding{Name: "inspect"}, 0,
			staticChild("inspect", 0, producer))
		requireMiss(t, got, "neither result, event nor artifact")
	})
}

// payloadKeysOrNone's own contract, exercised where the caller cannot reach it:
// the empty-map and nil-map cases are the same answer, and the sort is what makes
// two runs of the same miss produce the same reason string — a reason that
// reordered on every reconcile would be a status diff for a value that never moved.
func TestPayloadKeysOrNone_IsSortedAndSaysNoneForAnEmptyPayload(t *testing.T) {
	for name, tc := range map[string]struct {
		payload map[string]string
		want    string
	}{
		"nil payload":     {payload: nil, want: "none"},
		"empty payload":   {payload: map[string]string{}, want: "none"},
		"one key":         {payload: map[string]string{"zone": "dock"}, want: "zone"},
		"sorted, not map": {payload: map[string]string{"zone": "", "atSeq": "", "confidence": ""}, want: "atSeq, confidence, zone"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := payloadKeysOrNone(tc.payload); got != tc.want {
				t.Errorf("payloadKeysOrNone = %q, want %q", got, tc.want)
			}
		})
	}
}
