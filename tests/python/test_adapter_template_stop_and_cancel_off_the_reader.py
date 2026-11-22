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

"""An adapter generated from `adapters/template/` answers stops and cancels off the stream reader.

A stop is acknowledged ESTOP_STATE_STOPPING on the reader that carried it, once its ID is latched
and the stop commanded, and STOPPED or FAILED from a worker once rest is confirmed or the 2 s
budget runs out. RFC-0001 v0.4 allows that first STOPPING, and the control plane's delivery window
takes it. So a second stop on the same stream is acknowledged at once even while the first waits
on a robot that does not stop, and a command behind a fence is answered at once. This holds on
the SafetyStream, for a fence on the ControlStream, and on the EdgeStream.

A cancel is answered STOPPED_SAFELY only once a state counted after the stop shows the robot at
rest, within the same 2 s budget, and the wait runs off the ControlStream reader. If rest is not
witnessed in time, a connection that negotiated contract 1.4.0 is told
CANCEL_DISPOSITION_STOP_UNCONFIRMED, and an older one acknowledged=false. A cancel naming an
action the robot no longer runs stops nothing, and the template's binding cannot read which
action the robot is on, so it is answered as not confirmed in the same way.

The robot here takes a set time to come to rest after its stop, or never does. Its stop, its
rest and every message the adapter queues are written to one log in the order they happen.
"""

from __future__ import annotations

import importlib
import importlib.util
import json
import queue
import sys
import threading
import time
import types
from pathlib import Path

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

ROOT = Path(__file__).resolve().parents[2]
ROBOT = "robot-a"
NEVER = None          # a robot that does not come to rest
FIRST_ACK_S = 0.5     # the SafetyStream budget for a stop's first EstopAck
READER_S = 0.2        # how long the reader may spend on one message
BUDGET_S = 2.0        # the stop's confirmation budget (40 looks of 0.05 s)
CANCEL_S = 1.5        # a cancel's answer budget, from its arrival


