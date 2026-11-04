# warehouse-quickstart

The 5-minute path from an empty machine to a running Swarmada control plane that
schedules work across a simulated fleet — on a local `kind` cluster, in one
command.

```bash
make quickstart
```

That runs [`run.sh`](run.sh), which brings up a **real** control plane (no mocks),
applies the maintained sample fleet, drives the robots Ready, and lets the real
scheduler assign work. On the default (LIVE) path the one live robot is assigned an action
and completes it, and the other two sample actions stay `Pending` (see
[Honest notes](#honest-notes-what-is-and-isnt-real)). It ends with a success line and
leaves the cluster up so you can poke at it.

## What it does

Every step is real except where the "Honest notes" below say otherwise. It
reuses [`config/samples/demo_a.yaml`](../../config/samples/demo_a.yaml) — one
`FleetZone`, three simulated `Robot`s, two `FleetAction`s — so this example can
never drift from the canonical manifests.

0. **Clean up** any leftover port-forward/adapter processes from a previous run,
   then **create** a `kind` cluster (`swarmada-quickstart`) — reusing it if it
   already exists and its manager is healthy, otherwise deleting and
   recreating it, so a reused cluster never starts from an unclean state.
1. **Build** the controller-manager image and **load** it into the cluster.
2. **Install** the CRDs and **deploy** the control plane: on the LIVE path,
   [`config/overlays/quickstart-mtls`](../../config/overlays/quickstart-mtls/),
   after installing cert-manager to issue the `ControlStream` server and adapter
   client certificates; on the status-projected path,
   [`config/overlays/quickstart-projected`](../../config/overlays/quickstart-projected/)
   (`ControlStream` off, no cert-manager). Neither overlay serves the admission
   webhooks. See [Honest notes](#honest-notes-what-is-and-isnt-real).
3. **Reset** namespace `warehouse-a` (deletes any leftover `Robot`/`FleetAction`
   objects from a previous run against a reused cluster), then **apply** the
   sample fleet fresh.
4. **Drive the robots Ready** — LIVE (a real adapter drives `sim-robot-001`)
   unless `--projected` selects the status-projected path — see
   [Honest notes](#honest-notes-what-is-and-isnt-real).
5. **Assert** the end state: wait until all robots are `Idle` **and** the
   scheduler has assigned a `FleetAction`, then print a success line (or a failure line plus diagnostics and
   exit non-zero).

## Prerequisites

- `docker` (running), `kind`, `kubectl`, `make`, and `go`.
- `python3` — needed for the LIVE path, which every scenario takes by default,
  including `healthy-fleet` (see [Honest notes](#honest-notes-what-is-and-isnt-real)).
  Only the status-projected path (`--projected`) runs without it.
- Network access on the LIVE path: the script installs cert-manager from its
  GitHub release manifest.
- No cluster required up front — the script creates one (or reuses an existing
  one if it's still healthy — see [Knobs](#knobs)). Nothing is installed
  outside the `kind` cluster and your Go build cache.
- Installing these for the first time? See [`docs/quickstart.md`](../../docs/quickstart.md)
  for the full per-OS list (macOS via `make setup-macos`, Linux package-manager
  install, Windows via WSL2) — this page assumes they're already on your PATH.

## Run

```bash
# from the repository root
make quickstart
```

On a terminal this first asks you to pick a scenario (see [Scenarios](#scenarios));
press Enter for the default. For CI or a recorded video, pass `--scenario` to skip
the prompt.

### Expected output

Abridged, from the default LIVE path (`healthy-fleet`); timing and ages will vary. The
status-projected path (`--projected`) does not reach this end state at
this release: it exits 1 with `no FleetAction was assigned` (see
[Honest notes](#honest-notes-what-is-and-isnt-real)).

```text
==> 5/7 — Apply the sample fleet (reuses config/samples/demo_a.yaml)
fleetzone.swarmada.io/warehouse-a created
robot.swarmada.io/sim-robot-001 created
robot.swarmada.io/sim-robot-002 created
robot.swarmada.io/sim-robot-003 created
fleetaction.swarmada.io/deliver-pallet-001 created
fleetaction.swarmada.io/inspect-receiving-dock created
fleettask.swarmada.io/receiving-round-001 created

==> 6/7 — Drive the robots ready: sim-robot-001 is LIVE (scenario 'healthy-fleet')
    real telemetry confirmed (battery reported by the live adapter) ✓
    sim-robot-001 phase=InProgress (real) ✓
    FleetAdapter sim-fleet-adapter phase=Connected (real) ✓

==> 7/7 — Wait for the verifiable end state
    all robots Idle ✓
    a FleetAction was assigned by the scheduler ✓
    a FleetAction was completed by the live robot ✓

NAME            PHASE   ZONE          BATTERY   ACTION   AGE
sim-robot-001   Idle    warehouse-a   99                 43s
sim-robot-002   Idle    warehouse-a   85                 43s
sim-robot-003   Idle    warehouse-a   85                 43s

NAME                               TYPE       VERB   PHASE       OUTCOME   PRIORITY   ROBOT           ZONE          AGE
deliver-pallet-001                 Navigate          Succeeded             High       sim-robot-001   warehouse-a   43s
inspect-receiving-dock             Navigate          Pending               Normal                     warehouse-a   43s
receiving-round-001-inspect-dock   Navigate          Pending               Normal                     warehouse-a   43s

NAME                  PHASE     ACTIONS         DESIRED   AGE
receiving-round-001   Running   0/1 Succeeded   Running   43s

✅ Quickstart end state reached: all robots Idle, the real scheduler
   assigned a FleetAction in warehouse-a, and the live robot completed one.
```

### Explore it, then tear it down

```bash
kubectl get robots,fleetactions -n warehouse-a
kubectl -n system logs deployment/swarmada-controller-manager

kind delete cluster --name swarmada-quickstart
```

## Scenarios

The quickstart fronts a **scenario** choice before it dials the control plane, so
you can pick what the simulated fleet does. See
[`SCENARIOS.md`](SCENARIOS.md) for a per-scenario reference (what's real vs.
simulated in each, known limitations, open questions) — this section is only
the picker itself. On a terminal it shows an interactive numbered picker
(press Enter for the default):

```text
Pick a scenario to run against your local Swarmada cluster:
  1) healthy-fleet     — 3 robots, nothing goes wrong (default)
  2) battery-edge      — a robot's battery drains while it keeps its task
  3) battery-handoff   — a working robot's battery drops mid-task; watch its
                          task get safely handed off to a healthy robot
  4) hardware-fault    — camera fails mid-task, watch it reroute and recover
  5) comms-flaky       — a robot drops and reconnects
  6) estop-drill       — trigger and confirm an emergency stop
  7) clean everything  — tear down the cluster, kill any live processes, and exit
> 3
```

**`battery-handoff`** is a different story from `battery-edge`: instead
of a dying robot losing out on *new* work, a robot that's *already
working* runs low and its task gets moved — live — to a healthy
one. Every robot in this scenario, and in `hardware-fault`, runs its own live
adapter, so the spare that receives the task is as real as the robot it
leaves, and completes it. Only one of the two sample FleetActions is applied, so
both spares stay free to receive the hand-off; see
[Honest notes](#honest-notes-what-is-and-isnt-real) for exactly what's real
here (all of it except one decision: when to hand off is this script's call —
there's no controller that does that on its own yet).

Option 7 (or `--clean` — see [Knobs](#knobs)) doesn't run a scenario at all: it
deletes the `kind` cluster, kills any live/stale port-forward or adapter
processes this script left behind, removes the local pidfile/log state, and
exits without bringing anything back up:

```bash
examples/warehouse-quickstart/run.sh --clean
```

The picker is a convenience layer on top of a flag, **not** a second code path.
Every scenario, `healthy-fleet` included, takes the **LIVE** path (see
[Honest notes](#honest-notes-what-is-and-isnt-real) below) unless
`--projected` forces the status-projected path. `CI=true`, which GitHub Actions sets,
only turns pacing off:

```bash
examples/warehouse-quickstart/run.sh --scenario hardware-fault --robots 5
CI=true examples/warehouse-quickstart/run.sh --scenario hardware-fault  # LIVE, no pause
examples/warehouse-quickstart/run.sh --projected                        # status-projected
```

With no TTY (a pipe, or CI) and no `--scenario`, it uses `healthy-fleet`; whether
that run is LIVE or status-projected depends only on `--projected`.

Beyond the seven picker entries there's also `full-surface`, an advanced
coverage scenario reachable only via `--scenario full-surface` (not in the
interactive picker): it runs camera degrade→fail→recover, then a ControlStream
drop/reconnect, then a confirmed estop in one run, so every non-column field
moves through swarmtop's views in a single pass.

Every scenario except `battery-handoff` is a named preset every simulated
Fleet Adapter consumes directly
([`adapters/scenarios`](../../adapters/scenarios/README.md)). `battery-handoff`
is script-level orchestration layered on top: `sim-robot-001` still runs the
real `battery-edge` preset underneath (there's no `battery-handoff.yaml`
preset — the adapter would hard-error on an unknown `--scenario` name), while
`run.sh` itself drives the actual hand-off story — see the scenario picker
text above and `handle_battery_handoff` in `run.sh`.

| Flag             | Default         | Meaning                                              |
| ---------------- | --------------- | ---------------------------------------------------- |
| `--scenario NAME`| `healthy-fleet` | scenario preset; non-interactive (skips the picker)  |
| `--robots N`     | `3`             | informational only — this quickstart uses the fixed 3-robot sample fleet |
| `--projected`    | —               | force the status-projected path (no adapter process) |
| `--clean`        | —               | tear down the cluster + kill live/stale processes + remove local state, then exit. Same as picking `7` in the picker |
| `-h`, `--help`   | —               | show flag help and exit                              |

Whenever the script stops — success, failure, or Ctrl-C — it prints a reminder
of how to clean up the cluster (`run.sh --clean` or `kind delete cluster`),
unless `--clean` itself was what last ran (nothing left to remind you about).

## Honest notes (what is and isn't real)

The **control plane, the CRDs, the capability derivation, and the scheduler are
fully real** — the task assignments you see are produced by the actual
`DefaultScheduler` matching each task's required capabilities against the robots'
derived, Active capabilities in the declared zone. This is true on both paths
below.

### Two readiness paths

- **Status-projected** (any scenario when `--projected` is passed):
  Step 6 patches each robot's `status.phase` to `Idle` and the `FleetAdapter`'s
  phase to `Connected` — no adapter runs. CI does not take this path. At this
  release it does not reach its end state: no projected robot
  reports a position, so candidate filter 2 withholds every action in the zone and
  the run exits 1 with `no FleetAction was assigned`.
- **LIVE** (the default for every scenario, CI included): `sim-robot-001` is driven by a
  real, running
  [`adapters/simulation/sim_adapter.py`](../../adapters/simulation/sim_adapter.py)
  process, talking to the real ControlStream over mTLS through a `kubectl port-forward` to
  the `swarmada-controlstream` Service — so the chosen scenario's fault/
  battery/comms/estop behaviour actually happens, live, on that robot: real
  connection, real Hello/Register handshake, real telemetry ingestion. The
  other two robots stay status-projected (with a healthy baseline battery), so
  the fleet shows three robots — though, as below, only the live one can be
  given a zoned action — except in
  `battery-handoff` and `hardware-fault`, which move work to a spare: there
  each spare runs its own live adapter too, under its own `FleetAdapter`
  identity, deployed via
  [`config/overlays/demo-ab-live`](../../config/overlays/demo-ab-live/), and
  nothing is projected.

  **`sim-robot-001`'s readiness is derived, not projected.** The script waits
  for real telemetry (a `batteryPercent` reported by the live adapter — see
  `live_robot_telemetry_seen` in `run.sh`), then waits for the Robot reconciler to
  advance `sim-robot-001` off `Discovered` on its own: to `Idle`, or to
  `InProgress` when it already holds an assigned action, from fresh adapter
  liveness (`isLivenessOwnedPhase` / `steadyPhaseForLive` in
  `internal/controller/robot_controller.go`, ADR-0029). It then waits for the
  `FleetAdapter` to reach `Connected` from the adapter's verified mTLS identity.
  It writes neither. [`docs/quickstart.md`](../../docs/quickstart.md#what-the-control-plane-derives-and-what-the-runner-writes)
  lists what the runner does still write, and why.

  Both sample FleetActions and the runner's one-step FleetTask are applied
  together, and only `sim-robot-001` receives work. Where `sim-robot-002` and
  `sim-robot-003` are status-projected they run no adapter and report no
  position, so the Zone Controller never places them in `warehouse-a`: their
  `status.currentZone` stays empty and candidate filter 2 withholds them from
  every action in the zone. The scheduler assigns one action to `sim-robot-001`,
  which completes it, and the other two stay `Pending` — which is the end state
  `healthy-fleet` checks. With one eligible robot there is nothing for
  `DefaultScheduler.SelectRobot` (`internal/scheduler/scheduler.go`) to compare, so
  this run does not stage a battery comparison between robots.

  This deploys the control plane via
  [`config/overlays/quickstart-mtls`](../../config/overlays/quickstart-mtls/)
  instead of `config/default`. **DEV/DEMO ONLY**: the `ControlStream` is mutually
  authenticated — the adapter's identity comes from its client certificate, which
  `sim_adapter.py` presents over a TLS channel (`--tls-ca`, `--tls-cert`,
  `--tls-key`) — and per-robot authorization is on: `sim-fleet-adapter` lists the
  sample robots' class `simbot-v1` in `spec.servesRobotClasses`, and each robot-scoped
  message is checked against that and the certificate identity
  (RFC-0001 #fleet-adapter-protocol-transport-security, RFC-0001 #security-fleet-adapter-authentication-robot-identity).
  Its certificates are self-signed and its admission webhooks are off, so never apply
  `config/overlays/quickstart-mtls` outside a local throwaway cluster.

  Watch it live, in another terminal:
  ```bash
  kubectl -n warehouse-a get robot sim-robot-001 -w
  tail -f /tmp/swq-swarmada-quickstart-adapter.log
  ```

On the LIVE path the live robot's action reaches `Succeeded`: the reference sim
adapter reports completion, and the run waits for it. On the status-projected path no
action is assigned, as described above.

## CI

The quickstart is exercised on every push/PR by the `quickstart` job in
[`.github/workflows/ci.yml`](../../.github/workflows/ci.yml), which runs:

```bash
make quickstart-test
```

That is `make quickstart` on a `kind` cluster with automatic teardown; it fails
the build if the success end state is not reached within the timeouts. The job
takes the LIVE path: it installs Python, and a real sim adapter drives
`sim-robot-001` over the mTLS ControlStream, so the scheduler assigns that robot
work as it does when you run `make quickstart` locally.

GitHub Actions sets `CI=true` automatically, which only turns pacing off. So the CI
run depends on a live subprocess, a port-forward, cert-manager and
`config/overlays/quickstart-mtls`, as a local run does. CI does not run the
status-projected path (`--projected`), which at this release exits 1 with
`no FleetAction was assigned` (see [Honest notes](#honest-notes-what-is-and-isnt-real)).

## Knobs

CLI flags (`--scenario`, `--robots`) are in [Scenarios](#scenarios). The
environment knobs below tune the cluster and timeouts:

| Env var                     | Default               | Meaning                              |
| --------------------------- | --------------------- | ------------------------------------ |
| `QUICKSTART_CLUSTER`        | `swarmada-quickstart` | kind cluster name — reused if it exists and its manager Deployment is `Available`; deleted and recreated otherwise |
| `QUICKSTART_IMG`            | `swarmada-controller:dev` | controller image tag. **Only the default is supported.** Another value builds and loads the image under that tag, but the deploy still runs `swarmada-controller:dev` (pinned in `config/default/kustomization.yaml`). On a cluster the run creates, the manager pod stays in `ErrImagePull`/`ImagePullBackOff` and the run fails at step 4/7 with `timed out waiting for the condition`. On a reused cluster that already holds `:dev` from an earlier run, the run prints `Quickstart end state reached` and exits 0, but the manager runs that earlier `:dev` image, not the one this run built. Leave it unset. If you set it on a reused cluster, delete the cluster (`run.sh --clean`) and run again without it |
| `QUICKSTART_READY_TIMEOUT`  | `15`                  | seconds to wait for robots `Idle`    |
| `QUICKSTART_ASSIGN_TIMEOUT` | `15`                  | seconds to wait for a task `Assigned` |
| `QUICKSTART_CS_PORT`        | `19443`               | local port for the ControlStream port-forward (LIVE path only) |
| `QUICKSTART_PAUSE_SECONDS`  | `8`                   | duration for `--pace timed` (see below) |
| `CI`                        | unset                 | `true` disables pacing; the path stays LIVE unless `--projected` is passed |

The 15s timeout defaults assume a fully simulated fleet (this quickstart never
talks to real hardware). If you adapt this script for a real adapter
(`adapters/external/fleet-adapter-*`), raise both via the env vars above —
real handshake/comms/startup latency won't fit in 15s.

### Pacing, for presenting live

Rolling straight through all 7 steps is fine for a quick check, but not for
narrating to an audience. Rather than pausing at every step (which only
dilutes attention), there's exactly **one** deliberate pause, placed right
before the LIVE scenario's actual payoff — the moment the live sim adapter
starts talking to the real control plane and the scenario clock begins
counting down to its fault/drop/estop:

```bash
examples/warehouse-quickstart/run.sh --scenario hardware-fault --pace keypress   # wait for Enter (default on LIVE)
examples/warehouse-quickstart/run.sh --scenario hardware-fault --pace timed --pause-seconds 15
examples/warehouse-quickstart/run.sh --scenario hardware-fault --pace off        # old behaviour, no pause
```

`--pace` defaults to `keypress` on the LIVE path (there's an actual payoff
worth narrating) and `off` on the status-projected path (nothing to pace on).
It's always `off` under `CI=true`, regardless of what's passed.

The prompt itself says what's about to happen, not only "press Enter" — e.g.
for `hardware-fault`:

```text
    optional — open a second terminal and run:
        swarmtop -n warehouse-a --robot sim-robot-001

    About to start the live 'hardware-fault' scenario on sim-robot-001: All three
    robots come online, each driven by its own live adapter. sim-robot-001 is the only
    camera robot, so it takes the camera task. Its adapter reports camera_front DEGRADED
    about 20s after it starts: the capability degrades, and the control plane
    automatically safe-stops the task and REROUTES it to the spare camera robot,
    sim-robot-002, which completes it.
    press Enter to continue...
```

`--pace timed` shows the same explanation before its countdown.

**Watching with swarmtop (preferred), or a `jq` fallback.** `Robot`'s printer
columns are Phase, Zone, Battery, Action and Age, plus Class with `-o wide`
(`api/v1/robot_types.go`) —
`status.hardware[]`, `status.capabilities[]`, and `status.estopState` aren't
columns, so `kubectl get robots` can't show them at all.
[`tools/swarmtop`](../../tools/swarmtop/README.md) is the terminal fleet
inspector that renders exactly these fields — a live, color-coded robot list
with split and full-detail views, plus FleetAction (`t`) and adapter-health (`a`)
views. Build it once with:

```bash
make -C tools/swarmtop build
```

Then, for the scenarios whose payoff lives in a non-column field —
`hardware-fault`, `estop-drill`, and `full-surface` — `run.sh`'s
`scenario_watch_command()` prints `swarmtop -n warehouse-a --robot sim-robot-001`,
which opens straight into that robot's detail view (press `s`/`enter`/`t`/`a`
to move around). If no `swarmtop` binary is on `PATH` or built in-tree, those
fall back automatically to their field-specific `kubectl -o json | jq`
one-liner. The other scenarios (`healthy-fleet`, `battery-edge`,
`battery-handoff`) keep plain `watch kubectl get robots`, since Battery, Phase
and Action already have printer columns.

**`comms-flaky` is different — it's a logs story, not a swarmtop one.** Its
payoff (a full ControlStream drop/reconnect, lease self-stop C4, fencing-token
survival C3) is in the adapter and control-plane logs rather than in a `Robot`
status column. The one per-robot liveness field, `status.connectivity.lastSeenAt`,
is written by the `FleetAdapter` controller at most once per half the namespace's
offline threshold, so it is not a view of the drop and reconnect. For it,
`scenario_watch_command()` prints a `tail -f` on the adapter log (and points at
`kubectl logs -n system deploy/swarmada-controller-manager -f` for the control
plane) instead. swarmtop is strictly optional — nothing in the quickstart
depends on it being installed. See
[`tools/swarmtop/README.md`](../../tools/swarmtop/README.md) for the tool.

Every run also resets `warehouse-a`'s `Robot`/`FleetAction` objects and kills any
leftover port-forward/adapter processes from a previous invocation before
doing anything else, so re-running against a reused cluster — with the same or
a different scenario — always starts from a clean slate rather than
inheriting stale status from an earlier run.
