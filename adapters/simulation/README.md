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

## Run

```bash
# $0, dependency-free (KinematicSim):
PYTHONPATH=proto python3 adapters/simulation/sim_adapter.py --endpoint localhost:9090

# Validate against the conformance harness (C0–C19), with --latch:
make conformance-sim

# Run the safety unit tests (C3/C4/C5) — repo suite and the SDK's own:
make test-py

# The SDK suite alone (separate distribution, its own pytest config):
make test-sdk
```

Inside NVIDIA Isaac Sim's Python runtime, add `--simulator isaac` and wire
`IsaacSimBackend` to the articulation APIs (preserving the confirmed-stop
contract). mTLS/client-cert identity (C1.3) is a deployment concern; the reference
run uses an insecure channel.
