# warehouse-quickstart — scenario reference

Working document for reviewing and updating each scenario `examples/warehouse-quickstart/run.sh`
offers. Source of truth for the *code* is `run.sh` plus `adapters/scenarios/presets/*.yaml` — this
file exists so the set of scenarios, what each one is supposed to prove, and what's real vs.
simulated in each can be reviewed as a whole instead of re-derived from the script each time.

Last synced with `run.sh`: 2026-07-11. If you change a scenario's behavior, update its section here
in the same change.

## At a glance

| # | Scenario | Picker text | LIVE? | Adapter preset used | Fleet-wide "real" | What's simulated |
|---|----------|-------------|-------|----------------------|--------------------|-------------------|
| 1 | `healthy-fleet` | 3 robots, nothing goes wrong (default) | No | — | Control plane, CRDs, scheduler | All 3 robots' readiness (`kubectl patch phase=Idle`) |
| 2 | `battery-edge` | watch the scheduler avoid a dying robot | Yes | `battery-edge` | `sim-robot-001`'s connection, registration, telemetry, battery drain | `sim-robot-002`/`003`'s readiness + battery (`85`, patched) |
| 3 | `battery-handoff` | a working robot's battery drops mid-task; watch its task get safely handed off | Yes | `battery-edge` (reused) | `sim-robot-001`'s telemetry; the cancel/confirm-stop/release/reassign mechanics once triggered | Which robot's battery is cut, when, and the battery number itself — no controller decides this on its own |
| 4 | `hardware-fault` | camera fails mid-task, watch it reroute and recover | Yes | `hardware-fault` | `sim-robot-001`'s fault delta + capability re-derivation + reroute | `sim-robot-002`/`003`'s readiness + battery |
| 5 | `comms-flaky` | a robot drops and reconnects | Yes | `comms-flaky` | `sim-robot-001`'s real stream drop/reconnect, fencing + lease behavior | `sim-robot-002`/`003`'s readiness + battery |
| 6 | `estop-drill` | trigger and confirm an emergency stop | Yes | `estop-drill` | `sim-robot-001`'s confirmed estop via `safety.confirm_estop` (ground truth, never inferred) | `sim-robot-002`/`003`'s readiness + battery |
| 7 | clean everything | tear down the cluster, kill any live processes, and exit | — | — | — | — |

"LIVE?" — whether the scenario drives `sim-robot-001` with a real, running `sim_adapter.py` process
against the real ControlStream (as opposed to `healthy-fleet`'s pure `kubectl patch` status
projection). Every LIVE scenario deploys via `config/overlays/quickstart-dev` (DEV/DEMO ONLY —
disables ControlStream per-robot authorization; see that overlay and
`cmd/manager/main.go --fleet-adapter-insecure-authz`). CI (`CI=true`, what `make quickstart-test`
uses) always forces the `healthy-fleet` status-projected path regardless of `--scenario`, so none of
1–7 above run LIVE in CI.

Most scenarios need exactly **one** Enter press (`--pace keypress`, the LIVE default) — see
`pause_for_demo` in `run.sh`. Pauses are deliberately not scattered across steps; `battery-handoff`
briefly had a second pause mid-scenario, which turned out confusing (looked hung) and was removed.
**Exception: `hardware-fault` has a second, deliberate pause** — after the camera task lands on the
live robot and the spare is staged, so a presenter can show the before-state, then press Enter to
degrade the camera and watch the reroute. Its prompt is explicit ("Press Enter to DEGRADE …") so it
doesn't read as hung. Both pauses honor `--pace timed`/`off` and `CI=true`.

---

## 1. `healthy-fleet`

**Status: reviewed.**

What it proves: the control plane, CRDs, capability derivation, and scheduler are real end to end —
3 robots go `Idle`, the real `DefaultScheduler` assigns both sample `FleetAction`s.

- Real: control plane, CRD reconciliation, capability derivation, scheduler assignment.
- Simulated: readiness. `project_readiness()` patches all 3 robots' `status.phase=Idle` directly —
  no adapter runs at all.
