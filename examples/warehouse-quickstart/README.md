# warehouse-quickstart

The 5-minute path from an empty machine to a running Swarmada control plane that
schedules work across a simulated fleet — on a local `kind` cluster, in one
command.

```bash
make quickstart
```

That runs [`run.sh`](run.sh), which brings up a **real** control plane (no mocks),
applies the maintained sample fleet, drives the robots Ready, and lets the real
scheduler assign the sample tasks. It ends with a success line and leaves the cluster up so
you can poke at it.

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
2. **Install** the CRDs and **deploy** the control plane (`config/default`, or
   [`config/overlays/quickstart-dev`](../../config/overlays/quickstart-dev/)
   on the LIVE path — see [Honest notes](#honest-notes-what-is-and-isnt-real)
   — neither carries admission webhooks, so no cert-manager is needed on a
   bare cluster).
3. **Reset** namespace `warehouse-a` (deletes any leftover `Robot`/`FleetAction`
   objects from a previous run against a reused cluster), then **apply** the
   sample fleet fresh.
4. **Drive the robots Ready** — status-projected (`Idle`) or LIVE (a real
   adapter), depending on the chosen scenario — see
   [Honest notes](#honest-notes-what-is-and-isnt-real).
5. **Assert** the end state: wait until all robots are `Idle` **and** the
   scheduler has assigned a `FleetAction`, then print a success line (or a failure line plus diagnostics and
   exit non-zero).

## Prerequisites

- `docker` (running), `kind`, `kubectl`, `make`, and `go`.
- `python3` — only needed for the LIVE scenario path (see
  [Honest notes](#honest-notes-what-is-and-isnt-real)); the default
  `healthy-fleet` path doesn't need it.
- No cluster required up front — the script creates one (or reuses an existing
  one if it's still healthy — see [Knobs](#knobs)). Nothing is installed
  outside the `kind` cluster and your Go build cache.

## Run

```bash
# from the repository root
make quickstart
```

On a terminal this first asks you to pick a scenario (see [Scenarios](#scenarios));
press Enter for the default. For CI or a recorded video, pass `--scenario` to skip
the prompt.

### Expected output

Abridged; timing, ages, and which robot gets which task will vary:

```text
==> 4/7 — Install CRDs and deploy the control plane
deployment "swarmada-controller-manager" successfully rolled out

==> 5/7 — Apply the sample fleet (reuses config/samples/demo_a.yaml)
fleetzone.swarmada.io/warehouse-a created
robot.swarmada.io/sim-robot-001 created
robot.swarmada.io/sim-robot-002 created
robot.swarmada.io/sim-robot-003 created
fleetaction.swarmada.io/deliver-pallet-001 created
fleetaction.swarmada.io/inspect-receiving-dock created

==> 6/7 — Drive the robots Ready (status projection — see header note)
robot.swarmada.io/sim-robot-001 patched
robot.swarmada.io/sim-robot-002 patched
robot.swarmada.io/sim-robot-003 patched

==> 7/7 — Wait for the verifiable end state
    all robots Idle ✓
    a FleetAction was assigned by the scheduler ✓

NAME            PHASE   ZONE          BATTERY   TASK   AGE
sim-robot-001   Idle    warehouse-a                    18s
sim-robot-002   Idle    warehouse-a                    18s
sim-robot-003   Idle    warehouse-a                    18s

NAME                     TYPE       PHASE      PRIORITY   ROBOT           ZONE          AGE
deliver-pallet-001       Navigate   Assigned   High       sim-robot-003   warehouse-a   6s
inspect-receiving-dock   Navigate   Assigned   Normal     sim-robot-002   warehouse-a   6s

✅ Quickstart end state reached: all robots Idle and the real scheduler
   assigned a FleetAction to a robot in warehouse-a.
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
  2) battery-edge      — watch the scheduler avoid a dying robot
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
working* gets its battery cut and its task gets moved — live — to a healthy
one. Only one of the two sample FleetActions is applied for this scenario, so
one status-projected robot stays genuinely spare to receive the hand-off; see
[Honest notes](#honest-notes-what-is-and-isnt-real) for exactly what's real
here (most of it) and what isn't (which robot to cut, and when, is this
script's call — there's no controller that does that on its own yet).

Option 7 (or `--clean` — see [Knobs](#knobs)) doesn't run a scenario at all: it
deletes the `kind` cluster, kills any live/stale port-forward or adapter
processes this script left behind, removes the local pidfile/log state, and
exits without bringing anything back up:

```bash
examples/warehouse-quickstart/run.sh --clean
```

The picker is a convenience layer on top of a flag, **not** a second code path.
Picking anything other than `healthy-fleet` (or `clean`) takes the **LIVE**
path (see [Honest notes](#honest-notes-what-is-and-isnt-real) below) — set
`CI=true` to force the deterministic status-projected path regardless of
scenario (this is what `make quickstart-test` does automatically, so it is
unaffected):

```bash
examples/warehouse-quickstart/run.sh --scenario hardware-fault --robots 5
CI=true examples/warehouse-quickstart/run.sh --scenario hardware-fault  # forces status-projected
```

With no TTY (a pipe, or CI) and no `--scenario`, it uses `healthy-fleet`, which
always takes the status-projected path — so `make quickstart-test` is unaffected
either way.

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

- **Status-projected** (scenario `healthy-fleet`, or any scenario when
  `CI=true`): Step 6 patches each robot's `status.phase` to `Idle` (the same
  mechanism `make demo-b` uses) — no adapter runs. This is what
  `make quickstart-test` always uses, so it stays deterministic.
- **LIVE** (any other scenario, outside CI): `sim-robot-001` is driven by a
  real, running
  [`adapters/simulation/sim_adapter.py`](../../adapters/simulation/sim_adapter.py)
  process, talking to the real ControlStream over a `kubectl port-forward` to
  the `swarmada-controlstream` Service — so the chosen scenario's fault/
  battery/comms/estop behaviour actually happens, live, on that robot: real
  connection, real Hello/Register handshake, real telemetry ingestion. The
  other two robots stay status-projected (with a healthy baseline battery) so
  the scheduler still has a full 3-robot fleet to choose from.

  **One thing on the LIVE path is still projected, and it's a real product
  gap, not a shortcut**: `sim-robot-001`'s `status.phase` itself. As of this
  writing, nothing in the control plane ever transitions a Robot from
  `Discovered` to `Idle` — `robot_controller.go`'s reconciler only ever writes
  `Discovered` (default) or `Offline` (heartbeat timeout); the Health Monitor
  (RFC-0001 §9.3.3) is specified to write phase only to `Offline`; and
  `robot_status_sink.go` explicitly never writes `Phase` at all (RA-1: that's
  telemetry-owned battery/hardware only). The only `Idle` writer anywhere in
  the codebase is in `fleetaction_controller.go`, on task completion — which
  presupposes the robot was already `Idle`. So this script waits for genuine
  proof of life (a real `batteryPercent` reported via telemetry — see
  `live_robot_telemetry_seen` in `run.sh`) and only then projects `Idle`
  itself, the same way it does for the other two robots. Until a real
  first-registration/heartbeat → `Idle` admission transition exists in the
  control plane, this is the honest state of things.

  Both sample FleetActions are applied together, same as the status-projected
  path — `sim-robot-002`/`sim-robot-003` typically claim them before
  `sim-robot-001`'s real handshake finishes, since `sim-robot-001`'s readiness
  depends on an actual network round-trip while the other two are patched
  Idle instantly. `DefaultScheduler.SelectRobot`
  (`internal/scheduler/scheduler.go`) has no hard battery cutoff — among
  eligible Idle candidates it sorts by battery descending and takes the top
  one, but it only ever compares robots that are Idle *at the same reconcile*,
  so this fixed 2-task fleet doesn't reliably stage a live, side-by-side
  battery comparison against `sim-robot-001`.

  This deploys the control plane via
  [`config/overlays/quickstart-dev`](../../config/overlays/quickstart-dev/)
  instead of `config/default`. **DEV/DEMO ONLY**: that overlay sets
  `--fleet-adapter-insecure-authz=true` on the manager, which disables
  ControlStream's per-robot authorization (RFC-0001 §9.2.7, §9.5.1.2) —
  otherwise every message from a plaintext, non-mTLS adapter (which is what
  the reference sim adapter is; mTLS is backlog §F) is denied by design. This
  activates a dev-mode code path that already exists in
  `internal/controlstream/server.go` (a `nil` Authorizer authorizes everything
  and logs a loud warning at connect) — it does not add a new bypass, and it
  is off by default everywhere else, including `config/default` and
  production. Never apply `config/overlays/quickstart-dev` outside a local
  throwaway cluster.

  Watch it live, in another terminal:
  ```bash
  kubectl -n warehouse-a get robot sim-robot-001 -w
  tail -f /tmp/swq-swarmada-quickstart-adapter.log
  ```

Either way, one thing is deliberately simplified everywhere:

- **Tasks reach `Assigned`, not `Succeeded`.** The reference sim adapter accepts
  an assignment but never reports completion, so a terminal `Succeeded` is not
  reachable via this path. Scheduler **assignment** is therefore the verifiable
  end state — it proves the control plane matched work to a robot.

## CI

The quickstart is exercised on every push/PR by the `quickstart` job in
[`.github/workflows/ci.yml`](../../.github/workflows/ci.yml), which runs:

```bash
make quickstart-test
```

That is `make quickstart` on a `kind` cluster with automatic teardown; it fails
the build if the success end state is not reached within the timeouts. This is what
keeps "getting started" from silently rotting — an untested quickstart is the #1
source of a broken first experience.

GitHub Actions sets `CI=true` automatically, which forces the status-projected
path (see [Honest notes](#honest-notes-what-is-and-isnt-real)) regardless of
scenario — CI never takes the LIVE path, so it never depends on a live
subprocess, a port-forward, or `config/overlays/quickstart-dev`.

## Knobs

CLI flags (`--scenario`, `--robots`) are in [Scenarios](#scenarios). The
environment knobs below tune the cluster and timeouts:

| Env var                     | Default               | Meaning                              |
| --------------------------- | --------------------- | ------------------------------------ |
| `QUICKSTART_CLUSTER`        | `swarmada-quickstart` | kind cluster name — reused if it exists and its manager Deployment is `Available`; deleted and recreated otherwise |
| `QUICKSTART_IMG`            | `swarmada-controller:dev` | controller image tag             |
| `QUICKSTART_READY_TIMEOUT`  | `15`                  | seconds to wait for robots `Idle`    |
| `QUICKSTART_ASSIGN_TIMEOUT` | `15`                  | seconds to wait for a task `Assigned` |

The 15s timeout defaults assume a fully simulated fleet (this quickstart never
talks to real hardware). If you adapt this script for a real adapter
(`adapters/external/fleet-adapter-*`), raise both via the env vars above —
real handshake/comms/startup latency won't fit in 15s.
| `QUICKSTART_CS_PORT`        | `19443`               | local port for the ControlStream port-forward (LIVE path only) |
| `QUICKSTART_PAUSE_SECONDS`  | `8`                   | duration for `--pace timed` (see below) |
| `CI`                        | unset                 | `true` forces the status-projected path (and disables pacing) regardless of scenario |

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

    About to start the live 'hardware-fault' scenario on sim-robot-001: sim-robot-001
    comes online with its camera capability Inactive (no camera hardware yet), so the
    camera task stays Pending. Its camera_front hardware is then marked Healthy — the
    capability derives Active and the scheduler assigns it the camera task. You'll then
    be prompted ONCE MORE to degrade the camera (this script drives the hardware status,
    same as battery-handoff drives the battery): the capability degrades, and the control
    plane automatically safe-stops the task and REROUTES it to the idle spare camera
    robot, sim-robot-002.
    press Enter to continue...
    ...
    sim-robot-001 now holds the camera task; sim-robot-002 is staged as the idle camera
    spare. Press Enter to DEGRADE sim-robot-001's camera — the control plane will
    safe-stop the task and reroute it to sim-robot-002.
    press Enter to continue...
```

`--pace timed` shows the same explanation before its countdown.

**Watching with swarmtop (preferred), or a `jq` fallback.** `Robot`'s printer
columns are Phase/Zone/Battery/Task/Class/Age only (`api/v1/robot_types.go`) —
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
`battery-handoff`) keep plain `watch kubectl get robots`, since Battery/Phase/
Task already have printer columns.

**`comms-flaky` is different — it's a logs story, not a swarmtop one.** Its
payoff (a full ControlStream drop/reconnect, lease self-stop C4, fencing-token
survival C3) never reaches any `Robot` status field: per-robot liveness is kept
off the status write path (RA-1), so `status.connectivity` is never written and
neither swarmtop nor a `kubectl -o json` field would show anything move. For it,
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