@pytest.fixture(scope="module")
def rendered(tmp_path_factory):
    """(adapter module, robot module, package name) of a freshly rendered template."""
    spec = importlib.util.spec_from_file_location(
        "check_adapter_template", ROOT / "scripts" / "check-adapter-template.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    dest = tmp_path_factory.mktemp("render")
    mod.render(dest)
    (project,) = [p for p in dest.iterdir() if p.is_dir()]
    package = json.loads((ROOT / "adapters/template/cookiecutter.json").read_text())[
        "python_package"]
    for p in (str(project), str(ROOT / "sdk/python/src")):
        if p not in sys.path:
            sys.path.insert(0, p)
    adapter_mod = importlib.import_module(f"{package}.adapter")
    robot_mod = importlib.import_module(f"{package}.robot")
    yield adapter_mod, robot_mod, package
    sys.path.remove(str(project))


class _Log:
    """Everything that happens, in order: the robot's stop and rest, and each message queued."""

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
        """A queue that logs each message as it is put, so its place among the robot's events
        is exact, not when a reader thread happened to take it."""

        def put(self, item, *args, **kwargs):
            if hasattr(item, "DESCRIPTOR"):   # a message; not a lane's task or an address
                log.add("put", item)
            return super().put(item, *args, **kwargs)
    return LoggingQueue


def _slow_robot(robot_mod, log: _Log, stop_s):
    """The template's SimulatedRobot, braking for `stop_s` seconds after a stop (NEVER: forever).
    Rest is reached on a tick, as a state report, and logged then."""

    class SlowStop(robot_mod.SimulatedRobot):
        def __init__(self) -> None:
            super().__init__()
            self.braking_since = None
            self.sim_time = 0.0     # how far the robot's world has been moved, in seconds
            self.stop_delay = 0.0   # how long the stop command itself takes, as a call can

        def command_stop(self, robot_id: str) -> None:
            time.sleep(self.stop_delay)
            log.add("stop", robot_id)
            if self._moving.get(robot_id) and self.braking_since is None:
                self.braking_since = time.monotonic()
            self._target[robot_id] = None   # the move is abandoned; the robot is still braking
            if stop_s == 0:
                self._moving[robot_id] = False

        def tick(self, dt: float) -> None:
            if (self.braking_since is not None and stop_s is not NEVER
                    and time.monotonic() - self.braking_since >= stop_s):
                for r in self._moving:
                    self._moving[r] = False
                self.braking_since = None
                log.add("rest")
            super().tick(dt)
            self.sim_time += dt

    return SlowStop()


def _adapter(rendered, monkeypatch, stop_s, *, negotiated: str = "1.4.0", edge=None):
    adapter_mod, robot_mod, package = rendered
    log = _Log()
    monkeypatch.setattr(adapter_mod, "queue", types.SimpleNamespace(
        Queue=_logging_queue(log), Empty=queue.Empty))
    robot = _slow_robot(robot_mod, log, stop_s)
    robot.spawn(ROBOT)
    pb_grpc = types.SimpleNamespace(EdgeServiceStub=lambda _channel: edge)
    a = getattr(adapter_mod, f"{package}Adapter")(pb, pb_grpc, robot, ROBOT)
    a._registered = True        # an accepted RegisterAck on this session
    a._negotiated_contract = negotiated
    return a, robot, log


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

    def __init__(self, stream: str, rendered, monkeypatch, stop_s) -> None:
        self.stream = stream
        stub = _Stub()
        self.a, self.robot, self.log = _adapter(rendered, monkeypatch, stop_s, edge=stub)
        stub.log = self.log
        if stream == "safety":
            threading.Thread(target=self.a.run_safety_stream, args=(stub,), daemon=True).start()
            self._s = stub.opened.get(timeout=2)
        elif stream == "edge":
            self.a._dial_edge("edge-node:1")
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
def via(rendered, monkeypatch):
    opened = []

    def make(stream: str, stop_s):
        v = _Via(stream, rendered, monkeypatch, stop_s)
        opened.append(v)
        return v
    yield make
    for v in opened:
        v.close()


STREAMS = ["safety", "fence", "edge"]


# ── part 2: a stop is answered STOPPING on the reader, and confirmed off it ─────────

@pytest.mark.parametrize("stream", STREAMS)
def test_a_second_stops_first_ack_is_not_held_behind_a_first_stop_that_fails(via, stream):
    """The first stop meets a robot that never comes to rest, so its confirmation runs the whole
    2 s budget and ends FAILED. A second stop sent meanwhile must still be acknowledged within
    the SafetyStream's 500 ms budget: its STOPPING must not wait for the first's worker."""
    v = via(stream, NEVER)
    v.robot.command_move(ROBOT, 5.0, 0.0)
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


@pytest.mark.parametrize("stream", STREAMS)
def test_the_first_ack_is_stopping_after_the_stop_and_the_terminal_ack_follows_rest(via, stream):
    """STOPPING is sent once the stop is commanded, carrying stop_initiated_at; STOPPED only once
    a state counted after the stop shows the robot at rest."""
    v = via(stream, 0.3)
    v.robot.command_move(ROBOT, 5.0, 0.0)
    v.stop("e1")
    terminal = _terminal(v.log, "e1")
    assert terminal, "the stop was never confirmed"
    acks = _acks(v.log, "e1")
    first_t, first_state, first_ack = acks[0]
    assert first_state == pb.ESTOP_STATE_STOPPING, "the first EstopAck is STOPPING"
    assert first_ack.stop_initiated_at > 0, "STOPPING carries the moment the stop was commanded"
    stopped_at = _event_at(v.log, "stop")
    assert stopped_at is not None and stopped_at <= first_t, \
        "STOPPING was sent before the stop was commanded"
    t, state, ack = terminal[0]
    assert state == pb.ESTOP_STATE_STOPPED
    rest = _event_at(v.log, "rest")
    assert rest is not None and rest <= t, "STOPPED was sent before the robot was at rest"
    assert ack.confirmed_at_ms > 0


def test_a_command_behind_a_fence_is_answered_at_once(via):
    """A fence's wait for rest does not hold the ControlStream: the AssignAction behind it is
    read at once, and refused under the latch the fence set."""
    v = via("fence", NEVER)
    v.robot.command_move(ROBOT, 5.0, 0.0)
    v.stop("e1", 901)
    v._s.send(pb.Command(
        command_id=902, robot_id=ROBOT, leader_term=1, assign_action=pb.AssignAction(
            action_id="t1", assignment_id="a-t1", action_type="Navigate", fencing_token=1,
            lease_duration_ms=30_000, lease_generation=1,
            payload_json=json.dumps({"x": 2.0, "y": 0.0}).encode())))
    sent = _sent_at(v.log, lambda m: m.command_id == 902)
    handled = _wait(lambda: [t for t, kind, *rest in v.log.snapshot()
                             if kind == "handled" and rest[0] == 902], 3 * BUDGET_S)
    assert handled, "the command behind the fence was never handled"
    assert handled[0] - sent <= READER_S, (
        f"the command behind the fence waited {handled[0] - sent:.2f} s for the fence's rest")
    results = [rest[0].command_result for _t, kind, *rest in v.log.snapshot()
               if kind == "put" and rest[0].DESCRIPTOR.name == "AdapterMessage"
               and rest[0].HasField("command_result")
               and rest[0].command_result.command_id == 902]
    assert results and not results[0].assign_action.accepted
    assert results[0].assign_action.rejection == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE


def test_a_wait_for_rest_and_the_telemetry_loop_never_both_tick_the_robot(via):
    """A wait for rest ticks the demo robot only while the telemetry loop does not. The loop is
    started here while a stop waits on a robot that never rests: from then on the robot's world
    runs at its own speed, about one second a second, and not twice that. An adapter built from
    the template copies this robot, so it must not run fast while a stop is confirmed."""
    v = via("safety", NEVER)
    v.robot.command_move(ROBOT, 5.0, 0.0)
    v.stop("e1")
    time.sleep(0.3)                                  # the wait is ticking the robot
    threading.Thread(target=v.a._telemetry_loop, args=(None,), daemon=True).start()
    assert _wait(lambda: getattr(getattr(v.a, "_plant", None), "ticking", True), 2)
    time.sleep(0.2)
    t0, s0 = time.monotonic(), v.robot.sim_time
    time.sleep(1.0)
    wall, moved = time.monotonic() - t0, v.robot.sim_time - s0
    assert moved <= 1.3 * wall, f"the robot moved {moved:.2f} s in {wall:.2f} s: two tickers"
    assert moved >= 0.5 * wall, f"the robot moved {moved:.2f} s in {wall:.2f} s: no ticker"


def _reports(log: _Log, estop_id: str, after: float) -> list:
    """The latch reports for `estop_id` queued after `after`: (state, ack)."""
    return [(rest[0].estop_ack.state, rest[0].estop_ack) for t, kind, *rest in log.snapshot()
            if kind == "put" and t > after and rest[0].DESCRIPTOR.name == "AdapterSafetyMessage"
            and rest[0].HasField("estop_ack") and rest[0].estop_ack.estop_id == estop_id
            and rest[0].estop_ack.message == "latched (report)"]


def _refused_assign(v, command_id: int, action_id: str, reader=None) -> float:
    (reader or v._s).send(pb.Command(
        command_id=command_id, robot_id=ROBOT, leader_term=1, assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", action_type="Navigate",
            fencing_token=command_id, lease_duration_ms=30_000, lease_generation=1,
            payload_json=json.dumps({"x": 2.0, "y": 0.0}).encode())))
    sent = _sent_at(v.log, lambda m: m.DESCRIPTOR.name == "Command"
                    and m.command_id == command_id)
    assert _wait(lambda: [t for t, kind, *rest in v.log.snapshot()
                          if kind == "handled" and rest[0] == command_id], 3 * BUDGET_S)
    return sent


