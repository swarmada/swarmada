# ADR-0023: Wire four controller-pending status projections — model suspension stamp, FleetZone conditions, maintenance resume-estop gate, maintenance counts

- **Status:** accepted
- **Date:** 2026-07-24
- **Deciders:** Principal Architect, Kubernetes/controller maintainers
- **Related:** RFC-0001 §9.3.6 (Model Update Manager), §9.3.4/§9.1.5 (Zone Controller), §9.1.11 (ZoneMaintenance), §9.6.2 (estop); ADR-0013 (graceful drain timeout); AGENTS.md RA-1

## Context

Four status fields are defined in `api/v1` and marked "Specified, controller
pending". Each already exists on its CRD — this ADR decides *how each is
computed and written*, not *what it is*. **No CRD schema change is required by
any of the four.** The unifying constraint is RA-1: every write must be
transition-driven (materialised only on an actual change), never emitted on a
telemetry tick.

1. **`ModelRollout.status.currentBatch[].capabilitiesSuspendedAt`** — a
   per-batch-robot timestamp for when the robot's model-driven capabilities were
   suspended for the update. The `ModelRollout` controller suspends capabilities
   at batch entry (it marks the model `Updating`, which the Capability Controller
   reads as suspension). The batch is built by the shared `buildRolloutBatch`,
   used by **both** the model and firmware rollouts; only the model path suspends
   capabilities. "Per-attempt" means a robot that fails and re-enters (e.g. Auto
   rollback → `Updating` again) must carry the *current* attempt's suspension
   time, not the original.

2. **`FleetZone.status.conditions`** — the Zone Controller computes topology
   (`isLeaf`/`childZones`/`robotCount`) behind a `DeepEqual`-guarded status
   patch, but publishes no standard `metav1.Condition`s. Two are wanted:
   `ZoneReady` and `CapacityAvailable`. The controller package already has an
   RA-1-safe `upsertCondition` helper that preserves `LastTransitionTime` when a
   condition's status is unchanged.

3. **`ZoneMaintenance.spec.requireEstopClearBeforeResume`** — an operator
   preference (per-ZM, `*bool`, CRD default `true`; namespace default from
   `SwarmadaConfig.spec.maintenance.requireEstopClearBeforeResume`) to gate a
   paused robot's resume (`Maintenance → Idle`) until its estop is Clear. **This
   is an operational convenience gate, not a safety mechanism** — it must not be
   coupled to or conflated with the hardware estop path (§9.6.2), and must never
   be represented as something that stops or holds a robot.

4. **`ZoneMaintenance.status.pausedRobotsCount` / `windingDownRobotsCount`** —
   print-column convenience counts that must mirror the already-computed
   `pausedRobots[]` / `windingDownRobots[]` lists.

## Decision

### 1. Stamp `capabilitiesSuspendedAt` at batch entry, model path only, via `buildRolloutBatch`

Extend `buildRolloutBatch` with a `stampSuspended bool` (the model call site
passes `true`, firmware `false`). A **new** batch entrant under a model rollout
gets `CapabilitiesSuspendedAt = &now` — the same instant as `UpdateStartedAt`,
because suspension coincides with the `Updating` write at batch entry. Continuing
entrants are preserved as-is (the helper already copies the prior entry by name),
so the stamp is stable for the life of an attempt. A robot that leaves the batch
(done/failed) and later re-enters is a fresh entrant → a fresh stamp — which is
exactly the **per-attempt** semantics required, achieved with no extra
bookkeeping. Firmware rollouts never set the field (they suspend nothing).

- **RA-1:** the value rides the existing `computeRolloutStatus` →
  `DeepEqual(rollout.Status, newStatus)` → `Status().Patch` write. No new write
  path; nothing per-tick.

### 2. Compute `ZoneReady` and `CapacityAvailable` in the Zone Controller as `metav1.Condition`s

In `ZoneController.Reconcile`, after topology is computed and before the existing
`DeepEqual`-guarded patch, upsert two conditions via the existing
`upsertCondition` helper:

