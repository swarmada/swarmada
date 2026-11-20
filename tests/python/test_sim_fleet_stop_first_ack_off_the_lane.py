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

"""A simulation adapter serving several robots acknowledges a stop STOPPING on the reader.

SimFleet already confirms a robot's rest in that robot's lane, off the stream readers. But a
robot's stops are confirmed in the order they came, so a second stop for a robot whose first stop
is still being confirmed was not acknowledged until the first one's wait ended, which for a robot
that does not come to rest is the whole 2 s budget. An edge stop was confirmed on the EdgeStream
reader itself. Now every stop is acknowledged ESTOP_STATE_STOPPING once it is latched and
commanded, on the reader, and STOPPED or FAILED once rest is confirmed or the budget runs out.
This holds on the SafetyStream, for a fence on the ControlStream, and on the EdgeStream.
"""

from __future__ import annotations

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
from adapters.simulation.sim_adapter import SimFleet  # noqa: E402
from adapters.simulation.simulator import KinematicSim  # noqa: E402

IDS = ["amr-1", "amr-2"]
ROBOT = IDS[0]
EDGE = "edge-node:1"
FIRST_ACK_S = 0.5     # the SafetyStream budget for a stop's first EstopAck
BUDGET_S = 2.0        # the stop's confirmation budget (40 looks of 0.05 s)


class _Log:
    """Everything that happens, in order: the plant's stops and rests, and each message queued."""

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
        def put(self, item, *args, **kwargs):
            if hasattr(item, "DESCRIPTOR"):   # a message; not a lane's task or an address
                log.add("put", item)
            return super().put(item, *args, **kwargs)
    return LoggingQueue


class _Plant(KinematicSim):
    """KinematicSim, logging each stop and the tick on which a robot comes to rest. With
    `never`, a stopping robot keeps its speed."""

    def __init__(self, log: _Log, never: bool = False) -> None:
        super().__init__()
        self._log, self._never = log, never

    def command_stop(self, robot_id: str) -> None:
        self._log.add("stop", robot_id)
        super().command_stop(robot_id)

    def tick(self, dt: float) -> None:
        was_stopped = {r: self.is_stopped(r) for r in self._robots}
        held = {r: (s.vx, s.vy) for r, s in self._robots.items() if s.stopping and self._never}
        super().tick(dt)
        for r, (vx, vy) in held.items():
            self._robots[r].vx, self._robots[r].vy = vx, vy
        for r, stopped in was_stopped.items():
            if not stopped and self.is_stopped(r):
                self._log.add("rest", r)


class _Stream:
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
    def __init__(self) -> None:
        self.log: _Log | None = None
        self.opened: queue.Queue = queue.Queue()

    def SafetyStream(self, requests):  # noqa: N802 (the gRPC method name)
        s = _Stream(requests, self.log)
        self.opened.put(s)
        return s

    EdgeStream = SafetyStream


class _Reader:
    """The ControlStream reader: one thread handing each message to the fleet in turn."""

    def __init__(self, fleet: SimFleet, log: _Log) -> None:
        self._fleet, self._log, self._q = fleet, log, queue.Queue()
        threading.Thread(target=self._run, daemon=True).start()

    def _run(self) -> None:
        while (cmd := self._q.get()) is not None:
            self._fleet._handle_control_plane(pb.ControlPlaneMessage(command=cmd))

    def send(self, cmd) -> None:
        self._log.add("sent", cmd)
        self._q.put(cmd)

    def close(self) -> None:
        self._q.put(None)


def _acks(log: _Log, estop_id: str) -> list[tuple[float, int, object]]:
    out = []
    for t, kind, *rest in log.snapshot():
        if kind != "put":
            continue
        msg = rest[0]
        if msg.DESCRIPTOR.name in ("AdapterSafetyMessage", "AdapterEdgeMessage") \
                and msg.HasField("estop_ack") and msg.estop_ack.estop_id == estop_id:
            out.append((t, msg.estop_ack.state, msg.estop_ack))
    return out


def _wait(pred, timeout: float):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        got = pred()
        if got:
            return got
        time.sleep(0.01)
    return pred()


def _terminal(log: _Log, estop_id: str, timeout: float = BUDGET_S + 2.0):
    return _wait(lambda: [a for a in _acks(log, estop_id)
                          if a[1] in (pb.ESTOP_STATE_STOPPED, pb.ESTOP_STATE_FAILED)], timeout)


def _event_at(log: _Log, name: str) -> float | None:
    for t, kind, *_ in log.snapshot():
        if kind == name:
            return t
    return None


def _estop(estop_id: str):
    return pb.Estop(estop_id=estop_id, issued_by="test")


