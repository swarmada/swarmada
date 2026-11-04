# Changelog

All notable changes to Swarmada are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Changes accumulate under
**Unreleased** until the next release.

Each released section also records the **spec version** it implements and which RFCs
were **added** or **amended**; the version increment follows
[docs/release-process.md](docs/release-process.md#versioning).

## [Unreleased]

## [RFC-0001 v0.4] - 2026-11-04

**Spec version `0.4`.** RFC-0001 v0.4, with its two accepted amendments. Fleet-adapter contract `1.4.0`
(`fleet_adapter.v1`), supported range `>=1.3.0 <1.5.0`. Conformance suite revision `2026.10.22.2`.

- **RFCs amended:** RFC-0001, from v0.3 to v0.4. Its two accepted amendments, Zone Restrictions and
  Zone Exclusions, are published with it and are part of spec version `0.4`.
- **RFCs added:** none to the spec version. RFC-0003 and RFC-0004 are published as drafts for review
  and are not part of spec version `0.4`.
- **API:** 21 CRDs under `swarmada.io/v1`, from 13 at v0.3. RFC-0001 adds `EdgeNode`,
  `ComputeEndpoint`, `ComputeWorkloadTemplate`, `ResultSink` and `ArtifactStore`. `TaskTrigger`,
  `SemanticEvent` and `SemanticEventSource` are RFC-0003's and outside the contract.
- **Protocol:** contract `1.0.0` to `1.4.0`, in four minor steps, each in its own entry below.
- **Versions:** `pyproject.toml` and the chart's `appVersion` are `0.4.0`. The chart's default image,
  `ghcr.io/swarmada/swarmada:0.4.0`, is not published; build the manager image from source.
  `make docker-build` stamps the chart's `appVersion` into the manager, so its audit entries'
  `swarmada_version` and the `swarmada_version` metric read `0.4.0`.
- **The contract is frozen at RFC-0001 v1.0.** See *The contract is frozen at RFC-0001 v1.0, and the
  text says so*, below.

This is the first versioned section. It holds every entry recorded before it, including those
published with v0.3 (draft 3), which had no section of its own.

### Edge shutdown, the committed Python stubs, and a live quickstart in CI

- **`cmd/edge` no longer hangs at shutdown after its server exits cleanly.** When the edge server
  returned with no error, the shutdown path waited for that result a second time and never exited.
  It now waits only for a result it has not read.
- **The conformance targets use the committed Python stubs.** `make conformance`,
  `conformance-sim`, `conformance-sim-fault`, `conformance-profiles`, `conformance-endpoint` and
  `stream-lease-measure` no longer run `make proto-py` first. Only `make proto-py` regenerates the
  stubs under `proto/`, after a `.proto` change, and it needs `uv` or the `codegen` extra
  (`pip install '.[codegen]'`).
- **CI runs the quickstart with a live adapter.** `CI=true` now only turns pacing off; it no longer
  selects the status-projected path. In CI, `make quickstart-test` starts the simulation adapter,
  which drives one robot over the mTLS `ControlStream`. `--projected` still selects the
  status-projected path, which still exits 1.

### The conformance harness names a keyed `Custom` verb, and `C2.2` reads no `STOPPED` as an end

- **An assignment for a `Custom` entry keyed by `custom_type` carries that `custom_type`**, as the
  control plane sets it. An adapter that keys its `Custom` verbs was refused every assignment as
  naming no verb. Now `C4.2`, `C3.9` and `C16.1` reach it, and `C3.4`, `C3.5`, `C5.8`, `C17.1`,
  `C17.2` and `C19.1`–`C19.3` grade what they probe (suite revision `2026.10.22.2`).
- **`C2.2` grades the terminal report for the action the `RegisterAck` declares `CANCELLED`.** Any
  state RFC-0001 calls terminal passes, and an adapter that sends only `ACTION_STATE_STOPPED`
  fails.
- **The recorded simulation report the demos pin is re-recorded at suite revision `2026.10.22.2`**,
  with `--latch`, at contract `1.4.0`. Every check has the verdict it had at `2026.10.17`; `C4.2`
  passes on the terminal `ACTION_STATE_FAILED` that follows the commanded `STOPPED`. The two pinned
  digests moved with it, and the registry's `simulation` row is re-earned. The floor-mapping
  example's report is not re-recorded: it needs a run of the ROS 2 adapter.

### The conformance harness reads no `STOPPED` as an end

- **`C4.2` grades the lapsed lease's terminal report.** `ACTION_STATE_STOPPED` is the commanded
  stop and is not terminal, so an adapter that sends only `STOPPED` when its lease lapses fails
  `C4.2`. Any state RFC-0001 calls terminal passes. `C16.1`'s terminal half and `C3.9`'s wait for
  the preemptor's end read the report after a `STOPPED` (suite revision `2026.10.22`).
- **`C3.6` accepts `CANCEL_DISPOSITION_STOP_UNCONFIRMED`**, which contract `1.4.0` defines.
- **The simulation adapter, the adapter template and the no-op example follow a lease self-stop's
  `STOPPED` with a terminal `FAILED`**, with end cause `ACTION_END_CAUSE_LEASE_EXPIRED`, once rest
  is confirmed or cannot be.

### Observation and stopping after an adapter is deleted

- **A robot whose `FleetAdapter` is deleted is still observed and stopped, and gets no new work.**

### Observation and stopping after a class edit

- **A robot whose class leaves its adapter's list is still observed and stopped, and gets no new work.**

### The contract version per connection, a stop's end cause, and stop windows from the record

- **The control plane and the conformance harness negotiate the contract version per connection.** An
  adapter that reports a `1.3.x` version is told `1.3.x`, and is sent no `EstopClear` and no
  `ActionOutcomeAck`. `FleetAdapter.status.negotiatedContractVersion` records the version agreed for
  the connection. The harness gives a `1.3.x` connection no work, and records each row that needs an
  assignment as `contract-predates-obligation` (suite revision `2026.10.17`).
- **`--require-latch-contract`** gives work only to a robot whose adapter's connection negotiated
  `1.4.0` (candidate filter 9, and the version check at the `AssignAction` send). It is off by default.
- **A failure report that carries `ACTION_END_CAUSE_ESTOP` pauses the action**, under a stop or not.
- **A stop ID whose record was removed is remembered.** A later report of it, on a `1.4.0` connection,
  is answered with the stop's fence and its `EstopClear`.
- **A stop's delivery and confirmation windows run from its send and its first acknowledgement.** A new
  leader runs out the windows its predecessor recorded, and sends the recorded stop again under its
  ID, rather than issuing a fresh stop with fresh windows.

### The leader term on a connection below `1.4.0`

- **The adapter template and the simulation adapter (`--latch`) no longer reject a `Command` for its
  leader term on a connection that negotiated below `1.4.0`.**

### The stop clear, motion withheld under a stop, and the lease after a clear

- **A clear is written before it is sent.** Row S5 is one `Robot` status write with a
  `resourceVersion` precondition: `Resuming`, and `clearedAt` on each stop the clear ends. Only once
  it has landed does the control plane send an `EstopClear` naming each of them, with the leader
  term.
- **A clear does not move a `Stopping` robot.** A clear that reads a stop still in force, or a stop
  recorded since its read, writes nothing and sends nothing.
- **A cleared stop's record leaves on the adapter's `EstopClearAck`.** Each `SafetyStream` that
  opens while an entry awaits its acknowledgement is sent its `EstopClear` again. On an adapter that
  reported a `1.3.x` contract, the clear removes the entry and sends no `EstopClear`.
- **Every `estopState` write carries a `resourceVersion` precondition.** A stop's outcome is retried
  on a conflict. The operator release (S6) reads the stops in force again at its write.
- **No zone admission and no reservation grant reach a robot a stop holds.** Each is kept and sent
  once the robot is released, if its zone hold or its queue promotion still stands.
- **The simulation adapter drops the work a `SafetyStream` stop interrupts,** and answers an
  `EstopClear` with its `EstopClearAck`.
- **After a clear, held work is not renewed.** An `Assigned` or `InProgress` action whose stop was
  cleared before it was paused gets no `RenewActionLease`, and the condition `RenewalHeld`. A
  `Paused` action's lease advances only while its robot has confirmed its stop, is connected and is
  within its horizon.
- **A robot whose register was processed is not left unready by a failed read.** Its fences go ahead
  of its next `AssignAction` on the same stream; a fence that cannot be sent is a `Warning` Event on
  the `Robot`.

### Drafts published for review

- **Two drafts are published for review beside RFC-0001: RFC-0003 and RFC-0004.** RFC numbers 0002,
  0005 and 0007 are unpublished drafts, and 0006 is superseded by RFC-0001; none is part of this
  release. RFC-0001's references, non-goals, FleetTask chapter and overview no longer cite them.
  Until RFC-0001 v1.0 a change to the contract is a new contract version, specified by an amendment
  or a new RFC.

### Rollout resume, registry credentials, audit attribution and RBAC scope

- **`swarmctl rollout resume` takes effect on installs that use the shipped RBAC.** The manager can now
  record that it applied a resume, and an update that changes no `spec` field is no longer refused by the
  rollout signing check. One `ROLLOUT_RESUMED` entry is written per resume.
- **ACTION REQUIRED — registry credentials name their hosts.** The control plane presents the credentials in
  `swarmada-registry-credentials` only to the hosts listed in its new `registries` key, and only over HTTPS
  (a loopback development registry excepted). Each entry is a `host[:port]`, matched exactly apart from
  case: a subdomain or another port is a different host, and an entry written as a URL matches no host and
  raises a `Warning` Event. A redirect to a host the key does not list is followed without the credentials,
  and a registry whose token server is on another host needs both hosts listed. Without the key, fetches
  are anonymous and a `Warning` Event says so. Add `registries` to the Secret before upgrading if your
  firmware signatures or watched model registries need credentials.
- **`SWARMADA_CONFIG_MODIFIED` is recorded once the change is stored, and names the controller.** It is no
  longer written by the admission webhook, so a dry run, or a change another check refuses, records nothing.
  The Kubernetes API server's audit log records who made the change.
- **Only the control plane, or a holder of `estop-clear`, removes a stop's finalizer from a zone or a
  `SwarmadaConfig`** while the stop holds, except in a namespace being deleted.
- **The event bridge example accepts each signed request at most once within its timestamp window.**
- **ACTION REQUIRED — `swarmada:admin` writes only Swarmada's own CRDs.** Its CRD write is limited, by name,
  to the Swarmada CRDs, and it no longer creates CRDs. Install and upgrade CRDs with the identity that
  installs the chart.
- **A `FleetTask` acts only on the actions it created.** An existing `FleetAction` that has the name a member
  would get is reported as a conflict on the task and left unchanged. Only the control plane sets or changes
  a `FleetAction`'s owner references.
- **A `FleetTask`'s child actions are written only by the control plane.** A change to the `spec` of a
  `FleetAction` that a `FleetTask` controls is refused from any other identity, except a `desiredState`
  change or a requeue request by a holder of `cancel`. Only the control plane or the garbage collector
  removes the `FleetTask` owner reference. **Breaking** for a client that edits a child action directly:
  change or cancel the `FleetTask` instead.
- **A child action's `swarmada.io/` labels and annotations are written only by the control plane.** Adding,
  changing or removing a label or annotation under `swarmada.io/` on a `FleetAction` that a `FleetTask`
  controls is refused from any other identity, except a cancel or requeue request added by a holder of
  `cancel`. Other labels and annotations stay open. The garbage collector deletes a deleted task's members
  under the same identities it orphans them with. **Breaking** for a client that relabels a child action:
  change or cancel the `FleetTask` instead.

### A stop is sent first, recorded after, and fenced on the ControlStream

Control plane, CRD and proto. The `Robot` CRD gains `status.stops`; `fleet_adapter.v1` gains the
stop latch's `SafetyStream` messages. Contract stays `1.4.0`.

- **The proto carries the stop latch.** `ControlPlaneSafetyMessage` gains `EstopClear` (arm 5), and
  `AdapterSafetyMessage` gains `EstopClearAck` (arm 5) and `LocalStopReport` (arm 6).
  `LocalStopReport.unreadable` (field 5) says the adapter could not read the robot's local latch.
  `Command.estop` is the stop's fence, no longer marked deprecated.
- **A stop's ID is random and kept.** The `estop_id` is a version 4 UUID minted once when the stop is
  issued. A stop sent again down the stream that replaced its own keeps its ID, and an
  acknowledgement with that ID on the replaced stream confirms nothing.
- **`Robot.status.stops` records each stop sent.** After the stop is sent, the control plane appends
  its ID, scope, carrier and `sentAt`, and later its `acknowledgedAt`, retrying until the write lands.
  The record gates nothing. At 256 entries a stop is still sent, and a `Warning` Event `StopRecordFull`
  names the robot. Only the control plane's ServiceAccount may change the field.
- **A zone's or the namespace's stop reaches a stopped robot too.** A robot already under another stop
  is sent the carrier's stop as its own, once: the carrier's later passes key on the record.
- **The fence goes down the ControlStream.** Each stop's `Estop` is sent as `Command.estop` in order
  with the robot's `AssignAction`s. An `AssignAction` goes only to a robot that no stop in force holds,
  and only once its register has been processed on that stream, after the fences of its stops.
- **An action bound for a stopped zone waits.** A `Pending` action whose `spec.zone` is under a stop is
  not dispatched. It carries the condition `ZoneStopped`, and a `Warning` Event `ZoneStopped` names the
  zone.

### Work under an unconfirmed stop waits for an operator; preemption takes only running work

Control-plane code and the chart. No CRD or proto change.

- **Work a robot never confirmed stopping on stays with it.** Once the robot's lease has expired, its
  work waits for an operator, started or not, with a `Warning` Event `ActionAwaitingOperatorDecision`
  (RFC-0001 invariant I1). Previously work the robot had not reported running returned to `Pending`.
  The `ActionMovedFromUnconfirmedStop` Event is retired.
- **Preemption never displaces a `Paused` action** (row A11). A pending `Critical` or `High` action
  takes a victim only from `Assigned` or `InProgress`, on a robot under no stop. That holds on the robot
  path and on the zone-capacity path, where the traffic deconfliction engine evicted a reservation
  whatever its action's phase.
- **The manager is replaced, not rolled.** The chart and `config/manager` set the Deployment strategy
  to `Recreate`, so an upgrade never runs a second manager beside the leader. `make helm-verify-sync`
  checks both.
- **A register whose robot lookup fails is refused `NOT_ADMITTED`.** It was refused with no rejection
  code, which tells an adapter that the robot is not its own. The adapter now retries.
- **The control plane reads `stale_leader_term`.** A `Command` rejected for its leader term is read as
  not acted on, never as a refusal, and is logged.
- **A retained outcome needs the API reader.** The manager refuses to start without one, and an
  ingestor without one acknowledges no retained outcome.
- **A refused report from another run goes on the robot.** The `ActionReportRefused` Event for a
  fencing-token mismatch is raised on the bound `Robot`, once the stream is shown to serve it. It is no
  longer raised on the action.
- **A process that no longer leads records no stop outcome.** It still sends a stop, but writes no
  `estopState`, metric or audit entry for the answer.

### Work under a stop stays with its robot; a re-sent stop is metered and audited

Control-plane code only. No CRD, proto or adapter change. The `internal/webhook` tests now run in
their own `go test` with a 15-minute timeout inside `make test-go`; the other packages keep 10 minutes.

- **An `Assigned` action keeps its robot under a stop.** A stop pauses it as it pauses an
  `InProgress` action: the binding, the zone slot, the lease and the robot's record are kept until a
  row out of `Paused` releases them (RFC-0001 row A1). Previously a confirmed stop released them at
  once, and an unconfirmed one released them once the lease had expired.
- **`Preempted` work stays held under a stop, started or not** (row A12), with a `Warning` Event
  `ActionAwaitingOperatorDecision`.
- **A `Failed` (`StopUnconfirmed`) action is not released on a dead lease while a stop holds its
  robot** (rows A3 and A6). A requeue asked on it under the stop runs on row A2's proof. A cancel asked
  on it runs once the lease is provably dead.
- **A robot keeps the stop finalizer while any stop holds it:** its own, a zone's or the namespace's,
  issued or recorded, and while it is `Resuming`.
- **A stop sent again on a new `SafetyStream` is measured, metered and audited** like a first send:
  `swarmada_estop_command_latency_seconds`, `swarmada_estop_commands_total`, and over 500 ms
  `swarmada_estop_latency_violations_total` and an `EstopLatencyViolation` Event; and an `ESTOP_TRIGGERED` entry with the robot-scope timing fields and `resent`
  (`"true"`), outcome `Error` when the stop was not confirmed.
- **A running report refused because the action has left `Assigned`** (`Paused`, `Preempted`,
  `Revoking`, or a bound `Failed` with `StopUnconfirmed`) records `startedAt` and moves no phase.
- **Not met at v0.4.** A cancel of work a stop holds needs only `cancel` on the action, not
  `estop-clear` on the stop (row A4); the cancel of a `Failed` (`StopUnconfirmed`) action above runs on
  the `cancel` check alone. The windows of a stop's outcome are still kept in memory.

### The leader serves the adapter streams, stamps its term, and reads a retained outcome

Control-plane code only. No CRD, proto or adapter change.

- **Only the leader serves adapter streams.** A manager replica that does not hold leadership
  refuses a `ControlStream` or `SafetyStream` with `Unavailable` ("not the leader"), and a replica
  closes its open streams with `Unavailable` ("leadership lost") when it loses leadership. An
  adapter retries on `Unavailable`. One replica is still the only supported configuration with
  `controlStream.enabled`.
- **Every `Command` carries the leader term.** The term is taken on each acquisition of leadership
  as the larger of the election Lease's transition count and the highest term issued, plus one. The
  highest term issued is kept in the `swarmada-leader-term` Lease, annotation
  `swarmada.io/leader-term-floor`, in the manager's namespace (`POD_NAMESPACE`), so the term does
  not fall when the election Lease is deleted. A replica that does not lead sends no `Command`.
  No RBAC change: the manager already manages Leases.
- **A retained outcome is read and acknowledged.** An `ActionStatusUpdate` with `retained = true`
  is read as rows A13 and A25 state: a `SUCCEEDED` ends the work, under a stop or not; a `FAILED`
  under a stop, or with `ACTION_END_CAUSE_ESTOP`, pauses live work and leaves held work as it is;
  any other `FAILED` fails the work. The control plane answers with `ActionOutcomeAck` once it has
  recorded or decided the outcome. It does not acknowledge an outcome whose report token does not
  match the action's assignment, so the adapter reports it again on its next reconnect.
- **Not met at v0.4:** no in-tree adapter retains an outcome or reads the term.

### I1: after a stop, an operator moves the work

Specification text only: RFC-0001 and the conformance catalogs.

- **I1, work stays with a stopped robot.** Work assigned to a robot under a stop never moves to
  another robot automatically. It leaves only when the robot itself reports the work ended, or
  when an operator decides to move it: the requeue by a holder of `estop-clear` (row A2), carried
  out on that decision once no earlier `AssignAction` can still start the work (the robot's stop is confirmed,
  or the lease horizon passed), or an operator's cancel (rows A4, A10, A21).
- **The closed proof list is deleted.** Under a stop no proof moves work: not a dead lease, a cancel
  acknowledgement, a refusal or a failure report. Row A5 (unstarted work of a robot that never
  confirmed its stop, moved at the lease horizon) is deleted, and rows A3, A12 and A24 no longer
  move work off a robot under a stop. Under no stop, each row that moves work names its own proof
  (the robot's terminal report, a bound cancel acknowledgement, a bound refusal, or the lease
  horizon); I2, I4, I5 and I6 say so.
- **Not met at v0.4:** the reference control plane still moves work off a robot under a stop on a
  cancel acknowledgement, a refusal or a passed lease horizon.

### I7, one end state for abnormal events, and an adapter starts with no work

Specification text only: RFC-0001 and the conformance catalogs. No CRD field, proto field, Go or
Python file changes.

- **I7, fail safe on uncertainty.** A seventh safety invariant: no state change the control plane
  cannot confirm releases a robot, resumes work or moves work. What it cannot confirm is held, and
  only the operator release lets it go. A conforming Fleet Adapter keeps the parts of I6 and I7
  that are its own.
- **One end state for abnormal events; `spec.adapter` is immutable.** An adapter that registers
  a robot again (it restarted, or reconnected after losing contact), a report that the robot's local
  latch is set or unreadable, and a reported stop the control plane holds no record of each record
  a fresh **end-state stop**: an `EndState` entry of `Robot.status.stops`, with a fresh stop ID. It
  holds the robot whatever its `estopState`, the robot resolves `Failed`, and its work is paused and
  held. No clear, re-value or newest-outcome rule ends it: only an operator's **attestation** of the
  check at the robot does, the new `swarmada.io/estop-attestation` annotation, written by a person
  holding `estop-clear` and naming the robot's latest end-state stop. The operator release then
  returns the robot to work (row A13). A new `ControlStream` carries no assignment for a robot
  until its register on that stream is processed and the fences are sent there. A register from
  another adapter is refused `accepted: false` with no code, and that adapter stops any work it
  holds for the robot. A change of `spec.adapter` is refused: delete the robot while `Idle` and
  under no stop, and admit it again.
- **An adapter starts with no work.** Every adapter, whatever its contract version, never
  continues work a stop or an edge stop interrupted, and a process that starts after a restart
  continues nothing. Every adapter with an `EdgeStream` sets the local latch, and the latch
  survives a reboot or power loss of the robot.
- **Row A25: an adapter never executes an assignment twice.** The adapter keeps the report token
  of each assignment it accepted across its own restart, and deduplicates by it.
- **Not met at v0.4.** The `Robot` webhook admits a change of `spec.adapter`, and a robot that
  registers again is not stopped. The reference records no end-state stop, reads no attestation, and
  guards neither release marker. No in-tree adapter drops the work it held before a restart, and the simulation adapter and the adapter
  template command the motion again on an identical re-delivery.

### An edge stop latches at the robot and is cleared only there

Specification text only: RFC-0001 and the conformance catalogs. No CRD field, proto field, Go or
Python file changes.

- **An edge stop is not a control-plane stop.** A stop the adapter takes without the control plane,
  an `Estop` from the Zone Controller edge node or a stop input at the robot, sets the robot's
  **local latch**. The latch is held at the robot, so it binds every adapter that drives the robot
  and survives a reconnect, an adapter restart and a change of
  leader. It is lifted only by a **local reset** made by a person at the robot. No `EstopClear`,
  control-plane stop or clear, edge-node message or timer lifts it, and a reset moves nothing.
- **The control plane is told, and withholds the robot.** The adapter reports the local latch in a
  new `LocalStopReport` on the robot's `SafetyStream`. A report that it is set or unreadable, naming
  the set by the report's new `set_id`, records the robot's end-state stop (the entry above), and the
  adapter refuses every assignment until the `EstopClear` that ends that set (its new
  `ends_local_set`) takes effect. The control plane never records an edge stop in
  `status.stops`, gives it no stop ID and never clears it. An action an edge stop interrupted is never continued: after the reset the robot
  moves only on new work. The rules that recorded an edge stop as a
  reported stop and ended it with a later robot-scope clear are gone. A reported stop is now only a
  control-plane stop a predecessor sent and never recorded, looked up per robot.
- **A stop's record is removed only on the adapter's confirmation.** The adapter answers the
  clear that lifts a stop ID with a new `EstopClearAck`. An accepted assignment no longer removes
  an entry. Row S5's `EstopClear` names every stop that has ended and has not been cleared.
- **Smaller rules.** A stop and its acknowledgements travel on the `SafetyStream`, and its fence on
  the `ControlStream`. A fence that arrives on a replaced `ControlStream` still stops the robot but
  does not count toward a clear. No `RegisterAck` value resumes work while any latch is set.
  `ESTOP_REPORTED` and `EDGE_STOP_REPORTED` are required audit events, and
  `ESTOP_TRIGGERED` and `ESTOP_CLEARED` carry stop IDs.
- **Not met at v0.4.** The proto has no `EstopClearAck` or `LocalStopReport` arm, no in-tree
  adapter keeps a local latch, and the reference control plane writes neither audit event.

### A stop is named by its stop ID and fenced on the `ControlStream`

Specification text only: RFC-0001 and the conformance catalogs. No CRD field, proto field, Go or
Python file changes.

- **RFC-0001 drops the stop generation.** `Robot.status.stopGeneration`, the `stop_generation`
  fields on the safety envelope and on `AssignAction`, and the admission rule that refused a lower
  value are gone from the text. Each stop is named by its **stop ID**, the `estop_id` of its
  `Estop`, minted once and carried by every copy and every re-send.
- **A stop goes out on two paths at once, and is recorded after.** The control plane sends the
  `Estop` on the robot's `SafetyStream` and the same `Estop`, in order with the assignments, on its
  `ControlStream`: the stop's **fence**. No write, webhook or lock is on either send. The stop is
  then recorded in `Robot.status.stops`, one entry per stop.
- **The adapter's latch is per stop ID.** A stop latches from whichever path brings it. While a stop is latched the adapter refuses every assignment for the robot,
  except a re-delivery of the action the robot holds, begun or not. An `EstopClear` lifts a stop only after
  that stop's fence has arrived, and only a fence on the adapter's current `ControlStream` counts.
  An adapter reports the stop IDs it holds latched; the control plane holds one it has no record
  of in force until the attestation that ends the end-state stop recorded on its receipt. An edge stop is no
  longer one of them (the entry above).
- **Dispatch needs contract `1.4.0`.** A robot is given work only over a connection that
  negotiated `1.4.0`. A `1.3.x` adapter still registers, reports and is stopped.
- **Not met at v0.4.** The reference control plane dispatches to any connection inside the supported
  range. The conformance catalogs disclose the checks that grade an adapter that meets the latch as
  failing: `C3.1`, `C3.8`, `C5.4`, `C17.1`, `C17.2` and `C17.4`.

### A robot that reconnects under a stop is sent its stop again

A controller change: no CRD field, proto field or admission rule changes.

- **The stop is sent again when the adapter's `SafetyStream` opens.** Each robot of that adapter whose
  `estopState` is `Stopping`, `Stopped` or `Failed` is sent its stop once down the new stream, unless a stop
  still awaiting its acknowledgement has been moved onto that stream. A `Resuming` robot is not sent its
  cleared stop. The re-send writes no `estopState`: its answer is recorded as an Event on the `Robot`,
  `Normal` `EstopResent` when the adapter answers `STOPPED`, and `Warning` `EstopResendUnconfirmed` for
  any other answer or none, after which `estopState` stays what it was and the robot stays withheld from
  dispatch. Previously the stop was not sent again on a reconnect. RFC-0001 row A13 of the action
  lifecycle transition table now states a wider rule, which this change does not meet: a stop whose first
  acknowledgement has not arrived is sent again too, the answer to a re-sent stop is written to
  `estopState` like a first answer, and a new leader sends again every stop with no confirmed outcome,
  one whose `Estop` its predecessor never sent included.

### A robot's terminal report that loses a write conflict is decided again, not dropped

A controller change: no CRD field, proto field or admission rule changes.

- **A terminal action report is no longer dropped on a write conflict.** When the status write for a report lost to a
  newer write of the action, the report was dropped and an action the robot had finished stayed `InProgress`. It is now
  decided again from a fresh read, with the fencing, stop and supersession checks repeated. A report that is still not
  applied is logged and, when it would have ended the action, recorded as a `Warning` Event `ActionReportNotApplied`.
  RFC-0001's Fleet Adapter protocol chapter names the Event.

### The robot delete refusal names what releases the record; descriptions match the code

Text and messages only: one admission refusal's wording, one CLI line, CRD descriptions, RFC-0001 prose and
documentation. No CRD field or validation rule, no proto field, and no admission or controller decision changes.

- **The robot delete refusal promises no moving time.** For a robot running an action bound to it, the refusal
  says to cancel or requeue the action first; the lease is renewed while the robot is reachable, so the time it
  named moved forward at every attempt. It no longer says a cancel was sent when none was. RFC-0001's admission
  table, safety chapter and `Robot` chapter, and `docs/operations.md`, name the refusal.
- **`kubectl explain` says what the code does.** `status.pendingSince` is described as an input to the wait before
  work moves, `status.leaseExpiresAt` without the clock-skew margin it does not include,
  `status.actions[].attempt` as unwritten, and a `FleetTask`'s writes to its children's status as the one field
  it writes. `spec.parentZone` says a cycle or the depth bound is judged per edit.
- **A stop keeps the robot's record, not always its binding.** RFC-0001 and the known limitations say an action
  paused while `Assigned` is unbound once the stop is confirmed, while the robot keeps its record of it.
- **`swarmctl cancel task` on a task that has ended** says the controller cancels none of its members, instead of
  promising a cancel.

### Fencing tokens never repeat, a report without one is refused, and an issued stop holds work at once

Controller decisions, with the envtest rows that pin them. RFC-0001 rewrites its paragraphs on the
generation store and on reports without a fencing token. No CRD field or proto field changes; one
Event reason is new.

- **Every assignment generation in a namespace comes from one stored counter.** The control plane keeps
  the namespace's highest generation in a `coordination.k8s.io` Lease, `fencing-tokens.<namespace>` in
  its own namespace, where the namespace's users cannot write, and reserves the next one before it
  commits an assignment. A store read below a generation already issued is refused and raised
  (`Warning` Event `GenerationStoreBehind`); no generation above 2^62 is issued (`GenerationCeilingReached`). The fencing tokens a robot is given now rise
  across all its actions, and an action deleted and created again under its name never repeats a token
  the earlier one used. Before, each action counted from 1, so a late report from an earlier run could
  end the new one on another robot, and two actions given to one robot carried the same token. Tokens
  are now large numbers.
- **A report without a fencing token is refused, as a stale one is.** The report names no robot, so
  nothing else ties it to the run it came from. Every refused terminal report, and the first refused running report on
  an adapter connection, raises a `Warning` Event `AdapterOmitsFencingToken` on the `FleetAdapter`; every
  one is logged. **Breaking** for
  an adapter that does not echo the token, which conformance check C6.4 already requires: its reports no
  longer move work, and its actions end by lease or completion timeout.
- **A stop issued over a robot holds its work before the stop is written onto the robot.** A requeue
  (an operator's, a `ZoneMaintenance` window's or a capability loss's) and a completion timeout no
  longer move work off a robot that a stop covers before the robot's record names the stop, and the robot
  and its work hold the `swarmada.io/estop-in-force` finalizer from the stop's issue.
- **A requeue asked before a stop the robot never confirmed stays held.** Started work under such a stop
  is not moved by it once the lease expires, and no other hold rewrites the record of when the request
  was asked.

### Held work survives every delete, and only the stop's own authority decides where it goes

Admission and controller decisions, with the envtest rows and generated sequences that pin them. RFC-0001
adds one paragraph to the emergency-stop chapter. No CRD field, annotation, finalizer name or proto
field changes; one webhook registration and one Event reason are new.

- **Deleting held work needs `estop-clear`.** While a stop covers the robot that records an action,
  deleting the action is refused unless the requester holds `estop-clear` on that stop. The refusal
  names who decides and the commands. A new delete check, `vfleetactiondelete.swarmada.io`, has
  `failurePolicy: Ignore`.
- **Held work keeps the stop finalizer.** The action and its robot hold `swarmada.io/estop-in-force`
  while the work is held, so a delete admitted to `estop-clear`, or admitted while no manager replica
  served the check, leaves the object `Terminating` with its record until the stop no longer holds the
  robot. Removing that finalizer by hand needs `estop-clear` too.
- **Held work names its arbiter.** Work held without proof carries a `Warning` Event
  `ActionAwaitingOperatorDecision` and a message that starts "awaiting operator decision (estop-clear
  on <stop>)", with the requeue, cancel and resume commands.

### A failure under a stop is the stop's outcome; work of a robot that never confirmed its stop moves

Controller and admission decisions, with the envtest rows and generated sequences that pin them. RFC-0001
amends the FleetAction state table and the estop table for the last three items. No CRD field,
annotation, finalizer name or proto field changes; the Event reasons and status messages below are new.

- **A failure a robot reports while a stop covers it pauses the action.** The adapter's report is the
  stop's outcome, not a failure: the action is `Paused` with its robot, its message quotes the report,
  and a `Warning` Event `ActionFailedUnderEstop` goes on the action and its `FleetTask`. Before, the
  default `onFailure: Requeue` ran the action again on another robot while the stop stood, and a
  `FailFast` task failed.
- **No FleetAction leaves a terminal phase.** Every status write of the controller carries the
  resourceVersion it read, so a write computed from a stale copy fails and is read again. Before, the
  estop pause could write `Paused` over a `Failed` the adapter had reported a moment before, and a requeue
  `Pending` over a `Succeeded`. A `FleetTask` whose failure policy could not cancel a member stays
  non-terminal and tries again, instead of ending with the member still running.
- **Deleting a recorded action no longer frees its robot's delete.** A robot that records an action
  that was deleted is not deleted until its record is released. The control plane now releases that
  record itself, when the adapter acknowledges the `cancel_action` it is sent, or once the longest
  lease plus the clock-skew margin has passed; a robot whose running action was deleted is no longer
  stranded.
- **A create that carries `swarmada.io/requeue-requested` is checked like the update that adds it.**
  Deleting work a stop paused and creating it again with the annotation needs `estop-clear` on the stop.
- **A requeue asked before a stop waits for the operator release.** A `Warning` Event
  `RequeueHeldByEstop` says so.
- **Work of a robot that never confirmed its stop waits for an operator once its lease has expired.**
  Work the robot had started stays with the robot, which may hold its payload, and a `Warning` Event
  `ActionAwaitingOperatorDecision` asks an operator to inspect it, then requeue, cancel, or clear the
  stop and resume the robot. The robot stays under its stop until an operator clears it. A confirmed
  stop still keeps the work with its robot.

### A stop keeps the work it paused; a robot that may still run an action is not deleted

Admission and controller decisions, with the envtest rows and generated sequences that pin them. No
CRD field, annotation, finalizer name or proto field changes.

- **A robot that records an action is not deleted while it may still run it.** The Robot delete
  check and the last-finalizer rule refuse, naming the action, until the robot's adapter acknowledges
  the cancel or the action's lease is provably dead. A deleted robot had taken the only record of the
  action with it, and a requeue, or the action created again under its name, went to a second robot.
- **Work a stop paused stays tied to the stop.** While a stop covers a robot, the robot keeps its
  record of the action through the stop's pause of an `Assigned` action, a cancel and a member's
  deletion, so the requeue check names the stop and no other robot is given the work, or a member
  created again, until the stop is cleared or a holder of `estop-clear` requeues it.
- **A confirmed stop no longer frees work.** A requeue or a cancel of a `Paused` action waits for the
  adapter to acknowledge the cancel, or for the lease to be provably dead, even when the robot reads
  `Stopped`; an emergency stop on an `Assigned` action no longer releases the robot's record of it. A
  confirmed stop says the robot is at rest, not that it has dropped the action. An adapter that stays
  connected and never answers delays the requeue or the cancel by up to one lease plus the clock-skew
  margin, or, for an action paused while `Assigned`, its requeue by `300` seconds plus that margin from the requeue; such an
  action's cancel ends it at once, and the robot's record of it waits as long from the cancel.
- **A zone stop over a tree past the depth bound stops more robots, never fewer.** A robot whose zone
  chain cannot be walked is stopped and named in an `EstopScopeUndecided` Warning Event; the stop
  reaches every other robot and is applied. Before, the whole pass failed and the stop reached no
  robot. With no zone stop standing, an edit that returns such a tree within the bound is admitted.

### Operator text names what the guards refuse and who may move work a stop paused

RFC-0001 text, documentation, one admission refusal's next step, the manager's startup message and the test that
pins the guard's text. No CRD field or validation rule, no proto field, and no admission or controller decision
changes.

- **The `robots/status` guard is described in full.** RFC-0001's admission table and safety chapter and
  `docs/operations.md` name the two refusals they left out: a change of `status.assignedAction`, and a change of
  `status.phase` while the robot is `Assigned` or `InProgress`, from any principal but the control plane.
- **Work an emergency stop paused.** `docs/operations.md` and the known limitations say that while a stop covers
  a robot the action is bound to or recorded on, `swarmada.io/requeue-requested` needs `estop-clear` on that
  stop, and that an operator requeues the action once the robot is released. `swarmada:operator` is described as a role for shift
  supervisors, as the RBAC manifests and the chart describe it.
- **No admission column is promised.** `docs/operations.md` says what exists: the `Robot`'s age, which
  `swarmctl admit robot --force` does not reset.
- **RFC-0001 says what the code does.** A `desiredState` written onto a running flow task reaches every child
  that exists and is not terminal; a robot under an applied stop it does not record is held until the stop reaches it; a create
  or re-parent that gives a zone more than 64 ancestors is refused, though two edits admitted together can
  exceed it; an `estopHeldBy` entry is removed in each of the ways the known limitations name.
- **Smaller corrections.** Setting `spec.desiredState: Paused` is described as sending `cancel_action`;
  `--yes` is listed for `swarmctl estop resume`; the chart names six built-in roles; a `SwarmadaConfig` delete
  refused while only the processed marker remains says the clear is being applied.

### RFC-0001 closes the automated-clear exception and defines the draft names; operator text says what exists

RFC-0001 text, documentation, ADRs, doc comments (and so the CRD descriptions), help text, comments,
three admission refusals and the messages that name a stop whose clear is being applied, with the
tests that pin them. No
CRD field or validation rule, no proto field, and no admission or controller decision changes; the
regenerated CRDs and chart differ in descriptions only.

- **No exception lets automation clear an estop.** RFC-0001's safety and security chapters no longer
  describe a supervised clear that a separate specification could define, or a grant a later document
  could add. A time-bounded or single-use binding of an automation identity to `estop-clear` is a
  binding like any other, and the prohibition is absolute.
- **The names a drafted `FleetTask` carries are defined.** The FleetTask chapter lists the labels
  `swarmada.io/drafted-by`, `swarmada.io/drafted-from` and `swarmada.io/drafted-event` and the
  annotation `swarmada.io/draft-origin`: who writes each, what it holds, and that nothing protects or
  restores it after creation. Nothing about how they are written changes.
- **RFC-0001 relies on no unpublished document.** The architecture chapter no longer says later
  specifications add CRDs or fields; it names no draft as a source of CRDs or fields;
  `BarrierSatisfied` is no longer called specified; the `maxPendingActionsPerZone` schema
  comment says what the cap does: it counts `Pending` actions only, fails open, and exempts an action
  that names no zone.
- **Later-work wording is withdrawn across the operator-facing text.** RFC-0001, the ADRs, `docs/`,
  the chart, CRD descriptions, `swarmctl` help and this file say what exists. Rollout undo and stop are
  not supported; the `docs/operations.md` rows that named a `swarmctl` command that does not exist give
  the `kubectl` command that works; the adapter contract window is described as moving with the
  control plane's contract version, never with time.
- **`estopHeldBy`: every way an entry is removed.** `kubectl explain robot.status.estopHeldBy`, the
  refusal of a write to it, RFC-0001 and the known limitations name the clear of a stop on a carrier of
  the same kind and name, including a carrier created again under that name, and a robot-scope stop and
  clear when the carrier is gone. A unit test pins the refusal.
- **A release asked for while a clear is being applied says to retry.** `swarmctl estop resume`, and
  the control plane's own refusal, name a zone or namespace stop whose clear is being applied as such
  and say to retry the release once it is applied, instead of an empty trigger value and a second
  `estop-clear`.
- **Refusals say what the check does.** A `propagateToChildren` change on a zone below a pending stop
  is refused without the claim that it changes the stop's reach, and a zone delete with only the
  processed marker left says the clear is being applied. RFC-0001 states the over-deep `parentZone`
  refusal as it is printed.
- **Known limitations.** A flow member with the default `onFailure` has its own entry; a stop cleared
  while it is being issued is described as it behaves; the robot and `SwarmadaConfig` delete checks are
  both `failurePolicy: Ignore`; the no-webhook lists name `activate`; a supersession cancel answered
  `CANCEL_DISPOSITION_STOP_UNCONFIRMED` is disclosed as specified and not built.

### A stop covers a robot by one rule, and an action is redispatched by one proof

Two guards replace the readers that each answered "which stops cover this robot" and "may this
action be given to a robot" on their own. The stop guard answers from the API server, over the
robot's recorded and declared zone with up to 64 ancestors each, its own trigger, its records and the
namespace; a robot being deleted is a robot; a read error or a deeper chain holds or refuses, never
reads as "not covered". Every path that moves a robot, changes its placement or edits the zone tree
goes through it: the fan-out and the late-arrival pass (which now also runs on a timer), dispatch, the
rollout holds, the position hold (which reads the robot from the API server and writes with an
optimistic lock), and in admission the robot move, the robot delete, the last-finalizer removal and
one `FleetZone` edit rule that judges every zone below the edited one; a re-parent or a create that
would give a zone more than 64 ancestors is refused naming the depth. A robot that entered an
applied stop and does not record it yet is held where it is until the stop reaches it. The executor
guard gives an action to a robot, and releases a robot's `status.assignedAction`, only once every
robot that records the action is provably off it: its adapter acknowledged a cancel, reported the
action ended, or the lease is provably dead; a horizon the control plane does not know is bounded by
the longest lease the configuration admits plus the skew margin. `spec.desiredState: Paused` on an
`Assigned` or `InProgress` action sends the robot `cancel_action`; `swarmada.io/requeue-requested`
on an action whose robot a stop covers needs `estop-clear` on that stop; `status.assignedAction` is
the control plane's to write; a carrier holds the finalizer `swarmada.io/estop-in-force` while it
carries a stop; a hand-written `swarmada.io/pending-firmware-*` key is refused on a covered robot and
withdrawn from every robot a stop comes to cover; a `FleetTask` with `spec.flow` propagates its
`desiredState` to every existing member. The `swarmada:operator` role's description no longer names
integrations. CRD descriptions and the chart follow; no field, proto or RFC-0001 normative rule
changes beyond the two sentences the record of this change names. See `docs/known-limitations.md`.

### A getting-started page

`docs/getting-started.md` orders the ways to start by what each needs and what it shows: `make
quickstart` (a simulated robot on a local cluster), `make first-run` (a robot admitted through the
admission webhooks over mTLS), and the facts a robot of your own needs (an adapter inside the
contract window, its client certificate, the `Robot` resource). The README's Documentation list
links it. The page names the three Python packages the quickstart's live simulation adapter imports,
`grpcio`, `protobuf` and `pyyaml`; without the first two `make quickstart` fails when the live robot
starts, and without `pyyaml` the `hardware-fault` scenario fails. Text only: no code, manifest or
test changes.

### Operator text says what the code does: later-draft promises withdrawn, first run fixed

Doc comments, message strings, an example script, a Deployment's volumes and documentation. No
CRD field or validation rule, no proto field, number, name or type, and no controller behaviour
changes; the regenerated CRDs and chart differ in descriptions and two validation messages only.

- **`make first-run` passes step 8 again.** It waited for exactly six catalog entries, and the
  simulation adapter lists seven since `Custom explore-floor` has an entry of its own (contract
  `1.4.0`). It now waits for the six action types, `Charge`, `Custom`, `DropOff`, `Navigate`,
  `Patrol` and `PickUp`, whatever the number of `Custom` verbs.
- **The in-cluster simulation adapter saves its maps.** The demo-webhooks overlay and the
  first-run kustomization run it with a read-only root filesystem and no `/tmp`, and
  `Custom explore-floor` crashed it with `No usable temporary directory found`. The overlay mounts
  an `emptyDir` at `/var/lib/sim-adapter/artifacts` and passes it as `--artifact-dir`. The map
  lives as long as the pod; no `ArtifactStore` serves it.
- **No operator-facing text promises a later draft.** `kubectl explain` for
  `FleetAction.status.startTime` (kept, not removed), `FleetAction.spec.customType` (stays
  optional) and `Robot.status.estopState`; the `inputs[]` and `ComputeEndpoint` provision
  validation messages; the Robot delete refusal and the `AutoRemovalHeld` Event (no command
  decommissions one robot; a held robot that never reports again is removed only with its
  namespace); `docs/operations.md`; and the known limitations on the edge node's adapter check and
  the composite-zone chapters. A comment in `fleet_adapter.proto` no longer says where a later
  `TaskEvent` field would go; `ActionStatusUpdate` has none. The full-surface demo no longer says
  the edge node is held for a later release: on a checkout without `cmd/edge` it says `cmd/edge`
  is not present and skips the EdgeStream coverage, and the gate-integrity register gives the
  same reason for that skip. `docs/quickstart.md`, `docs/adapters.md` and
  `scripts/setup-macos.sh` no longer promise a Linux or Windows installer or a Makefile target
  named `adapters`: they say neither exists, and the script's refusal on another system no longer says
  "for now".
- **`kubectl explain robot.status.estopHeldBy` says how an entry ends.** An entry names its carrier
  by kind and name. The clear of a stop on a zone or `SwarmadaConfig` of that name removes it,
  including the clear of a carrier deleted and created again under the same name; an entry whose
  carrier no longer exists, or exists under another UID, is also removed by a robot-scope stop and
  clear. The known limitations say the same.
- **The fleet-adapter window is stated the same everywhere.** `adapters/REGISTRY.md`,
  `docs/adapters.md`, the known limitations and the in-tree adapters' comments say the
  range at contract `1.4.0`: it is `>=1.3.0 <1.5.0`. The refusal an adapter outside
  it receives says what to do: move the adapter to a build that declares a contract version in
  that range. The conformance harness's refusal says the same.
- **`activate` is documented.** `docs/operations.md` names it in the custom-verb list and in the
  `operator` role, and its no-webhook list and the known limitations name the draft gate that
  `make run` prints.
- **The zone and namespace `ESTOP_TRIGGERED` entries carry an `unwritten` detail key**, on the
  fan-out entry and on the entry for robots that came into scope after it: the number of robots
  whose stop state or hold record could not be written, which the fan-out then issues again.
- **A namespace delete and an unfinished rollout.** The known limitations said every unfinished
  rollout holds its namespace. A `ModelRollout` does; a `FirmwareRollout` does not: deleting the
  namespace deletes its `SwarmadaConfig`, the rollout's verification then fails closed and writes
  `Failed`, and the namespace goes with it. The entry and `docs/operations.md` say so.

### A stop issued and not yet applied, and a stop a robot never confirmed, are no longer shed

- **Four more edits are refused while a zone's stop is issued and not yet applied.** From the
  moment a zone carries `swarmada.io/estop-triggered` until it carries
  `swarmada.io/estop-processed`:
  - a change of `spec.estopPolicy.propagateToChildren`, in either direction, on that zone or on a
    zone below it that its stop reaches, is refused: "changing spec.estopPolicy.propagateToChildren
    of zone "anc" from true to false would change which robots the emergency stop of zone "anc"
    reaches, and that stop is issued and not yet applied … Retry once the stop is applied …". The
    fan-out reads only the stopped zone's own flag, so on a zone below it the refusal says the
    change does not alter which robots the stop reaches;
  - the delete of a `Robot` the stop reaches, through its `status.currentZone`, its `spec.zone` or
    an ancestor of either, is refused, and so is the delete of any `Robot` in a namespace whose
    `SwarmadaConfig` carries a stop not yet applied: "robot "rk" in namespace "ns" is under the
    emergency stop of zone "z", which is issued and not yet applied … Retry once the stop is
    applied …". The delete guard keeps `failurePolicy: Ignore`: while no manager replica serves,
    the delete is admitted, as before.
  - the removal of the last finalizer of such a `Robot`, which completes a delete admitted before
    the stop, is refused the same way, to every identity but the control plane.
  - The Zone Controller reads a robot with no `status.currentZone` as being in its `spec.zone`, as
    the fan-out does, so its first position report no longer moves it out of a zone whose stop is
    pending.
  - The zone walk behind these checks reaches a zone and 64 ancestors above it, as far as the
    fan-out does (it stopped at 32), and a chain longer than that is refused or held. The reading
    of which stops hold a robot, behind every resume and every clear, reaches the 64th ancestor too
    (it stopped one short).

  Previously each of these, landing before the stop's fan-out ran, left robots outside the stop for
  good: the fan-out did not find them and nothing recorded the stop on them.
- **A zone that carries a stop is not deleted, by any identity.** The delete of a `FleetZone` that
  carries `swarmada.io/estop-triggered`, or the processed marker that stands until the stop's clear
  is applied, is refused to every identity, `swarmada:admin` included, as the `SwarmadaConfig`
  delete is: "FleetZone "z" in namespace "ns" cannot be deleted while it carries an emergency stop …
  A stop is released only through its clear: clear it first …". A delete is admitted while the
  namespace itself is being deleted. Previously anyone RBAC let delete a zone could delete a stopped
  one and create it again, which ended the stop for every robot that later entered the zone, with no
  clear and no audit entry.
- **A zone stop is not narrowed while it is in force, by any identity.** While a zone carries a stop
  (the trigger, or the processed marker until the clear is applied), an edit that takes zones out of
  its reach, a `spec.parentZone` change out from under it, its own `propagateToChildren` set to
  `false`, or the delete of a zone below it, is refused to every identity: "… would take zones out of
  the reach of the emergency stop of zone "anc", which is in force: a robot that later enters them
  would not be stopped … A stop is released only through its clear …". The edit is admitted once the
  clear has been applied. Previously such an edit was admitted once the stop was applied, and a robot
  that entered the zones it took out was not stopped. The robots the stop had already reached kept it
  either way.
- **A FleetAction created again under the name of a deleted one waits for the old one's lease.**
  When a robot still records an action of that name (deleting a `FleetAction` sends its robot
  nothing, and a `FleetTask` creates a deleted member again under its name), the new action is given
  to no robot until one lease duration plus the clock-skew margin after it was created, by which time
  the old action's lease has provably expired. Its message names the robot. Previously the new action
  could go to a second robot while the first was still running the old one.
- **An action whose robot never confirmed its stop stays bound to that robot until the lease has
  provably expired.** An `Assigned` action paused by a stop its robot has not confirmed (the
  robot's `estopState` is not `Stopped`: the stop is in flight, went unanswered and ended `Failed`,
  or is recorded and not yet written) keeps `status.assignedRobot`, the robot's
  `status.assignedAction`, its zone slot and `status.leaseExpiresAt`, and its lease is not renewed.
  Once the lease has provably expired the binding is released and the action stays `Paused` for an
  operator. A requeue or a cancel before then waits for the adapter to acknowledge the cancel or for
  the same expiry. Previously the pause released the robot at once, and an operator's requeue gave
  the action to a second robot while the first, which never received the stop, could still be
  running it. An `Assigned` action paused by a confirmed stop releases its robot at once, as before.
  RFC-0001's `Assigned` → `Paused` row says so.

### The contract is frozen at RFC-0001 v1.0, and the text says so

The freeze covers the fleet-adapter wire contract version and the CRD API versions published with
RFC-0001 v1.0; it does not mean wire contract `1.0`. Until then, `0.x` revisions keep refining the
contract and keep changes to existing contracts to a minimum. This entry changes text only: no proto
or CRD file, and in Go and Python files only comments and one test's failure message.

- **The policy is stated** in RFC-0001's Resource model, in its overview, in `rfcs/README.md`, in
  `docs/api-principles.md` and in `docs/release-process.md`. Normative text is a sentence with an
  RFC 2119 keyword, a schema block, a state table such a sentence cites, and the RBAC matrix.
  Maturity notes, "at v0.4" paragraphs, Known-gaps cells, examples, citations and errata are
  informative and can still change. Annotation names and labels are inside the contract; Event
  reasons, metrics, CLI flags, printer columns, doc comments and message strings are outside it,
  and a change to one is recorded here.
- **CRD versions follow Kubernetes API rules.** Within `swarmada.io/v1` only backward-compatible
  changes are made. An incompatible schema change introduces `v2`, served alongside `v1` for at
  least one revision, with conversion and a migration note.
- **`stage:` governs rules and semantics until v1.0.** On `alpha` they change only through the
  contract-change process: a new contract version, with release notes and migration notes. A
  change off the wire is a patch bump by default, and a minor bump if it binds adapters: if it
  adds, changes or removes an obligation for adapters. A conformance check that tests an
  obligation already stated moves the suite revision only. The contract version does not change. A
  change that binds adapters for safety also raises the lowest supported contract version to the
  new one. On
  `beta` a resource changes by addition, and any other change needs a deprecation announced at
  least one revision ahead. A `stable` resource does not change before the next major revision.
  Each revision states which resources changed stage. At RFC-0001 v1.0 the thirteen original kinds
  are declared `stable` in `v1`.
- **Safety rules, at every stage.** A change goes through the contract-change process, with the
  conformance suite updated in the same version. A change that weakens a guarantee says so first,
  and every unmet rule keeps its Not-met note.
- **Amendments are a drafting tool.** Every release folds its accepted amendments into the chapters
  and assembles one complete specification, published with a "what changed since the previous
  version" document. The two v0.4 amendments are normative for v0.4 but published as separate
  files, and the next revision folds them into the chapters.
- **Two later-draft promises are withdrawn, and both fields stay as they are.**
  `FleetAction.spec.customType` stays optional when `type` is `Custom`; it is not made required.
  `FleetAction.status.startTime` stays; it is not removed.
- **The fleet-adapter window at this release.** RFC-0001 gives the contract as `1.4.0` and the
  supported range as `>=1.3.0 <1.5.0`. The range is not widened. A later minor moves it, by
  the N and N-1 rule, or to that minor alone if it binds adapters for safety. The three external reference adapters still declare `1.0.0`; each
  moves to a version inside the range in its own repository. ADR-0032 Amendment 1 §A1.2 records it,
  and `docs/architecture.md` and `docs/adapter-use-cases.md` name the new range.
- **RFC-0001 describes contract `1.4.0`.** The `ValidateAction`, `SupportedAction` and
  `AssignAction` schema blocks carry `custom_type`; a new paragraph states one catalog entry per
  Custom verb, how an adapter reads the verb and when it declines, and what a reader that does not
  read `custom_type` does; `FleetAdapter.status.supportedActions` shows a keyed entry. The cancel
  dispositions are four, and the safe-reassignment invariant, the FleetAction state machine, the
  capability-loss outcomes, ZoneMaintenance and the TDE eviction text say that an acknowledgement
  carrying `CANCEL_DISPOSITION_STOP_UNCONFIRMED` does not prove the robot stopped: on a requeue the
  action goes `Failed` and the robot stays bound until the lease is provably dead. The adapter rule
  is stated: send it only when the platform cannot witness rest, and only to a control plane at
  `1.4.0` or later.
- **A zone's stop that is not yet applied keeps its robots.** RFC-0001 describes three things the
  control plane does: the Zone Controller holds a robot's `status.currentZone` under such a stop,
  admission refuses a re-parent or a `spec.zone` move that takes a robot out from under it, and a
  maintenance window leaves a stopped or `Resuming` robot's action to an operator. The other edits
  refused in the same window, a `propagateToChildren` change and a robot delete, are in the entry
  "A stop issued and not yet applied, and a stop a robot never confirmed, are no longer shed".
- **The motion threshold is the adapter's own.** The motion-gated telemetry row no longer names
  `motionThresholdMeters` and `maxIdleIntervalSeconds` as values an adapter receives: the control
  plane sends neither, and `RegisterAck` gains no field for them.
- **Three gaps are disclosed as permanent.** No wire message carries the producer identity
  `FleetAction.status.result.producedBy` records. A `ComputeEndpoint` result declares no value
  domain. A robot in `Resuming` is released by an operator; nothing releases it automatically.
- **The suite does not grade contract `1.4.0`.** RFC-0001's conformance chapter says so: an
  adapter that sends `CANCEL_DISPOSITION_STOP_UNCONFIRMED` fails `C3.6` against the suite,
  and the suite has no check for `custom_type`.
- **What a robot under a stop gets from a rollout** is described in the FirmwareRollout and
  ModelRollout maturity notes: no firmware and no model push while a stop holds it, an
  unbegun firmware install withdrawn, a begun one left to run, and a rollback held until the stop
  clears.
- **The `Revoking` hold is in RFC-0001's state tables.** A `Revoking` action whose robot is under a
  stop or `Resuming` is neither released to `Pending` nor re-adopted, and a requeue request on it
  waits. The estop table has a `Revoking` row, and its `Assigned` row and the state diagram say
  that an action paused by a stop its robot has not confirmed keeps its robot until the lease has
  provably expired.
- **A supersession cancel answered with `CANCEL_DISPOSITION_STOP_UNCONFIRMED` fails the superseded
  action**, with its robot bound until the lease has provably expired, as a requeue does. No
  control-plane path issues that cancel: it is specified and not built.
- **The unified-fate ZoneMaintenance row matches the code.** A maintenance window requeues an
  action once its robot's stop is confirmed; it does not pause it.
- **RFC-0001 cites only the drafts that ship.** RFC-0003 and RFC-0004 are published beside it for
  review, each with a header saying it must not be implemented. RFC-0001 and its overview cite no
  other draft.

### CRD validation the specification already required: five admission tightenings

Each change refuses a write that was admitted before. Each one makes a sentence the specification
already stated true at admission, or closes a way around a refusal it already made. Nothing is
relaxed.

- **A `ZoneController` must list a zone.** A CEL rule on `EdgeNode.spec` refuses a
  `ZoneController` whose `spec.zones` is empty or absent, on create and on update. A `ComputeNode` or
  `SiteUplink` may still list none. The rule is enforced by the API server, so it holds on an install
  that serves no admission webhook.
- **`ComputeWorkloadTemplate` refuses `flexVolume` and inline `csi` volumes**, beside `hostPath` and
  `gitRepo`, in the CEL rule and in `compute.ValidatePodTemplate`, which the webhook and the workload
  builder both call. Each hands a pod whatever a node-installed driver exposes, node paths
  included. Pod Security Standards Restricted refuses `flexVolume` and permits inline `csi`, so
  neither profile closed this. Storage reaches a workload through a `persistentVolumeClaim` or a
  generic `ephemeral` volume.
- **`FleetTask.spec.inputs` and `spec.flow` are immutable after create.** Two CEL transition rules on
  `FleetTask.spec` refuse an update that adds, removes or changes an input, or that adds, removes or
  changes the flow, so a task cannot come to disagree with the children it has already created. A
  `TaskTrigger`'s `spec.taskTemplate` stays editable.
- **Activating a draft `FleetTask` needs the new `activate` verb, and never a ServiceAccount.** The
  `FleetTask` validating webhook requires `activate` on `fleettasks` (SubjectAccessReview) for an
  update that moves `spec.draft` from `true` to `false`. It refuses every ServiceAccount principal
  (username `system:serviceaccount:…` or group `system:serviceaccounts`) before it consults RBAC.
  `operator`, `fleet-manager` and `admin` hold `activate`, in `config/rbac` and in the chart.
- **A ServiceAccount cannot add members to a draft.** By the same test, the webhook refuses a
  ServiceAccount an update that appends to a stored draft. A status write, a label, a finalizer or
  a cancel by a ServiceAccount is still admitted, and a person may still append.

**What stays open, stated in the specification.** An identity that is not a ServiceAccount and holds
`append` can add members to a draft between a person reading it and activating it. An activation is
not written to the safety audit log. On an install that serves no admission webhooks the `activate`
check and the ServiceAccount refusals do not run, and `make run` says so.

**Migration.**
- A client that edited a task's inputs or flow creates a new `FleetTask` instead.
- A custom role that activates drafts needs `activate` on `fleettasks`.
- Automation that cleared `spec.draft`, or that appended to a draft, under a ServiceAccount is
  refused. A person activates the draft.
- A `ComputeWorkloadTemplate` that mounts an inline CSI or FlexVolume volume is refused. Move the
  storage to a `persistentVolumeClaim` or an `ephemeral` volume.

### Adapter contract: 1.3.0 → 1.4.0 (MINOR), and it EVICTS a 1.2.0 adapter

- **The fleet-adapter contract version is now `1.4.0`.** A **minor** bump on ADR-0032 Amendment 1
  §A1.1's rule. It carries:
  - three additive proto fields: `custom_type` on `SupportedAction`, `AssignAction` and
    `ValidateAction`;
  - one additive enum value, `CANCEL_DISPOSITION_STOP_UNCONFIRMED`;
  - four additions that RFC-0001 specifies for the stop latch: the `EstopClear` arm
    of `ControlPlaneSafetyMessage`; the `EstopClearAck` and `LocalStopReport`
    arms of `AdapterSafetyMessage`, with `EstopClear.ends_local_set` and `LocalStopReport.set_id`
    among their fields; and the stop's fence, `Command.estop` on the `ControlStream`,
    which the proto keeps as a deprecated field and RFC-0001 gives a new meaning;
  - the leader term: `Command.leader_term` (field 21) and `EstopClear.leader_term`, with
    `CommandResult.stale_leader_term` (field 20). An adapter rejects a `Command` other than a
    stop, and an `EstopClear`, whose term is lower than the highest it has seen for the robot;
  - the retained outcome: `ActionStatusUpdate.retained` (field 8) and the `ActionOutcomeAck` arm
    of `ControlPlaneMessage` (field 8). An adapter keeps the terminal outcome of a robot's last
    action and reports it on each reconnect until the control plane acknowledges it.

  The last three items are the seven additions the Fleet Adapter Protocol's *Current version*
  paragraph lists once. The first two items have their own entries below, and the stop-latch
  additions are in the entry "A stop is named by its stop ID and fenced on the `ControlStream`" and
  "An edge stop latches at the robot and is cleared only there". The v1 proto in this
  repository carries all seven, and `1.4.0` is published with this draft revision. Nothing is
  renumbered, removed or retyped.

  > **An adapter declaring `1.3.0` is inside the new window.** It registers, reports and is
  > stopped. RFC-0001 now gives a robot work only over a connection that negotiated `1.4.0`, so a
  > `1.3.0` adapter moves to `1.4.0` to be given work.
  >
  > **An adapter still declaring `1.2.0` or below is EVICTED by this release.** It keeps its
  > session, its telemetry and emergency stop, and cannot bring a robot under management.
  > Declaring `1.3.0` ends the eviction with no code change; work needs `1.4.0`.

  **The window at this release.** The supported range is `>=1.3.0 <1.5.0`. A later minor moves it,
  by the N and N-1 rule, or to that minor alone if it binds adapters for safety.

  **In-tree adapters.**
  - The simulation adapter declares `1.4.0`, the contract itself, when run with `--latch`, and
    `1.3.0` otherwise. It is the in-tree adapter that exercises `custom_type` and
    `CANCEL_DISPOSITION_STOP_UNCONFIRMED`. With `--latch` it latches stops and rejects a non-stop
    `Command` from a lower leader term. It does not implement the retained outcome.
  - `--command-derived-plant` makes it answer a cancel with `CANCEL_DISPOSITION_STOP_UNCONFIRMED`
    to a control plane at `1.4.0` or later, and not acknowledge one to an older control plane.
  - `example-noop` declares `1.3.0`, so the N-1 skew is still exercised on every conformance run.
  - The adapter template declares `1.4.0`.
  - The recorded simulation report the demos pin was re-recorded at `1.4.0`, with the same
    per-check verdicts, and its two pinned digests moved with it.
  - No `examples/*/run.sh` carries a contract-version literal, so none changed.

  **The three EXTERNAL reference adapters still declare `1.0.0`** and stay evicted until each
  declares a version inside the window, in its own repository.

  | Contract implemented | Supported range | Evicted at this release |
  | :-- | :-- | :-- |
  | `1.4.0` | `>=1.3.0 <1.5.0` | anything declaring `1.2.0` or below |

### `CANCEL_DISPOSITION_STOP_UNCONFIRMED`: a stop the adapter could not confirm keeps the robot bound

- **One additive enum value, part of contract `1.4.0`.** `CancelDisposition` gains
  `CANCEL_DISPOSITION_STOP_UNCONFIRMED = 4`: the adapter commanded the stop and could not witness the
  robot's rest, because its platform has no motion channel independent of the command path (ADR-0071).
  It is meaningful only with `acknowledged = true`. The contract version moves to `1.4.0` in its own entry.

  **What the control plane does.** An acknowledgement carrying it does not prove the lease dead:
  - **On a requeue** (capability loss, a ZoneMaintenance window, an operator requeue), the requeue
    is not carried out. The action goes `Failed` at once with `failureReason: StopUnconfirmed`
    and a Warning Event `ActionStopUnconfirmed`. The robot, its zone slot and the action's
    `leaseExpiresAt` are kept until the lease has provably expired. Then the robot is released
    (Normal Event `ActionStopUnconfirmedReleased`) and `onFailure` applies. The hold is read
    from the action's status, so a manager restart neither shortens nor extends it.
  - **On an operator cancel, a completion timeout, a `Returning` hold or a preemption handover**,
    the stop is not yet confirmed. The path waits for the lease to expire, as it waits for an
    unacknowledged cancel, and then ends as it always does: an operator cancel still ends
    `Cancelled`.
  - **An unknown value** is still read as `STOPPED_SAFELY`, and so is `UNSPECIFIED`. Making
    `UNSPECIFIED` an error would be a breaking change and is not made.
  - `failureReason` is a free string: no CRD regeneration. An adapter that reports FAILED with the
    message `StopUnconfirmed` holds its own robot bound for up to one lease duration plus the
    clock-skew margin, and affects no other robot or action.

  **Migration for an adapter author.**
  - Nothing is required.
  - An adapter that can't witness rest may answer a cancel `acknowledged = true` with this value,
    but only to a control plane that negotiated `1.4.0` or later. To an older one it answers
    `acknowledged = false`, as it should today.
  - An adapter that can witness rest must not send it.
  - A compute runtime does not send it (`compute.v1` reuses the enum).

  **Conformance.** The suite does not grade the value yet. `C3.6`'s change (the value accepted,
  and graded from `plant.yaml`) arrives in the same later `harness`-class suite revision as the
  Custom-verb rows.

