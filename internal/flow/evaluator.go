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

// Package flow evaluates a FleetTaskSpec.flow tree as a PURE
// function of current member state: given the tree and a way to look up each
// referenced member's current child status, it reports the tree's settled
// outcome (or that it is still in progress) and the concrete actions a
// caller must take — which members to ensure have a running child, and which
// non-terminal children to confirmed-cancel now.
//
// This package makes no Kubernetes API calls itself. internal/controller
// wires it to the cluster: Ensure requests become FleetAction Create/adopt
// calls, Cancel requests become the SAME desiredState=Cancelled fan-out the
// v0.3 dependsOn path already uses (which is what makes cancellation here go
// through the confirmed-cancel path — CancelAction/CancelDisposition — rather
// than an abandon: nothing in this package deletes or force-stops anything;
// it only ever asks for the declarative hold the FleetAction controller
// already honours).
package flow

import (
	"fmt"
	"strconv"
	"time"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Outcome is a node's settled result, or its still-running state.
type Outcome string

// Outcome values.
const (
	OutcomeInProgress Outcome = "InProgress"
	OutcomeSucceeded  Outcome = "Succeeded"
	OutcomeFailed     Outcome = "Failed"
	OutcomeCancelled  Outcome = "Cancelled"
)

// Settled reports whether the outcome is terminal.
func (o Outcome) Settled() bool {
	return o == OutcomeSucceeded || o == OutcomeFailed || o == OutcomeCancelled
}

// Ref identifies one member at one iteration. Iteration 0 means "not under a
// loop" — the plain, v0.3-shaped child name.
type Ref struct {
	Member    string
	Iteration int32
}

// ChildState is what the evaluator needs to know about a member's current
// child FleetAction at a given iteration.
type ChildState struct {
	Exists bool
	Phase  fleetv1.ActionPhase
	Result *fleetv1.ActionResult
	// Events is the producer's status.events[] — the bounded, seq-ordered
	// window of semantic observations a Service-contract member has raised.
	// Read by the event arms of select.subject and loop.until; empty for an
	// App-contract or robot member, which raises none.
	//
	// It is a WINDOW, not a log: see FleetActionStatus.Events' godoc for what a
	// consumer is and is not guaranteed. EventsDropped is carried alongside so
	// this package can tell "never raised" from "raised and evicted" — the two
	// absences an author most needs told apart, and the ones a plain empty
	// slice conflates.
	Events []fleetv1.ActionEvent
	// EventsDropped is the producer's status.eventsDropped: how many events
	// retention has evicted from Events over this run's lifetime.
	EventsDropped int64
}

// LatestEvent returns the HIGHEST-seq retained event of the named type, and
// whether one exists.
//
// Highest-seq, not first: a recognition service raises the same type
// repeatedly, and a consumer asking "what was recognised" means the most recent
// recognition, not the one that happened to survive longest in the window. The
// scan is explicit rather than relying on slice order, because the order is a
// property of the ingest rule and this must not silently become wrong if that
// rule ever changes.
func (c ChildState) LatestEvent(eventType string) (fleetv1.ActionEvent, bool) {
	var best fleetv1.ActionEvent
	found := false
	for i := range c.Events {
		if c.Events[i].Type != eventType {
			continue
		}
		if !found || c.Events[i].Seq > best.Seq {
			best, found = c.Events[i], true
		}
	}
	return best, found
}

// EventAbsenceNote renders the difference between "this producer has never
// raised that event" and "it may have, and retention evicted it". An author
// staring at a node that will not advance needs to know which, and the
// retention bound is the one thing in this design that can make a correct task
// fail with no other trace.
func (c ChildState) EventAbsenceNote() string {
	if c.EventsDropped > 0 {
		return fmt.Sprintf(
			"; %d event(s) have already been evicted from this producer's bounded status.events[] "+
				"window, so the one awaited may have been raised and dropped before it was read — "+
				"StreamStatus.since_seq resumes from the high-water mark and never replays an "+
				"evicted event",
			c.EventsDropped)
	}
	return "; no event has been evicted from this producer's status.events[], so none of this type has ever been raised"
}

func (c ChildState) outcome() Outcome {
	if !c.Exists {
		return OutcomeInProgress
	}
	switch c.Phase {
	case fleetv1.ActionPhaseSucceeded:
		return OutcomeSucceeded
	case fleetv1.ActionPhaseFailed:
		return OutcomeFailed
	case fleetv1.ActionPhaseCancelled:
		return OutcomeCancelled
	default:
		return OutcomeInProgress
	}
}

func (c ChildState) nonTerminal() bool {
	return c.Exists && !c.outcome().Settled()
}

// LoopProgress is a loop node's persisted state, read from and written back
// onto the loop's ANCHOR member (the first `run:` leaf found in Body,
// pre-order) — every member touched within one iteration of a loop's body
// advances together, so one counter suffices regardless of how many members
// the body's tree touches.
type LoopProgress struct {
	// Iteration is the current (1-based) iteration number; 0 means the loop
	// has not started.
	Iteration int32
	// StartedAt is when iteration 1 began, for maxDurationSeconds. Nil if
	// the loop has not started.
	StartedAt *time.Time
}

// Context supplies the evaluator with current cluster state. Nil Child or
// LoopProgress panics — both are required.
type Context struct {
	// Now is the evaluation instant, for maxDurationSeconds bounds.
	Now time.Time
	// Child resolves a member's current child state at a given iteration.
	Child func(member string, iteration int32) ChildState
	// LoopProgress resolves a loop's current progress, keyed by the loop's
	// anchor member name.
	LoopProgress func(anchorMember string) LoopProgress
}

// IterationSettlement is one loop iteration reaching a terminal outcome,
// reported so the caller can update FleetTaskActionStatus's iteration
// counters and bounded RecentIterations.
type IterationSettlement struct {
	AnchorMember string
	Iteration    int32
	Outcome      Outcome
}

// Eval is the evaluator's output for one Reconcile pass.
type Eval struct {
	// Outcome is the root node's outcome (InProgress until the whole tree settles).
	Outcome Outcome
	// Ensure lists members that should have a child FleetAction created (or
	// adopted, if one with that name already exists — the caller's Create is
	// idempotent the same way the existing dependsOn path already is).
	Ensure []Ref
	// Cancel lists members whose current non-terminal child should be
	// confirmed-cancelled now (desiredState: Cancelled fan-out).
	Cancel []Ref
	// Settled reports every loop iteration that reached a terminal outcome
	// THIS pass, so the caller can advance the anchor member's counters.
	Settled []IterationSettlement
	// StartIteration reports, per anchor member, the iteration number a NEW
	// iteration should start at this pass (the caller persists
	// LoopProgress.Iteration to this value before the next reconcile).
	StartIteration map[string]int32
	// Diagnostic explains a Failed outcome the FLOW ITSELF produced, as
	// distinct from one a child action produced. A child's failure already has
	// a place to be read — the child object — but a node that fails because the
	// evaluator cannot evaluate it has none, and "Failed" with no reason on a
	// task the author wrote correctly is the worst of both.
	//
	// Empty for every ordinary outcome, including a Failed one inherited from a
	// child: an explanation offered for a failure that explains itself teaches a
	// reader to ignore the field.
	Diagnostic string
}

func (e *Eval) ensure(m string, it int32) {
	e.Ensure = append(e.Ensure, Ref{Member: m, Iteration: it})
}
func (e *Eval) cancel(m string, it int32) {
	e.Cancel = append(e.Cancel, Ref{Member: m, Iteration: it})
}

// Evaluate walks root (the task's spec.flow) and returns the tree's current
// outcome plus the actions the caller must take. iter is always 0 at the
// root call; recursion supplies the loop-local iteration internally.
func Evaluate(root *fleetv1.FlowNode, ctx *Context) *Eval {
	out := &Eval{StartIteration: map[string]int32{}}
	out.Outcome = evalNode(root, 0, ctx, out)
	return out
}

func evalNode(node *fleetv1.FlowNode, iter int32, ctx *Context, out *Eval) Outcome {
	switch {
	case node == nil:
		return OutcomeSucceeded // absent node: no-op (used for a nil Default, never happens via API but safe)
	case node.Run != nil:
		return evalRun(*node.Run, iter, ctx, out)
	case len(node.Sequence) > 0:
		return evalSequence(node.Sequence, iter, ctx, out)
	case node.Parallel != nil:
		return evalParallel(node.Parallel, iter, ctx, out)
	case node.Loop != nil:
		return evalLoop(node.Loop, ctx, out)
	case node.Select != nil:
		return evalSelect(node.Select, iter, ctx, out)
	default:
		return OutcomeSucceeded // {} — the explicit no-op node
	}
}

func evalRun(member string, iter int32, ctx *Context, out *Eval) Outcome {
	st := ctx.Child(member, iter)
	if !st.Exists {
		out.ensure(member, iter)
	}
	return st.outcome()
}

func evalSequence(nodes []fleetv1.FlowNode, iter int32, ctx *Context, out *Eval) Outcome {
	for i := range nodes {
		o := evalNode(&nodes[i], iter, ctx, out)
		if o == OutcomeSucceeded {
			continue
		}
		return o // InProgress holds here; Failed/Cancelled short-circuits (later nodes never started)
	}
	return OutcomeSucceeded
}

// evalLoop, evalSelect land in later, separately-tested passes (build order:
// sequence, parallel+join, loop, select).

// evalParallel implements the join table, plus the rule this task
// is non-negotiable about: onJoin: CancelSiblings goes through the
// CONFIRMED-CANCEL path (never an abandon — it only ever ASKS via out.cancel,
// which the caller turns into the same desiredState=Cancelled fan-out the
// v0.3 dependsOn path already uses), and the node reaches ITS OWN terminal
// outcome only once EVERY branch has settled — join satisfaction merely
// INITIATES cancellation of the rest. Reporting Succeeded while a robot is
// still decelerating is exactly what that ordering prevents.
func evalParallel(p *fleetv1.ParallelNode, iter int32, ctx *Context, out *Eval) Outcome {
	branchOutcomes := make([]Outcome, len(p.Branches))
	for i := range p.Branches {
		branchOutcomes[i] = evalNode(&p.Branches[i], iter, ctx, out)
	}

	mode := fleetv1.JoinModeAll
	quorum := int32(len(p.Branches))
	if p.Join != nil {
		if p.Join.Mode != "" {
			mode = p.Join.Mode
		}
		if p.Join.Quorum != nil {
			quorum = *p.Join.Quorum
		}
	}
	onBranchFailure := fleetv1.OnBranchFailureFailNode
	if p.OnBranchFailure != "" {
		onBranchFailure = p.OnBranchFailure
	}
	onJoin := fleetv1.OnJoinCancelSiblings
	if p.OnJoin != "" {
		onJoin = p.OnJoin
	}

	succeeded, settled := 0, 0
	firstSettledIdx := -1
	for i, o := range branchOutcomes {
		if o.Settled() {
			settled++
			if firstSettledIdx == -1 {
				firstSettledIdx = i
			}
		}
		if o == OutcomeSucceeded {
			succeeded++
		}
	}
	total := len(branchOutcomes)

	joinSatisfied := false
	var nodeOutcome Outcome
	switch mode {
	case fleetv1.JoinModeFirst:
		// Takes whatever settles first, success or not — the distinction from
		// Any that the design calls out as easy to get wrong.
		if firstSettledIdx != -1 {
			joinSatisfied, nodeOutcome = true, branchOutcomes[firstSettledIdx]
		}
	case fleetv1.JoinModeAny:
		// Waits for a SUCCESS specifically; fails only once every branch has
		// settled non-Succeeded (nothing left that could still succeed).
		switch {
		case succeeded >= 1:
			joinSatisfied, nodeOutcome = true, OutcomeSucceeded
		case settled == total:
			joinSatisfied, nodeOutcome = true, OutcomeFailed
		}
	case fleetv1.JoinModeQuorum:
		remainingPossible := succeeded + (total - settled)
		switch {
		case int32(succeeded) >= quorum:
			joinSatisfied, nodeOutcome = true, OutcomeSucceeded
		case int32(remainingPossible) < quorum:
			joinSatisfied, nodeOutcome = true, OutcomeFailed
		}
	default: // All
		anyFailedNow := false
		for _, o := range branchOutcomes {
			if (o == OutcomeFailed && onBranchFailure == fleetv1.OnBranchFailureFailNode) || o == OutcomeCancelled {
				anyFailedNow = true
				break
			}
		}
		switch {
		case anyFailedNow:
			joinSatisfied, nodeOutcome = true, OutcomeFailed
		case settled == total:
			joinSatisfied, nodeOutcome = true, OutcomeSucceeded
		}
	}

	if !joinSatisfied {
		return OutcomeInProgress
	}

	if onJoin == fleetv1.OnJoinCancelSiblings {
		for i, o := range branchOutcomes {
			if !o.Settled() {
				collectCancel(&p.Branches[i], iter, ctx, out)
			}
		}
	}
	if settled < total {
		// Join satisfaction reached, cancellation initiated above, but the
		// NODE is not terminal until every branch confirms settlement.
		return OutcomeInProgress
	}
	return nodeOutcome
}

// collectCancel requests confirmed-cancellation of every non-terminal `run:`
// leaf reachable under node. It is conservative rather than precise about
// which branch of a nested select/loop is "active": a Ref for a member with
// no existing child is a no-op for the caller (Ensure was never requested
// for it), so over-approximating costs nothing.
func collectCancel(node *fleetv1.FlowNode, iter int32, ctx *Context, out *Eval) {
	switch {
	case node == nil:
		return
	case node.Run != nil:
		if ctx.Child(*node.Run, iter).nonTerminal() {
			out.cancel(*node.Run, iter)
		}
	case len(node.Sequence) > 0:
		for i := range node.Sequence {
			collectCancel(&node.Sequence[i], iter, ctx, out)
		}
	case node.Parallel != nil:
		for i := range node.Parallel.Branches {
			collectCancel(&node.Parallel.Branches[i], iter, ctx, out)
		}
	case node.Loop != nil:
		anchor := anchorMember(&node.Loop.Body)
		if anchor == "" {
			return
		}
		lp := ctx.LoopProgress(anchor)
		if lp.Iteration > 0 {
			collectCancel(&node.Loop.Body, lp.Iteration, ctx, out)
		}
	case node.Select != nil:
		for i := range node.Select.Cases {
			collectCancel(&node.Select.Cases[i].Then, iter, ctx, out)
		}
		collectCancel(&node.Select.Default, iter, ctx, out)
	}
}

// anchorMember returns the first `run:` leaf found in a pre-order walk of
// node — the member a loop's iteration counter is read from and written to
// (see LoopProgress).
func anchorMember(node *fleetv1.FlowNode) string {
	switch {
	case node == nil:
		return ""
	case node.Run != nil:
		return *node.Run
	case len(node.Sequence) > 0:
		for i := range node.Sequence {
			if m := anchorMember(&node.Sequence[i]); m != "" {
				return m
			}
		}
	case node.Parallel != nil:
		for i := range node.Parallel.Branches {
			if m := anchorMember(&node.Parallel.Branches[i]); m != "" {
				return m
			}
		}
	case node.Loop != nil:
		return anchorMember(&node.Loop.Body)
	case node.Select != nil:
		for i := range node.Select.Cases {
			if m := anchorMember(&node.Select.Cases[i].Then); m != "" {
				return m
			}
		}
		return anchorMember(&node.Select.Default)
	}
	return ""
}

// evalLoop. A loop's identity is its anchor member
// (anchorMember): every member the body touches advances in lockstep, one
// iteration at a time, so one persisted counter suffices.
//
// Starting a new iteration costs one extra reconcile with no body evaluation:
// on deciding to continue, this function only requests StartIteration and
// returns InProgress — the body is evaluated (and its children created) once
// the CALLER has persisted the advanced iteration number, so a child is
// never created against an iteration count this pass has not yet committed.
func evalLoop(l *fleetv1.LoopNode, ctx *Context, out *Eval) Outcome {
	anchor := anchorMember(&l.Body)
	if anchor == "" {
		return OutcomeSucceeded // no run: leaf anywhere in body — nothing to repeat
	}
	lp := ctx.LoopProgress(anchor)

	if lp.Iteration == 0 {
		out.StartIteration[anchor] = 1
		return OutcomeInProgress
	}

	exitReady := loopBoundSatisfied(l, lp, ctx)

	// interruptRunning: confirmed-cancel the in-flight child NOW rather than
	// waiting for it to settle between iterations. Default
	// false is the conservative choice — interrupting mid-execution costs a
	// full cancel-to-confirmed-rest cycle, and an author opts into paying it.
	if l.InterruptRunning && exitReady {
		collectCancel(&l.Body, lp.Iteration, ctx, out)
	}

	bodyOutcome := evalNode(&l.Body, lp.Iteration, ctx, out)
	if !bodyOutcome.Settled() {
		return OutcomeInProgress
	}

	out.Settled = append(out.Settled, IterationSettlement{AnchorMember: anchor, Iteration: lp.Iteration, Outcome: bodyOutcome})

	switch {
	case bodyOutcome == OutcomeFailed:
		return OutcomeFailed
	case bodyOutcome == OutcomeCancelled && !exitReady:
		// Cancelled for a reason other than THIS loop's own interrupt exit
		// (e.g. an external task-level cancel) — propagate, do not continue.
		return OutcomeCancelled
	}
	// bodyOutcome is Succeeded, or Cancelled-by-our-own-interrupt-exit — both
	// are a normal, intended end to this iteration.

	switch l.Mode {
	case fleetv1.LoopModeOnce:
		return OutcomeSucceeded
	case fleetv1.LoopModeTimes:
		times := int32(1)
		if l.Times != nil {
			times = *l.Times
		}
		if lp.Iteration >= times {
			return OutcomeSucceeded
		}
	case fleetv1.LoopModeForever:
		if exitReady {
			return OutcomeSucceeded
		}
	default:
		// Unreachable for a validated tree: ValidateStructure refuses an
		// absent or unenumerated mode at every depth, and it runs at
		// admission AND at the top of every reconcile. Kept explicit and
		// fail-CLOSED because the previous spelling of this switch was
		// `default: // Forever`, which turned an unset mode — the one the CRD
		// enum could not see below depth 1 — into an unbounded loop
		// (the depth-1 gap defeating the unbounded-loop rule). A loop whose bound cannot be
		// interpreted must stop, not repeat.
		return OutcomeFailed
	}

	out.StartIteration[anchor] = lp.Iteration + 1
	return OutcomeInProgress
}

// loopBoundSatisfied reports whether the loop's exit condition currently
// holds: a matching Until.AnyOf entry, or MaxIterations/MaxDurationSeconds
// reached.
//
// BOTH Until arms are evaluable now. The Result form reads an App-contract
// producer's terminal status.result.values; the Event form reads a
// Service-contract producer's status.events[], satisfied by the EXISTENCE of one
// retained event of the named type from the named member. Existence, not a
// payload comparison, because that is all UntilEventCondition declares — it
// carries a Type and a From and no field, so "it happened" is the whole
// question, and a loop that exits on a recognition exits on the recognition
// having occurred.
//
// UNSATISFIED REMAINS SILENT HERE, and deliberately — the asymmetry with
// evalSelect's event arm is the same one that was argued when neither could be
// read. An `until` that does not match is the ordinary case on every iteration
// before the last; a Forever loop must carry another bound (maxIterations or
// maxDurationSeconds, enforced by ValidateStructure) and therefore still
// terminates. A select subject has no such fallback, so silence there costs the
// author the diagnosis and is reported instead.
func loopBoundSatisfied(l *fleetv1.LoopNode, lp LoopProgress, ctx *Context) bool {
	if l.Until != nil {
		for i := range l.Until.AnyOf {
			cond := l.Until.AnyOf[i]
			switch {
			case cond.Event != nil:
				if resolveUntilEvent(cond.Event, lp.Iteration, ctx) {
					return true
				}
			case cond.Result != nil:
				if v, ok := resolveUntilResult(cond.Result, lp.Iteration, ctx); ok && v == cond.Result.Equals {
					return true
				}
			}
		}
	}
	if l.MaxIterations != nil && lp.Iteration >= *l.MaxIterations {
		return true
	}
	if l.MaxDurationSeconds != nil && lp.StartedAt != nil {
		if !ctx.Now.Before(lp.StartedAt.Add(time.Duration(*l.MaxDurationSeconds) * time.Second)) {
			return true
		}
	}
	return false
}

// resolveUntilEvent reports whether the named member has a retained event of
// the named type. It searches the SAME two places resolveUntilResult does, and
// for the same two design cases: the loop-body member at this iteration (the
// loop watching its own body), then a non-looped sibling at iteration 0 (a
// parallel watcher member outside the loop). The watcher is the common shape
// for an event — a resident recognition service runs beside the loop, not
// inside it — so the iteration-0 arm is the one that usually answers.
func resolveUntilEvent(e *fleetv1.UntilEventCondition, iteration int32, ctx *Context) bool {
	if st := ctx.Child(e.From, iteration); st.Exists {
		if _, ok := st.LatestEvent(e.Type); ok {
			return true
		}
	}
	if iteration != 0 {
		if st := ctx.Child(e.From, 0); st.Exists {
			if _, ok := st.LatestEvent(e.Type); ok {
				return true
			}
		}
	}
	return false
}

// resolveUntilResult reads (member, field) from an App-contract producer's
// terminal result. It tries the loop-body member at THIS
// iteration first (the common case: the loop is checking its own body's
// output), then falls back to a non-looped sibling at iteration 0 (design's
// other case: a parallel watcher member outside the loop).
func resolveUntilResult(r *fleetv1.UntilResultCondition, iteration int32, ctx *Context) (string, bool) {
	if st := ctx.Child(r.Member, iteration); st.Exists && st.Result != nil {
		if v, ok := st.Result.Values[r.Field]; ok {
			return v, true
		}
	}
	if st := ctx.Child(r.Member, 0); st.Exists && st.Result != nil {
		if v, ok := st.Result.Values[r.Field]; ok {
			return v, true
		}
	}
	return "", false
}

// evalSelect implements select: TOTAL (Default always runs when
// no Case matches — there is no "no match" outcome), and case order is
// significant (the first matching Case wins, which is how `present` gets to
// run before a value comparison without special-casing it here: an author
// who wants that puts `present` first in Cases).
//
// Binds to the SUBJECT member's status.result.values — write-once at the
// terminal transition for an App-contract producer — so this
// waits for that member to settle before reading anything, never samples a
// value that could still change.
func evalSelect(s *fleetv1.SelectNode, iter int32, ctx *Context, out *Eval) Outcome {
	// THE EVENT ARM READS status.events[], AND DOES NOT WAIT FOR THE PRODUCER
	// TO SETTLE. That is the whole difference from the result arm below, and it
	// is forced by the contract: an App producer writes its result ONCE at the
	// terminal transition, so the result arm waits for settlement to be sure the
	// value cannot still change; a Service producer is resident and never
	// settles while it is doing its job, so waiting for it would mean waiting
	// for the thing that is supposed to raise the event to stop existing. An
	// event is immutable once accepted — the ingest rule refuses to rewrite an
	// accepted seq — so reading one mid-run is not sampling a value that could
	// change, which is the property the result arm's wait exists to buy.
	//
	// The node waits (InProgress) while no matching event has been raised and
	// the producer is still running, and it FAILS, naming the event, once the
	// producer has settled without ever raising one. It still does not take
	// `default` on absence: `default` means "the producer ran and none of your
	// cases matched", which is a claim about a value, and no value was read at
	// all. The diagnostic now also says whether retention evicted anything,
	// because "never raised" and "raised and dropped" are different facts and a
	// bounded window is what makes the second one reachable.
	if s.Subject.Event != nil {
		st := ctx.Child(s.Subject.From, iter)
		if !st.Exists {
			out.ensure(s.Subject.From, iter)
			return OutcomeInProgress
		}
		ev, ok := st.LatestEvent(*s.Subject.Event)
		if !ok {
			o := st.outcome()
			if !o.Settled() {
				return OutcomeInProgress // still watching; the event may yet be raised
			}
			out.Diagnostic = fmt.Sprintf(
				"select.subject binds to event %q from member %q, which reached %s without that "+
					"event being readable. The node fails rather than taking select.default, "+
					"because no value was read and 'none of your cases matched' would be a claim "+
					"about a value that does not exist%s",
				*s.Subject.Event, s.Subject.From, o, st.EventAbsenceNote())
			return OutcomeFailed
		}
		for i := range s.Cases {
			if s.Cases[i].When != nil {
				out.Diagnostic = unevaluableWhenDiagnostic(i, *s.Cases[i].When)
				return OutcomeFailed
			}
			if caseMatches(&s.Cases[i], s.Subject.Field, ev.Payload) {
				return evalNode(&s.Cases[i].Then, iter, ctx, out)
			}
		}
		return evalNode(&s.Default, iter, ctx, out)
	}
	st := ctx.Child(s.Subject.Member, iter)
	if !st.Exists {
		out.ensure(s.Subject.Member, iter)
		return OutcomeInProgress
	}
	o := st.outcome()
	if !o.Settled() {
		return OutcomeInProgress
	}
	if o != OutcomeSucceeded {
		return o // the producer itself failed/was cancelled; no value was ever produced to branch on
	}

	for i := range s.Cases {
		// A `when` case is a CEL clause over the closed TaskTrigger-like context,
		// and no CEL environment is wired into this package. It is reported, not
		// skipped — and the reason is sharper than the event arm's.
		//
		// An unevaluable `when` that merely "does not match" does not stop the
		// select; it falls through to the NEXT case, or to `default`, and the
		// task then runs a real branch that the author's first rule was supposed
		// to pre-empt. Nothing fails, nothing is logged, and the run looks
		// correct. That is strictly worse than the event arm, which at least
		// produces no answer at all: here the wrong answer is indistinguishable
		// from the right one.
		//
		// The case is examined in ORDER, so a `when` after an already-matching
		// case is never reached and never reported — the author's earlier rule
		// decided the branch, and an unevaluated clause below it changed nothing.
		if s.Cases[i].When != nil {
			out.Diagnostic = unevaluableWhenDiagnostic(i, *s.Cases[i].When)
			return OutcomeFailed
		}
		if caseMatches(&s.Cases[i], s.Subject.Field, resultValues(st.Result)) {
			return evalNode(&s.Cases[i].Then, iter, ctx, out)
		}
	}
	return evalNode(&s.Default, iter, ctx, out)
}

// unevaluableWhenDiagnostic is the `when`-case message, shared by both subject
// arms. One spelling, because an author debugging a `when` should not have to
// notice that the wording differs depending on whether the subject was an event
// or a result — the limitation is the same one.
func unevaluableWhenDiagnostic(i int, when string) string {
	return fmt.Sprintf(
		"select.cases[%d] uses `when: %q`, which is not evaluable at v0.4: no CEL "+
			"environment is wired into the flow evaluator. The node fails rather than "+
			"falling through to the next case, because falling through would run a "+
			"branch this case was written to pre-empt and nothing would say so",
		i, when)
}

// resultValues is the result arm's adapter onto the same scalar map the event
// arm reads from a payload. Both subject forms name a Field and compare a
// string; the only difference is which map it is read from, and caseMatches
// should not have to know.
func resultValues(result *fleetv1.ActionResult) map[string]string {
	if result == nil {
		return nil
	}
	return result.Values
}

func caseMatches(c *fleetv1.SelectCase, subjectField string, values map[string]string) bool {
	switch {
	case c.Present != nil:
		_, present := fieldValue(values, subjectField)
		return present == *c.Present
	case c.Equals != nil:
		v, ok := fieldValue(values, subjectField)
		return ok && v == *c.Equals
	case len(c.In) > 0:
		v, ok := fieldValue(values, subjectField)
		if !ok {
			return false
		}
		for _, x := range c.In {
			if x == v {
				return true
			}
		}
		return false
	case c.Range != nil:
		field := subjectField
		if c.Range.Field != "" {
			field = c.Range.Field
		}
		v, ok := fieldValue(values, field)
		if !ok {
			return false
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return false
		}
		if c.Range.Lt != nil && !(f < *c.Range.Lt) {
			return false
		}
		if c.Range.Lte != nil && !(f <= *c.Range.Lte) {
			return false
		}
		if c.Range.Gt != nil && !(f > *c.Range.Gt) {
			return false
		}
		if c.Range.Gte != nil && !(f >= *c.Range.Gte) {
			return false
		}
		return true
	case c.When != nil:
		// Unreachable: evalSelect reports a `when` case before reaching this
		// matcher, because an unevaluable clause must not be reported as "did
		// not match" — see the comment there. Kept fail-closed rather than
		// deleted, so a future caller that reaches the matcher directly does
		// not get a silent true.
		return false
	default:
		return false
	}
}

func fieldValue(values map[string]string, field string) (string, bool) {
	v, ok := values[field]
	return v, ok
}
