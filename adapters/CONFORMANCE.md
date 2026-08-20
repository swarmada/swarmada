# Fleet Adapter Conformance

This document defines what a Fleet Adapter **must**, **should**, and **may** do
to be a conforming Swarmada adapter. It is derived from the `fleet_adapter.v1`
contract and RFC-0001 §5.3; where this document and RFC-0001 differ, RFC-0001
governs. The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are used
as in RFC 2119.

Conformance is what makes an adapter part of the standard — not whether it lives
in this repository. Every adapter, in-tree or vendor-owned, is expected to pass
the checks below.

> **Status:** This document is the conformance *specification*. An executable
> harness that drives an adapter under test against these checks lives in
> [`conformance/`](conformance/) and drives **C0 through C16**: C0 (report
> self-checks), C1 (handshake), C2 (registration and reconnect reconciliation),
> C3 (fencing-token ordering and durability), C4 (assignment leases, including the
> self-stop), C5 (confirmed estop), C6 (telemetry discipline and explicit
> presence), **C7** (optional-command decline via `unsupported=true`), **C8** (edge
> stream — the harness serves `EdgeService`, advertises an edge endpoint on the
> `RegisterAck`, and checks the adapter dials it and honours an edge-issued estop
> with a confirmed `EstopAck`), C9–C12 (probes, update progress, pause/resume,
> capability scan — each optional), C13–C15 (contract-version negotiation, estop
> version-invariance, version-bound conformance) and C16 (task status reporting).
> A check the adapter does not exercise is recorded `skip` with an enumerated
> cause (the SkipCause enum in `conformance/report.py`) rather than free text; a `skip`
> records what was and was not verified, and is not a failure. Run it with `make
> conformance` (see [`conformance/README.md`](conformance/README.md)).

**Catalog titles and report titles differ on purpose.** An entry here states the
**obligation** — the MUST or MUST NOT an adapter is being held to. The harness reports the
**evidence** — the property it actually observed. `C4.1` is the clearest case: the catalog
states *"Treat a held execution lease as valid only while the control plane renews it"*,
while the report reads *"Reports the task still running across lease renewals"*, because
that observable is what a black-box adapter test can establish. Reading the two as a
mismatch is the wrong conclusion; they answer different questions, and the report is
deliberately narrower than the obligation it supports. Where the evidence is narrower in a
way that matters, the gap is named in the entry itself rather than left to inference.

## C0 — Report integrity (self-checks on this suite)

- **C0.1 (MUST)** A skip reason must be consistent with what the run observed. Skips
  record an enumerated cause rather than free text, and a cause that the run itself
  contradicts — `no-telemetry-in-run` recorded in a run where telemetry *was* observed —
  fails. A conformance report's account of what it did **not** verify is part of its
  output, and an unchecked justification drifts exactly like any other unverified claim.

### C0.x — harness context

`C0.2` and above are recorded by the harness about **the run**, not about the adapter. They
state what the suite established about *itself* before asserting anything, and they exist
because a precondition that fails silently is how a check comes to assert the harness instead
of the robot.

**C0.x rows do NOT count toward conformance.** A failing C0.x row does not make an adapter
non-conformant; it **invalidates the checks that depend on it**, whose results must then be read
as unestablished rather than as passes. That distinction is the entire reason these carry their
own prefix instead of a catalog id.

- **C0.2 (context)** The `negotiated_contract_version` the harness set on its own `HelloAck`. Harness
  state, not an adapter behaviour.
- **C0.3 (context)** A matching `lease_generation` renews — the precondition for C4.3 and C4.4.
- **C0.4 (context)** A fresh (higher) fencing token is accepted — the precondition for C3.2's stale
  rejection, which cannot be demonstrated without first establishing a high-water mark.

## C1 — Connection and handshake

- **C1.1 (MUST)** Dial the control plane as a gRPC client and open both
  `ControlStream` and `SafetyStream` in the same dial step.
