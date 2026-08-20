# ADR-0045: A `desiredState` hold is entered declaratively and lifted only by an operator

- **Status:** accepted
- **Date:** 2026-08-20
- **Deciders:** Al — the operator is the only party who may lift a hold. `spec.desiredState: Paused`
  and `Returning` enact the hold; writing `Running` back does **not** resume the action
- **Related:** RFC-0001 §9.1.4 (`FleetAction.spec.desiredState`), §9.1.5 (`FleetTask` fan-out),
  §9.6.2.4 (FleetAction behaviour during estop — the operator-gated `Paused` phase),
  §9.6.3.5 (assignment lease and the single-executor guarantee); ADR-0007 (level-triggered
  declarative intent); Audit 3 finding D3

## Context

`FleetAction.spec.desiredState` is documented as level-triggered declarative intent: a write persists
and re-converges after a disconnect, and re-writing the same value is idempotent, so a dropped edge is
never a lost command (ADR-0007). Four values are defined — `Running`, `Paused`, `Returning`,
`Cancelled` — and `FleetTask` fans its own copy of the field onto every non-terminal child.

Audit 3 found that no controller read the field at all. `Cancelled` has since been wired into the
existing confirmed-cancel path, which closed the composite's FailFast and Compensate cancellation.
`Paused` and `Returning` were left unenacted for one reason: enacting them requires answering a
question the specification had not answered, and answering it wrongly weakens a safety property.

The question is what a subsequent write of `Running` means. Read as strictly level-triggered, it
resumes the action. But §9.6.2.4 already defines a `Paused` phase whose defining property is that the
control plane **never** auto-resumes it — an estop-paused action waits for a human, because the reason
the robot stopped is not visible to the control plane. Two paths into the same phase with opposite
resume rules would mean an operator could not tell, from a `Paused` FleetAction, whether it would move
on its own.

The interaction is worse than a documentation problem. The estop-pause check runs before any
`desiredState` reconciliation, so an action holding `Running` under a live estop would be re-paused on
every reconcile — a loop between two intents, on the safety path.

## Decision

**A hold is entered declaratively and lifted only by an operator.**

- `spec.desiredState: Paused` enacts a safe hold, taking the same transitions §9.6.2.4 specifies for
  the estop path: an `Assigned` action releases its robot binding and its zone slot; an `InProgress`
  action keeps its robot bound and its lease renewed, because that robot is physically committed and
  no other robot may take the action.
- `spec.desiredState: Returning` enacts an adapter-confirmed recovery and then holds, arriving in the
  same `Paused` phase.
- **Writing `Running` back onto a held action does not resume it.** The action stays `Paused` until an
  operator resumes, requeues, or cancels it through the verb-gated intake.

`desiredState` is therefore level-triggered in one direction. That asymmetry is deliberate and is
stated in the field's own documentation, so a client cannot infer symmetry from the word
"declarative".

## Consequences

- One resume rule for every `Paused` action, whatever stopped it. An operator reading
  `phase: Paused` never has to ask which path produced it before deciding whether to intervene.
- No intent loop on the safety path: `Running` cannot contend with the estop-pause check, because
  `Running` does not act on a held action at all.
- A composite that pauses its children cannot un-pause them by writing `Running` at the task level.
  Resuming a paused composite is a per-child operator action. This is a real ergonomic cost and is the
  price of the property above; a supervised bulk-resume verb is the place to address it, not the
  level-triggered field.
- `Cancelled` remains the one value that is fully level-triggered in both directions, because
  cancellation is terminal — there is nothing to lift.
