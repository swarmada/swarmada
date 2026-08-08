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

"""Per-robot telemetry cadence from RegisterAck (C2.3).

The control plane resolves an interval through SwarmadaConfig -> RobotClass ->
Robot.spec and returns it on RegisterAck (internal/registrar/registrar.go). The
adapter honouring it is the last hop of that path, and it was silently missing:
the simulation adapter used a hard-coded constant and ignored the field.

Covered here rather than in the conformance harness because the interesting cases
are the BOUNDS. Asserting a 30s cadence over the wire costs a 60s run; asserting
that the adapter adopted 30 costs nothing. The conformance suite still measures a
real 1s cadence end-to-end -- this fills in the range around it.
"""

from __future__ import annotations

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # pragma: no cover - depends on --stub-path layout
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402


def _adapter() -> SimAdapter:
    return SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim")


def _register_ack(seconds: int):
    return pb.ControlPlaneMessage(
        register_ack=pb.RegisterAck(accepted=True, telemetry_interval_seconds=seconds))


def test_adopts_the_advertised_interval() -> None:
    a = _adapter()
    before = a._telemetry_interval
    a._handle_control_plane(_register_ack(5), None)
    assert a._telemetry_interval == 5.0, (
        f"adapter kept {before}s instead of adopting the advertised 5s; the whole "
        f"SwarmadaConfig -> RobotClass -> Robot.spec resolution never reaches the robot"
    )


def test_adopts_the_documented_upper_bound() -> None:
    # 30s is the CRD maximum for Robot.spec.telemetryIntervalSeconds. The demos have
    # only ever run at the adapter default, so this bound is otherwise untested.
    a = _adapter()
    a._handle_control_plane(_register_ack(30), None)
    assert a._telemetry_interval == 30.0


def test_adopts_the_documented_lower_bound() -> None:
    a = _adapter()
    a._handle_control_plane(_register_ack(1), None)
    assert a._telemetry_interval == 1.0


def test_unset_interval_leaves_the_default_untouched() -> None:
    # THE DEMO-SAFETY CASE. No sample or example sets telemetryIntervalSeconds, so
    # Robot.spec carries nil, the ack carries 0, and the adapter must keep its own
    # fast default. If 0 were adopted the telemetry loop would spin without sleeping.
    a = _adapter()
    default = a._telemetry_interval
    a._handle_control_plane(_register_ack(0), None)
    assert a._telemetry_interval == default
    assert a._telemetry_interval > 0, "a zero interval would busy-loop the telemetry thread"


@pytest.mark.parametrize("advertised,expected", [(0, None), (45, 30.0), (-5, None)])
def test_out_of_range_values_are_clamped_or_ignored(advertised, expected) -> None:
    """Values outside 1-30 must never produce a nonsensical cadence.

    The CRD bounds the field, but an adapter cannot assume every control plane it
    meets does -- and a negative or huge interval is worse than the default either
    way: negative would busy-loop, and an hour-long gap would look like an outage
    to a Health Monitor watching for telemetry.
    """
    a = _adapter()
    default = a._telemetry_interval
    a._handle_control_plane(_register_ack(advertised), None)
    assert a._telemetry_interval == (default if expected is None else expected)
    assert 0 < a._telemetry_interval <= 30.0