### A Custom verb travels on the wire: `custom_type` on `SupportedAction`, `AssignAction` and `ValidateAction`

- **Three additive proto fields, part of contract `1.4.0`.** `SupportedAction.custom_type` (field 6)
  keys a catalog entry by the verb a `Custom` action performs, so an adapter serving two Custom
  verbs declares each one's parameters separately instead of one merged list.
  `AssignAction.custom_type` (field 12) and `ValidateAction.custom_type` (field 3) carry
  `FleetAction.spec.customType`, which until now reached no adapter. The CRD mirror is
  `FleetAdapter.status.supportedActions[].customType` (at most 63 characters, the bound
  `spec.customType` carries). Nothing is renumbered, removed or retyped. The contract version
  moves to `1.4.0` in its own entry, with the cancel-disposition value it also carries.

  **What the control plane does.** It projects `custom_type` onto the catalog on a `Custom` entry
  only (on any other type the field is dropped and the entry kept), and drops an entry whose
  verb is longer than 63 characters rather than let one entry cost the whole status write. It
  sends `spec.customType` on every `Custom` assignment and validation that names one, and never
  writes the verb into the payload. Admission matches a `Custom` action that names a verb on the
  pair: an entry keyed by that verb, or an unkeyed `Custom` entry, admits it. A verb is refused
  only when every `Custom` entry in the namespace is keyed and none by it. A `Custom` action that
  names no verb is admitted on any `Custom` entry, as before.

  **What an older control plane does with several `Custom` entries.** It does not read
  `custom_type`, and treats every entry sharing an action type as one declaration of that type:
  the type is declared if any entry declares it, no single entry's parameters are the type's,
  and the catalog is never refused because a type repeats. That is what every released control
  plane already does, the draft-3 one included (its list is atomic, its projection keeps every
  entry in order, and its admission check asks only whether some entry has the type), so no
  deployed control plane breaks. Its CRD prunes `customType`, so its operators see the entries
  without their verbs.

  **Migration for an adapter author.**
  - Nothing is required. An adapter that ignores the fields is unaffected, and the payload's
    `operation` convention stays accepted.
  - To serve several Custom verbs, declare one entry per verb with `custom_type` set and that
    verb's own `params` and `results`. An unkeyed `Custom` entry keeps its old meaning: every verb
    the adapter serves that no keyed entry names. A keyed entry may also declare `operation`
    (`kind: "enum"`, `allowed: [<verb>]`, not required), so a reader that does not read
    `custom_type` can still tell the entries apart.
  - Read the verb from `custom_type` when it is set and from the payload's `operation` otherwise.
    Decline `INVALID_ACTION` when the two disagree, and when the verb is one the adapter does not
    serve: never run it as another Custom verb. Answer `ValidateAction` the same way.
  - Keep naming the verb as `operation` in payloads meant for an adapter that predates the field
    (see *An adapter that predates `custom_type` reads the verb from the payload only* in
    `docs/known-limitations.md`).

  **Conformance.** The suite does not grade these fields yet. The rows that do (a catalog unique
  per action type and `custom_type`, a verb the adapter did not key declined, a `custom_type` the
  payload contradicts declined) arrive in one later `harness`-class suite revision.

  **In-tree adapters.** The simulation adapter keys `explore-floor` in its own catalog entry,
  keeps an unkeyed `Custom` entry for a `Custom` action that names no verb, and reads the verb as
  above. It now refuses a `Custom` verb it does not serve that it used to run as a verbless
  `Custom` (for example `customType: capture-clip` with a `verb` payload key). The adapter template
  accepts an optional fourth element, the verb, on a `catalog()` entry, and judges an assignment
  against the entry keyed by its verb.

