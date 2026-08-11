# Changelog

All notable changes to Swarmada are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project is
pre-release and does not yet cut versioned releases, so changes accumulate under
**Unreleased** until the first tag.

Each released section also records the **spec version** it implements and which RFCs
were **added** or **amended**; the version increment follows
[docs/release-process.md](docs/release-process.md#versioning).

## [Unreleased]

### Added

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
- **warehouse-quickstart scenarios.** The quickstart (`make quickstart`) fronts a
  scenario picker — `healthy-fleet`, `battery-edge`, `battery-handoff`,
  `hardware-fault`, `comms-flaky`, `estop-drill` — plus an advanced
  `full-surface` coverage run reachable via `--scenario full-surface`. See
  `examples/warehouse-quickstart/README.md` and `SCENARIOS.md` for what each one
  demonstrates and how to start it.

### Changed

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

### Fixed

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

[Unreleased]: https://github.com/swarmada/swarmada/commits/main
