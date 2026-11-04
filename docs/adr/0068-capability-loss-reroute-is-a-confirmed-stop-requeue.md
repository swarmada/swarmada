# ADR-0068: A capability-loss reroute is a confirmed-stop requeue, and the adapter's `CancelDisposition` decides the outcome

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, QA Manager
- **Related:** ADR-0013 (graceful drain timeout), ADR-0031 (hardware status Disabled), ADR-0034 (preferredRobot is a soft preference), ADR-0060 (preemption disposition is selected by action type), ADR-0061 (one cancel intent across four surfaces); `internal/controller/fleetaction_controller.go`, `internal/controller/robot_controller.go`, `proto/fleet_adapter/v1/fleet_adapter.proto`

## Context

When a robot loses a capability an in-flight action depends on — a camera degrades
mid-inspection — the fleet must move that work to another robot. The question is
*how*: a reassignment write that points the action at a new robot, or a full
cancel-confirm-requeue cycle.

The project's own honesty ledger described this as unfinished: *"Capability-loss
reroute ... is specified and partly implemented; the
degrade/recover arc is on the wire today, the reroute is the target behavior."*

A re-measurement on 2026-09-14, amended 2026-09-15, determined what is actually true in
the tree, re-deriving it rather than re-confirming it. **The ledger sentence
is stale.** The measured findings:

1. **The derivation chain exists end to end.** `HARDWARE_STATUS_DEGRADED` arrives on
   `TelemetryPayload.hardware` and is translated at
   `internal/controlstream/translate.go:109-110`; `evaluateCapability`
   (`internal/controller/robot_controller.go:641-687`) walks `capDef.RequiredHardware`
   and returns `CapabilityStatusDegraded`; `RobotSatisfiesActionCapabilities` then
   fails the robot, and the FleetAction controller acts on it.
2. **The reroute is not a status flip.** `beginCapabilityLossReassignment`
   (`internal/controller/fleetaction_controller.go:1841`) marks the action with
   `annRequeueRequested = reasonCapabilityLost`, emits
   `ActionReassignedCapabilityLoss`, and hands off to the existing confirmed-stop
   requeue path: `cancel_action` → confirmed stop → `Pending` → rescheduled.
   Single-executor is preserved through the same gate `ZoneMaintenance` Immediate uses.
3. **The part a code comment called "a later change" has landed.** The proto carries
   `CancelActionResult.CancelDisposition disposition = 4`, and `handleRequeue`
   (`:1726`) branches on all three dispositions.
4. **It is tested and the tests pass**: `TestReconcile_CapabilityLoss_InitiatesReassignment`,
   `TestReconcile_CapabilityLoss_AcceptDegradedNoReassign`,
   `TestCapabilityLoss_StoppedSafely_Requeues`, `TestCapabilityLoss_Recovered_Fails`,
   `TestCapabilityLoss_Completed_NoReassign` — plus 16 further requeue/lease tests in
   the same run.
5. **It has been observed on the wire four times**, on four separate fresh clusters, the
   most recent live on 2026-09-15:
   *"reroute confirmed OK — inspect-receiving-dock moved from sim-robot-001 to
   sim-robot-002"*, with no script trigger and no status patch.

What that measurement leaves to decide is not whether the mechanism exists but what it
**is**, and whether the three outcomes of a cancel are the control plane's to infer or
the adapter's to report.

## Decision

**A capability-loss reroute is a cancellation followed by a requeue, not a
reassignment.** The action goes back to `Pending` and is rescheduled through the normal
scheduling path; it is never repointed at a new robot in place.

**The confirmed stop is mandatory and is reached through the same path
`ZoneMaintenance` Immediate uses.** Capability loss is not a special case with its own
stopping logic; it borrows the one that already preserves single-executor.

**The adapter's `CancelDisposition` decides the terminal outcome, and the control plane
does not guess it.** `handleRequeue` branches on all three:

- `CancelRecovered` → `finalizeCapabilityLossFailure` (the robot recovered mid-cancel;
  the action is not silently re-run),
- `CancelCompleted` → clear the annotation and let the normal Succeeded path settle (the
  work finished before the stop landed; requeueing it would repeat physical work),
- otherwise → requeue.

**`acceptDegraded` remains the operator's opt-out.** An action may declare that a
degraded capability is acceptable, in which case no reassignment begins —
`TestReconcile_CapabilityLoss_AcceptDegradedNoReassign` pins it.

**The honesty ledger's sentence is superseded.** The reroute is implemented, wired,
tested and observed; it is no longer "the target behavior."

## Alternatives considered

- **Reassign in place**: write the new robot onto the action and let the old robot's
  adapter notice. Rejected — it breaks single-executor. For a window bounded only by
  adapter latency, two robots believe they own the same action, and the first one is
  still physically moving.
- **Fail the action and let a higher layer resubmit.** Rejected: it loses the action's
  identity, its retry accounting and its audit chain, and pushes a recovery decision
  that the control plane has all the information for onto an operator or a task
  composer.
- **Let the control plane infer the outcome from the robot's subsequent telemetry**
  rather than reading `CancelDisposition`. Rejected: the two interesting cases —
  recovered, and completed-before-the-stop — are indistinguishable from the outside for
  as long as it takes telemetry to settle, and guessing wrong either repeats physical
  work or abandons completed work.
- **Treat capability loss as a supersession** and reuse the D1/D2 machinery (ADR-0060).
  Rejected: a supersession names a *successor action* that displaces this one, and here
  there is none — the same action is still owed, by a different robot. The distinction
  is the same one ADR-0060 draws between a priority preemption (victim requeued) and a
  directive supersession (victim superseded). `CancellationReason` keeps them separate
  on the object: `CapabilityLoss` is its own value.
- **Wait for the capability to recover instead of rerouting.** Rejected as the default;
  it is available explicitly via `acceptDegraded` for actions that tolerate degradation,
  which puts the choice on the action rather than in the controller.

## Consequences

- A reroute costs a full stop-and-reschedule cycle. That latency is the price of never
  having two robots committed to one action.
- The reroute depends on adapters implementing `CancelActionResult.disposition`
  faithfully. An adapter that always returns the default disposition degrades to
  "always requeue", which is the safe direction but can repeat work that had already
  completed.
- **Two stale statements were found and are not this ADR's to fix.** The honesty
  ledger's sentence, and the docstring at `fleetaction_controller.go:1672-1675` which
  still calls the disposition branch "a later change" — the change is in, so the comment
  is now stale in the other direction.
- The reroute is demonstrable end to end from a fresh cluster today: `UC-2.1`/`UC-2.2`
  run green after `examples/demo-ab-live/run.sh:436` was pinned to contract version
  `1.2.0`. The demo's prior failure was a fabricated conformance
  report in a shell literal, not a protocol defect — the control plane's version gate
  (ADR-0032) behaved exactly as specified in refusing it.
- RA-1 holds: the requeue transition is written once per transition, never on a
  telemetry tick.
