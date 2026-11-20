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

"""The simulation adapter, serving one robot, answers stops and cancels off the stream reader.

A stop is acknowledged ESTOP_STATE_STOPPING on the reader that carried it, once its ID is latched
and the stop commanded, and STOPPED or FAILED from a worker once rest is confirmed or the 2 s
budget runs out. So a second stop on the same stream is acknowledged at once even while the first
waits on a robot that does not stop, and a command behind a fence is answered at once. This holds
on the SafetyStream, for a fence on the ControlStream, and on the EdgeStream.

A cancel is answered STOPPED_SAFELY only once the plant shows the robot at rest, within the same
2 s budget, and the wait runs off the ControlStream reader. If rest is not witnessed in time, a
connection that negotiated contract 1.4.0, from an adapter that declares it (--latch), is told
CANCEL_DISPOSITION_STOP_UNCONFIRMED, and any other is told acknowledged=false. A cancel naming an
action the robot no longer runs stops nothing, and is STOPPED_SAFELY only once a state read after
it shows the robot on the action that replaced it.

The plant is the simulator's own KinematicSim, so the time from a stop to rest is the plant's
deceleration, measured here and not assumed. It can also be made to never come to rest. Its stop,
its rest and every message the adapter queues are written to one log in the order they happen.
"""

from __future__ import annotations

import json
import queue
import threading
import time
import types

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation import sim_adapter  # noqa: E402
from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import KinematicSim  # noqa: E402

ROBOT = "amr-1"
FIRST_ACK_S = 0.5     # the SafetyStream budget for a stop's first EstopAck
READER_S = 0.2        # how long the reader may spend on one message
BUDGET_S = 2.0        # the stop's confirmation budget (40 looks of 0.05 s)