### RFC-0001: the post-clear MUSTs name the stop that holds the robot

- **Two MUSTs in RFC-0001 `#safety-estop-state-machine` are restated to match the stop record.** A
  conforming control plane MUST author `Resuming` on the clear of the last stop that holds a
  stopped robot (it said: on any accepted clear), and MUST refuse the robot's release while a
  zone's stop issued to it has not been cleared, wherever the robot is (it said: while the stop of
  a zone that covers it stands). A control plane that reads a zone's hold from the zone tree alone
  no longer conforms. The reference control plane meets both (`status.estopHeldBy`; see the
  *Security* entry on a stop holding a robot until that stop's own clear).

### Adapter contract: 1.1.0 → 1.2.0 (MINOR) — and this one EVICTS a 1.0.0 adapter

- **The fleet-adapter contract version is now `1.2.0`.** A **minor** bump, ruled as such in
  [ADR-0032](docs/adr/0032-fleet-adapter-contract-versioning-and-conformance-gating.md)
  Amendment 1 §A1.1: four additive proto fields on existing messages, one additive field on
  `CapabilitiesSnapshot`, and a new conformance profile — which is a *suite* change and
  deliberately not a component of the contract version.

  > **An adapter declaring `1.1.0` needs to do nothing.** It stays qualified, it stays
  > assignable, and it does not need to re-run `make conformance` or update its registry row.
  >
  > **An adapter still declaring `1.0.0` is EVICTED by this release.** It is out of the new
  > supported range and is refused `RegisterRobot` / `DiscoverRobot` with
  > `REGISTRATION_REJECTION_VERSION_MISMATCH`. It keeps its session, keeps streaming telemetry
  > and keeps receiving emergency stop — and cannot bring a robot under management. Declaring
  > `1.1.0` is sufficient to fix it; no code change is required, because every `1.1.0` change was
  > additive.

  **Why a minor bump can evict, when the previous entry said it could not.** The supported range
  is **N and N-1 minor**, derived in `internal/contract/version.go` — `>=1.1.0 <1.3.0` at
  `1.2.0`. A two-minor window expires: an adapter falls out on the **second** minor release after
  the version it reports. ADR-0032's original text promised that "patch and minor releases never
  break an adapter"; ADR-0032 Amendment 1 §A1.2 records that sentence as false as written and
  replaces it. Only the second half survives — a minor bump never *invalidates a qualification*,
  and an evicted adapter's conformance result is untouched. **A qualification that survives is
  not the same thing as an adapter that still works.**

  The three in-tree adapters (`adapters/simulation`, `adapters/example-noop`,
  `adapters/template`) moved `1.0.0` → `1.1.0` with this bump, and `1.1.0` → `1.2.0` with the
  one below. They stay deliberately one minor behind, so the N-1 skew is still exercised end to
  end on every conformance run rather than only asserted. (The template left this arrangement
  at `1.3.0`; see the entry below.)

### Adapter contract: 1.2.0 → 1.3.0 (MINOR) — and this one EVICTS a 1.1.0 adapter

- **The fleet-adapter contract version is now `1.3.0`.** A **minor** bump on the same rule as the
  entry above: one additive `Command` arm (`zone_constraints`) and its ack, carrying the operating
  rules in force where a robot now is. Nothing renumbers, removes, retypes or changes the meaning
  of an existing field, and the new arm is **optional** — an adapter that does not implement it
  answers `unsupported=true` and is not in breach.

  > **An adapter declaring `1.2.0` needs to do nothing.** It is inside the new window.
  >
  > **An adapter still declaring `1.1.0` or below is EVICTED by this release.** Same mechanism,
  > same consequence: session kept, telemetry flowing, emergency stop honoured, and no robot
  > brought under management. Declaring `1.2.0` is sufficient; no code change is required.

  `adapters/simulation` and `adapters/example-noop` moved `1.1.0` → `1.2.0` with this release and
  stay one minor behind by design, so the N-1 skew is exercised on every conformance run. The
  adapter template (`adapters/template`) declares `1.3.0`, the contract version itself, so a
  generated adapter starts at N rather than N-1; `make spec-contract` fails if the template's
  `CONTRACT_VERSION` differs from `contract.Version`.

  **The three EXTERNAL reference adapters — `fleet-adapter-ros2`, `fleet-adapter-vda5050`,
  `fleet-adapter-mavlink` — still declare `1.0.0` and are evicted.** They were already outside
  the previous window, so this release adds no new eviction for them; it does not clear theirs
  either. Each needs a one-line `CONTRACT_VERSION` change in its own repository, which this
  project cannot make (ADR-0005: independent install, independent cadence). Until then they are
  connected, qualified and unable to register a robot. The in-tree gate
  `TestRegistryRowsDeclareASupportedContractVersion` reads every `adapters/REGISTRY.md` row and
  logs each outstanding eviction with the action that clears it; it cannot skip, because the
  registry is in this tree even when the adapters are not.

  **Release → contract map, and how to read it.** A vendor tracking the contract should read the
  **window**, not the latest number — the question is not *which version is current* but *how many
  minors back am I*. The window is `N` and `N-1`, derived in `internal/contract/version.go`:

  | Contract implemented | Supported range | Evicted at this release |
  | :-- | :-- | :-- |
  | `1.1.0` | `>=1.0.0 <1.2.0` | — |
  | `1.2.0` | `>=1.1.0 <1.3.0` | anything declaring `1.0.0` |
  | `1.3.0` | `>=1.2.0 <1.4.0` | anything declaring `1.1.0` or below |

### Breaking

- **ACTION REQUIRED for tooling that deletes Robots: a Robot that holds a stop is refused.** On
  an install with admission webhooks, deleting a `Robot` whose `status.estopState` is set and not
  `Normal`, whose phase is `Error`, or that has a `Failed` component is refused, for every
  identity, until it is released (`swarmctl estop resume` for an estop; the robot's own report for
  `Error` or a `Failed` component). A robot held by an estop or a `Failed` component that never
  reports again cannot be released, and stays held; one held only by `Error` is marked `Offline`
  when it falls silent, which ends that hold. Deleting its namespace still removes it, during a manager outage too, although
  the namespace itself then waits for the manager (see *A namespace deleted during a manager
  outage stays Terminating* in known-limitations). A cleanup
  script that deletes robots one by one must release them first, or delete the namespace. The
  check runs as its own webhook, `vrobotdelete.swarmada.io`, with `failurePolicy: Ignore`: while
  no manager replica serves it, the delete of a held robot is admitted.
- **ACTION REQUIRED for tooling that relabels Robots: `swarmada.io/robot-id` cannot change after
  a Robot is created.** On an install with admission webhooks, an update that changes or removes
  the annotation is refused for every identity (an unset annotation may still take the Robot's
  name). To change a robot's id, delete the Robot and create it again.
- **`swarmada:admin` can no longer move a Robot's `status.phase` into or out of `Maintenance` or
  `Offline` through `robots/status`.** The control plane writes those phases; the resume check
  reads them.
- **ACTION REQUIRED for a Fleet Adapter that writes `robots/status` through the Kubernetes
  API: the `swarmada:robot` role no longer grants it.** The role keeps `get`, `list` and `watch`
  on robots, actions and zones, and holds no write. Report telemetry, phase, hardware and
  capabilities over the `ControlStream` (the `telemetry` and `capabilities` messages), and the
  control plane writes `Robot.status` from them. Every in-tree adapter already does. A direct
  status write from the adapter's service account is now refused by RBAC on every install,
  with or without admission webhooks.

### Added

- **A declined assignment is recorded on the `FleetAction` and the `Robot`.** When an adapter declines
  an assignment, the control plane writes a `Warning` Event `AssignmentDeclined` on both, naming the
  robot, the rejection and the adapter's reason. The action returns to `Pending` and can go to another
  robot as before; the Events keep the robot that declined, until they expire (see known-limitations).
- **`swarmctl estop resume <robot>...`** releases robots from `Resuming` after an estop clear, and
  prints the control plane's reason when a release is refused (see Changed).
- **`docs/rule-index.md`**, generated from the RFC-0001 sources by `scripts/gen-rule-index.py`:
  every rule identifier cited in the repository, with its statement and where it is stated. `make
  test` fails when a public file cites an identifier the index does not list.
- **The zone-restriction adapter surface (axis 3), all additive** — the wire half of
  [ADR-0058](docs/adr/0058-zone-restrictions-are-one-model-across-three-axes.md). Four
  `fleet_adapter.v1` surfaces and one CRD projection:
  `CapabilitiesSnapshot.honoured_operating_constraints` (FA-R1, field 8),
  `Robot.status.honouredOperatingConstraints[]`, `AssignAction.operating_constraints` carrying
  the *resolved effective* rules for the target zone chain (FA-R2, field 11), and
  `ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET` with
  `AssignActionResult.unhonourable_subjects` for an adapter that must decline (FA-R5).

  The control plane reads it. The capabilities ingestor projects the declaration onto
  `Robot.status.honouredOperatingConstraints[]`; candidate filter 12 withholds a robot whose
  adapter does not declare a subject an effective operating rule names; the resolved rules are
  carried on `AssignAction`; and an `ASSIGN_ACTION_REJECTION_OPERATING_CONSTRAINT_UNMET` decline
  returns the action to `Pending` for the next candidate. On the adapter side the decline obligation is graded by
  `C17.1`, `C17.2` and `C17.4`, and the declaration's shape by `C17.3`; the honour and attest
  obligations are not observable on the wire and are not tested.

### Security

- **A Robot that holds a stop cannot be deleted.** A `Robot` whose `status.estopState` is set and
  not `Normal`, whose phase is `Error`, or that has a `Failed` component is refused deletion by
  the `vrobotdelete.swarmada.io` webhook (DELETE on `robots`, `failurePolicy: Ignore`, so a
  manager outage never blocks deleting a `Robot`, and during one a held robot can be deleted; the
  `FleetZone`, `FleetTask`, `FirmwareRollout` and `ModelRollout` delete checks still fail closed,
  so a namespace that holds one of those objects waits for the manager),
  from every identity, the control plane's and `swarmada:admin`'s included. While such a robot is
  held, only the control plane may remove its last finalizer, and a `Robot` being deleted gets no
  new work. Previously a `swarmada:fleet-manager`,
  which cannot clear an estop, could delete a stopped robot and apply it again, and it came back
  `Normal` and dispatchable; Offline auto-removal followed by auto-admit did the same with no
  human. Offline auto-removal now leaves such a robot in place and records a `Warning` Event,
  `AutoRemovalHeld`. A delete is admitted while the namespace is being deleted. Installs without
  admission webhooks (`make deploy-minimal`, the quickstart overlays, the Helm chart with
  `webhooks.enabled=false`, `make run`) do not refuse the delete.
- **A write to a Robot's `status.estopState` or `status.estopBaseline` is refused unless it
  comes from the control plane's own service account.** Previously the `swarmada:robot` (Fleet
  Adapter) and `swarmada:admin` roles could release a robot from an estop with one `robots/status`
  patch, skipping the clear, the resume checks and the audit entry. The same check refuses,
  from any identity but the control plane's, a write that moves `status.phase` out of `Error`,
  or into or out of `Maintenance` or `Offline`, removes or changes a `status.hardware` entry
  reported `Failed`, or changes
  `status.capabilities`, or changes the state of any `status.hardware` entry, the `status` of any
  `status.installedModels` entry, or `status.modelGrantedCapabilities`, which the resume check
  derives the capabilities from again; the other status fields are not guarded. Enforced by a new
  validating webhook, `vrobotstatus.swarmada.io` (UPDATE on `robots/status`, `failurePolicy: Fail`;
  the chart renders `Ignore` only with `webhooks.acknowledgeFailOpenSecurity`), on installs with
  admission webhooks (`make deploy`, the Helm chart by default). `make deploy-minimal`, the
  quickstart overlays, the Helm chart with `webhooks.enabled=false` and `make run` install none,
  and there the fields stay writable by any principal allowed to write `robots/status`. The Fleet Adapter's role no longer is one on any install (see Breaking).