- This is the only scenario CI ever exercises (`make quickstart-test` forces it via `CI=true`
  regardless of `--scenario`), so it's the one gate that can never silently rot.

## 2. `battery-edge`

**Status: reviewed** (root-caused a stale/incorrect comment in the preset file; fixed).

What it proves: the scheduler's real battery-descending tie-break
(`DefaultScheduler.SelectRobot`, `internal/scheduler/scheduler.go`) keeps a low-battery robot from
winning new task assignments while a healthier Idle candidate exists.

- Real: `sim-robot-001`'s connection, Hello/Register handshake, and telemetry — its battery
  genuinely starts at 22% and drains to 0 (`battery-edge.yaml`: `drain_per_second: 1.0`).
- Simulated: `sim-robot-002`/`003` are patched to `Idle` + `batteryPercent: 85` directly — a healthy
  baseline so the contrast against `sim-robot-001` is real, not an artifact of the other two
  reporting no battery at all.
- **Known limitation, not yet fixed**: with the fixed 2-task, 3-robot sample fleet,
  `sim-robot-002`/`003` typically claim both sample tasks before `sim-robot-001`'s real handshake
  even finishes (patching Idle is instant; a real network round-trip isn't) — so in practice
  `sim-robot-001` is excluded because it isn't Idle *yet*, not because of a genuine live battery
  comparison. The scheduler mechanism this scenario is meant to showcase is real, but this specific
  demo doesn't reliably stage a head-to-head to prove it. (A staged version was built and reverted
  in this repo's history — it depended on FleetAction-controller reconcile timing that turned out
  less immediate than assumed, and produced a confusing result. `battery-handoff` below is the
  scenario that actually stages a reliable, provable comparison.)
- Corrected: `adapters/scenarios/presets/battery-edge.yaml`'s comment used to claim the mechanism
  was `FleetAction.spec.minBatteryPct`-based requeueing — that field doesn't exist anywhere in the
  codebase. Comment now correctly describes the battery-descending tie-break.

## 3. `battery-handoff`

**Status: reviewed, confirmed working end-to-end on a fresh cluster (2026-07-11).**

An earlier test against a `kind` cluster that had been reused across many manual re-runs this
session showed `sim-robot-002` still carrying a stale `status.assignedTask` after the hand-off, and
the real reassignment landing on the low-battery live robot instead of the intended spare — looked
like a genuine double-assignment bug. Rerunning `--clean` (fresh cluster) then the scenario once
produced the correct result: hand-off landed on the healthy spare robot, no stale state on the
released one. Root cause not fully isolated, but not reproducing on a clean cluster is strong
evidence it was leftover local state, not a control-plane bug. **If this resurfaces, always retest
on a fresh cluster (`run.sh --clean` first) before treating it as a real bug** — `reset_namespace_state()`
only deletes `Robots`/`FleetActions` between runs, not the whole cluster.

What it proves: a task can be safely moved off a robot mid-execution and onto a healthy one, using
the real confirmed-stop discipline (RA-4 single-executor guarantee) — not an instant "unreachable ⇒
reassign" shortcut, which the codebase deliberately avoids as a double-execution hazard.

