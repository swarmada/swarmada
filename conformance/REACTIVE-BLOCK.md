# Reactive Block Conformance

This document defines what an implementation of the **reactive tier** — a
`ReactiveBlock` dispatched with a `FleetAction` and executed on the adapter or
the edge node — must, should, and may do. Where this document and RFC-0001
differ in the interpretation of a shared term, **RFC-0001 governs**. The key
words MUST, MUST NOT, SHOULD, and MAY are used as in RFC 2119.

The party under test is a **fleet adapter** (or an edge node acting for one), so
this catalog's checks belong to the `FleetAdapter` surface and appear on a
`FleetAdapter` report.

**They are split across two profiles, and the split is load-bearing.**

| Profile | Level | Applies | Checks |
| :---- | :---- | :---- | :---- |
| `reactive-grammar` | **Core**, every tier | **always** | `RB0.1`, `RB2.7`, `RB3.5`, `RB3.6`, `RB3.7` |
| `reactive` | **Extended**, every tier | only when the adapter declares `reactive_grammar_version` | everything else in `RB1`–`RB6` |

`reactive-grammar` holds the checks that are properties of the **grammar** and of
this suite's **coverage** of it: is every construct covered, and does the grammar
offer a way to command motion, an open effect set, a control-plane predicate
source, or free-form cue content. Those answers are identical for every adapter,
including one that declines the reactive tier entirely — because an adapter that
declines it still ships into a fleet where the grammar is what it is. Folding
them into the conditional profile would make them vanish for exactly the adapters
that never implement the tier, which is most of them, and would leave the
guarantee unasserted in nearly every report that exists.

`reactive` is what the **adapter** is held to, and holding it never raises the
adapter's capability tier: none of the four effects commands motion, so a Read
Only adapter may host reactive blocks and remain Read Only.

The report-integrity rows `RB0.2`, `RB0.3` and `RB0.5` belong to the surface's
`attestation` profile, beside the adapter catalog's `C0.1`, because they are
statements about the report rather than about the reactive tier. `RB0.4` is
harness context and counts toward nothing.

## Why this profile is load-bearing

The reactive tier is **declared in the control plane and executed where the
control plane cannot watch it**: on the robot, at sensor rate, inside a
dispatched action, surviving a partition until its lease expires. Nothing
observes it in flight. The conformance profile is the only thing that makes that
tolerable.

So the profile is not written as a checklist chasing the grammar. The grammar is
enumerated machine-readably in
[`reactive-grammar.v1.yaml`](reactive-grammar.v1.yaml), every construct in it is
bound to a check by the coverage table below, and **`RB0.1` fails the run when
any construct has no check or any check names a construct that does not exist**.
Adding a construct without adding a check is a failing build. That is the
structural guard this tier needs, made mechanical rather than cultural.

> **Status:** the grammar is **specified and not implemented**. S5 is gated on
> physical hardware and has not run; there is no `spec.reactive` field in
> `api/v1`, no proto message, and no adapter in the tree that declares
> `reactive_grammar_version`. The grammar-level checks (`RB0.*`, `RB1.*`,
> `RB2.*`, `RB3.*`) run today against the manifest and are real results. The
> execution-level checks (`RB4.*`, `RB5.*`, `RB6.*`) require an adapter that
> declares the grammar and record `skip` with an enumerated cause until one
> exists. A skip is not a pass; the report says which is which.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## RB0 — Coverage and report integrity (self-checks on this suite)

- **RB0.1 (MUST)** **Every construct in the grammar manifest is bound to at least
  one check in the coverage table below, and every construct a check names
  exists in the manifest.** A grammar that grows a construct the profile does not
  cover fails this check, and the guarantee the reactive tier rests on becomes
  fiction the moment it is allowed to pass.
- **RB0.2 (MUST)** A skip reason must be consistent with what the run observed,
  and must name an enumerated cause rather than free text.
- **RB0.3 (MUST)** The run stands alone: every artefact it read resolves inside
  the public repository, and no profile, check id, check title or fixture path
  names a specification this suite cannot read.
- **RB0.5 (MUST)** Every check the registry names is defined in this document,
  and every check defined here appears in the registry.

### RB0.x — harness context

- **RB0.4 (context)** The `reactive_grammar_version` the adapter declared, and
  the manifest revision the run checked against. Harness context; it does not
  count toward conformance.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## RB1 — Block structure

- **RB1.1 (MUST)** Accept a block as `spec.reactive.rules[]` and dispatch it in
  the same `AssignAction` as its owning action. A block with no rule is not
  dispatched, and there is no path by which a block arrives separately from the
  action it belongs to.
- **RB1.2 (MUST)** Require a unique `name` per rule within a block. The name is
  how a fired rule is attributed (`RB5.2`); two rules sharing one make the
  attribution unrecoverable.
