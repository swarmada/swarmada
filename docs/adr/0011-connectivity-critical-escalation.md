# ADR-0011: Surface prolonged-offline "Critical" connectivity as a Robot condition

- **Status:** accepted
- **Date:** 2026-07-09
- **Deciders:** API Designer, Distributed Systems Reviewer, Kubernetes Expert
- **Related:** RFC-0001 §9.1.11 (SwarmadaConfig health tunables), §9.3.8 (offline
  accounting), ADR-0006 (north side is the Kubernetes API), the RA-1 status-write
  discipline

## Context

`SwarmadaConfig.spec.health` defines two connectivity thresholds:

- `connectivityOfflineThresholdSeconds` (default 30) — seconds without a heartbeat
  before the robot is considered Offline. **Wired:** the Robot controller marks
  `status.phase = Offline` past this threshold and anchors `status.offlineSince`.
- `connectivityCriticalThresholdSeconds` (default 120) — seconds *Offline* before
  the robot is considered **Critical**. **Not wired:** nothing in the control plane
  consumes it, and there is no representation of a "Critical" connectivity state.

The field forces one decision: **where does "Critical" connectivity live in the
API, and what writes it?** The escalation itself is cheap to compute — the Robot
controller already runs on a periodic requeue, already owns the Offline transition,
and already records `status.offlineSince`, so `now − offlineSince ≥
connectivityCriticalThresholdSeconds` is available with no new inputs. The hard part
is representation, and it is constrained by three forces:

1. **The phase machine is single-valued.** `status.phase` is one of
   Discovered/Idle/Assigned/InProgress/Charging/Error/Offline/Maintenance. A robot
   that has been offline for two minutes *is still Offline* — "Critical" is a
   duration-derived severity of that same condition, not a distinct lifecycle phase.
2. **`HealthState.Critical` already exists but means something else.** The
   `Healthy/Degraded/Critical` enum on `status.health` aggregates *hardware and
   capability* health ("a component Failed or a non-pauseable capability lost").
   Reusing it for connectivity would conflate two orthogonal axes.
3. **RA-1.** Any new signal must be a projection of material state written only on a
   material transition — never on a telemetry tick, and without adding a second
   status write per reconcile.

## Decision

**Surface prolonged-offline as a Robot status condition, not a new phase or a reused
health enum.** The Robot controller sets a condition of type `ConnectivityCritical`
(`status.conditions[]`, standard `metav1.Condition`) to `True` once the robot has
been Offline for at least `connectivityCriticalThresholdSeconds`, and back to
`False`/removed on reconnect. The robot's `status.phase` remains `Offline`
throughout.

Specifics:

- **Threshold source.** Resolve `connectivityCriticalThresholdSeconds` per namespace
  via the shared `namespaceConfig()` helper, failing safe to a 120s constant —
  exactly mirroring the offline-threshold wiring already in `robot_controller.go`.
- **Compute site.** In the existing Robot `Reconcile`, immediately after the Offline
  determination: if the robot is Offline and `now − offlineSince ≥ threshold`, set
  the condition `True` (reason `OfflineThresholdExceeded`); otherwise ensure it is
  `False`/absent. This rides the existing DeepEqual material-change guard, so it adds
  no extra write and never fires on an unchanged robot.
- **Requeue.** When Offline and not yet Critical, requeue at the Critical horizon
  (`offlineSince + threshold`) in addition to the existing half-offline-timeout
  requeue, so the escalation fires promptly without shortening the steady-state
  cadence.
- **Metric (complement, not the primary surface).** Increment/observe a
  `swarmada_robot_connectivity_critical` signal on the `False→True` edge for alerting
  pipelines, consistent with the existing `swarmada_robot_offline_duration_seconds`.

No `api/v1` schema change is required — conditions are already part of
`RobotStatus`. This keeps the change a controller-only wiring.

## Alternatives considered

- **New `RobotPhase = Critical`.** Rejected. It would evict the `Offline` phase for a
  robot that is precisely Offline, losing information; it would require every phase
  consumer (scheduler eligibility, metrics label set, the offline-duration
  accounting keyed on the Offline phase) to learn a new state that is really only
  "Offline, but longer"; and two duration-derived phases (Offline then Critical)
  invite a cascade of further phase splits.
- **New enum field `ConnectivityStatus.State` (Online/Offline/Critical).** Rejected
  as the primary surface. It is a reasonable typed model, but it duplicates
  information already carried by `phase == Offline` + `offlineSince`, adds a schema
  field and its defaulting/validation surface, and is less alert-friendly than a
  condition. A condition can be added without a CRD schema bump; a field cannot.
- **Reuse `status.health.Status = Critical`.** Rejected per Context force #2 —
  conflates connectivity with hardware/capability health; an operator seeing
  `health: Critical` would reasonably infer a failed component, not a comms gap.
- **Metric/event only, no status.** Rejected as the *primary* surface (kept as a
  complement). Alerting wants it, but a robot's own status should also reflect that
  it is in a degraded-connectivity severity band; hiding it only in a metric makes it
  invisible to `kubectl get robot -o yaml` and to controllers that may later gate on
  it.

## Consequences

- **Good.** No schema change; the change is a small, testable addition to a
  controller that already computes everything it needs. `status.phase` stays honest.
  The condition is the idiomatic Kubernetes place for an orthogonal, duration-derived
  severity and is directly consumable by alerting and by future gating logic.
- **Good.** RA-1 is preserved: one material transition (`False→True`, `True→False`),
  no per-tick writes, no second write per reconcile.
- **Obligation.** The Critical-horizon requeue must be covered by tests (fresh /
  offline-below-threshold / offline-past-threshold / reconnect-clears), mirroring the
  offline-threshold table test. The fail-safe (unreadable config → 120s) must be
  tested too.
- **Obligation / seam.** If a future consumer needs to *gate* on Critical (e.g., the
  scheduler refusing to hold assignments for critically-offline robots, or task
  cancellation policy keying off it), the condition is the join point — but that
  gating is out of scope here and should be its own decision. This ADR deliberately
  makes Critical *observable* without making it *actionable*.
- **Drawback accepted.** A condition is slightly less discoverable than a top-level
  field for someone scanning a Robot quickly; the complementary metric and the
  printer columns already on Robot mitigate this. If a typed field is later judged
  necessary, it can be added without contradicting this decision (the condition
  remains the transition record).