def test_a_refusal_under_a_stop_reports_it_stopping_until_answered_then_as_answered(via):
    """Each refusal under the latch reports the latched stops. A stop whose rest is still being
    confirmed is reported STOPPING, and one whose rest was never confirmed FAILED: never
    STOPPED, which would say a robot still moving is at rest."""
    v = via("fence", NEVER)
    v.robot.command_move(ROBOT, 5.0, 0.0)
    v.stop("e1", 901)
    sent = _refused_assign(v, 902, "t1")
    (state, ack), *_ = _reports(v.log, "e1", sent) or [(None, None)]
    assert state == pb.ESTOP_STATE_STOPPING, "a stop still being confirmed was reported " \
        f"{pb.EstopState.Name(state) if state is not None else 'not at all'}"
    assert not ack.confirmed_at_ms
    assert _terminal(v.log, "e1")[0][1] == pb.ESTOP_STATE_FAILED
    sent = _refused_assign(v, 903, "t2")
    (state, ack), *_ = _reports(v.log, "e1", sent) or [(None, None)]
    assert state == pb.ESTOP_STATE_FAILED, "a stop whose rest was never confirmed was reported " \
        f"{pb.EstopState.Name(state) if state is not None else 'not at all'}"
    assert not ack.confirmed_at_ms