class _Log:
    """Everything that happens, in order: the plant's stop and rest, and each message queued."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.events: list[tuple] = []

    def add(self, *event) -> None:
        with self._lock:
            self.events.append((time.monotonic(), *event))

    def snapshot(self) -> list[tuple]:
        with self._lock:
            return list(self.events)


def _logging_queue(log: _Log):
    class LoggingQueue(queue.Queue):
        """A queue that logs each message as it is put, so its place among the plant's events
        is exact, not when a reader thread happened to take it."""

        def put(self, item, *args, **kwargs):
            if hasattr(item, "DESCRIPTOR"):   # a message; not a lane's task or an address
                log.add("put", item)
            return super().put(item, *args, **kwargs)
    return LoggingQueue


class _Plant(KinematicSim):
    """KinematicSim, logging each stop and the tick on which a robot comes to rest. With
    `never`, a stopping robot keeps its speed: it never comes to rest."""

    def __init__(self, log: _Log, never: bool = False) -> None:
        super().__init__()
        self._log, self._never = log, never
        self.sim_time = 0.0     # how far the world has been moved, in simulated seconds

    def command_stop(self, robot_id: str) -> None:
        self._log.add("stop", robot_id)
        super().command_stop(robot_id)

    def tick(self, dt: float) -> None:
        was_stopped = {r: self.is_stopped(r) for r in self._robots}
        held = {r: (s.vx, s.vy) for r, s in self._robots.items() if s.stopping and self._never}
        super().tick(dt)
        self.sim_time += dt
        for r, (vx, vy) in held.items():
            self._robots[r].vx, self._robots[r].vy = vx, vy
        for r, stopped in was_stopped.items():
            if not stopped and self.is_stopped(r):
                self._log.add("rest", r)


def _adapter(tmp_path, monkeypatch, *, never: bool = False, latch: bool = True,
             negotiated: str = "1.4.0", edge=None, command_derived: bool = False):
    log = _Log()
    monkeypatch.setattr(sim_adapter, "queue", types.SimpleNamespace(
        Queue=_logging_queue(log), Empty=queue.Empty))
    plant = _Plant(log, never)
    pb_grpc = types.SimpleNamespace(EdgeServiceStub=lambda _channel: edge)
    a = SimAdapter(pb, pb_grpc, plant, ROBOT, "z1", "sim", artifact_dir=str(tmp_path),
                   latch=latch, command_derived_plant=command_derived)
    a._sim.spawn(ROBOT, 0.0, 0.0)
    a._negotiated_contract = negotiated
    return a, plant, log


def _cruise(a) -> None:
    """Bring the robot up to cruise speed toward x = 50, with no action: a stop meets it moving."""
    with a._sim_lock:
        a._sim.command_move(ROBOT, 50.0, 0.0)
        for _ in range(3):
            a._sim.tick(0.05)
    assert not a._sim.is_stopped(ROBOT)


class _Stream:
    """One side of a fake bidi stream: what the test sends is read by the adapter; what the
    adapter writes is drained on a thread (its queue already logged it)."""

    def __init__(self, requests, log: _Log) -> None:
        self._in: queue.Queue = queue.Queue()
        self._log = log
        threading.Thread(target=lambda: [None for _ in requests], daemon=True).start()

    def send(self, msg) -> None:
        self._log.add("sent", msg)
        self._in.put(msg)

    def close(self) -> None:
        self._in.put(None)

    def __iter__(self):
        return self

    def __next__(self):
        msg = self._in.get()
        if msg is None:
            raise StopIteration
        return msg


class _Stub:
    """A stub whose SafetyStream and EdgeStream hand back a _Stream the test drives."""

    def __init__(self) -> None:
        self.log: _Log | None = None
        self.opened: queue.Queue = queue.Queue()

    def SafetyStream(self, requests):  # noqa: N802 (the gRPC method name)
        s = _Stream(requests, self.log)
        self.opened.put(s)
        return s

    EdgeStream = SafetyStream


class _Reader:
    """The ControlStream reader: one thread handing each command to the adapter in turn."""

    def __init__(self, a, log: _Log) -> None:
        self._a, self._log, self._q = a, log, queue.Queue()
        threading.Thread(target=self._run, daemon=True).start()

    def _run(self) -> None:
        while (cmd := self._q.get()) is not None:
            self._a._handle_command(cmd)
            self._log.add("handled", cmd.command_id)

    def send(self, cmd) -> None:
        self._log.add("sent", cmd)
        self._q.put(cmd)

    def close(self) -> None:
        self._q.put(None)


# ── what the log says ────────────────────────────────────────────────────────────────

def _acks(log: _Log, estop_id: str) -> list[tuple[float, int, object]]:
    """(time, state, ack) of each EstopAck naming `estop_id` on a SafetyStream or an
    EdgeStream, in order. A ControlStream receipt is not an acknowledgement and is left out."""
    out = []
    for t, kind, *rest in log.snapshot():
        if kind != "put":
            continue
        msg = rest[0]
        if msg.DESCRIPTOR.name in ("AdapterSafetyMessage", "AdapterEdgeMessage") \
                and msg.HasField("estop_ack") and msg.estop_ack.estop_id == estop_id:
            out.append((t, msg.estop_ack.state, msg.estop_ack))
    return out


def _sent_at(log: _Log, match) -> float:
    (t,) = [t for t, kind, *rest in log.snapshot() if kind == "sent" and match(rest[0])]
    return t


def _wait(pred, timeout: float):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        got = pred()
        if got:
            return got
        time.sleep(0.01)
    return pred()


def _terminal(log: _Log, estop_id: str, timeout: float = BUDGET_S + 1.5):
    return _wait(lambda: [a for a in _acks(log, estop_id)
                          if a[1] in (pb.ESTOP_STATE_STOPPED, pb.ESTOP_STATE_FAILED)], timeout)


def _event_at(log: _Log, name: str) -> float | None:
    for t, kind, *_ in log.snapshot():
        if kind == name:
            return t
    return None


# ── the three streams a stop arrives on ──────────────────────────────────────────────

def _estop(estop_id: str):
    return pb.Estop(estop_id=estop_id, issued_by="test")


class _Via:
    """Sends stops to the adapter on one stream, as the control plane or an edge node would."""

    def __init__(self, stream: str, tmp_path, monkeypatch, *, never: bool, latch: bool) -> None:
        self.stream = stream
        stub = _Stub()
        self.a, self.plant, self.log = _adapter(tmp_path, monkeypatch, never=never, latch=latch,
                                                edge=stub)
        stub.log = self.log
        if stream == "safety":
            threading.Thread(target=self.a.run_safety_stream, args=(stub,), daemon=True).start()
            self._s = stub.opened.get(timeout=2)
        elif stream == "edge":
            threading.Thread(target=self.a._edge_loop, daemon=True).start()
            self.a._edge_addr.put("edge-node:1")
            self._s = stub.opened.get(timeout=2)
        else:
            self._s = _Reader(self.a, self.log)

    def stop(self, estop_id: str, command_id: int = 900) -> None:
        if self.stream == "safety":
            self._s.send(pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop=_estop(estop_id)))
        elif self.stream == "edge":
            self._s.send(pb.EdgeControlMessage(estop=_estop(estop_id)))
        else:
            self._s.send(pb.Command(command_id=command_id, robot_id=ROBOT, leader_term=1,
                                    estop=_estop(estop_id)))

    def stop_sent_at(self, estop_id: str) -> float:
        return _sent_at(self.log, lambda m: m.estop.estop_id == estop_id)

    def close(self) -> None:
        self.a._shutdown.set()
        self._s.close()


@pytest.fixture
def via(tmp_path, monkeypatch):
    opened = []

    def make(stream: str, *, never: bool = False, latch: bool = True):
        v = _Via(stream, tmp_path, monkeypatch, never=never, latch=latch)
        opened.append(v)
        return v
    yield make
    for v in opened:
        v.close()


# The fence is a 1.4.0 command: it is read only with --latch. The SafetyStream and the EdgeStream
# are read with or without it.
STREAMS = [("safety", True), ("safety", False), ("fence", True), ("edge", True), ("edge", False)]


# ── part 2: a stop is answered STOPPING on the reader, and confirmed off it ─────────

@pytest.mark.parametrize("stream,latch", STREAMS)
def test_a_second_stops_first_ack_is_not_held_behind_a_first_stop_that_fails(via, stream, latch):
    """The first stop meets a robot that never comes to rest, so its confirmation runs the whole
    2 s budget and ends FAILED. A second stop sent meanwhile must still be acknowledged within
    the SafetyStream's 500 ms budget: its STOPPING must not wait for the first's worker."""
    v = via(stream, never=True, latch=latch)
    _cruise(v.a)
    v.stop("e1", 901)
    time.sleep(0.1)
    v.stop("e2", 902)
    first = _wait(lambda: _acks(v.log, "e2"), FIRST_ACK_S + 3 * BUDGET_S)
    assert first, "the second stop was never acknowledged"
    took = first[0][0] - v.stop_sent_at("e2")
    assert took <= FIRST_ACK_S, (
        f"the second stop's first EstopAck came {took:.2f} s after it was sent, behind the "
        f"first stop's wait for rest")
    assert first[0][1] == pb.ESTOP_STATE_STOPPING
    assert _terminal(v.log, "e1")[0][1] == pb.ESTOP_STATE_FAILED, "a robot that never rested"


@pytest.mark.parametrize("stream,latch", STREAMS)
def test_the_first_ack_is_stopping_after_the_stop_and_the_terminal_ack_follows_rest(
        via, stream, latch):
    """STOPPING is sent once the stop is commanded, carrying stop_initiated_at; STOPPED only once
    the plant shows the robot at rest, after its own deceleration."""
    v = via(stream, latch=latch)
    _cruise(v.a)
    v.stop("e1")
    terminal = _terminal(v.log, "e1")
    assert terminal, "the stop was never confirmed"
    first_t, first_state, first_ack = _acks(v.log, "e1")[0]
    assert first_state == pb.ESTOP_STATE_STOPPING, "the first EstopAck is STOPPING"
    assert first_ack.stop_initiated_at > 0, "STOPPING carries the moment the stop was commanded"
    stopped_at = _event_at(v.log, "stop")
    assert stopped_at is not None and stopped_at <= first_t, \
        "STOPPING was sent before the stop was commanded"
    t, state, ack = terminal[0]
    assert state == pb.ESTOP_STATE_STOPPED
    rest = _event_at(v.log, "rest")
    assert rest is not None and rest <= t, "STOPPED was sent before the robot was at rest"
    assert rest > stopped_at, "the plant took time to come to rest"
    assert ack.confirmed_at_ms > 0


def test_a_command_behind_a_fence_is_answered_at_once(via):
    """A fence's wait for rest does not hold the ControlStream: the AssignAction behind it is
    read at once, and refused under the latch the fence set."""
    v = via("fence", never=True)
    _cruise(v.a)
    v.stop("e1", 901)
    v._s.send(_assignment("t1", 1, command_id=902))
    sent = _sent_at(v.log, lambda m: m.command_id == 902)
    handled = _wait(lambda: [t for t, kind, *rest in v.log.snapshot()
                             if kind == "handled" and rest[0] == 902], 3 * BUDGET_S)
    assert handled, "the command behind the fence was never handled"
    assert handled[0] - sent <= READER_S, (
        f"the command behind the fence waited {handled[0] - sent:.2f} s for the fence's rest")
    results = _results(v.log, 902)
    assert results and not results[0].assign_action.accepted
    assert results[0].assign_action.rejection == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE


def test_a_wait_for_rest_and_the_telemetry_loop_never_both_move_the_world(via):
    """A wait for rest moves the world only while the telemetry loop does not. The loop is
    started here while a stop waits on a robot that never rests: from then on the world runs at
    its own speed, about one simulated second a second, and not twice that."""
    v = via("safety", never=True)
    _cruise(v.a)
    v.stop("e1")
    time.sleep(0.3)                                  # the wait is moving the world
    threading.Thread(target=v.a._telemetry_loop, args=(None,), daemon=True).start()
    assert _wait(lambda: getattr(v.a, "_world_ticking", True), 2)
    time.sleep(0.2)
    t0, s0 = time.monotonic(), v.plant.sim_time
    time.sleep(1.0)
    wall, moved = time.monotonic() - t0, v.plant.sim_time - s0
    assert moved <= 1.3 * wall, f"the world moved {moved:.2f} s in {wall:.2f} s: two movers"
    assert moved >= 0.5 * wall, f"the world moved {moved:.2f} s in {wall:.2f} s: no mover"


def test_a_stop_is_answered_once_even_if_its_bookkeeping_fails(via, monkeypatch):
    """After a stop's final answer, the lane records it (_fleet_stop_answered). If that fails,
    the stop is not answered a second time, FAILED after STOPPED."""
    v = via("safety")
    _cruise(v.a)

    def answered(*_args, **_kwargs):
        raise RuntimeError("a fault recording the answer")
    monkeypatch.setattr(v.a, "_fleet_stop_answered", answered)
    v.stop("e1")
    assert _terminal(v.log, "e1"), "the stop was never answered"
    time.sleep(0.3)
    assert [s for _t, s, _a in _acks(v.log, "e1")] == [
        pb.ESTOP_STATE_STOPPING, pb.ESTOP_STATE_STOPPED], "the stop was answered twice"


# ── part 1: a cancel is answered STOPPED_SAFELY only on witnessed rest ───────────────

def _assignment(action_id: str, token: int, *, command_id: int | None = None):
    return pb.Command(
        command_id=token if command_id is None else command_id, robot_id=ROBOT, leader_term=1,
        assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", action_type="Navigate",
            fencing_token=token, lease_duration_ms=600_000, lease_generation=1,
            payload_json=json.dumps({"x": 50.0, "y": 0.0}).encode()))


def _results(log: _Log, command_id: int) -> list:
    return [rest[0].command_result for _t, kind, *rest in log.snapshot()
            if kind == "put" and rest[0].DESCRIPTOR.name == "AdapterMessage"
            and rest[0].HasField("command_result")
            and rest[0].command_result.command_id == command_id]


def _cancel(reader: _Reader, action_id: str, command_id: int = 50) -> None:
    reader.send(pb.Command(command_id=command_id, robot_id=ROBOT, leader_term=1,
                           cancel_action=pb.CancelAction(action_id=action_id, fencing_token=1)))


def _cancel_answer(log: _Log, command_id: int = 50, timeout: float = BUDGET_S + 1.5):
    def answered():
        return [(t, r.cancel_action) for t, kind, *rest in log.snapshot()
                if kind == "put" and rest[0].DESCRIPTOR.name == "AdapterMessage"
                and rest[0].HasField("command_result")
                and (r := rest[0].command_result).command_id == command_id]
    return _wait(answered, timeout)


def _handled_in(log: _Log, command_id: int) -> float:
    sent = _sent_at(log, lambda m: m.command_id == command_id)
    done = _wait(lambda: [t for t, kind, *rest in log.snapshot()
                          if kind == "handled" and rest[0] == command_id], 3 * BUDGET_S)
    assert done, f"command {command_id} was never handled"
    return done[0] - sent


@pytest.fixture
def control(tmp_path, monkeypatch):
    made = []

    def make(*, never: bool = False, latch: bool = True, negotiated: str = "1.4.0"):
        a, plant, log = _adapter(tmp_path, monkeypatch, never=never, latch=latch,
                                 negotiated=negotiated)
        reader = _Reader(a, log)
        made.append((a, reader))
        reader.send(_assignment("t1", 1))
        assert _wait(lambda: a._current_action == "t1", 2), "the assignment was not taken"
        with a._sim_lock:
            for _ in range(3):
                a._sim.tick(0.05)       # up to cruise speed toward t1's target
        assert not a._sim.is_stopped(ROBOT)
        return a, plant, log, reader
    yield make
    for a, reader in made:
        a._shutdown.set()
        reader.close()


def test_a_cancel_is_answered_stopped_safely_only_after_rest_and_off_the_reader(control):
    _a, _plant, log, reader = control()
    _cancel(reader, "t1")
    answer = _cancel_answer(log)
    assert answer, "the cancel was never answered"
    t, res = answer[0]
    assert res.acknowledged and res.disposition == pb.CANCEL_DISPOSITION_STOPPED_SAFELY
    stopped_at, rest = _event_at(log, "stop"), _event_at(log, "rest")
    assert rest is not None and rest <= t, "STOPPED_SAFELY was sent before the robot was at rest"
    assert stopped_at is not None and rest > stopped_at, "the plant took time to come to rest"
    assert _handled_in(log, 50) <= READER_S, "the cancel's wait for rest held the reader"


@pytest.mark.parametrize("latch,negotiated,acknowledged,disposition", [
    (True, "1.4.0", True, pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED),
    (True, "1.3.0", False, pb.CANCEL_DISPOSITION_UNSPECIFIED),
    (False, "1.4.0", False, pb.CANCEL_DISPOSITION_UNSPECIFIED),   # declares 1.3.0
    (False, "", False, pb.CANCEL_DISPOSITION_UNSPECIFIED),
])
def test_a_cancel_whose_rest_is_not_witnessed_in_2_s_is_not_answered_as_a_safe_stop(
        control, latch, negotiated, acknowledged, disposition):
    _a, _plant, log, reader = control(never=True, latch=latch, negotiated=negotiated)
    _cancel(reader, "t1")
    answer = _cancel_answer(log)
    assert answer, "the cancel was never answered within the stop's budget"
    t, res = answer[0]
    assert (res.acknowledged, res.disposition) == (acknowledged, disposition)
    waited = t - _sent_at(log, lambda m: m.command_id == 50)
    assert BUDGET_S - 0.2 <= waited <= BUDGET_S + 1.0, f"answered after {waited:.2f} s"


def test_a_cancel_naming_an_action_replaced_in_place_stops_nothing_and_is_a_safe_stop(control):
    """C3.9: the robot is on the action running, so the one named is off the robot. Stopping
    would abort the action that replaced it."""
    a, _plant, log, reader = control()
    _cancel(reader, "t0")
    answer = _cancel_answer(log)
    assert answer, "the cancel was never answered"
    _t, res = answer[0]
    assert res.acknowledged and res.disposition == pb.CANCEL_DISPOSITION_STOPPED_SAFELY
    assert _event_at(log, "stop") is None, "a cancel for an action not running stopped the robot"
    assert a._current_action == "t1" and not a._sim.is_stopped(ROBOT)


@pytest.mark.parametrize("latch,negotiated,acknowledged,disposition", [
    (True, "1.4.0", True, pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED),
    (False, "1.4.0", False, pb.CANCEL_DISPOSITION_UNSPECIFIED),
])
def test_a_cancel_naming_another_action_is_not_a_safe_stop_while_the_robot_is_off_the_current_one(
        control, latch, negotiated, acknowledged, disposition):
    """The action named is not the one running, but the plant shows the robot is not on the
    running one either: its move has been dropped. Nothing witnesses the named action off the
    robot, so the cancel is not answered as a safe stop."""
    a, _plant, log, reader = control(latch=latch, negotiated=negotiated)
    with a._sim_lock:
        a._sim._robots[ROBOT].target = None      # the plant has dropped t1's move
    _cancel(reader, "t0")
    answer = _cancel_answer(log)
    assert answer, "the cancel was never answered"
    _t, res = answer[0]
    assert (res.acknowledged, res.disposition) == (acknowledged, disposition)


class _SlowToAcquire:
    """The simulator lock, made slow to take for one thread, once: that thread is held between
    deciding to move the robot and taking the lock to move it, the window a cancel could land
    in."""

    def __init__(self, lock, thread_name: str) -> None:
        self._lock, self._name = lock, thread_name
        self.waiting = threading.Event()

    def __enter__(self):
        if threading.current_thread().name == self._name and not self.waiting.is_set():
            self.waiting.set()
            time.sleep(0.4)
        return self._lock.__enter__()

    def __exit__(self, *exc):
        return self._lock.__exit__(*exc)


def test_a_cancel_and_the_release_of_motion_a_stop_held_are_one_or_the_other(tmp_path,
                                                                            monkeypatch):
    """Work accepted while a stop of the robot is being confirmed is held, and set moving once
    the stop is answered. The release decides to move the robot, then takes the simulator lock
    to move it. A cancel of that work landing between the two must not be undone by the move:
    the robot would drive off after the cancel stopped it and was answered."""
    a, _plant, _log = _adapter(tmp_path, monkeypatch, latch=False, negotiated="1.3.0")
    a._fleet_stop_begun()                             # a stop of this robot, not yet answered
    a._handle_command(_assignment("t2", 2))
    assert a._current_action == "t2" and a._motion_held, "the work was not held under the stop"
    slow = _SlowToAcquire(a._sim_lock, "release")
    a._sim_lock = slow
    release = threading.Thread(target=a._fleet_stop_answered, args=(pb.ESTOP_STATE_STOPPED,),
                               name="release")
    release.start()
    assert slow.waiting.wait(2), "the stop's answer did not go to release the held work"
    a._handle_command(pb.Command(command_id=50, robot_id=ROBOT, cancel_action=pb.CancelAction(
        action_id="t2", fencing_token=2)))
    release.join(2)
    with a._sim_lock:
        heading = a._sim.heading_to(ROBOT)
    assert heading is None, f"the robot was set moving to {heading} after the cancel stopped it"


def test_a_cancel_naming_another_action_on_a_command_derived_plant_is_not_a_safe_stop(
        tmp_path, monkeypatch):
    """With --command-derived-plant, where the robot drives is read from the commands
    themselves, which witnesses nothing: a cancel for an action not running is not a safe stop,
    as a cancel's rest is not."""
    a, _plant, log = _adapter(tmp_path, monkeypatch, command_derived=True)
    reader = _Reader(a, log)
    try:
        reader.send(_assignment("t1", 1))
        assert _wait(lambda: a._current_action == "t1", 2)
        _cancel(reader, "t0")
        answer = _cancel_answer(log)
        assert answer, "the cancel was never answered"
        _t, res = answer[0]
        assert (res.acknowledged, res.disposition) == (
            True, pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED)
    finally:
        a._shutdown.set()
        reader.close()


