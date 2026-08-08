# Simulator scenario presets

Shared, protocol-agnostic scenario presets for the Fleet Adapter **simulated**
backends. A scenario decides *when* a simulated robot emits which value on the
fields the control plane already consumes — so a user picks a behaviour by name
and gets the same story whichever simulated backend they run.

The loader (`loader.py`) is deliberately **proto-free**: it parses the YAML into
dataclasses and exposes a pure `ScenarioEngine`. Each adapter maps the engine's
output onto its own wire protocol. Today the in-tree `adapters/simulation`
(`KinematicSim`) consumes it; the ROS 2 / VDA5050 / MAVLink simulated backends are
the next sequencing steps.

## Preset format

A preset is a YAML file in `presets/`, selected by its filename stem
(`--scenario healthy-fleet`) or an explicit path. Every knob below is part of the
format; a given backend consumes the subset it supports.

```yaml
name: hardware-fault            # required; matches the filename stem
description: >                  # human-readable, shown in the quickstart picker
  camera_front degrades at T+30s and recovers at T+90s.

fleet:
  size: 3                       # number of robots (where the backend supports it)
  robot_id_prefix: robot-sim    # IDs become <prefix>-1 .. <prefix>-N

manifest:
  capabilities:                 # declared capability set (dotted, lowercase)
    - navigation.2d
    - perception.camera
  hardware:                     # component inventory
    - name: camera_front        # unique within the robot
      type: Camera              # Lidar | Camera | Gripper | ...
      model: sim-cam-v1         # informational
  installed_models: []          # [{name, version}, ...]

battery:                        # battery curve (consumed by battery-edge)
  start_percent: 100
  drain_per_second: 0.05
  floor_percent: 5

position:
  pattern: patrol               # static | line | patrol

fault_timeline:                 # ordered hardware transitions (the Demo B knob)
  - component: camera_front
    at_seconds: 30              # seconds since telemetry start
    status: DEGRADED            # HEALTHY | DEGRADED | FAILED
    reason: simulated camera degradation
    affects_capabilities: [perception.camera]
  - component: camera_front
    at_seconds: 90
    status: HEALTHY

comms:                          # consumed by comms-flaky
  gaps: []                      # telemetry-only outages: [{start_seconds, end_seconds}]
  drop:                         # OPTIONAL full ControlStream drop + reconnect
    at_seconds: 20              #   tear the stream down at T+20s
    reconnect_seconds: 35       #   re-handshake at T+35s

estop:                          # trigger: none | on_command | at_seconds (estop-drill)
  trigger: none
```

### Bundled presets

| Preset | Story | Proto field(s) it drives |
|---|---|---|
| `healthy-fleet` (default) | N robots, full capability set, slow drain, nothing degrades. | — |
| `hardware-fault` | `camera_front` DEGRADED at T+30s, recovers at T+90s — Demo B (reroute + recovery). | `TelemetryPayload.hardware`, `CapabilitiesSnapshot` |
| `battery-edge` | One robot starts low (22%) and drains fast past a task's `minBatteryPct`; the scheduler avoids it. | `TelemetryPayload.battery.percent` |
| `comms-flaky` | One robot's ControlStream drops at T+20s and reconnects at T+35s (Hello/Register re-handshake). | stream torn down + reconnected; `TelemetryPayload` withheld while down |
| `estop-drill` | After a timer the robot performs a confirmed emergency stop via `confirm_estop` (never faked). | `AdapterSafetyMessage.estop_ack` |

Two scope notes on the last two, both surfaced deliberately:

- **`estop-drill`** is adapter-initiated (a demo affordance) and reports an
  `EstopAck` with a synthetic `drill-<robot>` id. The *stop itself* is real —
  `safety.confirm_estop` waits on the simulator's ground-truth `is_stopped`, never a
  timeout. On a live cluster the control plane initiates estop (RA-6/C5); the drill's
  unsolicited ack is for the local demo, while the safe-hold is genuine.
- **`comms-flaky`** does a full **bidirectional stream drop**: at `comms.drop.at_seconds`
  the adapter cancels the ControlStream RPC and, at `reconnect_seconds`, re-establishes
  it with a fresh Hello/Register — exercising the `RegisterRobot` reconnect, the
  persisted fencing high-water mark (C3), and lease self-stop during the outage (C4).
  A telemetry-only gap (no reconnect) is still available via `comms.gaps`. The reconnect
  loop is **gated behind `comms.drop`**: every other preset and the conformance run take
  the unchanged single-stream code path.

## Using it (in-tree simulation adapter)

```
python3 adapters/simulation/sim_adapter.py --endpoint localhost:9090 \
  --scenario hardware-fault \
  --fault-component camera_front --fault-at 30 --recover-at 90
```

- `--scenario` — preset name (default `healthy-fleet`) or a path to a YAML file.
- `--fault-component` / `--fault-at` / `--recover-at` — override the `hardware-fault`
  timeline. **Applied only when set**; leaving them unset uses the preset's own
  `fault_timeline` verbatim (the `hardware-fault` preset bakes in `camera_front` /
  `30` / `90`). This gating matters for multi-event coverage presets like
  `full-surface`, whose `DEGRADED→FAILED→HEALTHY` timeline the single-shape override
  would otherwise collapse. Inert on presets with no timeline.

What the adapter emits from a scenario:
- a `CapabilitiesSnapshot` with the full hardware manifest, once after registration;
- `TelemetryPayload.hardware` updates, delta-compressed (all components on the first
  payload, then only the fault/recover transitions).

The safety behaviour (C3 fencing, C4 lease self-stop, C5 confirmed estop) is
**untouched** — scenarios are additive tooling, not a safety requirement (ADR-0005).
If scenario support is unavailable (e.g. PyYAML not installed) the adapter logs a
warning and runs in pure safety/telemetry mode, so the conformance gate stays green.

## Running under conformance

```
make conformance-sim         # C0–C16, healthy-fleet (default)
make conformance-sim-fault   # C0–C16 while exercising the hardware-fault path
```

`conformance-sim-fault` uses fast timers (`FAULT_AT=2 RECOVER_AT=4`, overridable).

## Environment note (two-Python trap)

`make` uses `$(PYTHON)` (`python3`); `make test-py` runs bare `pytest`. On a machine
with more than one Python (a system/Homebrew install alongside your project `.venv`) these can resolve to *different*
interpreters, and only one may have all deps:

- the scenario loader needs **PyYAML** (declared in `pyproject.toml` dependencies);
- the conformance harness/adapter needs **grpcio ≥ 1.81.1** (matching the generated
  stubs).

Activate your project `.venv` (Python 3.12) so both resolve to it:

```
make install-py                       # installs into $(PYTHON); falls back to
                                      # --break-system-packages on Homebrew/PEP 668
# or target a specific interpreter with both deps:
make PYTHON=/path/to/python3 conformance-sim-fault
```

If an interpreter is missing PyYAML the adapter still runs (degraded, no scenario);
if it is missing a compatible grpcio the harness itself won't start — upgrade with
`python3 -m pip install -U "grpcio>=1.81.1"`.

## Tests

- `tests/python/test_scenarios.py` — the pure loader/engine (timeline, overrides,
  delta compression, errors). Proto-free; runs on any interpreter.
- `tests/python/test_sim_adapter_scenario.py` — asserts the adapter maps the fault
  onto the proto fields. Skips automatically if the interpreter lacks grpc.
