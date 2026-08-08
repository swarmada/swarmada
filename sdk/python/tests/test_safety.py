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