- **Documented: what an install without admission webhooks leaves unchecked, and a way past the
  `robots/status` guard.** `docs/known-limitations.md` lists the four installs that run no
  admission webhooks and what each leaves open: the estop-annotation checks, the `cancel` and
  `append` checks, the `robots/status` guard and the delete refusal. The chart README warns at
  `webhooks.enabled=false`. It also records that `swarmada:admin`, bound cluster-wide, holds write
  on `customresourcedefinitions`: it can remove the `Robot` CRD's status subresource and then set
  `status.estopState`, `status.phase` and `status.hardware` on the main resource, past the guard,
  on any install. Bind it with a `RoleBinding` per namespace.
- **ACTION REQUIRED — the manager refuses `--fleet-adapter-insecure-authz` together with
  ControlStream mTLS.** When the three `--fleet-adapter-tls-*` file flags are set and the flag is
  also set, the manager exits at startup with *"--fleet-adapter-insecure-authz cannot be combined
  with ControlStream mTLS … drop the flag and list each robot's class in its FleetAdapter's
  spec.servesRobotClasses"*. The pair used to be served with per-robot authorization switched off,
  which defeated the verified adapter identity mTLS exists to provide. To upgrade, remove the flag
  and authorize robots the supported way: give each `Robot` a `spec.robotClass` and list that class
  in its `FleetAdapter`'s `spec.servesRobotClasses`. The `quickstart-mtls` and `demo-ab-live`
  overlays and the sample robots (`robotClass: simbot-v1`) do this. `quickstart-dev` (plaintext) is
  the only overlay that still sets the flag.
