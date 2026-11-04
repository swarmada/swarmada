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

package v1

// FlowNode is one node of a FleetTaskSpec.flow tree. Exactly one
// of Run, Sequence, Parallel, Loop, Select is set, OR none are — the empty
// node `{}`, an explicit no-op that settles Succeeded immediately. This is
// how `select.default` stays REQUIRED without becoming an
// optional field: an author who has nothing further to do writes `{}`.
//
// "Exactly one of run/sequence/parallel/loop/select" is NOT CEL-enforced here:
// `loop` is a CEL RESERVED WORD (§ CEL's reserved-identifiers list includes
// as/break/const/continue/else/for/function/if/import/let/loop/package/
// namespace/return/var/void/while alongside true/false/null/in) — `self.loop`
// fails type-checking with "undefined field" even though it parses, and
// FlowNode.Sequence is itself Schemaless (see below), which has() also cannot
// examine. Both facts, plus the general depth-1 recursion limit (same
// comment), push ALL flow-node-shape validation to the validating webhook,
// which walks the decoded Go tree — where "loop" is an ordinary struct field
// and recursion is ordinary Go recursion, neither of which CEL's restrictions
// touch.
//
// Because the schema sees nothing below depth 1, internal/flow's
// ValidateStructure repeats EVERY constraint declared here — the enums, the
// required LoopNode.Mode, the conditional requirements and the numeric minima
// and maxima — at every depth, and additionally bounds the tree at
// flow.MaxNodes nodes and flow.MaxDepth levels. Only the DEFAULTS
// below stop at depth 1; the evaluator substitutes the identical values for
// the Go zero value, so an absent value stays admissible at any depth while a
// present, illegal one does not.
type FlowNode struct {
	// Run executes one member of spec.actions by name — the leaf.
	// +optional
	Run *string `json:"run,omitempty"`

	// Sequence runs each node in order; the next starts when the previous
	// settles Succeeded. Any non-Succeeded settlement short-circuits the
	// remainder (they were never started, so nothing to cancel) and the
	// sequence settles with that outcome.
	//
	// Schemaless: CRD structural schemas cannot express true recursion (no
	// $ref survives structural-schema flattening), so a genuinely
	// self-referential type like FlowNode can only be schema-validated one
	// level deep. The validating webhook performs the full recursive
	// structural validation (every rule this file's CEL markers state,
	// applied at every depth) that the schema cannot, and bounds the tree's
	// total node count and nesting depth — nothing else does.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Sequence []FlowNode `json:"sequence,omitempty"`

	// Parallel runs concurrent branches with an explicit join.
	// +optional
	Parallel *ParallelNode `json:"parallel,omitempty"`

	// Loop repeats Body.
	// +optional
	Loop *LoopNode `json:"loop,omitempty"`

	// Select branches on a value a previous member produced.
	// +optional
	Select *SelectNode `json:"select,omitempty"`
}

// JoinMode selects how a ParallelNode settles from its branches' outcomes.
// +kubebuilder:validation:Enum=All;Any;Quorum;First
type JoinMode string

// JoinMode values.
const (
	// JoinModeAll — every branch must settle Succeeded (default).
	JoinModeAll JoinMode = "All"
	// JoinModeAny — the node succeeds on the FIRST branch to settle Succeeded;
	// fails only if every branch settles non-Succeeded.
	JoinModeAny JoinMode = "Any"
	// JoinModeQuorum — at least Quorum branches must settle Succeeded.
	JoinModeQuorum JoinMode = "Quorum"
	// JoinModeFirst — the node takes the outcome of whichever branch settles
	// first, succeeded or not. Distinct from Any: Any waits for a success,
	// First takes whatever comes first.
	JoinModeFirst JoinMode = "First"
)

// JoinSpec configures a ParallelNode's join.
//
// +kubebuilder:validation:XValidation:rule="self.mode != 'Quorum' || has(self.quorum)",message="quorum is required when mode is Quorum"
type JoinSpec struct {
	// Mode selects the join semantics.
	// +kubebuilder:default=All
	// +optional
	Mode JoinMode `json:"mode,omitempty"`

	// Quorum is REQUIRED when Mode is Quorum, ignored otherwise (CEL).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Quorum *int32 `json:"quorum,omitempty"`
}