- **C1.2 (MUST)** Send `AdapterHello` as the first `AdapterMessage` on
  `ControlStream`, carrying the `contract_version` this adapter implements and
  `protocol_version` as the wire package identity (`fleet_adapter.v1`), and
  not proceed until `HelloAck.accepted` is true. A control plane that accepts the
  hello but returns no `negotiated_contract_version` has refused REGISTRATION for
  that connection (ADR-0032): the adapter stays observable and stoppable, but
  `RegisterRobot` and `DiscoverRobot` are rejected `VERSION_MISMATCH`. A missing
  or unparseable `contract_version` is treated as incompatible, never as a pass.
- **C1.3 (MUST)** Present a client certificate for mTLS; treat that identity as
  the adapter's identity and set no `robot_id` outside the set the identity is
  authorized for.
- **C1.4 (MUST NOT)** Declare itself healthy if `SafetyStream` cannot be
  established; retry with backoff instead.
- **C1.5 (SHOULD)** Reconnect with bounded exponential backoff on stream loss.

## C2 — Robot lifecycle

- **C2.1 (MUST)** Send `DiscoverRobot` for a robot with no `Robot` custom
  resource, and `RegisterRobot` for one that already has a `Robot` CRD; never
  the reverse.
- **C2.2 (MUST)** On `RegisterAck`, adopt `authoritative_action_state` as the
  source of truth and reconcile local state to it (detecting divergence after a
  connectivity gap).
- **C2.3 (MUST)** Honor the per-robot configuration returned in `RegisterAck`
  (`telemetry_interval_seconds`, `assigned_zone`, `active_capabilities`,
  `edge_endpoints`).

## C3 — Fencing tokens (anti double-execution)

- **C3.1 (MUST)** Persist the highest accepted fencing token per robot so it
  survives an adapter restart.
  **This suite verifies only the across-reconnect half** — that a faulted and redialled
  stream does not reset the mark. Durability across a process **restart** is C3.7, and it
  is recorded SKIP: no reference adapter persists the mark anywhere, so the requirement
  has no implementation in the project to test.
- **C3.2 (MUST)** Reject any `AssignAction` or `CancelAction` whose `fencing_token`
  is less than or equal to the highest accepted token for that robot, returning
  the matching `STALE_FENCING_TOKEN` / stale rejection.
- **C3.3 (MUST)** Reject `AssignAction` with `MISSING_FENCING_TOKEN` when the
  token is absent (distinguish "absent" from a value of `0`).
- **C3.4 (MUST)** Idempotently re-ack an identical re-delivery of the current
  assignment rather than treating it as stale.
- **C3.5 (MUST)** Echo the fenced token in `AssignActionResult.accepted_fencing_token`.
- **C3.6 (MUST)** Report a valid `CancelDisposition` on `CancelActionResult`, so the
  control plane can tell a safe stop from a completion or a recovery.
- **C3.7 (MUST)** The persisted high-water mark survives an adapter **process restart**,
  not only a stream reconnect. A reconnect does not touch process memory, so an adapter
  holding the mark in a plain dict passes C3.1 and fails this. The hazard is a control
  plane re-issuing tokens to an adapter that restarted and forgot: two executors accepting
  the same assignment, which is what C3 exists to prevent.

## C4 — Assignment leases (self-stop)

- **C4.1 (MUST)** Treat a held execution lease as valid only while the control
  plane renews it via `renew_lease` within `lease_duration_ms`.
- **C4.2 (MUST)** Bring the current task to a safe stop if no renewal arrives
  before the self-stop timer fires.
- **C4.3 (MUST)** Ignore a `RenewActionLease` whose `lease_generation` is stale
  and not reset the self-stop timer for it.
- **C4.4 (MUST)** Report `running=false` in `RenewActionLeaseResult` when the
  robot is no longer executing the task, so silent completion during a gap is
  detectable.

## C5 — Emergency stop (confirmed, never inferred)

- **C5.1 (MUST)** Accept `Estop` on `SafetyStream` and act on it immediately;
  estop is **not** fenced and is always honored.
