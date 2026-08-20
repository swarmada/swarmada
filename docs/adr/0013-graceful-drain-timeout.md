# ADR-0013: Bound Graceful zone-maintenance wind-down with a drain timeout

- **Status:** accepted
- **Date:** 2026-07-09
- **Deciders:** Distributed Systems Reviewer, API Designer, Kubernetes Expert
- **Related:** RFC-0001 §9.1.11 (SwarmadaConfig maintenance defaults, ZoneMaintenance
  lifecycle), §9.6.3.5 (lease / single-executor safety), the RA-1 status-write
  discipline

## Context

A `ZoneMaintenance` in `Graceful` mode pauses Idle robots immediately and lets
InProgress robots *wind down* — finish their current task, then pause once they
report Idle. Today that wind-down is **unbounded**: a robot on a stuck or very long
task stays in `WindingDownRobots` forever, and the maintenance never fully drains.
`Immediate` mode has no such problem — it force-requeues in-progress tasks at once.

`SwarmadaConfig.spec.maintenance.defaultGracefulDrainTimeoutSeconds` (default 300)
is defined precisely to bound this — "after this duration in Graceful mode, robots
are force-paused even if tasks are not yet complete" — but nothing reads it. Two
facts shape the fix:

- The controller already records `status.activatedAt` when a maintenance goes
  Active, giving a natural deadline anchor.
- The controller already has a safe force-pause path: `requeueTask` sets the
  `requeue-requested` annotation, and the FleetTask controller confirmed-stops the
  robot (single-executor safe, §9.6.3.5) and returns the task to Pending, freeing
  the robot to be paused. `Immediate` mode uses exactly this.

There is no per-`ZoneMaintenance` drain-timeout field — only the namespace default.

## Decision

**In `Graceful` mode, force-pause a still-winding-down robot once the maintenance
has been Active longer than the resolved graceful-drain timeout**, by routing it
through the same `requeueTask` path `Immediate` mode uses.

Specifics:

- **Timeout source.** Resolve `defaultGracefulDrainTimeoutSeconds` per namespace via
  the shared `namespaceConfig()` helper, failing safe to a 300s constant. No new
  `api/v1` field: the namespace default is the only knob today, and a per-resource
  override can be added later without contradicting this decision.
- **Deadline anchor.** `status.activatedAt + drainTimeout`. A single per-maintenance
  deadline (not per-robot) matches the field's wording ("after this duration in
  Graceful mode") and needs no new per-robot bookkeeping.
- **Action past the deadline.** For each in-scope robot still executing a task, call
  `requeueTask` — identical to `Immediate`. The robot is confirmed-stopped by the
  FleetTask controller and paused on a subsequent reconcile. Robots are still listed
  in `WindingDownRobots` until they actually reach Idle, so status stays truthful.
- **Cadence.** The existing 30s `zmReconcileInterval` requeue catches the deadline
  within one interval; no new timer is required.

## Alternatives considered

- **Per-robot drain deadline** (measured from when each robot began winding down).
  Rejected at v0.3: it needs a per-robot "windingDownSince" timestamp in status and
  more bookkeeping, for a semantic the field wording does not ask for. The
  maintenance-level deadline is simpler and sufficient; per-robot can be a later
  refinement if operators need it.
- **Add a per-`ZoneMaintenance` `gracefulDrainTimeoutSeconds` field now.** Rejected
  to keep this a controller-only change with no schema churn. The resolution seam
  (per-resource → namespace → constant) is established by ADR-0012 and can be
  extended here later if demand appears.
- **Escalate to `Immediate` by flipping `spec.mode`.** Rejected — the controller must
  not mutate operator spec; the timeout is a controller behavior, and `spec.mode`
  should keep reflecting the operator's stated intent.
- **Hard-cancel the task instead of requeue.** Rejected — requeue (return to Pending)
  is the less destructive, already-safe path; cancellation semantics are the
  operator's call via the task API, not a maintenance side effect.

## Consequences

- **Good.** Graceful maintenance now completes deterministically instead of hanging
  on a stuck task; the long-defined timeout finally does something, via the same
  fail-safe config pattern as the other health/maintenance tunables.
- **Good / RA-1.** No new status writes: the force-pause reuses the existing
  annotation path and the pause itself rides the existing transition-driven writes.
  The timeout read is read-only.
- **Safety.** Unchanged from `Immediate`: `requeueTask` is single-executor-safe —
  the robot is confirmed-stopped before the task is freed, so the drain timeout can
  never cause a double-execution.
- **Obligation — tests.** Envtest/unit coverage for: before the deadline a Graceful
  robot keeps winding down; past the deadline its task is requeued; the fail-safe
  (unreadable config → 300s); and that `Immediate` behavior is unchanged.
- **Drawback accepted.** A robot mid-task is interrupted when the timeout elapses —
  that is the explicit trade the timeout exists to make. Operators who want
  unbounded wind-down set a large value; a per-namespace default governs the rest.
  There is no per-maintenance override yet, so a single maintenance that needs a
  different bound must wait for the namespace default or the future field.
