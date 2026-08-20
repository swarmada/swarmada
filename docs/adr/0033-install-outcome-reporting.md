# ADR-0033: Install-outcome reporting for firmware and model rollouts

- **Status:** accepted
- **Date:** 2026-07-31
- **Deciders:** Principal Software Architect, API Designer, Distributed Systems Reviewer, Security Reviewer
- **Related:** ADR-0019 (adapter action discovery and validation), ADR-0026 (robot liveness projection), ADR-0032 (contract versioning and conformance gating), RFC-0001 §9.1.3 (Robot) / §9.1.8 (FirmwareRollout) / §9.1.9 (ModelRollout) / §9.2 (Fleet Adapter Protocol) / §9.6.5.1 (Required Events); `proto/fleet_adapter/v1/fleet_adapter.proto` (`UpdateProgress`, `InstalledModel`, `FirmwareResult`, `ModelUpdateResult`, `CapabilitiesSnapshot`), `adapters/CONFORMANCE.md`

## Context

A firmware or model rollout pushes an artifact to a robot and then needs to learn how it went. The push is
acknowledged quickly; the install is long-running (download, verify, install, reboot). The acknowledgement and
the outcome are therefore two different facts arriving at two different times, and the contract models only
the first.

What exists today, as built:

- **`PushFirmware` / `ModelUpdate`** are OPTIONAL commands. Their results — `FirmwareResult.accepted`
  ("download begun") and `ModelUpdateResult.acknowledged` — are **acceptance acks**. Neither reports what the
  install did.
- **`UpdateProgress`** streams advisory per-robot progress (`robot_id`, `kind`, `phase`, `percent`) and is
  fully wired: `ControlStream` → `UpdateProgressIngestor` → the active rollout's
  `status.currentBatch[].updatePhase`. It correlates `robot_id` to a `Robot` and locates the active batch. Its
  phase strings are non-terminal by construction (firmware: Pulling/Installing/Verifying/Rebooting).
- **`CapabilitiesSnapshot.installed_models[]`** carries `InstalledModel{name, version, status,
  running_version}` with a `ModelStatus` enum that includes `FAILED`. There is no firmware equivalent — no
  message in the contract describes a robot's firmware install state at all.

The gap is larger than a missing audit event, and the audit row is the symptom rather than the disease. Three
findings, each verified against the tree:

1. **No adapter-reported install state reaches `Robot.status`.** `CapabilitiesSnapshot.installed_models` is
   projected onto `DiscoveredRobot.status.reportedModels` at registration and nowhere else. The robot status
   sink projects battery and hardware only. `Robot.status.installedModels[]` is written by exactly one
   function, `upsertModelStatus`, called from exactly two sites, both with `Updating`.

2. **The model rollout therefore cannot complete.** `classifyModel` reaches `modelDone` only via
   `status == Active && runningVersion == target`, and reaches `modelFailed` only via `status == Failed`.
   Nothing in production writes `Active`, `Failed`, or `runningVersion`. A robot set to `Updating` stays
   `Updating` for good. `MODEL_UPDATE_SUCCEEDED` and `MODEL_UPDATE_FAILED` are consequently unreachable, and
   `installedModels[].failureReason` — which `modelFailureReason` reads — has no production writer at all.

3. **`FIRMWARE_INSTALL_FAILED` has no signal to key on**, and `status.failedRobots` is never populated.

Two of those audit rows are marked `emitted` in §9.6.5.1. They pass the mechanical Status check
because it verifies that a production writer *references* the event constant — a weaker property than the
writer being *reachable*. The column has drifted again, in a way the existing gate is blind to.

A constraint bounds the fix. `push_firmware` and `model_update` are OPTIONAL commands, and the optional-command
rule (declining MUST NOT cost an adapter's robots their work) is load-bearing. Whatever reports the outcome
cannot become mandatory for adapters that do not implement updates at all.

## Decision

Model the install **outcome** as a first-class, adapter-reported fact, delivered on two paths with different
jobs, and project it onto `Robot.status` so both rollout controllers and the audit log read the same state.

- **Terminal outcome on the streaming path (latency).** Extend `UpdateProgress` with an explicit terminal
  outcome enum — `INSTALL_OUTCOME_UNSPECIFIED` (not terminal; the message is progress, as today), `SUCCEEDED`,
  `FAILED` — plus a `failure_reason` string and the version the robot is left running. An explicit enum, not a
  bool: a defaulted `false` must never be readable as "failed" (AGENTS.md, explicit presence on
  safety-relevant scalars). This reuses plumbing that already exists and already correlates a report to the
  right rollout batch.

- **Durable outcome in the state snapshot (recovery).** Add `failure_reason` to `InstalledModel`, and add a
  firmware counterpart to `CapabilitiesSnapshot` describing the robot's firmware install state (running
  version, status, failure reason). A purely streamed terminal event is lost if the control plane restarts or
  the stream drops mid-install — and that loss is *exactly today's failure mode*, a rollout wedged forever in
  `Updating`. The snapshot lets a control plane that missed the stream recover the outcome on the next scan.
  Stream for timeliness, snapshot for truth.

