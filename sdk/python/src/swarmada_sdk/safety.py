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

"""The Fleet Adapter safety-critical MUSTs, as pure (proto-free) logic.

Every adapter — reference, simulation, or vendor — is *feature-basic but
safety-complete* (ADR-0005): it may decline optional commands with
``unsupported=true`` (CONFORMANCE.md C7.1), but the safety MUSTs are complete and
correct. This module is the single, audited implementation of those MUSTs so an
adapter never re-derives (and never fakes) them:

* :class:`FenceGuard`   — C3 fencing-token ordering (anti double-execution).
* :class:`LeaseMonitor` — C4 assignment-lease self-stop.
* :func:`confirm_estop` — C5 confirmed (never inferred) emergency stop.

It is proto-free so it can be unit-tested directly and reused by any adapter,
whatever fleet API sits behind it.
"""

from __future__ import annotations

import time
from collections.abc import Callable
from enum import Enum

__all__ = [
    "FenceDecision",
    "FenceGuard",
    "LeaseMonitor",
    "command_estop",
    "await_estop_confirmation",
    "confirm_estop",
    "ESTOP_STOPPED",
    "ESTOP_FAILED",
]


class FenceDecision(Enum):
    """Outcome of a fencing-token check (C3)."""

    ACCEPT = "accept"    # a strictly-higher token: adopt it
    REACK = "reack"      # identical re-delivery of the current assignment (C3.4)
    STALE = "stale"      # token <= highest accepted → reject STALE (C3.2)
    MISSING = "missing"  # no token present → reject MISSING (C3.3)


class FenceGuard:
    """Per-robot fencing-token ordering (C3.1–C3.5).

    Persists the highest accepted token per robot (in memory here; a real adapter
    persists it so it survives restart, C3.1) and the assignment accepted at that
    token, so an idempotent re-delivery (C3.4) is told apart from a stale one.
    """

    def __init__(self) -> None:
        self._highest: dict[str, int] = {}
        self._assignment: dict[str, str] = {}

    def check(self, robot: str, has_token: bool, token: int, assignment_id: str) -> FenceDecision:
        if not has_token:
            return FenceDecision.MISSING  # C3.3: absent ≠ 0
        current = self._highest.get(robot)
        same_assignment = assignment_id == self._assignment.get(robot)
        if current is not None and token == current and same_assignment:
            return FenceDecision.REACK    # C3.4: identical re-delivery
        if current is not None and token <= current:
            return FenceDecision.STALE    # C3.2: not strictly newer
        self._highest[robot] = token      # C3.1: persist the new high-water mark
        self._assignment[robot] = assignment_id
        return FenceDecision.ACCEPT

    def highest(self, robot: str) -> int:
        return self._highest.get(robot, 0)