def test_a_refusal_while_a_safety_stream_stop_is_commanded_does_not_report_it_stopped(via):
    """A SafetyStream stop is latched before its command is sent, and the command can take time
    (a call to the fleet API). A refusal on the ControlStream in that window reports the stop
    STOPPING: the robot has not even been told to stop yet."""
    v = via("safety", NEVER)
    v.robot.command_move(ROBOT, 5.0, 0.0)
    v.robot.stop_delay = 0.5
    reader = _Reader(v.a, v.log)
    try:
        v.stop("e1")
        assert _wait(lambda: "e1" in v.a._latched, 2), "the stop was not latched"
        sent = _refused_assign(v, 902, "t1", reader)
        assert _event_at(v.log, "stop") is None or _event_at(v.log, "stop") > sent, \
            "the refusal came after the stop was commanded: the window was missed"
        (state, ack), *_ = _reports(v.log, "e1", sent) or [(None, None)]
        assert state == pb.ESTOP_STATE_STOPPING, "a stop not yet commanded was reported " \
            f"{pb.EstopState.Name(state) if state is not None else 'not at all'}"
    finally:
        reader.close()


def test_a_stop_is_answered_once_even_if_what_follows_its_answer_fails(via, monkeypatch):
    """After a fence's answer, a clear kept for it is taken. If that fails, the fence is not
    answered a second time, FAILED after STOPPED."""
    v = via("fence", 0)
    v.a._on_estop_clear(pb.EstopClear(estop_id="e1", leader_term=1), v.a._safety_outbox)

    def take_clear(*_args):
        raise RuntimeError("a fault taking the clear")
    monkeypatch.setattr(v.a, "_take_clear", take_clear)
    v.stop("e1")
    assert _terminal(v.log, "e1"), "the fence was never answered"
    time.sleep(0.3)
    assert [s for _t, s, _a in _acks(v.log, "e1")] == [
        pb.ESTOP_STATE_STOPPING, pb.ESTOP_STATE_STOPPED], "the fence was answered twice"


def test_a_lapsed_lease_ends_with_rest_confirmed_while_the_telemetry_loop_runs(rendered,
                                                                               monkeypatch):
    """The lease expires on the telemetry loop, which is what ticks the demo robot. Its wait for
    rest runs elsewhere, so the robot keeps being ticked, its rest is seen, and the action ends
    "ended by lease expiry", not "rest not confirmed"."""
    a, _robot, log = _adapter(rendered, monkeypatch, 0.3)
    reader = _Reader(a, log)
    try:
        reader.send(pb.Command(
            command_id=1, robot_id=ROBOT, leader_term=1, assign_action=pb.AssignAction(
                action_id="t1", assignment_id="a-t1", action_type="Navigate", fencing_token=1,
                lease_duration_ms=300, lease_generation=1,
                payload_json=json.dumps({"x": 5.0, "y": 0.0}).encode())))
        assert _wait(lambda: a._current_action == "t1", 2), "the assignment was not taken"
        threading.Thread(target=a._telemetry_loop, args=(None,), daemon=True).start()

        def ended():
            return [rest[0].action_status for _t, kind, *rest in log.snapshot()
                    if kind == "put" and rest[0].DESCRIPTOR.name == "AdapterMessage"
                    and rest[0].HasField("action_status")
                    and rest[0].action_status.state == pb.ACTION_STATE_FAILED]
        end = _wait(ended, BUDGET_S + 3.0)
        assert end, "the lapsed lease's end was never reported"
        assert end[0].end_cause == pb.ACTION_END_CAUSE_LEASE_EXPIRED
        assert end[0].message == "ended by lease expiry", end[0].message
    finally:
        a._shutdown.set()
        reader.close()


