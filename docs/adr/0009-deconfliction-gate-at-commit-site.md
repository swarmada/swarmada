# ADR-0009: Traffic-deconfliction reservation gate at the commit site

- **Status:** accepted
- **Date:** 2026-07-08
- **Deciders:** Maintainers
- **Related:** RFC-0001 §9.4 (Traffic Deconfliction Engine), §9.6.3.5 (Assignment Lease and the Single-Executor Guarantee)

## Context

The reference control plane separates two concerns that RFC-0001 keeps distinct:

- The **scheduler** (`internal/scheduler`) is a *pure decision function* — given a task and
  candidate robots, it selects one and returns it. It performs no writes and holds no state.
  The scheduling algorithm is explicitly pluggable and non-normative (Non-Goals).
- The **FleetTask controller** (`internal/controller`) is the *commit site* — its reconcile
  loop owns the `FleetTask` phase transitions, mints the assignment lease, and persists the
  assignment (the `Pending → Assigned` write). It is the single writer and re-runs idempotently
  across retries, restart, and failover.

The Traffic Deconfliction Engine (TDE) must reserve zone capacity and shared resources before a
robot is committed to a task, so that two robots are never granted a conflicting slot. The
decision this record captures is **where the reservation gate runs**: at decision time (in the
scheduler) or at commit time (in the FleetTask controller).

## Decision

The authoritative TDE reservation gate runs at the **commit site — the FleetTask controller's
`Pending → Assigned` transition — not in the scheduler.** The reservation is acquired
immediately before the assignment is written:

```
SelectRobot()                    // decision (scheduler; unchanged, pure)
RequestReservation(zone, robot)  // gate (TDE), at the commit site
  Reserved → mint lease, write Assigned, send AssignTask   // commit
  Denied   → stay Pending, set a bounded RetryAfter, requeue
any post-reserve failure → Unreserve (idempotent)          // reservationTTL is the backstop
```

- **Reserve before write.** The reservation (TDE state) and the assignment (Kubernetes API) are
  two systems, not one transaction; a crash between them must be recoverable — the next
  reconcile calls `Unreserve`, and `reservationTTLSeconds` is the safety net.
- **The scheduler stays a pure decision function.** It MAY read TDE state as a *non-binding
  hint* to avoid proposing robots into full zones, but that is an efficiency optimization only;
  it is never the authority.
- **Reservation lifecycle is owned by the controller:** `Reserved → Occupied` (on the Zone
  Controller's zone-entry signal) `→ released` (on task completion/failure/cancellation, estop,
  or expiry).

## Alternatives considered

- **Gate in the scheduler (decision time).** Rejected: it opens a time-of-check/time-of-use gap
  between deciding and committing (the zone can fill in between — the double-grant bug); the
  scheduler is not the writer and cannot hold a reservation across reconciles; and because the
  scheduling algorithm is pluggable, every custom scheduler would have to re-implement the
  safety gate correctly.
- **A validating admission webhook on the assignment write.** Rejected as the primary
  mechanism: a webhook can validate that a reservation exists but cannot own the stateful
  `Reserved → Occupied → released` lifecycle, and it adds latency to every write. It may
  supplement the gate but does not replace it.

## Consequences

- The single-executor guarantee and deconfliction hold regardless of which (pluggable)
  scheduler chose the robot, because enforcement is at the one place the assignment is written.
- The reservation lifecycle has a natural home in the FleetTask reconcile loop, reusing the
  existing lease machinery (§9.6.3.5) and the Zone Controller's zone-entry signal.
- The commit path must handle `Denied` as backpressure (requeue with bounded `RetryAfter`) and
  must `Unreserve` on any failure after reserving; correctness depends on reserve-before-write
  plus the reservation TTL, which are required, not optional.
- This is a reference-implementation architecture decision; the normative guarantees it upholds
  live in RFC-0001, and an alternative implementation may satisfy them differently.
