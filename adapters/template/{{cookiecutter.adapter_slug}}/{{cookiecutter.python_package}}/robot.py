# Copyright {{cookiecutter.year}} {{cookiecutter.author}}
# SPDX-License-Identifier: Apache-2.0

"""The robot binding: how {{cookiecutter.adapter_name}} talks to real robots.

`RobotBinding` is the seam between the Swarmada protocol and your fleet API. The
generated adapter ships with `SimulatedRobot` so it runs before any fleet code is
written; **replace it with a real binding to your fleet API**. A conformance result
earned with `SimulatedRobot` is not a result for your binding.

SAFETY CONTRACT (never fake it): `is_stopped(robot_id)` MUST return True only when
the robot is *actually* at rest — confirmed from the robot, never inferred from a
timer or from silence. This is what makes emergency stop safe (CONFORMANCE.md C5).
"""

from __future__ import annotations

import math
from typing import Protocol

#: How fast the simulated robot drives, in m/s. The conformance harness sizes the goals it
#: sends for a robot no faster than this, so an action is still in flight when a check looks.
_SPEED_MPS = 0.5


class RobotBinding(Protocol):
    """The methods the adapter drives. Implement these against your fleet API."""

    def spawn(self, robot_id: str) -> None: ...

    def command_move(self, robot_id: str, x: float, y: float) -> None:
        """Begin driving the robot toward a target (task dispatch)."""

    def command_stop(self, robot_id: str) -> None:
        """Command a safe stop. NOT a confirmation — that is `is_stopped`."""

    def tick(self, dt: float) -> None:
        """Advance any internal model by dt seconds (no-op for a real robot)."""

    def arrived(self, robot_id: str) -> bool:
        """True once the robot is at rest AT the target of its last `command_move`.

        The adapter reports the action SUCCEEDED on this (CONFORMANCE.md C16.1), so it
        MUST mean the move completed: a robot brought to rest short of its target by a
        stop, a pause or an estop has not arrived.
        """

    def is_stopped(self, robot_id: str) -> bool:
        """CONFIRMED at rest — the ground truth the adapter waits on before it
        reports STOPPED. MUST reflect the real robot, never a timeout."""

    def pose(self, robot_id: str) -> tuple[float, float, int]:
        """(x, y, floor) in the zone frame."""

    def catalog(self) -> list[tuple[str, list[str], list[tuple[str, str, str]]]]:
        """The action types this robot serves, and nothing else.

        Each entry is ``(action_type, required_capabilities, required_params)``, a param
        being ``(name, unit, kind)``. A ``Custom`` entry may add a fourth element, the verb it
        declares (``custom_type``, contract 1.4.0): one entry per verb, each with that verb's
        own params; a ``Custom`` entry without it covers every verb no keyed entry names.
        The adapter DECLARES exactly this list on its CapabilitiesSnapshot and REFUSES any
        assignment outside it, or missing a required param, with INVALID_ACTION
        (CONFORMANCE.md C19): a type the robot does not serve is never run as the nearest
        thing it can do.
        """

    def pause(self, robot_id: str, require_stop: bool) -> bool:
        """Hold the robot's capabilities (CONFORMANCE.md C11.1, a REQUIRED command).

        Return True once paused. With ``require_stop`` the answer MUST wait for rest
        confirmed by `is_stopped`, never a timer. Return False when the robot cannot be
        paused; the adapter reports that, and still answers.
        """

    def resume(self, robot_id: str) -> bool:
        """Release a pause. Return True once the robot's capabilities are released."""

    def battery_percent(self, robot_id: str) -> int: ...


class SimulatedRobot:
    """A minimal simulated robot with a REAL confirmed stop, so the generated
    adapter can be run through the conformance harness before it is bound.

    TODO(vendor): delete this and implement `RobotBinding` against your fleet API.
    Keep the confirmed-stop contract — return `is_stopped=True` only when the robot
    has actually halted.
    """

    def __init__(self) -> None:
        self._moving: dict[str, bool] = {}
        self._pos: dict[str, tuple[float, float]] = {}
        self._target: dict[str, tuple[float, float] | None] = {}

    def spawn(self, robot_id: str) -> None:
        self._moving[robot_id] = False
        self._pos[robot_id] = (0.0, 0.0)
        self._target[robot_id] = None

    def command_move(self, robot_id: str, x: float, y: float) -> None:
        self._moving[robot_id] = True
        self._target[robot_id] = (x, y)

    def command_stop(self, robot_id: str) -> None:
        self._moving[robot_id] = False  # a real robot confirms this via is_stopped
        self._target[robot_id] = None  # a stopped move is abandoned, never arrived

    def tick(self, dt: float) -> None:
        # Drive each moving robot toward its target at _SPEED_MPS, and stop it there.
        for robot_id, moving in self._moving.items():
            target = self._target.get(robot_id)
            if not moving or target is None:
                continue
            x, y = self._pos.get(robot_id, (0.0, 0.0))
            dist = math.hypot(target[0] - x, target[1] - y)
            step = _SPEED_MPS * dt
            if dist <= step:
                self._pos[robot_id] = target
                self._moving[robot_id] = False
            else:
                self._pos[robot_id] = (x + (target[0] - x) * step / dist,
                                       y + (target[1] - y) * step / dist)

    def is_stopped(self, robot_id: str) -> bool:
        return not self._moving.get(robot_id, False)

    def arrived(self, robot_id: str) -> bool:
        target = self._target.get(robot_id)
        return (target is not None and not self._moving.get(robot_id, False)
                and self._pos.get(robot_id) == target)

    def pose(self, robot_id: str) -> tuple[float, float, int]:
        x, y = self._pos.get(robot_id, (0.0, 0.0))
        return (x, y, 0)

    def battery_percent(self, robot_id: str) -> int:
        return 100

    def catalog(self) -> list[tuple[str, list[str], list[tuple[str, str, str]]]]:
        # One type, and it needs a target: a Navigate with no x/y is refused, not driven
        # to the origin. TODO(vendor): list every FleetAction type your robots serve.
        return [("Navigate", ["navigation"], [("x", "m", "number"), ("y", "m", "number")])]

    def pause(self, robot_id: str, require_stop: bool) -> bool:
        # Held, not abandoned: the target is kept so that resume drives on to it.
        self._moving[robot_id] = False
        # The simulated stop is immediate; a real binding waits on its own confirmation.
        return self.is_stopped(robot_id) if require_stop else True

    def resume(self, robot_id: str) -> bool:
        if self._target.get(robot_id) is not None and not self.arrived(robot_id):
            self._moving[robot_id] = True
        return True
