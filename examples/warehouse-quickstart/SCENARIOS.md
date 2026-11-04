# warehouse-quickstart — scenario reference

Reference for each scenario `examples/warehouse-quickstart/run.sh` offers: what each one is meant
to prove, and what is real and what is simulated in it. The source of truth for the behaviour is
`run.sh` plus `adapters/scenarios/presets/*.yaml`; if you change a scenario's behaviour, update its
section here in the same change.

## At a glance

| # | Scenario | Picker text | LIVE? | Adapter preset used | Fleet-wide "real" | What's simulated |
|---|----------|-------------|-------|----------------------|--------------------|-------------------|
| 1 | `healthy-fleet` | 3 robots, nothing goes wrong (default) | Yes | `healthy-fleet` | `sim-robot-001`'s connection, telemetry, assignment and completion; control plane, CRDs, scheduler | `sim-robot-002`/`003`'s readiness (patched); they report no position, so they get no work |
| 2 | `battery-edge` | a robot's battery drains while it keeps its task | Yes | `battery-edge` | `sim-robot-001`'s connection, registration, telemetry, battery drain | `sim-robot-002`/`003`'s readiness + battery (`85`, patched) |
| 3 | `battery-handoff` | a working robot's battery drops mid-task; watch its task get safely handed off | Yes | `battery-edge` (reused) for `sim-robot-001`, `healthy-fleet` for each spare | All three robots, each on its own live adapter; the cancel/confirm-stop/release/reassign mechanics once triggered; the spare's completion | When to hand off — no controller decides this on its own |
| 4 | `hardware-fault` | camera fails mid-task, watch it reroute and recover | Yes | `hardware-fault` for `sim-robot-001`, `healthy-fleet` for each spare | All three robots, each on its own live adapter; the fault on the adapter's clock, capability re-derivation, reroute, the spare's completion | Nothing |
| 5 | `comms-flaky` | a robot drops and reconnects | Yes | `comms-flaky` | `sim-robot-001`'s real stream drop/reconnect, fencing + lease behavior | `sim-robot-002`/`003`'s readiness + battery |
| 6 | `estop-drill` | trigger and confirm an emergency stop | Yes | `estop-drill` | `sim-robot-001`'s confirmed estop via `safety.confirm_estop` (ground truth, never inferred) | `sim-robot-002`/`003`'s readiness + battery |
| 7 | clean everything | tear down the cluster, kill any live processes, and exit | — | — | — | — |

"LIVE?" — whether the scenario drives `sim-robot-001` with a real, running `sim_adapter.py` process
against the real ControlStream. Every scenario, `healthy-fleet` included, is LIVE unless
`--projected` selects the status-projected path, which runs no adapter and writes robot
readiness with `kubectl patch`. `battery-handoff` and `hardware-fault` drive every robot with a
live adapter, each under its own `FleetAdapter` identity, and deploy via
`config/overlays/demo-ab-live` (`quickstart-mtls` plus the spares' client certificates). Every
LIVE scenario deploys via `config/overlays/quickstart-mtls` (DEV/DEMO ONLY — self-signed
certificates, admission webhooks off; per-robot authorization on, against the `simbot-v1` class
the sample robots carry and `sim-fleet-adapter` serves). CI runs
`make quickstart-test` with no `--scenario`, so it runs `healthy-fleet`, LIVE (`CI=true` only turns
pacing off). The status-projected path (`--projected`) exits 1 at this release with `no FleetAction
was assigned`: no projected robot reports a position, so candidate filter 2 withholds every action.

Most scenarios need exactly **one** Enter press (`--pace keypress`, the LIVE default) — see
`pause_for_demo` in `run.sh`. Pauses are deliberately not scattered across steps; `battery-handoff`
briefly had a second pause mid-scenario, which turned out confusing (looked hung) and was removed.
`hardware-fault` no longer has a second pause either: its camera fault fires on the adapter's own
clock, so nothing waits for a key press between the task landing and the fault. The pause honors
`--pace timed`/`off` and `CI=true`.

---

## 1. `healthy-fleet`

What it proves: the control plane, CRDs, capability derivation and scheduler are real end to end —
the live `sim-robot-001` is assigned an action by the real `DefaultScheduler` and completes it.

- Real: `sim-robot-001`'s connection, telemetry, assignment and completion; control plane, CRD
  reconciliation, capability derivation, scheduler assignment.
- Simulated: `sim-robot-002`/`003`'s readiness (`status.phase=Idle` patched). They run no adapter and
  report no position, so their `status.currentZone` stays empty and candidate filter 2 withholds them:
  of the two sample `FleetAction`s and the runner's one-step `FleetTask`, the live robot takes one
  action and the other two stay `Pending`.
- This is the scenario CI runs (`make quickstart-test` with no `--scenario`), on the LIVE path,
  so CI guards it.

## 2. `battery-edge`

What the run shows: `sim-robot-001`'s real battery drains while it keeps its task. It takes the
delivery action and is still `InProgress` with its battery in the teens, while `sim-robot-002` and
`003` sit `Idle` at 85%. It keeps the task because it is the only candidate, not because the
scheduler compared batteries: the other two report no position (see the known limitation below).
The scheduler's battery-descending tie-break (`DefaultScheduler.SelectRobot`,
`internal/scheduler/scheduler.go`) is real, but this run does not exercise it; `battery-handoff` is
the scenario that stages that comparison.