def test_a_fault_answering_a_fence_does_not_strand_its_clear(tmp_path, monkeypatch):
    """The fence is answered from the robot's worker. If sending that answer faults, the fence
    has still arrived: a clear for it takes effect, is answered, and work is taken again."""
    a, _plant, log = _adapter(tmp_path, monkeypatch)

    def answer_fence(*_args, **_kwargs):
        raise RuntimeError("a fault answering the fence")
    monkeypatch.setattr(a, "_answer_fence", answer_fence)
    a._handle_command(pb.Command(command_id=901, robot_id=ROBOT, leader_term=1,
                                 estop=_estop("e1")))
    a._clear_in_lane(pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop_clear=pb.EstopClear(
        estop_id="e1", leader_term=1)), a._safety_outbox)
    assert a._solo_lane.idle(5.0), "the fence and its clear were not handled within 5 s"
    cleared = [rest[0] for _t, kind, *rest in log.snapshot()
               if kind == "put" and rest[0].DESCRIPTOR.name == "AdapterSafetyMessage"
               and rest[0].HasField("estop_clear_ack")
               and rest[0].estop_clear_ack.estop_id == "e1"]
    assert cleared, "the clear was never answered: the fence was never recorded"
    assert not a._latched, "the clear did not lift the latch"
    a._handle_command(_assignment("t1", 1, command_id=902))
    (res,) = _results(log, 902)
    assert res.assign_action.accepted, f"work was refused after the clear: {res.assign_action}"