// OnJoinPolicy selects what happens to still-running branches once the join
// is satisfied.
// +kubebuilder:validation:Enum=CancelSiblings;LetRunToCompletion
type OnJoinPolicy string

// OnJoinPolicy values.
const (
	// OnJoinCancelSiblings — confirmed-cancel every still-running branch once
	// the join is satisfied (default). Goes through the SAME confirmed-cancel
	// path as an operator cancel (CancelAction / CancelDisposition) — never an
	// abandon. The node reaches its terminal phase only once EVERY branch has
	// settled; join satisfaction merely INITIATES cancellation of the rest.
	OnJoinCancelSiblings OnJoinPolicy = "CancelSiblings"
	// OnJoinLetRunToCompletion — still-running branches are left alone.
	OnJoinLetRunToCompletion OnJoinPolicy = "LetRunToCompletion"
)

// OnBranchFailurePolicy selects a ParallelNode's response to one branch
// settling Failed.
// +kubebuilder:validation:Enum=FailNode;Ignore
type OnBranchFailurePolicy string

// OnBranchFailurePolicy values.
const (
	// OnBranchFailureFailNode — a branch Failed counts against the join
	// (default).
	OnBranchFailureFailNode OnBranchFailurePolicy = "FailNode"
	// OnBranchFailureIgnore — a branch Failed is treated as though it simply
	// did not succeed, without failing the node outright (still subject to
	// the join mode's own arithmetic).
	OnBranchFailureIgnore OnBranchFailurePolicy = "Ignore"
)

// ParallelNode runs concurrent branches with an explicit join.
type ParallelNode struct {
	// Branches are the concurrent nodes. At least one is required — enforced
	// by the validating webhook (Schemaless; see FlowNode.Sequence).
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Branches []FlowNode `json:"branches"`

	// Join configures the join semantics. Unset defaults to mode: All.
	// +optional
	Join *JoinSpec `json:"join,omitempty"`

	// OnJoin selects what happens to still-running branches once the join is
	// satisfied.
	// +kubebuilder:default=CancelSiblings
	// +optional
	OnJoin OnJoinPolicy `json:"onJoin,omitempty"`

	// OnBranchFailure selects the node's response to one branch failing.
	// +kubebuilder:default=FailNode
	// +optional
	OnBranchFailure OnBranchFailurePolicy `json:"onBranchFailure,omitempty"`
}

// LoopMode selects a LoopNode's repetition bound.
// +kubebuilder:validation:Enum=Once;Times;Forever
type LoopMode string

// LoopMode values.
const (
	LoopModeOnce    LoopMode = "Once"
	LoopModeTimes   LoopMode = "Times"
	LoopModeForever LoopMode = "Forever"
)

