# Copyright {{cookiecutter.year}} {{cookiecutter.author}}
# SPDX-License-Identifier: Apache-2.0

"""The robot binding: how {{cookiecutter.adapter_name}} talks to real robots.

`RobotBinding` is the seam between the Swarmada protocol and your fleet API. The
generated adapter ships with `SimulatedRobot` so it is conformant out of the box;
**replace it with a real binding to your fleet API**.

SAFETY CONTRACT (never fake it): `is_stopped(robot_id)` MUST return True only when
the robot is *actually* at rest — confirmed from the robot, never inferred from a
timer or from silence. This is what makes emergency stop safe (CONFORMANCE.md C5).
"""

from __future__ import annotations

from typing import Protocol


class RobotBinding(Protocol):
    """The methods the adapter drives. Implement these against your fleet API."""

    def spawn(self, robot_id: str) -> None: ...

    def command_move(self, robot_id: str, x: float, y: float) -> None:
        """Begin driving the robot toward a target (task dispatch)."""

    def command_stop(self, robot_id: str) -> None:
        """Command a safe stop. NOT a confirmation — that is `is_stopped`."""

    def tick(self, dt: float) -> None:
        """Advance any internal model by dt seconds (no-op for a real robot)."""

    def is_stopped(self, robot_id: str) -> bool:
        """CONFIRMED at rest — the ground truth the adapter waits on before it
        reports STOPPED. MUST reflect the real robot, never a timeout."""

    def pose(self, robot_id: str) -> tuple[float, float, int]:
        """(x, y, floor) in the zone frame."""

    def battery_percent(self, robot_id: str) -> int: ...


class SimulatedRobot:
    """A minimal simulated robot with a REAL confirmed stop, so the generated
    adapter passes conformance immediately.

    TODO(vendor): delete this and implement `RobotBinding` against your fleet API.
    Keep the confirmed-stop contract — return `is_stopped=True` only when the robot
    has actually halted.
    """

    def __init__(self) -> None:
        self._moving: dict[str, bool] = {}
        self._pos: dict[str, tuple[float, float]] = {}

    def spawn(self, robot_id: str) -> None:
        self._moving[robot_id] = False
        self._pos[robot_id] = (0.0, 0.0)

    def command_move(self, robot_id: str, x: float, y: float) -> None:
        self._moving[robot_id] = True
        self._pos[robot_id] = (x, y)

    def command_stop(self, robot_id: str) -> None:
        self._moving[robot_id] = False  # a real robot confirms this via is_stopped

    def tick(self, dt: float) -> None:
        pass

    def is_stopped(self, robot_id: str) -> bool:
        return not self._moving.get(robot_id, False)

    def pose(self, robot_id: str) -> tuple[float, float, int]:
        x, y = self._pos.get(robot_id, (0.0, 0.0))
        return (x, y, 0)

    def battery_percent(self, robot_id: str) -> int:
        return 100
