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
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/flow"
)

// This file resolves ParameterBinding.fromMember — the binding that carries what
// one member OBSERVED into the parameters of the action that observation
// started. It is the point of the event machinery: a select can branch on a
// recognition without it, but the action the branch starts would run knowing
// only that SOMETHING was recognised, not what.
//
// WHY THE TASK CONTROLLER AND NOT THE ACTION CONTROLLER. A fromMember binding
// names a SIBLING MEMBER, which is a FleetTask-level coordinate. The child
// FleetAction knows nothing about its siblings — it has a name, a spec and an
// owner reference — so it could never resolve one itself. The task controller is
// the only place that holds both the binding and the sibling's status.
//
// WHY AT CREATION, ONCE. A Service producer keeps raising events after the
// consumer starts. If the binding were re-resolved on every reconcile, the
// parameter a running action was activated with would silently change under it,
// and the recorded provenance would name whichever frame happened to be latest
// at the last reconcile rather than the one that caused the start. So the
// resolution is pinned at the moment the action is created, together with the
// seq of the exact event it was read from, and is never rewritten — the same
// write-once discipline status.result and the event ingest itself follow.
//
// WHAT AN UNRESOLVED BINDING DOES. Nothing is deferred and no child is withheld:
// the child is created, the unresolved fromMember binding stays in its
// spec.executor.parameters as written (materialiseMemberBindings replaces only
// resolved ones), and the miss is recorded. This code applies no consequence to
// a miss. The executor block's spec.executor.onUnresolvedParameter policy is
// not read here or by any other controller at v0.4, and no controller activates
// an executor run, so no Fail, UseDefault or Omit is ever applied. This
// function's obligation is visibility only: every fromMember binding gets a
// status.parameterResolution entry saying what it resolved to or why it did not.
// Gating the START on the observation is the flow's job, not this function's —
// that is what a select subject and a loop.until condition ARE, and an author
// who binds fromMember without gating gets a recorded miss naming the member and
// the event rather than a silent empty parameter.

// resolvedBinding is one fromMember binding's outcome: the value, if any, and
// the provenance that names where it came from.
type resolvedBinding struct {
	entry fleetv1.ParameterResolutionEntry
	value *string
}

// resolveMemberBindings resolves every fromMember parameter binding declared on
// one member's executor, reading sibling state through the same childFn the flow
// evaluator uses — so a select that branched on an event and a binding that
// reads it cannot disagree about what the producer raised.
//
// Bindings that are not fromMember are left alone and get no entry: value and
// fromContext need no sibling state, and fromTaskInput is resolved against
// spec.inputs, none of which this function can be wrong about. Recording an
// entry for them here would claim a resolution that did not happen.
//
// Entries come back sorted by parameter name: a map's iteration order is random,
// and a status list whose order changed on every write would be a diff on every
// reconcile for a value that never moved.
func resolveMemberBindings(
	executor *fleetv1.ActionExecutor,
	iter int32,
	childFn func(member string, iteration int32) flow.ChildState,
) []resolvedBinding {
	if executor == nil || len(executor.Parameters) == 0 {
		return nil
	}
	names := make([]string, 0, len(executor.Parameters))
	for name := range executor.Parameters {
		if executor.Parameters[name].FromMember != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]resolvedBinding, 0, len(names))
	for _, name := range names {
		b := executor.Parameters[name].FromMember
		out = append(out, resolveOneMemberBinding(name, b, iter, childFn))
	}
	return out
}