- **C5.2 (MUST)** Return `EstopAck` on `SafetyStream`, setting `state` to
  `ESTOP_STATE_STOPPED` only when the robot is confirmed halted in a safe state,
  and `ESTOP_STATE_FAILED` when a stop cannot be confirmed.
  This constrains **when an adapter may say STOPPED**; it does not require the FIRST
  ack to be terminal. An adapter MAY answer immediately with
  `ESTOP_STATE_STOPPING` ("command received; robot is decelerating") and send the
  terminal `STOPPED`/`FAILED` when rest is confirmed. C5.5 bounds the first ack;
  this entry governs the terminal one. A single terminal ack remains conformant.
- **C5.3 (MUST NOT)** Report `STOPPED` by inferring it from absent motion, a
  timeout, or telemetry; the state MUST be adapter-confirmed. An `EstopAck`
  reporting `ESTOP_STATE_STOPPED` MUST populate `stop_initiated_at` — whether an
  adapter *inferred* a stop is not decidable from the wire, and that timestamp is
  the only artefact attesting the stop was commanded. Necessary evidence, not
  proof.
- **C5.5 (MUST)** Answer an `estop` on `SafetyStream` with an `EstopAck` within
  500 ms. Measured by the harness from queueing the `Estop` to dequeuing the ack,
  so the harness's own transport is included and the figure is conservative.
  This bounds the **first** ack, which MAY be `ESTOP_STATE_STOPPING`. It is an
  acknowledgement budget, not a stopping-distance budget: a real base takes
  substantially longer to reach confirmed rest than 500 ms, and RFC-0001's
  SafetyStream row already requires the physical stop to BEGIN before the ack —
  a clause that only has meaning if the ack may precede confirmed rest.
- **C5.6 (MUST)** An `EstopAck` reporting `ESTOP_STATE_STOPPING` MUST be followed by
  a terminal `ESTOP_STATE_STOPPED` or `ESTOP_STATE_FAILED`. Acking STOPPING and never
  following up satisfies C5.5 while telling the control plane nothing, leaving the
  robot in an unresolved emergency. The permitted interval is a property of the base,
  not a constant in this document — it is bounded here by a documented harness value
  pending an adapter-declared figure in `CapabilitiesSnapshot`.
- **C5.4 (MUST NOT)** Carry estop traffic on `ControlStream`; the deprecated
  `Command.estop` / `CommandResult.estop` arms exist only for wire
  compatibility and MUST NOT be used by new adapters.

## C6 — Telemetry and status discipline

- **C6.1 (MUST)** Send the first `TelemetryPayload` after (re)connect as a full
  snapshot of all hardware components, and subsequent payloads as deltas.
- **C6.2 (MUST)** Preserve proto3 explicit presence on safety-relevant scalars
  (`RobotPosition`, `BatteryStatus`): never send a defaulted `0`/`false` where
  the true reading is unknown, and never interpret a received absent field as
  `0`/`false`.
- **C6.3 (MUST NOT)** Cause the control plane to write `Robot` status on a
  telemetry tick; telemetry is projected, not written to status per tick
  (the RA-1 status-write discipline). An adapter MUST NOT depend on per-tick
  status writes.
- **C6.4 (MUST)** Echo the executing `fencing_token` in `ActionStatusUpdate` so a
  robot acting on a superseded assignment can be detected.

## C7 — Optional operations

- **C7.1 (MAY)** Decline an optional command (`VerifyHardware`, `PushFirmware`,
  `ModelUpdate`, reservations, zone admission, and similar) by returning
  `CommandResult.unsupported = true` with an empty result.
- **C7.2 (MUST, when implemented)** For `PushFirmware` / `ModelUpdate`, verify
  the artifact checksum and, when signature verification is required, verify the
  signature against the configured trust roots and fail closed on failure.

## C8 — Edge stream (when a zone declares an edge node)

- **C8.1 (MUST, when `edge_endpoints` is non-empty)** Dial each edge endpoint
  and hold an `EdgeService` stream with the same mTLS and reconnect discipline
  as `ControlStream`.