- **RB1.3 (MUST)** Accept exactly **one** predicate under `when`. There is no
  `and`, no `or` and no `not`: a reaction needing two conditions is two rules and
  an event, which keeps the evaluation cost bounded and the block readable
  without an evaluator.
- **RB1.4 (MUST)** Accept one or more effects under `then` and apply them **in
  the order written**. An implementation MUST NOT reorder, parallelise, or drop
  an effect it does not recognise — an unrecognised effect is a rejection
  (`RB3.5`), never a skip.
- **RB1.5 (MUST)** Require `cooldownMs` on every rule and reject a rule that
  omits it. It MUST NOT be defaulted. A rule evaluated at sensor rate with no
  minimum interval between firings is a fork bomb with an actuator on the end,
  and a default would put the bound in the implementation rather than in the
  block an operator can read.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## RB2 — The `when` side: predicates, closed to robot-local sources

- **RB2.1 (MUST)** Support a **stream** predicate: an aggregate over a named
  sensor stream, compared against a threshold, held for `forMs`. The `stream`
  MUST name a `streamId` the adapter declares in `CapabilitiesSnapshot`.
- **RB2.2 (MUST)** Require `forMs` on every stream predicate and reject one that
  omits it. Debounce is mandatory and MUST NOT be defaulted: a proximity
  threshold with no hold fires on a single noisy sample.
- **RB2.3 (MUST)** Support a **computeResult** predicate over a field of a
  **local** executor's result — an executor the robot's own adapter hosts.
- **RB2.4 (MUST)** Support a **computeEvent** predicate over a semantic event
  raised by a **local** executor.
- **RB2.5 (MUST)** Support exactly the aggregates the manifest enumerates
  (`min`, `max`, `mean`, `count`) and reject any other. An unrecognised aggregate
  is a rejection, never a fallback to `min`.
- **RB2.6 (MUST)** Support exactly the comparisons the manifest enumerates
  (`lt`, `lte`, `gt`, `gte`) and reject any other.
- **RB2.7 (MUST NOT)** Match on a control-plane-delivered event or on
  control-plane state. **This is the whole safety argument**. With a
  downward event path, a `TaskTrigger` could reach a robot-local effect that ran
  with no admission, no scheduling, no reservation and no lease check — each step
  defensible, the invariant collapsing between them. Extending the `when`
  vocabulary to control-plane state is a spec change with an RFC and a
  conformance test, never a convenience.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## RB3 — The `then` side: exactly four effects

- **RB3.1 (MUST)** Support `activateStream` — a declared stream, at a bounded
  `rateHz`, for a bounded `holdMs`. An activation MUST NOT be held open
  indefinitely by a rule.
- **RB3.2 (MUST)** Support `invokeExecutor`, naming a class the **robot's own
  adapter** declares it can host. A reactive rule cannot reach off the robot; an
  `executorRef` resolving to a cluster endpoint is a rejection.
- **RB3.3 (MUST)** Support `raiseEvent`. It is the only effect by which a
  reaction can bear on where the robot goes, and it moves nothing itself: a
  decision that moves the robot is the control plane's alone. At this contract
  version no path carries the event to the control plane (`RB5.1`), so a fired
  `raiseEvent` is observable as a fired rule (`RB5.2`) and in no other way.
- **RB3.4 (MUST)** Support `playCue` with a **declared cue id** and nothing else.
- **RB3.5 (MUST)** Reject an effect key outside those four, at admission and at
  dispatch. There is no extension point, no `custom`, and no passthrough.
- **RB3.6 (MUST NOT)** Provide any effect that commands motion. `navigate`,
  `stop`, `setSpeed`, `setPose`, `goTo`, `dock`, `undock`, `move` and
  `cancelMotion` are absent from the grammar and MUST be rejected by name.
  Principle 2 — one authority for motion — is enforced by the **shape of the
  grammar**, not by a rule someone remembers, and this check is the assertion
  that the shape still holds. It is verified against the manifest, not by
  inspection.
- **RB3.7 (MUST NOT)** Accept free-form content on `playCue`. There is no `text`,
  `ssml`, `uri`, `body` or `script` field, and one must never be added: a cue id
  resolves to something an administrator installed, and free-form content would
  make a model's output audible in a facility with nothing in between.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## RB4 — Lifecycle and grammar versioning

- **RB4.1 (MUST)** Execute a block for exactly as long as its owning action, and
  tear it down on the action's terminal transition, its cancellation, or its
  lease expiry. A block that outlives its action is an unowned sense→effect loop
  with no lease behind it.
- **RB4.2 (MUST)** Stop evaluating on lease expiry, exactly as the action itself
  self-stops. The safe direction for a reactive block is **off**.
