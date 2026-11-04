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

"""Tests for the SDK's audited safety primitives (CONFORMANCE.md C3/C4/C5).

These are the single source of truth every adapter reuses, so they are tested
here directly and independently of any wire types or fleet API.
"""

from __future__ import annotations

import time

from swarmada_sdk.safety import (
    ESTOP_FAILED,
    ESTOP_STOPPED,
    FenceDecision,
    FenceGuard,
    LeaseMonitor,
    confirm_estop,
)

# ── C3: fencing-token ordering ────────────────────────────────────────────────

def test_c3_missing_token_rejected() -> None:
    g = FenceGuard()
    assert g.check("r1", has_token=False, token=0, assignment_id="a1") is FenceDecision.MISSING


def test_c3_ordering_accept_stale_reack() -> None:
    g = FenceGuard()
    assert g.check("r1", True, 5, "a1") is FenceDecision.ACCEPT
    assert g.check("r1", True, 5, "a1") is FenceDecision.REACK   # C3.4 idempotent re-delivery
    assert g.check("r1", True, 3, "a2") is FenceDecision.STALE   # C3.2 older token
    assert g.check("r1", True, 5, "a2") is FenceDecision.STALE   # C3.2 equal token, new assignment
    assert g.check("r1", True, 6, "a3") is FenceDecision.ACCEPT  # strictly newer
    assert g.highest("r1") == 6


def test_c3_per_robot() -> None:
    g = FenceGuard()
    g.check("r1", True, 9, "a1")
    assert g.check("r2", True, 1, "b1") is FenceDecision.ACCEPT


def test_c3_8_adopt_raises_the_mark_from_the_authoritative_token() -> None:
    """C3.8 — the control plane's own token raises the mark, not just an AssignAction."""
    g = FenceGuard()
    assert g.check("r1", True, 14, "a1") is FenceDecision.ACCEPT
    assert g.adopt("r1", 20) is True
    assert g.highest("r1") == 20
    # The whole point: a token BETWEEN the old mark and the adopted one is now stale.
    # Before adopt() existed this was ACCEPT, and a failed-over control plane's
    # predecessor could still authorise work.
    assert g.check("r1", True, 17, "a2") is FenceDecision.STALE


def test_c3_8_adopt_never_lowers_the_mark() -> None:
    """The safety invariant: a replayed or delayed RegisterAck must not re-open a token.

    RegisterAcks are not ordered against each other, so a stale one can land after a
    newer one. If adopt could lower the mark, that late frame would un-refuse an
    assignment the adapter had already rejected.
    """
    g = FenceGuard()
    g.adopt("r1", 20)
    assert g.adopt("r1", 14) is False    # a stale ack, replayed
    assert g.adopt("r1", 20) is False    # the same ack, re-delivered
    assert g.adopt("r1", 0) is False     # absent token read as 0
    assert g.highest("r1") == 20
    assert g.check("r1", True, 17, "a1") is FenceDecision.STALE


def test_c3_8_adopt_drops_the_reack_window_at_the_old_mark() -> None:
    """After adopting, a command AT the adopted token is STALE, not re-acked.

    The assignment accepted at the OLD mark is not the one the plane is fencing at
    now, so the C3.4 idempotency window must not survive the raise. Conservative by
    design: the plane re-issues at N+1 rather than the adapter re-acking an
    assignment whose identity was never confirmed against the new token.
    """
    g = FenceGuard()
    g.check("r1", True, 14, "a1")
    assert g.check("r1", True, 14, "a1") is FenceDecision.REACK   # window open at the old mark
    g.adopt("r1", 20)
    assert g.check("r1", True, 20, "a1") is FenceDecision.STALE   # and closed by the raise
    assert g.check("r1", True, 21, "a1") is FenceDecision.ACCEPT


def test_c3_8_adopt_is_per_robot() -> None:
    g = FenceGuard()
    g.adopt("r1", 20)
    assert g.highest("r2") == 0
    assert g.check("r2", True, 1, "b1") is FenceDecision.ACCEPT


# ── C4: assignment-lease self-stop ────────────────────────────────────────────

