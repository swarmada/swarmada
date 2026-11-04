# Quickstart — running the Swarmada demos

From an empty machine to a running Swarmada control plane scheduling work across a
simulated fleet on a local `kind` cluster — in one command. This page is the
single entry point for running **every** demo scenario. Before building on what the
demos show, read [Known limitations](known-limitations.md).

## Prerequisites

- **Docker** (running), **`kind`**, **`kubectl`**, **`make`**, and **Go 1.26+**.
- **Kubernetes 1.31 or later**, the chart's minimum, because it is the oldest version measured to admit an emergency stop on a zone stored before one of the `FleetZone` CRD's validation rules existed; the `kind` cluster the quickstart creates runs the `kind` release's default node image.
- **`python3`** — for the LIVE path, which is the default for every scenario,
  including `healthy-fleet`, in CI too, unless `--projected` is passed: a real
  simulation adapter process drives one robot. Only the projected path
  (`bash examples/warehouse-quickstart/run.sh --projected`) runs without it.
- **Network access** on the LIVE path: the runner installs cert-manager from its
  GitHub release manifest to issue the `ControlStream` and adapter certificates.
- No cluster required up front — the quickstart creates its own `kind` cluster
  (`swarmada-quickstart`) and tears nothing down outside it and your Go build
  cache.

The quickstart itself is cross-platform (a portable `bash` script; the `make`
targets only call `go`/`docker`/`kind`/`kubectl`). Install the prerequisites for
your OS:

- **macOS** — `make setup-macos` installs everything via Homebrew, then
  `make quickstart`.
- **Linux** — install the prerequisites with your distro's package manager
  (`docker`, `kind`, `kubectl`, `make`, `go` 1.26+, `python3`, `protobuf`), then
  `make quickstart`. (There is no `setup-linux` installer; the demo itself is the
  same on Linux.)
- **Windows** — run under **WSL2** (Windows Subsystem for Linux), not native
  PowerShell, since the quickstart is a bash script. Use Docker Desktop with the
  WSL2 backend, install the Linux prerequisites above inside your WSL2 distro,
  then `make quickstart`.

The generated gRPC stubs are committed under `proto/`, so you don't need to generate
them. `make proto-go` and `make proto-py` regenerate them after a `.proto` change.

## Fastest path

```bash
make quickstart
```

This runs a **real** control plane (no mocks) on `kind`, applies the sample fleet
(one `FleetZone`, three `Robot`s and two `FleetAction`s from
`config/samples/demo_a.yaml`, plus a one-step `FleetTask`), drives the robots Ready,
lets the real scheduler assign work, and ends with a success line once the live
robot has completed an action. It leaves the cluster running so you can
inspect it. On a terminal it first prompts for a scenario — press Enter for the
default.

## The demos / scenarios

Pick a scenario with `SCENARIO=<name>` (skips the prompt):

```bash
make quickstart SCENARIO=hardware-fault
```

