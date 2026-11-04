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

package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Tier 1 parameter-provenance checks: the binding errors no runtime state can ever
// make true, so admitting them buys nothing.
//
// Two of the seven are implemented here, and they are the two this stage's rules
// name. The other five are recorded as not-yet-enforced rather than silently
// skipped, in this comment and in the item, because a check believed to run that
// does not is this repository's signature defect.
//
//   - S1 (the shape of it): a binding names something not in spec.actions, or a
//     `fromTaskInput` names an input not in spec.inputs. A member's binding is
//     fixed once the member exists (spec.actions is append-only and an existing
//     member is immutable), and spec.inputs and spec.flow are immutable after
//     create (CEL rules on FleetTask.spec), so the API server refuses an update
//     that removes an input out from under a binding that reads it. The check
//     still re-runs on any update bindingSpecChanged (fleettask_webhook.go)
//     reports, which on a served CRD means an appended member.
//   - S6: the binding kind CONTRADICTS the producer's contract — a result binding
//     onto a Service producer, or an event binding onto an App producer. The
//     author does NOT choose which form to write, the producer's declared
//     contract does, and admission says so with a message naming it.
//
// NOT ENFORCED HERE, and each needs something this webhook does not yet have:
// S2 (the field is not in the producer's declared outputs) and S3 (kind
// mismatch) need the producer's declared output set resolved through the robot
// SupportedAction catalog as well as the ComputeEndpoint; S4 (a required
// parameter with no binding and no default) needs the full parameter roster;
// S5 (a binding cycle) needs the binding graph; S7 (fromContext outside the
// closed set) is already enforced by the CRD's own enum marker on
// ParameterBinding.FromContext, which is why it is absent rather than missing.

const (
	// reasonUndeclaredTaskInput — a `fromTaskInput` binding names an input the
	// task does not declare. Rejected on create, and on any update that changes
	// spec.inputs, spec.actions or spec.flow (bindingSpecChanged).
	reasonUndeclaredTaskInput = "UndeclaredTaskInput"
	// reasonUnknownMember — a flow node or binding names a member not in
	// spec.actions.
	reasonUnknownMember = "UnknownMember"
	// reasonContractMismatch — check S6: the binding kind contradicts the
	// producer's declared contract.
	reasonContractMismatch = "ExternalContractMismatch"
	// reasonUndeclaredEvent — check S6b: an event binding (any of the three
	// forms producerRefs enumerates) names an event type the producer's
	// ComputeEndpoint does not declare in spec.emitsEvents.
	reasonUndeclaredEvent = "UndeclaredEvent"
)

