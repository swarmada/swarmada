# ADR-0074: A flow tree carries a depth ceiling of 32 and a node ceiling of 1024, and every constraint the schema declares is re-enforced at every depth

- **Status:** accepted
- **Date:** 2026-09-21
- **Deciders:** Maintainers, API Designer, Controller Owner
- **Related:** ADR-0063 (a declared field that nothing evaluates is absent, not inert);
  RFC-0001 `crds/fleettask.md` (`#fleettask-the-flownode-tree`, and its Admission section);
  `internal/flow/validate.go`, `internal/flow/evaluator.go`,
  `internal/webhook/fleettask_webhook.go`, `api/v1/fleettask_flow_types.go`,
  `config/crd/bases/swarmada.io_fleettasks.yaml`

## Context

`FleetTask.spec.flow` is a genuinely recursive type. A `FlowNode` carries at most one of five keys,
and five of them nest another node: `sequence[]`, `parallel.branches[]`, `loop.body`,
`select.cases[].then` and `select.default`.

A structural CRD schema cannot express that recursion. No `$ref` survives structural-schema
flattening, so every recursion point is generated as `x-kubernetes-preserve-unknown-fields: true`
(`config/crd/bases/swarmada.io_fleettasks.yaml`) and **nothing below depth 1 is seen by the schema
at all** — not the enums, not the numeric minima and maxima, not the node-level CEL rules, and not
the defaults. `loop` compounds it: the identifier is a CEL reserved word, so `has(self.loop)` does
not type-check, verified against a real API server rather than assumed
(`api/v1/fleettask_flow_types.go`).

Two facts followed.

**The rule an author reads at depth 1 was not the rule that held below it.** The API server serves
the message that an unbounded loop is not admitted, while admitting exactly that for every loop
below the root. The live defect was narrower and sharper than "the CEL rules stop at depth 1":
`validateNode` already recursed the whole tree and already enforced the `Forever` bound at every
depth. What it never checked was that `loop.mode` held a legal value. A nested loop that
omitted `mode` skipped the `Forever` test — which keys on the literal string — and then fell
through the evaluator's `default: // Forever`, which turned an unset mode into an unbounded loop.

**Nothing bounded the tree's size.** No depth, breadth or node-count limit existed anywhere. The
only remaining limit was the API server's request-size and decode budget over the schemaless
subtree — a limit measured in bytes, not in work per reconcile. The whole tree is walked by
`validateNode` at admission and by `evalNode` on **every** reconcile of a non-terminal flow task,
which runs against a 5-second requeue backstop. Node count is therefore recurring control-plane
work that the author of an object chooses.

## Decision

**A flow tree is bounded by two ceilings — `MaxDepth = 32` and `MaxNodes = 1024` — and the
recursive walk re-enforces every constraint the CRD schema declares on a flow node, at every
depth.**

### 1 · The ceilings, and why these values

`internal/flow/validate.go:25-47` declares both, with the reasoning in the source:

- **`MaxDepth = 32`**, counting the root as 1. The value is chosen against authoring practice, not
  against a machine limit: the deepest schedule written by hand nests four or five levels, and a
  tree past 32 is a generator's output or a mistake. It also caps the recursive walk —
  `validateNode`'s and the evaluator's alike — at a stack depth that cannot grow with a submitted
  object.
- **`MaxNodes = 1024`**. Far above any hand-authored schedule over a member inventory that is
  itself a list in the same object, and low enough that the per-reconcile walk stays negligible.

`validateNode` tests depth on entry and increments the running node count before any other check
(`:98-108`), so both ceilings are enforced before the walk can do work proportional to the excess.

### 2 · Breadth is bounded transitively, with no per-key arity limit

No single key carries an arity of its own. The node ceiling counts the whole tree, so two branches
each individually under the ceiling still trip it together. A per-key arity limit would not close
the hole it appears to close: any per-key bound still permits an unbounded total through nesting.

### 3 · One walker, called from both enforcement points

`ValidateStructure` is the single source of truth for the shape check. It is called from the
`FleetTask` validating webhook, which rejects at the boundary on create and update, and again from
the controller's reconcile-time fallback. Admission is where an author learns; the reconcile-time
call is the belt-and-braces path for an object that reached storage another way.

### 4 · Every constraint is repeated at every depth, not only `mode` and the `Forever` bound

Because the schema applies none of its declarations below depth 1, the recursive check repeats all
of them: the enums, the required `loop.mode`, the conditional requirements, and the numeric minima
and maxima **including** `retention`'s. `retention.keepIterationRecords` above its maximum is the
unbounded-status failure by a second route, so enforcing only the loop bound would have left that
route open.

### 5 · An absent value stays legal; a value that is present and illegal is refused