# ── part 1: a cancel is answered STOPPED_SAFELY only on witnessed rest ───────────────

def _assign(reader: _Reader, action_id: str, token: int) -> None:
    reader.send(pb.Command(
        command_id=token, robot_id=ROBOT, leader_term=1, assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", action_type="Navigate",
            fencing_token=token, lease_duration_ms=30_000, lease_generation=1,
            payload_json=json.dumps({"x": 5.0, "y": 0.0}).encode())))


def _cancel(reader: _Reader, action_id: str, command_id: int = 50) -> None:
    reader.send(pb.Command(command_id=command_id, robot_id=ROBOT, leader_term=1,
                           cancel_action=pb.CancelAction(action_id=action_id, fencing_token=1)))


def _cancel_answer(log: _Log, command_id: int = 50, timeout: float = BUDGET_S + 1.5):
    def answered():
        return [(t, rest[0].command_result.cancel_action) for t, kind, *rest in log.snapshot()
                if kind == "put" and rest[0].DESCRIPTOR.name == "AdapterMessage"
                and rest[0].HasField("command_result")
                and rest[0].command_result.command_id == command_id]
    return _wait(answered, timeout)


def _handled_in(log: _Log, command_id: int) -> float:
    sent = _sent_at(log, lambda m: m.command_id == command_id)
    done = _wait(lambda: [t for t, kind, *rest in log.snapshot()
                          if kind == "handled" and rest[0] == command_id], 3 * BUDGET_S)
    assert done, f"command {command_id} was never handled"
    return done[0] - sent


@pytest.fixture
def control(rendered, monkeypatch):
    made = []

    def make(stop_s, negotiated: str = "1.4.0"):
        a, robot, log = _adapter(rendered, monkeypatch, stop_s, negotiated=negotiated)
        reader = _Reader(a, log)
        made.append((a, reader))
        _assign(reader, "t1", 1)
        assert _wait(lambda: a._current_action == "t1", 2), "the assignment was not taken"
        return a, robot, log, reader
    yield make
    for a, reader in made:
        a._shutdown.set()
        reader.close()


def test_a_cancel_is_answered_stopped_safely_only_after_rest_and_off_the_reader(control):
    _a, _robot, log, reader = control(0.3)
    _cancel(reader, "t1")
    answer = _cancel_answer(log)
    assert answer, "the cancel was never answered"
    t, res = answer[0]
    assert res.acknowledged and res.disposition == pb.CANCEL_DISPOSITION_STOPPED_SAFELY
    rest = _event_at(log, "rest")
    assert rest is not None and rest <= t, "STOPPED_SAFELY was sent before the robot was at rest"
    assert _handled_in(log, 50) <= READER_S, "the cancel's wait for rest held the reader"


@pytest.mark.parametrize("negotiated,acknowledged,disposition", [
    ("1.4.0", True, pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED),
    ("1.3.0", False, pb.CANCEL_DISPOSITION_UNSPECIFIED),
    ("", False, pb.CANCEL_DISPOSITION_UNSPECIFIED),
])
def test_a_cancel_whose_rest_is_not_witnessed_in_its_budget_is_not_a_safe_stop(
        control, negotiated, acknowledged, disposition):
    _a, _robot, log, reader = control(NEVER, negotiated)
    _cancel(reader, "t1")
    answer = _cancel_answer(log)
    assert answer, "the cancel was never answered"
    t, res = answer[0]
    assert (res.acknowledged, res.disposition) == (acknowledged, disposition)
    waited = t - _sent_at(log, lambda m: m.command_id == 50)
    assert CANCEL_S - 0.1 <= waited <= CANCEL_S + 0.25, f"answered after {waited:.2f} s"


@pytest.mark.parametrize("negotiated,acknowledged,disposition", [
    ("1.4.0", True, pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED),
    ("1.3.0", False, pb.CANCEL_DISPOSITION_UNSPECIFIED),
])
def test_a_cancel_naming_an_action_not_running_stops_nothing_and_is_not_confirmed(
        control, negotiated, acknowledged, disposition):
    """C3.9: the action named was replaced in place. Stopping would abort the one running, and
    the template's binding cannot read which action the robot is on, so the answer is not a
    safe stop."""
    a, robot, log, reader = control(0.3, negotiated)
    _cancel(reader, "t0")
    answer = _cancel_answer(log)
    assert answer, "the cancel was never answered"
    _t, res = answer[0]
    assert (res.acknowledged, res.disposition) == (acknowledged, disposition)
    assert _event_at(log, "stop") is None, "a cancel for an action not running stopped the robot"
    assert a._current_action == "t1" and not robot.is_stopped(ROBOT)