- **C8.2 (MUST)** Tee a `PositionFrame` per managed robot per telemetry tick to
  every connected edge node, fire-and-forget.
- **C8.3 (MUST)** Act on an edge-issued `Estop` and return a confirmed
  `EstopAck`, exactly as on `SafetyStream`.
- **C8.4 (MUST NOT, EDGE NODE — not an adapter obligation)** Interpret the
  absence of position frames as a boundary breach or a reason to stop. The party
  that observes silence on the tee is the Zone Controller edge node, which
  Swarmada owns; an adapter cannot violate this because it is not the observer.
  Listed here for completeness of the EdgeStream contract and verified in the
  edge node's own tests, not by this suite.

## C9 — Active verification results (when `verify_*` is implemented)

- **C9.1 (SHOULD, when implemented)** For a `VerifyHardware` / `VerifyCapability`
  / `VerifyModel` it answers (rather than declining per C7.1), return a
  `VerifyResult` populating `actual_metrics` with the measured values, so the
  control plane surfaces them as `RobotProbe.status.robotResults[].actualMetrics`.
  Declining remains conformant (C7.1).

## C10 — Update progress (when `push_firmware` / `model_update` is implemented)

- **C10.1 (MAY)** While applying a `PushFirmware` / `ModelUpdate` it accepted,
  emit advisory `UpdateProgress` messages (`robot_id`, `kind`, `phase`, optional
  `percent`) before the final result, so the control plane surfaces per-robot
  `FirmwareRollout` / `ModelRollout` `status.currentBatch[].updatePhase`. Progress
  is advisory and emitting none is conformant.

- **C10.2 (MUST, when implemented)** Report the TERMINAL result of every install
  the adapter answers, as an `UpdateProgress` carrying `outcome`
  (`INSTALL_OUTCOME_SUCCEEDED` / `INSTALL_OUTCOME_FAILED`), `failure_reason` on
  failure, and `resulting_version` — the version the robot is *actually* left
  running. On failure that is not the target and cannot be inferred: a failed
  install may leave the robot on its previous version, on a recovery image, or
  elsewhere. SHOULD additionally carry the same state in `CapabilitiesSnapshot`
  (`InstalledModel.failure_reason`, `FirmwareState`) so a control plane that
  restarted mid-install recovers the outcome on the next scan (ADR-0033).

  The `CommandResult` is not this signal: `FirmwareResult.accepted` means *a
  download began*. Without a terminal report the control plane cannot distinguish
  a slow install from a finished one, and the rollout never completes.

  **A refusal is terminal too.** An adapter that refuses an artifact under C7.2
  MUST still report `INSTALL_OUTCOME_FAILED` — declining in the `CommandResult`
  alone leaves the rollout waiting for an install that will never happen.

  Conditional on the capability, not universal: declining `push_firmware` /
  `model_update` outright (C7.1) carries no obligation here and remains fully
  conformant.

## C11 — Pause / resume (when implemented)

- **C11.1 (MUST)** Accept `pause` (`PauseCapabilities`) and `resume`
  (`ResumeCapabilities`), answering with a `PauseResult` / `ResumeResult`. When
  `require_stop_before_ack` is set, `paused=true` MUST be reported only once the
  robot is CONFIRMED at rest (never inferred). Declining is NOT conformant: these
  are in RFC-0001's Required-message table and absent from its optional-command
  list, and the ZoneMaintenance resource drains a zone through them — an adapter that
  declines makes a maintenance window silently do nothing.

## C12 — Capabilities scan (when implemented)

- **C12.1 (MUST)** Answer a `ScanCapabilities` request with a full
  `CapabilitiesSnapshot` (all hardware / installed models). Declining is NOT
  conformant: `scan` is how the control plane learns the adapter's
  `supported_actions` catalog, which is the pre-dispatch gate whenever the
  optional `validate_action` is declined. An adapter declining both is
  undispatchable in practice.

