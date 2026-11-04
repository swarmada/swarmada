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

"""The provisioning loop for a robot that is not yet admitted.

RFC-0001 #fleet-adapter-protocol-connection-lifecycle ("Provisioning loop") and
#discoveredrobot-controller-behavior-on-admit ("Post-admit sequence"): nothing tells
an adapter its robot was admitted, so while a robot is unadmitted the adapter re-sends
`register` every I seconds and re-announces on each NOT_ADMITTED. Admission is picked
up on the next iteration.

The simulation adapter used to announce once and then send nothing, so a robot
admitted after it connected was never registered on that stream. Its
CapabilitiesSnapshot had been sent while no Robot existed, the control plane dropped
it, and FleetAdapter.status.supportedActions stayed empty until the adapter restarted.
"""

from __future__ import annotations

import pathlib
import queue
import subprocess
import sys

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # pragma: no cover - depends on --stub-path layout
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

_ROOT = pathlib.Path(__file__).resolve().parents[2]
_SIM_ADAPTER = _ROOT / "adapters" / "simulation" / "sim_adapter.py"


class _ManualTimers:
    """Stands in for threading.Timer: records each scheduled call so a test fires it."""

    def __init__(self) -> None:
        self.pending: list[tuple[float, object]] = []

    def __call__(self, delay: float, fn) -> None:
        self.pending.append((delay, fn))

    def fire_all(self) -> None:
        due, self.pending = self.pending, []
        for _delay, fn in due:
            fn()


def _adapter() -> tuple[SimAdapter, _ManualTimers]:
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim")
    timers = _ManualTimers()
    a._schedule = timers
    a._outbox = queue.Queue()
    return a, timers


def _drain(a: SimAdapter) -> list[str]:
    kinds = []
    while True:
        try:
            kinds.append(a._outbox.get_nowait().WhichOneof("payload"))
        except queue.Empty:
            return kinds


def _not_admitted():
    return pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
        accepted=False, rejection=pb.REGISTRATION_REJECTION_NOT_ADMITTED))


def _accepted():
    return pb.ControlPlaneMessage(register_ack=pb.RegisterAck(accepted=True))


def _discover_ack(ttl_seconds: int = 0):
    return pb.ControlPlaneMessage(discover_ack=pb.DiscoverAck(
        accepted=True, ttl_seconds=ttl_seconds))


def test_an_unadmitted_robot_registers_again_after_the_interval() -> None:
    a, timers = _adapter()
    a._handle_control_plane(_not_admitted(), None)
    assert _drain(a) == ["discover"]
    assert [d for d, _ in timers.pending] == [60.0], (
        "after NOT_ADMITTED the adapter must schedule the next register at I = 60 s "
        "(ttl_seconds is 0 at this version); it scheduled nothing, so an admission "
        "made after it connected is never picked up")
    timers.fire_all()
    assert _drain(a) == ["register"]


def test_each_not_admitted_re_announces() -> None:
    # The announce keeps the DiscoveredRobot's TTL fresh, so it repeats per iteration.
    a, timers = _adapter()
    a._handle_control_plane(_not_admitted(), None)
    timers.fire_all()
    a._handle_control_plane(_not_admitted(), None)
    assert _drain(a) == ["discover", "register", "discover"]


def test_admission_on_a_later_iteration_sends_the_catalog() -> None:
    # The snapshot sent at hello time reached a control plane with no Robot for this id,
    # which drops it. The first accepted register after a refusal must send it again.
    a, timers = _adapter()
    a._handle_control_plane(_not_admitted(), None)
    timers.fire_all()
    _drain(a)
    a._handle_control_plane(_accepted(), None)
    assert _drain(a) == ["capabilities"], (
        "an admission picked up by the provisioning loop must be followed by a "
        "CapabilitiesSnapshot, or FleetAdapter.status.supportedActions stays empty")
    assert timers.pending == [], "an admitted robot leaves the provisioning loop"


def test_an_admitted_robot_does_not_resend_its_catalog_on_register() -> None:
    # The ordinary connect already sends register then the snapshot (hello_ack); an
    # accepted register without an earlier refusal adds nothing to the stream.
    a, timers = _adapter()
    a._handle_control_plane(_accepted(), None)
    assert _drain(a) == []
    assert timers.pending == []


def test_a_timer_left_over_from_admission_sends_nothing() -> None:
    a, timers = _adapter()
    a._handle_control_plane(_not_admitted(), None)
    _drain(a)
    a._handle_control_plane(_accepted(), None)
    _drain(a)
    timers.fire_all()
    assert _drain(a) == []


def test_a_timer_from_a_replaced_stream_does_not_write_to_the_new_one() -> None:
    # Each ControlStream session gets a fresh outbox, and the new session's hello_ack
    # registers on its own. An iteration scheduled on the old stream must not add a
    # second register to the new one.
    a, timers = _adapter()
    a._handle_control_plane(_not_admitted(), None)
    a._outbox = queue.Queue()
    timers.fire_all()
    assert _drain(a) == []


@pytest.mark.parametrize("ttl,interval", [(0, 60.0), (100, 50.0), (1000, 300.0)])
def test_the_interval_follows_the_announced_ttl(ttl: int, interval: float) -> None:
    # I = min(ttl_seconds/2, 300); 0 means TTL disabled and I = 60 s.
    a, timers = _adapter()
    a._handle_control_plane(_discover_ack(ttl), None)
    a._handle_control_plane(_not_admitted(), None)
    assert [d for d, _ in timers.pending] == [interval]


# --provisioning-interval: a deployment may shorten I (the first-run demo uses 5 s so a
# fresh robot is picked up within seconds of admission). Unset, the RFC's rule applies.

def _adapter_with_interval(seconds: float) -> tuple[SimAdapter, _ManualTimers]:
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim",
                   provisioning_interval_s=seconds)
    timers = _ManualTimers()
    a._schedule = timers
    a._outbox = queue.Queue()
    return a, timers


def test_a_set_interval_replaces_the_default() -> None:
    a, timers = _adapter_with_interval(5.0)
    a._handle_control_plane(_not_admitted(), None)
    assert [d for d, _ in timers.pending] == [5.0]


@pytest.mark.parametrize("ttl,interval", [(0, 5.0), (100, 5.0), (4, 2.0)])
def test_a_set_interval_never_outlasts_half_the_ttl(ttl: int, interval: float) -> None:
    # Announcing less often than TTL/2 would let the DiscoveredRobot expire while the
    # robot is still connected, so the TTL's bound still holds under the flag.
    a, timers = _adapter_with_interval(5.0)
    a._handle_control_plane(_discover_ack(ttl), None)
    a._handle_control_plane(_not_admitted(), None)
    assert [d for d, _ in timers.pending] == [interval]


def _run_cli(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(_SIM_ADAPTER), *args],
        capture_output=True, text=True, timeout=30, cwd=_ROOT)


def test_the_flag_is_documented() -> None:
    out = _run_cli("--help")
    assert "--provisioning-interval" in out.stdout


@pytest.mark.parametrize("value", ["0", "-5", "soon"])
def test_the_flag_refuses_a_value_that_is_not_a_positive_number(value: str) -> None:
    out = _run_cli("--provisioning-interval", value)
    assert out.returncode == 2
    assert "--provisioning-interval: must be a positive number of seconds" in out.stderr