class _Clock:
    def __init__(self) -> None:
        self.t = 0.0

    def __call__(self) -> float:
        return self.t


def test_c4_self_stop_on_expiry() -> None:
    clock = _Clock()
    stopped: list[str] = []
    lm = LeaseMonitor(on_expiry=stopped.append, clock=clock)
    lm.grant("r1", 10.0, generation=1)
    clock.t = 5.0
    lm.tick()
    assert stopped == []
    clock.t = 11.0
    lm.tick()
    assert stopped == ["r1"]  # C4.2


def test_c4_renew_and_stale_generation() -> None:
    clock = _Clock()
    stopped: list[str] = []
    lm = LeaseMonitor(on_expiry=stopped.append, clock=clock)
    lm.grant("r1", 10.0, generation=5)
    clock.t = 5.0
    assert lm.renew("r1", 10.0, generation=6) is True   # honoured
    assert lm.renew("r1", 10.0, generation=3) is False  # C4.3 stale, ignored
    clock.t = 16.0  # within the generation-6 window (5 + 10 = 15)? crossed → but renewed at t=5→15
    lm.tick()
    # deadline was 15 (renew at t=5, dur 10); at t=16 it has expired.
    assert stopped == ["r1"]


# ── C5: confirmed emergency stop ──────────────────────────────────────────────

class _StoppableSim:
    def __init__(self) -> None:
        self.moving = True

    def command_stop(self, robot: str) -> None:
        self.moving = False

    def tick(self, dt: float) -> None:
        pass

    def is_stopped(self, robot: str) -> bool:
        return not self.moving


class _NonStoppingSim:
    def command_stop(self, robot: str) -> None:
        pass

    def tick(self, dt: float) -> None:
        pass

    def is_stopped(self, robot: str) -> bool:
        return False


def test_c5_confirmed_stopped() -> None:
    assert confirm_estop(_StoppableSim(), "r1") == ESTOP_STOPPED


def test_c5_unconfirmed_is_failed_never_stopped() -> None:
    # Cardinal safety property: no confirmation → FAILED, never STOPPED by timeout.
    assert confirm_estop(_NonStoppingSim(), "r1", tick_dt=0.01, max_ticks=5) == ESTOP_FAILED


class _AsyncSim:
    """A robot whose tick() advances NOTHING - the shape of every real binding.

    Nav2Binding.tick() is `pass` ("rclpy spins its own executor"). The robot comes
    to rest on its own clock; confirmation arrives via telemetry. This stands in for
    that: it reports stopped only after `stop_after` seconds of REAL time have
    elapsed since the stop was commanded.
    """

    def __init__(self, stop_after: float = 0.12) -> None:
        self._stop_after = stop_after
        self._commanded_at: float | None = None

    def command_stop(self, robot: str) -> None:
        self._commanded_at = time.monotonic()

    def tick(self, dt: float) -> None:
        pass                      # deliberately a no-op

    def is_stopped(self, robot: str) -> bool:
        if self._commanded_at is None:
            return False
        return (time.monotonic() - self._commanded_at) >= self._stop_after


def test_confirm_estop_confirms_an_asynchronous_robot() -> None:
    """The loop must WAIT, not just iterate.

    Before the fix this returned FAILED - max_ticks iterations completed in
    microseconds against a robot that needed real time to decelerate, and every
    estop on real hardware reported FAILED while the simulated path passed.
    """
    assert confirm_estop(_AsyncSim(stop_after=0.12), "r1") == ESTOP_STOPPED


def test_confirm_estop_still_fails_when_the_robot_never_stops() -> None:
    """The wait must not become an inference: no stop, no STOPPED."""
    assert confirm_estop(_NonStoppingSim(), "r1", tick_dt=0.01, max_ticks=5) == ESTOP_FAILED


def test_confirm_estop_sleep_is_injectable_so_tests_stay_fast() -> None:
    """The simulated path must not pay 2 s per estop in a unit test."""
    slept: list[float] = []
    assert confirm_estop(_StoppableSim(), "r1", sleep=slept.append) == ESTOP_STOPPED
    # _StoppableSim stops on the first tick, so the wait is never reached.
    assert slept == []