## C13 — Contract-version negotiation (ADR-0032)

- **C13.1 (MUST)** Report on `AdapterHello` the `contract_version` this adapter
  implements, inside the range the control plane supports (minor N and N-1 within
  the current major). A control plane treats a missing, unparseable, or
  out-of-range value as incompatible and refuses the adapter's registrations
  `VERSION_MISMATCH` — never as an implicit pass. An in-range adapter receives
  `HelloAck.negotiated_contract_version`; an empty value on that field means no
  compatible contract was agreed, never "compatible but unrecorded".
- **C13.2 (SHOULD)** Having had a registration refused `VERSION_MISMATCH`, do not
  then accept an `AssignAction`. This is defence in depth, not a MUST: a control
  plane withholds the dispatch itself (ADR-0032's assignment gate is on work
  dispatch only), so an adapter that accepts a command it should never have been
  sent is not non-conforming. The harness exercises it by faulting the
  ControlStream after the other checks and refusing the redial's registration; an
  adapter that does not reconnect records a `skip`, never a pass.

## C14 — Emergency stop is version-invariant (ADR-0032)

- **C14.1 (MUST)** Honour `Estop` and return a confirmed `EstopAck` even when the
  adapter has failed the contract-version gate. Refusing an incompatible adapter
  *work* is only acceptable because refusing to *stop* it never happens.
- **C14.2 (MUST)** The `Estop` / `EstopAck` schema is unchanged across the
  supported contract versions — field numbers and names are pinned, so an adapter
  built against an older minor can always parse an estop from a newer control
  plane. Verified against the proto descriptors, not by a live exchange, so it
  cannot pass by accident.

## C15 — Version-bound conformance (ADR-0032)

- **C15.1 (MUST)** The report carries the `contract_version` its result was earned
  against. The control plane copies this to
  `FleetAdapter.status.conformanceContractVersion` and refuses assignment when it
  is absent or out of range, so an unstamped report does not only look
  incomplete — it makes the adapter unassignable.
- **C15.2 (MUST)** A non-conformant report stays version-bound and fully
  observable: `conformant: false` while still carrying the contract version and
  the per-check detail, so work can be withheld without costing an operator the
  ability to see why. Assignability itself is control-plane behaviour and is
  asserted there, not here — this suite has no scheduler.

## Passing

An adapter is conforming when it satisfies every **MUST** and **MUST NOT** above
for the operations it implements, and correctly declines the optional operations
it does not (C7.1).

### What the report attests, and against which version

A result is earned against a specific **contract version** — a semver over the proto surface and the
`SupportedAction` schema — not against the standard as a whole. It is not a version of this suite: the
checks below are the *evidence* for a result, never a component of the version it is earned against, which
is why adding a check re-attests adapters without moving the version any adapter negotiates. The harness
stamps the contract version into the report as `contract_version` (alongside `protocol_version`, the
wire-package identity `fleet_adapter.v1`, which is not a semver and cannot express compatibility). Record
that semver in your [`REGISTRY.md`](REGISTRY.md) row, and record the suite revision you ran alongside it —
never folded into it — both in that row and in `FleetAdapter.spec.conformanceReport.suiteVersion`.

A **major** bump is breaking: re-run `make conformance` and update the row. Minor and patch bumps are
compatible and never invalidate an existing qualification.

Conformance is **self-certified** ([ADR-0007](../docs/adr/0007-conformance-self-certification.md)):
you run the harness, the registry pull request attests the result, and a control plane only
*consumes* it — after checking the report's digest, and its signature where the namespace requires
one. There is no certification authority, and this project does not operate one. A submission is expected to include a test suite
demonstrating C3–C6 in particular, since those encode the safety and
anti-double-execution guarantees the standard exists to provide.

## C16 — Task status reporting

- **C16.1 (MUST)** Send an `ActionStatusUpdate` at every task phase transition,
  terminal states included. The control plane drives `FleetAction`
  `Assigned -> InProgress` from it, and a task that ends without one is
  resolvable only by lease expiry.