- **An emergency-stop annotation present at create is authorized.** A `Robot`, `FleetZone` or
  `SwarmadaConfig` created with `swarmada.io/estop-triggered` already set is refused unless the
  creator holds the `estop-trigger` verb on that resource. The estop controllers act on such an
  object — on a `SwarmadaConfig`, across every robot in the namespace — so the create path was an
  unchecked way to issue a stop. No built-in role was affected; a custom role holding `create`
  without `estop-trigger` was.
- **Cancelling a `FleetTask` requires the `cancel` verb on `fleettasks`.** Changing
  `spec.desiredState` to `Cancelled` is checked by a SubjectAccessReview; `update` alone is no
  longer enough. The built-in `operator`, `fleet-manager` and `admin` roles gain `cancel` on
  `fleettasks` in `config/rbac` and in the Helm chart, so they are unaffected. **A custom role that
  cancels tasks needs the verb added.** `swarmctl cancel task` preflights `patch` and `cancel`, so
  a principal without `cancel` is refused before anything is written.
- **A ControlStream `CommandResult` answers only the command it was sent for.** A result is
  matched to its waiter by the stream the command went down and the robot it named, not by
  `command_id` alone. A result from another adapter's stream, or naming another robot, is refused,
  logged `PERMISSION_DENIED` and sealed in the audit log as `ROBOT_AUTHZ_DENIED`; the stream stays
  up.
- **An `EstopAck` confirms only the estop sent down the same stream.** On the `SafetyStream`, and
  on the edge node's `EdgeStream`, an ack arriving on any other connection is refused and audited
  as `ROBOT_AUTHZ_DENIED`, and the estop keeps waiting for its own stream (resolving `Failed` if
  that stream never answers). Before this, a second process holding the same certificate could
  record a robot `Stopped` for a stop it never received.
- **The control plane sends a `SafetyStream`'s response headers once it serves the stream.** When
  the stream is registered for estops under a verified adapter identity, the control plane sends
  its gRPC response headers, before any message. An adapter can wait for them
  (`call.initial_metadata()` in Python, `stream.Header()` in Go) before it counts its estop path as
  open. Before this, the control plane sent nothing on the stream until the first estop, and a
  gRPC client's call to `SafetyStream` returns before the server has accepted the stream: an
  adapter that counted the stream as open when it dialled could register a robot and accept
  motion while no estop could reach it. A stream refused for configuration (`UNAVAILABLE`),
  refused at the TLS handshake, or opened with a trusted certificate whose SAN names no adapter
  receives no headers, and no estop can reach it. An adapter that does not wait for the headers is
  unaffected; `adapters/simulation`, `adapters/example-noop` and `adapters/template` do not wait
  for them. The conformance harness's stand-in control plane sends them on accept too (see
  Fixed).
- **The ControlStream server certificate is limited to `server auth`.** The `controlstream-server`
  certificates in `config/controlstream-tls` and `config/overlays/quickstart-mtls` declare
  `usages: [server auth, digital signature, key encipherment]`. With no extended key usage, the
  server key was accepted as an adapter client identity in namespace `system`.
- **The Helm chart refuses a fail-open admission webhook unless asked twice.** `webhooks.failurePolicy: Ignore`
  renders only with `webhooks.acknowledgeFailOpenSecurity: true`; the default is `Fail`. With `Ignore`,
  every request is admitted while the webhook server is unreachable, which switches off the estop verb
  and estop-marker checks, the Robot admission gate and the zone-delete check.
- **A robot in a stopped zone gets no work, and one that arrives in it is stopped.** Dispatch
  withholds every robot whose `status.currentZone` or `spec.zone`, or a `spec.parentZone` ancestor
  of either, has an estop in force or requested and not cleared, whatever the robot's own
  `estopState`. A `FleetZone` list that cannot be read withholds every robot. A robot created into
  a stopped zone, or moved into one, is sent the zone's stop through the same confirmed path as the
  fan-out, and reads `Stopped`, or `Failed` if the stop is not confirmed. Previously such a robot
  read `Normal` and was dispatched while the zone read `Triggered`. A robot the stop has not reached
  when the zone is cleared (created as the clear is applied, or while the manager is down across
  the clear) is not stopped: it is withheld until the clear is applied, then reads `Normal` and
  takes work with no `swarmctl estop resume` (see known-limitations).
- **A namespace-scope estop holds a robot created after it.** While the namespace's `SwarmadaConfig`
  carries the `swarmada.io/estop-triggered` annotation or the `swarmada.io/estop-processed` marker,
  dispatch withholds every robot in the namespace, whatever its own `estopState`, and a
  `SwarmadaConfig` list that cannot be read withholds every robot. A `Robot` created in the
  namespace while the stop is in force is sent the namespace's stop through the confirmed path and
  reads `Stopped`, or `Failed` if the stop is not confirmed. Previously such a robot read `Normal`
  and was dispatched. The zone stop's exception applies: a robot the stop has not reached when the
  namespace is cleared is not stopped.
