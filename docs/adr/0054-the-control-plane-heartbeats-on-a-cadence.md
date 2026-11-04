# ADR-0054: The control plane heartbeats adapters on a cadence, so ADR-0026's throttle has a floor under it

- **Status:** accepted
- **Date:** 2026-09-06
- **Deciders:** Controller Engineer
- **Related:** ADR-0026 (robot liveness projection — the throttle this one puts a floor
  under), RFC-0001 [#fleetadapter-status-fields-and-phase-machine](../../rfcs/dist/RFC-0001-core-spec.md#fleetadapter-status-fields-and-phase-machine) (`FleetAdapter`
  liveness: Connected → Degraded → Disconnected),
  [#safety-control-plane-response](../../rfcs/dist/RFC-0001-core-spec.md) (the per-robot
  confirming exchange, which this does **not** change), ADR-0050 (the other clock this one
  runs alongside), the RA-1 status-write discipline.

## Context

ADR-0026 projects Fleet Adapter liveness onto every served Robot's
`status.connectivity.lastSeenAt` and throttles the fan-out:

> **Throttling (write-amplification).** The fan-out fires on every heartbeat, so each
> Robot's `lastSeenAt` is refreshed **at most once per throttle interval**.

*"The fan-out fires on every heartbeat"* is load-bearing, and nothing sent heartbeats on a
cadence. Three source facts, re-derived 2026-09-06:

1. **The heartbeat is control-plane-initiated by contract.** `proto/fleet_adapter/v1`:
   *"the control plane pushes HeartbeatRequest, the adapter replies HeartbeatResponse."*
   `AdapterMessage.heartbeat` is a `HeartbeatResponse`, so there is no unsolicited
   adapter→control-plane arm and an adapter **cannot** refresh its own liveness. The
   reference adapter, which replies to inbound heartbeats and sends none of its own, is
   conformant.
2. **There was exactly one sender of `HeartbeatRequest`.**

       $ grep -rn "ControlPlaneMessage_Heartbeat{" --include="*.go" . | grep -v _test | grep -v '\.pb\.go'
       internal/command/dispatcher.go:260

3. **Its only caller was the timeout path.** `Dispatcher.Heartbeat` is reached only from
   `robot_controller.go:146` (`confirmLiveness`), called only from `:219`, inside the
   `age > heartbeatTimeout && phase != Offline` branch at `:208`.

So the clock was refreshed **only at the moment the control plane had already decided the
robot looked stale**. With the defaults (`connectivityOfflineThresholdSeconds: 30`,
requeue `heartbeatTimeout/2`) a robot answering every heartbeat it was asked refreshed
`lastSeenAt` about once per 45 s — 1.5 × the threshold that same value is compared
against. Its staleness at an arbitrary instant was roughly uniform on [0, 45 s], so a
healthy robot spent about a third of its life already past its offline threshold, and the
confirming exchange — the safeguard that exists to prevent a **false** Offline and its
false revocation of in-flight work — was doing that job constantly, for robots that were
never in doubt.

`FleetAdapter.status.phase` had the same dependency and the same symptom. The staleness
backstop moves a Connected adapter to `Degraded` after one `heartbeatIntervalSeconds` of
silence, and at the CRD default of 10 s that meant *almost always*, since the only
heartbeats were the ~45-second confirmations. `examples/demo-ab-live/run.sh` raises the
interval to 30 s and attributes the need to CPU contention between three adapter processes
and a kind cluster. That reads as a load problem and is not one: **at any interval, no
cadence can satisfy a staleness check when there is no cadence.**

## Decision

**The control plane pushes an adapter-scoped `HeartbeatRequest` down every registered
ControlStream, on a period of `spec.heartbeatIntervalSeconds / 2`.**

Four parts, each of which was a choice:

**1. Per adapter, not per robot.** One exchange refreshes
`FleetAdapter.status.lastHeartbeat` and, through `projectLiveness`, every Robot that
adapter serves. The cost is one timer and one stream write per connected adapter, and *N*
throttled Robot patches per `heartbeatTimeout/2` — which is exactly the budget ADR-0026
was sized for.

