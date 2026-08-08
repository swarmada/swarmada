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
- Honours `zone_admission` (§5.4.4 zone-capacity hold/admit) and declines the remaining optional commands with `unsupported = true` (C7), and dials the
  **EdgeStream** with the same confirmed-estop discipline when a zone advertises an
  edge node (C8).

The safety logic lives in [`safety.py`](safety.py) as pure, proto-free code and is
unit-tested in `tests/python/test_sim_adapter.py`.

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

# Validate against the conformance harness (C0–C16):
make conformance-sim

# Run the safety unit tests (C3/C4/C5):
make test-py
```

Inside NVIDIA Isaac Sim's Python runtime, add `--simulator isaac` and wire
`IsaacSimBackend` to the articulation APIs (preserving the confirmed-stop
contract). mTLS/client-cert identity (C1.3) is a deployment concern; the reference
run uses an insecure channel.
