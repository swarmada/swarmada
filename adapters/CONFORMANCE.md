# Fleet Adapter Conformance

This document defines what a Fleet Adapter **must**, **should**, and **may** do
to be a conforming Swarmada adapter. It is derived from the `fleet_adapter.v1`
contract and RFC-0001 #fleet-adapter-protocol; where this document and RFC-0001 differ, RFC-0001
governs. The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are used
as in RFC 2119.

Conformance is what makes an adapter part of the standard — not whether it lives
in this repository. Every adapter, in-tree or vendor-owned, is expected to pass
the checks below.

> **Status:** This document is the conformance *specification*. An executable
> harness that drives an adapter under test against these checks lives in
> [`conformance/`](conformance/) and drives **C0 through C19**: C0 (report
> self-checks), C1 (handshake), C2 (registration and reconnect reconciliation),
> C3 (fencing-token ordering and durability), C4 (assignment leases, including the
> self-stop), C5 (confirmed estop), C6 (telemetry discipline and explicit
> presence), **C7** (optional-command decline via `unsupported=true`), **C8** (edge
> stream — the harness serves `EdgeService`, advertises an edge endpoint on the
> `RegisterAck`, and checks the adapter dials it and honours an edge-issued estop
> with a confirmed `EstopAck`), C9–C10 (probes and update progress — owed only by an
> adapter that accepts the command), C11 (pause/resume — required), C12 (capability
> scan — required), C13–C15 (contract-version negotiation, estop
> version-invariance, version-bound conformance), C16 (task status reporting) and **C17**
> (zone operating constraints: the honoured-subject declaration is well formed, and an
> assignment carrying a rule the adapter cannot honour is DECLINED naming the subject
> rather than accepted and ignored), C18 (sensor streams and the duty cycle) and **C19**
> (an assignment the adapter's own catalog does not cover is refused). Every assignment the
> harness sends carries an action type the adapter declared on its `scan` reply, with the
> parameters that type declares required, so a refusal can only be about the thing a row
> probes.
> A check the adapter does not exercise is recorded `skip` with an enumerated
> cause (the SkipCause enum in `conformance/report.py`) rather than free text; a `skip`
> records what was and was not verified, and is not a failure. It is not a pass either:
> in the profiled report every row is graded, an unmeasured row names what would make
> it testable, and a skipped Core row prevents a `pass` verdict
> ([`../conformance/PROFILES.md`](../conformance/PROFILES.md), *The capability sheet*).
> Run it with `make conformance` (see [`conformance/README.md`](conformance/README.md)).

> **Profiles and levels.** The catalog below is flat; the *partition* of it into
> named profiles at Core and Extended levels, the capability tiers that select
> which profiles an adapter must hold, and the machine-readable
> conformance report (a YAML artifact, not a wire message) are specified in RFC-0001's
> *Conformance Profiles and Support Levels* chapter and registered machine-readably in
> [`../conformance/profiles.yaml`](../conformance/profiles.yaml). Run
> `make conformance-profiles` to produce a profiled report. Draft 4 adds three
> profiles to this surface: `reactive-grammar` (Core, always applicable) and
> `reactive` (Extended) from [`../conformance/REACTIVE-BLOCK.md`](../conformance/REACTIVE-BLOCK.md),
> and `zone-restrictions` (Extended, always applicable) from `C17` below.

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

- **C0.2 (context)** The `negotiated_contract_version` the harness set on its own `HelloAck`: `1.4.0`
  for an adapter that reported a `1.4` version, and the `1.3.x` an adapter reported otherwise, as a
  control plane agrees it for the connection. Harness state, not an adapter behaviour.
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

- **C2.1 (MUST)** Send `RegisterRobot` FIRST for every managed robot, and send
  `DiscoverRobot` only after a `NOT_ADMITTED` rejection routes the adapter into
  the provisioning loop. `DiscoverRobot` MUST NOT be attempted before `register`.
  An adapter cannot know whether a `Robot` custom resource exists — that is
  control-plane state — so the ordering is not a guess it makes but a sequence
  the rejections drive: `register` first, `discover` on `NOT_ADMITTED`, and a
  `discover` for an already-admitted robot answered in band with
  `DiscoverAck{accepted: false, rejection: ALREADY_EXISTS}`.

  The harness plays first contact with a robot that has no `Robot` yet (RFC-0001,
  *Re-Connection Flow: RegisterRobot*). The first `register` is refused
  `NOT_ADMITTED`, and the check passes when a `discover` announcing the robot
  follows it. The register of the provisioning loop's next iteration is accepted
  as the robot's admission, and the run goes on with the robot admitted. The
  harness's `DiscoverAck` carries `ttl_seconds`, but the loop schedules that next
  iteration when the register is refused, before the TTL arrives, so the harness
  waits up to 75 s for it: 60 s is the loop's interval with no TTL. The check
  fails when no `discover` follows the refusal, when another `register` answers
  it, or when a `discover` is the first lifecycle message. Before suite
  `2026.11.09` the harness played a control plane that already held the `Robot`,
  so this check passed an adapter that never announces a robot and so cannot
  join a control plane.
- **C2.2 (MUST)** On `RegisterAck`, adopt `authoritative_action_state` as the
  source of truth and reconcile local state to it (detecting divergence after a
  connectivity gap).

  The harness's `RegisterAck` declares the action the adapter was running
  `CANCELLED`, and the check grades the adapter's terminal report for that action:
  any state RFC-0001 calls terminal (safety chapter, *terminal*) passes. An
  `ACTION_STATE_STOPPED` report says the stop was commanded and is not an end, so
  it does not satisfy this check, as it does not satisfy `C4.2`.
- **C2.3 (MUST)** Honor the per-robot configuration returned in `RegisterAck`
  (`telemetry_interval_seconds`, `assigned_zone`, `active_capabilities`,
  `edge_endpoints`).

  Measured as the spacing of the payloads that follow, against an advertised 1 s.
  The harness DRIVES THE ROBOT for that window, because the motion-gated position
  telemetry SHOULD lets a conformant adapter space payloads out to
  `maxIdleIntervalSeconds` (recommended class default 30 s) while the robot is
  stationary — neither `motionThresholdMeters` nor `maxIdleIntervalSeconds` is on
  the wire, so the harness cannot turn gating off, only remove the condition that
  triggers it. Where the robot could not be driven, a wide gap is recorded `skip`
  rather than FAIL: a satisfied SHOULD and a violated MUST are indistinguishable
  on that trace, and reporting one as the other was this check's defect.

- **C2.4 (MUST NOT)** Re-discover on reconnect. After a lost connection the
  adapter MUST send `RegisterRobot` first on the redial and MUST NOT send
  `DiscoverRobot` before that register is rejected `NOT_ADMITTED` (RFC-0001
  safety, local behaviour during connectivity loss, item 3; and the
  Required-message table, which says `RegisterRobot` is the first attempt "on
  connect and reconnect"). In this check the robot is admitted when the
  reconnect happens, so the register is accepted and a `discover` asks the
  control plane to create a `DiscoveredRobot` for a robot that already has a
  `Robot`.

  Distinct from C2.1, which grades the FIRST connection: the two are separate
  rows because on this check's reconnect no rejection licenses a `discover` —
  the robot is admitted, so re-discovery is never correct here — while C2.1's
  ordering is driven by the rejections. The harness answers a redial `discover` `ALREADY_EXISTS` and keeps
  waiting for the `register` that must follow, so an adapter that corrects itself
  still reaches C6.1 and C2.2 and the row records that it had to.

## C3 — Fencing tokens (anti double-execution)

- **C3.1 (MUST)** Persist the highest accepted fencing token per robot so it
  survives an adapter restart.
  **This suite verifies only the across-reconnect half** — that a faulted and redialled
  stream does not reset the mark. Durability across a process **restart** is C3.7, and it
  is recorded SKIP: no reference adapter persists the mark anywhere, so the requirement
  has no implementation in the project to test.
- **C3.2 (MUST)** Reject any `AssignAction` or `CancelAction` whose `fencing_token`
  is less than or equal to the highest accepted token for that robot, returning
  the matching `STALE_FENCING_TOKEN` / stale rejection — except an identical
  re-delivery of the current assignment, and a `CancelAction` naming the action the
  adapter is executing at the token that assigned it (C3.6). While a stop latches the robot,
  an `AssignAction` is answered as fleet adapter protocol, *The Stop Latch*, requires, not by
  this row.
- **C3.3 (MUST)** Reject `AssignAction` with `MISSING_FENCING_TOKEN` when the
  token is absent (distinguish "absent" from a value of `0`).
- **C3.4 (MUST)** Idempotently re-ack an identical re-delivery of the current
  assignment rather than treating it as stale. While a stop latches the robot, *The Stop Latch*
  governs the answer, and it keeps this re-ack only for a re-delivery of an action the adapter
  holds as interrupted work, begun or not.
  **Scope.** This row grades only that an identical re-delivery on one connection is accepted, not
  that the adapter does not execute it again, nor that deduplication by report token survives the
  adapter's own restart (*Durable deduplication*, under *Adapter obligations with no C-row*).
- **C3.5 (informational)** Echo the fenced token in `AssignActionResult.accepted_fencing_token`.
  RFC-0001 does not state this echo as an obligation. At v0.4 the harness still grades it as
  mandatory, and the registry lists it among `dispatch`'s Core rows. An adapter that does not echo
  the token is not conformant, and its robots are refused admission (RFC-0001 conformance
  profiles, *Not met at v0.4 (C3.5)*).
- **C3.6 (MUST)** Report a valid `CancelDisposition` on `CancelActionResult`, so the
  control plane can tell a safe stop from a completion, a recovery, or a stop whose rest
  could not be witnessed. Every value the contract defines is valid,
  `CANCEL_DISPOSITION_STOP_UNCONFIRMED` (contract `1.4.0`) included.
- **C3.7 (MUST)** The persisted high-water mark survives an adapter **process restart**,
  not only a stream reconnect. A reconnect does not touch process memory, so an adapter
  holding the mark in a plain dict passes C3.1 and fails this. The hazard is a control
  plane re-issuing tokens to an adapter that restarted and forgot: two executors accepting
  the same assignment, which is what C3 exists to prevent.
- **C3.8 (MUST at the attested contract version; advisory at N-1)** **Adopt**
  `RegisterAck.authoritative_action_state.fencing_token` as the high-water mark. The proto
  states the obligation on the field itself — it is the token the adapter should be
  executing under, and the adapter applies the fencing rule (C3.2) against it: it rejects
  an `AssignAction` or `CancelAction` at or below it, except an identical re-delivery and a
  `CancelAction` naming the executing action at the token that assigned it —
  and C3.1–C3.7 never tested it, because every one of them raises the mark from an
  `AssignAction` the adapter itself accepted. Nothing read the control plane's own
  statement of the token.
  **What it observes.** The accepting reconnect declares an authoritative token strictly
  ABOVE the mark the adapter reached on its own (22 against 19); an `AssignAction` then
  arrives at a token strictly BETWEEN the two (20). A conformant adapter has adopted 22 and
  refuses 20 `STALE_FENCING_TOKEN`. One that ignored the field still holds 19 and accepts
  it. The probe must be strictly between: at 19 the surviving mark alone refuses it (that
  is C3.1), and at 22 an adapter that adopted would re-ack rather than refuse (C3.4).
  **The hazard.** After a control-plane **failover** the new plane legitimately issues from
  a higher generation than this adapter ever saw. An adapter that never adopts keeps
  honouring commands the dead plane authorised — two executors on one robot, arriving
  through the one door C3.2 does not watch, since C3.2 only refuses tokens below a mark the
  adapter raised itself.
  **Why this went uncaught until 2026-09-04.** The harness's accepting reconnect used to
  send a token EQUAL to the one the adapter already held, so adopting and ignoring produced
  byte-identical traces and C2.2 and C3.1 both passed against an adapter that never read
  the field. A fixture cannot detect a difference it does not create.
  **Version binding.** Graded MUST when the adapter reports the contract version this suite
  attests, and SHOULD when it reports N-1. ADR-0032 sets the supported range at N and N-1
  minor and states that minor releases never invalidate an adapter's qualification; grading
  this MUST against an N-1 adapter would take a CONFORMANT adapter to NON-CONFORMANT on a
  minor bump alone, and the control plane's robot-admission gate would then refuse it assignment. The check
  still runs and still reports FAIL at N-1 — the author sees the defect, the fleet keeps
  moving. It becomes an unconditional MUST at the next major, which requires
  re-qualification regardless.
  **Not on the estop path.** Adoption is for the assignment path only. The estop path
  carries no fencing token on purpose, so a stale token can never refuse a stop
  (RFC-0001 `#safety-emergency-stop-protocol`).
- **C3.9 (MUST)** A `CancelAction` naming an action the robot is no longer executing MUST NOT
  stop the action it is executing. The case is in-place preemption: the control plane pushes
  the preempting action as an `AssignAction` of the same type at a strictly higher token, the
  robot switches to it (RFC-0001 `#fleet-adapter-protocol-directive-supersession-on-the-wire`),
  and the preempted action's reconcile then sends `cancel_action` naming the **preempted**
  action, to confirm it stopped. The proto states why that cancel must not reach the robot's
  current work: a stale cancel "cannot abort a newer assignment" (`CancelAction.fencing_token`),
  and RFC-0001 `#fleet-adapter-protocol-fleet-adapter-compliance-checklist` requires
  `cancel_action` to honour the fencing rule.
  **What it observes.** The harness assigns `t-preempted` at 15, then `t-preemptor` in place
  at 16, confirms with `renew_lease` that the preemptor is running, and sends two cancels
  naming `t-preempted`: one at its own token (15, below the mark) and one at the mark (16).
  The first is the one the reference control plane sends: it draws every token from one
  counter per namespace, so the preemptor's token is above the victim's, and the victim's
  cancel is below the mark. The second, at the mark, is what a control plane that counted
  tokens per action could send; `action_id` is then the only field that says the cancel is
  not about the running action. An adapter that checks the token and never reads `action_id` would
  pass on the first cancel alone. The row passes when `renew_lease` still reports the
  preemptor `running=true` after both cancels and the preemptor then reports
  `ACTION_STATE_SUCCEEDED`. It fails when the preemptor ends any other way, or is not seen to
  complete within the harness's wait.
  **The `CancelActionResult` for the preempted action is recorded, not graded here.** For a
  `CancelAction` naming an action the adapter is not executing, RFC-0001's fencing rule
  (`#fleet-adapter-protocol-the-controlstream-service`) accepts either of two answers: a stale
  rejection (`stale = true`), or a terminal state for the named action; it still requires the
  stale rejection at or below the mark, so for both of these cancels C3.2 asks for
  `stale = true`. With either answer the adapter MUST NOT stop the action it is executing,
  and that is what this row grades. The
  reference control plane releases a preempted action as row A12 of RFC-0001
  `#safety-action-table` states, with that row's note on the `stale`
  flag (`confirmPreemptedStop`). The
  simulation adapter acknowledges such a cancel `STOPPED_SAFELY`: it compares no token on
  `cancel_action`, the gap RFC-0001 discloses with the fencing rule, and the C3.2 check sends
  only a stale `AssignAction`. The detail records what the adapter said to each cancel.
  **The hazard.** An adapter that stops whatever is running on any cancel aborts every
  in-place preemptor. On a robot that is a task stopping mid-route for no reason an operator
  can see, and the preemptor then never completes.

- **C3.10 (MUST at the attested contract version; NOT YET RUN)** Reject a `Command` other than a
  stop, and an `EstopClear`, whose `leader_term` is lower than the highest term seen for the robot
  (RFC-0001 `#fleet-adapter-protocol-the-stop-latch`, *The leader term*). The adapter does not act
  on the rejected `Command`, and answers it with `CommandResult.stale_leader_term = true`. A
  rejected `EstopClear` lifts nothing and is not answered with an `EstopClearAck`.
  **What it observes.** On a connection that negotiated `1.4.0`, the harness sends an
  `AssignAction` at `leader_term` 2 (the control: it is accepted), then a second `AssignAction`
  and a `CancelAction` at `leader_term` 1. Both are answered `stale_leader_term = true`, and the
  running action is neither replaced nor stopped. After a stop is latched, the harness sends an
  `EstopClear` for its ID at `leader_term` 1: the latch stays, and the next `AssignAction` at
  term 2 is still refused as the latch requires.
  **The hazard.** A leader that was deposed and then resumes, or still holds the adapter's streams
  the new leader cannot reach, sends from stale state. A stale assignment can start work on a robot
  the new leader has stopped, and a stale release can lift a latch the new leader relies on.
  **Version binding.** The row grades only a connection that negotiated `1.4.0`; a `1.3.x`
  connection carries no term. A control plane that sends `leader_term` 0 is not rejected, since 0
  means no term was sent.
- **C3.11 (MUST at the attested contract version; NOT YET RUN)** Accept a stop whatever its
  `leader_term`: a fence (`Command.estop`) is latched and the robot stopped even when its term is
  lower than the highest the adapter has seen, and `stale_leader_term` is never set for a stop. A
  stop with a lower term does not lower the highest term seen.
  **What it observes.** After the adapter has seen term 2, the harness sends a fence at term 1.
  The adapter latches its ID, stops the robot and answers with an `EstopAck` on the `SafetyStream`.
  A following `AssignAction` at term 1 is still answered `stale_leader_term = true`.
  **The hazard.** An adapter that applies the term rule to a stop sheds a stop a deposed leader
  issued. A stale stop can only hold the robot, so refusing it is the unsafe side.
  **Not run yet.** C3.10 and C3.11 are defined here for draft 4. The harness
  (`adapters/conformance/harness.py`) does not run them, and neither row is registered in a profile
  (`conformance/profiles.yaml`) until a suite revision adds its probe (RFC-0001
  *Conformance Profiles and Support Levels*). Until then they do not count toward
  conformance.

## C4 — Assignment leases (self-stop)

- **C4.1 (MUST)** Treat a held execution lease as valid only while the control
  plane renews it via `renew_lease` within `lease_duration_ms`.
- **C4.2 (MUST)** Bring the current task to a **confirmed** stop if no renewal
  arrives before the self-stop timer fires, and report its end. An
  `ACTION_STATE_STOPPED` report says the stop was commanded; it is not terminal,
  and no row of the action table reads it as an end (RFC-0001 safety chapter,
  *terminal*; the `ACTION_STATE_STOPPED` comment in the proto). The adapter then
  sends the action's terminal report (RFC-0001 safety chapter, *terminal*), and
  sends it only once the robot is observed at rest, or once rest cannot be
  confirmed within the adapter's stopping budget; it sets `end_cause` as fleet
  adapter protocol, *What the adapter MUST report*, requires. That terminal
  report is what this check grades. Commanding the stop is not the same as
  establishing it. The lease exists to make dual execution impossible, and a
  stop that was announced but never observed does not establish that the first
  executor halted — which is the whole property C3 and C4 provide. This is the
  same discipline C5.3 applies on the estop path; the argument does not weaken
  because the trigger is a lapsed lease rather than an operator, and the lease
  case is worse, because an estop has someone watching and a lapsed lease has
  nobody.

  **What this suite establishes, and what it does not.** The harness observes the
  wire: it assigns under a short lease, stops renewing, and requires a terminal
  `ActionStatusUpdate` within its window; an `ACTION_STATE_STOPPED` report does not
  satisfy it. That shows the end was REPORTED. It cannot show the robot was at rest
  when the terminal report was sent — no field on `ActionStatusUpdate` attests
  confirmation the way `stop_initiated_at` does on `EstopAck` (C5.3) — so a PASS
  here is evidence that the adapter reported a terminal state, not that it
  confirmed rest before doing so. An adapter that sends its terminal report at T+0
  while the base decelerates for a further 2.5 s passes this check and violates
  the sentence above. The `end_cause` is not graded here (*Cause reporting*, under
  *Adapter obligations with no C-row*).
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
  `ESTOP_STATE_STOPPING` and send the terminal `STOPPED`/`FAILED` when rest is
  confirmed. C5.5 bounds the first ack; this entry governs the terminal one. A
  single `STOPPED` ack remains conformant **only when rest is confirmed within C5.5's
  500 ms**: when it cannot be confirmed by then, the first ack MUST be
  `ESTOP_STATE_STOPPING` (or `ESTOP_STATE_FAILED` when the stop has already failed),
  and the terminal ack follows.
  **`STOPPING` is a receipt, never a stop.** It means *received and braking; rest
  not yet confirmed*: the estop arrived and the adapter has already commanded the
  stop to the drive. It says nothing about whether the robot is still moving. An
  adapter MUST NOT send `STOPPING` before it has commanded the stop, and nothing
  that reads an `EstopAck` — a control plane, a display, a log, a report — may
  treat or present `STOPPING` as a stop. Only `STOPPED` says the robot is at rest.
- **C5.3 (MUST NOT)** Report `STOPPED` by inferring it from absent motion, a
  timeout, or telemetry; the state MUST be adapter-confirmed. An `EstopAck`
  reporting `ESTOP_STATE_STOPPED` MUST populate `stop_initiated_at` — whether an
  adapter *inferred* a stop is not decidable from the wire, and that timestamp is
  the only artefact attesting the stop was commanded. Necessary evidence, not
  proof. *Adapter-confirmed* has a platform precondition and a veto, both stated
  under *What may confirm rest* below.
- **C5.5 (MUST)** Answer an `estop` on `SafetyStream` with an `EstopAck` within
  500 ms. Measured by the harness from queueing the `Estop` to dequeuing the ack,
  so the harness's own transport is included and the figure is conservative.
  This bounds the **first** ack, which MAY be `ESTOP_STATE_STOPPING`: the receipt
  of C5.2, *received and braking; rest not yet confirmed*. The receipt exists so
  that within the budget a control plane can tell "received, braking" from "lost,
  still driving", and escalate on the second; it is not evidence of rest. It is an
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
- **C5.7 (MUST NOT)** Report `ESTOP_STATE_STOPPED` before the robot is at rest, as a
  motion witness records it. Graded when the run has a witness — something other than the
  adapter's own confirmation path that records the robot's actual speed — and
  `not-testable-here` without one. The measurement is defined under *An early `STOPPED`
  fails* below.
- **C5.4 (MUST NOT)** Carry a stop's acknowledgement on `ControlStream` in place of
  `SafetyStream`. The `EstopAck` that the control plane reads for a stop (rows S2 to S4 of
  RFC-0001's robot stop table) travels on `SafetyStream`. `Command.estop` on
  `ControlStream` is the stop's fence: the adapter latches on it (fleet adapter protocol,
  *The Stop Latch*), and its `CommandResult.estop` is a receipt that rows S2 to S4 never read.
  **What it observes.** The harness sends a fence alone, for a stop ID that no `Estop` carried.
  The row fails when the fence is answered with a `CommandResult.estop` and no `EstopAck` for
  that stop ID arrives on `SafetyStream`. A receipt beside the `SafetyStream` ack passes, and so
  does no answer on `ControlStream`. On a `1.4.0` connection the latch also requires the
  `SafetyStream` ack, and no row grades that (*Adapter obligations with no C-row*).
- **C5.8 (MUST at contract `1.4.0`)** Hold the robot's work under a stop until the stop's
  `EstopClear` lifts it, and say when it is lifted (fleet adapter protocol, *The Stop Latch*,
  *While latched* and *The clear*). After a stop, every `AssignAction` for the robot is refused
  `ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE`, whatever its fencing token. An `EstopClear` for
  the stop, once its fence has arrived, lifts it and is answered with an `EstopClearAck`
  naming it. The robot then takes new work.
  **What it observes.** On a `1.4.0` connection, after the accepting reconnect, the harness
  cancels its cadence drive and stops the robot as row S1 does: the `Estop` on `SafetyStream`
  and its fence on `ControlStream`. Once the `EstopAck` naming the stop arrives, it sends an
  `AssignAction` at token 24. It then clears the stop with an `EstopClear` at leader term 1,
  waits for the `EstopClearAck`, and sends a second `AssignAction` at token 25. The row passes
  when the first is refused `ROBOT_UNAVAILABLE`, the clear is acknowledged and the second is
  accepted. It fails an adapter that accepts the first (it keeps no latch) or refuses it for
  another reason, one that sends no `EstopClearAck`, and one that refuses the second after
  acknowledging the clear.
  **Version binding.** A `1.3.x` adapter records `contract-predates-obligation`, which is never
  an obligation: the latch binds a connection at `1.4.0`, and a control plane sends a `1.3.x`
  connection no `EstopClear`.
  **Not graded here.** A fence alone as a stop, the leader term (`C3.10`, `C3.11`), the reports,
  a clear that arrives before its fence, the local latch and interrupted work. *Adapter
  obligations with no C-row* lists them.

### What may confirm rest (C5.2, C5.3)

C5.3 forbids inferring `STOPPED`. On some platforms the only motion reading that
reaches software is itself an inference: odometry whose velocity is the commanded
velocity reports what the drive was told to do, not what the wheels did, and a
"confirmed rest" read from it is an echo of the stop command. The rules below say
which readings may confirm rest, which may forbid it, and how a report records a
platform that has nothing able to confirm it.

**A motion channel independent of the command path.** A channel is *independent of
the command path* when both of these hold:

1. its reading is produced by a sensor that observes the robot's actual motion, or
   the world relative to the robot; and
2. no velocity command, setpoint, or quantity computed from one enters the
   computation of that reading — neither directly nor through a filter or estimator
   that fuses it with other inputs.

The test is counterfactual: if the drive did not do what it was commanded — a wheel
slipped, a motor stalled or free-wheeled, a person pushed the robot — would the
channel show the difference? A channel that would go on reporting the commanded
motion is not independent, whatever its topic name, message type or label.

- *Qualify, when they meet both conditions:* wheel or motor encoders read from the
  drivetrain; the motion or standstill output of a safety-rated scanner; an external
  witness (a motion-capture system, a fixed camera, a reference outside the
  platform).
- *Does not qualify:* odometry whose velocity reproduces the commanded velocity —
  the command clamped, filtered or integrated — and any estimate fused from such
  odometry. A reading that equals the command while the robot is held still is the
  command.

To **confirm** rest a channel must also be able to observe motion at constant
velocity. An IMU observes acceleration and rotation rate; a robot translating at
constant velocity produces neither, so an IMU can refute rest and cannot confirm it.

**Precondition.** An adapter MAY report `ESTOP_STATE_STOPPED` only when at least one
motion channel that is independent of the command path, and able to confirm rest,
shows the robot at rest. The `safety` profile is therefore claimable only on a
platform that has such a channel. On a platform without one, no adapter satisfies
C5.2 and C5.3 together: the honest terminal answer to every estop is
`ESTOP_STATE_FAILED`, and a `STOPPED` is the inference C5.3 forbids.

**Refute wins.** If **any** channel available to the adapter shows motion —
including a channel that cannot confirm rest, such as an IMU, a scan of a static
scene that changes, or a bumper contact — the adapter MUST NOT report
`ESTOP_STATE_STOPPED` while it does. A channel does not need to be able to confirm
rest in order to veto it. Disregarding an available channel, or setting its
threshold above the motion it exists to detect, is the inference C5.3 forbids. The
cost of this rule falls in the safe direction: a spurious refutation delays
`STOPPED` and may end in `ESTOP_STATE_FAILED` (C5.2); it never produces a `STOPPED`
for a robot that is moving.

**How a report records an unmet precondition.** A report for an adapter on a
platform with no independent motion channel grades the `C5.3` row
`not-supported-by-platform`
([ADR-0070](../docs/adr/0070-a-conformance-report-grades-each-row-and-names-its-unlock.md)),
with an `unlock` naming such a channel — for example *wheel or motor encoders read
from the drivetrain, a safety-rated scanner's motion output, or an external motion
witness*. The row records an obligation left unestablished, not one that never
applied: it keeps the `safety` profile short of `pass` and the report's verdict at
`not-established` or worse. This is a grading rule for a hardware run. A simulated
run has no platform channels to judge; its `C5` rows are graded
`measured-simulation`, which says nothing about the robot the adapter will drive.

> **Enforced from a plant declaration.** When a run is profiled with a
> [`plant.yaml`](../conformance/PLANT.md) (`python -m conformance fleet-adapter --plant`), a
> declaration whose odometry topics are all `command-derived`, with none `measured`, grades
> five rows — `C3.6`, `C5.2`, `C5.3`, `C8.3` and `C14.1`, none of which such a platform can
> satisfy without claiming a rest it cannot confirm — `not-supported-by-platform` with the cause `no-independent-motion-channel`,
> whatever the run observed on the wire (`PLATFORM_PRECONDITION_CHECKS` in
> [`conformance/adapter.py`](../conformance/adapter.py)). The cause is outside the never-an-obligation set,
> so the verdict cannot reach `pass`. A run with no plant declaration, or one whose motion
> channels are unknown, cannot apply the precondition, and its `C5.3` result is then only as
> good as the adapter author's account of the channel it confirms rest from.

### An early `STOPPED` fails (`C5.7`)

C5.3 concedes that inference is not decidable from the wire. It is decidable against
a **motion witness**: something other than the adapter's own confirmation path that
records the robot's actual speed over time. On a hardware run the witness is external
to the platform — a motion-capture system, a fixed camera, a reference outside the
robot. In a simulated run it is the simulator's plant model: the physics state the
simulation advances, recorded as it is advanced, not the adapter's report of it.

- The **witnessed moment of rest** is the start of the first interval, at least the
  witness's stated hold time long, during which the witness shows no motion beyond
  its own stated rest noise. The hold time, the noise threshold and the witness's
  timing resolution belong to the witness set-up, and are printed beside the result.
- The **time of `STOPPED`** is the ack's `confirmed_at_ms`, on the witness's clock.
  Where the adapter set none, the harness's receipt time of the `EstopAck` is used, and
  the result says the comparison is then lenient by the transport delay.
- A `STOPPED` whose time precedes the witnessed moment of rest by more than the
  tolerance **fails**. The tolerance is the larger of the witness's timing resolution
  and the gap between the last sample that shows motion and the first that shows rest.
  The robot came to rest somewhere in that gap, so a sparse witness can place rest up
  to one gap late; with this tolerance a `STOPPED` fails only when it precedes a sample
  that still showed the robot moving. The gap and the tolerance are printed beside the
  result. On hardware the report also flags the robot —
  the plant named in its `subject` — as well as the adapter: an early `STOPPED` means
  either that the adapter inferred rest or that the channel it confirmed rest from does
  not measure the robot's motion, and the second is a fact about the platform that no
  change of adapter cures.
- The robot must be moving when the estop is sent. A run whose witness shows it already
  at rest records the row as not reached: a stop issued to a robot at rest cannot tell an
  early `STOPPED` from a timely one.

Without a witness the row is graded `not-testable-here`, with the `unlock` *an external
motion witness on a hardware run, timestamped on a clock aligned with the harness; in a
simulated run, the simulator's plant model supplied as the witness*. A simulated result
says nothing about the robot the adapter will drive: it tests that the adapter waits for
the rest its plant reports, not that the platform can report rest.

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
  status writes. This row is graded on the ControlPlane surface as `CP8.1` and belongs
  to no Fleet Adapter profile: nothing an adapter puts on the wire can show whether the
  plane writes status per frame. The adapter's half — not depending on per-tick status
  writes — is not observable by this suite.
- **C6.4 (MUST)** Echo, in every `ActionStatusUpdate`, the `fencing_token` the reported
  action was assigned with, so a robot acting on a superseded assignment can be detected:
  the control plane refuses a report whose token is absent or differs from the token the
  action was assigned with.

## C7 — Optional operations

- **C7.1 (MAY)** Decline an optional command (`VerifyHardware`, `PushFirmware`,
  `ModelUpdate`, reservations, zone admission, and similar) by returning
  `CommandResult.unsupported = true` with an empty result.
- **C7.2 (MUST, when implemented)** For `ModelUpdate`, verify the artifact checksum
  and, when signature verification is required, verify the signature against the
  publisher keys provisioned to the adapter (the protocol carries no trust root),
  and fail closed on failure.

  **Two legs, checked to different depths.** The harness probes with an artifact
  whose checksum cannot match and whose `model_signature_ref` resolves nowhere.
  Failing closed passes, whichever leg refused — the requirement is that the
  artifact is not installed. Accepting fails, and the row states which of the two
  accepting shapes it saw: an accept naming no `verified_signer` (specified
  populated on an accepted artifact) says on the wire that nothing verified the
  signature reference the probe carried.

  **Not established here:** trust-root verification under
  `SwarmadaConfig.spec.signing.requireSignatureVerification`. That flag is a
  control-plane resource, not a wire field, so this suite cannot assert the
  condition the "when signature verification is required" clause depends on, and
  a PASS is not evidence that an adapter would verify against real trust roots.
  It is a deployment property, in the same class as C1.3's mTLS identity. Before
  the signature fields were sent at all, this row published a signature obligation
  against a probe that asked nothing about signatures.
- **C7.3 (MUST, when implemented)** For `PushFirmware`, re-verify the delivered
  **bytes** against `firmware_checksum` on the robot before flashing, verify
  `firmware_signature_ref` against the publisher keys provisioned to the adapter, on
  the same terms as C7.2, and
  fail closed on either. This is a distinct obligation from C7.2, not a restatement of it: the
  control plane verifies the publisher's signature over the checksum, which says
  nothing about the bytes that arrived at this particular robot. An adapter that
  flashes without re-hashing is trusting every hop of the delivery path.

  C7.2 and C7.3 are independent. Declining `ModelUpdate` under C7.1 says nothing about
  `PushFirmware`, and an adapter may implement either alone.

## C8 — Edge stream (when a zone declares an edge node)

- **C8.1 (MUST, when `edge_endpoints` is non-empty)** Dial each edge endpoint
  and hold an `EdgeService` stream with the same mTLS and reconnect discipline
  as `ControlStream`.
- **C8.2 (MUST)** Tee a `PositionFrame` per managed robot per telemetry tick to
  every connected edge node, fire-and-forget.
- **C8.3 (MUST)** Act on an edge-issued `Estop` by stopping every robot the
  adapter serves through that edge node (the `Estop` names no robot), and return
  one confirmed `EstopAck`, as on `SafetyStream`. Scope: the harness drives a
  single-robot adapter, so it does not exercise an adapter that serves several
  robots through one edge node. That includes C5.2's two-phase
  answer: a `STOPPING` receipt followed by the terminal `STOPPED` within C5.6's
  budget passes. Suites before `2026.09.28` graded the first ack only and failed
  that answer.
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

  A skip on this row names WHY the path was not exercised, and the two reasons are
  not the same fact. `optional-declined` means the adapter declined the command —
  it does not offer the feature. An adapter that ANSWERED the command and refused
  the probe artifact under C7.2/C7.3 declined nothing: the progress path was made
  unreachable by an earlier, correct refusal, and that records
  `adapter-did-not-reach-state`. Recording both as a decline made the cause mean
  two opposite things, and a cause that cannot be read literally cannot be trusted
  with a profile verdict.

  An adapter that ACCEPTED the install and emitted no progress declined nothing either.
  It reached the path and used the permission this row grants, and the row records
  `optional-not-exhibited`. The row is a skip, not a pass, because a pass here reports
  the update-progress feature as supported. It is not a failure and moves no verdict.
  `C0.1` contradicts that cause only if the adapter declined `push_firmware`. The
  terminal report is a separate obligation, C10.2, and still applies.

- **C10.2 (MUST, when implemented)** Report the TERMINAL result of every install
  the adapter accepts, as an `UpdateProgress` carrying `outcome`
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

  **A refusal is not an install.** An adapter that refuses an artifact in the
  `CommandResult` (`FirmwareResult.accepted = false`, or a `ModelUpdateResult` not
  acknowledged), under C7.2, C7.3 or for any other reason, has answered the command
  there, and no terminal report follows: the rollout acts on the refusal itself (for
  firmware, RFC-0001 `#firmwarerollout` says the controller retries on the next batch
  cycle). A signature failure is answered there, `accepted = false`. An install the adapter
  accepted whose delivered bytes then fail the C7.3 re-hash is not a refusal: it
  ends `INSTALL_OUTCOME_FAILED` and is reported (RFC-0001
  `#fleet-adapter-protocol-fleet-adapter-compliance-checklist`, install-outcome
  reporting). **Not met at v0.4 by the harness:** `_record_terminal_outcome`
  (`adapters/conformance/harness.py`) still grades every install the adapter answered,
  so it fails an adapter that refuses the probe artifact and sends no terminal report.

  **A MODEL report SHOULD name its model** in `UpdateProgress.model_name`, and MUST when
  the adapter can have two model installs in flight at once. Model rollouts are
  independent, so one robot legitimately sits in two rollouts' batches simultaneously; a
  terminal outcome that does not name its model is one the control plane can only correlate
  by guessing, and a wrong guess under `rollbackPolicy: Auto` pushes a revert of a model
  that never failed (ADR-0055). Omitting it stays conformant and wire-compatible — the
  control plane falls back to correlation and **defers** rather than misattributing when the
  answer is ambiguous.

  > **The harness does not yet check `model_name`, and there is no C-row for it.** Adding
  > one is a conformance-suite revision; it re-attests adapters and does not move the
  > contract version ([ADR-0032](../docs/adr/0032-fleet-adapter-contract-versioning-and-conformance-gating.md)
  > Amendment 1 §A1.1). Until one is added this obligation is stated
  > here and enforced by the control plane's behaviour (it defers), not by the suite.

  Conditional on the capability, not universal: declining `push_firmware` /
  `model_update` outright (C7.1) carries no obligation here and remains fully
  conformant.

  **Every install, both commands.** The harness probes `push_firmware` AND
  `model_update`, and an adapter that accepts an install from either owes a terminal
  outcome for it; a refusal is not an install (above). Sending only the firmware
  probe let an adapter hold half a published MUST by never being asked. The model
  probe carries a `model_uri`: an Auto rollback is the one
  `ModelUpdate` that legitimately arrives with no artifact, and a probe without a
  URI asks about the rollback branch rather than about an install.

## Heartbeat — no C-row

The control plane pushes `HeartbeatRequest` and the adapter replies `HeartbeatResponse`; there is
no unsolicited adapter→control-plane heartbeat arm, so an adapter cannot assert its own liveness.
`robot_id` selects between the two questions the exchange asks, and **an adapter MUST answer both,
echoing the request's `robot_id` in either case**:

- **non-empty** — per-robot: *"is this robot alive?"*, sent before a robot whose telemetry has gone
  quiet is declared `Offline`;
- **empty** — adapter-scoped: *"is this adapter alive?"*, pushed on a cadence of half
  `FleetAdapter.spec.heartbeatIntervalSeconds` ([ADR-0054](../docs/adr/0054-the-control-plane-heartbeats-on-a-cadence.md)).
  It names no robot and asserts nothing about any robot.

An adapter that does not answer the adapter-scoped form decays to `Degraded` and then
`Disconnected`, and its robots become inadmissible. The reference adapter satisfies this by echoing
`robot_id` whatever it is; substituting a robot id when the field is empty is the way to get it
wrong.

> **This catalog has no heartbeat C-row and the harness checks none of the above.** Adding one is a
> conformance-suite revision; it re-attests adapters and does not move the contract version
> ([ADR-0032](../docs/adr/0032-fleet-adapter-contract-versioning-and-conformance-gating.md)
> Amendment 1 §A1.1).

## Adapter obligations with no C-row

RFC-0001 states these adapter obligations, and this catalog has no check for any of them. Each is
an obligation all the same: passing this suite does not show an adapter meets them. The RFC-0001
section that states each is named in parentheses.

- **No resume after a stop.** Keep fleet adapter protocol, *The Stop Latch*, *Interrupted work*
  and *An adapter starts with no work*, which bind every adapter whatever contract version its
  connection negotiated: what a stop, an edge stop or a set or unreadable local latch
  interrupted, and what an adapter process holds from before it started, is never continued.
  Those rules are the one statement; this entry does not restate them.
- **The stop latch.** Keep the stop latch, as fleet adapter protocol, *The Stop Latch*, states it,
  on a connection that negotiated contract `1.4.0`. That section is the one statement of the
  rule; this entry does not restate it. `C5.8` grades one part of it: a stop holds the robot's
  work until its clear lifts it, and the lift is acknowledged. No row grades the rest: how a
  fence alone latches, a clear that arrives before its fence, what survives a reconnect or a
  restart, which `ControlStream` counts, and when the latched IDs are reported.

  A connection at `1.3.x` is bound only by the rules *The Stop Latch*, *Version*, names for every
  adapter (no resume after a stop, above, and the local latch, below), and is sent no `AssignAction`
  (safety,
  invariant I6; fleet adapter protocol, *The Stop Latch*; control plane, *Scheduler*, candidate
  filter 9). The simulation adapter keeps the latch with `--latch` and reports `1.4.0`; without
  it, it keeps no latch and reports `1.3.0` (`adapters/simulation/sim_adapter.py`). The adapter
  template keeps it.

  **How the harness meets the latch.** The suite gives the harness robot work after it stops
  it, so it acts as a `1.4.0` control plane does (fleet adapter protocol, *The Stop Latch*):
  - It sets leader term 1 on every `Command`. On a `1.4.0` connection it sends each stop's fence
    with the stop, and clears every stop it issues before the next check gives the robot work:
    an `EstopClear` at term 1, then a wait for the `EstopClearAck` naming the stop. It sends no
    `EstopClear` below `1.4.0`.
  - It reads every stop's answer by its arm and its stop ID: `C5`, `C5.4`, `C8.3` and `C14.1`.
    A report of the local latch, a report of a latched stop, or a fence's own `EstopAck` waiting
    on `SafetyStream` is not taken as the answer to a later stop.
  - It runs `C8.3` last. Its edge stop sets the robot's local latch, which only a local reset at
    the robot lifts, and no harness message can send one.
  - On the session whose registration it refuses, it runs `C3.1` and `C13.2` before `C14.1`'s
    stop. That session negotiated no contract version, so no clear may be sent there.

  A clear that draws no `EstopClearAck` changes no grade. A later check that is refused
  `ROBOT_UNAVAILABLE` is graded as before, and its detail names the stop that was not lifted.
  `C5.8` checks the latch's core. Passing this suite does not show that an adapter keeps the
  rest of it.
- **The local latch.** Keep the local latch of an edge stop, report it with its set ID until the
  `EstopClear` that ends that set takes effect, and accept no work until then, as fleet adapter
  protocol, *The Stop Latch*, and safety, *Edge / Headless Emergency Stop*, state: every adapter
  with an `EdgeStream` sets it, whatever contract version its connection negotiated. Those sections
  are the one statement of the rule; this entry does not restate it. **Not met at v0.4:** the
  simulation adapter keeps a local latch only with `--latch`, and has no local reset
  (`adapters/simulation/sim_adapter.py`).
- **The control plane's identity.** Verify the serving certificate of the `ControlStream` and
  `SafetyStream` peer as security, *Transport Security*, states: that it names the service (a DNS
  SAN) and carries the `serverAuth` extended key usage. A client certificate chained to the same
  CA is not the control plane, and an `EstopClear` from a peer that fails these checks lifts
  nothing.
- **Every `Estop` is acted on.** Never skip a stop, or answer it from a cache, because its
  `estop_id` matches one seen before: a re-send of a stop carries the same stop ID (fleet adapter
  protocol, *Reconnect Action State Reconciliation* and *The Stop Latch*).
- **A stop re-sent on a new `SafetyStream`.** Honour an `Estop` that the control plane sends again
  on the adapter's new `SafetyStream` to a robot whose stop is not cleared (*Sending a stop
  again* says to which robots and what its answer records), exactly as a first one:
  issue the hardware stop before the first `EstopAck` and answer `STOPPING`, `STOPPED` or `FAILED`
  (safety, *Robot Stop Table*, *Sending a stop again*; fleet adapter protocol, *Reconnect Action
  State Reconciliation*).
- **A rejection means not taken.** Never answer `accepted = false` to an `AssignAction` the
  robot has begun executing: under no stop the control plane releases the action on that answer (safety,
  *Assignment Lease and the Single-Executor Guarantee*, *Safe reassignment*; fleet adapter
  protocol, compliance checklist).
- **Directive supersession.** Accept an `AssignAction` of the same `action_type` at a higher
  `fencing_token` in place, and report the displaced action terminal with
  `ACTION_END_CAUSE_SUPERSEDED` and a populated `superseding_action_id`; decline a type change
  with `ASSIGN_ACTION_REJECTION_ROBOT_BUSY` even when the token is valid (fleet adapter protocol,
  *Directive Supersession on the Wire* and the compliance checklist).
- **Cause reporting.** Echo the `CancelCause` acted on in `CancelActionResult.cause`; set
  `end_cause` on every terminal state that is not `ACTION_STATE_SUCCEEDED`; report an action an
  estop ended with `ACTION_END_CAUSE_ESTOP` (compliance checklist; safety, *Estop Is Not a
  Preemption*).
- **Lease renewal.** Answer `renewed = false`, and do not re-arm the self-stop timer, for a
  `renew_lease` whose `action_id` is not the action executing under the current fencing mark,
  and for one that arrives after the local lease has lapsed. `C4.3` checks only a stale
  `lease_generation` (compliance checklist; safety, *Assignment Lease and the Single-Executor
  Guarantee*, renewal acceptance).
- **Custom verbs.** Decline `INVALID_ACTION` (`servable = false` on `ValidateAction`) a `Custom`
  action whose `custom_type` and payload `operation` disagree, or whose verb the adapter does not
  serve, and never run it as another Custom verb (fleet adapter protocol, *one entry per Custom
  verb*).
- **Reconnect reconciliation beyond `CANCELLED`.** `C2.2` sends `ROBOT_ACTION_PHASE_CANCELLED` only.
  `IDLE` with a local action, an `IN_PROGRESS` mismatch, an expired `REVOKING` and `UNKNOWN` are not
  checked (fleet adapter protocol, *Reconnect Action State Reconciliation*), and neither is the hold
  on action streaming (*Register first*, below).
- **Identity and provisioning.** Synthesize a stable `robot_id` for a vendor without one; send
  `DiscoverRobot` after a `NOT_ADMITTED` rejection and re-announce on every provisioning-loop
  interval (fleet adapter protocol, discovery and the compliance checklist). On any refused register,
  stop any work held for the robot; on one refused `accepted: false` with no rejection code, also
  announce nothing (fleet adapter protocol, *The Stop Latch*, *A refused register*).
- **One live process per adapter identity.** Serve an adapter identity from one live process at a
  time; a second process under the same identity is not a second adapter (fleet adapter protocol,
  *Connection lifecycle*).
- **Firmware push on a busy robot.** Refuse a `push_firmware` (`FirmwareResult.accepted = false`)
  while the robot is not Idle, whatever `safetyConstraints.requireIdleState` says (FirmwareRollout,
  *Delivery Mechanisms*).
- **Zone constraints.** An adapter that declares any honoured operating subject implements the
  `zone_constraints` Command (FA-R7), and an adapter that accepts an assignment carrying an
  operating rule honours it for the whole time the robot is in the zone, passing through included
  (FA-R3; nothing on the wire shows it). Treat a `zone_constraints` push that names
  `unresolved_zones` as fail-closed for each named zone, and do not read an empty rule set as a
  lockout (fleet adapter protocol, *`zone_constraints` fields*; the zone-restrictions amendment,
  FA-R7).
- **Commands and requests.** Honour a capability-loss `cancel_action` at a safe stopping point;
  apply its own timeout to a `ResourceRequest` the control plane never answers; handle
  `reservation_granted` when it issues `reserve` requests, and honour a `ReserveResult` before using
  a shared resource (fleet adapter protocol; traffic deconfliction).
- **Telemetry.** Stream telemetry continuously, report the robot's `phase` in every payload, and
  send a motion-gated position frame at least once per idle interval (compliance checklist).
- **Sensor streams.** Keep `stream_id` stable for a robot across reconnects; infer deactivation only
  from `deactivate_stream` or the stream's own lease expiring (`C18.5` checks only that the stream
  stops on `deactivate_stream`); fail an activation whose sink endpoint is absent (sensor
  streams).
