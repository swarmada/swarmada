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

"""Shared simulator-scenario presets for Fleet Adapter simulated backends.

A scenario is a small YAML preset (see ``presets/``) that decides *when* a
simulated robot emits which value on the fields the control plane already
consumes — fleet size, capability/hardware manifest, battery curve, position
pattern, fault timeline, comms behaviour, and estop trigger. The same preset name
and behaviour is intended to drive every simulated backend (the in-tree
``KinematicSim`` first; the ROS 2 / VDA5050 / MAVLink simulated backends after),
so the pieces here are deliberately proto-free and dependency-light.
"""

from adapters.scenarios.loader import (
    HW_DEGRADED,
    HW_FAILED,
    HW_HEALTHY,
    HardwareFaultOverrides,
    HardwareState,
    Scenario,
    ScenarioEngine,
    load_scenario,
)

__all__ = [
    "HW_DEGRADED",
    "HW_FAILED",
    "HW_HEALTHY",
    "HardwareFaultOverrides",
    "HardwareState",
    "Scenario",
    "ScenarioEngine",
    "load_scenario",
]
