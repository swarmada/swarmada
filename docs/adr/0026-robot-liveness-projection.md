# ADR-0026: Project Fleet Adapter liveness onto served Robots

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** API Designer, Distributed Systems Reviewer, Security Reviewer
- **Related:** RFC-0001 §9.1.12 (adapter presence), §9.3.8 (offline-duration accounting), the Robot schema (`status.connectivity.lastSeenAt`); ADR-0011 (connectivity-critical); ADR-0025 (mTLS ControlStream — the verified presence source)

## Context

`robot_controller.go` treats `status.connectivity.lastSeenAt` as the robot's
liveness clock: past `heartbeatTimeout` it moves the Robot to `Offline` and sets
`Ready=False`; on a never-initialised Robot (`phase == ""`) it stamps `Discovered`
with `Ready=Unknown` "Waiting for first Fleet Adapter heartbeat", and it requeues
at `heartbeatTimeout/2` to catch the lapse.

Two facts break this today:

1. **Nothing writes `lastSeenAt`.** A full-tree search finds only readers
   (`robot_controller.go:106-107`, `swarmctl describe`, deepcopy). Adapter liveness
   is recorded on `FleetAdapter.status.lastHeartbeat` by the presence handlers
   `AdapterConnected`/`AdapterHeartbeat` (`fleetadapter_controller.go`), driven by
   ControlStream presence — now mTLS-verified per ADR-0025 — but that liveness is
   **never projected onto the Robots the adapter serves**. So every
   adapter-registered Robot sits at `Discovered` / "Waiting for first Fleet Adapter
   heartbeat" forever, even over a fully verified ControlStream.

2. **The consumer never promotes to Ready (design finding).** Even if `lastSeenAt`
   were written, `robot_controller` sets `Ready` only to `Unknown` (Initialising)
   or `False` (HeartbeatTimeout) — **never `True`** (confirmed repo-wide; the only
   `Ready=True` is on FleetZone). A fresh `lastSeenAt` enters the timeout branch,
   finds the age under threshold, and does nothing — leaving `Ready=Unknown`.
   There is also no `Offline → online` recovery and no `Discovered → Idle`
   promotion anywhere. **Projecting `lastSeenAt` is therefore necessary but not
   sufficient for a Robot to "reach Ready": the consumer needs a matching
   liveness→`Ready` branch.**

A Robot binds its adapter by `spec.adapter.name`, which must name a FleetAdapter in
the **same namespace**; that namespace is the mTLS-authenticated boundary (ADR-0025).
`Ready` (liveness) is deliberately separate from the schedulability admission gate
(adapter `Connected` **and** `Conformance == Passed`): liveness says "reachable",
admission says "eligible to be assigned work".

## Decision

Project adapter liveness from **presence** onto served Robots, and complete the
consumer so a live Robot reaches `Ready`. Two coupled changes:

**A. Projection (the missing write).** Extend the presence path — `AdapterConnected`
and `AdapterHeartbeat` — so that, after updating `FleetAdapter.status`, it fans out
to every Robot with `spec.adapter.name == id.AdapterName` in `id.Namespace` and sets
`status.connectivity.lastSeenAt = now` (creating `connectivity` if nil). This is
driven by presence events (connect / liveness Heartbeat), **never by a
`TelemetryPayload`** — RA-1: a telemetry tick must not write `Robot.status`.

**B. Consumer (the missing promotion).** In `robot_controller`, when the Robot is
live (`lastSeenAt != nil && age <= heartbeatTimeout`), set `Ready=True`
(reason `AdapterLive`) instead of leaving the stuck `Unknown`/`False`, and if the
phase is `Offline`, recover it to `Discovered` (re-entering the normal flow). Phase
promotion to `Idle`/schedulability stays **out of scope** — that is the conformance
/ admission gate, and `Ready` is separate from it by design.

**Throttling (write-amplification).** The fan-out fires on every heartbeat, so each
Robot's `lastSeenAt` is refreshed **at most once per throttle interval**: write only
when `now - existing.lastSeenAt >= interval` (or on first-seen / a stale or Offline
Robot). The interval is `heartbeatTimeout/2`, resolved once per fan-out from the
namespace `SwarmadaConfig` (the same read `robot_controller` and the telemetry
Router already do) and reused for all served Robots. This keeps `lastSeenAt` fresh
enough that a live Robot never falsely times out, while bounding writes to **≤2 per
`heartbeatTimeout` per Robot**. The persisted `lastSeenAt` **is** the throttle state
— no in-memory map, correct across controller restarts and multiple replicas.
Patches are status-subresource `MergeFrom` and are applied **only on a material
change**, consistent with `mutateStatus`'s discipline.