- Real, once triggered: setting the `swarmada.io/requeue-requested` annotation on a `FleetAction`
  (`internal/controller/fleetaction_controller.go`'s `handleRequeue`) is the exact mechanism
  `ZoneMaintenance`'s Immediate mode uses in production — it pushes a real `cancel_task`, waits for
  a **confirmed** stop (adapter ack, or here, provable lease expiry — `leaseDuration = 30s`), then
  releases the robot and returns the task to `Pending` for real re-scheduling.
- Simulated / script-decided: **which** robot's battery drops, **when**, and the battery number
  itself (patched to `8`). There is no controller anywhere that watches battery and decides to
  trigger a hand-off on its own — that would be new product work (a real "battery below X while
  InProgress → auto-requeue" feature), not built here. The annotation is what does the real work;
  the battery patch is cosmetic narrative.
- `sim-robot-001` runs the real `battery-edge` preset underneath (there's no
  `battery-handoff.yaml` — `sim_adapter.py` hard-errors on an unknown `--scenario` name), so it
  stays low-battery and excluded throughout, exactly like scenario 2. It exists in this scenario
  purely so the fleet still has its usual 3-robot shape; it plays no role in the hand-off itself.
- Setup: only `deliver-pallet-001` is applied (`apply_fleet`, scoped to this scenario) —
  `inspect-receiving-dock` is intentionally never applied, so one of `sim-robot-002`/`003` stays
  genuinely spare (Idle, healthy) to receive the hand-off instead of also holding a task.
- Timing: expect up to ~40s after the single Enter press for the full sequence (telemetry confirm →
  battery patch → annotate → confirmed-stop wait, gated on the real 30s lease expiry since the
  demoted robot has no live wire → reassignment). `handle_battery_handoff()` blocks and `fail()`s
  loudly (with diagnostics) if the hand-off doesn't complete in that window — this is treated as a
  genuine failure, not a soft warning.
- **Open question for review**: should this scenario also cover the *live-wire* confirmed-cancel
  path (cutting `sim-robot-001`'s task instead of a projected robot's) to get a near-instant
  hand-off instead of the ~30s lease-expiry fallback? Would need `sim-robot-001` to actually hold a
  task at some point first, which conflicts with its current "always excluded" role in every other
  battery scenario — deliberately not built yet, flagged here for a decision.

## 4. `hardware-fault`

What it proves: degrading `sim-robot-001`'s `camera_front` *capability* triggers **Capability-loss
reassignment** (RFC-0001): the control plane safely stops the in-flight camera task and reroutes it
to a spare camera-capable robot.

- Real: the capability re-derivation and the automatic in-flight reroute — a real `cancel_task` over
  `sim-robot-001`'s **live** ControlStream (the sim adapter acks `STOPPED_SAFELY`), requeue, and
  reassignment. The reroute is **not** script-triggered: the FleetAction controller detects the
  capability loss itself (unlike `battery-handoff`, which the script triggers with a
  `requeue-requested` annotation).
- Script-driven (honesty note, same as `battery-handoff`'s battery): the *capability degradation* is
  driven by `run.sh`, not the adapter. In a status-projected quickstart the live adapter's hardware
  telemetry does not reach `status.hardware` (battery doesn't either), so `handle_hardware_fault`
  patches `sim-robot-001.status.hardware[camera_front]` Healthy (→ capability Active, task lands) then
  Degraded (→ capability Degraded, reroute). Everything from the capability degrading onward is real,
  unmodified control-plane code.
- Deterministic setup, all in `run.sh` (`config/samples/demo_a.yaml` untouched): only the camera task
  (`inspect-receiving-dock`) is applied (`deliver-pallet-001` held back); `sim-robot-002`/`003` are
  stripped to nav-only so the camera task can only land on the live robot; `sim-robot-001`'s
  `camera_front` capability is gated on a `camera_front` hardware component so a Degraded hardware
  status degrades the *capability* the scheduler reads (the sample's ungated capability would not);
  and `handle_hardware_fault` re-adds `camera_front` to the idle `sim-robot-002` as the reroute target.
- Simulated: `sim-robot-002`/`003` readiness, as in every LIVE scenario.
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
- **The adapter-health view now DOES move — this is no longer logs-only.** `FleetAdapter.status.phase`
  flips `Connected → Disconnected → Connected` across the outage, so swarmtop's adapter-health (`a`)
  view is the place to watch. That path is keyed on a *verified mTLS* identity, which the LIVE
  quickstart now presents (`config/overlays/quickstart-mtls`); under the older plaintext overlay it
  could never move, which is why this scenario used to be described as logs-only.
  The **robot** views still do not flip: per-robot liveness is deliberately kept off the status
  write path (RA-1), so `robot_controller.go`'s heartbeat-timeout → `Offline` path stays quiet. The payoff is in the **adapter + manager logs** — the stream drops, the adapter
  self-stops its task on lease expiry (C4), reconnects with a fresh Hello/Register, and its fencing
  token + lease survive (C3). `scenario_watch_command()` therefore points comms-flaky at
  `tail -f /tmp/swq-…-adapter.log` (and `kubectl logs -n system deploy/swarmada-controller-manager -f`),
  NOT at swarmtop. Closing an honest signal here (robot-Offline on silence, or adapter connectivity
  for a plaintext adapter) would need real control-plane/mTLS work — see the CHANGELOG entry.

## 6. `estop-drill`

What it proves: a robot that is **actually holding work** is emergency-stopped, CONFIRMS the stop
itself (`safety.confirm_estop` — simulator ground truth, never inferred) reported over the
`SafetyStream` as an `EstopAck`, and the control plane pauses the action it was executing
(§9.6.2.4 — estop takes precedence over the lease).

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

- **No automatic `Discovered` → `Idle` admission transition exists in the control plane.**
  `sim-robot-001` reaching `Idle` in every LIVE scenario is `run.sh` projecting it (gated on real
  observed telemetry, not a blind timer) — not a real control-plane feature. See the project memory
  `project_swarmada_no_discovered_to_idle_transition.md` for the full trace. This is the single
  biggest thing that would change if/when the real Health Monitor admission path gets built.
- **`sim-robot-002`/`003` are never live.** Every scenario's "healthy baseline" is a status
  projection. If a future scenario needs two genuinely live robots (e.g., a true head-to-head
  battery race, or `battery-handoff` over a live wire — see its open question above), that's new
  `run.sh` plumbing (a second adapter process, a second port-forward), not something any current
  scenario does.
- **Timeouts are simulator-tuned.** `QUICKSTART_READY_TIMEOUT`/`QUICKSTART_ASSIGN_TIMEOUT` default
  to 15s each — fine for an all-simulated fleet, too tight for real hardware. `battery-handoff`'s
  own internal wait (up to 40s) is intentionally separate from these.
- **`kubectl get robots` can't show hardware/capability/estop state.** See
  "Watching fields `kubectl get` can't show" below — affects `hardware-fault` and `estop-drill`
  most. (`comms-flaky` is deliberately NOT in this set: its payoff is a logs story, not a status
  field — see §5.)

## Watching fields `kubectl get` can't show

`Robot`'s printer columns are Phase/Zone/Battery/Task/Class/Age only
(`api/v1/robot_types.go`) — `status.hardware[]`, `status.capabilities[]`, and
`status.estopState` aren't columns, so plain `kubectl get robots` can't surface
them at all. This matters most for `hardware-fault` (§4): its entire payoff —
camera degrades → `perception.camera` capability degrades → task reroutes →
camera recovers → capability recovers — happens entirely inside
`status.hardware[]`/`status.capabilities[]`, invisible without `-o yaml`/`-o
jsonpath` mid-demo. `estop-drill` (§6, `status.estopState`) has the same problem,
only less central to its story. `comms-flaky` (§5) does NOT belong here: its
payoff never reaches any status field — `status.connectivity` is never written
(RA-1) — so it's a logs story, watched via the adapter/manager logs.
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

## Review checklist

Use this when walking the list end to end:

- [ ] `healthy-fleet` — reviewed 2026-07-11
- [ ] `battery-edge` — reviewed 2026-07-11 (known limitation documented above)
- [x] `battery-handoff` — confirmed working end-to-end on a clean cluster 2026-07-11 (open question above still stands)
- [ ] `hardware-fault` — capability-loss reassignment landed (genuine reroute); needs a live run to confirm current behavior
- [ ] `comms-flaky` — rewritten as a logs story 2026-07-11 (no swarmtop/status signal); needs a live run to confirm reconnect
- [ ] `estop-drill` — ControlStream reconnect-resilience fix landed 2026-07-11; needs a live run to confirm current behavior
- [ ] `--clean` / option 7 — reviewed 2026-07-11