- **RB4.3 (MUST)** Declare `reactive_grammar_version` on `AdapterHello`, and
  execute only constructs that version covers. An adapter MUST NOT silently
  ignore a construct it does not recognise; the control plane refuses to dispatch
  a block using a construct the declared version does not cover (`CP6.1`), and an
  adapter that accepts one anyway has removed the gate's only enforcement point.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## RB5 — Observability upward

- **RB5.1 (MUST)** Open no event channel of its own for a `raiseEvent` effect. A
  reactive event travels only on a path the contract defines for semantic events,
  and at this contract version the adapter surface has none: no field of
  `ActionStatusUpdate` carries a `TaskEvent` (`fleet_adapter.proto`, *Semantic
  events*), and carrying one is outside this contract version. Until a path exists,
  a fired `raiseEvent` is observable as a fired rule (`RB5.2`) and in no other way.
  A reactive event is not a second event channel.
- **RB5.2 (MUST)** Make a fired rule observable: which rule fired, and when. The
  block itself is declared rather than opaque, and a tier that cannot be watched
  in flight must at least be reconstructable afterwards.
- **RB5.3 (MUST NOT)** Consume any downward event path. An adapter MUST NOT
  accept, and the control plane MUST NOT provide, a channel by which a
  control-plane event reaches a reactive predicate. `RB2.7` closes the grammar;
  this closes the transport, and both are needed — a closed grammar with an open
  transport is one field away from the hole this section describes.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## RB6 — Local cues

- **RB6.1 (MUST)** Honour `SwarmadaConfig.spec.reactive.allowLocalCues: false` by
  refusing to execute `playCue`. The same use case then runs a round trip slower,
  which is the point: an operator who disagrees with the tier-2 cue decision can
  turn it off without losing the use case.
- **RB6.2 (MUST)** Resolve a cue id to a declared robot capability, and emit
  nothing for an undeclared id. `playCue` is the acknowledged soft spot — an
  actuator with no control-plane transition behind it — permitted only because
  sound is not motion and is reversible. Both halves of that justification are
  properties of the implementation, and this check is where they are asserted.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## Grammar coverage table

`RB0.1` reads this table. Every construct id in
[`reactive-grammar.v1.yaml`](reactive-grammar.v1.yaml) MUST appear exactly once
in the left column with at least one check, and every check named MUST exist in
the registry. Edit the grammar without editing this table and the run fails.

<!--rb-coverage-->

| Construct | Checks |
| :---- | :---- |
| `block.rules` | RB1.1 |
| `rule.name` | RB1.2 |
| `rule.when` | RB1.3 |
| `rule.then` | RB1.4 |
| `rule.cooldownMs` | RB1.5 |
| `predicate.stream` | RB2.1, RB2.2 |
| `predicate.computeResult` | RB2.3 |
| `predicate.computeEvent` | RB2.4 |
| `aggregate.min` | RB2.5 |
| `aggregate.max` | RB2.5 |
| `aggregate.mean` | RB2.5 |
| `aggregate.count` | RB2.5 |
| `comparison.lt` | RB2.6 |
| `comparison.lte` | RB2.6 |
| `comparison.gt` | RB2.6 |
| `comparison.gte` | RB2.6 |
| `effect.activateStream` | RB3.1 |
| `effect.invokeExecutor` | RB3.2 |
| `effect.raiseEvent` | RB3.3, RB5.1 |
| `effect.playCue` | RB3.4, RB6.1, RB6.2 |
| `prohibition.motionEffect` | RB3.6 |
| `prohibition.controlPlaneSource` | RB2.7, RB5.3 |
| `prohibition.freeFormCue` | RB3.7 |
| `prohibition.unboundedRule` | RB1.5, RB2.2 |
| `prohibition.openEffectSet` | RB3.5 |

<!--/rb-coverage-->

## Passing

An adapter holds the `reactive` profile when it declares
`reactive_grammar_version` and satisfies every check in `RB1`–`RB6` for the
grammar version it declares. Declining it — declaring no grammar version and
executing no block — is fully conformant at every tier, and an adapter that does
so is not measured against those checks at all.

`reactive-grammar` is a different matter: it is Core, it always applies, and no
adapter can decline it, because its subject is the grammar rather than the
adapter. A failure there is a defect in the specification or in this suite, not
in the implementation under test — and that is precisely why it must appear in
every report rather than be skipped in most of them.

## Self-certification

Self-certified on the same terms as every other Swarmada profile (ADR-0007): the
harness produces the result, the implementer publishes it, and there is no
certification authority, no gate, and no fee. A published claim MUST state the
grammar version the result was earned against — a `reactive` result carries no
meaning without it, because the grammar is exactly what was measured.
