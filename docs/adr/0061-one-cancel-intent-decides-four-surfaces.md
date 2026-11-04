# ADR-0061: One cancel intent decides the object, the wire, the event and the audit entry — and an unattributable cancel stays unattributed

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, Kubernetes Expert
- **Related:** ADR-0046 (e-stop actor stamped at admission), ADR-0060 (preemption disposition is selected by action type), RFC-0001 #fleetaction-directive-supersession; `internal/controller/supersession.go`, `internal/command/action.go`

## Context

RFC-0001 requires that an operator cancellation and a directive supersession stay
distinguishable "on the object, on the wire, and in the audit trail." A Kubernetes
Event is a fourth surface, since it is what an operator actually watches.

The measurement, taken 2026-09-15, found those four surfaces being decided
**independently, from one free-text `reason` string**. A string that happened
to read "cancelled by operator" carried no machine-readable cause onto the wire, set
no `cancellationReason` on the object, and chose no audit verb. The consequences were
measurable rather than theoretical:

- every cancel went out as `CANCEL_CAUSE_UNSPECIFIED`, so **an adapter could not tell
  a supersession from an operator stop** and could not echo the matching
  `ActionEndCause` back on its terminal report;
- `cancellationReason` and `status.supersededBy` existed in the CRD and were written
  by nothing (`grep -rn 'CancellationReason' --include='*.go'` outside `api/v1` and
  tests returned nothing);
- the audit trail recorded one verb, `FLEETACTION_CANCELLED`, for both causes.

Phase cannot be used to tell them apart, and RFC-0001 is explicit that an
implementation must not try: phase is `Cancelled` for a supersession and for an
operator cancellation alike.

A second, separate question surfaced in the same measurement. `spec.desiredState:
Cancelled` is written **either** by a person **or** by the FleetTask
FailFast/Compensate path, and at the point the controller acts on it nothing can tell
which.

## Decision

**A cancellation is decided once, as an intent, and the intent writes all four
surfaces.** Three constructors — `operatorCancelIntent`, `supersededCancelIntent`,
`unattributedCancelIntent` — produce a `cancelIntent` that carries the object
attribution, the wire cause, the Kubernetes Event reason and the audit verb together,
so the four cannot drift by construction rather than by review.

- **Object.** `applyCancelAttribution` writes `cancellationReason`, `supersededBy`,
  and the `Superseded` condition with reason `InPlacePreemption` or
  `CancelThenDispatch`.
- **Wire.** `command.CancelAction` carries a `Cause` field over the existing proto
  `CancelCause` values (`internal/command/action.go:124`); `PushCancelAction` sets it.
- **Events.** `ActionCancelledByOperator`, `ActionSupersededInPlace`,
  `ActionSupersededAfterCancel`, each emitted as **`Normal`**. A supersession is the
  fleet working as designed and an operator cancel is a deliberate act; grading either
  `Warning` trains people to ignore the channel that carries real faults.
- **Audit.** `EventFleetActionSuperseded = "FLEETACTION_SUPERSEDED"` is selected by the
  intent in place of `FLEETACTION_CANCELLED`, with `supersededBy` in the detail.

> **Implementation note (2026-09-24).** The list above describes what each intent carries,
> not what is reached. `supersededCancelIntent` has no production caller: no control-plane
> path supersedes a directive, so the `CancelThenDispatch` condition,
> `ActionSupersededAfterCancel` and the control-plane `FLEETACTION_SUPERSEDED` entry are built
> but unreachable. The `InPlacePreemption` surfaces are written only by the ActionStatus
> ingestor on an adapter-reported `ACTION_END_CAUSE_SUPERSEDED`, which no in-tree adapter
> sends. The operator surfaces (`cancellationReason: Operator`, `ActionCancelledByOperator`)
> are reached. RFC-0001 {{ref:fleetaction-directive-supersession}} grades this as partial.

**An unattributable cancellation is recorded with no actor.** `spec.desiredState:
Cancelled` does **not** set `cancellationReason: Operator`. RFC-0001 provides for
exactly this — empty means no controller stated a cause — and a test asserts the
emptiness so that a later author does not "fix" it.

## Alternatives considered

- **Keep deriving each surface from the free-text `reason` at each site.** The status
  quo. Rejected: it is what produced four surfaces disagreeing, and it makes
  correctness a property of every future call site rather than of one type.
- **Parse the cause back out of the `reason` string.** Rejected. It makes a
  human-readable message load-bearing, so an editorial improvement to the wording
  becomes a behaviour change.
- **Derive the cause from the action's phase or from `supersededBy` being non-empty.**
  Rejected for the object surface: phase is identical for both causes by design, and
  inferring the cause from a field the same code is responsible for setting only moves
  the inconsistency rather than removing it.
- **Default `spec.desiredState: Cancelled` to `cancellationReason: Operator`.**
  Rejected, and this is the load-bearing rejection: it is right whenever a person
  cancelled and wrong for **every** composite-driven cancel, which would put a false
  actor in the audit trail of an automated action. An audit trail that names the wrong
  actor is worse than one that names none, because only the second is legible as a
  gap. ADR-0046 takes the same position for e-stop from the other direction — where an
  actor *can* be established, it is stamped at admission rather than inferred later.
- **Emit the supersession events as `Warning`.** Rejected; see above.

## Consequences

- Adding a new cause in future means adding a constructor, not editing four call
  sites — the surfaces stay consistent by construction.
- Adapters can now distinguish a supersession from an operator stop and echo the
  matching `ActionEndCause`. This is additive: `CANCEL_CAUSE_UNSPECIFIED` remains
  legal and is what an unattributed cancel sends.
- Tests assert the **distinguishing** facts and never the phase. Several assert a
  negative (no child action exists, the victim was not committed, a `Paused` action
  did not move), which is the harder property to hold and the easier one to lose.
- The audit trail gains a second verb for an event class that previously had one.
  Consumers that matched `FLEETACTION_CANCELLED` exhaustively will no longer see
  supersessions under it — intended, and the reason the verb is separate rather than a
  detail field.
- **A deliberate hole remains:** a cancellation arriving through `spec.desiredState`
  carries no actor at all, so the audit trail cannot answer "who cancelled this" for
  that path. Closing it needs provenance at the point the field is written, not a
  guess at the point it is read.
- The control plane reads `end_cause` and `superseding_action_id` from adapter reports
  but does not **send** them — nothing in-tree emits `ActionStatusUpdate` toward an
  adapter, so there was no site to change. Recorded so the next reader does not assume
  symmetry.
