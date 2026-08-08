# Quickstart — running the Swarmada demos

From an empty machine to a running Swarmada control plane scheduling work across a
simulated fleet on a local `kind` cluster — in one command. This page is the
single entry point for running **every** demo scenario.

## Prerequisites

- **Docker** (running), **`kind`**, **`kubectl`**, **`make`**, and **Go 1.26+**.
- **`python3`** — only for the LIVE scenarios (a real simulation adapter drives
  one robot). The default `healthy-fleet` path does not need it.
- No cluster required up front — the quickstart creates its own `kind` cluster
  (`swarmada-quickstart`) and tears nothing down outside it and your Go build
  cache.

The quickstart itself is cross-platform (a portable `bash` script; the `make`
targets just call `go`/`docker`/`kind`/`kubectl`). Install the prerequisites for
your OS:

- **macOS** — `make setup-macos` installs everything via Homebrew, then
  `make quickstart`.
- **Linux** — install the prerequisites with your distro's package manager
  (`docker`, `kind`, `kubectl`, `make`, `go` 1.26+, `python3`, `protobuf`), then
  `make quickstart`. (There is no `setup-linux` installer yet; the demo itself is
  unchanged.)
- **Windows** — run under **WSL2** (Windows Subsystem for Linux), not native
  PowerShell, since the quickstart is a bash script. Use Docker Desktop with the
  WSL2 backend, install the Linux prerequisites above inside your WSL2 distro,
  then `make quickstart`.

Generated gRPC stubs are produced automatically by the build (`make proto-go` /
`proto-py`); you don't need to run them by hand.

## Fastest path

```bash
make quickstart
```

This runs a **real** control plane (no mocks) on `kind`, applies the sample fleet
(one `FleetZone`, three `Robot`s, two `FleetTask`s from
`config/samples/demo_a.yaml`), drives the robots Ready, lets the real scheduler
assign the tasks, and ends with a ✅. It leaves the cluster running so you can
inspect it. On a terminal it first prompts for a scenario — press Enter for the
default.

## The demos / scenarios

Pick a scenario with `SCENARIO=<name>` (skips the prompt):

```bash
make quickstart SCENARIO=hardware-fault
```

| Scenario | What it shows | LIVE? |
|---|---|---|
| `healthy-fleet` (default) | 3 robots come up, the scheduler assigns both tasks | no (status-projected) |
| `battery-edge` | the scheduler avoids assigning to a draining robot | yes |
| `battery-handoff` | a robot's battery drops mid-task; the task is safely handed off | yes |
| `hardware-fault` | **Demo B** — a camera fails mid-task; capability degrades, the task reroutes, then recovers | yes |
| `comms-flaky` | a robot drops its connection and reconnects; fencing/lease behavior holds | yes |
| `estop-drill` | trigger and confirm an emergency stop (confirmed from ground truth, never inferred) | yes |
| `full-surface` | coverage run exercising every status field / phase / estop / alert path | yes |

`make demo-a` (`kubectl apply -f config/samples/`) applies the sample fleet and
lets the scheduler assign tasks — but it assumes a cluster with Swarmada already
running and your kubecontext pointing at it, so run it **after** `make quickstart`
(or `make install && make deploy` against your own cluster), not on a bare
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

CI (`make quickstart-test`) forces the `healthy-fleet` status-projected path, so
the gate never depends on a live adapter.

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
- `healthy-fleet` projects robot readiness with a direct `kubectl patch` — no
  adapter runs. It's the one path CI always exercises.
- LIVE scenarios drive **`sim-robot-001`** with a real `sim_adapter.py` process;
  the other two robots are patched to a healthy baseline so the contrast is real.
- LIVE scenarios deploy via `config/overlays/quickstart-dev`, a **dev/demo-only**
  overlay that disables per-robot ControlStream authorization — never a
  production configuration.

Scenario-by-scenario detail (what each proves, what's simulated) is in
[`examples/warehouse-quickstart/SCENARIOS.md`](../examples/warehouse-quickstart/README.md).