The API server cannot default below depth 1, so requiring a key at depth 2 that a depth-1 node gets
by defaulting would make a nested `parallel` need keys that a top-level one does not. The evaluator
substitutes the identical defaults for the Go zero value, so behaviour matches. What is refused at
every depth is a value that is **present and illegal**.

The cost is recorded plainly in `crds/fleettask.md`: a nested node's stored object does not show
the defaulted keys in `kubectl get -o yaml`. Behaviour matches; the object is not self-describing.

### 6 · The evaluator's loop-mode switch is explicit and fails closed

`internal/flow/evaluator.go:510-524` names `Forever` as its own case and makes the `default` arm
`return OutcomeFailed`. The arm is unreachable for a validated tree — `ValidateStructure` refuses
an absent or unenumerated mode at every depth, at admission and at the top of every reconcile — and
it is kept explicit anyway. The entire defect was a fall-through that turned "unknown" into "run
forever". If the gate is ever bypassed, a loop whose bound cannot be interpreted must stop.

### 7 · The messages are published

RFC-0001 #fleettask-admission lists the rejection messages the webhook returns, including
`flow nests deeper than the maximum depth of 32` and `flow has more than the maximum of 1024
nodes`. RFC-0001 #fleettask-the-flownode-tree states that no single key carries an arity limit of
its own and that the tree as a whole carries the two ceilings.

## Alternatives considered

- **Rely on the API server's request-size limit.** Rejected. Bytes bound neither stack depth nor
  per-reconcile walk cost. A compact object can nest deeply, and the walk repeats on every
  reconcile of a non-terminal task, so the quantity that needs bounding is not the one that limit
  measures.
- **A tighter ceiling, 8 to 10 levels.** Rejected. It refuses legitimate generated trees for no
  safety gain: the walk is already bounded at any of these values, and the difference between 10
  and 32 is not the difference between safe and unsafe. The ceiling exists to make the walk
  bounded, not to enforce a house style.
- **A per-key arity limit instead of, or in addition to, the node ceiling.** Rejected; see decision
  2. Per-key limits still permit an unbounded total via nesting, so they would add a rule without
  closing the hole.
- **Enforce only `loop.mode` and the `Forever` bound at depth, since that was the live defect.**
  Rejected. `retention.keepIterationRecords` reaches the same unbounded-status failure by another
  route, and every other schema declaration has the same standing below depth 1: none of them is
  applied. Fixing the one route found would leave the class open.
- **Require the defaulted keys at every depth, so a nested node is self-describing.** Rejected; see
  decision 5. It would make a nested `parallel` need keys a depth-1 one does not, which is a rule
  an author cannot learn from the schema they read.
- **Leave the evaluator's `default:` arm as unreachable-behind-validation.** Rejected. That is
  exactly what it was, and the fall-through it carried was the defect. Unreachable code on a path
  whose failure mode is "run forever" is cheap to make fail closed.
- **Express the recursion in the CRD schema itself.** Not available. Structural schemas cannot
  express true recursion past depth 1, and `loop` cannot be reached from CEL because the identifier
  is reserved. Both were verified rather than assumed.

## Consequences

**What a reader gains.** The rule an author reads at depth 1 is the rule that holds at depth 9. A
nested loop omitting `mode` is refused at admission with a message that names the problem, rather
than being admitted and run without a bound.

**What some objects lose.** A tree deeper than 32 levels or larger than 1024 nodes is refused,
including one a generator emits. The ceilings are constants in the source, so raising one is a code
change and not a configuration change. That is intended: both bound recurring control-plane work
that the submitter of an object chooses.

**The stored object is not self-describing below depth 1.** `join.mode`, `onJoin`,
`onBranchFailure` and the three `retention` fields are defaulted by the API server only on a node
that is a direct child of `spec.flow`. A nested node's stored form will not show them. The
evaluator substitutes the same values, so behaviour matches, and `crds/fleettask.md` states the
gap for authors who will otherwise read the stored object as the whole truth.

**New obligations.** Every constraint added to the CRD schema on a flow node must be added to
`validateNode` in the same change, or it silently applies at depth 1 only — which is the defect
class this ADR closes rather than a one-off. The same applies to any new recursion point: it must
be walked, or the subtree beneath it is unvalidated.

**Two enforcement points, one rule.** The webhook and the reconcile-time fallback call the same
function, so they cannot drift. A caller that validates a tree by any other route is outside this
guarantee.

**What is tested.** `internal/flow/validate_test.go`: a tree exactly at the depth ceiling is
admitted and one level past it is rejected with a message naming depth (`:162`); the same either
side of the node ceiling, with a message naming nodes (`:175`); and the node ceiling counts every
branch, so two branches each under it still trip it together (`:190`). At the admission boundary,
`internal/webhook/fleettask_flow_depth_test.go` rejects a tree past each ceiling with an `Invalid`
error (`:72`), a nested loop without `mode` (`:39`), and a nested unbounded `Forever` (`:58`).
