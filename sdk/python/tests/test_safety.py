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

import logging
import time

import pytest

from swarmada_sdk import safety
from swarmada_sdk.safety import (
    ESTOP_FAILED,
    ESTOP_STOPPED,
    EstopToken,
    FenceDecision,
    FenceGuard,
    LeaseMonitor,
    await_estop_confirmation,
    command_estop,
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
    tick_keeps_state_current = True

    def __init__(self) -> None:
        self.moving = True

    def command_stop(self, robot: str) -> None:
        self.moving = False

    def tick(self, dt: float) -> None:
        pass

    def is_stopped(self, robot: str) -> bool:
        return not self.moving


class _NonStoppingSim:
    tick_keeps_state_current = True

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


# ── C5: only a report newer than the stop confirms it (state_seq) ──────────────

class _ReportingRobot:
    """A real robot's binding: tick() does nothing, and the state arrives as reports.

    ``is_stopped`` answers from the last report. ``state_seq`` counts the reports, and a
    report stores its state before it raises the count, as the binding contract asks.
    """

    def __init__(self, at_rest: bool) -> None:
        self.at_rest = at_rest
        self.seq = 0
        self.calls: list[str] = []

    def report(self, at_rest: bool) -> None:
        self.at_rest = at_rest
        self.seq += 1

    def command_stop(self, robot: str) -> None:
        self.calls.append("command_stop")

    def tick(self, dt: float) -> None:
        pass

    def is_stopped(self, robot: str) -> bool:
        self.calls.append("is_stopped")
        return self.at_rest

    def state_seq(self, robot: str) -> int:
        self.calls.append("state_seq")
        return self.seq


def _no_sleep(_: float) -> None:
    pass


def test_a_report_from_before_the_stop_does_not_confirm_it() -> None:
    # The robot's last report said at rest; it was sent before the stop, and no report
    # has come since. Without state_seq this confirmed STOPPED at once.
    robot = _ReportingRobot(at_rest=True)
    robot.report(True)
    assert confirm_estop(robot, "r1", max_ticks=5, sleep=_no_sleep) == ESTOP_FAILED


def test_a_report_after_the_stop_confirms_it() -> None:
    robot = _ReportingRobot(at_rest=False)
    slept: list[float] = []

    def sleep(dt: float) -> None:
        slept.append(dt)
        robot.report(True)

    assert confirm_estop(robot, "r1", max_ticks=5, sleep=sleep) == ESTOP_STOPPED
    assert len(slept) == 1


def test_confirms_on_the_first_report_at_rest_not_on_an_earlier_moving_one() -> None:
    robot = _ReportingRobot(at_rest=True)
    reports = iter([False, True])
    slept: list[float] = []

    def sleep(dt: float) -> None:
        slept.append(dt)
        robot.report(next(reports))

    assert confirm_estop(robot, "r1", max_ticks=5, sleep=sleep) == ESTOP_STOPPED
    assert len(slept) == 2  # the moving report did not confirm; the next one did


class _ReportsDuringTheStop(_ReportingRobot):
    """A binding that receives an 'at rest' report while command_stop is still running.

    The robot sent it before the stop reached it (a Nav2 binding halts its explorer before
    it commands the hold), so it must not confirm the stop.
    """

    def command_stop(self, robot: str) -> None:
        super().command_stop(robot)
        self.report(True)


def test_a_report_received_while_the_stop_is_commanded_does_not_confirm_it() -> None:
    robot = _ReportsDuringTheStop(at_rest=False)
    assert confirm_estop(robot, "r1", max_ticks=3, sleep=_no_sleep) == ESTOP_FAILED


class _ReportsRightAfterTheStop(_ReportingRobot):
    """An 'at rest' report lands just after command_estop reads the count, before the
    wait starts: received after the stop was commanded, so it counts."""

    def state_seq(self, robot: str) -> int:
        value = super().state_seq(robot)
        if self.calls.count("state_seq") == 1:
            self.report(True)
        return value


def test_confirm_estop_passes_its_count_on_to_the_wait() -> None:
    # Without since passed on, the wait would read the count on entry, after that report,
    # and the first look would not confirm.
    robot = _ReportsRightAfterTheStop(at_rest=False)
    assert confirm_estop(robot, "r1", max_ticks=1, sleep=_no_sleep) == ESTOP_STOPPED


def test_a_count_equal_to_since_does_not_confirm() -> None:
    # The comparison is strict: a count equal to since is the report the stop already saw.
    robot = _ReportingRobot(at_rest=True)
    robot.report(True)
    assert await_estop_confirmation(robot, "r1", max_ticks=3, sleep=_no_sleep,
                                    since=EstopToken("r1", robot.seq, id(robot))) == ESTOP_FAILED
    assert await_estop_confirmation(robot, "r1", max_ticks=3, sleep=_no_sleep,
                                    since=EstopToken("r1", robot.seq - 1, id(robot))
                                    ) == ESTOP_STOPPED


def test_a_report_between_the_stop_and_the_wait_counts_when_since_is_passed() -> None:
    robot = _ReportingRobot(at_rest=False)
    since = command_estop(robot, "r1")
    assert since == EstopToken("r1", 0, id(robot))
    robot.report(True)
    assert await_estop_confirmation(robot, "r1", max_ticks=1, sleep=_no_sleep,
                                    since=since) == ESTOP_STOPPED


def test_without_since_the_count_is_read_on_entry_which_is_stricter() -> None:
    # The same report, received after the stop but before the wait, does not count when
    # since is not passed: the count is read on entry, after it.
    robot = _ReportingRobot(at_rest=False)
    command_estop(robot, "r1")
    robot.report(True)
    assert await_estop_confirmation(robot, "r1", max_ticks=3, sleep=_no_sleep) == ESTOP_FAILED


def test_command_estop_reads_the_count_after_commanding_the_stop() -> None:
    robot = _ReportingRobot(at_rest=False)
    command_estop(robot, "r1")
    assert robot.calls == ["command_stop", "state_seq"]


def test_the_count_is_read_before_the_state_on_every_look() -> None:
    # By the binding contract a state read after a raised count is at least as new as the
    # report that raised it; read the other way round, an old state could pair with it.
    robot = _ReportingRobot(at_rest=True)
    robot.report(True)
    since = EstopToken("r1", robot.seq - 1, id(robot))
    robot.calls.clear()
    assert await_estop_confirmation(robot, "r1", max_ticks=1, sleep=_no_sleep,
                                    since=since) == ESTOP_STOPPED
    assert robot.calls == ["state_seq", "is_stopped"]


def test_polling_with_one_tick_needs_since_and_fails_safe_without_it() -> None:
    # One look per call, the robot's reports arriving between calls (the ROS 2 adapter's
    # first look has this shape; its later wait is a single long call). Without since,
    # each call reads the count anew, so no call sees a newer report: the answer is
    # FAILED, never a false STOPPED. With since the second poll confirms.
    robot = _ReportingRobot(at_rest=False)
    since = command_estop(robot, "r1")
    answers = []
    for _ in range(3):
        answers.append(await_estop_confirmation(robot, "r1", max_ticks=1, sleep=_no_sleep))
        robot.report(True)
    assert answers == [ESTOP_FAILED] * 3

    robot = _ReportingRobot(at_rest=False)
    since = command_estop(robot, "r1")
    answers = []
    for _ in range(3):
        answers.append(await_estop_confirmation(robot, "r1", max_ticks=1, sleep=_no_sleep,
                                                since=since))
        robot.report(True)
    assert answers == [ESTOP_FAILED, ESTOP_STOPPED, ESTOP_STOPPED]


def test_since_is_ignored_for_a_binding_without_state_seq() -> None:
    sim = _StoppableSim()
    assert command_estop(sim, "r1") is None
    assert await_estop_confirmation(sim, "r1", sleep=_no_sleep,
                                    since=EstopToken("r1", 7, id(sim))) == ESTOP_STOPPED


@pytest.fixture
def fresh_warnings(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(safety, "_warned_without_state_seq", set())


def test_a_binding_without_state_seq_is_warned_about_once(
        fresh_warnings: None, caplog: pytest.LogCaptureFixture) -> None:
    class _LegacyBinding(_StoppableSim):
        tick_keeps_state_current = False

    with caplog.at_level(logging.WARNING, logger="swarmada_sdk.safety"):
        confirm_estop(_LegacyBinding(), "r1", sleep=_no_sleep)
        confirm_estop(_LegacyBinding(), "r2", sleep=_no_sleep)
    warnings = [r for r in caplog.records if "has no state_seq" in r.getMessage()]
    assert len(warnings) == 1
    assert "_LegacyBinding" in warnings[0].getMessage()


def test_a_simulated_binding_can_say_its_tick_keeps_the_state_current(
        fresh_warnings: None, caplog: pytest.LogCaptureFixture) -> None:
    class _Simulator(_StoppableSim):
        tick_keeps_state_current = True

    with caplog.at_level(logging.WARNING, logger="swarmada_sdk.safety"):
        assert confirm_estop(_Simulator(), "r1", sleep=_no_sleep) == ESTOP_STOPPED
    assert not [r for r in caplog.records if "has no state_seq" in r.getMessage()]


def test_a_binding_with_state_seq_is_not_warned_about(
        fresh_warnings: None, caplog: pytest.LogCaptureFixture) -> None:
    robot = _ReportingRobot(at_rest=False)
    with caplog.at_level(logging.WARNING, logger="swarmada_sdk.safety"):
        confirm_estop(robot, "r1", max_ticks=1, sleep=_no_sleep)
    assert not caplog.records


def test_the_count_and_the_state_are_read_for_the_robot_being_stopped() -> None:
    class _TwoRobots:
        def __init__(self) -> None:
            self.asked: list[tuple[str, str]] = []
            self.seq = {"a": 0, "b": 0}

        def command_stop(self, robot: str) -> None:
            self.asked.append(("command_stop", robot))

        def tick(self, dt: float) -> None:
            pass

        def state_seq(self, robot: str) -> int:
            self.asked.append(("state_seq", robot))
            return self.seq[robot]

        def is_stopped(self, robot: str) -> bool:
            self.asked.append(("is_stopped", robot))
            return True

    robots = _TwoRobots()

    def sleep(_: float) -> None:
        robots.seq["b"] += 1  # only the OTHER robot reports

    assert confirm_estop(robots, "a", max_ticks=3, sleep=sleep) == ESTOP_FAILED
    assert {r for _, r in robots.asked} == {"a"}
    robots.seq["a"] += 1
    assert await_estop_confirmation(robots, "a", max_ticks=1, sleep=_no_sleep,
                                    since=EstopToken("a", 0, id(robots))) == ESTOP_STOPPED


def test_a_moving_robot_reporting_from_another_thread_is_never_confirmed() -> None:
    # Reports arrive on the binding's own thread, under its lock, as the contract asks.
    import threading

    class _Threaded:
        def __init__(self) -> None:
            self._lock = threading.Lock()
            self._seq = 0
            self._at_rest = True  # at rest before the stop: the stale report

        def post(self, at_rest: bool) -> None:
            with self._lock:
                self._at_rest = at_rest
                self._seq += 1

        def command_stop(self, robot: str) -> None:
            pass

        def tick(self, dt: float) -> None:
            pass

        def state_seq(self, robot: str) -> int:
            with self._lock:
                return self._seq

        def is_stopped(self, robot: str) -> bool:
            with self._lock:
                return self._at_rest

    binding = _Threaded()
    done = threading.Event()
    commanded = threading.Event()
    real_command_stop = binding.command_stop

    def command_stop(robot: str) -> None:
        real_command_stop(robot)
        commanded.set()

    binding.command_stop = command_stop  # type: ignore[method-assign]

    def keep_moving() -> None:
        # The robot's next report comes 20 ms after the stop and says it is still moving;
        # until then only the stale 'at rest' report exists, which the old check confirmed.
        commanded.wait()
        time.sleep(0.02)
        while not done.is_set():
            binding.post(False)
            time.sleep(0.001)

    t = threading.Thread(target=keep_moving)
    t.start()
    try:
        assert confirm_estop(binding, "r1", tick_dt=0.002, max_ticks=50) == ESTOP_FAILED
    finally:
        done.set()
        t.join()


def test_positional_arguments_are_unchanged() -> None:
    # External adapters call await_estop_confirmation(binding, robot, tick_dt, max_ticks,
    # sleep) positionally; since is keyword-only and moves nothing.
    robot = _ReportingRobot(at_rest=False)
    robot.report(True)
    assert await_estop_confirmation(robot, "r1", 0.01, 1, _no_sleep) == ESTOP_FAILED
    sim = _StoppableSim()
    command_estop(sim, "r1")
    assert await_estop_confirmation(sim, "r1", 0.01, 1, _no_sleep) == ESTOP_STOPPED


@pytest.mark.parametrize("bad", [None, "3", 2.0, True])
def test_a_count_that_is_not_an_int_answers_failed(
        bad: object, caplog: pytest.LogCaptureFixture) -> None:
    class _BadCount(_ReportingRobot):
        def state_seq(self, robot: str):  # type: ignore[override]
            return bad

    robot = _BadCount(at_rest=True)
    with caplog.at_level(logging.ERROR, logger="swarmada_sdk.safety"):
        assert confirm_estop(robot, "r1", max_ticks=3, sleep=_no_sleep) == ESTOP_FAILED
    assert any("not an int" in r.getMessage() for r in caplog.records)


def test_a_token_from_another_robots_stop_answers_failed(
        caplog: pytest.LogCaptureFixture) -> None:
    # Robot b's token carries b's count; used for robot a it could confirm a from an old
    # state. It is refused, and the refusal names both robots.
    robot = _ReportingRobot(at_rest=True)
    robot.report(True)
    with caplog.at_level(logging.ERROR, logger="swarmada_sdk.safety"):
        assert await_estop_confirmation(robot, "a", max_ticks=3, sleep=_no_sleep,
                                        since=EstopToken("b", 0, id(robot))) == ESTOP_FAILED
    message = " ".join(r.getMessage() for r in caplog.records)
    assert "'a'" in message and "'b'" in message


@pytest.mark.parametrize("bare", [0, 3, "3", (("r1", 0))])
def test_since_must_be_a_token_not_a_bare_count(
        bare: object, caplog: pytest.LogCaptureFixture) -> None:
    robot = _ReportingRobot(at_rest=True)
    robot.report(True)
    with caplog.at_level(logging.ERROR, logger="swarmada_sdk.safety"):
        assert await_estop_confirmation(robot, "r1", max_ticks=3, sleep=_no_sleep,
                                        since=bare) == ESTOP_FAILED  # type: ignore[arg-type]
    assert any("not the EstopToken" in r.getMessage() for r in caplog.records)


def test_the_token_is_frozen() -> None:
    import dataclasses

    token = EstopToken("r1", 3, 0)
    with pytest.raises(dataclasses.FrozenInstanceError):
        token.seq = 0  # type: ignore[misc]


def test_a_count_that_turns_into_a_non_int_on_a_later_look_answers_failed() -> None:
    class _TurnsFloat(_ReportingRobot):
        def state_seq(self, robot: str):  # type: ignore[override]
            super().state_seq(robot)
            return 0 if self.calls.count("state_seq") == 1 else 1.5

    assert confirm_estop(_TurnsFloat(at_rest=True), "r1", max_ticks=3,
                         sleep=_no_sleep) == ESTOP_FAILED


@pytest.mark.parametrize("shape", ["property", "attribute"])
def test_a_state_seq_that_is_not_a_method_answers_failed(
        shape: str, caplog: pytest.LogCaptureFixture) -> None:
    # Not a silent fall-back to the plain check: that would confirm the stale report.
    if shape == "property":
        class _Binding(_ReportingRobot):
            @property  # type: ignore[override]
            def state_seq(self) -> int:
                return self.seq
    else:
        class _Binding(_ReportingRobot):  # type: ignore[no-redef]
            pass

    robot = _Binding(at_rest=True)
    if shape == "attribute":
        robot.state_seq = 5  # type: ignore[assignment,method-assign]
    with caplog.at_level(logging.ERROR, logger="swarmada_sdk.safety"):
        assert confirm_estop(robot, "r1", max_ticks=3, sleep=_no_sleep) == ESTOP_FAILED
    assert any("not a method" in r.getMessage() for r in caplog.records)


def test_a_state_seq_that_raises_after_the_stop_still_sends_the_stop() -> None:
    class _Raises(_ReportingRobot):
        def state_seq(self, robot: str) -> int:
            super().state_seq(robot)
            raise RuntimeError("transport down")

    robot = _Raises(at_rest=True)
    assert command_estop(robot, "r1") is None
    assert robot.calls[0] == "command_stop"


def test_a_token_from_another_binding_answers_failed(
        caplog: pytest.LogCaptureFixture) -> None:
    # Two bindings each serving a robot named r1 (two fleets, or a binding rebuilt after
    # a reconnect, whose count restarted): the first's token must not confirm on the
    # second, whose count it does not describe.
    first, second = _ReportingRobot(at_rest=False), _ReportingRobot(at_rest=True)
    token = command_estop(first, "r1")
    second.report(True)
    with caplog.at_level(logging.ERROR, logger="swarmada_sdk.safety"):
        assert await_estop_confirmation(second, "r1", max_ticks=3, sleep=_no_sleep,
                                        since=token) == ESTOP_FAILED
    assert any("another binding" in r.getMessage() for r in caplog.records)


def test_a_token_kept_from_an_earlier_stop_of_the_same_robot_is_not_caught() -> None:
    # Documented limit, pinned so a change to it is deliberate: the token cannot tell two
    # stops of one robot apart, so a kept token lets a report from between the stops
    # confirm the later one. Callers pass the token of the stop they have commanded.
    robot = _ReportingRobot(at_rest=False)
    kept = command_estop(robot, "r1")
    robot.report(True)          # between the two stops
    command_estop(robot, "r1")  # the later stop; its own token would refuse that report
    assert await_estop_confirmation(robot, "r1", max_ticks=1, sleep=_no_sleep,
                                    since=kept) == ESTOP_STOPPED