- **The leader term.** Keep, for each robot, the highest leader term seen on any message; reject a
  `Command` other than a stop, and an `EstopClear`, whose term is lower; accept a stop whatever its
  term (fleet adapter protocol, *The Stop Latch*, *The leader term*). `C3.10` and `C3.11` define
  the check, and the harness does not run them.
- **Stop-ID reports.** Report the stop IDs latched for a robot on each `SafetyStream` that opens,
  and with each refusal under the latch, each as an `EstopAck` naming the ID and the robot's state
  (fleet adapter protocol, *The Stop Latch*, *Report*).
- **Register first.** Resume no `ActionStatusUpdate` streaming and no `ControlStream` action RPC
  until every `RegisterAck` has been processed and local state reconciled against each robot's
  `authoritative_action_state` (fleet adapter protocol, *Reconnect Action State Reconciliation*).
- **The retained outcome.** On a connection that negotiated contract `1.4.0`, retain the terminal
  outcome of the robot's last action until the control plane acknowledges it, and report it with
  `retained = true` on each reconnect once every `RegisterAck` is processed (fleet adapter
  protocol, *Reconnect Action State Reconciliation*, *The retained outcome*). `C16.2` defines the
  check, and the harness does not run it.
- **Durable deduplication.** Deduplicate every delivery by the assignment's report token, durably
  across the adapter's own restart, and never execute an assignment twice (RFC-0001 safety chapter,
  the adapter's rules; Fleet Adapter Protocol, *Fencing-token persistence*).