- **`ZoneReady`** — `True` (reason `Reconciled`) once topology is resolved and
  the zone is serviceable; `False` (reason `NoPhysicalBounds`) for a **leaf** zone
  that declares no `spec.physicalBounds`, since the Zone Controller then cannot
  derive `currentZone` by containment and cannot place robots there. Non-leaf
  (aggregating) zones are Ready once reconciled. Derived from `spec.physicalBounds`
  + the computed `isLeaf`; deliberately **not** coupled to estop, which has its
  own `status.estopStatus` field.
- **`CapacityAvailable`** — `True` (reason `Available`) when
  `spec.maxConcurrentRobots == 0` (unlimited) or
  `status.currentConcurrentRobots < spec.maxConcurrentRobots`; `False` (reason
  `AtCapacity`) otherwise. Derived from the TDE-written
  `status.currentConcurrentRobots` and `spec.maxConcurrentRobots`.

- **RA-1 safeguards:** `upsertCondition` moves `LastTransitionTime` only on a
  status flip, and the whole status is still behind the existing `DeepEqual`
  patch. Condition **messages must be static** (no live counts/robot names) — a
  count baked into the message would differ every reconcile, defeat `DeepEqual`,
  and churn a write on every telemetry-driven `currentConcurrentRobots` change.
  The numbers live in the dedicated numeric status fields, never in the message.

### 3. Gate controller-driven resume on estop-Clear — an operational admin gate, explicitly not a safety mechanism

**Framing (normative for this decision):** this gate only delays the
administrative `Maintenance → Idle` **phase transition**. It does **not** stop,
hold, slow, or otherwise affect a robot; it does **not** participate in the
hardware estop path (§9.6.2), which is entirely separate and authoritative. It
*reads* the already-published estop projection to decide whether to flip a phase,
nothing more. It must never be documented, logged, or surfaced as a safety
interlock.

- **Effective value:** `spec.requireEstopClearBeforeResume` if non-nil, else
  `SwarmadaConfig.spec.maintenance.requireEstopClearBeforeResume` (via the
  fail-safe `namespaceConfig` helper), else `true` (the CRD default).
- **"Estop Clear" for a robot:** `robot.status.estopState == Normal` **and** the
  robot's zone `FleetZone.status.estopStatus ∈ {Clear, ""}`.
- **Where:** in `resumeAll`, per in-scope `Maintenance`-phase robot. When the
  gate is on and the robot is not Clear, **skip** it (leave it in `Maintenance`) —
  a benign hold on the phase flip; a later reconcile resumes it once Clear.
- **Deletion is never gated.** `resumeAll` is also called from the finalizer on
  delete, where the contract is "deleting a ZoneMaintenance resumes all robots".
  The gate applies **only** to the auto-resume path; deletion-driven resume runs
  ungated so a stuck estop can never wedge the finalizer and block deletion.
  Implemented by threading a `gated bool` into `resumeAll` (auto-resume `true`,
  delete `false`).
- **Auto-resume with blocked robots:** if the gate blocks ≥1 robot at the
  auto-resume deadline, do **not** mark the maintenance `Completed` (that implies
  everyone resumed). Resume the unblocked robots, set a `ResumeBlockedByEstop`
  condition on `ZoneMaintenance.status.conditions` (via `upsertCondition` —
  transition-driven, RA-1), and requeue; the maintenance completes automatically
  once estops clear. Reuses the existing `Conditions` field — no schema change.

### 4. Populate the maintenance counts from the existing lists, transition-driven

Set `status.pausedRobotsCount = len(pausedRobots)` and
`status.windingDownRobotsCount = len(windingDownRobots)` in `Reconcile` at the
point those slices are assigned, and reset both to `0` on the auto-resume path
where the lists are cleared. The values ride the existing
`patchStatusIfChanged` (`DeepEqual`) write — no new write path (RA-1).

## Alternatives considered

- **`capabilitiesSuspendedAt` stamped by a separate write at suspension time
  (rejected).** A dedicated patch at `enterBatch` would be a second writer to the
  rollout status racing `computeRolloutStatus`, and would need its own per-attempt
  reset logic. Folding it into `buildRolloutBatch` reuses the preserve-by-name
  machinery that *already* gives per-attempt semantics for free.
- **`ZoneReady` coupled to estop, or a third `EstopClear` condition (rejected).**
  Zone estop already has a first-class `status.estopStatus`; duplicating it into a
  condition invites divergence. `ZoneReady` stays a single-purpose
  topology/serviceability signal.