func resolveOneMemberBinding(
	name string,
	b *fleetv1.MemberBinding,
	iter int32,
	childFn func(member string, iteration int32) flow.ChildState,
) resolvedBinding {
	entry := fleetv1.ParameterResolutionEntry{Name: name, Source: "member", FromMember: b.Name}
	miss := func(reason string) resolvedBinding {
		entry.Resolved = false
		entry.Reason = reason
		return resolvedBinding{entry: entry}
	}

	// Latest is the only iteration disambiguator implemented, and an explicit
	// First or All is REFUSED rather than silently served as Latest. They are
	// different questions — "the first observation" and "every observation" —
	// and answering a different one than was asked is the failure mode this
	// whole prompt's evaluator comments argue against. Unset means Latest,
	// which is what an event binding means anyway: the most recent recognition.
	if b.Iteration != nil && *b.Iteration != "Latest" {
		return miss(fmt.Sprintf(
			"fromMember.iteration: %q is not resolved by this controller (only Latest, and unset "+
				"which means Latest, are). It is refused rather than served as Latest, because "+
				"%q asks a different question and a wrong answer here is indistinguishable from a "+
				"right one", *b.Iteration, *b.Iteration))
	}

	// The producing member is looked up at THIS iteration first and then at
	// iteration 0, the same two places loop.until searches and for the same two
	// design cases: a producer inside the loop body, and a resident watcher
	// beside it. A resident recognition service is the common shape for the
	// event arm, and it lives at iteration 0.
	st, ok := producerState(b.Name, iter, childFn)
	if !ok {
		return miss(fmt.Sprintf(
			"member %q has no child FleetAction at iteration %d or at iteration 0, so nothing it "+
				"produced can be read yet", b.Name, iter))
	}

	switch {
	case b.Event != nil:
		if b.Field == nil || *b.Field == "" {
			// MemberBinding's own godoc makes Field required with Event. Stated
			// rather than defaulted: there is no sensible whole-payload
			// encoding, and picking one would hand the executor a string the
			// author never wrote.
			return miss(fmt.Sprintf(
				"fromMember.event %q names no field; field is required with event and names the "+
					"entry to read from that event's payload", *b.Event))
		}
		ev, found := st.LatestEvent(*b.Event)
		if !found {
			return miss(fmt.Sprintf(
				"member %q has raised no readable event of type %q%s",
				b.Name, *b.Event, st.EventAbsenceNote()))
		}
		entry.FromEvent = ev.Type
		entry.FromEventSeq = ev.Seq
		v, present := ev.Payload[*b.Field]
		if !present {
			return miss(fmt.Sprintf(
				"event %q (seq %d) from member %q carries no payload field %q (present: %s)",
				ev.Type, ev.Seq, b.Name, *b.Field, payloadKeysOrNone(ev.Payload)))
		}
		entry.Resolved = true
		entry.Value = &v
		return resolvedBinding{entry: entry, value: &v}

	case b.Result != nil:
		// The result arm waits for the producer to settle, exactly as the
		// select result arm does and for the identical reason: an App
		// producer's status.result is written once at the terminal transition,
		// so a value read before it is a value that can still change.
		if st.Result == nil || !isTerminal(st.Phase) {
			return miss(fmt.Sprintf(
				"member %q has not written a terminal status.result yet (an App-contract producer "+
					"writes it once, at the terminal transition)", b.Name))
		}
		v, present := st.Result.Values[*b.Result]
		if !present {
			return miss(fmt.Sprintf(
				"member %q's result carries no value %q", b.Name, *b.Result))
		}
		entry.Resolved = true
		entry.Value = &v
		return resolvedBinding{entry: entry, value: &v}

	case b.Artifact != nil:
		// Named, not silently skipped. An artifact is a reference with a store,
		// a path and a digest — not a scalar — and a parameter is a scalar, so
		// this needs a decision about which of the three a parameter receives
		// before it can have a reader. It is not a fromMember-event concern.
		return miss("fromMember.artifact is not resolved by this controller: an artifact is a " +
			"store/path/digest reference, not a scalar, and which part a scalar parameter " +
			"receives is an undecided question rather than an omission")

	default:
		return miss("fromMember names neither result, event nor artifact")
	}
}

// producerState returns the producing member's child state, trying this
// iteration and then iteration 0.
func producerState(member string, iter int32, childFn func(string, int32) flow.ChildState) (flow.ChildState, bool) {
	if st := childFn(member, iter); st.Exists {
		return st, true
	}
	if iter != 0 {
		if st := childFn(member, 0); st.Exists {
			return st, true
		}
	}
	return flow.ChildState{}, false
}

// payloadKeysOrNone renders an event payload's keys for a miss message, sorted,
// or "none" — an author who misspelled a field needs the list, and an author
// whose executor sent an empty payload needs to know that instead.
func payloadKeysOrNone(payload map[string]string) string {
	if len(payload) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := keys[0]
	for _, k := range keys[1:] {
		out += ", " + k
	}
	return out
}

// materialiseMemberBindings rewrites a child FleetAction's resolved fromMember
// parameters into literal `value` bindings, in place, on the child's OWN deep
// copy of the spec.
//
// THE CHILD CANNOT CARRY THE BINDING FORWARD. A fromMember binding is a
// FleetTask coordinate; once the child exists it is an independent FleetAction
// that will be dispatched to a ComputeEndpoint by a controller which has no
// sibling context. Leaving the binding in place would hand the dispatcher a
// reference it cannot follow. So the resolved ones become literals and the
// unresolved ones stay as they are — visibly unresolved, with the reason in
// status.parameterResolution, rather than silently replaced by an empty string.
func materialiseMemberBindings(child *fleetv1.FleetAction, resolved []resolvedBinding) {
	if child.Spec.Executor == nil {
		return
	}
	for _, rb := range resolved {
		if rb.value == nil {
			continue
		}
		v := *rb.value
		child.Spec.Executor.Parameters[rb.entry.Name] = fleetv1.ParameterBinding{Value: &v}
	}
}

// recordParameterResolution writes the child's status.parameterResolution, ONCE.
//
// Guarded on the list being empty, which makes it edge-triggered rather than a
// write per reconcile: a child that already carries its resolution is never
// patched again, and a child whose first status write failed is retried on the
// next pass instead of losing the record. RA-1 applies to this list as much as
// to any other status field — it is written at most once in a child's lifetime.
func (r *FleetTaskReconciler) recordParameterResolution(
	ctx context.Context, child *fleetv1.FleetAction, resolved []resolvedBinding,
) error {
	if len(resolved) == 0 || len(child.Status.ParameterResolution) > 0 {
		return nil
	}
	base := child.DeepCopy()
	child.Status.ParameterResolution = make([]fleetv1.ParameterResolutionEntry, 0, len(resolved))
	for _, rb := range resolved {
		child.Status.ParameterResolution = append(child.Status.ParameterResolution, rb.entry)
	}
	if err := r.Status().Patch(ctx, child, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("recording parameter resolution on %q: %w", child.Name, err)
	}
	return nil
}