**Placement — extend the presence path (not a dedicated sink).** The fan-out lives
in the FleetAdapter controller alongside `AdapterConnected`/`AdapterHeartbeat`,
which already hold a client and clock and already own adapter liveness. Recording
the adapter heartbeat and projecting it to served Robots is one logical operation;
splitting it into a separate presence-wired sink would spread it across two
components with no benefit at this scale. (A dedicated sink stays the escape hatch
if liveness later needs its own lifecycle.)

**Efficiency — a field index.** Register a field index on Robot `spec.adapter.name`
in the FleetAdapter controller's `SetupWithManager`, and enumerate served Robots
with `MatchingFields{...: id.AdapterName}` + `InNamespace(id.Namespace)`. Cost is
O(Robots on this adapter) served from the informer cache, versus a full-namespace
`List` + filter on every heartbeat. The index is a one-time setup cost and matches
the pattern already used for the per-zone pending-action index.

**Concurrency.** The fan-out and `robot_controller` both patch Robot status, so a
patch may lose to a concurrent reconcile ("object has been modified"). Wrap each
Robot patch in `retry.RetryOnConflict` (re-Get, re-apply, re-patch) so a conflict is
retried, not surfaced as an error. `CapabilitiesIngestor.IngestCapabilities`
uses a full `Status().Update` with no retry (`capabilities_ingestor.go:62`)
— it shares this bug and is brought onto the same retry-on-conflict discipline.

## Alternatives considered

- **Drive `lastSeenAt` from the telemetry ingest path.** A `TelemetryPayload` does
  carry a robot_id and arrives frequently, so it is a tempting liveness signal.
  Rejected: it violates RA-1 (a per-tick telemetry frame would write `Robot.status`)
  and would couple liveness to data cadence rather than to adapter presence.

- **Write `lastSeenAt` on every heartbeat with no throttle.** Simple, but N Robots ×
  a fast heartbeat is exactly the write amplification RA-1 exists to prevent.
  Rejected in favour of the `heartbeatTimeout/2` refresh floor.

- **Only write on material transitions (first-seen + recovery), never refresh.**
  Rejected: without periodic refresh the persisted `lastSeenAt` ages past
  `heartbeatTimeout` and a steadily-live Robot falsely lapses to `Offline`. The
  throttle must still refresh, only rate-limited.

- **A dedicated robot-liveness sink wired to presence in `cmd/manager`.** More
  decoupled, but adds wiring and splits one operation; not warranted now (see
  Placement).

- **List + filter instead of a field index.** Simpler to write, but scans every
  Robot in the namespace on each heartbeat. Rejected on cost.

- **Projection only, leave `robot_controller` unchanged.** This is the literal
  "write the field" framing, but per Context §2 it leaves `Ready=Unknown`
  forever — the Robot still never reaches Ready. Rejected: the consumer promotion is
  required for the stated outcome.

## Consequences

- A Robot served by a live, mTLS-verified adapter now gets a refreshed `lastSeenAt`
  and reaches `Ready=True`; when the adapter disconnects and heartbeats stop, the
  clock ages out and `robot_controller`'s existing timeout path lapses it to
  `Offline` / `Ready=False`. The requeue-at-`heartbeatTimeout/2` already detects the
  lapse.
- `Ready` reflects **liveness only**. Becoming schedulable still additionally
  requires the adapter `Connected` and `Conformance == Passed`; this ADR does not
  touch that gate, and a live-but-unconformed Robot is `Ready` but not yet
  assignable.
- New obligations: a Robot field index to maintain; a bounded per-heartbeat fan-out
  (one config read + an indexed list + ≤N throttled status patches); retry-on-conflict
  on the Robot and Capabilities status writes.
- Security: the fan-out is strictly scoped to `id.Namespace` — an adapter's presence
  in namespace A never writes a Robot in namespace B, even one that names the same
  `spec.adapter.name`. The namespace is the mTLS identity boundary carried through.
- Seams left: `Offline → online` recovery restores phase to `Discovered`, not to a
  remembered pre-offline phase (there is no `Discovered → Idle` promotion in the tree
  today — a separate admission/conformance concern, explicitly out of scope here);
  multi-replica presence still runs the fan-out on every replica (the throttle keeps
  that idempotent and cheap).
