# ADR-0063: A declared field that nothing evaluates is absent, not inert

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, API Designer
- **Related:** ADR-0019 (adapter action discovery and validation), ADR-0062 (a semantic event produces a draft FleetTask), ADR-0064 (composite-zone validity splits across CEL and the webhook), RFC-0003 §6.10; `api/v1/tasktrigger_types.go`

## Context

Across WS-B, three independent measurements found the same failure shape: **a field or
a flag that an operator can set, that is accepted, and that nothing reads.** It is
worse than a missing feature, because a missing feature is legible and this is not —
the apply succeeds, the read-back looks right or looks empty, and the operator
believes a scope, a fault or a limit is in force when none is.

The three measurements:

1. **A write that the API server silently prunes.** (B4, 2026-09-15.) RFC-0001 #fleetzone-composite-zones-and-zone-expansion
   specified `FleetZone.spec.memberZones` in full while repeating, on nearly every
   paragraph, that it was *"specified, not in the v0.4 schema — a write to this path is
   pruned by the API server"*. An operator applying a composite zone got a **success
   response** and a zone with **no members**, and the loss was observable neither from
   the response nor from a read-back. The envtest written for the fix
   (`TestEnvtest_FleetZoneMemberZonesRoundTrip`) asserts the **`Get`**, not the write,
   because the write always passed.

2. **A flag that parses and is never read, measured on hardware.** (2026-09-15, on a
   physical MentorPi.)
   `adapter.py:682-705` constructs `ScenarioEngine` and `HardwareFaultOverrides` only
   in the `else` arm that builds `SimulatedRobot`. The `ros2` arm constructs
   `Nav2Binding()` and nothing else, and `Nav2Binding` has no capability-fault concept.
   So `--fault-component`, `--fault-at` and `--recover-at` **parse and are never
   read** — an operator running the UC-2.2 degrade arc against a real robot gets a
   clean run and no fault. It is the same shape as an earlier silently-inert bring-up
   flag in the same adapter, which is the point: this recurs.

3. **A field the tree has no evaluator for.** (B5, 2026-09-15.) RFC-0003 §6.10
   specifies `TaskTrigger.spec.condition`, a CEL expression that *narrows* what fires.
   **This tree vendors no CEL evaluator.** A `condition` field that nothing evaluated
   would mean: the operator writes a scope, the apply succeeds, and the trigger fires
   on **every** event regardless. The same reasoning covers `paramsFrom` payload
   substitution, where a half-done version would silently drop unmapped fields and
   produce a task that looks parameterised and is not.

The forces in tension are real. Declaring the field early keeps the schema aligned
with the specification, avoids a later API addition, and lets operators write manifests
against the intended shape. Against that: every one of the three cases above is a
*safety-adjacent* misreading, because what the operator believed was narrowed,
faulted or grouped was in fact open, healthy or empty.

## Decision

**Where this tree cannot evaluate a specified field, the field is omitted from the
schema rather than accepted and ignored, and the type comment says why.**

- `TaskTrigger.spec.condition` and `paramsFrom` are **absent**, with the absence
  documented on the type.
- Where a field must exist before its consumer does — because the schema half and the
  controller half are owned by different changes — **the gap is recorded as an open
  item against the consumer, and the field's own documentation states that an empty
  value is not a claim**. `SupportedAction.results[]` is the worked example (ADR-0065).
- A deliberate narrowing that predates the fields it now interacts with is **named as
  a decision to revisit, not swept in**. The CRD `ActionParam` mirror omits `min`/`max`
  as an intentional v0.1 coarse projection; that predates there being an output
  descriptor at all, so it is recorded as an option against the projection gap in
  ADR-0065 rather than quietly changed under an unrelated change.

An honest gap is an absent field plus a stated reason. An inert field is a gap that
lies.

## Alternatives considered

- **Declare the field and ignore it until the evaluator lands.** Rejected — it is
  precisely measurement 3's failure, and measurements 1 and 2 are what it looks like
  in production: a success response over a silent no-op.
- **Declare the field and reject any manifest that sets it.** Genuinely defensible:
  the shape is reserved, and setting it is a loud admission error rather than a silent
  one. Rejected for `condition` because a rejection message that says "not implemented"
  on a field the specification documents is more confusing to an operator than the
  field not existing, and because the CRD then carries a field whose only behaviour is
  to fail — which must itself be removed later, a second API change rather than none.
  Reconsider for a field whose shape is genuinely frozen and whose consumer is
  imminent.
- **Ship a partial evaluator** (substring matching for `condition`, best-effort
  mapping for `paramsFrom`). Rejected: a scope that narrows *some* of what the operator
  wrote is the hardest variant to debug, because it works in the demo and fails on the
  case nobody tried.
- **Rely on documentation to warn operators.** Rejected. RFC-0001 #fleetzone-composite-zones-and-zone-expansion *did* warn,
  on nearly every paragraph, that `memberZones` was pruned — and the field still had
  to be implemented to stop the silent loss. A warning in prose does not survive
  contact with an apply that returns success.

## Consequences

- A real functional gap against RFC-0003 §6.10 is now visible in the schema: a trigger
  cannot be scoped to one vendor or one zone (ADR-0062). This is the intended trade —
  the gap is legible instead of misleading.
- Adding these fields later is an **additive** API change, which the project's
  versioning policy permits without a new version. The cost of deferral is therefore
  bounded; the cost of an inert field is not.
- The rule creates an obligation on reviewers: a new spec field arriving without its
  consumer must come with either the consumer, a stated reason and an item against the
  consumer, or a rejection rule. It is not enough for the field to round-trip.
- The rule is asymmetric by design. It restrains *spec* (operator intent, where a
  silent no-op misleads) more than *status* (derived, where an empty block reads as "not
  computed yet"). `FleetZone.status.composition` is schema-present and written by no
  controller today; that is acceptable under this ADR because nothing an operator wrote
  is being discarded.
- It does not license removing a field that is already published. This governs what is
  added; removal is a breaking change and is governed by the versioning policy.
