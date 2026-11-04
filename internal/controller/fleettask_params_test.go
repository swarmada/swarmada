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

// ParameterBinding.fromMember{event, field} — the third reader, and the one the
// whole event channel exists for. A select can branch on a recognition without
// it, but the action the branch starts would know only that SOMETHING was
// recognised.
//
// Every test here goes through reconcileFlow, not through the resolver in
// isolation: "the started action can see the observation" is a claim about the
// FleetAction that ends up in the cluster, and a unit test of the resolver would
// pass whether or not it was ever called.

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// watcherMember is a Service-contract producer member.
func watcherMember(name, endpoint string) fleetv1.FleetTaskAction {
	return fleetv1.FleetTaskAction{
		Name: name,
		Action: fleetv1.FleetActionSpec{
			Type:     fleetv1.ActionTypeCustom,
			Executor: &fleetv1.ActionExecutor{EndpointRef: endpoint},
		},
	}
}

// consumerMember binds one parameter to a sibling's event payload field.
func consumerMember(name, endpoint, param, from, event, field string) fleetv1.FleetTaskAction {
	return fleetv1.FleetTaskAction{
		Name: name,
		Action: fleetv1.FleetActionSpec{
			Type: fleetv1.ActionTypeCustom,
			Executor: &fleetv1.ActionExecutor{
				EndpointRef: endpoint,
				Parameters: map[string]fleetv1.ParameterBinding{
					param: {FromMember: &fleetv1.MemberBinding{
						Name: from, Event: &event, Field: &field,
					}},
				},
			},
		},
	}
}

// watcherChild is the producer's child action, carrying a raised event.
func watcherChild(taskName, member string, evs ...fleetv1.ActionEvent) *fleetv1.FleetAction {
	return &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{
			Name:            childName(taskName, member),
			Namespace:       actionNS,
			Labels:          map[string]string{ownedByLabel: taskName, memberLabel: member},
			OwnerReferences: []metav1.OwnerReference{fakeTaskControllerRef(taskName)},
		},
		Spec: fleetv1.FleetActionSpec{
			Type:     fleetv1.ActionTypeCustom,
			Executor: &fleetv1.ActionExecutor{EndpointRef: "intruder-detector"},
		},
		Status: fleetv1.FleetActionStatus{Phase: fleetv1.ActionPhaseInProgress, Events: evs},
	}
}

func resolutionOf(t *testing.T, a *fleetv1.FleetAction, param string) fleetv1.ParameterResolutionEntry {
	t.Helper()
	for _, e := range a.Status.ParameterResolution {
		if e.Name == param {
			return e
		}
	}
	t.Fatalf("no status.parameterResolution entry for %q (have %+v)", param, a.Status.ParameterResolution)
	return fleetv1.ParameterResolutionEntry{}
}

// THE ONE THIS EXISTS FOR. A watcher raises IntruderDetected{zone: dock}; a
// select branches on it; the branch's action is created with `zone` already
// bound to "dock", and its status records WHICH member raised it, WHICH event
// type, and WHICH seq.
func TestFromMemberEvent_CarriesTheObservationIntoTheStartedAction(t *testing.T) {
	evType := "IntruderDetected"
	task := flowTask("watch-task",
		&fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{
			Branches: []fleetv1.FlowNode{
				runNode("watch"),
				{Select: &fleetv1.SelectNode{
					Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
					Cases: []fleetv1.SelectCase{
						{Equals: strPtr("dock"), Then: runNode("respond")},
					},
					Default: fleetv1.FlowNode{},
				}},
			},
			OnJoin: fleetv1.OnJoinLetRunToCompletion,
		}},
		watcherMember("watch", "intruder-detector"),
		consumerMember("respond", "responder", "zone", "watch", "IntruderDetected", "zone"),
	)

	producer := watcherChild("watch-task", "watch",
		fleetv1.ActionEvent{Type: "IntruderDetected", Seq: 4, Payload: map[string]string{"zone": "aisle"}},
		fleetv1.ActionEvent{Type: "IntruderDetected", Seq: 9, Payload: map[string]string{"zone": "dock", "confidence": "0.93"}},
	)

	r, c := newFleetTaskReconciler(t, task, producer)
	reconcileFT(t, r, "watch-task")

	started := getActionOrNil(t, c, childName("watch-task", "respond"))
	if started == nil {
		t.Fatal("the select branch did not start 'respond' — the event arm did not read the observation at all")
	}

	// THE OBSERVATION IS IN THE STARTED ACTION'S SPEC, as a literal. A
	// fromMember binding is a FleetTask coordinate the child could never follow.
	b := started.Spec.Executor.Parameters["zone"]
	if b.Value == nil {
		t.Fatalf("parameter 'zone' was not materialised: %+v", b)
	}
	if *b.Value != "dock" {
		t.Fatalf("parameter 'zone' = %q, want \"dock\" — the highest-seq event said dock", *b.Value)
	}
	if b.FromMember != nil {
		t.Fatal("the unresolvable fromMember binding was left on the child; the dispatcher cannot follow it")
	}

	// AND WHICH MEMBER RAISED IT. The value alone cannot say: two members can
	// raise the same type with the same payload field and mean different things.
	e := resolutionOf(t, started, "zone")
	switch {
	case !e.Resolved:
		t.Fatalf("resolution not recorded as resolved: %+v", e)
	case e.Source != "member":
		t.Errorf("source = %q, want \"member\"", e.Source)
	case e.FromMember != "watch":
		t.Errorf("fromMember = %q, want \"watch\"", e.FromMember)
	case e.FromEvent != "IntruderDetected":
		t.Errorf("fromEvent = %q, want \"IntruderDetected\"", e.FromEvent)
	case e.FromEventSeq != 9:
		t.Errorf("fromEventSeq = %d, want 9 — the resolution must pin the exact observation, not just the type", e.FromEventSeq)
	case e.Value == nil || *e.Value != "dock":
		t.Errorf("recorded value = %v, want \"dock\"", e.Value)
	}
}