- **Resume gate reading or driving the hardware estop path (rejected — and
  prohibited by the requirement).** Reading `estopState`/`estopStatus`
  *projections* to decide an administrative phase flip is safe and cheap; touching
  the estop delivery/acknowledgement path would turn an operational convenience
  into a safety-coupled mechanism it was explicitly not to be.
- **Gating deletion-driven resume too (rejected).** It would let a stuck estop
  wedge the finalizer and make a ZoneMaintenance undeletable — a worse operational
  failure than resuming a robot whose estop is still set (the hardware estop keeps
  the robot stopped regardless of its administrative phase).
- **Counts as computed/printer-only, not stored (rejected).** `int32` status
  fields already exist for print columns; computing them once next to the lists is
  cheaper than a CRD printer expression and keeps the list and its count atomic in
  one write.

## Consequences

- **New obligations.**
  - `buildRolloutBatch` grows one parameter; both call sites
    (`computeRolloutStatus`, firmware `computeStatus`) and the test helper update.
  - The Zone Controller and ZoneMaintenance controller each gain condition
    computation; tests must assert `LastTransitionTime` stability (no churn when
    status is unchanged) and static messages.
  - The resume path gains a `gated` distinction and a `ResumeBlockedByEstop`
    condition; tests must cover gate on/off, robot-Clear vs zone-Clear, the
    deletion-never-gated invariant, and the "blocked ⇒ stay Active + requeue"
    path.
- **Two writers on `FleetZone.status`, disjoint fields.** The zone-estop
  controller owns `estopStatus`; the Zone Controller owns topology + the two
  conditions. Both patch only on change, so they converge; an `estopStatus` change
  re-triggers the Zone Controller (it watches `FleetZone`) but, because
  `ZoneReady` is not estop-coupled, does not change the conditions it writes.
- **Accepted drawbacks.**
  - The resume gate can leave a maintenance `Active` indefinitely if an estop
    never clears; this is intentional (the operator either clears the estop or
    deletes the maintenance, which resumes ungated).
  - `capabilitiesSuspendedAt` equals `updateStartedAt` for model rollouts today;
    the field is kept distinct because it is model-only and semantically the
    suspension instant, not the entry instant (they only coincide at entry).
- **Non-goals preserved.** No CRD field is added or renamed; the four
  "controller pending" doc-comments in `api/v1` are dropped when the controllers
  land (a comment-only `api/v1` edit that regenerates CRD descriptions, no schema
  change). This is a reference-implementation decision; the normative fields live
  in `api/v1` and RFC-0001.

## Implementation surface (file-by-file; no code written yet)

- `internal/controller/firmwarerollout_controller.go` — add `stampSuspended bool`
  to `buildRolloutBatch`; new entrant sets `CapabilitiesSuspendedAt` only when
  `stampSuspended`. Update the firmware call site to pass `false`.
- `internal/controller/modelrollout_controller.go` — pass `true` at the
  `buildRolloutBatch` call in `computeRolloutStatus`.
- `internal/controller/zone_controller.go` — in `Reconcile`, upsert `ZoneReady`
  and `CapacityAvailable` conditions (static messages) into `zoneObj.Status.Conditions`
  before the existing `DeepEqual` patch.
- `internal/controller/zonemaintenance_controller.go` — (a) set
  `PausedRobotsCount`/`WindingDownRobotsCount` beside the list assignments and
  reset to `0` on auto-resume; (b) add the effective-value resolver
  (`spec` → `SwarmadaConfig` → `true`); (c) thread `gated bool` through
  `resumeAll`, skip non-Clear robots on the gated (auto-resume) path only; (d) on
  auto-resume with blocked robots, set `ResumeBlockedByEstop` and stay Active +
  requeue instead of `Completed`.
- `api/v1/rollout_types.go`, `api/v1/fleetzone_types.go`,
  `api/v1/zonemaintenance_types.go` — **doc-comment only**: drop the four
  "controller pending" notes once the controllers land. No schema change.
- Tests: `internal/controller/` — model-suspension stamp + per-attempt reset;
  FleetZone condition compute + `LastTransitionTime` stability; resume-gate matrix
  (incl. deletion-never-gated and blocked-stays-Active); count population.
- `docs/adr/README.md` — **existing file, needs confirmation**: add the ADR-0023
  index row.