class _Via:
    """A fleet of two latching robots, and one stream to send it stops on."""

    def __init__(self, stream: str, monkeypatch, never: bool) -> None:
        self.stream = stream
        self.log = log = _Log()
        monkeypatch.setattr(sim_adapter, "queue", types.SimpleNamespace(
            Queue=_logging_queue(log), Empty=queue.Empty))
        stub = _Stub()
        stub.log = log
        self.fleet = SimFleet(pb, types.SimpleNamespace(EdgeServiceStub=lambda _ch: stub),
                              _Plant(log, never), IDS, "z-1", "simulation", None, latch=True)
        with self.fleet.sim_lock:
            for rid in IDS:
                self.fleet._sim.spawn(rid, 0.0, 0.0)
        for rid in IDS:
            self.fleet.robot(rid)._schedule = lambda _delay, _fn: None
        self.fleet._handle_control_plane(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
            accepted=True, negotiated_contract_version="1.4.0")))   # starts the tick thread
        if stream == "safety":
            threading.Thread(target=self.fleet.run_safety_stream, args=(stub,),
                             daemon=True).start()
            self._s = stub.opened.get(timeout=2)
        elif stream == "edge":
            self.fleet._edge_attach(self.fleet.robot(ROBOT), EDGE)
            self._s = stub.opened.get(timeout=2)
        else:
            self._s = _Reader(self.fleet, log)
        with self.fleet.sim_lock:
            self.fleet._sim.command_move(ROBOT, 50.0, 0.0)
        assert _wait(lambda: not self._stopped(), 2), "the robot never got moving"

    def _stopped(self) -> bool:
        with self.fleet.sim_lock:
            return self.fleet._sim.is_stopped(ROBOT)

    def stop(self, estop_id: str, command_id: int = 900) -> None:
        if self.stream == "safety":
            self._s.send(pb.ControlPlaneSafetyMessage(robot_id=ROBOT, estop=_estop(estop_id)))
        elif self.stream == "edge":
            self._s.send(pb.EdgeControlMessage(estop=_estop(estop_id)))
        else:
            self._s.send(pb.Command(command_id=command_id, robot_id=ROBOT, leader_term=1,
                                    estop=_estop(estop_id)))

    def stop_sent_at(self, estop_id: str) -> float:
        (t,) = [t for t, kind, *rest in self.log.snapshot()
                if kind == "sent" and rest[0].estop.estop_id == estop_id]
        return t

    def close(self) -> None:
        self._s.close()
        self.fleet.shutdown()


@pytest.fixture
def via(monkeypatch):
    opened = []

    def make(stream: str, *, never: bool = False):
        v = _Via(stream, monkeypatch, never)
        opened.append(v)
        return v
    yield make
    for v in opened:
        v.close()


STREAMS = ["safety", "fence", "edge"]


@pytest.mark.parametrize("stream", STREAMS)
def test_a_second_stops_first_ack_is_not_held_behind_a_first_stop_that_fails(via, stream):
    v = via(stream, never=True)
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
    v = via(stream)
    v.stop("e1")
    terminal = _terminal(v.log, "e1")
    assert terminal, "the stop was never confirmed"
    first_t, first_state, first_ack = _acks(v.log, "e1")[0]
    assert first_state == pb.ESTOP_STATE_STOPPING, "the first EstopAck is STOPPING"
    assert first_ack.stop_initiated_at > 0, "STOPPING carries the moment the stop was commanded"
    stopped_at = _event_at(v.log, "stop")
    assert stopped_at is not None and stopped_at <= first_t, \
        "STOPPING was sent before the stop was commanded"
    t, state, _ack = terminal[0]
    assert state == pb.ESTOP_STATE_STOPPED
    rest = _event_at(v.log, "rest")
    assert rest is not None and rest <= t, "STOPPED was sent before the robot was at rest"


def test_an_edge_stop_that_cannot_be_commanded_for_one_robot_is_failed_at_once(via,
                                                                            monkeypatch):
    """Two robots behind one edge node; one robot's stop cannot be commanded. The answer is
    already FAILED: it is sent at once, not after the other robot's wait for rest, and only
    once."""
    v = via("edge", never=True)
    other = v.fleet.robot(IDS[1])
    v.fleet._edge_attach(other, EDGE)
    with v.fleet.sim_lock:
        v.fleet._sim.command_move(IDS[1], 50.0, 0.0)

    def fault():
        raise RuntimeError("a fault commanding the stop")
    monkeypatch.setattr(v.fleet.robot(ROBOT), "_command_stop_now", fault)
    v.stop("e1")
    first = _wait(lambda: _acks(v.log, "e1"), 3 * BUDGET_S)
    assert first, "the edge stop was never answered"
    took = first[0][0] - v.stop_sent_at("e1")
    assert first[0][1] == pb.ESTOP_STATE_FAILED, pb.EstopState.Name(first[0][1])
    assert took <= FIRST_ACK_S, f"FAILED came {took:.2f} s after the stop, behind a wait for rest"
    time.sleep(BUDGET_S + 0.5)
    assert len(_acks(v.log, "e1")) == 1, "the edge stop was answered twice"