// LoopNode repeats Body.
//
// A Forever loop MUST carry a bound (until, maxIterations, or
// maxDurationSeconds) — NOT a convenience check: an unbounded loop is a robot
// that never stops, a stream that never powers down, and a status object that
// grows until the API server rejects writes to it.
//
// +kubebuilder:validation:XValidation:rule="self.mode != 'Times' || has(self.times)",message="times is required when mode is Times"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Forever' || has(self.until) || has(self.maxIterations) || has(self.maxDurationSeconds)",message="mode: Forever requires until, maxIterations or maxDurationSeconds — an unbounded loop is not admitted"
type LoopNode struct {
	// Mode selects the repetition bound. REQUIRED, and required at every
	// depth: the schema cannot see a nested loop, and an unset Mode skips the
	// Forever bound check below (which keys on the literal value) while the
	// evaluator treats it as Forever — an unbounded loop admitted by the very
	// rule that forbids it. The structural validator
	// refuses an unset or unenumerated Mode wherever it appears.
	Mode LoopMode `json:"mode"`

	// Times is the iteration count, REQUIRED iff Mode is Times (CEL).
	// +optional
	// +kubebuilder:validation:Minimum=1
	Times *int32 `json:"times,omitempty"`

	// Body is the node repeated on each iteration (Schemaless; see
	// FlowNode.Sequence).
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Body FlowNode `json:"body"`

	// Until is the exit condition, evaluated between iterations (or mid-
	// iteration when InterruptRunning is true). At least one of Until,
	// MaxIterations, MaxDurationSeconds is required when Mode is Forever
	// (CEL).
	// +optional
	Until *UntilSpec `json:"until,omitempty"`

	// InterruptRunning, when true, confirmed-cancels the in-flight child on an
	// Until match rather than waiting for it to settle between iterations.
	// Defaults to false — the conservative choice, since interrupting
	// mid-execution costs a full cancel-to-confirmed-rest cycle and an author
	// should opt into paying it.
	// +optional
	InterruptRunning bool `json:"interruptRunning,omitempty"`

	// MaxIterations bounds a Forever loop by iteration count.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxIterations *int32 `json:"maxIterations,omitempty"`

	// MaxDurationSeconds bounds a Forever loop by wall-clock duration since
	// the loop's first iteration started.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxDurationSeconds *int32 `json:"maxDurationSeconds,omitempty"`

	// Retention bounds this loop's status and child-object growth.
	// Unset applies the built-in defaults (keepIterationRecords: 10,
	// keepChildren: 20, keepFailedChildren: 5) — the bound ships with the loop
	// construct itself, not as an operator afterthought.
	// +optional
	Retention *FlowRetention `json:"retention,omitempty"`
}

// UntilSpec is a loop's exit condition: satisfied when ANY listed condition
// matches (an `anyOf`).
type UntilSpec struct {
	// AnyOf is the list of conditions; the loop exits when any one matches.
	// +kubebuilder:validation:MinItems=1
	AnyOf []UntilCondition `json:"anyOf"`
}

// UntilCondition is one exit-condition test. Exactly one of Event or Result
// is set.
//
// +kubebuilder:validation:XValidation:rule="has(self.event) != has(self.result)",message="exactly one of event or result must be set"
type UntilCondition struct {
	// Event matches a semantic event raised by a Service-contract producer.
	// +optional
	Event *UntilEventCondition `json:"event,omitempty"`

	// Result matches a field in an App-contract producer's terminal result.
	// +optional
	Result *UntilResultCondition `json:"result,omitempty"`
}

// UntilEventCondition matches a TaskEvent raised by a named member.
type UntilEventCondition struct {
	// Type is the TaskEvent.type to match.
	Type string `json:"type"`
	// From names the member the event must come from.
	From string `json:"from"`
}

// UntilResultCondition matches a field in a named member's terminal result
// (status.result.values, App-contract producer).
type UntilResultCondition struct {
	// Member names the producer.
	Member string `json:"member"`
	// Field names the result.values entry to read.
	Field string `json:"field"`
	// Equals is the string-encoded value that satisfies the condition.
	Equals string `json:"equals"`
}

// FlowRetention bounds a loop's status and child-object growth. The bound is
// part of the loop construct, not a tunable added
// later: an orchestrator that offers indefinite repetition owes a bounded
// status object.
type FlowRetention struct {
	// KeepIterationRecords bounds status.actions[].recentIterations. Full
	// history is still available as the iterationsCompleted/iterationsFailed
	// scalar counters, which never truncate.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	KeepIterationRecords int32 `json:"keepIterationRecords,omitempty"`

	// KeepChildren bounds how many completed iteration child FleetActions are
	// kept before the controller deletes the oldest (owner-referenced, so
	// deletion is a normal Delete call, not a GC race).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=20
	KeepChildren int32 `json:"keepChildren,omitempty"`

	// KeepFailedChildren bounds how many FAILED iteration children are kept,
	// separately from KeepChildren — the failures are the ones worth keeping,
	// so they are not evicted merely because more recent successes exist.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	KeepFailedChildren int32 `json:"keepFailedChildren,omitempty"`
}