# ── a fault answering a fence does not strand the clear that follows it ──────────────

def _clear_acks(log: _Log, estop_id: str) -> list[float]:
    return [t for t, kind, *rest in log.snapshot()
            if kind == "put" and rest[0].DESCRIPTOR.name == "AdapterSafetyMessage"
            and rest[0].HasField("estop_clear_ack")
            and rest[0].estop_clear_ack.estop_id == estop_id]


def _assign_accepted(log: _Log, command_id: int, timeout: float = 3.0):
    def results():
        return [rest[0].command_result.assign_action for _t, kind, *rest in log.snapshot()
                if kind == "put" and rest[0].DESCRIPTOR.name == "AdapterMessage"
                and rest[0].HasField("command_result")
                and rest[0].command_result.command_id == command_id]
    return _wait(results, timeout)


def _faulting_put(target, fails):
    """Make `target.put` raise, once, for the first message `fails` picks out."""
    real, faulted = target.put, []

    def put(item, *args, **kwargs):
        if not faulted and hasattr(item, "DESCRIPTOR") and fails(item):
            faulted.append(item)
            raise RuntimeError("a fault sending the fence's answer")
        return real(item, *args, **kwargs)
    target.put = put
    return faulted


FAULTS = ["the final answer", "the receipt", "the stop command"]


@pytest.mark.parametrize("fault", FAULTS)
def test_a_fault_answering_a_fence_does_not_strand_its_clear(via, fault):
    """Stop, fence, EstopClear: the clear takes effect once the fence has arrived, and is
    answered with an EstopClearAck, and the robot takes work again. That holds when the fence's
    final EstopAck cannot be sent, when its ControlStream receipt cannot, and when the stop
    itself cannot be commanded (then the fence is answered FAILED at once): the fence has
    arrived all the same, so the clear is not kept for a fence that is never recorded."""
    v = via("fence", 0)
    v.robot.command_move(ROBOT, 5.0, 0.0)
    if fault == "the final answer":
        faulted = _faulting_put(v.a._safety_outbox, lambda m: m.HasField("estop_ack")
                                and m.estop_ack.state != pb.ESTOP_STATE_STOPPING)
    elif fault == "the receipt":
        faulted = _faulting_put(v.a._outbox, lambda m: m.HasField("command_result")
                                and m.command_result.HasField("estop"))
    else:
        faulted, real_stop = [], v.robot.command_stop

        def command_stop(robot_id):
            if not faulted:
                faulted.append(robot_id)
                raise RuntimeError("a fault commanding the stop")
            return real_stop(robot_id)
        v.robot.command_stop = command_stop
    v.stop("e1", 901)
    # The reader is done with the fence (a fault that escapes it leaves no "handled").
    _wait(lambda: [t for t, kind, *rest in v.log.snapshot()
                   if kind == "handled" and rest[0] == 901], 2)
    assert "e1" in v.a._latched, "the fence did not latch its stop"
    assert v.a._lane.idle(BUDGET_S + 1.5), "the fence's confirmation never finished"
    assert faulted, f"{fault} never faulted: the test proves nothing"
    # The clear, as the SafetyStream reader hands it on: to the robot's lane.
    v.a._lane.submit(lambda: v.a._on_estop_clear(pb.EstopClear(estop_id="e1", leader_term=1),
                                                 v.a._safety_outbox))
    assert v.a._lane.idle(2), "the clear was never taken"
    assert _wait(lambda: _clear_acks(v.log, "e1"), 1.0), \
        f"the clear was never answered when {fault} faulted: the fence was not recorded"
    assert "e1" not in v.a._latched, "the clear did not lift the latch"
    _assign(v._s, "t2", 902)
    (result, *_) = _assign_accepted(v.log, 902) or [None]
    assert result is not None and result.accepted, f"work after the clear was refused: {result}"
