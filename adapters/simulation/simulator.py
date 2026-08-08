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

"""Simulator backends for the reference simulation adapter.

The adapter is simulator-agnostic: it drives a `Simulator` through a small,
proto-free interface. Two backends ship here:

* `KinematicSim` — a dependency-free 2-D kinematic model. It is the **$0**
  default: it builds, runs, and is unit-testable with no external simulator or
  licence. Crucially it models a **real** stop — `command_stop` decelerates the
  robot and `is_stopped` reports *confirmed* zero velocity (ground truth), so the
  adapter's emergency stop is confirmed, never inferred.
* `IsaacSimBackend` — the seam for NVIDIA Isaac Sim (and the pattern any other
  simulator follows). It lazily imports the Isaac runtime and raises a clear
  error if it is not installed; it never fakes physics.

Keeping this interface tiny is what makes the adapter multi-simulator.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Protocol


@dataclass
class Pose:
    """A robot's planar pose. Floor is discrete; x/y are metres in the zone frame."""

    x: float = 0.0
    y: float = 0.0
    yaw: float = 0.0
    floor: int = 0


class Simulator(Protocol):
    """The proto-free contract the adapter drives. Any simulator implements it."""

    def spawn(self, robot_id: str, x: float = 0.0, y: float = 0.0) -> None: ...

    def command_move(self, robot_id: str, target_x: float, target_y: float) -> None:
        """Begin driving the robot toward a target (task dispatch)."""

    def command_stop(self, robot_id: str) -> None:
        """Command a safe stop (estop / lease expiry / cancel). Not confirmation."""

    def tick(self, dt: float) -> None:
        """Advance the physics by dt seconds."""

    def is_stopped(self, robot_id: str) -> bool:
        """CONFIRMED zero velocity — the ground truth the adapter waits on before
        reporting STOPPED. Never a timeout or an inference."""

    def pose(self, robot_id: str) -> Pose: ...

    def battery_percent(self, robot_id: str) -> int: ...


@dataclass
class _RobotState:
    pose: Pose
    vx: float = 0.0
    vy: float = 0.0
    target: tuple[float, float] | None = None
    stopping: bool = False
    battery: float = 100.0


class KinematicSim:
    """A minimal, dependency-free kinematic simulator (the $0 backend)."""

    #: cruise speed in m/s and deceleration in m/s^2.
    SPEED = 1.0
    DECEL = 4.0

    def __init__(self) -> None:
        self._robots: dict[str, _RobotState] = {}

    def spawn(self, robot_id: str, x: float = 0.0, y: float = 0.0) -> None:
        self._robots[robot_id] = _RobotState(pose=Pose(x=x, y=y))

    def command_move(self, robot_id: str, target_x: float, target_y: float) -> None:
        r = self._robots[robot_id]
        r.stopping = False
        r.target = (target_x, target_y)

    def command_stop(self, robot_id: str) -> None:
        r = self._robots[robot_id]
        r.stopping = True
        r.target = None

    def tick(self, dt: float) -> None:
        for r in self._robots.values():
            if r.stopping:
                # Decelerate toward zero; a real, observable stop (not instantaneous).
                speed = math.hypot(r.vx, r.vy)
                if speed <= self.DECEL * dt:
                    r.vx = r.vy = 0.0
                else:
                    scale = (speed - self.DECEL * dt) / speed
                    r.vx *= scale
                    r.vy *= scale
            elif r.target is not None:
                dx = r.target[0] - r.pose.x
                dy = r.target[1] - r.pose.y
                dist = math.hypot(dx, dy)
                if dist <= self.SPEED * dt:
                    r.pose.x, r.pose.y = r.target
                    r.vx = r.vy = 0.0
                    r.target = None
                else:
                    r.vx = self.SPEED * dx / dist
                    r.vy = self.SPEED * dy / dist
            r.pose.x += r.vx * dt
            r.pose.y += r.vy * dt
            r.battery = max(0.0, r.battery - 0.01 * dt)

    def is_stopped(self, robot_id: str) -> bool:
        r = self._robots[robot_id]
        return r.vx == 0.0 and r.vy == 0.0  # confirmed ground truth

    def pose(self, robot_id: str) -> Pose:
        return self._robots[robot_id].pose

    def battery_percent(self, robot_id: str) -> int:
        return int(self._robots[robot_id].battery)


class IsaacSimBackend:
    """NVIDIA Isaac Sim backend (optional). The multi-simulator seam.

    It lazily imports the Isaac runtime; when Isaac Sim is not installed it raises
    with an actionable message rather than degrading to a fake. A production
    integration wires these methods to Isaac's articulation/robot APIs.
    """

    def __init__(self) -> None:
        try:
            import isaacsim  # noqa: F401  (only present inside the Isaac runtime)
        except ImportError as exc:  # pragma: no cover — requires the Isaac runtime
            raise RuntimeError(
                "IsaacSimBackend requires NVIDIA Isaac Sim and must run inside its "
                "Python runtime. Use KinematicSim for a $0, dependency-free run "
                "(`--simulator kinematic`), or launch this adapter from Isaac Sim."
            ) from exc
        raise NotImplementedError(  # pragma: no cover
            "Wire these methods to the Isaac Sim articulation APIs. The confirmed-"
            "stop contract (is_stopped == real zero velocity) MUST be preserved."
        )


def make_simulator(name: str) -> Simulator:
    """Select a simulator backend by name (the multi-simulator entry point)."""
    if name == "kinematic":
        return KinematicSim()
    if name == "isaac":
        return IsaacSimBackend()  # pragma: no cover — requires the Isaac runtime
    raise ValueError(f"unknown simulator {name!r}; expected 'kinematic' or 'isaac'")