// RESOLUTION IS PINNED AT CREATION AND NEVER REWRITTEN. The producer keeps
// raising events after the consumer starts; if the binding were re-resolved, the
// parameter a running action was activated with would change under it.
func TestFromMemberEvent_ResolutionIsPinnedAtCreation(t *testing.T) {
	evType := "IntruderDetected"
	task := flowTask("pin-task",
		&fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{
			Branches: []fleetv1.FlowNode{
				runNode("watch"),
				{Select: &fleetv1.SelectNode{
					Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
					Cases:   []fleetv1.SelectCase{{Present: boolPtr(true), Then: runNode("respond")}},
					Default: fleetv1.FlowNode{},
				}},
			},
			OnJoin: fleetv1.OnJoinLetRunToCompletion,
		}},
		watcherMember("watch", "intruder-detector"),
		consumerMember("respond", "responder", "zone", "watch", "IntruderDetected", "zone"),
	)
	producer := watcherChild("pin-task", "watch",
		fleetv1.ActionEvent{Type: "IntruderDetected", Seq: 2, Payload: map[string]string{"zone": "dock"}},
	)

	r, c := newFleetTaskReconciler(t, task, producer)
	reconcileFT(t, r, "pin-task")

	// The watcher recognises something else. The already-started action must not
	// change underneath.
	live := getActionOrNil(t, c, childName("pin-task", "watch"))
	live.Status.Events = append(live.Status.Events,
		fleetv1.ActionEvent{Type: "IntruderDetected", Seq: 17, Payload: map[string]string{"zone": "loading-bay"}})
	live.Status.EventSeqHigh = 17
	if err := c.Status().Update(context.Background(), live); err != nil {
		t.Fatalf("update producer status: %v", err)
	}

	reconcileFT(t, r, "pin-task")
	reconcileFT(t, r, "pin-task")

	started := getActionOrNil(t, c, childName("pin-task", "respond"))
	if v := started.Spec.Executor.Parameters["zone"].Value; v == nil || *v != "dock" {
		t.Fatalf("parameter 'zone' = %v, want it pinned at \"dock\" — it was re-resolved to a later frame", v)
	}
	if e := resolutionOf(t, started, "zone"); e.FromEventSeq != 2 {
		t.Fatalf("fromEventSeq = %d, want 2 — the provenance was rewritten to a frame the action never saw", e.FromEventSeq)
	}
}