- **An estop annotation with an empty value is a stop.** The presence of
  `swarmada.io/estop-triggered` on a `Robot`, `FleetZone` or `SwarmadaConfig` is the trigger,
  whatever its value. Written empty (`kubectl annotate robot <name> swarmada.io/estop-triggered=`,
  or a script's unset variable), it stops the robot, the zone's robots or the namespace's robots,
  and the safety audit log records the reason `none given`. Previously the write was admitted and
  stopped nothing at robot or zone scope; at namespace scope the robots were stopped and removing
  the annotation did not clear them.
- **A zone or namespace clear leaves a robot its own estop.** A robot that carries its own
  `swarmada.io/estop-triggered` annotation stays stopped when a zone- or namespace-scope clear
  releases the robots around it. A `Warning` Event `EstopHeldByOwnTrigger` on the robot says so,
  and the `ESTOP_CLEARED` audit entry lists it under `held_by_own_estop`. `swarmctl estop-clear`
  names each such robot, and `swarmctl estop resume` and the control plane refuse its release
  while the annotation is present. Removing the robot's own annotation is its clear. Previously
  the wider clear moved it to `Resuming`, and a resume returned it to work with its own estop
  annotation still on it.
- **A clear releases only the robots no other stop holds.** Removing a robot's own
  `swarmada.io/estop-triggered` while a zone's or its namespace's estop still holds it, clearing a
  zone while the namespace's estop or another zone's estop over it is in force, and clearing the
  namespace while a zone's estop is in force each leave those robots stopped. A `Warning` Event
  `EstopHeldByWiderStop` on the robot names the stop that holds it; the `ESTOP_CLEARED` audit entry
  lists it under `held_by_wider_estop` (or `held_by` on a robot-scope clear). `swarmctl
  estop-clear` names each such robot, and `swarmctl estop resume` and the control plane refuse its
  release until that stop is cleared. Previously each clear moved the robot to `Resuming`, and a
  resume to `Normal`, while another stop was in force on it; dispatch still withheld it.
- **ACTION REQUIRED for runbooks that release a robot that moved out of a stopped zone: a stop
  holds a robot until that stop's own clear, wherever the robot goes.** Each robot a zone's or the
  namespace's stop reaches records that stop in a new status field, `status.estopHeldBy`, and only
  that stop's clear removes the record. While it stands the robot is withheld from dispatch, an
  action it holds is paused, and its release is refused, whatever the zone tree, the robot's
  `spec.zone`, its `status.currentZone` or an ancestor's `estopPolicy.propagateToChildren` say
  now. A zone's clear reaches every robot that records the zone's stop, wherever the robot is, and
  moves it to `Resuming`. Previously the hold was read from the zone tree as it stood: a
  fleet-manager's re-parent or move, a propagate flip, or the robot's own position report took a
  stopped robot out from under the stop, and another scope's clear and a resume released it while
  the stop stood; and a robot released by a robot-scope stop and clear after it moved out was not
  stopped again when it came back into the still-stopped zone. A robot-scope stop and clear no
  longer releases a robot a zone's or the namespace's stop holds: the release is refused, naming
  the stop. A robot that records a stop whose zone or `SwarmadaConfig` no longer exists is released
  by a robot-scope stop and clear (see known-limitations). A zone created or re-parented under a
  stopped zone stops the robots it brings under that stop on its own reconcile, rather than when
  the stopped zone is next reconciled.
- **A stop is recorded as issued only once its outcome is written.** When the write of a robot's
  `Stopped`, `Failed` or `Stopping` to `status.estopState` is refused, the robot-, zone- and
  namespace-estop controllers do not write their `swarmada.io/estop-processed` marker, and issue the
  stop again; the `ESTOP_TRIGGERED` audit entry records `state_written: false` for the attempt whose
  outcome was not written. A robot that carries its own `swarmada.io/estop-triggered` annotation, or
  records a zone's or the namespace's stop, is withheld from dispatch, its held action is paused, and
  no firmware or model rollout enters it into a batch, whatever its `estopState` reads. A robot that
  records a stop and reads `Normal` is stopped on the next reconcile of that stop, and a clear moves
  it to `Resuming`. Previously one refused status write left the robot `Normal`, dispatchable and
  running the action it held, nothing retried the stop, the audit entry said the robot was stopped,
  and the zone's clear returned it to work with no resume.
- **A new stop on a robot is issued before a pending resume request, and a resume request is
  evaluated once.** The control plane claims a resume request (writes
  `swarmada.io/estop-resume-claimed`) before it evaluates it, so a request whose handling could not
  be written is never evaluated a second time, and a refused release stays closed. A request claimed
  and never answered is reported with a `Warning` Event `EstopResumeUnanswered`; issue it again.
  Previously a resume request whose handled write kept failing blocked a new robot-scope stop on the
  same robot, and a refused request could be evaluated again and release the robot later.
- **`swarmctl estop resume` no longer reports a release as refused.** The control plane records a
  resume request as handled (`swarmada.io/estop-resume-processed`) only after the robot's
  `estopState` answers it, in the same write that drops the claim, so a reader that sees the request
  handled reads its outcome. Previously the marker was written with the claim, before the request
  was evaluated: `swarmctl estop resume --wait` could read the robot still `Resuming`, print "resume
  refused" and exit 1 for a robot the control plane released a moment later.
- **A zone's stop is in force from its processed marker alone.** Whether a zone's stop is in force
  is decided from the `swarmada.io/estop-processed` marker, which only the control plane writes. A
  write of `status.estopStatus` or `status.lastEstopAt` by another identity (`swarmada:edge` may
  patch `fleetzones/status`) is written back on the reconcile that write triggers, and robots that
  enter the zone are still stopped. Previously writing `estopStatus: Clear` and
  `lastEstopAt: null` switched off the stop for robots entering the zone, and the zone read `Clear`
  while its stop stood. A zone created already carrying the trigger and the marker (only the control
  plane can create one) stops its robots.
- **The `SwarmadaConfig` that carries a namespace's stop cannot be deleted.** A new webhook,
  `vswarmadaconfigdelete.swarmada.io` (DELETE on `swarmadaconfigs`, `failurePolicy: Ignore`),
  refuses the delete while the config carries `swarmada.io/estop-triggered` or its clear has not been
  applied, from every identity, and names the clear:
  `kubectl annotate swarmadaconfig <name> swarmada.io/estop-triggered- -n <namespace>`. A delete is
  admitted while the namespace is being deleted. Previously the delete was admitted and ended the
  namespace's stop with no clear and no audit entry, and a robot created afterwards took work.
  While no manager replica serves the webhooks the delete is admitted; the robots the stop reached
  keep it.
- **`status.estopHeldBy` and `status.currentZone` are written by the control plane only.** The
  robots/status guard (`vrobotstatus.swarmada.io`) refuses a change to either from any other
  identity, `swarmada:admin` included. Previously an admin's `status.currentZone` write placed a
  robot in another zone on paper, and the position code kept that value: a zone's stop then did not
  reach the robot, or a stopped robot was taken out from under its zone's stop.
- **`swarmctl estop-clear` on a zone that has no stop fails.** It says the zone has no emergency
  stop to clear, changes nothing and exits non-zero; on a zone whose clear is already being applied
  it says so. Previously it printed that the stop was cleared and its robots were `Resuming`, and
  exited 0, though nothing was written.
- **Firmware and model rollouts hold back a robot under an emergency stop.** A robot whose
  `estopState` is `Stopping`, `Stopped`, `Failed` or `Resuming`, that carries its own
  `swarmada.io/estop-triggered` annotation, or that records a zone's or the namespace's stop is not
  entered into a `FirmwareRollout` or `ModelRollout` batch; it counts as skipped and is checked again
  on each pass. A robot stopped after it entered a batch is covered by the next entry. Previously
  the batch check read only phase, battery and hardware.
- **A robot stopped after it entered a rollout batch gets no further update.** A robot told to pull
  firmware (the `swarmada.io/pending-firmware-*` annotations) that is stopped before it reports its
  install under way has those annotations withdrawn; it counts as skipped and is told again once
  nothing holds it. A failed robot under a stop is not pushed its `ModelRollout` rollback; it stays
  failed and the push is retried. Each `model_update` push first re-reads the robot from the API
  server, so a stop that reached the robot after the manager's cache last saw it is honoured. Entry
  to a firmware or a model batch also reads the holds dispatch reads: the stop of the robot's zone
  or of any zone above it, whatever that zone's `propagateToChildren` says, and the namespace's
  stop, from its trigger until its clear is applied. Three `Warning` Event reasons are added on the
  rollout: `FirmwareUpdateWithheld` (a pull withdrawn), `FirmwareUpdateUnderWayUnderStop` (once per
  robot: a stopped robot whose install it reported under way, which is kept) and
  `ModelRollbackWithheld` (once per robot). Previously such a robot kept the pull it was told to
  make before its stop, and was pushed its rollback under the stop. A firmware install the robot has
  reported under way, and a model update the adapter acknowledged, are not taken back (see
  known-limitations).
- **A robot that comes back while a stop holds it is not given more time on its action.** An action
  that went `Revoking` because its robot was unreachable stays `Revoking` while a stop holds the
  robot (`estopState` `Stopping`, `Stopped`, `Failed` or `Resuming`, its own
  `swarmada.io/estop-triggered` annotation, or a stop it records in `status.estopHeldBy`). It is not
  re-adopted, no `renew_lease` is sent, and it is not returned to `Pending`, whatever
  `actionCancellation.onDisconnect` says. Its status message starts `held by estop`. The robot
  stops itself when the lease it already holds runs out. The hold ends when the robot's resume is
  confirmed; an operator cancel ends it at any time, and an operator requeue waits for it.
  Previously the robot's return moved the action to `InProgress` and sent the robot a fresh lease
  before the action was paused, so a robot whose stop never reached it (`Failed`) was told to carry
  on for up to one more lease; under `onDisconnect: AfterTimeout` the action was requeued to
  another robot. At this change the stop itself was not sent again on the reconnect; a later
  change sends it again (see the Unreleased entry on the re-sent stop).
- **A maintenance window does not move held work off a stopped robot.** A `ZoneMaintenance` window
  in `Immediate` mode, or in `Graceful` mode past its drain timeout, does not mark for requeue the
  action of a robot a stop holds, or any `Paused` action, whether an estop or
  `spec.desiredState: Paused` paused it; the robot stays on `status.windingDownRobots`. A requeue
  request that stands on an `Assigned` or `InProgress` action when a stop reaches its robot is
  dropped, the action is paused, and a `Warning` Event `RequeueOvertakenByEstop` on the action says
  so; an operator requeues or cancels the action once the work is inspected. Previously the window
  requeued the action an estop paused: it went `Pending` and another robot took it, with no
  operator decision and no record that an estop pause was overridden.
- **A resume confirmation never releases a stop.** A confirmed resume writes `Normal` only over
  `Resuming`. A stop confirmed on the robot after the confirmation read `Resuming`, and before it
  wrote, stands, and the resume is answered `a later estop event superseded this confirmation`.
  Previously the confirmation wrote `Normal` over the confirmed `Stopped`; dispatch still withheld
  the robot while the stop it recorded stood.
- **A maintenance window's gated close reads every stop that holds a robot.** When
  `requireEstopClearBeforeResume` applies, the close keeps in `Maintenance` a robot that carries its
  own `swarmada.io/estop-triggered` annotation or records a zone's or the namespace's stop in
  `status.estopHeldBy`, even when the robot reads `Normal` and its own zone reads `Clear`.
  Previously the close read only the robot's `estopState` and its own zone's `estopStatus`, and
  returned such a robot to `Idle`.
- **A zone's stop reaches the robots that were under it when it was issued.** From the moment a
  zone carries `swarmada.io/estop-triggered` until it carries `swarmada.io/estop-processed`, the
  admission webhooks refuse a `spec.parentZone` change that takes a zone out from under it and a
  `spec.zone` change that takes a robot out of it, from every identity; the refusal names the stop
  and says to retry once it is applied. A `propagateToChildren` change and the delete of a robot in
  that window are refused too, by the later entry "A stop issued and not yet applied, and a stop a
  robot never confirmed, are no longer shed". In the same window the Zone Controller does not move a
  robot's `status.currentZone` out of the stopped zone on a position report: it applies the move
  from the robot's next report after the stop is applied (`status.containingZones` follows every
  report). Both read the zones from the API server, not the cache. If the stop is never applied
  (no control plane running), the edits stay refused and the recorded zone stays where it was.
  Previously such an edit or report, landing before the stop's fan-out ran, left the robot outside
  the stop for good: the fan-out did not find it, the zone read `Triggered` and processed, and the
  robot kept its work.
- **Two `Robot`s that carry one robot-id get no work.** Dispatch reads the `Robot` list from the API
  server, not the manager's cache, and withholds every `Robot` whose robot-id (its
  `swarmada.io/robot-id`, or its name when it has none) another `Robot` also carries, until all but
  one are deleted. Each carries a `RobotIDConflict` condition naming the others. A `Robot` list
  that cannot be read withholds every robot. Two creates that arrive together can still both be
  admitted; neither receives work. Previously commands for that id reached one physical robot
  through either object.
- **The simulation adapter refuses a command or an estop addressed to a robot it does not serve.**
  One ControlStream carries the commands for every `Robot` bound to an adapter, and the adapter ran
  an assignment addressed to another `Robot` on its own robot, driving a stopped robot out of a
  stopped zone. Every command that acts on a robot and names another `robot_id` is answered
  with that command's refusal (an assignment `ROBOT_UNAVAILABLE`, a cancel `acknowledged=false`)
  and nothing reaches the robot. A `SafetyStream` estop for another id is answered
  `ESTOP_STATE_FAILED`, and this robot is not stopped by it. The adapter template refuses them the
  same way (next entry).
- **The adapter template refuses a command or an estop addressed to a robot it does not serve.** A
  generated adapter answers an assignment `ROBOT_UNAVAILABLE`, and a cancel, a lease renewal, a
  pause or a resume with that command's negative, when `Command.robot_id` names a robot other than
  the one it serves. Each answer keeps the `robot_id` it was addressed to and names that id and the
  served one, and nothing reaches the robot. A `SafetyStream` estop for another id is answered
  `ESTOP_STATE_FAILED` and stops nothing. Before this, a generated adapter ran such an assignment or
  cancel on its own robot, and answered such an estop `ESTOP_STATE_STOPPED`, confirming a stop it
  had not made. An adapter generated before this change keeps the fault until its
  `_handle_command` and `run_safety_stream` are updated.

### Changed

- **ACTION REQUIRED for an adapter generated from the template — implement `arrived()` in your
  robot binding.** The template now passes C3.9. A generated adapter stops the robot on a cancel only
  when the cancel names the action it is running; a cancel for a preempted action is acknowledged and
  stops nothing. The adapter reports an action `SUCCEEDED` when the robot arrives at its target, and
  to know that it calls `RobotBinding.arrived()` on every telemetry tick. If you started from an
  earlier template, bring over the new cancel handling and the arrival report, and implement
  `arrived()` in your binding: without it the call raises `AttributeError`, no action reports
  `SUCCEEDED`, and C3.9 fails. The simulated robot drives to its target; at suite revision
  `2026.09.29` on Python 3.10 a freshly generated adapter reports 51 passed, 0 failed, 18 skipped.
- **`make conformance-profiles` exits 0 on a verdict.** A `not-established` or failed verdict is
  written into the reports and printed. The target stops only when the harness run writes no
  report, or when the profiler exits with an error. It also copies the reports' gate-integrity
  entries in beside them, so `make gate-integrity` is green on a clone after it.
- **A `FleetAction` payload may be written as plain keys.** `spec.payload: {route: {...}}` now
  reaches the adapter as that object, and a `FleetTask` member's payload reaches its child
  `FleetAction` intact; before, both were silently emptied to `{}`. The RFC-0001 `raw` form (an
  object whose only key is `raw`, holding base64) keeps its meaning, and each form is kept as
  written. **Compatibility:** a payload that is itself a single `raw` key holding a base64
  string must be sent in the `raw` form, because written as plain keys it reads as the `raw` form.
  The CRD schema is unchanged. RFC-0001 amended: `crds/fleetaction.md` (The Payload Field).
- **Under `rollbackPolicy: Auto`, a reverted robot pauses a `ModelRollout` with `pauseOnError`.**
  A model that fails on its first robot is no longer pushed to every robot in turn; the rollout
  pauses after the batch in which the revert happened. `swarmctl rollout resume` adds the reverted
  robots to `status.excludedRobots` with the failed ones (ADR-0041, Amendment 1). The terminal phase
  is unchanged. RFC-0001 amended: `crds/modelrollout.md`.
- **ACTION REQUIRED — after an estop clear, an operator releases each robot.** `estop-clear` writes
  `Resuming` and sends nothing to the robot. The robot returns to `Normal` only when an operator
  releases it with the new `swarmctl estop resume <robot>... --reason <text>` (the `estop-clear`
  verb, admin-only). Nothing releases it automatically — not its telemetry, not its adapter, not a
  timer — and nothing times the window out. The release is refused, with the reason, while the robot
  has sent no telemetry since the clear, while its own latest report puts it in `Error` or
  `Offline`, while a hardware component is reported `Failed`, or while a capability that was Active
  when the stop was taken is not Active again. A capability a `ZoneMaintenance` window has paused
  never holds the release, nor does one the robot no longer declares; a stop taken before the
  control plane recorded its Active capabilities requires every capability that is not paused to be
  Active. There is no override. Each release and refusal is recorded (`ESTOP_RESUME_CONFIRMED`,
  `ESTOP_RESUME_REFUSED`). The previous automatic release on post-clear telemetry is withdrawn.
- **RFC-0001 no longer specifies two edge estop behaviours that were never implemented**: an edge
  estop when a robot enters a zone it is not admitted to (the edge node checks the robot's assigned
  zone only), and a clear from the edge node or a panel (no message carries one). The safety chapter
  now states that an edge estop reaches only the robot's adapter: the control plane does not record
  it, does not withhold the robot from dispatch, and has no clear path for it. `EDGE_ESTOP_TRIGGERED`
  is written to the edge node's local log and is not marked as reaching the audit chain.
- **ACTION REQUIRED — the Helm chart requires Kubernetes 1.31 or later** (`kubeVersion:
  ">=1.31.0-0"`, was `>=1.27.0-0`; chart version `0.3.0`). `helm install` on 1.27–1.30 is refused.
  On Kubernetes 1.29 (measured), an emergency stop and its clear on a `FleetZone` stored before one
  of the CRD's validation rules existed are refused by that rule; 1.31 and later do not re-check a
  field the update leaves unchanged. The envtest suites run against Kubernetes 1.37.0 (was 1.29.0):
  run `make setup-envtest` to fetch the assets.
- **ACTION REQUIRED — `make deploy` installs `config/controlstream-tls` and needs cert-manager.**
  It deploys the control plane with admission webhooks and ControlStream mTLS
  (`kubectl apply -k config/controlstream-tls/ --server-side --force-conflicts`) and stops early
  if cert-manager is absent. The old behaviour is `make deploy-minimal`: `config/default`, with no
  admission webhooks and no ControlStream, so it serves no adapters.
