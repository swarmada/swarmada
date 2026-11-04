# ADR-0060: The disposition of a preemption is selected by action type, and neither an e-stop nor a paused action is preemptable

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, Kubernetes Expert
- **Related:** ADR-0016 (estop delivery config), ADR-0034 (preferredRobot is a soft preference), ADR-0045 (a desiredState hold is operator-lifted), ADR-0061 (one cancel intent across four surfaces), ADR-0068 (capability-loss reroute is a confirmed-stop requeue), RFC-0001 #fleetaction-directive-supersession "Directive supersession"; `internal/controller/supersession.go`

## Context

A directive supersession is the case where the fleet commits action B to a robot
that is already executing action A. RFC-0001 #fleetaction-directive-supersession specifies two dispositions —
**D1**, preempt in place, and **D2**, cancel then dispatch — and requires that the
two causes of a cancellation stay distinguishable. The question this ADR settles is
*which disposition applies when*, and *what may be superseded at all*.

The starting state, measured on 2026-09-15, was that the API surface looked finished
and the behaviour was absent:

- `CancellationReason` (`Operator` | `SupersededByDirective` | `CapabilityLoss`) and
  `status.supersededBy` were already in the CRD, with a unit test asserting they
  round-trip. **`grep -rn 'CancellationReason' --include='*.go'` outside `api/v1` and
  tests returned nothing at all** — nothing in the tree wrote either field.
- The wire already carried `CancelAction.cause` with a full `CancelCause` enum and
  `ActionStatusUpdate.end_cause` + `superseding_action_id`. **No Go code read or wrote
  any of it**, so every cancel left the control plane as
  `CANCEL_CAUSE_UNSPECIFIED`.
- The one place the machinery had a real caller was **wrong**. The priority-preemption
  handover dispatched its preemptor to the evicted robot identically regardless of
  action type, so a `Critical` action of a *different* type took a committed robot
  **with no confirmed stop**. RFC-0001 says such a dispatch "executes that dispatch
  through the applicable disposition"; the code answered "whether to preempt" and
  "how to preempt" in one branch.

So the decision was not whether to build a supersession mechanism — the fields, the
enum and the wire were already there — but what selects between D1 and D2, and which
phases are eligible at all. Those questions have a safety edge: D1 sends **no**
`cancel_action`, therefore waits for **no** confirmed stop.

## Decision

**The disposition is a pure function of the two action types, and nothing else.**
`DispositionFor(a, b)` (`internal/controller/supersession.go:111`) selects **D1
(in-place)** when `B.spec.type == A.spec.type` and **D2 (cancel-then-dispatch)**
otherwise. A nil operand defaults to D2 — the direction that does *not* skip the
confirmed stop. Being a pure function of two types, it is unit-testable with no
cluster, no robot and no wire.

**Priority decides *whether*; the type decides *how*.** The priority-preemption
handover now executes through the same disposition: a type-changing handover requires
a confirmed stop of the victim before the commit, releasing the TDE slot and
requeueing while it waits.

**The victim of a priority preemption is requeued, never marked superseded**
(`Preempted` → `Pending`, `retryCount` unchanged). It is work the fleet still owes,
and that is precisely what separates a priority preemption from a directive
supersession.

**`MaySupersede` admits only `Assigned` and `InProgress`**
(`internal/controller/supersession.go:141`). `Paused` is excluded explicitly, and
**an e-stop is not a preemption**: an action paused by an e-stop or a
`ZoneMaintenance` window is operator-gated (ADR-0045), and superseding it would
resolve — automatically and silently — an action whose resolution the pause
deliberately handed to a human. The `ActionStatusUpdate` ingestor enforces the same
phase guard at the wire boundary, so a *robot report* claiming a supersession cannot
lift a human's hold either.

**D1 is driven by the adapter's terminal report, and only when the adapter says so.**
Under D1 no `cancel_action` is sent, so there is no confirmed stop to wait on and no
other path by which the victim reaches a terminal phase — the adapter picks the safe
stopping point and reports it. The ingestor therefore reads a supersession only from
an explicit `ACTION_END_CAUSE_SUPERSEDED`; an untyped `CANCELLED` report (`end_cause`
UNSPECIFIED, which is what every pre-RFC adapter sends and which is explicitly "no
cause stated") falls through unchanged. A `SUPERSEDED` report carrying no
`superseding_action_id` is **refused rather than written half-formed** — `supersededBy`
is what makes the distinction structural, so a supersession that cannot name its
successor is malformed.

## Alternatives considered

- **Select the disposition by priority, urgency, or a `spec` field on B.** Rejected.
  Priority already answers a different question (whether this robot should be taken
  at all), and a caller-chosen disposition lets an operator or a trigger request D1
  across a type change — which is exactly the case that must not skip the confirmed
  stop. Making the rule a function of the types removes the choice from the caller.
- **Always D2 (always cancel first).** Safe, uniform, and the behaviour a nil operand
  still falls back to. Rejected as the general rule because it makes the common case
  — a new waypoint replacing the current waypoint — pay a full stop-and-redispatch
  cycle, which is both slower and rougher on the robot than letting the adapter
  retarget in place.
- **Always D1 (always in place).** Rejected outright: a type change with no confirmed
  stop is the failure the priority handover was already committing.
- **Let a robot report drive any cancellation.** Rejected. Accepting an untyped
  `CANCELLED` as a supersession would let a pre-RFC adapter's ordinary terminal
  report silently rewrite attribution, and accepting one over a `Paused` action would
  let a robot lift an operator's hold.
- **Treat e-stop as the highest-priority preemption.** Rejected. It collapses a safety
  stop and a scheduling decision into one mechanism; ADR-0016 keeps e-stop on its own
  path precisely so nothing about work eligibility can gate it.

## Consequences

- The D1/D2 rule is decidable without a cluster, so it is covered by ordinary unit
  tests; 15 supersession tests accompany the change and pass (`make test` exit 0,
  `internal/controller` coverage 82.8%).
- Cancels now carry a machine-readable cause on the wire for the first time, which is
  a behaviour change visible to adapters. It is additive: `CANCEL_CAUSE_UNSPECIFIED`
  remains a legal value and is what an adapter sees for any cancel the control plane
  cannot attribute (ADR-0061).
- **Two of the three entry points are unreachable at runtime, and that is a known,
  recorded gap, not an oversight.** Nothing in the control plane initiates a
  supersession: `spec.preferredRobot` is a soft preference among *available* robots
  and displaces nobody (ADR-0034), and `robotSelector` pins without preempting. So
  today **D1 is reachable only when an adapter reports
  `ACTION_END_CAUSE_SUPERSEDED`, and D2 only through the priority-preemption
  handover.** A directive intake — an operator or trigger saying "run B on robot R
  now, replacing what R is doing" — is a new API surface and was deliberately not
  invented here.
- The priority handover is now slower in the type-changing case, by exactly one
  confirmed stop. That latency is the point.
- New obligation on adapters: an `AssignAction` whose `action_type` differs from the
  one being executed must be rejected with
  `ASSIGN_ACTION_REJECTION_ROBOT_BUSY`. The control-plane half is built; the
  adapter-side conformance check for the D1/D2 split is not written.
- RFC-0001 #fleetaction-directive-supersession's maturity note is now substantially false — it still says no
  controller selects a disposition and that `cancellationReason`/`supersededBy` are
  written by nothing. Six of its eight claims no longer hold; the surviving true one
  (nothing initiates a supersession) must survive the rewrite, or the correction swaps
  one false statement for another.