- **`assignment_id`.** Never use `assignment_id` to tell two assignments apart: `action_id`
  together with `lease_generation` identifies an assignment attempt (fleet adapter protocol, *The
  ControlStream service*).
- **Standard probe metrics.** Support the standard metrics for the robot's hardware types, under
  the names RobotProbe lists (RobotProbe, *Standard metric names by hardware type*). `C9.1` asks
  only that a `VerifyResult` the adapter answers carries measured values.
- **Stream commands in pairs.** Decline `deactivate_stream` whenever it declines
  `activate_stream`; declining one and accepting the other is malformed (fleet adapter protocol,
  compliance checklist, *Optional commands*).
- **Sink loss.** When a stream's sink endpoint stops consuming mid-run, never buffer without bound
  and never redirect the stream to the control plane; `onEndpointLoss` decides what follows (sensor
  streams, *Failure modes*). `C18.2` compares the `ControlStream` byte rate before and after an
  activation, and does not exercise a sink that stops consuming.
- **Heartbeat.** See *Heartbeat — no C-row yet*, above.

## C11 — Pause / resume

- **C11.1 (MUST)** Accept `pause` (`PauseCapabilities`) and `resume`
  (`ResumeCapabilities`), answering with a `PauseResult` / `ResumeResult`. When
  `require_stop_before_ack` is set, `paused=true` MUST be reported only once the
  robot is CONFIRMED at rest (never inferred). Declining is NOT conformant: these
  are in RFC-0001's Required-message table and absent from its optional-command
  list, and they are the commands through which RFC-0001's ZoneMaintenance resource is
  specified to drain a zone. **The obligation on the adapter stands although the
  in-tree control plane does not exercise it:** no controller sends `pause` or `resume`
  at this contract version, so a maintenance window changes control-plane state only
  and does not halt a robot
  ([known limitations](../docs/known-limitations.md#a-maintenance-window-does-not-halt-robots)).
  An adapter MUST NOT infer from their absence in a deployment that they are optional.

## C12 — Capabilities scan

- **C12.1 (MUST)** Answer a `ScanCapabilities` request with a full
  `CapabilitiesSnapshot` (all hardware / installed models). Declining is NOT
  conformant: `scan` is how the control plane learns the adapter's
  `supported_actions` catalog, which is the pre-dispatch gate whenever the
  optional `validate_action` is declined. An adapter declining both is
  undispatchable in practice.
- **C12.2 (MUST)** Send an unsolicited `capabilities` `CapabilitiesSnapshot` for each
  robot at registration, carrying the same `supported_actions` catalog — types,
  required capabilities and parameters — as the `scan` reply, and send one again
  whenever the catalog changes. No control plane issues `scan` at this contract
  version, so a snapshot the adapter sends unasked is the only way its catalog reaches
  the plane; an adapter that never pushes one has every action refused at admission,
  and one that pushes a different catalog from its `scan` reply is admitted work it
  then refuses.

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

**Two reports carry a `conformant` field, and they do not mean the same thing.**

- The harness's own JSON report (`python -m adapters.conformance --json`) writes
  `conformant: true` when no MUST or MUST NOT check **failed** among the checks that
  ran. A skipped MUST does not make it `false`. This is the report a
  `FleetAdapter.spec.conformanceReport` references, and the one the control plane
  reads to set `status.conformance`.
- The profiled conformance report (`make conformance-profiles`) grades every row and
  writes `conformant: true` only when every Core row of every required profile was
  **measured** and passed; its `verdict` says whether a `false` is a `fail` or
  `not-established`. That is the report a published claim rests on.

A harness report reading `conformant: true` beside a profiled report reading
`verdict: not-established` is the expected state for every adapter today. `C1.3`
and `C3.7` need harness work (TLS from a test CA, a restart hook), `C1.4` has no
wire observable, and `C6.3` grades the control plane's write path, so no adapter
can be measured on all four.

### What the report attests, and against which version

A result is earned against a specific **contract version** — a semver over the proto surface, the
`SupportedAction` schema and contract changes off the wire — not against the standard as a whole. It is not a version of this suite: the
checks below are the *evidence* for a result, never a component of the version it is earned against, which
is why adding a check re-attests adapters without moving the version any adapter negotiates. The harness
stamps the contract version into the report as `contract_version` (alongside `protocol_version`, the
wire-package identity `fleet_adapter.v1`, which is not a semver and cannot express compatibility). Record
that semver in your [`REGISTRY.md`](REGISTRY.md) row, and record the suite revision you ran alongside it —
never folded into it — both in that row and in `FleetAdapter.spec.conformanceReport.suiteVersion`.

A **major** bump is breaking: re-run `make conformance` and update the row. Minor and patch bumps never
invalidate an existing qualification. A minor bump can still take an adapter out of range, because the
window is relative to the control plane, not to a date: a control plane accepts minor N and N-1 of the
contract version it implements, within its major, so an adapter built at an older minor stays admissible
until the control plane's version moves past it. A change that binds adapters for safety raises the lowest
supported version to the new one at once. Outside the window the adapter's `RegisterRobot` and
`DiscoverRobot` are refused `VERSION_MISMATCH` until it declares a version inside it (ADR-0032 Amendment 1
§A1.2). The contract is `1.4.0` and its range is `>=1.3.0 <1.5.0` (RFC-0001, *Package and
versioning*). `1.4.0` is not published until the v1 proto carries the wire additions that section's
*Current version* paragraph lists, the one list of them. A robot is
given work only over a connection that negotiated `1.4.0`, because `1.4.0` carries the stop latch: a
`1.3.x` adapter keeps its qualification, registers, reports and is stopped, and is given no work
(RFC-0001, control plane, *Scheduler*, candidate filter 9).

**A `1.3.x` connection.** The harness negotiates per connection, as a control plane does (RFC-0001,
*Package and versioning*): it agrees `1.3.x` with an adapter that reports a `1.3` version. On that
connection it sends every stop and its fence, and no `AssignAction` and no `EstopClear`. Each row that
needs an assignment records `contract-predates-obligation`, which the verdict counts as never an obligation, although the Fleet Adapter Protocol binds the fencing and lease rows at every contract version, so a `1.3.x` report is no evidence that the adapter meets them: `C2.2`,
`C3.1` to `C3.6`, `C3.8`, `C3.9`, `C4.1` to `C4.4`, `C5.8`, `C6.4`, `C16.1`, `C17.1`, `C17.2`, `C17.4`
and `C19.1` to `C19.5`. `C2.3` is measured on a robot nothing drove. Every other row grades the
adapter as at `1.4.0`: an adapter that does not answer a stop on a `1.3.x` connection fails `C5`. The
harness told every compatible adapter `1.4.0` until suite `2026.10.17`.

Conformance is **self-certified** ([ADR-0007](../docs/adr/0007-conformance-self-certification.md)):
you run the harness, the registry pull request attests the result, and a control plane only
*consumes* it — after checking the report's digest, and its signature where the namespace requires
one. There is no certification authority, and this project does not operate one. A submission is expected to include a test suite
demonstrating C3–C6 in particular, since those encode the safety and
anti-double-execution guarantees the standard exists to provide.

## C16 — Task status reporting

- **C16.1 (MUST)** Send an `ActionStatusUpdate` at every task phase transition,
  terminal states included. The control plane drives `FleetAction`
  `Assigned -> InProgress` from it (row A17 of RFC-0001
  `#safety-action-table`). What ends a task whose adapter
  sends no terminal update is stated by the other rows of that table.

  Both halves are checked, in one row. The transition half is the acceptance
  update for the assignment this check sends; the terminal half is the update for
  the task the harness CANCELLED under C3.6 — a task driven to an end, so an
  adapter that reports nothing has left exactly the rollout the second clause
  describes. An `ACTION_STATE_STOPPED` before it is the commanded stop, not an
  end, and does not satisfy this half. Matching the first update for an action
  matched the acceptance `RUNNING` and asserted only the first clause.
- **C16.2 (MUST at the attested contract version; NOT YET RUN)** Retain the terminal outcome
  of the robot's last action until the control plane acknowledges it, and report it on
  reconnect (RFC-0001 `#fleet-adapter-protocol-reconnect-action-state-reconciliation`, *The
  retained outcome*). The report is the action's terminal `ActionStatusUpdate` with its report
  token and `retained = true`, sent once the robot's `RegisterAck` is processed. An outcome the
  control plane acknowledged with an `ActionOutcomeAck` is not reported again, and only a
  retained outcome carries `retained = true`.
  **What it observes.** On a connection that negotiated `1.4.0`, the harness assigns an action,
  closes both streams before the action ends, and lets the action finish. On the redial it
  answers the register and expects the terminal report, `retained = true`, carrying the
  assignment's token. It does not acknowledge it, closes the streams again, and expects the same
  report after the next register. It then acknowledges it and redials once more: no retained
  report follows. An adapter process restart between the two redials does not change what is
  expected.
  **The hazard.** A robot that finished its work during a network drop loses its report with the
  stream. The control plane then holds the finished work for an operator, who may send it to
  another robot, so the work is done twice.
  **Version binding.** The row grades only a connection that negotiated `1.4.0`; a `1.3.x`
  connection carries neither `retained` nor `ActionOutcomeAck`.
  **Not run yet.** C16.2 is defined here for draft 4. The harness
  (`adapters/conformance/harness.py`) does not run it, and the row is registered in no profile
  (`conformance/profiles.yaml`) until a suite revision adds its probe (RFC-0001 *Conformance
  Profiles and Support Levels*). Until then it does not count toward conformance.

## C17 — Zone operating constraints (ADR-0058)

An operator states a site rule once — *no cameras in patient rooms*, *no water on
the carpet* — on the `FleetZone` that names the region, and every path through the
fleet respects it. Axis 3 of that model constrains what a robot may have **running
while it is present**, which is true or false of a robot that was asked to do
nothing at all: the deciding case is a robot **transiting** a ward with a live
microphone, where no action was refused, no capability was required, and the
constraint was still broken.

**Nothing in a control plane can switch a camera off.** Three obligations an adapter
can be held to follow from that, with a fourth withdrawn, and only one of them is an
observable message on a wire — plus a fifth row below
that is not an obligation of its own but the guard that stops the observable one being
satisfied by a reflex:

| Obligation | Testable here |
| :-- | :-- |
| **Declare** — the adapter states which operating subjects it can honour, in `CapabilitiesSnapshot.honoured_operating_constraints` | Its **shape**, as `C17.3`. Declaring none is not a breach. |
| **Carry** — the control plane sends the resolved effective rules on `AssignAction` | Not an adapter obligation, but its ARRIVAL is now graded: `C17.4` requires the adapter's answer to depend on the subjects that arrived. |
| **Honour** — the adapter keeps the rule for the whole time the robot is in the zone | **No check, and there cannot be one from this surface.** Whether a camera is off is not observable in any message the adapter sends. |
| **Attest** — withdrawn at v0.4 (FA-R4 of the zone-restrictions amendment) | **No obligation and no check.** See *What is not checked here*, below. |
| **Decline** — the adapter refuses an assignment carrying a rule it cannot honour, **naming the subject** | `C17.1` and `C17.2`. This is the one an adapter can be failed on. |
| **Read** — the refusal is a function of what the assignment carried, not a reflex | `C17.4`. Without it, an adapter that declines everything passes both decline rows without reading the message. |

- **C17.1 (MUST)** Decline an assignment carrying an operating constraint the
  adapter cannot honour, rather than accepting it and ignoring the rule. The
  decline is `accepted: false`, `rejection: ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET`,
  and `AssignActionResult.unhonourable_subjects` **naming every subject it could not
  honour**. Declining costs the adapter nothing: the action returns to `Pending` and
  the Scheduler tries the next candidate. Accepting it costs the operator the rule —
  the control plane has authorised an action it cannot police, and has no idea
  whether the subject was running.
  **One check over the seven, not seven checks.** The normative core is closed at
  `camera`, `microphone`, `water`, `chemical`, `uv`, `speed` and `noise`, and
  conformance covers those only. The check dispatches **one** `AssignAction` carrying
  every core subject the adapter did not declare it honours, in all three modes
  (`Denied`, `Limited` with `max`, `Limited` with `allowed`, and `Required`), and
  requires the decline to name all of them. That is strictly stronger than seven
  separate probes, each of which an adapter could satisfy by naming the first subject
  it saw and stopping.
  **A decline for the wrong reason fails.** `INVALID_ACTION` or `ROBOT_UNAVAILABLE`
  tells the control plane something else: only this reason says the robot cannot
  honour the rule, and only this one carries the subject list an operator needs to
  see why. When a decline releases the assignment is row A24 of RFC-0001
  `#safety-action-table`.
  **Skipped**, with cause `all-core-subjects-honoured`, when the adapter declared all
  seven — the run can then construct no core rule it cannot honour.
- **C17.2 (MUST)** Decline an assignment whose operating `subject` the adapter does
  **not recognise**, naming that subject, under the same rejection reason as `C17.1`.
  **An unrecognised subject is not an absent subject.** A `subject` containing a `/`
  is a namespaced extension whose prefix is a DNS domain the extender controls;
  treating one as absent would make that form a universal bypass, since any vendor
  could then route around every operating rule by inventing a subject. The extension
  form is exercised exactly **once**, here, with `example.com/leaf-blower` — the RFC
  2606 example domain, so no adapter can honestly claim to honour it. One probe
  establishes the property as well as a hundred would.
- **C17.3 (MUST)** Declare `honoured_operating_constraints` in a well-formed
  vocabulary: a subject containing no `/` is one of the core seven, and a subject
  containing a `/` is `<dns-domain>/<name>`. **Declaring none passes** — an adapter
  that honours no subject is not in breach, and its robots are withheld from
  zones that restrict one.
  **Why the shape is worth a check.** The control plane **drops** an entry outside
  the vocabulary rather than rejecting the whole declaration, so one typo cannot cost
  a robot the valid subjects beside it — and therefore fails nowhere. A dropped
  subject is an undeclared subject: the robot is withheld from zones restricting it,
  by a filter that never says why. This row is where a typo becomes visible.
- **C17.4 (MUST)** **Accept** an assignment carrying only operating subjects the
  adapter has declared it honours. This is the row that establishes that the carry
  **arrived**, and it exists because `C17.1` and `C17.2` cannot.
  **Both decline rows require a refusal, and a refusal is cheap to fake.** An adapter
  that refuses every assignment carrying any `operating_constraints` — or every
  assignment at all — passes `C17.1` and `C17.2` without ever reading the message.
  Its green lines would then mean *this adapter declines*, not *this adapter was
  told*. Passing all three is possible only for an adapter whose answer **depends on
  the subjects that arrived**, which is the observable consequence of the constraint
  set reaching it.
  **The probe carries only declared subjects**, so a decline would be the wrong
  answer for a reason the adapter itself stated; the accepted task is cancelled at the
  same fencing token immediately afterwards. **Skipped**, with cause
  `adapter-declares-no-operating-subject`, when the adapter declares none — there is
  then no set it claims to honour and no way to tell reading from reflex. This row
  is then unexercised for that adapter; the `zone-restrictions` profile still applies to it,
  and its other rows grade it.

### What these rows establish, and what they do not

**They are LIVE, not static.** The adapter under test is a running process this
harness dials into, and every row above is graded on a message that adapter actually
sent. A green `C17.1` means the adapter **declined**, on the wire, an assignment it
was really sent — not that it declared a decline path somewhere. That is the
difference between this surface and the `ControlPlane` surface's `CP7.1`/`CP7.2`,
which are read off shipped schemas and establish only that the surface exists.

**`C17.4` is what makes a green line mean the adapter was TOLD.** Live is necessary
and not sufficient: an adapter can send a real refusal on a real wire without having
read the message it refused. `C17.4` requires an acceptance for a set the adapter
declared, so the family as a whole grades a *function of the constraint set* rather
than a reflex. **What none of these rows establishes is that a real control plane
fills the field** — this harness IS the control plane here, and it fabricates the
rules. The plane's half is verified in the reference implementation's own tests
(`internal/controller/zone_operating_constraints_dispatch_envtest_test.go` for the
dispatch path, `internal/command/operating_constraints_wire_test.go` for the message),
and on the `ControlPlane` surface `CP7.3` records that the profiler cannot establish
it from artefacts.

**A green `C17.3` is not a measurement.** It grades the *shape* of a declaration.
Whether a subject an adapter declared is actually held off is the *honour*
obligation, and nothing the adapter sends can establish it.

**The `zone-restrictions` profile is ALWAYS applicable.** It used to apply only when
the adapter declared at least one honoured subject, and this paragraph used to explain
why that was compatible with the decline rows being graded in every report. It was not
worth explaining: the specification's own load-bearing case is a run going RED against
an adapter that **accepts and ignores** a rule, which is an adapter declaring
**nothing** — so the profile was reported `notApplicable` for exactly the adapters whose
red run it demanded, and a reader saw `notApplicable` beside a failing MUST from the
same profile.

`C17.1`'s obligation has no antecedent about declaring — an adapter that honours nothing
cannot honour anything, so every rule it receives is one it must decline — and the
profile's applicability now says the same thing. No row here needed the conditional:
`C17.1` and `C17.2` have no declaring antecedent, `C17.3` grades the **shape** of
whatever was declared and **declaring none passes**, and `C17.4` needs a declaration to
exist and carries its own enumerated skip cause for the absence. Corrected 2026-09-14.

### What is not checked here: attestation

An adapter reporting *"camera off"* has asserted something. It has not measured
anything, and the control plane cannot. **An attestation is a claim, not a
measurement**, and this catalog says so rather than implying otherwise. FA-R4 is
withdrawn at v0.4, so attestation is not an obligation an adapter can fail.

There is **no attestation check at all** in this suite, and the reason is narrower
than the paragraph above: `fleet_adapter.v1` carries no field for one.
`ActionStatusUpdate` has no honoured-subject list, and no other terminal message does
either, so there is nothing on the wire to read. A check asserting that an
attestation *arrived* would have had to invent the field it read. Until the wire
carries it, the honest report is that the attest obligation is **specified and
unverified** — which is what this section says instead of a green line.

## C18 — Sensor streams and the duty cycle

A sensor stream is a named, continuously-producible output of a robot's sensor that an
off-robot compute run consumes. The control plane switches it on with `ActivateStream`
and off with `DeactivateStream`, refcounted per `(robot_id, stream_id)` and bound to a
lease. Two properties govern everything below.

**Frames never enter the control plane.** `StreamSink` names the compute runtime's
address and the adapter ships frames there on its own data path. The control plane
learns *that* a stream is active and never what the camera saw.

**The safe direction is off.** The adapter stops the stream when its lease expires
without a renewal, so a control-plane partition *stops* a camera and can never leave one
running. This is the deliberate **inverse** of the emergency-stop lease direction: a stop
must always be honoured, so nothing may gate it; a stream's failure mode is *left on*,
because an unattended, still-powered sensor is itself the hazard, the cost and the
privacy exposure. Each fails toward its own safe state and those states point opposite
ways.

The whole family is **optional**: an adapter declining `activate_stream` declines the
duty cycle for its robots, which is conformant under `C7.1` and means nothing activates,
so nothing can be left on. An adapter honouring `activate_stream` MUST also honour
`deactivate_stream` — declining one while implementing the other leaves every stream it
starts running until its lease lapses.

- **C18.1 (MUST)** Declare sensor streams in a well-formed
  `CapabilitiesSnapshot.sensor_streams`. Every entry carries a non-empty, unique
  `stream_id`; an `activation_cost` that is not `UNSPECIFIED`; and no
  `clip_pre_seconds_max` on a stream declaring `supports_clip: false`.
  **Why each clause is load-bearing.** `stream_id` is the key an activation is
  **refcounted on**, so an empty or duplicated one produces two activations for one
  sensor and no way to tell when the last consumer left. `activation_cost` is how an
  author discovers whether switching a stream on costs anything; `UNSPECIFIED` leaves
  them unable to tell an always-running odometry feed from a camera that draws power.
  A clip bound on a stream that cannot serve clips is a contradiction an author
  discovers only when an activation is refused, in a building.
  **Declaring no stream passes.** A robot with nothing to offer offers nothing, and the
  rest of this family is then not an obligation of it.
- **C18.2 (MUST NOT)** Route sensor-stream bytes through `ControlStream`, `EdgeStream`,
  or any other control-plane-terminated channel. The check baselines the ControlStream's
  byte rate while idle, activates a stream, and requires the rate not to rise to carry
  it.
  **This is the property that keeps a fleet's video off the control plane's disks and
  out of its blast radius**, and it is why `StreamSink` carries an address rather than
  the control plane carrying bytes.
  **LIMIT, stated on the row as well as here.** A byte-rate comparison detects
  **relaying** — a stream's worth of traffic appearing where it must not — and not
  steganography. An adapter dribbling frames through an existing field below the
  threshold passes this check and still violates the rule.
- **C18.3 (MUST)** Stop a stream when its lease expires without a renewal. Two
  independent single-probe trials bracket the horizon: **held** — the stream survives
  renewals across more than one whole lease duration; **partitioned** — the stream is
  gone after the horizon has passed with nothing sent.
  **Why two trials and not a poll.** The only wire observable for stream state is
  `DeactivateStream`, whose answer distinguishes *there was a stream and I stopped it*
  from *there was nothing to stop* — and asking **consumes** the activation. A polling
  loop would switch the stream off with its first poll and report an interval of one
  round trip. `fleet_adapter.v1` carries no stream-status message, no active-stream field
  on any status message and no read-only stream query, so this is a gap in the wire and
  is recorded as one rather than worked around silently.
  **Why the held arm exists.** Without it an adapter that stops on a fixed timer
  regardless of renewals passes — a sensor abandoning live work, which is the same
  defect from the other side. It is what `C4.1` is to `C4.2`.
  **The interval is measured elsewhere, on purpose.** `make stream-lease-measure` stands
  up a control plane, lets the adapter hold a stream under a renewed lease, kills the
  control plane **hard** (RPCs cancelled, port closed — a graceful close is a legitimate
  *the session is over* that an adapter may shut down on, and timing a process exit measures
  nothing), and reads the stop instant off the adapter's own clock. A fail-safe claimed
  without a number is an intention.
- **C18.4 (MUST)** Refuse a `CLIP` activation against a stream declaring
  `supports_clip: false`, and **bound** rather than refuse a `pre_seconds` above the
  declared `clip_pre_seconds_max`.
  **The two bounds fail differently and the asymmetry is deliberate.** A clip against a
  stream that cannot serve one asks for something unavailable at any length, so the
  activation is refused. A pre-roll longer than the maximum asks for something the
  stream *can* serve, only not for as long — so it is bounded, because a shorter
  pre-roll is still evidence and refusing would discard the post-roll too.
  **An implementation that bounds MUST say so in `ActivateStreamResult.message`.** The
  result carries no structured window echo, so a consumer that is not told has no way to
  discover it received less pre-roll than it asked for.
- **C18.5 (MUST)** Stop a live stream on `DeactivateStream`, reporting
  `deactivated: true`. An adapter that ignores it keeps a sensor running for a whole
  lease after the last consumer settled, every time — the lease is the **fail-safe**, not
  the ordinary path.

### What is not checked here: refcounting, and absence-is-not-signal

**Refcounting is a control-plane obligation, and the wire cannot confirm it.** The
control plane issues one activation per `(robot, stream)` at the maximum `rate_hz` any
live consumer requested, re-issuing under the next fencing token as consumers join.
`ActivateStreamResult` has two fields, `activated` and `message`: it echoes no fencing
token, no rate, no window and no lease. The contrast inside one contract is exact —
`AssignActionResult` echoes `accepted_fencing_token` so the control plane can confirm
its assignment won, and `AssignActionRejection` makes `STALE_FENCING_TOKEN` a typed
rejection. The stream commands have neither, and the monotonicity rule that requires an
adapter to reject a stale token names only `AssignAction` and `CancelAction`. So two
re-issues that reorder in flight are **not detectable on this wire**, and no check here
can establish that a rate in force is the rate the control plane last asked for.

**Absence-is-not-signal is a consumer obligation.** A gap in frames MUST NOT be read as
an absence of the thing being looked for: a run whose stream dries up fails on its own
timeout rather than reporting *nothing detected*, because a confident negative computed
from no evidence is worse than an error. That governs what a compute run may conclude,
which is not a message on this wire, so no row above grades it and none pretends to.

## C19 — Serving only what is declared

An adapter's catalog (`CapabilitiesSnapshot.supported_actions`) says what it can serve.
The control plane admits and dispatches against it, and the adapter is the last gate:
`validate_action` is optional, and a catalog projection can be stale. RFC-0001: an adapter
MUST NOT treat an `action_type` it does not recognise as a nearest-neighbour verb; it
rejects the assignment. An adapter that runs every assignment as navigation drives to
some default point on a `Clean`, and on a `Navigate` with no target.

- **C19.1 (MUST)** Decline an `AssignAction` whose `action_type` is not in the adapter's
  declared catalog with `ASSIGN_ACTION_REJECTION_INVALID_ACTION`, and do nothing with it.
  Paired with a control: the same assignment carrying a declared type MUST be accepted,
  so an adapter that refuses every assignment does not pass.
- **C19.2 (MUST)** Decline an `AssignAction` whose payload omits a parameter its catalog
  entry marks `required` with `ASSIGN_ACTION_REJECTION_INVALID_ACTION`. Paired with a
  control: the same assignment with every required parameter MUST be accepted. An adapter
  that declares no required parameter cannot be probed here, and the row records that as
  unestablished rather than inapplicable: an action that needs a target it never declared
  is the case this row exists for.
- **C19.3 (MUST)** Decline an `AssignAction` that states no `action_type` with
  `ASSIGN_ACTION_REJECTION_INVALID_ACTION`, and do nothing with it. A catalog cannot
  declare an empty type, so an untyped assignment is one the adapter does not recognise:
  it MUST NOT be taken as the catalog's sole type, or as any other. C19.1 names a type and
  cannot see this. Paired with a control: the same assignment carrying a declared type
  MUST be accepted.
- **C19.4 (MUST)** Decline an `AssignAction` whose payload gives a parameter its catalog
  declares with `kind: "enum"` a value outside that parameter's `allowed` values, with
  `ASSIGN_ACTION_REJECTION_INVALID_ACTION`. Paired with a control: the same assignment with
  an allowed value MUST be accepted. An adapter that declares no enum parameter has no
  declared set of values to enforce, and the row records `adapter-declares-no-enum-param`,
  which is never an obligation. Numeric `min`/`max` bounds are not graded by this row.
- **C19.5 (MUST)** Accept a `Custom` `AssignAction` that carries no `custom_type` when its
  payload's `operation` names a verb the adapter declared. `custom_type` is optional:
  RFC-0001's `AssignAction` sets it "only for a Custom action that names one", and the adapter
  takes the verb from the payload's `operation` when it is not set (fleet adapter protocol,
  *One entry per Custom verb*). A control plane below `1.4.0` never sets it. The probe names
  the first declared `Custom` entry's verb: its `custom_type`, or the first allowed value of
  its `operation` enum. An adapter that declares no `Custom` verb records
  `adapter-declares-no-custom-verb`, which is never an obligation. A `Custom` action that
  names no verb is not graded: no adapter obligation states what it must do with one.

**How the harness probes.** Every assignment the harness sends, other than the refusal
probes of this section, carries
an action type from the adapter's own `scan` reply (`Navigate` when it is declared) and
every parameter that type declares required. When that entry is a `Custom` entry keyed by
`custom_type`, the assignment carries that `custom_type`, as a control plane sets it from
`FleetAction.spec.customType`; with it set, the payload need not repeat the verb. The
probes and controls of this section carry it too when they name a declared entry. `C19.1`'s
and `C19.3`'s probes name none, and `C19.5`'s names its verb only in the payload, by design.
An adapter that declares no action has
nothing the harness can ask it to do, and every row that needs an accepted assignment is
recorded `adapter-declares-no-action`: an adapter that serves nothing cannot hold the
`dispatch` profile.