// SelectNode branches on a value a previous member produced.
//
// select is TOTAL: Default is REQUIRED. An unmatched select with no default
// is an admission error, not a runtime hang — `{}` is the explicit no-op that
// satisfies this without weakening totality into an optional field.
//
// The field is `subject`, never `on`: YAML 1.1 (what go-yaml v2, and
// therefore kubectl, parses) reads a bare `on:` mapping key as the boolean
// true, so the key an author writes would not be the key the API server
// receives.
type SelectNode struct {
	// Subject is the value to branch on.
	Subject SelectSubject `json:"subject"`

	// Cases are tried in order; the first matching case's Then runs. Case
	// order is significant: a `present` case is evaluated before any value
	// comparison, so a `present: false` case never falls through to an
	// `equals` test on an absent value.
	// +optional
	Cases []SelectCase `json:"cases,omitempty"`

	// Default runs when no case matches. REQUIRED — the key must be present
	// in the object (still enforced structurally: PreserveUnknownFields makes
	// the VALUE schemaless, not the key's presence) — use `{}` for an
	// explicit no-op (Schemaless; see FlowNode.Sequence).
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Default FlowNode `json:"default"`
}

// SelectSubject addresses the value to branch on. It is a CLOSED DISCRIMINATED
// UNION with exactly two arms, and which arm is legal is decided by the
// producer's contract, NOT by the author:
//
//   - the RESULT form, `{member, field}` — valid only for an App-contract
//     producer, which runs to completion and writes status.result ONCE at the
//     terminal transition. The value cannot change after it is read.
//   - the EVENT form, `{event: {type, from}, field}` — valid only for a
//     Service-contract producer, which is resident, has no meaningful result
//     until it ends, and reports its observations as semantic events.
//
// A result binding onto a Service producer would read either nothing or
// something that keeps changing — a field whose meaning depends on where it is
// read, which every consumer would then have to treat as a sample rather than a
// fact — so admission rejects it, resolving the producer's contract from the
// referenced ComputeEndpoint and naming it in the message
// (internal/webhook/fleettask_bindings.go).
//
// The shape deliberately mirrors UntilCondition above, which made the same
// Event-or-Result split first and for the same reason. Two spellings of one
// distinction is two things to learn and two places for them to drift.
//
// # WHAT THE EVENT ARM READS, AND WHEN
//
// The event arm reads the producing member's `FleetAction.status.events[]`,
// filled by the control plane's consumer of
// `compute.v1.ComputeRuntimeService.StreamStatus`. It reads the HIGHEST-seq
// retained event of the named type and branches on that event's payload.
//
// It does NOT wait for the producer to settle, and the result arm does. That
// asymmetry is the contract, not an inconsistency: an App producer writes its
// result once at the terminal transition, so the result arm waits to be sure the
// value cannot still change; a Service producer is resident and never settles
// while it is doing its job, so waiting for it would mean waiting for the thing
// that raises the event to stop existing. An accepted event is immutable — the
// ingest refuses to rewrite a seq it has already recorded — which is the same
// property the result arm's wait buys, obtained a different way.
//
// TWO THINGS AN AUTHOR MUST KNOW. First, `status.events[]` is a BOUNDED WINDOW
// and not a log: a producer that outruns the consumer can have the awaited event
// evicted before it is read, and `since_seq` never replays an evicted event.
// `status.eventsDropped` is what tells that apart from "never raised", and the
// evaluator's diagnostic says which. Second, a node whose producer SETTLED
// without the event ever being readable fails, naming it — it does not take
// `default`. Taking `default` would report "the producer ran and none of your
// cases matched", a claim about a value, when no value was read at all.
//
// Admission checks that the named event type appears in the producing
// `ComputeEndpoint`'s `spec.emitsEvents`, so a subject bound to an event nobody
// raises is refused at write time rather than waited on forever
// (internal/webhook/fleettask_bindings.go).
//
// +kubebuilder:validation:XValidation:rule="has(self.event) != has(self.member)",message="exactly one of member (result binding, App producer) or event (event binding, Service producer) must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.event) || has(self.from)",message="from is required with event: it names the member that raises it"
// +kubebuilder:validation:XValidation:rule="!has(self.from) || has(self.event)",message="from is meaningful only with event; the result binding names its producer in member"
type SelectSubject struct {
	// Member names an App-contract producer whose terminal status.result.values
	// carries Field. Exactly one of Member or Event is set.
	// +optional
	Member string `json:"member,omitempty"`

	// Event names the semantic event type a Service-contract producer raises,
	// whose payload carries Field. Set together with From. Exactly one of
	// Member or Event is set.
	// +optional
	Event *string `json:"event,omitempty"`

	// From names the member that raises Event. Required with Event, meaningless
	// without it — the result binding names its producer in Member.
	// +optional
	From string `json:"from,omitempty"`

	// Field names the entry to read — from the producer's result (the Member
	// form) or from the matching event's payload (the Event form).
	// +kubebuilder:validation:MinLength=1
	Field string `json:"field"`
}

