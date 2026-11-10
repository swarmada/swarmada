# Adapter Registry

The registry of Fleet Adapters known to the project: where each one lives, who
maintains it, which contract version it targets, and its conformance status. It
exists so a bug can be routed to the right repository and so fleet-wide adapter
health is legible at a glance.

Listing is **factual**: an entry records a conformance result against a contract
version. It is not a certification or an endorsement — the project publishes the
neutral [conformance suite](CONFORMANCE.md) and this matrix, and runs no
certification program (see [ADR-0005](../docs/adr/0005-reference-adapter-policy.md)).

## How placement works

Per [ADR-0005](../docs/adr/0005-reference-adapter-policy.md):

- **In-tree** (`adapters/<name>/`): the reference skeleton/template, and a
  simulation adapter used as test infrastructure.
- **Own repository**: every functional adapter lives in its own repository and is
  tracked here by a Registry row — for both open, vendor-neutral interfaces and
  vendor-specific adapters.

Scaffold your adapter from the [`adapters/template/`](template/) cookiecutter — it
generates an adapter wired to the shared `swarmada-sdk` safety primitives (C3/C4/C5)
that you own from commit one, redials a dropped stream with bounded backoff, and
declares the current contract version. Run unmodified with its simulated binding at
suite `2026.09.25`, a generated adapter fails no check, and its profiled verdict is
`not-established`: three required rows are measured for no adapter, and one needs a
motion witness the simulated binding cannot provide
([what the harness reports](template/README.md#what-the-harness-reports-for-a-generated-adapter)).
Nothing re-runs that measurement when the template or the suite changes, and the
template has no row below. Your adapter's result is the one you earn against your own
binding.

## Conformance status legend

| Value | Meaning |
| :---- | :------ |
| `passing` | Passes the conformance suite for the listed contract version. |
| `partial` | Implements the required (safety-critical) subset; some optional areas unverified. |
| `wip` | Under development; not yet conformant. |
| `template` | A skeleton/example, not a usable adapter. |

**The value is a record for readers, and the control plane does not read it.** It does not set
`FleetAdapter.status.conformance`. That field is set from the conformance report the `FleetAdapter`
references in `spec.conformanceReport`: it is `Passed` when the report's digest verifies, the
report's `conformant` field is `true`, and — where the namespace requires it — the report's
signature verifies. The harness writes `conformant: true` when no MUST or MUST NOT check failed
among the checks that ran (`adapters/conformance/report.py`). Declining an optional command fails
no MUST, so a `partial` row can stand beside a report that yields `Passed`; the `simulation` row
below is one. Nothing in the control plane gates admission on optional coverage.

Robot admission requires `status.conformance: Passed` **and** a result earned against a contract
version inside the supported window
([admission gating](../rfcs/dist/RFC-0001-core-spec.md), §9.1.13 *FleetAdapter*). For the VDA5050 and MAVLink rows
below, it is the second condition — contract `1.0.0`, outside the window — that refuses their
robots, not the grade.

Every conforming adapter — whatever its status — must satisfy the safety-critical
MUSTs (confirmed estop, fencing-token ordering, assignment-lease self-stop);
"partial" refers only to *optional* commands declined via `unsupported = true`.

## Registry

The **Contract version** column carries the fleet-adapter contract version (semver, e.g.
`1.0.0`) that the row's conformance result was earned against, with the wire-package
identity in parentheses (`fleet_adapter.v1`). The semver is what compatibility is
judged on. A **major** bump requires re-running the suite and updating the row;
minor/patch bumps do not, until a minor takes the row's contract version out of the supported
range. A change that binds adapters for safety does that at once.

> **A ROW OUTSIDE THE WINDOW STAYS VALID, AND ITS ADAPTER CANNOT REGISTER ROBOTS.** The control plane
> implements contract `1.4.0` and accepts **N and N-1 minor**: the range is `>=1.3.0 <1.5.0` at
> that contract. A row reading **`1.2.0` or below is out of range**: that
> adapter keeps its session, keeps streaming telemetry and keeps receiving emergency stop, and is
> refused `RegisterRobot` / `DiscoverRobot` with `REGISTRATION_REJECTION_VERSION_MISMATCH`. Its
> conformance result is untouched and its row stays valid — and it cannot bring a robot under
> management. ADR-0032's original "patch and minor releases never break an adapter" is struck; see
> that ADR's Amendment 1 §A1.2. **Read the row against the current window, not against the latest
> number.**

> **Suite revision.** A row records two versions, never one: the **contract version** the result was
> earned against (the column below) and the **suite revision** that produced it — this page is
> written against **`2026.11.09`, C0–C19, 71 automated checks** — named in the **Conformance** cell. The suite
> revision is recorded alongside the contract version and is never folded into it: a suite change
> re-attests adapters, it does not move the contract they negotiate
> ([ADR-0032](../docs/adr/0032-fleet-adapter-contract-versioning-and-conformance-gating.md) binds a
> result to a contract version; this is the provenance record beside it). The authoritative value is
> `revision:` in [`../conformance/profiles.yaml`](../conformance/profiles.yaml).
>
> **Each revision has a declared class**, listed under `revisions:` in the same file. A `harness`
> revision changes what an implementation must do to pass — a check's logic, the set of checks, a
> check's level, or the rule that turns rows into a verdict — and a result earned before it must be
> re-earned. A `profile-layer` revision changes only how results are grouped, named or reported, and a
> result earned before it carries forward. Each **Conformance** cell names the revision its result was
> earned at. A row is **stale** when a `harness` revision later than that one is at or below the
> current revision; otherwise it is current. A stale row is unverified at the current revision, not
> failed.
>
> The catalogue in [`CONFORMANCE.md`](CONFORMANCE.md) lists one further check, **C8.4**, which the
> harness does not run: it is an obligation on the *edge node*, not on an adapter, so no adapter
> result can pass or fail it. 71 is the number a `make conformance` run reports.
>
> The revision, the family range and the check count above are pinned by
> `tests/python/test_registry_suite_revision.py`, which `make test` runs: it fails when they
> disagree with `revision:` in `profiles.yaml` or with the check ids the harness records.

> **`2026.11.09` is `harness`: a result earned before it is re-earned.** `C2.1` plays first contact
> with a robot that has no `Robot` yet: the first `register` is refused `NOT_ADMITTED`, the adapter
> must announce the robot with a `discover`, and the register of its provisioning loop's next
> iteration is accepted as the admission. Before it, the harness played a control plane that already
> held the `Robot`, so `C2.1` passed an adapter that never announces a robot. `C5.8` records
> `adapter-declares-no-action` for an adapter that serves no action, where the run ended without a
> report, and is graded in `dispatch` Core, not `safety` Core. The check set and the levels did not
> change. The `simulation` and `fleet-adapter-ros2` rows below are re-earned at it; every other row
> stays stale.

> **`2026.10.22.2` is `harness`: a result earned before it is re-earned.** An assignment for a
> `Custom` entry keyed by `custom_type` carries that `custom_type`, as a control plane sets it. An
> adapter that keys its `Custom` verbs was refused every assignment as naming no verb; now `C4.2`,
> `C3.9` and `C16.1` reach it, and `C3.4`, `C3.5`, `C5.8`, `C17.1`, `C17.2` and `C19.1`–`C19.3`
> grade what they probe. `C2.2` grades the terminal report for the action the `RegisterAck`
> declares `CANCELLED`, any state RFC-0001 calls terminal, and never counts `ACTION_STATE_STOPPED`.
> The check set and the levels did not change. The `simulation` row below is re-earned at it;
> every other row stays stale.

> **`2026.10.22` is `harness`: a result earned before it is re-earned.** No row reads
> `ACTION_STATE_STOPPED`, the commanded stop, as an end. `C4.2` grades the lapsed lease's terminal
> report, any state RFC-0001 calls terminal, and fails an adapter that sends only `STOPPED`;
> `C16.1`'s terminal half and `C3.9`'s wait for the preemptor's end read the report after a
> `STOPPED`; `C3.6` accepts `CANCEL_DISPOSITION_STOP_UNCONFIRMED`. The check set and the levels did
> not change. No row below is re-earned at it, so every row is stale.

> **`2026.10.17` is `harness`: a result earned before it is re-earned.** The harness negotiates
> the contract version per connection, as a control plane does: `1.4.0` with an adapter that
> reports a `1.4` version, and the reported `1.3.x` otherwise. A connection held to `1.3.x` is sent
> every stop and no `AssignAction`, so each row that needs an assignment records
> `contract-predates-obligation` there, and the rules that bind every adapter are still graded: a
> `1.3.x` adapter that does not answer a stop fails. Before it, every compatible adapter was told
> `1.4.0`, and no row ran on a `1.3.x` connection. The check set and the levels did not change, and
> a `1.4.0` run grades as before. The `simulation` row below is re-earned at it; every other row
> stays stale.

> **`2026.10.15` is `harness`: a result earned before it is re-earned.** It adds `C5.8` (MUST at
> contract `1.4.0`, `safety` Core): a stop holds the robot's work until its `EstopClear` lifts it,
> and the lift is acknowledged. An adapter that reports `1.4.0` and keeps no latch now fails. The
> harness meets the stop latch: on a `1.4.0` connection it sends each stop's fence and clears
> every stop it issues before the next check gives the robot work, it reads each stop's answer by
> its stop ID, and it runs `C8.3` last. An adapter that keeps the latch no longer fails `C5.2`,
> `C5.4`, `C17.1`, `C17.2`, `C3.8`, `C3.1` and `C14.1`. It adds `C19.5` (MUST, `dispatch` Core):
> a `Custom` action with no `custom_type` is accepted when its payload's `operation` names a
> declared verb. The `simulation` adapter reports `1.3.0` unless run with `--latch`. The
> `simulation` row below is re-earned at it; every other row stays stale.

> **`2026.10.13` is `harness`: a result earned before it is re-earned.** `CE6.1`–`CE6.5` leave the
> ComputeEndpoint `events` profile and `CP2.3`–`CP2.5` leave the ControlPlane `endpoint-dispatch`
> profile, so the pass bar moved on those two surfaces. The FleetAdapter harness's check set (69
> checks, C0–C19) and levels are unchanged, and the contract version did not move. No row below has
> been re-run at it, so the `simulation` row, current at `2026.09.29`, is now stale like the rest.
> Re-running the rows at it is outstanding.

> **`2026.09.29` is `harness`: a result earned before it is re-earned.** It adds `C3.9` (MUST,
> `dispatch` Core): after an in-place preemption, a `cancel_action` naming the **preempted**
> action — at its own token and at the mark — must leave the preemptor running, and the
> preemptor must then succeed. No row had sent a cancel naming anything but the running action,
> so an adapter that stops whatever is running on any cancel passed every row while aborting
> every in-place preemptor. To make room below the `C3.8` adoption probe, `C17`'s probes and
> the `C3.8` fixture move to higher tokens; neither row's logic changed. The `simulation` row
> below is re-earned at it; every other row stays stale.

> **`2026.09.28` is `harness`: a result earned before it is re-earned.** `C8.3` now grades the
> terminal `EstopAck` on the EdgeStream, skipping a `STOPPING` receipt as the SafetyStream checks
> already did, so the two-phase answer `C5.2` requires no longer fails it. No check was added or
> removed and no level moved. Every row below was already stale, and each stays stale until it is
> re-run. From this revision the harness writes `suite_revision` into its JSON report.

> **`2026.09.25` is `profile-layer`: every result carries forward across it, and no row below
> changes state.** It changes what a report *lists*, not what any row measures. A check that
> returns early because the adapter never reached the state it needs — no redial, no registered
> robot, no EdgeStream — now records every row that branch owns, with
> `adapter-did-not-reach-state`, where before it left some out, and the profile layer graded an
> absent Core row a suite gap. An adapter that never redials therefore now reads `C2.3` and `C3.1`
> as `not-testable-here` rather than as suite gaps; both still prevent a `pass`. `C0.1`'s pass
> line now says how many recorded causes it can contradict. No outcome, check, level or verdict
> rule moved.

> **`2026.09.24` re-earns every result: no row has been re-run at it, so every row below is
> stale.** `2026.09.24` is `harness`-class, and the check set and levels are unchanged. What
> moved is one row's grade. An adapter that accepts a firmware install and emits no advisory
> progress does what `C10.1` (MAY) permits: "emitting none is conformant". The harness recorded
> that row as `optional-declined`, a cause meaning the adapter declined the command, and `C0.1`
> (MUST) correctly contradicted a decline the run never observed, so the adapter was graded
> NON-CONFORMANT. `C10.1` now records `optional-not-exhibited`. The row is still a skip and not a
> pass, because a `C10.1` pass reports the update-progress feature as supported. `C10.2` still
> requires the terminal report. A result earned before this revision could be NON-CONFORMANT for
> this reason alone. Neither in-tree adapter takes that path (the simulation adapter refuses the
> unverifiable probe artifact and the reference skeleton declines the command), so both would
> grade identically at `2026.09.24`, and both are marked stale for the reason given under
> `2026.09.22` below. The harness also now binds its port exclusively and serves one adapter per
> run. That changes no row of an isolated run; it stops two concurrent runs on one port from
> grading each other's adapters.

> **`2026.09.22` re-earned every result: no row has been re-run at it, so every row below is
> stale.** `2026.09.22` is `harness`-class, and the check set is unchanged — what moved is the rule
> that turns rows into a verdict. ADR-0071's platform precondition already re-graded `C5.3` from a
> plant declaration whose odometry is all `command-derived`; it now re-grades `C5.2`, `C8.3`,
> `C14.1` and `C3.6` the same way, `not-supported-by-platform` with cause
> `no-independent-motion-channel`. The cause stays outside the never-an-obligation set, so a run on
> such a platform still cannot reach `pass`; what changes is that the rows are attributed to the
> platform rather than to the adapter. `C16.1` is deliberately not in the set: a cancel that emits
> no terminal `ActionStatusUpdate` is an adapter gap whatever the drivetrain can witness. Every
> report also now states, in its text and its JSON, the one limitation of the suite's fixture: goals
> needing a reversal longer than the fixture's dwell are pre-empted before the reversal completes,
> so motion rows attest one-directional travel only.
>
> A plant declaration is the only thing that triggers the re-grading, and neither in-tree row is
> profiled with one, so both would grade identically at `2026.09.22`. They are marked **stale** all
> the same: the rule says a `harness` revision re-earns a result, and a row that reasons its way out
> of the rule is a row a reader has to take on trust.

> **`2026.09.19.4` re-earned every result before it: the two in-tree rows were re-run at it, and the
> three external rows were stale.** `2026.09.19.4` is `harness`-class. Two rows are new, each paired with
> an acceptance control: `C19.3` (an assignment that states no action type is declined) and `C19.4`
> (a declared enum parameter given a value outside its allowed values is declined; not applicable to
> an adapter that declares no enum parameter). `C5.7` now allows for the gap between the witness's
> samples at the rest where it is wider than the witness's declared resolution, so a sparse witness
> no longer fails a timely `STOPPED`.
>
> `2026.09.19.3` is `harness`-class too. Every assignment the harness
> sends now carries an action type the adapter declared, with the parameters that type requires,
> and four rows are new: `C19.1` and `C19.2` (an undeclared action type, or an assignment
> missing a declared required parameter, is declined — each paired with an acceptance control),
> `C12.2` (the snapshot pushed at registration carries the `scan` reply's catalog) and `C5.7` (an
> early `STOPPED` fails against a motion witness; the simulation adapter's plant model is the
> witness in a simulated run).
>
> `2026.09.19.2` is `harness`-class too. `C11.1` is graded at its catalog level:
> pause and resume are required commands, so a declined or unanswered pause is a failed MUST, where it
> was a skip. `C6.3` (the control plane's status-write discipline) is graded on the ControlPlane surface
> as `CP8.1` and no longer holds an adapter's verdict. The harness's check ids and levels are pinned to
> the revision, so the set cannot change without a new one.
>
> `2026.09.19` is `harness`-class too: the report grades every row and states a verdict, and a required Core row that was not measured prevents `pass`
> ([ADR-0070](../docs/adr/0070-a-conformance-report-grades-each-row-and-names-its-unlock.md)). `2026.09.15`
> is `harness`-class too. The bump itself only assigned `C18.1`–`C18.5` to a profile, but those five
> checks entered the harness after `2026.09.13` without a revision of their own, so a `2026.09.13`
> result never faced them.
>
> **Verdict and fidelity are read off the profiled report** (`make conformance`, then
> `python -m conformance fleet-adapter --adapter-report …`). `fail` means a required Core row failed;
> `not-established` means none failed and at least one was not measured; the cell names which rows and why. The harness's own
> `conformant` — what the control plane reads — is recorded beside it, and means something narrower: no
> MUST failed among the checks that ran.

> **`2026.09.13` INVALIDATES EVERY RESULT EARNED BEFORE IT, AND THE CONTRACT VERSION DID NOT MOVE.**
> This revision adds the `zone-restrictions` profile to this surface — `C17.1`, `C17.2`, `C17.3` and
> `C17.4`: the obligation to **decline** an assignment carrying an operating constraint the adapter
> cannot honour rather than accept it and ignore the rule, and (`C17.4`) to **accept** one carrying
> only subjects it declared, so a decline is a function of what arrived rather than a reflex. Adding a profile changes what an adapter must do
> to pass, so a result earned against `2026.09.03` no longer carries forward and every row below is
> read against this revision or marked as not yet re-run. **No adapter renegotiates anything**: the
> contract stays `1.2.0`, and a row's **Contract version** column is untouched by a suite change.
> *(The contract has since moved to `1.3.0` for ADR-0059's `zone_constraints` arm — an additive
> proto change, so MINOR per ADR-0032 A1.1, and independent of this suite revision. The two in-tree
> rows moved `1.1.0` → `1.2.0` with it, which is the version they implement: they carry the
> FA-R5/FA-R6 decline that `1.2.0` added, and they decline the new OPTIONAL `zone_constraints` arm
> with `unsupported=true`, which is conformant and is why that arm is optional. `1.2.0` is inside
> the window a `1.3.0` plane advertises — the N-1 skew working as designed. No in-tree conformance
> result was re-earned for the contract move, because a minor bump does not invalidate one; both
> were nonetheless re-run and stayed CONFORMANT.)*
> A row whose result predates a revision is not a failing row — it is an **unverified** one, and
> saying which is the whole reason two versions are recorded instead of one.

| Adapter | Robot interface | Repository | Maintained by | Contract version | Conformance |
| :------ | :-------------- | :--------- | :------------ | :--------------- | :---------- |
| `example-noop` | — (reference skeleton) | in-tree `adapters/example-noop/` | Core project | `1.3.0` (`fleet_adapter.v1`) | `template`, **stale** — the `harness`-class revisions `2026.09.22`, `2026.09.24`, `2026.09.28`, `2026.09.29`, `2026.10.13`, `2026.10.15`, `2026.10.17`, `2026.10.22`, `2026.10.22.2` and `2026.11.09` followed the run below, and the row has not been re-run at any of them (skeleton/example; registers no robot). Earned at suite `2026.09.19.4` via `make conformance`: verdict **`not-established`**, fidelity `simulated`, no row failed. It answers pause and resume (`C11.1` passes: it manages no robot and says so), declares no action type, and refuses every assignment `INVALID_ACTION` — so every row that needs an accepted assignment (`C3`, `C4`, `C16.1`, `C19`, `C6.4`) is not supported, and `C1.3`, `C3.7` (suite gaps), `C2.3`, `C3.1` (that run graded them suite gaps; the suite measures both, and the cause is that this adapter never redials, which suite `2026.09.25` records as such), `C1.4`, `C2.1`, `C2.2`, `C2.4`, `C5.7`, `C6.1`, `C6.2`, `C12.2`, `C14.1` (not testable here: it registers no robot) are not measured. Harness report: 18 passed, 0 failed, 44 skipped (`conformant: true`) |
| `simulation` | Simulator (test infrastructure) | in-tree `adapters/simulation/` | Core project | `1.3.0`; `1.4.0` with `--latch` (`fleet_adapter.v1`) | `partial`. Earned at suite `2026.11.09` (2026-11-09) by the harness on this revision, running the adapter as `make conformance-profiles` runs it: `--latch --scenario healthy-fleet`, with its plant model as the motion witness, at contract `1.4.0`. Without `--latch` it declares `1.3.0`, as the deployments in this repository run it, and the harness then gives it no work; that mode was not re-run at this revision. Verdict **`not-established`**, fidelity `simulated`, capability tier `FullControl`, no Core row failed. Four required Core rows were not measured: `C1.3`, `C3.7` (suite gaps: the harness serves plaintext and never restarts the adapter), `C1.4` (not testable here: no wire carries it), and `C19.2` (not supported: its catalog declares no parameter required, so no assignment could omit one). `C2.1` passes (its first register, refused `NOT_ADMITTED`, drew a `discover`, and the provisioning loop's next register was accepted as the admission); `C5.8` passes, graded in `dispatch` (work is refused `ROBOT_UNAVAILABLE` while a stop is latched, and taken after its `EstopClear` draws the `EstopClearAck`); `C19.5` passes (a `Custom` action with no `custom_type` whose `operation` names a declared verb is accepted); `C19.1`, `C19.3` and `C19.4` pass; `C12.2` passes; `C3.9` passes (after both cancels naming the preempted action, the preemptor still reports `running=true` and then succeeds); `C5.7` passes against its plant model (rest witnessed at +0.262s, `STOPPED` at +0.262s); `C4.2` passes on the terminal `ACTION_STATE_FAILED` that follows the lease self-stop's `STOPPED`. `safety`, `lease`, `lifecycle`, `telemetry`, `traffic`, `discovery`, `edge` and `attestation` Core measured and passing. 1 failed, `C13.2` (SHOULD, advisory). Harness report: 61 passed, 1 advisory, 9 skipped (`conformant: true`), the same per-check verdicts as the `--latch` recording pinned at this revision |
| `fleet-adapter-ros2` | ROS 2 / Nav2 | [`swarmada/fleet-adapter-ros2`](https://github.com/swarmada/fleet-adapter-ros2) | [@AlexBahel](https://github.com/AlexBahel) | `1.3.0` (`fleet_adapter.v1`) | `partial`. Earned at suite `2026.11.09` (2026-11-09) against `efe5151`, by the harness on this revision with the adapter in its default simulated binding (`--binding simulated`). It declares `1.3.0`, inside the supported `>=1.3.0 <1.5.0` window; the harness agrees `1.3.0` with it and gives it no work, so each row that needs an assignment, `C5.8` among them, records `contract-predates-obligation` (a `1.3.x` connection is sent every stop and no `AssignAction`): 26 rows. Of the other skips, 5 record `optional-declined` (optional commands declined) and 11 are not measured in this run: `adapter-declares-no-sensor-stream` (4), `not-observable-on-wire` (2), `legal-alternative-path`, `no-motion-witness`, `needs-adapter-restart`, `deployment-property` and `needs-extended-run` (1 each). `C2.1` passes (its first register, refused `NOT_ADMITTED`, drew a `discover`, and the provisioning loop's next register was accepted as the admission). Verdict **`not-established`**, fidelity `simulated`, capability tier `FullControl`. 1 failed, `C13.2` (SHOULD, advisory: an `AssignAction` sent after a `VERSION_MISMATCH` refusal is accepted; a real control plane withholds that dispatch). Harness report: 28 passed, 0 failed, 1 advisory, 42 skipped (`conformant: true`) |
| `fleet-adapter-vda5050` | VDA5050 (MQTT) — *target interface; not yet exercised* | [`swarmada/fleet-adapter-vda5050`](https://github.com/swarmada/fleet-adapter-vda5050) | [@AlexBahel](https://github.com/AlexBahel) | `1.0.0` (`fleet_adapter.v1`) | **Not qualified at the current contract — pending re-qualification.** Contract `1.0.0` is outside the supported `>=1.3.0 <1.5.0` window, so this control plane refuses its robots whatever the grade below. Last result: `partial`, **stale** — earned at suite `2026.09.03`, and the `harness`-class revisions `2026.09.13`, `2026.09.15`, `2026.09.19`, `2026.09.19.2`, `2026.09.19.3`, `2026.09.19.4`, `2026.09.22`, `2026.09.24`, `2026.09.28`, `2026.09.29`, `2026.10.13`, `2026.10.15`, `2026.10.17`, `2026.10.22`, `2026.10.22.2` and `2026.11.09` followed it, so the result does not carry forward; not re-graded here (safety-complete; CONFORMANT, in the harness's sense, vs the C0–C16 harness at that revision against a simulated binding, optional commands declined; C13.2 defence-in-depth absent — non-blocking; the `zone-restrictions` profile added at `2026.09.13` was not exercised, so `C17.1`/`C17.2`/`C17.3`/`C17.4` are **unverified**, not failed; no live MQTT/VDA5050 runtime yet) |
| `fleet-adapter-mavlink` | MAVLink / PX4 | [`swarmada/fleet-adapter-mavlink`](https://github.com/swarmada/fleet-adapter-mavlink) | [@AlexBahel](https://github.com/AlexBahel) | `1.0.0` (`fleet_adapter.v1`) | **Not qualified at the current contract — pending re-qualification.** Contract `1.0.0` is outside the supported `>=1.3.0 <1.5.0` window, so this control plane refuses its robots whatever the grade below. Last result: `partial`, **stale** — earned at suite `2026.09.03`, and the `harness`-class revisions `2026.09.13`, `2026.09.15`, `2026.09.19`, `2026.09.19.2`, `2026.09.19.3`, `2026.09.19.4`, `2026.09.22`, `2026.09.24`, `2026.09.28`, `2026.09.29`, `2026.10.13`, `2026.10.15`, `2026.10.17`, `2026.10.22`, `2026.10.22.2` and `2026.11.09` followed it, so the result does not carry forward; not re-graded here (safety-complete; CONFORMANT, in the harness's sense, vs the C0–C16 harness at that revision against a simulated binding, optional commands declined; C13.2 defence-in-depth absent — non-blocking; the `zone-restrictions` profile added at `2026.09.13` was not exercised, so `C17.1`/`C17.2`/`C17.3`/`C17.4` are **unverified**, not failed; no live MAVLink/PX4 runtime yet) |

**Privately-held adapters are not listed here, and their absence is not an omission.** An adapter
built against this contract and distributed as a compiled artifact or container image — rather than
as source — cannot carry a Registry row, because a row records a conformance result a reader can
**re-derive**: an open repository, and a suite they can re-run against it
([ADR-0007](../docs/adr/0007-conformance-self-certification.md)). A row pointing at a source tree the
reader cannot open would assert a result with no way to check it, and listing here is factual.

Such adapters are expected — every vendor-specific adapter is one — and they are conformant or not
on exactly the same terms as the rows above: the same suite revision, the same contract version, the
same safety-critical MUSTs. **A conformance report should be published with the artifact**, so the
claim travels with the thing it describes. The absence of a row means this project has no verifiable
result to publish, never that none was earned.

New reference or vendor adapters are welcome — open a pull request adding a Registry
row once the adapter passes the [conformance suite](CONFORMANCE.md).

## Adding or updating an entry

Open a pull request that edits this file:

1. Run the [conformance suite](CONFORMANCE.md) against your adapter and record the
   result together with the **contract version** it was earned against — the
   `contract_version` the harness stamps into the report (semver, e.g. `1.0.0`).
   Put that semver in the **Contract version** column, with the wire-package identity in
   parentheses: `` `1.0.0` (`fleet_adapter.v1`) ``. The suite self-certifies
   ([ADR-0007](../docs/adr/0007-conformance-self-certification.md)): this pull
   request is the attestation — no authority signs off on it.
   Re-qualification is required only on a **major** contract-version bump.
2. Add (or update) a row in **Registry** with the repository and maintainer.
3. For a vendor-specific adapter, the repository is the vendor's own; for a
   neutral reference adapter, it is a repository under the project org.
