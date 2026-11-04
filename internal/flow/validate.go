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

package flow

import (
	"fmt"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Ceilings on a flow tree, enforced by ValidateStructure at admission and
// again at reconcile. Nothing else bounds the tree: every recursion point
// generates as x-kubernetes-preserve-unknown-fields, so the only other limit
// is the API server's request-size and decode budget over the schemaless
// subtree — a limit measured in bytes, not in work per reconcile.
const (
	// MaxDepth is the deepest a flow tree may nest, counting the root as 1.
	// The value is chosen against authoring practice, not against a machine
	// limit: the deepest schedule anyone writes by hand nests four or five
	// levels, and a tree past 32 is a generator's output or a mistake. It
	// also caps the recursive walk — validateNode's and the evaluator's
	// alike — at a stack depth that cannot grow with a submitted object.
	MaxDepth = 32

	// MaxNodes is the most nodes a flow tree may contain. The whole tree is
	// walked by validateNode at admission and by evalNode on EVERY reconcile
	// of a non-terminal flow task (a 5 s requeue backstop), so node count is
	// recurring control-plane work an author chooses, not a one-time parse
	// cost. 1024 is far above any hand-authored schedule over a member
	// inventory that is itself a list in the same object, and low enough that
	// the per-reconcile walk stays negligible.
	MaxNodes = 1024
)

// ValidateStructure recursively validates a flow tree's shape — the rules the
// CRD schema itself cannot express for a genuinely recursive type: `loop` is
// a CEL RESERVED WORD (has(self.loop) fails type-checking, verified against a
// real API server, not assumed) and structural CRD schemas cannot express
// true recursion past depth 1 at all (no $ref survives structural-schema
// flattening) — see api/v1/fleettask_flow_types.go's comments for both.
//
// It is the SINGLE source of truth for this check, called from both the
// FleetTask validating webhook (reject at the boundary, on create/update) and
// the FleetTask controller's reconcile-time fallback (belt-and-suspenders,
// the same role v0.3's validateGraph already plays for dependsOn — its own
// comment: "at v0.3 this is validated by validateGraph on every non-terminal
// reconcile instead, not by the admission webhook"). Calling it from both
// means a webhook that is down, misconfigured, or bypassed (a raw
// subresource write, a downgrade) never leaves an invalid flow unchecked, and
// the two call sites can never report a different verdict for the same tree.
//
// members is the set of valid spec.actions[].name values a `run:` leaf may
// reference — MemberNames builds it from spec.actions.
func ValidateStructure(node *fleetv1.FlowNode, members map[string]bool) (string, bool) {
	return validateNode(node, &walkState{members: members}, 1)
}

// MemberNames builds the set ValidateStructure's members parameter expects.
func MemberNames(actions []fleetv1.FleetTaskAction) map[string]bool {
	out := make(map[string]bool, len(actions))
	for i := range actions {
		out[actions[i].Name] = true
	}
	return out
}

// walkState is the per-tree state validateNode threads through the recursion:
// the member inventory a `run:` leaf resolves against, and the running node
// count the MaxNodes ceiling is tested against. Depth is a parameter rather
// than a field because it is per-branch, not per-tree.
type walkState struct {
	members map[string]bool
	nodes   int
}

// validateNode checks one node and recurses into its children. Every check
// the CRD schema declares on a flow node is repeated here, because the schema
// applies none of them below depth 1: the five recursion points are all
// schemaless, so the enums, the minima and the maxima stop at the first
// nesting level and the defaults are not applied there either.
// Absent values therefore stay legal wherever the API server would have
// defaulted them — the evaluator substitutes the identical defaults — while a
// value that is PRESENT and illegal is refused at every depth.
func validateNode(node *fleetv1.FlowNode, w *walkState, depth int) (string, bool) {
	if node == nil {
		return "", true
	}
	if depth > MaxDepth {
		return fmt.Sprintf("flow nests deeper than the maximum depth of %d", MaxDepth), false
	}
	w.nodes++
	if w.nodes > MaxNodes {
		return fmt.Sprintf("flow has more than the maximum of %d nodes", MaxNodes), false
	}
	kinds := 0
	if node.Run != nil {
		kinds++
	}
	if len(node.Sequence) > 0 {
		kinds++
	}
	if node.Parallel != nil {
		kinds++
	}
	if node.Loop != nil {
		kinds++
	}
	if node.Select != nil {
		kinds++
	}
	if kinds > 1 {
		return "a flow node has at most one of run/sequence/parallel/loop/select, or none (the empty no-op node {})", false
	}

	switch {
	case node.Run != nil:
		if !w.members[*node.Run] {
			return fmt.Sprintf("flow references member %q which is not in spec.actions", *node.Run), false
		}
	case len(node.Sequence) > 0:
		for i := range node.Sequence {
			if msg, ok := validateNode(&node.Sequence[i], w, depth+1); !ok {
				return msg, false
			}
		}
	case node.Parallel != nil:
		p := node.Parallel
		if len(p.Branches) == 0 {
			return "parallel.branches must have at least one entry", false
		}
		switch p.OnJoin {
		case "", fleetv1.OnJoinCancelSiblings, fleetv1.OnJoinLetRunToCompletion:
		default:
			// A misspelling here is not cosmetic: it silently becomes
			// LetRunToCompletion, and the siblings a satisfied join was meant
			// to confirmed-cancel are left running.
			return fmt.Sprintf("parallel.onJoin must be CancelSiblings or LetRunToCompletion (got %q)", p.OnJoin), false
		}
		switch p.OnBranchFailure {
		case "", fleetv1.OnBranchFailureFailNode, fleetv1.OnBranchFailureIgnore:
		default:
			// Likewise: a misspelling becomes Ignore, and a Failed branch
			// stops counting against the node at all.
			return fmt.Sprintf("parallel.onBranchFailure must be FailNode or Ignore (got %q)", p.OnBranchFailure), false
		}
		if p.Join != nil {
			switch p.Join.Mode {
			case "", fleetv1.JoinModeAll, fleetv1.JoinModeAny, fleetv1.JoinModeQuorum, fleetv1.JoinModeFirst:
			default:
				return fmt.Sprintf("parallel.join.mode must be one of All, Any, Quorum or First (got %q)", p.Join.Mode), false
			}
			if p.Join.Mode == fleetv1.JoinModeQuorum && p.Join.Quorum == nil {
				return "parallel.join.quorum is required when mode is Quorum", false
			}
			if p.Join.Quorum != nil && *p.Join.Quorum < 1 {
				return "parallel.join.quorum must be at least 1", false
			}
		}
		for i := range p.Branches {
			if msg, ok := validateNode(&p.Branches[i], w, depth+1); !ok {
				return msg, false
			}
		}
	case node.Loop != nil:
		l := node.Loop
		// mode is REQUIRED and enumerated. Unset is the sharp case: the
		// Forever bound below keys on the literal value, so an unset mode
		// skips it, and the evaluator's mode switch then falls through to its
		// Forever branch — an unbounded loop admitted by the very check whose
		// message says it is not (the depth-1 gap defeating the unbounded-loop rule).
		switch l.Mode {
		case fleetv1.LoopModeOnce, fleetv1.LoopModeTimes, fleetv1.LoopModeForever:
		default:
			return fmt.Sprintf("loop.mode is required and must be one of Once, Times or Forever (got %q)", l.Mode), false
		}
		if l.Mode == fleetv1.LoopModeTimes && l.Times == nil {
			return "loop.times is required when mode is Times", false
		}
		if l.Times != nil && *l.Times < 1 {
			return "loop.times must be at least 1", false
		}
		if l.Mode == fleetv1.LoopModeForever && l.Until == nil && l.MaxIterations == nil && l.MaxDurationSeconds == nil {
			return "loop mode: Forever requires until, maxIterations or maxDurationSeconds — an unbounded loop is not admitted", false
		}
		if l.MaxIterations != nil && *l.MaxIterations < 1 {
			return "loop.maxIterations must be at least 1", false
		}
		if l.MaxDurationSeconds != nil && *l.MaxDurationSeconds < 1 {
			return "loop.maxDurationSeconds must be at least 1", false
		}
		if msg, ok := validateRetention(l.Retention); !ok {
			return msg, false
		}
		// An `until` condition names a member too, and it dangled for the same
		// reason `select.subject` did: only `run:` leaves were ever resolved.
		// The failure is quieter here and therefore worse to diagnose — an
		// unresolvable condition simply never matches, so the loop silently
		// falls back to its maxIterations/maxDurationSeconds bound and an
		// author sees a patrol that runs to the wall clock instead of to the
		// thing it was waiting for.
		if l.Until != nil {
			for i := range l.Until.AnyOf {
				c := &l.Until.AnyOf[i]
				switch {
				case c.Result != nil && !w.members[c.Result.Member]:
					return fmt.Sprintf("loop.until.anyOf[%d].result names member %q which is not in spec.actions", i, c.Result.Member), false
				case c.Event != nil && !w.members[c.Event.From]:
					return fmt.Sprintf("loop.until.anyOf[%d].event names member %q which is not in spec.actions", i, c.Event.From), false
				}
			}
		}
		if msg, ok := validateNode(&l.Body, w, depth+1); !ok {
			return msg, false
		}
	case node.Select != nil:
		s := node.Select
		// The subject's producer is resolved against spec.actions exactly as
		// `run:` is, and until this existed it was the ONE reference in the tree
		// that was not. A subject naming a member that does not exist is not a
		// harmless typo: the evaluator waits on that producer, so the select
		// never branches, the enclosing node never settles, and the task sits
		// InProgress forever with nothing anywhere saying which name was wrong.
		//
		// Checked here rather than only in the webhook for the reason every
		// other rule in this file is: the webhook can be down, misconfigured or
		// bypassed, and this same function runs again at the top of every
		// non-terminal reconcile.
		if producer := s.Subject.Producer(); producer == "" {
			return "select.subject must name a producer: `member` for a result binding (App contract) or `from` with `event` for an event binding (Service contract)", false
		} else if !w.members[producer] { // nil members rejects everything, exactly as the `run:` leaf above does
			return fmt.Sprintf("select.subject names member %q which is not in spec.actions", producer), false
		}
		for i := range s.Cases {
			c := &s.Cases[i]
			predicates := 0
			if c.Equals != nil {
				predicates++
			}
			if len(c.In) > 0 {
				predicates++
			}
			if c.Range != nil {
				predicates++
			}
			if c.Present != nil {
				predicates++
			}
			if c.When != nil {
				predicates++
			}
			if predicates != 1 {
				return fmt.Sprintf("select.cases[%d] must have exactly one of equals/in/range/present/when", i), false
			}
			if msg, ok := validateNode(&c.Then, w, depth+1); !ok {
				return msg, false
			}
		}
		if msg, ok := validateNode(&s.Default, w, depth+1); !ok {
			return msg, false
		}
	}
	return "", true
}

// validateRetention re-enforces the CRD's ranges on loop.retention. This is
// the status bound: keepIterationRecords sizes
// status.actions[].recentIterations, so a nested loop asking to keep a million
// records is the unbounded-status failure by a second route. Zero is "unset"
// at every depth — the API server defaults these on a depth-1 loop only, and
// effectiveRetention substitutes the identical defaults below it.
func validateRetention(r *fleetv1.FlowRetention) (string, bool) {
	if r == nil {
		return "", true
	}
	if r.KeepIterationRecords < 0 || r.KeepIterationRecords > 100 {
		return "loop.retention.keepIterationRecords must be between 1 and 100 (0 takes the default of 10)", false
	}
	if r.KeepChildren < 0 {
		return "loop.retention.keepChildren must be at least 1 (0 takes the default of 20)", false
	}
	if r.KeepFailedChildren < 0 {
		return "loop.retention.keepFailedChildren must be at least 1 (0 takes the default of 5)", false
	}
	return "", true
}
