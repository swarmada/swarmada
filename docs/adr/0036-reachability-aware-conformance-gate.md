# ADR-0036: The Required-Events/conformance gate must assert reachability, not just reference

- **Status:** proposed
- **Date:** 2026-08-05
- **Deciders:** _(pending)_
- **Related:** ADR-0033 (surfaced the drift), ADR-0032 (contract versioning and conformance gating), ADR-0007 (conformance self-certification), RFC-0001 §9.6.5.1 (Required Events)

## Context

The mechanical Status check for Required Events (§9.6.5.1) verifies that a production
writer **references** an event constant. It does not verify that the writer is
**reachable** — that a real code path can actually fire it. ADR-0033 found two audit
rows (`MODEL_UPDATE_SUCCEEDED`, `MODEL_UPDATE_FAILED`) marked `emitted` while being
provably unreachable: nothing in production wrote the `Active`/`Failed`/`runningVersion`
state they key on, so the events could never fire. As that ADR put it, the column
"drifted again, in a way the existing gate is blind to."

A gate that a dangling reference can satisfy gives **false assurance**: it lets an
incomplete design ship marked done. That is the systemic cause behind ADR-0033, not
just its symptom — review missed it twice, and the gate is meant to be the backstop.

## Decision

_(Pending — this is a proposal stub.)_ Proposed direction: strengthen the
Required-Events / conformance gate so a row may be marked `emitted` **only** when its
writer is demonstrably reachable — for example an exercised test that observes the
event fire, or static reachability analysis — not merely that the constant is
referenced. Where reachability cannot be established mechanically, the gate must
record its own limitation where a reader will see it, rather than reporting a false
pass.

## Alternatives considered

- **Leave the gate as-is and rely on review.** Rejected: review already missed this
  twice; the gate exists precisely to be the backstop review is not.
- _(Further options to be enumerated during design.)_

## Consequences

_(To be completed during design.)_ A reachability-aware gate closes the
`emitted`-but-unreachable drift class; the cost is a stronger check (test-observed
events or analysis) and re-validating existing `emitted` rows against the higher bar.
