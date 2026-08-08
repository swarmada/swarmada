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

"""The safety-critical MUSTs, as pure (proto-free) logic.

Per ADR-0005 an adapter may be feature-basic but MUST be safety-complete. These
three components encode the CONFORMANCE.md safety MUSTs independently of the wire
types, so they can be unit-tested directly and reused by any adapter:

* `FenceGuard`   — C3 fencing-token ordering (anti double-execution).
* `LeaseMonitor` — C4 assignment-lease self-stop.
* `confirm_estop`— C5 confirmed (never inferred) emergency stop.
"""

from __future__ import annotations

import time
from collections.abc import Callable
from enum import Enum


class FenceDecision(Enum):
    """Outcome of a fencing-token check (C3)."""

    ACCEPT = "accept"        # a strictly-higher token: adopt it
    REACK = "reack"          # identical re-delivery of the current assignment
    STALE = "stale"          # token <= highest accepted → reject STALE (C3.2)
    MISSING = "missing"      # no token present → reject MISSING (C3.3)


class FenceGuard:
    """Per-robot fencing-token ordering (C3.1–C3.5).

    Persists the highest accepted token per robot (in memory here; a real adapter
    persists it so it survives restart, C3.1) and the assignment accepted at that
    token so an idempotent re-delivery (C3.4) is told apart from a stale one.
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
    `lease_duration`. When a renewal does not arrive before the deadline, the robot
    MUST be brought to a safe stop (C4.2). Renewals carrying a stale
    `lease_generation` are ignored and do not reset the timer (C4.3).

    Timer-free by design: call `tick()` from the adapter's telemetry loop. A fake
    `clock` makes the self-stop deterministically testable.
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


# C5 — confirmed emergency stop -------------------------------------------------

ESTOP_STOPPED = "STOPPED"
ESTOP_FAILED = "FAILED"


def confirm_estop(sim, robot: str, tick_dt: float = 0.05, max_ticks: int = 40) -> str:
    """Command a stop and confirm it from the simulator's ground truth (C5.2/C5.3).

    Returns ``STOPPED`` only once ``sim.is_stopped(robot)`` is true — the robot is
    actually at rest. If the stop cannot be confirmed within the bound, returns
    ``FAILED`` (escalate). It NEVER infers STOPPED from a timeout or from silence.
    """
    sim.command_stop(robot)
    for _ in range(max_ticks):
        sim.tick(tick_dt)
        if sim.is_stopped(robot):
            return ESTOP_STOPPED
    return ESTOP_FAILED
