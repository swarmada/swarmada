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
targets only call `go`/`docker`/`kind`/`kubectl`). Install the prerequisites for
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
assign the tasks, and ends with a success line. It leaves the cluster running so you can
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
- LIVE scenarios deploy via `config/overlays/quickstart-dev`, a **dev/demo-only**
  overlay that disables per-robot ControlStream authorization — never a
  production configuration.

### The one shortcut you should know about: `Robot.status.phase` is patched by hand

**Every scenario — including the LIVE ones, and including `sim-robot-001` —
reaches `phase: Idle` because the runner writes that field itself:**

```bash
kubectl patch robot/<name> -n warehouse-a --subresource=status --type=merge \
  -p '{"status":{"phase":"Idle"}}'
```

`examples/warehouse-quickstart/run.sh:899`, `:937`, `:1040`;
`examples/full-surface-demo/run.sh:429`, `:476`, `:492`.

This is a **simulation shortcut, and the control plane does not perform it.** RFC-0001
says status is controller-owned and operators must not write it
(RFC-0001 §9.1.3), and the RA-1 status-write discipline
(RFC-0001 Terminology) makes status a controller projection written
only on a material transition. The runner is doing what the specification forbids.

It is here because of a **known control-plane gap**: nothing transitions a `Robot` from
`Discovered` to `Idle`. RFC-0001 requires that transition
(RFC-0001 §9.1.2) but assigns it to no component —
the Robot reconciler writes `phase` only to `Offline` (RFC-0001 §9.3.3), the Zone
Controller only to `Maintenance` (RFC-0001 §9.3.4), and the Scheduler only reads it
(RFC-0001 §9.3.2). In the code, the sole writer of `Idle`
(`internal/controller/fleetaction_controller.go:1450`) fires on *task release* and requires
the robot to have already been `Assigned`/`InProgress`. Since scheduler filter 1
(RFC-0001 §9.3.2, `internal/scheduler/scheduler.go:131`) admits only `Idle` robots,
without the patch no robot is ever schedulable and no task is ever assigned.

**What that means for reading this demo:** the scheduler's *selection* — filters, ranking,
assignment, lease, reroute — is real and unmodified. The robot's *readiness* is asserted by
the runner, not derived by the control plane. Do not read a green quickstart as evidence that
robot admission works end to end.

Two smaller projections, same rule, same reason:

- `status.hardware[]` on the `hardware-fault` scenario
  (`examples/warehouse-quickstart/run.sh:1272`, `:1296`) — the live adapter's hardware
  telemetry does not reach `status.hardware`, so the runner writes it. Everything downstream of
  the capability degrading (safe-stop, requeue, reroute) is real control-plane code.
- `status.estopState` in the full-surface gate (`examples/full-surface-demo/run.sh:373-380`) —
  the real estop path needs mTLS adapter identity, which the dev overlay disables. RFC-0001
  requires this field to come from a confirmed `EstopAck` and never be inferred
  (RFC-0001 §9.1.3); the runner infers it, and labels the step accordingly.