- **ACTION REQUIRED — the ControlStream listens on port 9445 by default** (manager flag
  `--fleet-adapter-bind-address`, was `:9443`, which collided with the admission webhook server's
  fixed 9443). The chart (`controlStream.port`), every overlay's Service and container, and the
  simulation adapter's `--endpoint` default use 9445. An adapter or manifest that names 9443 for
  the ControlStream needs the port changed.
- **The Helm chart serves the ControlStream with mTLS.** With `controlStream.tls.certManager.enabled`
  (default `true`) the chart issues a self-signed root, a CA issuer and a server certificate
  (`server auth` only), mounts it, and renders the three `--fleet-adapter-tls-*` flags.
  `controlStream.tls.certManager.adapterClients[]` issues per-adapter client certificates
  (`client auth` only); `controlStream.tls.existingSecret` supplies your own. The chart refuses to
  render with the ControlStream on and no TLS source, with `--fleet-adapter-insecure-authz` in the
  extra arguments, or with `replicaCount` above 1: commands are pushed only to the streams the
  leading replica holds, so an adapter connected to another replica would receive none.
- **A robot's `spec.zone` must name a terminal zone.** A `Robot` create, or an update that changes
  `spec.zone`, is refused (`Invalid`) when the zone has `spec.memberZones`: *"spec.zone … is a
  composite zone (members: […]); a robot must be placed in a terminal zone … Name one of its
  members instead"*. A composite holds no robot directly, and a zone estop on a composite is
  refused, so no zone estop could reach a robot placed in one. From the other side, adding
  `spec.memberZones` to a zone that a `Robot` declares is refused and names the robots, and adding
  members to a zone whose estop is active is refused until the estop is cleared. Editing an
  existing composite's members is still allowed.
- **A composite zone's polygon is never used for containment.** A composite's
  `status.geometry.state` is `Absent`, with `GeometryAvailable` `False` and reason
  `CompositeZone`. **Behaviour change:** a robot whose `spec.zone` names a composite (possible
  only if it was admitted before the rule above, or through concurrent writes) is always placed in
  the composite by declaration, even when the composite has a polygon; before, it was derived by
  position. The per-robot and namespace estops reach such a robot. The edge node (`cmd/edge`)
  likewise geofences only zones classified `Containment`, so a `FrameLocal` or composite zone is no
  longer enforced at the edge and no longer expects `EdgeStream` feeds.
- **An emergency stop is not refused for an unrelated admission rule.** An update that changes
  only the estop annotations (`swarmada.io/estop-triggered`, and the control plane's
  `estop-actor` and `estop-processed`) on a `Robot`, `FleetZone` or `SwarmadaConfig` skips the
  carrier's other admission rules — for example a descendant zone that contradicts an ancestor's
  tightened restriction, or a `RobotClass` deleted after the robot was admitted. The estop verbs,
  the processed-marker guard and the composite-zone refusal still apply. CRD validation rules run
  in the API server and are not covered; see the Kubernetes 1.31 entry above.
