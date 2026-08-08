# ADR-0016: Resolve estop delivery tuning per namespace in the dispatcher; implement partial-delivery response

- **Status:** accepted
- **Date:** 2026-07-09
- **Deciders:** Security Reviewer, Distributed Systems Reviewer, API Designer
- **Related:** RFC-0001 §9.1.11.8 (estop delivery config), §9.6.2 (confirmed estop
  delivery over SafetyStream), the RA-1 discipline

## Context

`SwarmadaConfig.spec.estop` carries delivery tuning that nothing reads:

- **`delivery.perAdapterTimeoutMs`** (default 500) — max wait for one adapter to ACK
  before retry.
- **`delivery.retryPolicy.{maxAttempts,retryIntervalMs}`** (0 / 250) — redelivery
  bounds.
- **`delivery.partialDeliveryBehavior`** (`BlockNewTasks` | `Alert`) — response when
  some adapters in a zone have ACKd an estop and others have not.

Two obstacles:

1. **The dispatcher is a config-free shared singleton.** `safety.New(client,
   recorder)` is built once in `main.go` with hardcoded timeouts
   (`defaultSLAThreshold 500ms`, `defaultConfirmTimeout 10s`, `defaultDeliveryTimeout
   2s`). Estop config is per namespace, so the singleton must resolve config
   *per estop event* by namespace — it already holds the client to do so.
2. **The field→timeout mapping is not 1:1.** The dispatcher has three timeouts; the
   CRD names one (`perAdapterTimeoutMs`). Wiring requires pinning which dispatcher
   timeout the field governs.
3. **`partialDeliveryBehavior` is entirely unimplemented** — there is no partial-
   delivery detection or response anywhere in the estop path.

Because estop is safety-critical, the resolution must fail safe: an unreadable config
must never lengthen or weaken delivery beyond the built-in defaults.

## Decision

1. **Resolve delivery tuning per namespace, per event.** The dispatcher gains a
   config lookup (via the shared `namespaceConfig()` helper) keyed on the estop's
   namespace, computed once per estop episode and threaded into that episode's
   delivery loop. Fail-safe to the current constants on any unreadable/absent config.
2. **Pin the mapping.** `perAdapterTimeoutMs` → the per-adapter ACK wait before
   retry (today's `deliveryTimeout`). `retryPolicy.maxAttempts` /
   `retryIntervalMs` → the redelivery loop's ceiling and inter-attempt delay.
   `confirmTimeout` and `slaThreshold` remain operational constants (latency SLA is a
   metric threshold, not a delivery knob) and are explicitly **not** mapped to this
   field.
3. **Implement `partialDeliveryBehavior` in the zone estop fan-out.** When an estop
   targets multiple adapters in a zone and the retry budget is exhausted with some
   ACKd and some not: `BlockNewTasks` marks the zone to refuse new assignments until
   full delivery/clearance (the scheduler already gates on zone state); `Alert` emits
   a Warning event/metric only. Fail-safe default `BlockNewTasks` (the safer choice).
   This is the larger, feature-shaped part of the work.

## Alternatives considered

- **Per-namespace dispatcher instances.** Rejected — one SafetyStream fabric serves
  all adapters; N dispatchers would fragment the shared stream state and estop
  fan-out. A single dispatcher resolving config per event is correct.
- **Wire only the timeouts, defer partial-delivery.** Viable as a phased path, and
  the ADR keeps them separable: (1) per-namespace timeout/retry resolution is a
  contained change; (2) `partialDeliveryBehavior` is a new subsystem. But the field is
  named in the same config block, so this ADR decides both even if they land in two
  PRs.
- **Map `perAdapterTimeoutMs` to `confirmTimeout`.** Rejected — `confirmTimeout` is
  the total wait for a CONFIRMED stop (stop complete), a stronger guarantee than "ACK
  = stop initiated" the field describes. Mapping to the delivery/ACK wait matches the
  field's documented meaning.

## Consequences

- **Good.** Operators can tune estop redelivery per namespace, and partial delivery
  finally has a defined, safe response instead of silence.
- **Safety.** Fail-safe throughout: unreadable config → built-in defaults; partial
  delivery defaults to `BlockNewTasks`. The dispatcher's confirmed-delivery invariant
  (record STOPPED only on adapter-CONFIRMED ACK, §9.6.2) is unchanged — this ADR
  tunes timing and adds a partial-delivery response, never relaxes confirmation.
- **Obligation.** Thread namespace-resolved config through the dispatcher episode
  (tests: honored value, fail-safe). Build partial-delivery detection + response with
  tests (all-ACK → no action; partial after retries → BlockNewTasks/Alert). Security
  Reviewer should confirm no config value can weaken confirmation semantics.
- **RA-1.** The config reads are read-only; `BlockNewTasks` rides existing zone-state
  writes, not a new per-tick sink.
- **Drawback accepted.** Per-event config lookups add reads on the estop path; these
  hit the informer cache (cheap) and are worth the per-namespace tunability. The
  three-timeout model stays (only one is operator-tunable) to avoid over-exposing
  internals.
