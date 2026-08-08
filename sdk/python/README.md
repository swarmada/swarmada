# swarmada-sdk (Python)

The shared building blocks for writing a Swarmada **Fleet Adapter** in Python.

Its purpose is to provide **one audited implementation of the safety-critical
behaviours every adapter must get right**, so that reference and vendor adapters
depend on the same correct code instead of each re-deriving it. The vendor
adapter template (`adapters/template`) generates an adapter already wired to this
package, and every reference adapter (ROS 2, VDA5050, MAVLink) imports it.

## Install

```bash
pip install -e sdk/python
# On a Homebrew-managed macOS Python, add --break-system-packages
```

Requires Python 3.12+. The package ships type information (PEP 561), so `mypy`
sees the full API.

## What it provides

The public surface is intentionally small and stable: the three safety MUSTs from
the conformance suite (`adapters/CONFORMANCE.md`), as pure, proto-free logic you
wire into your adapter.

| Import | Conformance | What it enforces |
| :----- | :---------- | :--------------- |
| `FenceGuard` / `FenceDecision` | **C3** | Fencing-token ordering — reject stale or out-of-order task assignments. |
| `LeaseMonitor` | **C4** | Assignment-lease self-stop — stop the robot when the control plane stops renewing its lease. |
| `confirm_estop`, `ESTOP_STOPPED`, `ESTOP_FAILED` | **C5** | Confirmed emergency stop — verify the robot actually halted from its own ground truth, never inferred. |

### C3 — fencing-token ordering (`FenceGuard`)

The control plane stamps each assignment with a monotonically increasing token.
An adapter must reject a delayed or duplicate assignment, or a robot could act on
a superseded command.

```python
from swarmada_sdk import FenceGuard, FenceDecision

guard = FenceGuard()
guard.check("robot-1", has_token=True,  token=5, assignment_id="a1")  # ACCEPT
guard.check("robot-1", has_token=True,  token=5, assignment_id="a1")  # REACK  — idempotent re-delivery
guard.check("robot-1", has_token=True,  token=3, assignment_id="a2")  # STALE  — older token, reject
guard.check("robot-1", has_token=False, token=0, assignment_id="a3")  # MISSING — no token ≠ token 0
```

Adopt the assignment on `ACCEPT`, ignore on `REACK`, and reject on `STALE`/`MISSING`.
A real adapter persists the accepted high-water mark so ordering survives a restart.

### C4 — assignment-lease self-stop (`LeaseMonitor`)

A held execution lease is valid only while the control plane keeps renewing it. If
renewals stop (a crash or network partition), the robot must self-stop — it cannot
keep running a task nobody is supervising.

```python
from swarmada_sdk import LeaseMonitor

monitor = LeaseMonitor(on_expiry=lambda robot: adapter.safe_stop(robot))
monitor.grant("robot-1", duration_s=10, generation=1)

# When a renewal arrives:
monitor.renew("robot-1", duration_s=10, generation=2)   # stale generations are ignored

# Call from the adapter's telemetry loop (timer-free, so it is deterministically testable):
monitor.tick()   # once the deadline passes with no renewal → fires on_expiry → safe-stop
```

### C5 — confirmed emergency stop (`confirm_estop`)

The most safety-critical primitive. When a stop is commanded, the adapter must
confirm the robot **actually** stopped from its own ground truth — never infer
"stopped" from a timeout or from silence.

```python
from swarmada_sdk import confirm_estop, ESTOP_STOPPED

# `binding` exposes command_stop(robot), tick(dt) and is_stopped(robot),
# bound to the robot's real confirmed-halt signal (or a simulator in tests).
result = confirm_estop(binding, "robot-1")
if result == ESTOP_STOPPED:
    ...        # confirmed at rest
else:          # ESTOP_FAILED
    escalate() # could not confirm within the bound
```

This prevents the worst adapter bug: reporting "stopped" merely because the stop
*command* was sent while the robot kept moving.

## Why a shared package

These three behaviours are subtle and safety-critical — the cases where a bug means
a robot does not stop when it should. Implementing them once, here, means:

- one place to audit and fix, rather than N diverging copies across adapters;
- the conformance harness's C3/C4/C5 checks verify the same code everywhere;
- a vendor generating an adapter from `adapters/template` starts conformant on the
  safety MUSTs, and can decline optional commands (`unsupported=true`) without
  weakening them.

## Versioning

Versioned independently of the control plane (see
[`docs/adr/0002-repository-topology.md`](../../docs/adr/0002-repository-topology.md)):
a control-plane release does not force an SDK release, and vice versa. The safety
surface above is stable; additional client helpers (for the `api/v1` custom
resources and the `fleet_adapter.v1` gRPC contract) may be added over time without
changing it.

## Layout

```
sdk/python/
├── pyproject.toml              # package metadata + tooling (own version)
├── src/swarmada_sdk/
│   ├── __init__.py             # public entrypoint; re-exports the safety API + __version__
│   ├── safety.py               # FenceGuard, LeaseMonitor, confirm_estop (C3/C4/C5)
│   └── py.typed                # PEP 561 marker: ships type information
└── tests/
    ├── test_safety.py          # the safety primitives
    └── test_smoke.py           # import + version sanity check
```

The `src/` layout is deliberate: it stops the working directory from shadowing the
installed package, so tests run against what users actually install.

## Development

```bash
python -m pip install -e '.[dev]'   # editable install with dev tools
pytest                              # run tests
ruff check . && black --check . && mypy src
```