// validateBindings runs the Tier 1 checks that need no client read: every
// `fromTaskInput` names a declared input, and every producer reference — a
// select subject, a loop.until condition, a `fromMember` binding — names a real
// member.
//
// Separate from validateFlow (internal/flow.ValidateStructure) because that
// function is deliberately pure over the flow tree and is ALSO called from the
// controller at reconcile; these checks span spec.inputs and spec.actions'
// executor blocks, which the flow tree does not contain.
func validateBindings(t *fleetv1.FleetTask) error {
	declared := make(map[string]bool, len(t.Spec.Inputs))
	for _, in := range t.Spec.Inputs {
		declared[in.Name] = true
	}
	members := make(map[string]bool, len(t.Spec.Actions))
	for _, a := range t.Spec.Actions {
		members[a.Name] = true
	}

	// Every executor, from the one enumeration producerRefs also uses. This loop
	// once walked spec.actions[].action.executor by hand while producerRefs
	// walked the compensation too, so an undeclared input on a compensation
	// was admitted.
	var errs field.ErrorList
	for _, te := range taskExecutors(t) {
		for _, name := range parameterNames(te.executor) {
			b := te.executor.Parameters[name]
			if b.FromTaskInput != nil && !declared[*b.FromTaskInput] {
				errs = append(errs, field.Invalid(te.path.Child("parameters").Key(name), *b.FromTaskInput,
					fmt.Sprintf("%s: no input %q is declared in spec.inputs (declared: %s)",
						reasonUndeclaredTaskInput, *b.FromTaskInput, namesOrNone(declared))))
			}
		}
	}

	// Every producer reference, from the one enumeration validateProducerContracts
	// also uses. The select subject was once the ONE reference in the flow tree
	// that was never resolved: a subject naming a member that does not exist
	// decoded to a producer the evaluator then waited on forever — an unbounded
	// InProgress with nothing to report. A loop.until member is also resolved by
	// validateFlow, which runs first and returns on it, so it is never reported
	// twice.
	for _, ref := range producerRefs(t) {
		if !members[ref.member] {
			errs = append(errs, field.Invalid(ref.path, ref.member,
				fmt.Sprintf("%s: no member %q is declared in spec.actions (members: %s)",
					reasonUnknownMember, ref.member, namesOrNone(members))))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(fleetv1.GroupVersion.WithKind("FleetTask").GroupKind(), t.Name, errs)
}

// validateProducerContracts is check S6, and its S6b source-exists refinement,
// for EVERY binding that reads a value a sibling member produces: a result
// binding is valid only for an App producer, an event binding only for a Service
// producer, and an event binding must name a type the producer declares.
//
// It runs over producerRefs, which is the one answer to "where can a FleetTask
// name a producer". It once ran over select subjects only, while
// loop.until and fromMember named event types the same way and were read at
// runtime the same way — and the until miss was the silent one.
//
// # WHY THE AUTHOR DOES NOT CHOOSE
//
// An App producer runs to completion and writes status.result ONCE, at the
// terminal transition, so a result binding onto it is safe — the value cannot
// change after it is read. A Service producer is resident and has no meaningful
// result until it ends; its observations are semantic events. A result binding
// onto a Service would read either nothing or something that keeps changing,
// which is the mutable-non-terminal-result problem this rule exists to prevent:
// a field whose meaning depends on where it is read is a field every consumer
// must treat as a sample rather than a fact. The producer's declared contract
// settles the question, so admission settles it.
//
// FAILURE DISCIPLINE, matching validateExternalRefs on the FleetAction side: a
// definitive NotFound on the referenced ComputeEndpoint is NOT an error here —
// that is FleetAction's G1 gate to report, on the member that carries the
// reference, and duplicating it would give one mistake two messages. Any other
// read error fails OPEN: an unreadable dependency must not block an otherwise
// valid write.
//
// RA-1: read-only. It writes no status and emits no event.
func (v *FleetTaskValidator) validateProducerContracts(ctx context.Context, t *fleetv1.FleetTask) error {
	if v.Reader == nil {
		return nil
	}
	refs := producerRefs(t)
	if len(refs) == 0 {
		return nil
	}
	byMember := make(map[string]*fleetv1.FleetTaskAction, len(t.Spec.Actions))
	for i := range t.Spec.Actions {
		byMember[t.Spec.Actions[i].Name] = &t.Spec.Actions[i]
	}

	// One read per endpoint, not one per reference: a loop whose until and
	// whose started action both bind to the same watcher ask it the same
	// question. A nil entry records "read, and not usable" (NotFound or any
	// other error), so a failing endpoint is not re-read per reference either.
	endpoints := make(map[string]*fleetv1.ComputeEndpoint)

	var errs field.ErrorList
	for _, ref := range refs {
		if ref.kind == readsOther {
			continue // no contract to contradict and no event to look up; S1 still ran
		}
		member := byMember[ref.member]
		if member == nil || member.Action.Executor == nil {
			// Not an external producer: a robot action has a result and raises
			// no events, so a result binding is the only form and there is
			// nothing for S6 to contradict. (An unknown member is reported by
			// validateBindings, once.)
			continue
		}
		name := member.Action.Executor.EndpointRef
		endpoint, seen := endpoints[name]
		if !seen {
			var ep fleetv1.ComputeEndpoint
			if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: name}, &ep); err == nil {
				endpoint = &ep
			}
			// NotFound is FleetAction's G1 to report; anything else fails open.
			endpoints[name] = endpoint
		}
		if endpoint == nil {
			continue
		}

		switch {
		case ref.kind == readsEvent && endpoint.Spec.Contract == fleetv1.ExecutorContractApp:
			errs = append(errs, field.Invalid(ref.path, ref.member, fmt.Sprintf(
				"%s: %s binds to an EVENT, but member %q's ComputeEndpoint %q declares "+
					"contract: App. An App producer runs to completion and raises no events — bind to "+
					"its result instead: %s",
				reasonContractMismatch, ref.form.name, ref.member, name, ref.form.resultHint(ref.member))))
		case ref.kind == readsResult && endpoint.Spec.Contract == fleetv1.ExecutorContractService:
			errs = append(errs, field.Invalid(ref.path, ref.member, fmt.Sprintf(
				"%s: %s binds to a RESULT, but member %q's ComputeEndpoint %q declares "+
					"contract: Service. A resident producer has no terminal result to read mid-run — "+
					"bind to one of the events it declares (%s) instead: %s",
				reasonContractMismatch, ref.form.name, ref.member, name,
				listOrNone(endpoint.Spec.EmitsEvents), ref.form.eventHint(ref.member))))

		// S6b — THE SOURCE-EXISTS CHECK. The endpoint is already fetched and
		// its declared event list is already printed in the message above; all
		// that was ever missing was asking whether the named type is IN it.
		//
		// FAIL-CLOSED ON A DEFINITE ANSWER, AND THAT IS A DECISION, NOT AN
		// INHERITANCE. The Get above fails OPEN on any read error and still
		// does: an unreadable dependency must not block an otherwise valid
		// write, and a blip is not evidence that the event is undeclared. But
		// once the endpoint HAS been read, its spec.emitsEvents is a definite
		// answer, and this check refuses on it — including when the list is
		// EMPTY, which says "this producer declares no events at all" and is a
		// different fix from choosing the wrong one.
		//
		// The two disciplines in this repository were weighed rather than
		// inherited by where the code sits. crossobject_admission_test.go's rule
		// — "a validator that admits on a read error turns an API-server blip
		// into an open gate" — governs the GATES, where admitting wrongly lets
		// something unsafe through. This is not a gate: a binding onto an event
		// no producer declares is a select that never branches, a loop that
		// runs to its bound, or a parameter that never resolves — a correctness
		// failure for its author and a safety failure for nobody. The cost of
		// failing closed on a read error would be refusing correct tasks during
		// an API-server hiccup; the cost of failing open is one of those three,
		// each of which the runtime reports late. Open on unknown, closed on
		// known, is therefore the right split here — and the one place it would
		// be wrong is if this check were ever reused to gate dispatch.
		case ref.kind == readsEvent && !containsString(endpoint.Spec.EmitsEvents, ref.eventType):
			errs = append(errs, field.Invalid(ref.eventPath, ref.eventType, fmt.Sprintf(
				"%s: %s names event type %q, but member %q's ComputeEndpoint %q does not declare "+
					"it in spec.emitsEvents (declared: %s). %s: either name a declared type, or "+
					"add this one to the endpoint's spec.emitsEvents",
				reasonUndeclaredEvent, ref.form.name, ref.eventType, ref.member, name,
				listOrNone(endpoint.Spec.EmitsEvents), ref.form.neverRaised)))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(fleetv1.GroupVersion.WithKind("FleetTask").GroupKind(), t.Name, errs)
}

// producerForm is one of the places a FleetTask can name a producing member,
// with what an admission message needs to say about it. A struct rather than a
// name looked up in a table, so a form cannot exist without its messages.
type producerForm struct {
	// name is how the message refers to the binding the author wrote.
	name string
	// resultHint and eventHint spell the correct form of THIS binding, so the
	// fix an S6 rejection offers is one the author can paste where they are.
	resultHint func(member string) string
	eventHint  func(member string) string
	// neverRaised is what an event binding onto an undeclared type does at
	// runtime — which differs per form, and the until case is the silent one.
	neverRaised string
}

// The three producer forms. allProducerForms is what the guard test in
// fleettask_bindings_test.go holds the type graph and the fixtures against: a
// fourth form is added here, to producerRefs, and to that test, or the test
// fails.
var (
	formSelectSubject = &producerForm{
		name:       "select.subject",
		resultHint: func(m string) string { return fmt.Sprintf("subject: {member: %s, field: ...}", m) },
		eventHint:  func(m string) string { return fmt.Sprintf("subject: {event: <type>, from: %s, field: ...}", m) },
		neverRaised: "A select bound to an event the producer never raises is a node that " +
			"waits forever",
	}
	formLoopUntil = &producerForm{
		name:       "loop.until",
		resultHint: func(m string) string { return fmt.Sprintf("result: {member: %s, field: ..., equals: ...}", m) },
		eventHint:  func(m string) string { return fmt.Sprintf("event: {type: <type>, from: %s}", m) },
		neverRaised: "An until condition on an event the producer never raises is never " +
			"satisfied, and an unsatisfied until is not an error — the loop silently runs to its " +
			"maxIterations/maxDurationSeconds bound instead of to the event",
	}
	formFromMember = &producerForm{
		name:       "fromMember",
		resultHint: func(m string) string { return fmt.Sprintf("fromMember: {name: %s, result: ...}", m) },
		eventHint:  func(m string) string { return fmt.Sprintf("fromMember: {name: %s, event: <type>, field: ...}", m) },
		neverRaised: "A parameter bound to an event the producer never raises never " +
			"resolves: it is recorded resolved: false in the started action's " +
			"status.parameterResolution, on a different object from the one you wrote",
	}
	allProducerForms = []*producerForm{formSelectSubject, formLoopUntil, formFromMember}
)

// bindingKind is what a producer reference reads from its producer.
type bindingKind int

const (
	// readsResult — the producer's terminal status.result (App contract).
	readsResult bindingKind = iota
	// readsEvent — a semantic event the producer raises (Service contract).
	readsEvent
	// readsOther — neither: a fromMember naming only the member, or its
	// artifact arm. S6 has nothing to contradict; the member must still exist.
	readsOther
)

// producerRef is one resolved reference from a binding to the member that
// produces its value: which form it is, what it reads, and where it sits.
type producerRef struct {
	form   *producerForm
	member string
	kind   bindingKind
	// eventType is the named event type, non-empty exactly when kind is
	// readsEvent. Carried rather than re-walked, because the membership check
	// needs the NAME and not only the fact that an event arm was used.
	eventType string
	// path points at the field naming the producer; eventPath at the field
	// naming the event type, where the membership rejection is reported
	// because it is ABOUT the event name. eventPath is nil unless readsEvent.
	path      *field.Path
	eventPath *field.Path
	// onCompensation is set for a fromMember binding on a member's
	// compensation executor, which the runtime creates from the spec as written
	// and never resolves (see resolvedAtRuntime).
	onCompensation bool
}

// taskExecutor is one ActionExecutor a FleetTask carries, with the field path of
// the executor itself.
type taskExecutor struct {
	executor       *fleetv1.ActionExecutor
	path           *field.Path
	onCompensation bool
}

// taskExecutors returns every ActionExecutor in t: each member's action
// executor and each member's compensation executor, in spec.actions order.
//
// THE ONE ANSWER TO "WHERE DOES A FLEETTASK CARRY PARAMETERS". A compensation is
// a whole FleetActionSpec, so the Go type lets it carry an executor with its own
// parameters. CEL on FleetTaskSpec refuses a compensation executor before this
// webhook runs; the route is walked anyway because the webhook does not assume
// the schema ran first: an object stored before the rule, a CRD installed without
// it, or a caller invoking the validator directly all reach this code. producerRefs
// and validateBindings' fromTaskInput loop both ask here, so neither can drop it.
// TestBindingRoutes_* derives the routes from the Go types and builds a binding
// on each, so a third executor on a member fails a test until it is added here.
func taskExecutors(t *fleetv1.FleetTask) []taskExecutor {
	var out []taskExecutor
	for i := range t.Spec.Actions {
		a := &t.Spec.Actions[i]
		ap := field.NewPath("spec", "actions").Index(i)
		if a.Action.Executor != nil {
			out = append(out, taskExecutor{executor: a.Action.Executor, path: ap.Child("action", "executor")})
		}
		if a.Compensation != nil && a.Compensation.Executor != nil {
			out = append(out, taskExecutor{executor: a.Compensation.Executor,
				path: ap.Child("compensation", "executor"), onCompensation: true})
		}
	}
	return out
}

// parameterNames returns an executor's parameter names sorted: a map's
// iteration order is random, and an admission message that names a different
// offending parameter on each retry is one an author cannot act on.
func parameterNames(ex *fleetv1.ActionExecutor) []string {
	names := make([]string, 0, len(ex.Parameters))
	for name := range ex.Parameters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// producerRefs returns every reference in t to a value a sibling member
// produces: each select subject and each loop.until condition in spec.flow, and
// each `fromMember` parameter binding on every executor taskExecutors returns.
//
// ONE ENUMERATION, BECAUSE THE DEFECT IT REPLACES WAS A SECOND ONE. The
// source-exists check was added over a walk that visited select subjects and
// nothing else, so the two other forms that name an event type were unchecked
// by construction, not by oversight at the check. Every caller that asks "which
// member does this binding read, and what" asks here, and a new form is added
// here once rather than at each check that should have covered it.
func producerRefs(t *fleetv1.FleetTask) []producerRef {
	var out []producerRef
	for _, te := range taskExecutors(t) {
		out = append(out, parameterProducerRefs(te)...)
	}
	return append(out, flowProducerRefs(t.Spec.Flow, field.NewPath("spec", "flow"))...)
}

// parameterProducerRefs returns the fromMember bindings of one executor's
// parameters, in parameter-name order.
func parameterProducerRefs(te taskExecutor) []producerRef {
	base := te.path.Child("parameters")
	var out []producerRef
	for _, name := range parameterNames(te.executor) {
		b := te.executor.Parameters[name].FromMember
		if b == nil {
			continue
		}
		fp := base.Key(name).Child("fromMember")
		ref := producerRef{form: formFromMember, member: b.Name, kind: readsOther, path: fp.Child("name"),
			onCompensation: te.onCompensation}
		switch {
		case b.Event != nil:
			ref.kind, ref.eventType, ref.eventPath = readsEvent, *b.Event, fp.Child("event")
		case b.Result != nil:
			ref.kind = readsResult
		}
		out = append(out, ref)
	}
	return out
}

// flowProducerRefs is producerRefs' walk of the flow tree. A walk rather than a
// visitor because several callers want the same list for different questions,
// and two walks would eventually disagree about which nodes a reference can
// hide under — which is exactly how the emitsEvents check once came to cover
// select subjects and nothing else.
func flowProducerRefs(node *fleetv1.FlowNode, path *field.Path) []producerRef {
	if node == nil {
		return nil
	}
	var out []producerRef
	for i := range node.Sequence {
		out = append(out, flowProducerRefs(&node.Sequence[i], path.Child("sequence").Index(i))...)
	}
	if node.Parallel != nil {
		for i := range node.Parallel.Branches {
			out = append(out, flowProducerRefs(&node.Parallel.Branches[i],
				path.Child("parallel", "branches").Index(i))...)
		}
	}
	if node.Loop != nil {
		if node.Loop.Until != nil {
			for i := range node.Loop.Until.AnyOf {
				c := &node.Loop.Until.AnyOf[i]
				cp := path.Child("loop", "until", "anyOf").Index(i)
				switch {
				case c.Event != nil:
					ep := cp.Child("event")
					out = append(out, producerRef{
						form: formLoopUntil, member: c.Event.From, kind: readsEvent, eventType: c.Event.Type,
						path: ep.Child("from"), eventPath: ep.Child("type"),
					})
				case c.Result != nil:
					out = append(out, producerRef{
						form: formLoopUntil, member: c.Result.Member, kind: readsResult,
						path: cp.Child("result", "member"),
					})
				}
			}
		}
		out = append(out, flowProducerRefs(&node.Loop.Body, path.Child("loop", "body"))...)
	}
	if node.Select != nil {
		sp := path.Child("select", "subject")
		s := node.Select.Subject
		ref := producerRef{form: formSelectSubject, member: s.Producer(), kind: readsResult, path: sp.Child("member")}
		if s.Event != nil {
			ref.kind, ref.eventType, ref.path, ref.eventPath = readsEvent, *s.Event, sp.Child("from"), sp.Child("event")
		}
		out = append(out, ref)
		for i := range node.Select.Cases {
			out = append(out, flowProducerRefs(&node.Select.Cases[i].Then,
				path.Child("select", "cases").Index(i).Child("then"))...)
		}
		out = append(out, flowProducerRefs(&node.Select.Default, path.Child("select", "default"))...)
	}
	return out
}

// namesOrNone renders a declaration set for an error message, sorted, or the
// word "none" — which is the case an author most needs told apart from a typo.
func namesOrNone(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// containsString reports membership in a declared list. A linear scan: these
// lists are a handful of entries declared by an administrator, and a set built
// per subject would cost more than it saved.
func containsString(list []string, want string) bool {
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

// listOrNone renders a declared list for an error message, or "none declared" —
// which tells an author their Service producer declares no events at all, a
// different fix from choosing the wrong one.
func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none declared"
	}
	return strings.Join(items, ", ")
}

// resolvedAtRuntime reports whether the runtime ever reads ref's binding.
//
// The select subject and loop.until are read by the flow evaluator
// (internal/flow). A fromMember binding is resolved by resolveMemberBindings
// (internal/controller/fleettask_params.go), which is called for a child a FLOW
// starts (fleettask_flow.go) and nowhere else. A dependsOn child is created from
// a.Action as written (fleettask_controller.go, the eligible-children loop) and a
// compensation child from *a.Compensation as written (the Compensate path), so a
// fromMember binding in a task with no spec.flow, or on any compensation, is
// admitted, checked, and never resolved.
//
// This is a statement about the controller as it is, not about what the binding
// should mean. When the controller resolves those routes, this predicate is
// what changes, and TestBindingWarnings_UnresolvedFromMemberIsToldSo with it.
func resolvedAtRuntime(t *fleetv1.FleetTask, ref producerRef) bool {
	if ref.form != formFromMember {
		return true
	}
	return !ref.onCompensation && t.Spec.Flow != nil
}

// refPath is where a warning names ref: the field naming the event type for an
// event binding, since the event is what the warning is about, else the field
// naming the producer.
func refPath(ref producerRef) string {
	if ref.eventPath != nil {
		return ref.eventPath.String()
	}
	return ref.path.String()
}

// unevaluableEventBindingWarnings tells an author, at write time, about the
// bindings and flow constructs whose admitted shape the runtime cannot fully
// honour.
//
// THE EVENT ARM IS NO LONGER ONE OF THEM. FleetAction carries status.events[]
// and internal/flow reads it from both the select subject and loop.until. At
// v0.4 no shipped component fills it (ComputeEventIngestor, the StreamStatus
// consumer, is not started by the manager — see FleetActionStatus.Events), so an
// event binding sees no event at all. The warning that used to say "not
// evaluable, settles Failed naming the missing channel" would now be false, and
// a warning an author has learned to ignore because it was wrong once is worse
// than no warning at all. What replaces it is not silence: status.events[] is a
// BOUNDED WINDOW, so an event binding is warned about the one way it can still
// fail with nothing else to read — the producer outrunning the consumer — which
// is a property of the retention bound and not of a missing channel.
//
// EVERY EVENT BINDING THE RUNTIME READS, FROM producerRefs. This warning once
// listed select subjects only and ended "the same bound applies to loop.until's
// event conditions" — printed only when a select was present, so a task whose
// only event binding was an until got nothing, and an evicted until event is the
// quietest failure of the three: an unsatisfied condition, and a loop that runs
// to its bound. The consequence sentences come from each form's
// neverRaised, because an evicted event is, to its reader, one never raised.
//
// A fromMember binding the runtime never resolves (resolvedAtRuntime) is not
// told about a window it is never read through. It is told it is not resolved,
// of any kind: a result or artifact binding there is equally unread.
//
// `when` is unchanged and still unevaluable: no CEL environment is wired.
func unevaluableEventBindingWarnings(t *fleetv1.FleetTask) []string {
	var evicting, unresolved []string
	var consequences []string
	seenForm := map[*producerForm]bool{}
	for _, ref := range producerRefs(t) {
		switch {
		case !resolvedAtRuntime(t, ref):
			unresolved = append(unresolved, refPath(ref))
		case ref.kind == readsEvent:
			evicting = append(evicting, refPath(ref))
			if !seenForm[ref.form] {
				seenForm[ref.form] = true
				consequences = append(consequences, ref.form.neverRaised)
			}
		}
	}
	whens := selectWhenCases(t.Spec.Flow, field.NewPath("spec", "flow"))

	var out []string
	if len(evicting) > 0 {
		out = append(out, fmt.Sprintf(
			"%s: bound to a semantic event, read from the producing member's status.events[]. "+
				"That list is a BOUNDED WINDOW of the most recent events, not a log: if the producer "+
				"raises enough further events between two reconciles, the one a binding is waiting "+
				"for is evicted and will never be seen — StreamStatus.since_seq resumes from the "+
				"high-water mark and never replays it. status.eventsDropped on the producer's child "+
				"action reports when that has happened. An evicted event reads as one never raised. %s",
			strings.Join(evicting, ", "), strings.Join(consequences, ". ")))
	}
	if len(unresolved) > 0 {
		out = append(out, fmt.Sprintf(
			"%s: this fromMember binding is checked at admission but NOT RESOLVED at runtime. "+
				"fromMember is resolved only on a member a spec.flow starts; a member of a task "+
				"without spec.flow, and every compensation, is created from its spec as written, "+
				"so the binding is never replaced by a value and no status.parameterResolution "+
				"entry records it",
			strings.Join(unresolved, ", ")))
	}
	if len(whens) > 0 {
		out = append(out, fmt.Sprintf(
			"%s: select.cases[].when is a CEL clause and is NOT EVALUABLE at v0.4 — no CEL "+
				"environment is wired into the flow evaluator. Such a node settles Failed; it does "+
				"NOT fall through to the next case, because falling through would run a branch the "+
				"`when` case was written to pre-empt and nothing would say so",
			strings.Join(whens, ", ")))
	}
	return out
}

// selectWhenCases returns the field path of every select case carrying a `when`
// clause. Separate from flowProducerRefs because they answer different
// questions about the same walk, and one function returning two unrelated lists
// is a function whose callers each ignore half of it.
func selectWhenCases(node *fleetv1.FlowNode, path *field.Path) []string {
	if node == nil {
		return nil
	}
	var out []string
	for i := range node.Sequence {
		out = append(out, selectWhenCases(&node.Sequence[i], path.Child("sequence").Index(i))...)
	}
	if node.Parallel != nil {
		for i := range node.Parallel.Branches {
			out = append(out, selectWhenCases(&node.Parallel.Branches[i],
				path.Child("parallel", "branches").Index(i))...)
		}
	}
	if node.Loop != nil {
		out = append(out, selectWhenCases(&node.Loop.Body, path.Child("loop", "body"))...)
	}
	if node.Select != nil {
		for i := range node.Select.Cases {
			cp := path.Child("select", "cases").Index(i)
			if node.Select.Cases[i].When != nil {
				out = append(out, cp.Child("when").String())
			}
			out = append(out, selectWhenCases(&node.Select.Cases[i].Then, cp.Child("then"))...)
		}
		out = append(out, selectWhenCases(&node.Select.Default, path.Child("select", "default"))...)
	}
	return out
}