**2. The adapter-scoped probe names no robot.** `HeartbeatRequest.robot_id` now selects
between the two questions the exchange can ask: non-empty is per-robot (*"is robot X
alive?"* — the confirming exchange, unchanged), empty is adapter-scoped (*"is this adapter
alive?"*). The reply echoes the field either way. This is a **comment-only** proto change:
no field was added, moved, or renumbered, and the reference adapter already echoed
`cp.heartbeat.robot_id` verbatim, so it answers the adapter-scoped form without
modification.

An empty `robot_id` falls through the per-message authorization block without being
checked, and that is correct rather than an omission: there is no per-robot authority to
check on a message that names no robot, and the identity being asserted is the mTLS one
`AdapterHeartbeat` already gates on. It touches no robot state — `RouteHeartbeat` returns
immediately on an empty id — so an adapter cannot use the adapter-scoped form to answer a
per-robot confirming exchange it was never asked. That is the same reasoning, on the same
path, as the existing carve-out that lets a `NOT_ADMITTED` robot's heartbeat still prove
its adapter alive.

**3. Half the interval, not the whole one.** The staleness backstop compares
`now - lastHeartbeat >= interval` and re-evaluates on a requeue of exactly `interval`.
Probing on the same period would put the comparison on its own boundary, where the round
trip and the status write are enough to trip it — and a `Degraded` adapter is one the
`RobotAdmissionGate` refuses to create Robots behind. At `interval/2` a live
adapter's `lastHeartbeat` is never much more than half an interval old, so `Degraded` means
what RFC-0001 #fleetadapter-status-fields-and-phase-machine says it means: at least one probe was actually missed. It is the same shape as
ADR-0026's own *"at most once per `heartbeatTimeout/2`"*, and it makes
`spec.heartbeatIntervalSeconds` a **staleness budget** rather than a cadence nobody kept —
which is what the field's own doc comment already claimed it was.

**4. The period is re-read from the informer cache on every tick**, so raising or lowering
`spec.heartbeatIntervalSeconds` takes effect within one period without restarting the
stream. A read failure keeps the default rather than stopping the cadence: losing a tunable
must not cost an adapter its liveness.

### What this does NOT change

- **The per-robot confirming exchange.** A robot whose *telemetry* has stalled while its
  adapter is alive is still asked directly, still three times, still five seconds apart,
  before it is declared Offline. Adapter presence and robot liveness remain different
  questions; this decision answers only the first one on a cadence.
- **ADR-0026's throttle.** Unchanged, and now a ceiling over a real floor.
- **`robot_controller.go`'s comment** that `lastSeenAt` is left untouched when a robot
  answers a confirmation, which the presence path contradicts one layer down. That
  reconciliation is deliberately out of scope here and stays open: settling it
  either way changes *when* a robot is declared Offline, and this decision deliberately
  changes only *how often the clock is wound*.

## RA-1

RA-1 forbids writing status on a **telemetry tick**. This writes no status itself; what it
drives is the adapter **presence** path (RFC-0001 #fleetadapter-status-fields-and-phase-machine), which is the alternative RA-1
distinguishes by name and which `internal/controlstream`'s dispatch already annotates as
*"deliberately NOT the telemetry path (RA-1): a Heartbeat proves the adapter is alive; a
TelemetryPayload is a per-robot data tick and never touches adapter status."* The rule
licenses the presence path; it was the *absence* of a presence cadence that left the
telemetry stream as the only periodic signal in the system. Refreshing `lastSeenAt` from
telemetry — the obvious alternative, considered and rejected below — is the thing RA-1
exists to refuse and remains refused.

The writes this causes are the ones ADR-0026 budgeted for and are still gated twice:
`mutateStatus` patches `FleetAdapter.status` only on a material change, and
`projectLiveness` skips any Robot refreshed within `heartbeatTimeout/2`.

## Consequences

- A live Robot's `lastSeenAt` is now no older than `heartbeatTimeout/2` (15 s at the
  defaults) instead of up to ~45 s, so `swarmctl describe`, the `Ready` gate, ADR-0030's
  auto-removal grace and ADR-0011's `ConnectivityCritical` escalation all read a duration
  that means what it says.
- The Offline latency after an adapter dies collapses from a ~10-55 s range to a bounded
  one, because `a₀` — the staleness of `lastSeenAt` at the moment of death — is now bounded
  by `heartbeatTimeout/2` rather than by `1.5 × heartbeatTimeout`.
- A Connected adapter at the CRD default `heartbeatIntervalSeconds` stays Connected.
  `examples/demo-ab-live/run.sh`'s 30-second override and the UC-1.6 runbook's
  *Troubleshooting* entry blaming CPU contention were both treating this defect as load.
  The runbook entry is corrected here to name the cause; **the override is left in place
  and re-labelled**, because retiring it is a claim about a live three-adapter kind cluster
  and no such run has been made since this change.
- One additional `FleetAdapter` status write per adapter per `interval/2` (12/min at the
  defaults). That is per **adapter**, not per robot; the per-robot fan-out is unchanged.
- A third-party adapter that ignores a `HeartbeatRequest` it cannot attribute to a robot
  will not answer the adapter-scoped probe and will decay to Degraded. That is not a
  regression: with no cadence at all it already decayed, and the contract now states the
  obligation explicitly.

## The contract version implication — NOT taken here

ADR-0032 defines `contract.Version` as a semver over *"the proto surface, the
`SupportedAction` schema, and the conformance-suite revision"*, and this decision changes
the proto surface. That is a **MINOR** bump (compatible; it never invalidates an existing
qualification), so `1.1.0 → 1.2.0`.

It is **not** taken in this change, deliberately. The value is pinned in
`internal/contract/version.go`, asserted by name in `internal/contract/version_test.go`,
stamped by `adapters/conformance/report.py`, and stated three times in RFC-0001 — a
published document currently inside a review window. Bumping it is a decision with owners
beyond this fix, not a side effect of it. Until it is taken, contract `1.1.0` describes a
marginally wider proto surface than it did, which is a register defect rather than a
runtime one: every adapter that negotiated `1.1.0` still negotiates, and every field added
here is optional to an adapter that ignores it.

> **Implementation note (2026-09-24).** The contract version has moved past `1.1.0` since this
> was written — to `1.2.0` under ADR-0032 Amendment 1 §A1.1, and again since — so the "until it is
> taken" state above has ended and `1.1.0` is not the current version. The implemented version is
> stated in RFC-0001 *Package and versioning*. The definition quoted at the head of this section — a
> semver over the proto surface, the `SupportedAction` schema *and the conformance-suite
> revision* — is the reading ADR-0032 Amendment 1 §A1.1 records as not governing: the
> suite revision is not a component of the contract version. The decision recorded here is
> unchanged.

It is tracked as its own decision.

## Alternatives rejected

- **Refresh `lastSeenAt` from telemetry.** RA-1, and ADR-0026 rejected
  it in writing: it would couple liveness to data cadence rather than to adapter presence.
- **Refresh `lastSeenAt` on an answered confirmation** (option 3). Cheap and wrong in an
  instructive way — it winds the clock only on the path that already assumes the clock has
  run down, which is the defect restated rather than fixed.
- **Add an unsolicited adapter heartbeat arm to the protocol** (option 4). A wire change
  that moves the cadence to the side of the boundary that cannot be trusted to keep it.
  Liveness asserted by the thing being measured is not liveness.