- Real: `sim-robot-001`'s connection, Hello/Register handshake, and telemetry — its battery
  genuinely starts at 22% and drains to 0 (`battery-edge.yaml`: `drain_per_second: 1.0`).
- Simulated: `sim-robot-002`/`003` are patched to `Idle` + `batteryPercent: 85` directly — a healthy
  baseline so the contrast against `sim-robot-001` is real, not an artifact of the other two
  reporting no battery at all.
- **Known limitation**: `sim-robot-002`/`003` report no position, so candidate filter 2 withholds
  them from every action in the zone and `sim-robot-001` is the only candidate — this run does not
  stage a live battery comparison between robots. (A staged version was built and reverted
  in this repo's history — it depended on FleetAction-controller reconcile timing that turned out
  less immediate than assumed, and produced a confusing result. `battery-handoff` below is the
  scenario that actually stages a reliable, provable comparison.)
- Corrected: `adapters/scenarios/presets/battery-edge.yaml`'s comment used to claim the mechanism
  was `FleetAction.spec.minBatteryPct`-based requeueing. The field exists (scheduler candidate
  filter 4 refuses a robot below it), but it only filters new assignments; it never
  requeues an action a robot already holds, and this scenario's actions do not set it. The
  comment now says what the run shows: the draining robot keeps its task as the only candidate,
  and the battery-descending tie-break is not exercised.

## 3. `battery-handoff`

An earlier test against a `kind` cluster that had been reused across many manual re-runs showed `sim-robot-002` still carrying a stale `status.assignedTask` after the hand-off, and
the real reassignment landing on the low-battery live robot instead of the intended spare — looked
like a genuine double-assignment bug. Rerunning `--clean` (fresh cluster) then the scenario once
produced the correct result: hand-off landed on the healthy spare robot, no stale state on the
released one. Root cause not fully isolated, but not reproducing on a clean cluster is strong
evidence it was leftover local state, not a control-plane bug. **If this resurfaces, always retest
on a fresh cluster (`run.sh --clean` first) before treating it as a real bug** — `reset_namespace_state()`
only deletes `Robots`/`FleetActions` between runs, not the whole cluster.

What it proves: a task can be safely moved off a robot mid-execution and onto a healthy one, using
the real confirmed-stop discipline (the single-executor guarantee) — not an instant "unreachable ⇒
reassign" shortcut, which the codebase deliberately avoids as a double-execution hazard.