Every scenario runs LIVE (`sim-robot-001` driven by a real adapter process) unless
`--projected` selects the status-projected path — see
[What the control plane derives](#what-the-control-plane-derives-and-what-the-runner-writes).

| Scenario | What it shows |
|---|---|
| `healthy-fleet` (default) | 3 robots come up; the one live robot is assigned an action and completes it. The other two actions stay `Pending`: the two robots without an adapter report no position, so none is placed in the zone and candidate filter 2 withholds them |
| `battery-edge` | one robot's battery drains while it keeps its task; the other robots report no position, so it is the only candidate and no comparison is staged (`battery-handoff` stages one) |
| `battery-handoff` | a robot's battery drops mid-task; the task is safely handed off |
| `hardware-fault` | **Demo B** — a camera fails mid-task; capability degrades, the task reroutes, then recovers |
| `comms-flaky` | a robot drops its connection and reconnects; fencing/lease behavior holds |
| `estop-drill` | trigger and confirm an emergency stop (confirmed from ground truth, never inferred) |
| `full-surface` | coverage run exercising every status field / phase / estop / alert path |

`make demo-a` (`kubectl apply -f config/samples/ -l 'swarmada.io/sample-posture!=webhooks-on'`,
everything except the one document that needs admission webhooks) applies the sample fleet and
lets the scheduler assign tasks — but it assumes a cluster with Swarmada already
running and your kubecontext pointing at it, so run it **after** `make quickstart`
(or `make install && make deploy-minimal` against your own cluster), not on a bare
machine.

## Human-in-the-loop, with the live inspector

To step through a scenario and watch every field update live in the `swarmtop`
terminal inspector:

```bash
make demo SCENARIO=hardware-fault
```

This builds and launches `swarmtop` alongside the run (see
[`docs/swarmtop-setup.md`](swarmtop-setup.md)). Quit `swarmtop` with `q`.

## Headless / CI

```bash
make quickstart-test    # runs the quickstart end-to-end, asserts ✅, deletes the cluster
```

For the full coverage run (every status field / phase / estop / alert path), use
the scenario:

```bash
make quickstart SCENARIO=full-surface
```

`make quickstart-test` runs the LIVE `healthy-fleet` path, in GitHub Actions too
(`CI=true` only turns pacing off), so it needs `python3`; the CI job installs it.
The projected path (`--projected`) does not reach its success line at this release: no
projected robot reports a position, so none is placed in the zone, candidate filter 2
withholds every action, and the run exits 1 with `no FleetAction was assigned`
(see *Projected mode* below).

## Inspect what's running

Plain `kubectl`:

```bash
kubectl get robots,fleettasks -n warehouse-a -w      # -w streams live updates
```

Or the optional `swarmtop` terminal inspector (renders the status fields `kubectl`
can't column-ize). Build it from source, then run it in a second terminal:

```bash
make swarmtop                              # -> tools/swarmtop/bin/swarmtop
tools/swarmtop/bin/swarmtop -n warehouse-a
```

`swarmtop` is optional — the demo runs fine without it. `make demo` launches it
automatically when a binary is available and falls back to `kubectl` otherwise.
See [`docs/swarmtop-setup.md`](swarmtop-setup.md).

## Tear down

```bash
bash examples/warehouse-quickstart/run.sh --clean   # delete the cluster + kill any live processes
# or:
kind delete cluster --name swarmada-quickstart
```

## Honest notes — what's real vs. simulated

- The control plane, CRDs, capability derivation, scheduler, and (on LIVE
  scenarios) the ControlStream + adapter path are **real**.
- LIVE scenarios deploy via `config/overlays/quickstart-mtls`, a **dev/demo-only**
  overlay. The ControlStream is mutually authenticated — the adapter's identity
  comes from its client certificate — and per-robot authorization is on: each
  robot-scoped message is checked against that identity, the robot's
  `spec.adapter.name`, and the adapter's `spec.servesRobotClasses`, which lists the
  sample robots' class `simbot-v1`. It is dev/demo-only because its certificates are
  self-signed and its admission webhooks are off.

### What the control plane derives, and what the runner writes

LIVE is the default path: `run.sh` selects it whenever `CI` is unset and
`--projected` was not passed. On that path `sim-robot-001` is driven by a real
`sim_adapter.py` process over a real ControlStream, and its readiness is
**derived by the control plane, not asserted by the runner**:

- **`Robot.status.phase` on `sim-robot-001`.** The Robot reconciler advances a
  live robot off `Discovered` to `Idle` — or to `InProgress` when it already
  holds an assigned action — from fresh adapter liveness
  (`isLivenessOwnedPhase` / `steadyPhaseForLive` in
  `internal/controller/robot_controller.go`, ADR-0029). `run_live_scenario`
  waits for that transition rather than writing it.
- **`FleetAdapter.status.phase: Connected`.** Derived from the adapter's
  verified mTLS identity registering on the ControlStream (`AdapterPresence`).

What the runner still writes to `status` itself, and why:

- `status.phase: Idle` and `status.batteryPercent` on `sim-robot-002` and
  `sim-robot-003` (`run_live_scenario`), in every LIVE scenario except the two
  below. Neither robot has a Fleet Adapter there, so nothing ever reports
  liveness for them and the reconciler's advance never fires; scheduler filter 1
  admits only `Idle` robots. The patch stands in for an absent adapter, not for
  an absent control-plane feature.
- `status.estopState` in the full-surface demo's estop walk
  (`project_estop_states` in `examples/full-surface-demo/run.sh`). RFC-0001
  requires this field to come from a confirmed `EstopAck` and never to be
  inferred (RFC-0001 §9.1.3); the runner infers it, and labels the step
  accordingly.

`battery-handoff` and `hardware-fault` write no `status` at all. They move work
to a spare, so every robot there runs its own `sim_adapter.py` process under its
own `FleetAdapter` identity (`config/overlays/demo-ab-live`), and the spare that
takes the work reports its own readiness and its own completion. The camera fault
comes from the adapter's clock, and every battery value comes from an adapter.
The runner decides one thing: when `battery-handoff` requests the hand-off, with
the `swarmada.io/requeue-requested` annotation.

**Projected mode** — `--projected` — runs
no adapter process at all and deploys `config/overlays/quickstart-projected`
with the ControlStream off. There the runner writes `status.phase: Idle` on all
three robots and `Connected` on the `FleetAdapter` (`project_readiness`), so
only the scheduler's *selection* is exercised. At this release it assigns nothing:
no projected robot reports a position, so none is placed in `warehouse-a` and
candidate filter 2 withholds all three from every action in the zone. The run ends
`❌ Quickstart FAILED: no FleetAction was assigned`.

Writing `status` directly is reserved to controllers (RFC-0001 §9.1.3), and the
RA-1 status-write discipline (RFC-0001 Terminology) makes `status` a controller
projection written only on a material transition. Every patch listed above is a
stand-in for a device that is not present, and the runner names each one as it
runs.
