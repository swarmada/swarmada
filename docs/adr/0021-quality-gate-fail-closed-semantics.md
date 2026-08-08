# ADR-0021: Quality-gate evaluation is fail-closed on missing and simulation-only metrics

- **Status:** accepted
- **Date:** 2026-07-23
- **Deciders:** API Designer, Security Reviewer, Test Engineer
- **Related:** RFC-0001 §9.1.9 (ModelPolicy quality gate), ADR-0020 (model artifact integrity), ADR-0012 (probe-status debounce)

## Context

A `ModelPolicy` quality gate blocks a trained model from reaching the fleet
unless reported metrics satisfy its thresholds. Metrics arrive as
`ReportedMetrics` (`map[string]float64`); gate rules reference metric keys by
free string (the built-in `minPickSuccessRate`, `maxFailureRate`,
`maxSimToRealGap`, and arbitrary `customMetrics[].name`). The type documents that
"ALL rules must pass" for the gate to pass, but two behaviors at the edge of that
statement are undefined, and both fail open on a safety mechanism.

1. **Missing metric.** If a configured rule references a key that is absent from
   the payload — a typo, a renamed metric, a pipeline change — the type does not
   say whether that rule fails or is skipped. If it is skipped, the rule silently
   protects nothing while the gate still reports a pass. "ALL rules must pass" is
   ambiguous for an input that is not present.

2. **Simulation-only evaluation.** `maxSimToRealGap` is validated "only when both
   `simPickSuccessRate` and `realPickSuccessRate` are reported." A model
   evaluated only in simulation therefore skips the sim-to-real guard entirely —
   the guard disappears exactly when there is no real-world data, which is when
   it matters most.

Both are the same fail-open shape on the gate whose purpose is to keep an
under-performing model off physical robots. The forces in tension:
simulation-first workflows report simulation-only metrics as the normal case, so
the safe default must not make simulation iteration impossible; free-form metric
keys (`customMetrics`) are deliberately flexible and should stay that way; and a
pluggable evaluation-provider interface (a future extension point that lets an
external evaluation system compute the metrics) could reintroduce whatever
semantics this ADR does not pin down.

## Decision

The quality gate evaluates **fail-closed**.

- A configured rule — built-in or custom — whose referenced metric is absent
  from the reported payload evaluates to **FAIL**, and the missing key is named
  in `status.…FailedRules`. Absence is never treated as a pass.
- A gate-key/payload-key mismatch is surfaced as a distinct
  `MetricSchemaMismatch` condition on the ModelPolicy status, separate from an
  ordinary quality Reject, so an operator can distinguish "the model genuinely
  underperformed" from "your metric names do not line up."
- When `maxSimToRealGap` is set, real-world evaluation metrics are a **required**
  input: a simulation-only report FAILs (surfaced as a Reject with a named
  reason). An explicit `requireRealEval` field governs this and defaults to
  `true` whenever `maxSimToRealGap` is set; a simulation or development namespace
  may set it `false` to permit simulation-only promotion. The unsafe mode is
  opt-in and named, never the default.
- A pluggable evaluation-provider interface, if and when added, inherits these
  semantics: a provider MUST map an absent or insufficient metric to FAIL and
  MUST NOT redefine absence as skip.

## Alternatives considered

- **Skip-if-absent.** The most permissive reading, and the least surprising for
  partial pipelines, but it silently disables safety rules — the exact fail-open
  the gate exists to prevent. Rejected.
- **Fail-closed with no schema feedback.** Safe, but opaque: an operator whose
  metric is named `pick_success_rate` while the gate expects `pickSuccessRate`
  sees only "Reject" and may conclude the gate is broken and work around it.
  Rejected in favor of adding the `MetricSchemaMismatch` condition.
- **Always require real metrics, with no escape hatch.** Safest in isolation,
  but it makes promotion impossible in a pure-simulation namespace and breaks
  simulation-first development outright. Rejected in favor of an explicit, named
  opt-out.
- **Strongly-typed metric schema (enumerate allowed keys in the CRD).** Gives
  clean validation, but removes the `customMetrics` flexibility and couples the
  CRD to a fixed model-metric vocabulary — against the neutral-standard goal of
  not baking model-domain semantics into the API. Rejected; keep free-form keys,
  add fail-closed evaluation and mismatch surfacing instead.

## Consequences

- The gate can no longer pass by omission. The safety mechanism is sound against
  typos, pipeline drift, and simulation-only evaluations.
- New obligations. The controller treats an absent configured metric as FAIL and
  populates `FailedRules` / the `MetricSchemaMismatch` condition; a
  `requireRealEval` field is added (a CRD change — show the spec/diff and confirm
  before editing existing types, then `make generate manifests` and tests); the
  documentation states the simulation-only opt-out explicitly. Any future
  evaluation-provider interface carries a contract test that an absent metric
  maps to FAIL.
- Accepted drawback: partial or experimental pipelines will see more Rejects
  until their metric names and real-evaluation coverage line up. This friction is
  intended and is made debuggable by the `MetricSchemaMismatch` condition and the
  named opt-out.
- Leaves a seam: the metric-to-artifact attestation from ADR-0020 layers on top
  of these semantics — requiring that the metrics' provenance matches the
  deployed artifact — without changing how the gate itself evaluates.

## Follow-up test obligations

- A configured rule whose metric is missing from the payload produces a Reject
  naming the missing key.
- A simulation-only report is rejected when `requireRealEval` is in effect, and
  is promoted when the opt-out is explicitly set.
- A gate-key/payload-key mismatch raises the `MetricSchemaMismatch` condition
  rather than silently passing.