class LeaseMonitor:
    """Assignment-lease self-stop (C4).

    A held execution lease is valid only while the control plane renews it within
    ``lease_duration``. When a renewal does not arrive before the deadline, the
    robot MUST be brought to a safe stop (C4.2). Renewals carrying a stale
    ``lease_generation`` are ignored and do not reset the timer (C4.3).

    Timer-free by design: call :meth:`tick` from the adapter's telemetry loop. A
    fake ``clock`` makes the self-stop deterministically testable.
    """

    def __init__(
        self,
        on_expiry: Callable[[str], None],
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._on_expiry = on_expiry
        self._clock = clock
        self._deadline: dict[str, float] = {}
        self._generation: dict[str, int] = {}

    def grant(self, robot: str, duration_s: float, generation: int) -> None:
        self._deadline[robot] = self._clock() + duration_s
        self._generation[robot] = generation

    def renew(self, robot: str, duration_s: float, generation: int) -> bool:
        """Reset the deadline unless the generation is stale (C4.3). Returns whether
        the renewal was honoured."""
        if robot in self._generation and generation < self._generation[robot]:
            return False
        self._deadline[robot] = self._clock() + duration_s
        self._generation[robot] = generation
        return True

    def release(self, robot: str) -> None:
        self._deadline.pop(robot, None)
        self._generation.pop(robot, None)

    def tick(self) -> None:
        """Fire the self-stop callback for any robot whose lease has expired."""
        now = self._clock()
        expired = [r for r, dl in self._deadline.items() if now >= dl]
        for robot in expired:
            del self._deadline[robot]
            self._on_expiry(robot)  # C4.2: bring the task to a safe stop


ESTOP_STOPPED = "STOPPED"
ESTOP_FAILED = "FAILED"


def confirm_estop(sim, robot: str, tick_dt: float = 0.05, max_ticks: int = 40,
                  sleep: Callable[[float], None] = time.sleep) -> str:
    """Command a stop and confirm it from the robot's ground truth (C5.2/C5.3).

    Returns ``STOPPED`` only once ``sim.is_stopped(robot)`` is true — the robot is
    actually at rest. If the stop cannot be confirmed within ``max_ticks * tick_dt``
    (2.0 s by default), returns ``FAILED`` (escalate). It NEVER infers STOPPED from
    a timeout or from silence.

    ``sim`` is any object exposing ``command_stop(robot)``, ``tick(dt)`` and
    ``is_stopped(robot)``. Two shapes must both work, and the difference is where
    time comes from:

    * **Synchronous** (a simulator): ``tick(dt)`` advances the world, so the loop
      itself produces the elapsed time.
    * **Asynchronous** (a binding to a real robot): ``tick(dt)`` is a no-op — the
      robot decelerates on its own clock and confirmation arrives via telemetry.
      Nothing in the loop advances anything, so the loop MUST wait.

    ``sleep`` supplies that wait. Without it the loop burned ``max_ticks`` in
    microseconds and returned ``FAILED`` for a robot that stopped correctly, which
    is ITEM-0107: every real estop reported FAILED while the simulated one passed,
    because the simulator was quietly supplying the time. Injectable so unit tests
    stay instant.

    ``max_ticks * tick_dt`` is therefore a **wall-clock budget for a confirmed
    stop**, not a loop count. Treat it as a safety parameter.
    """
    command_estop(sim, robot)
    return await_estop_confirmation(sim, robot, tick_dt, max_ticks, sleep)


def command_estop(sim, robot: str) -> None:
    """Issue the hardware stop and return IMMEDIATELY, without confirming rest.

    Split out of :func:`confirm_estop` so an adapter can acknowledge an estop the moment
    the command is issued (``ESTOP_STATE_STOPPING``) and confirm rest afterwards.

    The estop SLA bounds the FIRST acknowledgement. Waiting for a real base to decelerate
    before answering misses it by roughly 30% -- measured 604-657 ms against a live Nav2
    stack, against a 500 ms budget, while the same code answers in 5 ms against a fake
    server that was already stationary.

    This does NOT weaken the confirmed-stop discipline. STOPPED is still reported only by
    :func:`await_estop_confirmation`, from the robot's own ground truth; only the
    acknowledgement moves earlier.
    """
    sim.command_stop(robot)


def await_estop_confirmation(sim, robot: str, tick_dt: float = 0.05, max_ticks: int = 40,
                             sleep: Callable[[float], None] = time.sleep) -> str:
    """Wait for CONFIRMED rest after :func:`command_estop`.

    Returns ``ESTOP_STOPPED`` only once ``sim.is_stopped(robot)`` is true, and
    ``ESTOP_FAILED`` if rest cannot be confirmed within ``max_ticks * tick_dt``. It NEVER
    infers rest from a timeout or from silence -- see :func:`confirm_estop` for why
    ``sleep`` is injected rather than assumed.
    """
    for _ in range(max_ticks):
        sim.tick(tick_dt)
        if sim.is_stopped(robot):
            return ESTOP_STOPPED
        sleep(tick_dt)
    return ESTOP_FAILED
