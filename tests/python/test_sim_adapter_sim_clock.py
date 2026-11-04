# Copyright 2026 The Swarmada Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""The simulated robot moves in wall-clock time whatever the telemetry cadence.

A RegisterAck carries the robot's telemetry interval (C2.3), and a RobotClass may set it
to several seconds. The simulator advanced one fixed step per telemetry payload, so at a
5 s interval simulated time ran at 1/25 of wall time: a 10 m drive at 1 m/s took over
four minutes, and an operator watching a task that never arrived had nothing to read.
Telemetry cadence is what the control plane hears; it must not set how fast the robot
moves.
"""

from __future__ import annotations

import queue
import threading
import time

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # pragma: no cover - depends on --stub-path layout
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

_RUN_S = 1.0


def _run_loop(interval_s: float) -> tuple[float, int]:
    """Run the telemetry loop for _RUN_S at the given cadence. Returns (simulated seconds
    advanced, telemetry payloads sent)."""
    sim = make_simulator("kinematic")
    advanced = [0.0]
    real_tick = sim.tick

    def counting_tick(dt: float) -> None:
        advanced[0] += dt
        real_tick(dt)

    sim.tick = counting_tick
    sim.spawn("amr-1", 0.0, 0.0)  # the real flow spawns in run_control_stream
    a = SimAdapter(pb, None, sim, "amr-1", "z1", "sim")
    a._outbox = queue.Queue()
    a._telemetry_interval = interval_s
    t = threading.Thread(target=a._telemetry_loop, args=(None,), daemon=True)
    t.start()
    time.sleep(_RUN_S)
    a._shutdown.set()
    t.join(timeout=interval_s + 2.0)
    sent = 0
    while not a._outbox.empty():
        if a._outbox.get_nowait().WhichOneof("payload") == "telemetry":
            sent += 1
    return advanced[0], sent


def test_a_slow_telemetry_cadence_does_not_slow_the_robot() -> None:
    advanced, _ = _run_loop(5.0)
    assert advanced >= 0.6 * _RUN_S, (
        f"the simulator advanced {advanced:.1f} s in {_RUN_S:.1f} s of wall time at a 5 s "
        f"telemetry interval; the robot moves at a fraction of its speed")


def test_telemetry_still_follows_the_interval() -> None:
    _, sent = _run_loop(5.0)
    assert sent == 1, f"{sent} telemetry payloads in {_RUN_S:.1f} s at a 5 s interval"


def test_the_default_cadence_is_unchanged() -> None:
    advanced, sent = _run_loop(0.2)
    assert advanced >= 0.6 * _RUN_S
    assert sent >= 3
