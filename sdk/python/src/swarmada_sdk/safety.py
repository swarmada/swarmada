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

import logging
import time
from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum

__all__ = [
    "FenceDecision",
    "FenceGuard",
    "LeaseMonitor",
    "EstopToken",
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

    def adopt(self, robot: str, token: int) -> bool:
        """Raise the mark to the control plane's OWN statement of the current token.

        ``check()`` raises the mark from a command the adapter is being asked to run.
        This raises it from ``RegisterAck.authoritative_action_state.fencing_token`` --
        the control plane saying, out of band, "this robot is at token N". The proto
        states the obligation on that field directly: "Current fencing token the adapter
        should be executing under; the adapter MUST reject any command with a lower token
        for this robot."

        Without it the mark only ever rises from commands this adapter accepted, so an
        adapter cannot learn about a token it never saw. After a control-plane FAILOVER
        the new plane legitimately holds a higher token, and an adapter that never adopts
        it goes on honouring commands the DEAD plane authorised -- two executors on one
        robot, which is the single hazard C3 exists to prevent (C3.8).

        MONOTONIC, and that is the safety property, not an optimisation. Adopting a value
        at or below the current mark is a no-op returning ``False``. Control-plane
        messages are not ordered against each other on this path: a RegisterAck delayed
        behind a newer one, or replayed by a reconnect, arrives carrying a token that was
        current when it was sent and is stale by the time it lands. If adopt could LOWER
        the mark, that late message would re-open a token the adapter had already refused
        -- turning the reconnect handshake into a way to un-refuse a stale assignment, and
        handing an attacker who can replay one frame a way to reinstate a revoked command.
        There is no legitimate reason for the authoritative token to go backwards; a plane
        that means "stop" says so on the estop path, which carries no fence at all
        (RFC-0001 #safety-emergency-stop-protocol) precisely so a stale token can never
        refuse a stop.

        The assignment recorded at the OLD mark is dropped, so the C3.4 idempotent-re-ack
        window does not survive the raise: after adopting N, a command AT N is STALE
        rather than re-acked, because the assignment this adapter accepted at the old mark
        is not the one the plane is now fencing at. That is deliberately conservative --
        the plane re-issues at N+1 and loses a round trip, where the alternative is
        re-acking an assignment whose identity was never confirmed against the new token.
        On a safety MUST the failure has to land on the refusing side.

        Returns ``True`` if the mark advanced.

        Not internally locked, in keeping with the rest of this class: every reference
        adapter handles ``register_ack`` and ``command`` on the same stream-receive
        thread, so ``adopt`` and ``check`` never interleave. An adapter that dispatches
        them to different threads must serialise the two calls itself.
        """
        if token <= self._highest.get(robot, 0):
            return False                      # never lower the mark; a replay must not re-open it
        self._highest[robot] = token
        self._assignment.pop(robot, None)     # the assignment at the old mark is not this one
        return True

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

@dataclass(frozen=True)
class EstopToken:
    """What :func:`command_estop` hands to :func:`await_estop_confirmation`: the robot it
    stopped, the binding's ``state_seq`` for that robot once the stop was commanded, and
    which binding object the stop went through (its ``id()``).

    Only a report counted after ``seq`` confirms the stop. The token names its robot and
    its binding, so a token from another robot's stop, or from another binding's, is
    refused (FAILED) instead of confirming this stop from a count that is not its own.

    It does NOT tell two stops of the same robot on the same binding apart: a token kept
    from an earlier stop carries a lower count, and passed to a later stop's wait it lets
    a report from between the two stops confirm the later one. Pass the token of the stop
    you have commanded, never a kept one.
    """

    robot: str
    seq: int
    binding: int


_log = logging.getLogger(__name__)
#: Binding classes already warned that they have no ``state_seq``: one warning per class.
_warned_without_state_seq: set[type] = set()


def confirm_estop(sim, robot: str, tick_dt: float = 0.05, max_ticks: int = 40,
                  sleep: Callable[[float], None] = time.sleep) -> str:
    """Command a stop and confirm it from the robot's ground truth (C5.2/C5.3).

    Returns ``STOPPED`` only once ``sim.is_stopped(robot)`` is true — the robot is
    actually at rest. If the stop cannot be confirmed within ``max_ticks * tick_dt``
    (2.0 s by default), returns ``FAILED`` (escalate). It NEVER infers STOPPED from
    a timeout or from silence.

    ``sim`` is any object exposing ``command_stop(robot)``, ``tick(dt)`` and
    ``is_stopped(robot)``, and, for a real robot, ``state_seq(robot)`` (see
    :func:`await_estop_confirmation`). Two shapes must both work, and the difference is
    where time comes from:

    * **Synchronous** (a simulator): ``tick(dt)`` advances the world, so the loop
      itself produces the elapsed time.
    * **Asynchronous** (a binding to a real robot): ``tick(dt)`` is a no-op — the
      robot decelerates on its own clock and confirmation arrives via telemetry.
      Nothing in the loop advances anything, so the loop MUST wait.

    ``sleep`` supplies that wait. Without it the loop burned ``max_ticks`` in
    microseconds and returned ``FAILED`` for a robot that stopped correctly —
    every real estop reported FAILED while the simulated one passed, because the
    simulator was quietly supplying the time. Injectable so unit tests stay
    instant.

    ``max_ticks * tick_dt`` is therefore a **wall-clock budget for a confirmed
    stop**, not a loop count. Treat it as a safety parameter.
    """
    since = command_estop(sim, robot)
    return await_estop_confirmation(sim, robot, tick_dt, max_ticks, sleep, since=since)


def command_estop(sim, robot: str) -> EstopToken | None:
    """Issue the hardware stop and return IMMEDIATELY, without confirming rest.

    Returns an :class:`EstopToken` holding the robot and the binding's ``state_seq(robot)``
    read once ``command_stop`` has returned, or ``None`` for a binding without
    ``state_seq``. Pass it to :func:`await_estop_confirmation` as ``since``: only a state
    report received after the stop was commanded can confirm it. The count is read after
    the command, not before, so a report received while ``command_stop`` runs, which the
    robot sent before the stop, does not count.

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
    try:
        seq = _state_seq(sim, robot)
    except Exception:  # the stop is sent; the wait reads the count on entry, stricter
        _log.exception("state_seq(%r) raised after the stop was commanded", robot)
        return None
    return None if seq is None else EstopToken(robot, seq, id(sim))


def await_estop_confirmation(sim, robot: str, tick_dt: float = 0.05, max_ticks: int = 40,
                             sleep: Callable[[float], None] = time.sleep, *,
                             since: EstopToken | None = None) -> str:
    """Wait for CONFIRMED rest after :func:`command_estop`.

    Returns ``ESTOP_STOPPED`` only once ``sim.is_stopped(robot)`` is true, and
    ``ESTOP_FAILED`` if rest cannot be confirmed within ``max_ticks * tick_dt``. It NEVER
    infers rest from a timeout or from silence -- see :func:`confirm_estop` for why
    ``sleep`` is injected rather than assumed.

    **Only a report newer than the stop confirms it.** A real robot's ``tick()`` does
    nothing, so ``is_stopped()`` answers from the last state report the binding received,
    which may predate the stop: a robot that was at rest a moment before the stop and is
    moving now would be confirmed STOPPED at once. A binding to a real robot therefore
    exposes ``state_seq(robot) -> int``, a count of the state reports it has received from
    that robot. The binding contract:

    * ``state_seq(robot)`` is a method that returns a Python ``int`` and only ever goes up
      for that robot;
    * it is raised only by a NEW state report from that robot: not by another robot's
      report, and not by a duplicate or redelivered message (an MQTT QoS 1 redelivery, a
      retained message);
    * the binding stores the state ``is_stopped`` reads BEFORE it raises ``state_seq``
      (or does both under one lock), so a raised count never pairs with an older state.

    With ``state_seq``, STOPPED needs ``state_seq(robot) > since.seq`` (strictly) and then
    ``is_stopped(robot)``. ``since`` is the :class:`EstopToken` :func:`command_estop`
    returned for this stop; a token for another robot or another binding, or anything that
    is not a token, answers FAILED. A token kept from an earlier stop of the same robot is
    NOT caught (see :class:`EstopToken`). Without a token, the count is read on entry,
    which is after the stop, so it is stricter, never looser. A caller that polls with
    ``max_ticks=1`` should pass ``since``: read anew on every call, the count is passed
    only if a report lands within that one look, so the answer is almost always FAILED
    (safe, but slow to confirm).
    A count that is not a Python ``int`` (a float, a bool, ``None``, a NumPy integer)
    answers FAILED, and so does a ``state_seq`` that is not a method.

    It proves only that the report was RECEIVED after the stop was commanded. A report
    the robot sent before the stop reached it can still count; where the robot's
    protocol acknowledges the stop itself, the binding's ``is_stopped`` should require
    that acknowledgement too. And ``max_ticks * tick_dt`` must exceed the robot's longest
    gap between state reports, or a robot at rest is reported FAILED.

    A binding without ``state_seq`` keeps the plain ``is_stopped`` check, correct only
    when ``tick()`` keeps the state current (a simulator). Such a binding is warned about
    once per class; a simulated binding declares ``tick_keeps_state_current = True`` to
    say so and silence it.
    """
    seq_attr = getattr(sim, "state_seq", None)
    if seq_attr is not None and not callable(seq_attr):
        _log.error("%s.state_seq is %r, not a method state_seq(robot): estop for %r not "
                   "confirmed", type(sim).__qualname__, seq_attr, robot)
        return ESTOP_FAILED
    has_seq = seq_attr is not None
    if not has_seq:
        _warn_without_state_seq(sim)
        after = None
    elif since is None:
        after = sim.state_seq(robot)
    elif not isinstance(since, EstopToken):
        _log.error("estop for %r: since is %r, not the EstopToken command_estop returned; "
                   "estop not confirmed", robot, since)
        return ESTOP_FAILED
    elif since.robot != robot:
        _log.error("estop for %r: since is the token of robot %r's stop; estop not "
                   "confirmed", robot, since.robot)
        return ESTOP_FAILED
    elif since.binding != id(sim):
        _log.error("estop for %r: since is a token from another binding object; estop not "
                   "confirmed", robot)
        return ESTOP_FAILED
    else:
        after = since.seq
    if has_seq and not _is_count(after):
        _log.error("state_seq(%r) gave %r, not an int: estop not confirmed", robot, after)
        return ESTOP_FAILED
    for _ in range(max_ticks):
        sim.tick(tick_dt)
        if not has_seq:
            if sim.is_stopped(robot):
                return ESTOP_STOPPED
        else:
            # The count first, then the state: by the binding contract, a state read after
            # a raised count is at least as new as the report that raised it.
            seen = sim.state_seq(robot)
            if not _is_count(seen):
                _log.error("state_seq(%r) gave %r, not an int: estop not confirmed", robot, seen)
                return ESTOP_FAILED
            if seen > after and sim.is_stopped(robot):
                return ESTOP_STOPPED
        sleep(tick_dt)
    return ESTOP_FAILED


def _is_count(value: object) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _state_seq(sim, robot: str) -> int | None:
    fn = getattr(sim, "state_seq", None)
    return fn(robot) if callable(fn) else None


def _warn_without_state_seq(sim) -> None:
    cls = type(sim)
    if getattr(cls, "tick_keeps_state_current", False) or cls in _warned_without_state_seq:
        return
    _warned_without_state_seq.add(cls)
    _log.warning("%s has no state_seq(robot): estop confirmation relies on tick() keeping "
                 "is_stopped() current, so a state report older than the stop could "
                 "confirm it. Give a real robot's binding state_seq; a simulated binding "
                 "sets tick_keeps_state_current = True.", cls.__qualname__)
