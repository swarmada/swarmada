# ADR-0012: Debounce RobotProbe status with pointer-typed thresholds

- **Status:** accepted
- **Date:** 2026-07-09
- **Deciders:** API Designer, Distributed Systems Reviewer, Test Engineer
- **Related:** RFC-0001 §9.1.6 (active health probing), §9.1.11 (SwarmadaConfig
  health tunables), ADR-0011 (connectivity-critical condition), the RA-1
  status-write discipline

## Context

A `RobotProbe` runs an active health check on a set of robots and records the
outcome in `status`. Three pieces of the intended design are defined but not
connected:

1. **`RobotProbeSpec.FailureThreshold` (default 3) and `RecoveryThreshold`
   (default 2) are dead config.** `aggregateProbeStatus` reports the *raw* worst
   per-cycle status immediately (one failing cycle ⇒ `lastProbeResult: Degraded`)
   and increments `status.consecutiveFailures`, but never compares that streak to
   any threshold. A single transient RPC blip flaps the probe result. The whole
   point of the thresholds — debounce — is missing.
2. **`SwarmadaConfig.spec.health.defaultProbeFailureThreshold` /
   `defaultProbeRecoveryThreshold` (A3/A4) are unwired**, and cannot be wired
   cleanly today: the per-probe fields are non-pointer `int32` with
   `kubebuilder:default` values, so the API server stamps 3/2 onto every probe
   that omits them. The controller therefore cannot distinguish "operator left it
   unset (use the namespace default)" from "operator chose 3" — the same
   unset-vs-zero ambiguity that blocked A5. A namespace default over a
   kubebuilder-defaulted field is unresolvable.
3. **There is no persisted success streak**, so recovery debounce (return to
   Healthy only after N consecutive successes) has nothing to count against.

Constraints: the resolution must respect the API-Designer guardrail ("pointers for
optional scalars"), stay RA-1-safe (status writes only on material change, no
per-tick sink), and keep `kubectl get robotprobe` readable.

## Decision

Implement probe-status debounce, sourced from thresholds resolved through a clean
three-level fallback, and make the per-probe thresholds optional pointers so that
fallback is expressible.

1. **Type change (api/v1).** `RobotProbeSpec.FailureThreshold` and
   `RecoveryThreshold` become `*int32` with the `kubebuilder:default` removed
   (they are already `omitempty`). Unset now means nil — distinguishable from an
   explicit value. This also brings them into line with the optional-scalar
   guardrail.
2. **Threshold resolution order** (per probe, per reconcile): the per-probe
   `*int32` if non-nil and positive → else
   `SwarmadaConfig.spec.health.defaultProbe{Failure,Recovery}Threshold` if
   positive → else the built-in constants (3 / 2). This wires A3/A4 without
   ambiguity, via the shared `namespaceConfig()` fail-safe helper.
3. **Debounce semantics.** `status.lastProbeResult` becomes the *effective*
   (debounced) status: it flips Healthy→Degraded/Failed only after
   `failureThreshold` consecutive failing cycles, and Degraded/Failed→Healthy only
   after `recoveryThreshold` consecutive Healthy cycles. Raw per-robot outcomes
   stay in `status.robotResults` (undebounced), so nothing is hidden.
4. **New status field.** Add `status.consecutiveSuccesses int32` to mirror the
   existing `consecutiveFailures`, giving recovery debounce a counter to persist
   across reconciles. Both counters ride the existing DeepEqual material-change
   guard (RA-1: no extra write, never on an unchanged cycle).

## Alternatives considered

- **Keep the per-probe fields non-pointer, wire the namespace default only when the
  field == 0.** Rejected. `kubebuilder:default=3/2` means the API server never
  stores 0, so the branch is dead; and dropping the default while keeping the
  non-pointer type would silently reinterpret a legitimate stored 0. Pointers are
  the correct model for "optional, no default."
- **Delete the SwarmadaConfig namespace-default fields as vestigial.** Rejected.
  Namespace-wide defaults are genuinely useful (set the fleet's debounce once), and
  removing published fields is a breaking change for no benefit once pointers make
  them wireable.
- **Debounce without a new status field** (e.g. encode successes as negative
  `consecutiveFailures`). Rejected as a false economy — it overloads one field with
  two meanings and confuses `kubectl` output. An explicit `consecutiveSuccesses` is
  clearer and cheap.
- **Leave `lastProbeResult` as the raw per-cycle worst and add a separate
  `effectiveResult`.** Rejected. Two result fields invite consumer confusion about
  which is authoritative; the debounced value is what operators and downstream
  health aggregation should read, so it belongs in the existing, printed field.

## Consequences

- **Good.** The thresholds finally do something; a transient blip no longer flaps
  the probe result. A3/A4 wire cleanly through the same fail-safe pattern as the
  other health tunables, and the optional-scalar guardrail violation is corrected.
- **Obligation — schema regen.** This is an `api/v1` change: run
  `make generate manifests`. The pointer change is not wire-breaking (a stored `3`
  unmarshals into `*int32`; omitted becomes nil), but new objects no longer get an
  auto-stamped 3/2 — the controller's built-in constant supplies the same effective
  default, so behavior is preserved.
- **Compatibility note — validation tightening.** Adding `Minimum=1` while removing
  the `+kubebuilder:default=3/2` is technically a tightening (api-principles calls
  tightening breaking): a stored object with an explicit `0` would be rejected on the
  next apply. Risk is negligible — `0` is a nonsensical threshold the old default
  stamped away, so no valid stored object holds it — but it is called out here as the
  point of record. This also motivates the api-principles "Defaults belong in the
  schema" exception documented for multi-level-resolution fields.
- **Obligation — tests.** Debounce needs table tests: N-1 failing cycles hold
  Healthy, the Nth flips to Degraded, M-1 Healthy cycles hold Degraded, the Mth
  returns to Healthy; plus the resolution order (per-probe > namespace > constant)
  and the fail-safe.
- **Behavior change (intended).** `lastProbeResult` now lags raw outcomes by up to
  `failureThreshold`/`recoveryThreshold` cycles. This is the desired debounce, but
  any consumer that expected instantaneous flips must be aware. Documented here as
  the point of record.
- **Drawback accepted.** A robot that is genuinely failing is reported Healthy for
  up to `failureThreshold-1` extra cycles. That is the explicit trade the threshold
  exists to make; operators tune it per probe or per namespace. Safety-critical
  fast-fail is out of scope for active probing and remains the estop/lease path.
