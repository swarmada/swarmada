# ADR-0047: Control intent is level-triggered declarative state, not an edge-triggered command

- **Status:** accepted
- **Date:** 2026-09-01
- **Deciders:** Maintainers
- **Related:** RFC-0001 "FleetAction" (`spec.desiredState`), "FleetTask" (fan-out);
  ADR-0006 (the north side is the Kubernetes API); ADR-0045 (a `desiredState` hold is lifted only by
  an operator); RFC-0003 alternatives B

## Context

**This ADR records a principle the project has relied on since RFC-0001 without ever writing it
down.** It is dated today rather than backdated, because no evidence was found that the decision was
taken separately at any earlier point; what exists is four documents citing it as though it had been.

The principle is normative in RFC-0001, where `FleetAction.spec.desiredState` is specified as
level-triggered declarative intent: a write persists and re-converges after a disconnect, and
re-writing the same value is idempotent, so a dropped edge is never a lost command.

Four places cite that rationale, and **all four cited ADR-0007**, whose subject is conformance
self-certification:

- `api/v1/fleetaction_types.go:89`
- `api/v1/fleettask_types.go:157`
- `docs/adr/0045-desiredstate-hold-is-operator-lifted.md` — its `Related:` line, which is where the
  attribution appears to originate
- RFC-0003 `front/non-goals.md` item 5 and `alternatives.md` option B

A fifth was worse than a miscitation: RFC-0003's `references.md` carried a bibliography entry reading
*ADR-0007, "External triggers act by writing desired state, not by commanding robots"* — a title no
ADR carries. `grep -rln "external trigger" docs/adr/*.md` returns nothing, and no ADR title contains
"desired", "trigger" or "command" except ADR-0024 (probe kill-switch) and ADR-0045.

So the belief that an ADR decided this was itself propagated by citation, without anyone reading the
target. That is the shape of the defect, and the reason this record now exists.

## Decision

**Control intent in Swarmada is level-triggered declarative state. A control action MUST NOT be
expressed as an edge-triggered imperative command.**

Concretely:

- Intent is written as desired state (`FleetAction.spec.desiredState`, `FleetTask.spec.desiredState`)
  and reconciled toward. A write persists; re-writing the same value is idempotent.
- A dropped, duplicated or reordered message MUST NOT change the outcome. Delivery is at-least-once;
  effect is convergent.
- Nothing in the control plane may offer a path by which an external actor causes a robot effect
  **without passing through admission, scheduling, deconfliction, the assignment lease and the audit
  trail.** The bounded declarative response set is how this is enforced rather than only stated:
  an effect cannot bypass machinery it has no verb to bypass it with.

This does not mean a trigger cannot cause a robot to act — it can, through `appendAction` and
`setDesiredState`. It means the effect travels the same path an operator's would.

## Consequences

- The six properties the path provides are preserved: the scheduler may **refuse**; the traffic
  deconfliction engine **serialises** simultaneous intents at shared resources; intent **persists**
  across a disconnect; every effect is **attributable**; it is **revocable** through the
  confirmed-cancel path; and it stays **coherent with e-stop**, where a held action needs an
  operator-gated resume.
- ADR-0045 builds directly on this: it decides that a hold entered declaratively is lifted only by an
  operator. That asymmetry is a *refinement* of this principle, not the principle itself — which is
  why citing ADR-0045 for the premise would also have been wrong.
- RFC-0003's alternative B rejects edge-triggered imperative commands on these grounds.
- A future proposal to add an imperative command path — or a downward event channel feeding a
  robot-local reactor, which reaches the same place by a longer route — is a proposal to
  supersede this ADR, and should be argued as one.

## Alternatives considered

- **Cite RFC-0001's `desiredState` section and write no ADR.** The rationale is normative there, and
  a specification outranks a decision record. Rejected as insufficient rather than wrong: RFC-0001
  states the requirement, and the reasoning — why an edge is unsafe here — belongs where reasoning is
  looked for. Four documents citing a nonexistent record is evidence the reasoning was wanted.
- **Backdate to the original RFC-0001 decision.** Rejected. No evidence was found that a separate
  decision was taken, and inventing a date would be the same class of error this record exists to
  correct.
- **Repoint everything at ADR-0045.** Rejected: ADR-0045 decides how a hold is *lifted*, and cites
  ADR-0007 for this premise exactly as the others do.