// Producer returns the member name this subject reads from, whichever arm is
// set. One accessor rather than two call-site switches: every consumer wants
// "which member produces this value", and a consumer that forgot the Event arm
// would read the empty string and wait on a member called "".
func (s SelectSubject) Producer() string {
	if s.Event != nil {
		return s.From
	}
	return s.Member
}

// SelectCase is one branch of a SelectNode. Exactly one predicate
// (Equals/In/Range/Present/When) is set — enforced by the validating webhook,
// not CEL: `in` is a CEL RESERVED WORD (the `in` operator), so `self.in` is a
// parse error and the has() macro also rejects bracket/index syntax
// (`self['in']`) as an argument, and a bare index expression on a typed
// object (as opposed to a genuine open map) has no evaluator overload either
// — there is no CEL spelling of "does this object have an `in` field" left to
// try. See FlowNode's comment for the parallel `loop` case and the general
// webhook-does-recursion rationale.
type SelectCase struct {
	// Equals matches the subject against a literal string.
	// +optional
	Equals *string `json:"equals,omitempty"`

	// In matches the subject against a set of literal strings.
	// +optional
	// +listType=set
	In []string `json:"in,omitempty"`

	// Range matches the subject (or, if set, Range.Field) against numeric bounds.
	// +optional
	Range *SelectRange `json:"range,omitempty"`

	// Present matches whether the producer wrote a value for the subject
	// field at all — a producer may settle Succeeded having produced no value
	// (e.g. a classifier that recognised nothing), which is a different
	// outcome from a failure. Evaluated BEFORE any value comparison case
	// (case order in Cases), so a `present: false` case never falls through
	// to a comparison on an absent value.
	// +optional
	Present *bool `json:"present,omitempty"`

	// When is an optional CEL clause for compound conditions. It is validated and
	// stored, but no CEL environment is wired into the flow evaluator, so it is not
	// evaluated: a select whose evaluation reaches a `when` case fails rather than
	// falling through to the next case (RFC-0001 #fleettask-structured-control-flow-spec-flow).
	// +optional
	When *string `json:"when,omitempty"`

	// Then runs when this case matches (Schemaless; see FlowNode.Sequence).
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Then FlowNode `json:"then"`
}

// SelectRange matches a numeric field against bounds. At least one bound
// SHOULD be set; an empty range matches everything, which is legal but
// pointless.
type SelectRange struct {
	// Field overrides Subject.Field for this comparison only (so a rule can
	// compare `confidence` while branching on a different subject).
	// Unset compares Subject.Field itself.
	// +optional
	Field string `json:"field,omitempty"`

	// +optional
	Lt *float64 `json:"lt,omitempty"`
	// +optional
	Lte *float64 `json:"lte,omitempty"`
	// +optional
	Gt *float64 `json:"gt,omitempty"`
	// +optional
	Gte *float64 `json:"gte,omitempty"`
}
