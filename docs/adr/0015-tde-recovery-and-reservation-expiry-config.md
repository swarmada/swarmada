# ADR-0015: Source TDE reservation-expiry per namespace and recovery tunables from the manager namespace

- **Status:** accepted
- **Date:** 2026-07-09
- **Deciders:** Distributed Systems Reviewer, API Designer, Kubernetes Expert
- **Related:** RFC-0001 §9.4 (traffic deconfliction), §9.4.7 (startup recovery),
  §9.1.11.10 (TDE tunables), ADR-0009 (deconfliction gate at commit site)

## Context

Four `SwarmadaConfig.spec.trafficDeconfliction` tunables remain unconnected. They do
NOT share a scope, and that is the whole difficulty:

- **`onReservationExpiry`** (`ReleaseAndRequeue` | `ReleaseOnly`) — per task/namespace.
  Today reservation expiry is *lazy*: an expired Reserved slot stops counting
  toward zone capacity (`engine.counts`). Nothing actively notices that a task's
  reservation expired before the robot entered the zone, so neither disposition is
  applied. This is an unimplemented behavior, not only an unread field.
- **`tdeCallTimeoutMs`** — per namespace; bounds a `RequestReservation` call. The
  engine is in-process today (see ADR-deferred A10), so this is low-value until the
  engine can block.
- **`recovery.zoneControllerWaitTimeoutSeconds`** and
  **`recovery.conservativeRecoveryFallback`** — consumed by `RecoveryRunnable`, which
  is a **cluster-wide, leader-elected singleton** that rebuilds *all* zones' state.
  It has no single namespace to read; the values are hardcoded in `main.go`
  (`ReadyTimeout: 30s`, `Fallback: RecoverReleaseAll`).

The engine already has the per-namespace seam: `tdeEngine.WithConfigResolver(func(ns)
tde.Config)` reads `ReservationTTLSeconds`/`DisconnectedReservationTTLSeconds` from
each namespace's SwarmadaConfig. That is the model for the per-namespace half.

## Decision

Split by scope.

1. **`onReservationExpiry` — implement + wire per namespace.** Add active expiry
   handling: when a task holds a Reserved slot that has expired before zone entry is
   confirmed, the FleetTask reconciler applies the namespace's disposition —
   `ReleaseAndRequeue` returns the task to Pending (releasing the slot);
   `ReleaseOnly` releases the slot but leaves the task Assigned for the operator.
   Resolve the field through the existing per-namespace config path
   (`WithConfigResolver` / `namespaceConfig`), fail-safe to `ReleaseAndRequeue` (the
   CRD default). This is the substantive part of the work.
2. **`recovery.*` — source once from the manager namespace, not per namespace.**
   These govern a cluster-wide action, so per-namespace config is the wrong model.
   `RecoveryRunnable` reads the SwarmadaConfig in the manager's own namespace
   (`POD_NAMESPACE`) at recovery time and applies
   `zoneControllerWaitTimeoutSeconds` → `ReadyTimeout` and
   `conservativeRecoveryFallback` → `Fallback`, failing safe to today's 30s /
   `RecoverReleaseAll`. One designated config, read once per recovery, matching the
   cluster scope of the engine.
3. **`tdeCallTimeoutMs` — remain deferred** until the engine becomes an
   out-of-process/blocking call (tracked with A10). Wiring a timeout around an
   in-process call guards nothing and risks flakiness.

## Alternatives considered

- **Read `recovery.*` per namespace and reconcile a cluster-wide policy from many
  configs.** Rejected — there is no principled merge of N namespaces' recovery
  timeouts into one cluster action; it invites surprising "whichever namespace won"
  behavior. A single designated (manager) namespace is predictable.
- **Keep `recovery.*` as manager flags only and deprecate the CRD fields.** A clean
  option, but it strands two published API fields. Reading the manager-namespace
  config keeps the documented fields meaningful while honoring the cluster scope; the
  flags can still override for air-gapped setups.
- **Make expiry active inside the engine (engine requeues tasks).** Rejected — the
  engine deliberately does not own task lifecycle (ADR-0009 keeps the gate at the
  commit site). Task disposition belongs to the FleetTask reconciler; the engine only
  reports/releases slots.

## Consequences

- **Good.** `onReservationExpiry` becomes real and namespace-tunable through the
  established resolver; `recovery.*` gets a single, scope-appropriate home.
- **Obligation.** New expiry-detection path in the FleetTask reconciler with tests
  (expired-before-entry → requeue vs release-only; fail-safe default). `RecoveryRunnable`
  gains a one-shot manager-namespace config read (needs `POD_NAMESPACE` wiring in
  `main.go`) with tests for both dispositions and the fail-safe.
- **Safety / RA-1.** Expiry handling is transition-driven (only on an actually-expired
  reservation), never per tick. Recovery stays fail-closed until rebuilt; sourcing the
  timeout/fallback from config cannot open the gate early.
- **Drawback accepted.** `recovery.*` semantics become "the manager namespace's
  config wins," which operators must understand; documented here. `tdeCallTimeoutMs`
  stays inert until the engine model changes.
- **Scope.** This ADR covers the design; onReservationExpiry is the meaningful
  implementation and can land independently of the recovery-config read.