- Real, once triggered: setting the `swarmada.io/requeue-requested` annotation on a `FleetAction`
  (`internal/controller/fleetaction_controller.go`'s `handleRequeue`) is the exact mechanism
  `ZoneMaintenance`'s Immediate mode uses in production — it pushes a real `cancel_task` down the
  holder's live ControlStream, waits for a **confirmed** stop (the sim adapter acks
  `STOPPED_SAFELY`), then releases the robot and returns the task to `Pending` for real
  re-scheduling. The scheduler sorts eligible robots by battery, descending, so the task goes to a
  spare rather than back to the drained robot, and the spare completes it (its adapter reports
  `SUCCEEDED` on arrival).
- Script-decided: **when** to hand off. There is no controller anywhere that watches battery and
  decides to trigger a hand-off on its own — that would be new product work (a real "battery below
  X while InProgress → auto-requeue" feature), not built here. No battery value is written: every
  one shown comes from an adapter.
- `sim-robot-001` runs the real `battery-edge` preset (there's no `battery-handoff.yaml` —
  `sim_adapter.py` hard-errors on an unknown `--scenario` name). It comes online alone, so it is the
  only robot that can take the task, and its reported battery drains toward 0 while it drives.
- Setup: only `deliver-pallet-001` is applied (`apply_fleet`, scoped to this scenario), sent to the
  far corner of the zone so it is still driving when the hand-off is requested;
  `inspect-receiving-dock` is intentionally never applied. Once `sim-robot-001` holds the task,
  `sim-robot-002`/`003` come online, each on its own live adapter, and wait Idle to receive it.
- Timing: the hand-off itself takes a few seconds (a confirmed stop over a live wire, then a
  reassignment); the spare then drives the route, about 32 s. `handle_battery_handoff()` blocks
  and `fail()`s loudly (with diagnostics) if the reassignment or the spare's completion doesn't
  happen in its window — this is treated as a genuine failure, not a soft warning.

## 4. `hardware-fault`

What it proves: degrading `sim-robot-001`'s `camera_front` *capability* triggers **Capability-loss
reassignment** (RFC-0001): the control plane safely stops the in-flight camera task and reroutes it
to a spare camera-capable robot.

- Real: the capability re-derivation and the automatic in-flight reroute — a real `cancel_task` over
  `sim-robot-001`'s **live** ControlStream (the sim adapter acks `STOPPED_SAFELY`), requeue, and
  reassignment. The reroute is **not** script-triggered: the FleetAction controller detects the
  capability loss itself (unlike `battery-handoff`, which the script triggers with a
  `requeue-requested` annotation).
- Real too: the fault. `sim-robot-001`'s adapter reports `camera_front` HEALTHY (→ capability
  Active, task lands) and then, about 20 s after it starts (`--fault-at 20`), DEGRADED (→ capability
  Degraded, reroute). The task is sent to the far corner of the zone, so it is still driving when
  the fault fires. The spare runs its own live adapter and completes the rerouted task.