// A MISS IS RECORDED, NOT HIDDEN, AND DOES NOT WITHHOLD THE CHILD.
// ActionExecutor.OnUnresolvedParameter is schema-only at v0.4: no controller
// reads it, so no consequence of a miss is applied. This layer owes the
// visibility half, and the reason must name the
// member and the event so an author can act on it.
func TestFromMemberEvent_UnresolvedIsRecordedWithAReasonNamingWhatWasMissing(t *testing.T) {
	task := flowTask("miss-task",
		&fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("watch"), runNode("respond")}},
		watcherMember("watch", "intruder-detector"),
		consumerMember("respond", "responder", "zone", "watch", "IntruderDetected", "zone"),
	)
	// The producer settled having raised only a DIFFERENT type.
	producer := watcherChild("miss-task", "watch",
		fleetv1.ActionEvent{Type: "HeartbeatSeen", Seq: 3},
	)
	producer.Status.Phase = fleetv1.ActionPhaseSucceeded

	r, c := newFleetTaskReconciler(t, task, producer)
	reconcileFT(t, r, "miss-task")

	started := getActionOrNil(t, c, childName("miss-task", "respond"))
	if started == nil {
		t.Fatal("the child was withheld; an unresolved binding must be recorded, not deferred")
	}
	// The binding is LEFT AS IT IS — visibly unresolved, not replaced by "".
	if started.Spec.Executor.Parameters["zone"].FromMember == nil {
		t.Fatal("an unresolved binding was materialised anyway; a silent empty parameter is the failure this avoids")
	}
	e := resolutionOf(t, started, "zone")
	if e.Resolved {
		t.Fatalf("recorded as resolved: %+v", e)
	}
	for _, want := range []string{"watch", "IntruderDetected"} {
		if !containsSub(e.Reason, want) {
			t.Errorf("reason %q does not name %q", e.Reason, want)
		}
	}
	if e.FromMember != "watch" {
		t.Errorf("fromMember = %q, want \"watch\" even on a miss", e.FromMember)
	}
}

// The author's spec is not mutated by materialisation. Before the deep copy, the
// child's spec aliased task.Spec.Actions[i].Action, so writing a resolved
// literal into the child's parameter map wrote into the AUTHOR'S task.
func TestFromMemberEvent_MaterialisationDoesNotMutateTheTaskSpec(t *testing.T) {
	evType := "IntruderDetected"
	task := flowTask("alias-task",
		&fleetv1.FlowNode{Parallel: &fleetv1.ParallelNode{
			Branches: []fleetv1.FlowNode{
				runNode("watch"),
				{Select: &fleetv1.SelectNode{
					Subject: fleetv1.SelectSubject{Event: &evType, From: "watch", Field: "zone"},
					Cases:   []fleetv1.SelectCase{{Present: boolPtr(true), Then: runNode("respond")}},
					Default: fleetv1.FlowNode{},
				}},
			},
			OnJoin: fleetv1.OnJoinLetRunToCompletion,
		}},
		watcherMember("watch", "intruder-detector"),
		consumerMember("respond", "responder", "zone", "watch", "IntruderDetected", "zone"),
	)
	producer := watcherChild("alias-task", "watch",
		fleetv1.ActionEvent{Type: "IntruderDetected", Seq: 1, Payload: map[string]string{"zone": "dock"}},
	)

	r, c := newFleetTaskReconciler(t, task, producer)
	reconcileFT(t, r, "alias-task")

	after := getFT(t, c, "alias-task")
	for i := range after.Spec.Actions {
		if after.Spec.Actions[i].Name != "respond" {
			continue
		}
		b := after.Spec.Actions[i].Action.Executor.Parameters["zone"]
		if b.FromMember == nil || b.Value != nil {
			t.Fatalf("the author's spec was rewritten by materialisation: %+v", b)
		}
	}
}

// First and All are REFUSED rather than served as Latest. They ask different
// questions, and a wrong answer here is indistinguishable from a right one.
func TestFromMemberEvent_UnsupportedIterationIsRefusedNotApproximated(t *testing.T) {
	first := "First"
	member := consumerMember("respond", "responder", "zone", "watch", "IntruderDetected", "zone")
	member.Action.Executor.Parameters["zone"].FromMember.Iteration = &first

	task := flowTask("iter-task",
		&fleetv1.FlowNode{Sequence: []fleetv1.FlowNode{runNode("watch"), runNode("respond")}},
		watcherMember("watch", "intruder-detector"),
		member,
	)
	producer := watcherChild("iter-task", "watch",
		fleetv1.ActionEvent{Type: "IntruderDetected", Seq: 1, Payload: map[string]string{"zone": "dock"}},
	)
	producer.Status.Phase = fleetv1.ActionPhaseSucceeded

	r, c := newFleetTaskReconciler(t, task, producer)
	reconcileFT(t, r, "iter-task")

	started := getActionOrNil(t, c, childName("iter-task", "respond"))
	e := resolutionOf(t, started, "zone")
	if e.Resolved {
		t.Fatalf("iteration: First was served as Latest: %+v", e)
	}
	if !containsSub(e.Reason, "First") {
		t.Errorf("reason %q does not name the unsupported disambiguator", e.Reason)
	}
}

func boolPtr(b bool) *bool { return &b }
func containsSub(h, n string) bool {
	for k := 0; k+len(n) <= len(h); k++ {
		if h[k:k+len(n)] == n {
			return true
		}
	}
	return false
}
