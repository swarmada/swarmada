# ADR-0029: Robot.status.phase is a derived state machine, not a sticky Discovered label

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** Control Plane / CRD-controller capability
- **Related:** RFC-0001 §5.2.3 (RobotPhase wire enum), ADR-0026 (per-robot liveness projection), ADR-0011 (connectivity-critical escalation); `fleet_adapter.v1 RobotPhase` proto enum

## Context

`Robot.status.phase` is meant to be the one-glance lifecycle summary of a robot,
mirroring the `fleet_adapter.v1 RobotPhase` wire enum (`Discovered`, `Idle`,
`Assigned`, `InProgress`, `Charging`, `Error`, `Offline`, `Maintenance`).

Today a robot enters the fleet on `Discovered` (auto-admit / registrar) and the
Robot controller only ever moves it to `Offline` on a heartbeat lapse — and, on
recovery from `Offline`, back to `Discovered`. Nothing advances a *live, admitted*
robot off `Discovered`. So once the `Ready` condition flips True, the phase still
reads `Discovered`. The scheduler (`fleetaction_controller`) does set `InProgress`
when it assigns an action and returns the robot to `Idle` when the action ends —
but a robot that never receives an action sits on `Discovered` forever.

The result: `Discovered` means both "pending admission / awaiting first liveness"
**and** "admitted, live, and working." swarmtop shows `Discovered` permanently, and
the phase is useless as a status summary.

Two constraints bound the fix:

1. **The phase value set is the wire enum.** It has no `Initialising` and no
   `Ready` member; adding one is a protocol change, out of scope here (and would
   break the "phase mirrors the wire enum" invariant). The conceptual states
   "Initialising" and "Ready" must map onto existing values.
2. **RA-1.** Phase is a throttled projection of conditions/liveness, never a
   per-telemetry-tick write. The fix must derive phase from state the control
   plane already holds (the `Ready` condition, liveness, `status.assignedAction`)
   and ride the existing material-change patch — no new status writes, none on a
   telemetry tick.

## Decision

Make `Robot.status.phase` a derived summary with explicit per-phase ownership.

**State machine (values are the wire enum):**

```
                       admission (auto-admit / registrar)
                                  │
                                  ▼
   ┌─────────────┐  first liveness / Ready=True  ┌──────┐ action assigned  ┌────────────┐
   │ Discovered  │ ─────────────────────────────►│ Idle │─────────────────►│ InProgress │
   │ (pending;   │                               │      │◄─────────────────│ (Working)  │
   │ Ready=      │                               └──┬───┘  action ends     └─────┬──────┘
   │ Unknown/    │           heartbeat timeout      │                            │
   │Initialising)│◄──────────────────┐   ┌──────────┴─────── heartbeat timeout ──┘
   └─────┬───────┘                   │   ▼
         │ heartbeat timeout       ┌───────────┐  live again (Ready=True) → Idle / InProgress
         └────────────────────────►│  Offline  │──────────────────────────────────────────────
                                   └───────────┘
```

- **"Initialising"** (no wire enum member) is represented as phase `Discovered`
  with the `Ready` condition `Unknown` / reason `Initialising` — a condition
  sub-state of `Discovered`, not a distinct phase.
- **"Ready"** (no wire enum member) is phase `Idle`: admitted, live, no action.
- **"Working" / "Executing"** is `InProgress`.

**Transition triggers and ownership:**

| Transition | Trigger | Owner |
|---|---|---|
| → `Discovered` | admission; no liveness yet | registrar / auto-admit; Robot controller (init) |
| `Discovered`/`Offline`/`""` → `Idle` | `Ready` True (live) **and** no assigned action | **Robot controller** |
| `Discovered`/`Offline`/`""` → `InProgress` | `Ready` True (live) **and** an assigned action present | **Robot controller** |
| `Idle` ↔ `InProgress` | action assigned / action ends | Scheduler (`fleetaction_controller`) |
| any → `Offline` | heartbeat older than the offline threshold | Robot controller |
| ↔ `Maintenance` | ZoneMaintenance window | ZoneMaintenance controller |
| `Charging` / `Error` | robot-reported | adapter / robot lifecycle |

The Robot controller owns exactly the **liveness-class** phases (`Discovered`,
`Offline`, empty). When a robot is live it advances *only* from one of those to the
steady summary derived from `status.assignedAction` (`InProgress` if set, else
`Idle`). It never overrides `Idle`/`Assigned`/`InProgress`/`Charging`/
`Maintenance`/`Error` — those belong to the scheduler, the maintenance controller,
and robot reporting. Deriving `InProgress` from a present `assignedAction` mirrors
what the scheduler already writes, so the two agree on a robot that is both live
and assigned.

After this change `Discovered` has a single meaning: **not yet live** (pre-admission
or awaiting first heartbeat). Admitted-and-live robots read `Idle`/`InProgress`.

## Alternatives considered

- **Add `Initialising` and `Ready` to the enum.** Rejected: the phase value set is
  the `fleet_adapter.v1` wire enum; adding members is a protocol change and breaks
  the mirror invariant. The conceptual states map onto `Discovered`+condition and
  `Idle` with no loss of information.
- **Let the scheduler own the Discovered→Idle transition too.** Rejected: a robot
  that never gets an action would still never leave `Discovered` — the scheduler
  only runs on assign/release. Liveness is the correct trigger, and liveness is the
  Robot controller's job.
- **Derive phase entirely from conditions on every reconcile (single writer).**
  Cleaner in theory, but it would fight the scheduler's authoritative
  `Idle↔InProgress` writes and risk racing the single-executor action binding
  (§9.6.3.5). Scoping the Robot controller to only the liveness-owned phases keeps
  a single owner per transition.
- **Project phase from telemetry frames.** Rejected outright: that is a per-tick
  status write and violates RA-1.

## Consequences

- **swarmtop is truthful:** a live admitted robot reads `Idle`/`InProgress`;
  `Discovered` now means only "not yet live."
- **New obligation:** the liveness-owned phase set (`Discovered`/`Offline`/`""`) and
  the derivation (`assignedAction` → `InProgress`/`Idle`) are a contract the Robot
  controller must keep consistent with the scheduler's `Idle↔InProgress` writes. If
  a future `Assigned` (assigned-but-not-executing) transition is introduced for
  robots, both owners must agree on it.
- **RA-1 preserved:** the advance is derived from the `Ready` condition, liveness,
  and `assignedAction`, and rides the existing material-change patch — no new write,
  none on a telemetry tick. It fires on the edge only (once off `Discovered`, the
  phase is no longer liveness-owned), so it does not churn.
- **Offline recovery improved:** recovery from `Offline` now lands on the correct
  steady phase (`Idle`/`InProgress`) instead of bouncing through `Discovered`.
- **Drawback accepted:** "Initialising" is not a first-class phase; a consumer that
  wants to distinguish "admitted, warming up" from "never admitted" must read the
  `Ready` condition reason, not the phase. This is the price of keeping the phase
  value set equal to the wire enum.