- Deterministic setup, all in `run.sh` (`config/samples/demo_a.yaml` untouched): only the camera task
  (`inspect-receiving-dock`) is applied (`deliver-pallet-001` held back); `sim-robot-002`/`003` are
  stripped to nav-only so the camera task can only land on the live robot; `sim-robot-001`'s
  `camera_front` capability is gated on a `camera_front` hardware component so a Degraded hardware
  status degrades the *capability* the scheduler reads (the sample's ungated capability would not);
  and `handle_hardware_fault` re-adds `camera_front` to the idle `sim-robot-002` as the reroute target.
- Simulated: nothing. `sim-robot-002`/`003` are live here, each on its own adapter.
- **Best watched with swarmtop.** The fault/reroute/recover arc lives in `status.hardware[]`/
  `status.capabilities[]` and the task's `assignedRobot`, which `kubectl get` can't column-ize — see
  "Companion tool: swarmtop" below. `run.sh` prints a `swarmtop -n warehouse-a --robot sim-robot-001`
  command for a second terminal before the pause, falling back to a `kubectl -o json | jq` one-liner
  when `swarmtop` isn't built.

## 5. `comms-flaky`

What it proves: a full ControlStream drop and reconnect (`comms-flaky.yaml`: drop at T+20s,
reconnect at T+35s) exercises the real Hello/Register reconnect path, fencing-token staleness
rejection (C3), and lease self-stop during the outage (C4).

`run.sh`'s `handle_comms_flaky` holds the run open across the whole outage and asserts both
transitions. Before it existed the scenario silently never happened: nothing kept the run alive to
T+20s, so the adapter was torn down first and its log came back empty while the generic end-state
checks still passed.

- Real: the actual stream teardown/reconnect, fencing/lease behavior during the gap.
- Simulated: `sim-robot-002`/`003` readiness + battery, as usual.
- This is explicitly called out in the preset's own comment as "the least pretty demo, the most
  relevant for a distributed-systems reviewer" — worth keeping that framing when presenting it.
- **The adapter-health view moves.** `FleetAdapter.status.phase` flips
  `Connected → Disconnected → Connected` across the outage, so swarmtop's adapter-health (`a`)
  view shows it. That path is keyed on a *verified mTLS* identity, which the LIVE quickstart
  presents (`config/overlays/quickstart-mtls`).
  The **robot** views do not flip. The outage (15 s) is shorter than the namespace offline
  threshold (`spec.health.connectivityOfflineThresholdSeconds`, 30 s by default), so the Robot
  reconciler's `Offline` path does not fire, and `status.connectivity.lastSeenAt` is written at
  most once per half that threshold, so it does not show the drop either. The rest of the payoff
  is in the **adapter + manager logs**: the stream drops, the adapter self-stops its task on lease
  expiry (C4), reconnects with a fresh Hello/Register, and its fencing token + lease survive (C3).
  `scenario_watch_command()` therefore points comms-flaky at `tail -f /tmp/swq-…-adapter.log`
  (and `kubectl logs -n system deploy/swarmada-controller-manager -f`), not at swarmtop.

## 6. `estop-drill`

What it proves: a robot that is **actually holding work** is emergency-stopped, CONFIRMS the stop
itself (`safety.confirm_estop` — simulator ground truth, never inferred) reported over the
`SafetyStream` as an `EstopAck`, and the control plane pauses the action it was executing
(RFC-0001 #safety-fleetaction-behaviour-during-estop — estop takes precedence over the lease).

The drill is driven by the `swarmada.io/estop-triggered` annotation (the operator-facing path,
`internal/controller/robotestop_controller.go`) once a FleetAction has landed on `sim-robot-001`,
**not** by the preset's `estop.trigger: at_seconds` timer. The timer fires 20s after the adapter
starts, which races the scheduler — it would fire before any task landed (proving nothing) or after
the runner had already torn the adapter down. `run.sh`'s `handle_estop_drill` waits for the task,
triggers, then asserts both the confirmed stop and the paused action; `assert_end_state` re-checks
the stop at the end, so a drill that silently never ran cannot report success.

- Real: the confirmed stop itself and the `EstopAck` over the real `SafetyStream`.
- Simulated: `sim-robot-002`/`003` readiness + battery, as usual.
- Likely the scenario to lead with for a healthcare/logistics-safety-focused audience per the
  preset's own comment.
- **Resilience fix (2026-07-11):** the estop rides the `SafetyStream`, but the `ControlStream`'s
  single-session path (`sim_adapter.py run_control_stream`, the non-`comms.drop` branch) previously
  had no error handling, so an *unexpected* transport drop mid-scenario (a demo port-forward blip or
  a manager restart) crashed the adapter with a raw `grpc._channel._MultiThreadedRendezvous`
  traceback. It now logs `control stream dropped (…); reconnecting` and re-opens with a fresh
  Hello/Register — mirroring the existing `run_safety_stream` resilience — so the drill survives a
  blip instead of aborting. comms-flaky's dedicated scenario-drop path is untouched; the conformance
  stub server never drops mid-run, so that path is behaviourally unchanged for conformance.

## 7. Clean everything (`--clean` / picker option 7)

Not a fleet scenario — tears down the `kind` cluster, kills any live/stale port-forward or adapter
processes this script tracked, removes local pidfile/log state (`/tmp/swq-*`), and exits without
bringing anything up. Reminder to run this (or `kind delete cluster`) is printed on every exit path
(success, failure, or Ctrl-C) unless `--clean` itself was what last ran.

---

## Cross-cutting gaps to keep in view

These apply to every LIVE scenario, not only one:

- **A live robot leaves `Discovered` on its own.** The Robot reconciler advances a live robot from
  `Discovered` to `Idle` (or `InProgress` when it holds an action) once fresh liveness arrives
  (`internal/controller/robot_controller.go`, `steadyPhaseForLive`); `run.sh` waits for that
  transition and does not patch it.
- **`sim-robot-002`/`003` are live only in `battery-handoff` and `hardware-fault`.** Everywhere
  else their "healthy baseline" is patched status, with no adapter behind it. Those two run one adapter process per robot,
  each under its own `FleetAdapter` identity, over the one port-forward.
- **Timeouts are simulator-tuned.** `QUICKSTART_READY_TIMEOUT`/`QUICKSTART_ASSIGN_TIMEOUT` default
  to 15s each — fine for an all-simulated fleet, too tight for real hardware. `battery-handoff`'s
  own internal wait (up to 40s) is intentionally separate from these.
- **`kubectl get robots` can't show hardware/capability/estop state.** See
  "Watching fields `kubectl get` can't show" below — affects `hardware-fault` and `estop-drill`
  most. (`comms-flaky` is deliberately NOT in this set: its payoff is a logs story, not a status
  field — see §5.)

## Watching fields `kubectl get` can't show

`Robot`'s printer columns are Phase, Zone, Battery, Action and Age, plus Class with `-o wide`
(`api/v1/robot_types.go`) — `status.hardware[]`, `status.capabilities[]`, and
`status.estopState` aren't columns, so plain `kubectl get robots` can't surface
them at all. This matters most for `hardware-fault` (§4): its entire payoff —
camera degrades → `perception.camera` capability degrades → task reroutes →
camera recovers → capability recovers — happens entirely inside
`status.hardware[]`/`status.capabilities[]`, invisible without `-o yaml`/`-o
jsonpath` mid-demo. `estop-drill` (§6, `status.estopState`) has the same problem,
only less central to its story. `comms-flaky` (§5) does NOT belong here: apart
from the `FleetAdapter` phase, its payoff does not reach a status field. The one
per-robot liveness field, `status.connectivity.lastSeenAt`, is written at most
once per half the namespace's offline threshold, so it does not show the drop
and reconnect; it's a logs story, watched via the adapter/manager logs.
`battery-edge`/`battery-handoff` don't need this either — `Battery` is already a
real printer column.

## Companion tool: swarmtop

[`tools/swarmtop`](../../tools/swarmtop/README.md) is the terminal fleet
inspector (Go + Bubble Tea) that renders these hidden fields as a live,
color-coded robot list with split (`s`) and full-detail (`enter`) views, plus
FleetAction (`t`) and adapter-health (`a`) views. Build it once with
`make -C tools/swarmtop build` (or `make swarmtop` from the repo root).

`run.sh`'s `scenario_watch_command()` (called right before the one deliberate
pause, printed as "optional — open a second terminal and run:") uses it for the
scenarios whose payoff isn't a printer column — `hardware-fault`, `estop-drill`,
and `full-surface` — printing `swarmtop -n warehouse-a --robot sim-robot-001`,
which opens straight into that robot's detail view. If no `swarmtop` binary is
on `PATH` or built in-tree, each of those falls back automatically to its
field-specific `kubectl -o json | jq` one-liner; the remaining scenarios stay on
plain `watch kubectl get robots`. `comms-flaky` is intentionally excluded — it's
a logs story (§5), so its watch command is `tail -f` on the adapter + manager
logs, not swarmtop. swarmtop is therefore strictly optional — never a hard
dependency.