- **Project adapter-reported state onto `Robot.status`.** This is the piece missing on both paths.
  `Robot.status.installedModels[]` gains an adapter-reported writer (today it holds only the control plane's
  own record of what it pushed), and `Robot.status` gains firmware install state alongside the existing
  `firmwareVersion`. Written on change only, never on a telemetry tick (RA-1); both carriers are already
  event- or scan-driven, so neither introduces a per-tick write.

- **Confirmed, never inferred — the audit boundary.** `FIRMWARE_INSTALL_FAILED` and `MODEL_UPDATE_FAILED` seal
  **only** on a reported terminal `FAILED`. A rollout that gives up on an unresponsive robot is a legitimate
  *operational* outcome and is recorded as such on the rollout, as **unconfirmed** — it MUST NOT seal an
  install-failure entry. "Nothing was heard" is not "it failed", and a chain whose value rests on every entry
  being confirmed cannot carry an inference. This is the same discipline that keeps a TTL sweep from
  masquerading as an operator rejection.

- **Conditional conformance requirement.** Reporting a terminal outcome is REQUIRED **if and only if** the
  adapter implements `push_firmware` or `model_update`. Declining the optional command entirely remains free
  and costs nothing (the rule stands). But an adapter that accepts an update and then never reports an outcome
  wedges the rollout, so the obligation attaches to the capability, not to every adapter.

- **Additive only.** New fields and a new message; no field renumbered or removed, so the `make proto-lint`
  breaking-change check stays green and the change carries no contract-version major bump (ADR-0032). It is a
  **minor** contract-version increment: an N-1 adapter that never reports outcomes is still compatible, and
  degrades to the unconfirmed path above.

- **Correct the Status column, and the gate behind it.** `MODEL_UPDATE_SUCCEEDED` and `MODEL_UPDATE_FAILED`
  are corrected from `emitted` until their writers are reachable. The Status check is strengthened, or its
  limitation recorded where a reader will see it: *a writer that references the constant is not proof the
  writer can fire.*

## Consequences

**Positive.**

- A model rollout can complete. Today it cannot; this is a functional fix, not only an evidence one.
- Both install-failure audit rows become genuinely reachable, on confirmed signals.
- One reported-state shape serves the rollout controllers, the audit log, and `swarmctl`/`swarmtop`.
- The recovery path removes a class of permanently-wedged rollouts caused by a dropped stream.

**Negative / accepted costs.**

- The three reference adapters (ros2, vda5050, mavlink) must report outcomes to stay conformant for the update
  commands they implement, and `adapters/CONFORMANCE.md` gains a case.
- Two `emitted` rows are corrected *downward* before they are corrected upward. Understating what ships is the
  lesser error, and the honest one to publish in the interval.
- `Robot.status` grows an adapter-written region, so the RA-1 discipline now has one more place it must hold.

**Rejected alternatives.**

- **Carry the outcome on `FirmwareResult` / `ModelUpdateResult`.** These are acceptance acks returned long
  before the install finishes. Making them terminal would mean holding a command open across a robot reboot.
- **Infer failure from a stalled `updatePhase` or a batch timeout.** This is the tempting one, because it needs
  no contract change. It fabricates a fact: a slow robot, a dropped stream, and a genuinely failed install are
  indistinguishable from the control plane's side. It is rejected for the audit entry outright, and admitted
  for the *rollout* only under the explicit `unconfirmed` label above.
- **Snapshot only (no streamed terminal outcome).** Correct but slow: outcome latency becomes the scan
  interval, which for a staged rollout means batches advancing far behind reality.
- **Stream only (no snapshot).** Timely but lossy, and loses exactly when it matters most — a control-plane
  restart mid-rollout, which is the failure this ADR exists to close.

## Follow-up / known limitations

This is a correct, in-force fix for firmware and model install outcomes, but it is
deliberately scoped. Three follow-ups are tracked as separate work rather than folded
in here:

1. **Point-solution, not a general pattern.** The acknowledgement-vs-outcome gap this
   ADR names is general to *any* long-running command, but the fix is specific to
   firmware/model install (bespoke terminal enum on `UpdateProgress`, bespoke snapshot
   fields). A general async-operation-outcome mechanism is proposed in **ADR-0035**;
   if adopted it supersedes the carriers introduced here.
2. **The reachability gate is not yet closed.** The Status/Required-Events check still
   verifies that a writer *references* an event constant, not that it is *reachable* —
   the weakness that let `MODEL_UPDATE_SUCCEEDED`/`_FAILED` ship marked `emitted` while
   unreachable. Making the gate reachability-aware is proposed in **ADR-0036**.
3. **Stream↔snapshot reconciliation is underspecified.** "Stream for timeliness,
   snapshot for truth" states precedence but not the ordering, idempotency, and
   conflict rules when a streamed `SUCCEEDED` and a later snapshot `FAILED` disagree.
   This must be pinned down (a spec erratum, or as part of the ADR-0035 work).
