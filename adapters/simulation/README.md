# Simulation Fleet Adapter (reference, in-tree)

A **feature-basic but safety-complete** Fleet Adapter that drives a *simulated*
robot instead of real hardware. Per [ADR-0005](../../docs/adr/0005-reference-adapter-policy.md)
a simulation adapter lives in the core tree as test/demo infrastructure; it keeps
the protocol and the [conformance suite](../CONFORMANCE.md) honest with a real,
runnable adapter that costs **$0** to build and run.

## What it does

- Speaks `fleet_adapter.v1`: opens `ControlStream` + `SafetyStream` together (C1),
  registers a robot (C2), and streams live telemetry from the simulator (C6).
- **Safety-complete** — the CONFORMANCE.md MUSTs are real, never faked:
  - **C3** fencing-token ordering (`FenceGuard`): missing → `MISSING`, `<=`highest
    → `STALE`, identical re-delivery → idempotent re-ack, else accept + persist.
  - **C4** assignment-lease self-stop (`LeaseMonitor`): a lease not renewed within
    its duration brings the task to a safe stop; a stale `lease_generation` renewal
    is ignored.
  - **C5** confirmed emergency stop (`confirm_estop`): reports `STOPPED` only when
    the simulator confirms **zero velocity** (ground truth) — never inferred from a
    timeout. If it cannot confirm, it reports `FAILED`.
- With `--latch`, keeps the stop latch of contract `1.4.0` (RFC-0001
  `#fleet-adapter-protocol-the-stop-latch`, conformance `C5.8`) and reports `1.4.0`: after a stop
  it refuses work `ROBOT_UNAVAILABLE` until the stop's `EstopClear` lifts it. Without `--latch` it
  keeps no latch and reports `1.3.0`. The latch is off by default because the reference control
  plane ends no local set at v0.4, so a latching adapter there that took an edge stop takes no work
  until it restarts. `make conformance-sim` runs it with `--latch`.
- Honours `zone_admission` (RFC-0001 #control-plane-zone-controller zone-capacity hold/admit) and declines the remaining optional commands with `unsupported = true` (C7), and dials the
  **EdgeStream** with the same confirmed-estop discipline when a zone advertises an
  edge node (C8).

The safety logic is not in this directory. It comes from
[`swarmada_sdk.safety`](../../sdk/python/src/swarmada_sdk/safety.py) — pure, proto-free
code that every adapter shares, reference and vendor alike (ADR-0005). This adapter used
to carry its own copy; that copy froze before `FenceGuard.adopt()` was added and C3.8
failed here while passing for anything generated from `adapters/template/`, which has
always imported the SDK. One audited implementation, no second copy to drift.

It is unit-tested from two directions:
[`sdk/python/tests/test_safety.py`](../../sdk/python/tests/test_safety.py) covers the
primitives themselves, and the `tests/python/test_sim_adapter_*.py` files cover this
adapter's behaviour on the wire: completing an action only on arrival and with the
action's own fencing token, the cancel disposition, the action catalog, and the
telemetry interval taken from `RegisterAck`. `make test-py` runs both.

## Multi-simulator

The adapter is simulator-agnostic — it drives a small `Simulator` interface
([`simulator.py`](simulator.py)):

- `KinematicSim` — the **$0 default**: a dependency-free 2-D kinematic model with a
  *real* decelerating stop. No external simulator or licence required.
- `IsaacSimBackend` — the seam for NVIDIA Isaac Sim (and the pattern any other
  simulator follows). It lazily imports the Isaac runtime and errors clearly if it
  is absent; it never fakes physics.

## Several robots on one connection

One `ControlStream` carries every robot an adapter manages, and each message names
its robot (RFC-0001, *The ControlStream service*). Give `--robot-id` more than once
and this adapter serves every robot named, on one `ControlStream` and one
`SafetyStream`, in one simulated world:

```bash
PYTHONPATH=proto python3 adapters/simulation/sim_adapter.py --endpoint localhost:9090 \
  --robot-id amr-1 --robot-id amr-2 --robot-id amr-3
```

With one `--robot-id`, or none (`robot-conformance-1`), the adapter runs exactly as it
always has; `tests/python/test_sim_adapter_golden_transcript.py` pins its traffic.
With two or more, `SimFleet` in [`sim_adapter.py`](sim_adapter.py) holds the
connection, and each robot keeps its own fencing token, lease, stop latch and action:

- a command, a stop or a clear acts on the robot whose `robot_id` it names and on no
  other. One naming a robot the adapter does not serve reaches no robot: the
  robot-acting commands are refused, as the single-robot adapter refuses them, and an
  `Estop` or a fence for it is answered `ESTOP_STATE_FAILED`, since nothing was
  stopped;
- a stop is commanded the moment it arrives and its rest is confirmed in that robot's
  own lane, so a stop for every robot is commanded for each before any one robot's rest
  is awaited, and no robot's stop waits on another's;
- a `RegisterAck` names no robot, so each is paired with the register it answers, in
  the order the registers went out;
- an edge-issued `Estop` stops every robot whose `RegisterAck` listed that edge node,
  with one ack, `STOPPED` only once all of them are at rest.

Every robot runs the same `--scenario`, and all of them start at the origin.

## Run

```bash
# $0, dependency-free (KinematicSim):
PYTHONPATH=proto python3 adapters/simulation/sim_adapter.py --endpoint localhost:9090

# Validate against the conformance harness (C0–C20), with --latch:
make conformance-sim
# The same, one adapter serving two robots (multi-robot mode, C20):
make conformance-sim-multi

# Run the safety unit tests (C3/C4/C5) — repo suite and the SDK's own:
make test-py

# The SDK suite alone (separate distribution, its own pytest config):
make test-sdk
```

Inside NVIDIA Isaac Sim's Python runtime, add `--simulator isaac` and wire
`IsaacSimBackend` to the articulation APIs (preserving the confirmed-stop
contract). mTLS/client-cert identity (C1.3) is a deployment concern; the reference
run uses an insecure channel.