- **One stream per adapter identity.** A second ControlStream under the same verified identity
  (namespace and FleetAdapter name) replaces the first: the older stream is closed with gRPC
  `ABORTED` (*"superseded: a newer ControlStream for adapter … was accepted; one adapter identity is
  served on one stream, so this one is closed"*), commands and liveness go to the newer stream only,
  the `FleetAdapter` stays `Connected` through the handover, and one `Warning`
  `ControlStreamSuperseded` Event is recorded on the `FleetAdapter`. The `SafetyStream` follows the
  same rule with a `SafetyStreamSuperseded` Event. An estop still awaiting its ack on the replaced
  `SafetyStream` is re-sent down the newer stream, under a fresh `estop_id`, within its original
  deadline. Two processes that share one certificate displace each other on every reconnect; give
  each its own `FleetAdapter` and client certificate. `ABORTED` is distinct from `UNAVAILABLE` so an
  adapter can tell supersession from a fault.
- **A silent robot behind a live adapter goes `Offline`.** A robot known to stream telemetry that
  sends no frame for max(`connectivityOfflineThresholdSeconds`, 3 × `spec.telemetryIntervalSeconds`)
  moves to `Offline` with `Ready=False`, reason `RobotSilent`, even while its adapter is connected,
  and is not dispatched to. It returns to its steady phase when frames resume. Defaults: 30 s, and
  90 s at the 30 s maximum interval.
- **`spec.timeoutSeconds` on a `FleetAction` is enforced.** It is measured from
  `status.startedAt`. A reachable `InProgress` action past its window is stopped through the
  confirmed-stop path and goes `Failed` with `failureReason` *"timeoutSeconds (Ns) elapsed since
  status.startedAt"*; until the stop is confirmed it holds with *"timeoutSeconds elapsed — awaiting
  confirmed stop"*. A lost robot's action goes `Revoking`, and `onDisconnect: WhenActionExpired`
  cancels it. `status.startTime` is superseded by `status.startedAt`, written by no controller,
  and kept for compatibility. Not implemented: the `TimedOut` condition, and `Paused` → `Failed` on
  timeout.
- **`FleetTask` updates re-run the create checks.** When `spec.flow`, `spec.inputs` or
  `spec.actions` changes, the binding and producer-contract checks run again, so an update that
  removes an input a binding reads, or binds an event of an incompatible producer, is refused as
  the same create would be. `spec.inputs` stays mutable.
- **Fields the API accepts and nothing reads are named at the write.** Admission returns a warning
  of the form *"<path>: setting this field has no effect in this release: <detail>"* for:
  `spec.constraints.maxPayloadKg` and `spec.constraints.maxSpeedMs` on a `Robot` (*"Swarmada does
  not enforce this limit and does not send it to the adapter; the adapter must enforce it"*),
  `spec.charging.minBatteryPctToCharge` and `spec.charging.targetBatteryPct` (*"no controller sends
  a robot to charge"*), `spec.motionThresholdMeters`, `spec.maxIdleIntervalSeconds`,
  `spec.adapter.version` (*"nothing compares it with the version the adapter reports; it is only
  copied, as written, into the safety audit entry for an estop that breached its latency bound"*),
  `spec.installedModels[].requiredHardware` (*"a model's capability is granted whatever the state of
  the hardware it names"*); `onUnresolvedParameter` on a `FleetAction` and on every `FleetTask`
  executor (*"no controller reads it, so none of Fail, UseDefault or Omit is applied …"*); and
  `spec.provisioning.mode: DirectRegister` on a `SwarmadaConfig` (*"DirectRegister is not
  implemented, so robots are onboarded as under TwoPhase …"*). A Robot update warns only when one of
  these fields changes. A `FleetAdapter` has no validating webhook, so its `spec.protocolVersion`
  (when it differs from `fleet_adapter.v1`) and `spec.tlsSecretRef` are reported instead through a
  `SpecFieldsIgnored` condition and one Warning Event on the transition — weaker than a warning at
  the write, because the author has to look for it. The fields themselves are unchanged.
- **Signing that cannot verify anything is reported.** A `SwarmadaConfig` that requires signature
  verification and has no `spec.signing.trustRoots` carries condition `SigningIncomplete=True`
  (reason `NoTrustRoots`) and one Warning Event on the transition; the other reasons are
  `TrustRootsConfigured` and `VerificationDisabled`. A `FleetAdapter` refused in that state names
  the condition instead of asking for a signature nothing could verify.
- **A `ModelPolicy` trigger dropped while the policy is suspended emits a Warning Event**,
  `TriggerDroppedWhileSuspended`, naming `swarmctl modelpolicy reset <name>`. The trigger is still
  not queued.
- **A `ModelPolicy` trigger is evaluated once.** One `swarmada.io/model-trigger` annotation
  produces one `status.history` record and adds at most one to `rejectionCount` and
  `consecutiveRejections`, however often the reconciler runs. Before this, one failing payload
  could be recorded as two rejections. The controller writes a second annotation,
  `swarmada.io/model-trigger-claim`, while it evaluates, and removes it together with the trigger.
  It is controller-owned; do not set it.
- **Whole-directory sample applies select by posture.** `kubectl apply -f config/samples/` is
  documented as `kubectl apply -f config/samples/ -l 'swarmada.io/sample-posture!=webhooks-on'`
  for a control plane without admission webhooks (`make run`, `make deploy-minimal`, the
  quickstart overlays, the Helm chart with `webhooks.enabled=false`); the one
  document that needs the Robot webhook carries that label. `tasktrigger_demo.yaml` uses
  `warehouse-a`.
- **Adapter SDK and template: Python 3.10 floor and a lower protobuf floor.** `swarmada-sdk` and the
  adapter template declare `requires-python = ">=3.10"` (was `>=3.12`). The committed Python stubs
  are generated by grpcio-tools 1.64.1 and import on `protobuf>=3.20.3` and `grpcio>=1.64.1` (were
  `>=6.33.5` and `>=1.81.1`), which ROS 2 Humble robot images carry; the root package, SDK and
  template floors moved with them. The generator is a `codegen` extra pinned to 1.64.1, and
  `make proto-py` generates only with that version — through an isolated `uv` environment when
  the active interpreter carries another — and otherwise refuses. `docs/adapter-authoring.md`
  covers installing beside ROS 2.
- **The adapter template ships at the current contract version**, `1.3.0` (see the contract entry
  above), redials a dropped ControlStream and SafetyStream with bounded, jittered exponential
  backoff, and takes its namespace from a `namespace` cookiecutter variable and `--namespace` flag.
  At suite revision `2026.09.25` on Python 3.10 a generated adapter reports 50 passed, 0 failed, 18
  skipped.
- **`swarmctl` exits non-zero on an unknown subcommand**, with cobra's suggestion
  (`swarmctl estop trigge` suggests `trigger`). A bare command group still prints help and exits 0.
- **Demos do not write conformance verdicts.** The quickstart and demo runners use a report the
  conformance harness generates against the simulation adapter (`hack/sim-conformance-report.sh`)
  or the recorded harness report in `config/samples/first-run/`.

### Added

- **`make first-run`** — a secure first run on a freshly created kind cluster: cert-manager, a Helm install with
  admission webhooks and ControlStream mTLS (and no `--fleet-adapter-insecure-authz`), an in-cluster
  simulation adapter with a chart-issued client certificate, `swarmctl admit robot … --class
  simbot-v1`, and a two-step `FleetTask` run to `Succeeded` on the adapter's own report.
- **`config/samples/first-run/`** — a kustomization for the secure posture: `SwarmadaConfig`,
  `RobotClass` `simbot-v1`, `FleetZone`, `FleetAdapter` (`servesRobotClasses: [simbot-v1]`) with a
  harness-generated conformance report, and a `FleetTask`. It emits the `SwarmadaConfig` before any
  `FleetZone` or `FleetAdapter`, which needs the kustomize in kubectl 1.27 or later (kubectl 1.26
  refuses the file: `unknown field "sortOptions"`). `make first-run` applies it in two phases once
  admission is serving.

### Fixed

- **The simulation adapter and the adapter template confirm the first estop after a
  `SafetyStream` redial.** Each `SafetyStream` session reads its own outbound queue, and the
  request generator of a stream that dropped is released. Before this, that generator stayed
  blocked on a queue the redialled stream shared and took the next `EstopAck`, so after a manager
  restart or a dropped connection the first estop resolved `Failed` (not confirmed) although the
  robot stopped, once for each stream that had dropped. An adapter generated from the template
  before this change has the same fault, in `run_safety_stream`.
- **The conformance harness sends a `SafetyStream`'s response headers when it accepts the
  stream**, as the control plane does (see Security). An adapter that waits for them before it
  counts its estop path as open is served by the harness from the start of a run.
- **The texts say what a manager outage blocks.** A manager outage does not block deleting a
  `Robot`, but it does block deleting a namespace that holds a `FleetZone`, `FleetTask`,
  `FirmwareRollout`, `ModelRollout` or `ZoneMaintenance`, which every namespace with robots does.
  The safety chapter named the held-robot delete refusal as `vrobot.swarmada.io` with
  `failurePolicy: Fail`; it is `vrobotdelete.swarmada.io`, `failurePolicy: Ignore`, and it fails
  open. `docs/operations.md` has a runbook for deleting a namespace during an outage.
- **A namespace whose `Robot` carries another tool's finalizer finishes deleting.** In a namespace
  being deleted, the `RobotClass` can go before a `Robot` that a backup or GitOps tool still holds
  with its own finalizer. Removing that finalizer was then refused for every identity, with
  `spec.robotClass: … RobotClass "<class>" not found`, and the namespace stayed `Terminating`.
  With the manager serving, an update to a `Robot` being deleted that changes only its finalizers
  is now admitted without the checks that read other objects. Its robot-id still cannot change,
  and the last finalizer of a robot held by its stop still goes only to the control plane or in a
  namespace being deleted.
- **The operator texts say what `delete fleetaction`, the outage runbook and the Helm notes do.**
  `docs/operations.md` said a `FleetAction` delete is refused while the action runs. Nothing
  refuses it, and the robot is left `InProgress` with no work (see known-limitations). The outage
  runbook's `kubectl replace` takes the webhook rules away from a release Helm writes server-side,
  and the next `helm upgrade` failed with a field conflict. The runbook now ends with a step that
  gives them back. The Helm install notes printed a CRD command that listed nothing; it lists the
  `swarmada.io` CRDs.
- **`swarmctl admit` no longer reports success for an admission that cannot complete.** The
  admit in `config/samples/README.md` (no `--class`), run against the simulation adapter,
  exited 0 and no `Robot` ever appeared: the adapter reports `0.1.0-sim`, which the `Robot`
  CRD's `spec.adapter.version` pattern refuses, and the controller resent the refused create
  on every reconcile with nothing on the `DiscoveredRobot` to say why. `admit` refuses such
  a version before recording anything, naming the field and the pattern (it is not normalised:
  cutting `0.1.0-sim` to `0.1.0` would claim a release build the adapter never reported; a
  `--class` that pins a release version is the route), and refuses a missing or unserved
  `--class` for an adapter that lists `spec.servesRobotClasses`, naming the class to pass. The
  controller never sends a `Robot` the CRD pattern refuses, and records a create the API server
  refuses as an `AdmissionFailed` event, as `docs/operations.md` already said. The README's
  command passes `--class simbot-v1`.
- **The quickstart's conformance line no longer prints a failure inside a passing verdict.**
  `hack/sim-conformance-report.sh` printed the harness report's raw tally,
  `conformant=true … (pass 59, fail 1, skip 9)`, where the one "fail" was a `SHOULD` row. It
  splits the tally as the harness's own summary does since ADR-0048:
  `(pass 59, fail 0, advisory 1, skip 9)`. A row with no level counts as a failure, so the split
  can overstate failures but never hide one.
- **Published text that said something the code does not do.**
  - RFC-0001's FleetAction chapter said that a `Revoking` action queues a `cancel_action` for the
    adapter's reconnect. Nothing is queued: on reconnect the robot's `RegisterAck` carries the
    action as `ROBOT_ACTION_PHASE_REVOKING`, and the robot self-stops when its lease expires.
  - `kubectl explain fleettask.spec.inputs` (and the TaskTrigger template) said an update is not
    checked. An update that changes `spec.inputs`, `spec.flow` or `spec.actions` runs the same
    binding check as a create.
  - RFC-0001's admission table quoted four rejection messages the code does not print (the
    `Robot` adapter gate, `FleetZone` `parentZone` and cycle, `ZoneMaintenance` `zoneName`). Each
    quote in the table is one message the code prints.
  - `examples/warehouse-quickstart/SCENARIOS.md` said `battery-edge` shows the scheduler avoiding
    a dying robot. The run gives that robot the task, because it is the only candidate.
- **Deleting a `Robot` releases its shared-resource holds at once.** A lift or door held (or
  waited for) by the deleted robot's adapter passes to the next robot in the queue, whose adapter is
  sent `reservation_granted`, without a manager restart. A waiter that recovery promotes is sent
  its grant too. Automatic expiry of a hold is still not implemented. RFC-0001 amended: `tde.md`
  (TDE-7).
- **A `ModelRollout` no longer hangs when a robot reports right after acknowledging.** A terminal
  install report written between the controller's re-read of the robot and its `Updating` mark was
  overwritten by that mark, and the rollout waited for ever with the model's capabilities
  suspended. The mark now carries an optimistic lock, on the forward and the rollback push.
- **A model install failure reported right after the acknowledgement is recorded.** A FAILED
  report that landed before the rollout wrote its batch was dropped, so the robot read `Updating`
  for ever and `pauseOnError` never fired; a revert reported failed was dropped the same way. The
  ModelRollout controller now records each acknowledged push for the ingestor, and a declined
  push is still never recorded as a failure (ADR-0055, Amendment 1).
- **`swarmctl cancel task` on a dependency chain ends the task `Cancelled`.** A member waiting on a
  cancelled member is never created, and was counted as still running, so the task was failed
  instead; a cancellation still winding down was also reported `Failed`. Under `ContinueOthers`, the
  tail of a chain whose head failed no longer leaves the task `Running` for ever.
- **The edge node geofences a robot admitted under another name.** It keyed robots by object name and
  matched positions by the robot_id the adapter reports; the two differ after `swarmctl admit
  --name`, and such a robot's breaches were ignored. It now keys by the `swarmada.io/robot-id`
  annotation.
- **A Robot deleted and recreated under the same name gets its status again.** The control plane
  kept per-robot telemetry state in memory across the recreation, so the new Robot never received
  `status.batteryPercent`, `status.hardware` or `status.currentZone` from an unchanged stream, and
  a quickstart scenario re-run on the same cluster failed. That state is now cleared when a Robot
  is recreated.
- **The simulation adapter reports `SUCCEEDED` on arrival.** When the robot comes to rest at the
  target of its current action, the adapter releases the lease and reports `SUCCEEDED` under the
  fencing token the action was accepted with. Before this it never reported `SUCCEEDED`, so no
  `FleetAction` driven by it could complete. Estop, cancel, pause, zone hold and lease self-stop
  still act first. Actions that do not move the robot are not completed this way.
- **`helm install` of the chart failed on the ServiceAccount template** (`apiVersion not set`): a
  whitespace-trimming template tag joined the licence comment to `apiVersion`. `helm template` had
  hidden it.
- **A preempted action is not re-dispatched while its first robot may still run it.** A preempted
  victim on a reachable robot is held until the adapter acknowledges `cancel_action`, or its lease
  provably expires, before it returns to `Pending`. Before this, a lost preemption push plus a free
  robot let the victim run on two robots for up to one lease.
- **A `RobotProbe` verdict holds until the probe clears it.** A component the probe degraded stays at
  least `Degraded` against telemetry, and across a manager restart, until the probe's recovery edge
  clears it; a probe degrade with no adapter message records *"probe <name> failed N consecutive
  cycles"*.
- **Candidate filter 8 fails closed.** When the `ZoneMaintenance` list cannot be read, every
  candidate is withheld and the action stays `Pending`, with an error logged; before, the filter
  was skipped and a robot under maintenance could be assigned.
- **`ZoneMaintenance` resumes every robot it paused**, including robots that left its scope during
  the window (a member removed from a composite, a member zone deleted, a robot re-homed).
- **An estop audit entry is never attributed to an earlier operator.** The `swarmada.io/estop-actor`
  stamp is removed in the same write that seals the edge, so a stamp left by a skipped defaulter is
  sealed as unattributed, never as someone else's.
- **Zone admission reads the API server, not the manager's cache**, for the parent-zone check, the
  ancestry walk and the Robot gate's zone reads.
- **Estop outcomes for two outstanding triggers.** A newer trigger's confirmed `Stopped` is written
  even if the robot's state changed after that trigger began, and an older trigger's later timeout
  no longer turns `Stopped` into `Failed`; it is recorded as a Warning Event,
  `EstopOutcomeSuperseded`, and still audited as a failed trigger.
- **Quickstart and demo scenarios complete for real.** `make quickstart SCENARIO=hardware-fault` and
  `SCENARIO=battery-handoff` run three live simulated robots, each under its own `FleetAdapter`,
  and the moved task completes on the spare from that spare's own adapter report; the runner writes
  no robot status in these scenarios. In `make demo-test`, the assignment-latency fixture's robot is
  served by a second live simulation adapter, so `SwarmadaSchedulerAssignmentLatencyHigh` fires from
  a real assignment.

### Adapter contract: 1.0.0 → 1.1.0 (MINOR)

- **The fleet-adapter contract version is now `1.1.0`.** This is a **minor** bump, and
  under [ADR-0032](docs/adr/0032-fleet-adapter-contract-versioning-and-conformance-gating.md)
  that has a defined meaning for an adapter vendor:

  > **Nothing is required of an adapter that declares `1.0.0`.** It stays qualified, it
  > stays assignable, and it does not need to re-run `make conformance` or update its
  > registry row.

  The control plane accepts **N and N-1 minor** within the current major — the range is
  `>=1.0.0 <1.2.0`, derived in `internal/contract/version.go` — so a `1.0.0` adapter
  negotiates and registers normally against a `1.1.0` control plane. Only a **major** bump
  requires re-qualification. This is stated here rather than left to inference precisely
  because a version number moving is something a vendor reasonably reads as a
  demand on their time; it is not one.

  What did change is additive: new proto fields on existing messages, new CRD kinds, and
  new conformance checks. Nothing an adapter already sends or answers changed shape or
  number. The reference `adapters/simulation/sim_adapter.py` deliberately still declares
  `1.0.0`, so the N-1 skew is exercised end to end on every conformance run rather than
  only asserted.

  **Reading the conformance report.** `contractVersion` in a `ConformanceReport` is the
  version the *adapter* reported and the result was earned against — for the reference
  adapter, `1.0.0`. The suite revision that produced the result is recorded beside it as
  `suiteRevision`, never folded into it.

### Added

- **Five new CRD kinds** for the compute-action surface, all namespaced and all additive —
  no existing kind changed shape:
  - **`ComputeEndpoint`** (`cep`) — an administrator-installed reference to an addressable
    non-robot executor: a binding (gRPC / HTTP / MCP), a provisioning mode, a contract, and
    declared parameters and results.
  - **`ComputeWorkloadTemplate`** (`cwt`) — an administrator-authored pod template for
    `provision: workload`. It is the **only** place a pod spec, an image, or a command may
    be written: a `ComputeEndpoint` names one by name, and no field in a `FleetTask` or
    `FleetAction` can supply one directly.
  - **`ResultSink`** (`rs`) — a named egress destination (`Database`, `ObjectStore`,
    `Webhook`) holding its own credential, so a result destination is administrator-owned
    rather than task-supplied.
  - **`ArtifactStore`** (`as`) — credential-bearing artifact storage. The credential is
    resolved by the control plane and passed to a runtime as a short-lived, scope-bounded
    grant; it never appears in a `FleetTask` or `FleetAction`, because no field exists
    there to carry one.
  - **`EdgeNode`** (`en`) — a non-robot node the control plane can push to, with a
    `nodeClass` that a `ModelRollout`'s `targetSelector` resolves against when
    `spec.targetKind` is `EdgeNode`.
- **Capability-loss reassignment.** When a reachable robot's required capability
  degrades while a task is in flight, the control plane safely stops and
  reassigns that task instead of leaving it stranded. The stop is gated on an
  adapter-reported stoppable state: a robot in the middle of a non-stoppable
  action completes it before releasing. Three outcomes are represented — a safe
  hand-off returns the task to `Pending` for rescheduling; a complete-then-release
  finishes the action first; a recovery marks the task `Failed` with reason
  `CapabilityLostDuringExecution` and runs its `onFailure`. Implemented in the
  `FleetTask` controller and scheduler; specified in RFC-0001
  (`control-plane`, `crds/fleettask`, `fleet-adapter-protocol`).
- **Fleet Adapter protocol: `CancelDisposition`.** `CancelTaskResult` gains a
  `disposition` field (`STOPPED_SAFELY`, `COMPLETED`, `RECOVERED`) so an adapter
  reports how a cancel resolved. Wired through the simulation adapter, the ROS 2,
  VDA5050, and MAVLink reference adapters, the adapter cookiecutter template, and
  the conformance harness (case C3.6).
- **`tools/swarmtop` — terminal fleet inspector.** A read-only Bubble Tea TUI
  (its own Go module, `github.com/swarmada/swarmtop`) that renders the `Robot`
  status fields `kubectl get` cannot column-ize: a color-coded robot list, split
  and full-screen detail (events, conditions, health, firmware and models), a
  combined tasks-and-actions view (`t`), and an adapter-health view (`a`). Supports
  `--namespace`/`--robot`; the UI is driven by a `FleetWatcher` interface and an
  in-memory reducer store, so it is unit-testable without a cluster.
- **`swarmtop`: composite `FleetTask` view.** The `t` screen shows both shapes at
  once. Every row states its `KIND` (`task` or `action`) and, for an action, the
  `TASK` that owns it or `—`; composites list first with their members nested,
  and actions no task owns follow. A task row carries its action summary, desired
  state, and the member currently executing; `enter` opens completion and failure
  policy, every member from `status.actions[]`, and conditions. Grouping reads the
  `swarmada.io/fleettask` label the composite controller stamps on each generated
  child, so no cross-resource join is needed. A member row and a standalone action
  row are produced by the same renderer and differ only in what the view struct
  carries, so a `FleetTask` holding one action reads the same as that action
  authored standalone in every lifecycle column. With no composites present the
  screen collapses to the flat action list, with no task sections and no rows
  added or lost; the columns themselves changed, gaining `KIND` and `TASK` and
  dropping `DEADLINE` (still in the detail pane).
- **warehouse-quickstart: composite coverage and a stricter reset.** The
  `healthy-fleet` scenario now applies a single-member `FleetTask` alongside the
  two standalone actions, so both shapes appear together; every other scenario
  stays composite-free, keeping the zero-composite path exercised. The namespace
  reset now cancels and deletes leftover `FleetTask`s before the `FleetAction`s
  they own — deleting children alone is a no-op, because the controller
  regenerates them deterministically — and fails the run rather than proceeding
  against state a previous run left behind.
- **`FleetTask` sample manifest, and a guard that keeps every sample decodable.**
  `config/samples/fleettask_sample.yaml` ships a three-member `receiving-round`
  chain — `approach-dock` → `inspect-dock` → `return-to-bay`, each gated on the
  previous by `dependsOn` — so `kubectl apply -f config/samples/` now creates a
  composite. A single-member task would never exercise `dependsOn`,
  `startCondition`, or a partial `actionSummary`, which are the behaviours that
  distinguish a composite from a standalone action. `docs/operations.md` gains a
  pointer to it, resolving a `<fleettask.yaml>` placeholder that named no file in
  the repository. `test/samples` decodes every document under `config/samples/`
  against the `api/v1` types with unknown fields rejected, and asserts the
  required fields a Go zero value would hide, so a manifest naming a field the
  API no longer has fails in CI rather than on a reader's cluster — the failure
  mode the `FleetTask`/`FleetAction` rename left undetected in other manifest
  sets. The warehouse quickstart keeps its own inline single-member composite:
  the three-member sample needs three free robots and would leave that scenario
  no idle spare.
- **Simulation adapter: reports the disposition when it honours a cancel.** Handling
  `cancel_action` now emits one stderr line —
  `CANCEL_HONOURED robot=<id> action=<name> command_id=<n> disposition=STOPPED_SAFELY`
  — after the stop, the lease release and the `CANCELLED` status emit have all
  succeeded, so the line records that the cancel was honoured rather than that one
  arrived. The disposition is read back from the value put on the wire, so the log
  cannot disagree with the `CancelActionResult` the control plane receives.
  `CancelDisposition` is the adapter's own determination that the robot reached a
  safe stop — the control plane cannot derive it, and a mishandled cancel was
  observable only as the control plane not reassigning, which is equally consistent
  with a scheduler, lease or capability fault. Nothing on the wire changed: the same
  message carries the same field, and conformance case C3.6 is unaffected. Tooling
  that parses the adapter's stderr strictly will see one additional line per cancel;
  `EDGE_ESTOP_CONFIRMED` is byte-identical and is now pinned by a test rather than by
  convention.
- **warehouse-quickstart scenarios.** The quickstart (`make quickstart`) fronts a
  scenario picker — `healthy-fleet`, `battery-edge`, `battery-handoff`,
  `hardware-fault`, `comms-flaky`, `estop-drill` — plus an advanced
  `full-surface` coverage run reachable via `--scenario full-surface`. See
  `examples/warehouse-quickstart/README.md` and `SCENARIOS.md` for what each one
  demonstrates and how to start it.

### Changed

- **ACTION REQUIRED — a namespace with no `SwarmadaConfig` now enforces artifact signature
  verification.** Previously such a namespace was treated as *verification off*: an adapter's
  conformance report was accepted unsigned and the adapter was qualified `Passed`. It now
  resolves the same way the rollout admission webhook and the firmware rollout reconciler
  already resolved it — enforcement **on**, fail closed
  ([ADR-0053](docs/adr/0053-an-absent-swarmadaconfig-enforces-signature-verification.md),
  superseding [ADR-0051](docs/adr/0051-a-namespace-is-born-with-signing-on-and-incomplete.md)
  decision 6).

  **Who is affected.** Any namespace that holds `FleetAdapter` objects and no
  `SwarmadaConfig`. Find them before upgrading:

  ```bash
  kubectl get fleetadapter -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' | sort -u
  kubectl get swarmadaconfig -A
  ```

  Every namespace in the first list that is absent from the second needs one object.

  **What you will see if you do nothing.** The adapter's `status.conformance` goes to
  `Failed` with `status.message` reading *"no SwarmadaConfig in namespace ..."*, and because
  the assignment gate requires a passing adapter, no robot in that namespace is dispatchable.

  **What to do.** Create the namespace's singleton config, choosing the policy you actually
  want. To keep behaving as before — no signature required:

  ```bash
  kubectl apply -f - <<'EOF'
  apiVersion: swarmada.io/v1
  kind: SwarmadaConfig
  metadata:
    name: swarmada-config      # the singleton name; the CRD requires it
    namespace: <namespace>
  spec:
    signing:
      requireSignatureVerification: false
  EOF
  ```

  To start verifying instead, omit that line and add a `trustRoots` entry. Either way the
  adapter re-qualifies on the next reconcile; no restart is needed.

  **Why this is not a downgrade being papered over.** A namespace with no config was not
  verifying anything — it was not saying so, and it gave two different answers
  depending on which controller asked. Writing the config makes the namespace's posture
  explicit and auditable, whichever posture you choose.

- **`swarmada-config` is now auto-created for a namespace that has `FleetAdapter` objects and
  no `FleetZone`.** The bootstrap reconciler previously triggered only on the first
  `FleetZone`. A `FleetAdapter` needs no zone, so an adapter-only namespace never received a
  config — the state the change above turns into a visible failure. New namespaces get the
  object on the first `FleetZone` **or** the first `FleetAdapter`; existing ones get it as
  soon as either is created or updated.

- **`hardware-fault` quickstart scenario reroutes for real.** With
  capability-loss reassignment in place, degrading the live robot's `camera_front`
  capability now triggers a genuine controller-detected safe-stop and reroute to a
  spare camera-capable robot, rather than a scripted hand-off.
- **`comms-flaky` quickstart scenario documented as a logs story.** A ControlStream
  drop/reconnect does not change any `Robot` status field: per-robot liveness is
  kept off the status write path, so `status.connectivity` is never written, and
  the plaintext reference adapter presents no verified mTLS identity for the
  `FleetAdapter` connectivity path. The scenario's watch command therefore points
  at the adapter and manager logs (lease self-stop, fencing-token survival on
  reconnect) instead of at swarmtop.
- **swarmtop connectivity indicator reads `status.phase`.** Robot liveness is
  shown from the phase the control plane actually writes, not the unwritten
  `status.connectivity`, so the display never implies a signal the control plane
  does not produce.

### Conformance catalog

The `C0`–`C16` catalog in [`adapters/CONFORMANCE.md`](adapters/CONFORMANCE.md) gained checks
and disclosures. A vendor at contract `1.0.0` is not obliged to re-run against these (see the
contract note above), but the results are worth reading:

- **`C2.4` (MUST NOT), new.** An adapter must **re-register** on reconnect and must never
  re-discover. The harness answers a redial `discover` with `ALREADY_EXISTS` and keeps
  waiting for the `register` that must follow, so an adapter that corrects itself still
  reaches the reconnect checks and the row records that it had to.
- **`C3.7` (MUST), new and recorded `skip`.** The high-water fencing token must survive an
  adapter **process restart**, not only a stream reconnect. The suite spawns the adapter once
  and never restarts it, so this half is not verified — recorded explicitly rather than left
  to a reader comparing the catalog against the harness. No reference adapter persists the
  mark today.
- **`C3.8`, new.** The adapter must **adopt** `RegisterAck.authoritative_action_state.fencing_token`
  as its high-water mark. `fleet_adapter.proto` states the obligation on the field itself, and
  no check tested it: every other `C3` check raises the mark from an `AssignAction` the adapter
  itself accepted, so nothing read the control plane's own statement of the token. Graded MUST
  at the attested contract version and advisory at N-1.
- **Two disclosures**, because the code cannot earn what the catalog claimed: `C4.2`'s
  confirmed-versus-reported limit, and `C7.2`'s trust-root leg. Neither weakens a MUST.
- **`C11` and `C12` headings corrected.** Both said "(when implemented)" over bodies stating
  that declining is **not** conformant. `pause`/`resume` and `scan` are required commands;
  the headings now match the bodies.

### Fixed

- **Emergency stop: a clear issued before its trigger no longer releases a live stop.**
  `internal/safety/dispatcher.go` merged estop state with no lock, fence, or epoch, so a
  `ClearEstop` racing a trigger could write `Normal` from a stale base and release a stop
  that had been commanded moments before. Estop events now carry a monotonic per-robot epoch: a clear
  older than the most recent trigger is refused, a stop is always honoured even on a stale
  epoch, and a genuinely-latest clear is never dropped. Proved by five ordering tests in
  `internal/safety/estop_ordering_test.go`, all passing under `-race`.
- **Fencing-token adoption in the adapter SDK.** `swarmada_sdk.safety.FenceGuard` gains
  `adopt()`, which raises the per-robot high-water mark from the control plane's own
  authoritative token rather than only from assignments the adapter accepted. It is
  monotonic — adopting a value at or below the current mark is a no-op — so a delayed or
  replayed `RegisterAck` cannot lower the mark and re-open a token already refused. Without
  it, an adapter that survived a control-plane failover kept honouring commands the previous
  plane had authorised. Wired into the ROS 2, VDA5050, and MAVLink reference adapters and
  covered by `C3.8`.
- **Simulation adapter: resilient ControlStream single-session path.** An
  unexpected transport drop (for example a demo port-forward blip or a manager
  restart) on the non-`comms.drop` path previously surfaced as an uncaught gRPC
  traceback and terminated the adapter. It now logs and re-opens the stream with a
  fresh Hello/Register, mirroring the existing SafetyStream reconnect behavior. The
  dedicated `comms-flaky` scenario-drop path is unchanged, and the conformance
  stub server (which never drops mid-run) is behaviorally unaffected.
- **Simulation adapter: resilient SafetyStream.** A dropped SafetyStream now logs
  and reconnects rather than raising an uncaught error, keeping an estop path live
  across transient connectivity loss.

[Unreleased]: https://github.com/swarmada/swarmada/compare/rfc-0001-v0.4...HEAD
[RFC-0001 v0.4]: https://github.com/swarmada/swarmada/releases/tag/rfc-0001-v0.4
