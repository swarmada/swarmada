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

"""KinematicSim's tick() keeps its state current, and it says so to the SDK.

The SDK's estop confirmation needs a state report newer than the stop (state_seq) from a
real robot's binding, and warns about a binding without one. KinematicSim declares
tick_keeps_state_current instead: removing that would log the warning on every
simulated estop.
"""

from __future__ import annotations

import logging

import pytest
from swarmada_sdk import safety
from swarmada_sdk.safety import ESTOP_STOPPED, confirm_estop

from adapters.simulation.simulator import KinematicSim


def test_kinematic_sim_confirms_a_stop_without_a_state_seq_warning(
        monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture) -> None:
    monkeypatch.setattr(safety, "_warned_without_state_seq", set())
    sim = KinematicSim()
    sim.spawn("r1")
    sim.command_move("r1", 5.0, 0.0)
    sim.tick(0.1)
    with caplog.at_level(logging.WARNING, logger="swarmada_sdk.safety"):
        assert confirm_estop(sim, "r1", sleep=lambda _: None) == ESTOP_STOPPED
    assert not [r for r in caplog.records if "has no state_seq" in r.getMessage()]
