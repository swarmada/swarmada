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

"""A cancel is answered within 1.5 s of its arrival, whatever its robot's lane is doing.

The control plane waits 2 s for a cancel's answer and drops a later one. A late
CANCEL_DISPOSITION_STOP_UNCONFIRMED is lost with it, and once the lease is provably dead the
action is recorded as a safe stop, though nothing saw the robot at rest. So the simulation
adapter (serving one robot, and serving several) and the adapter template answer a cancel within
1.5 s of reading it, the time a stop ahead of it in the robot's lane spends included: STOPPED_SAFELY
if a reading by then shows the robot at rest, otherwise not as a safe stop (STOP_UNCONFIRMED on a
connection at 1.4.0, acknowledged=false below). Each cancel is answered once.

The robot here never comes to rest unless a test says otherwise. Each message the adapter
queues, and each message the test sends, goes to one log with the time it happened.
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

from adapters.simulation import sim_adapter  # noqa: E402
from adapters.simulation.sim_adapter import SimAdapter, SimFleet  # noqa: E402
from adapters.simulation.simulator import KinematicSim  # noqa: E402

ROOT = Path(__file__).resolve().parents[2]
NO_EDGE = types.SimpleNamespace(EdgeServiceStub=lambda _ch: None)   # no EdgeStream is dialled
ROBOT = "amr-1"
CANCEL_S = 1.5        # a cancel's answer budget, from its arrival
STOP_S = 2.0          # a stop's confirmation budget (40 looks of 0.05 s)
EARLY_S = 0.1         # how much sooner than CANCEL_S an unconfirmed answer may come
LATE_S = 0.25         # how much later than CANCEL_S any answer may come
# When the robot's lane must be done with a cancel whose rest never comes, from the cancel's send:
# 0.2 s or more after the lane is done with it within the cancel's budget, and 0.2 s or more
# before a lane that waits a stop's 40 looks for it is done.
LANE_IDLE_S = 1.8
CANCEL_ID = 50


class _Log:
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
        """A queue that logs each message as it is put, not when a reader takes it."""

        def put(self, item, *args, **kwargs):
            if hasattr(item, "DESCRIPTOR"):   # a message; not a lane's task or an address
                log.add("put", item)
            return super().put(item, *args, **kwargs)
    return LoggingQueue


def _wait(pred, timeout: float):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        got = pred()
        if got:
            return got
        time.sleep(0.01)
    return pred()


class _Stream:
    """A SafetyStream the test sends stops on; what the adapter writes is drained."""

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
    def __init__(self, log: _Log) -> None:
        self.log = log
        self.opened: queue.Queue = queue.Queue()

    def SafetyStream(self, requests):  # noqa: N802 (the gRPC method name)
        s = _Stream(requests, self.log)
        self.opened.put(s)
        return s


class _Reader:
    """The ControlStream reader: one thread handing each command to `handle` in turn."""

    def __init__(self, handle, log: _Log) -> None:
        self._handle, self._log, self._q = handle, log, queue.Queue()
        threading.Thread(target=self._run, daemon=True).start()

    def _run(self) -> None:
        while (cmd := self._q.get()) is not None:
            self._handle(cmd)
            self._log.add("handled", cmd.command_id)

    def send(self, cmd) -> None:
        self._log.add("sent", cmd)
        self._q.put(cmd)

    def close(self) -> None:
        self._q.put(None)


class _Plant(KinematicSim):
    """KinematicSim, logging each robot's rest. With `never`, a stopping robot keeps its speed."""

    def __init__(self, log: _Log, never: bool) -> None:
        super().__init__()
        self._log, self._never = log, never

    def tick(self, dt: float) -> None:
        was_stopped = {r: self.is_stopped(r) for r in self._robots}
        held = {r: (s.vx, s.vy) for r, s in self._robots.items() if s.stopping and self._never}
        super().tick(dt)
        for r, (vx, vy) in held.items():
            self._robots[r].vx, self._robots[r].vy = vx, vy
        for r, stopped in was_stopped.items():
            if not stopped and self.is_stopped(r):
                self._log.add("rest", r)


def _assignment(action_id: str, token: int):
    return pb.Command(
        command_id=token, robot_id=ROBOT, leader_term=1,
        assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", action_type="Navigate",
            fencing_token=token, lease_duration_ms=600_000, lease_generation=1,
            payload_json=json.dumps({"x": 50.0, "y": 0.0}).encode()))


class _Rig:
    """One adapter serving ROBOT, its ControlStream reader and its SafetyStream, all logged."""

    log: _Log
    lane = None
    _reader: _Reader
    _safety: _Stream

    def stop(self, estop_id: str) -> None:
        self._safety.send(pb.ControlPlaneSafetyMessage(
            robot_id=ROBOT, estop=pb.Estop(estop_id=estop_id, issued_by="test")))

    def cancel(self, action_id: str, command_id: int = CANCEL_ID) -> None:
        self._reader.send(pb.Command(
            command_id=command_id, robot_id=ROBOT, leader_term=1,
            cancel_action=pb.CancelAction(action_id=action_id, fencing_token=1)))

    def answers(self, command_id: int = CANCEL_ID) -> list[tuple[float, object]]:
        return [(t, msg.command_result.cancel_action) for t, kind, *rest in self.log.snapshot()
                if kind == "put" and (msg := rest[0]).DESCRIPTOR.name == "AdapterMessage"
                and msg.HasField("command_result")
                and msg.command_result.command_id == command_id]

    def answer(self, command_id: int = CANCEL_ID, timeout: float = 2 * STOP_S + 1.0):
        """(seconds from the cancel's send to its first answer, the answer)."""
        got = _wait(lambda: self.answers(command_id), timeout)
        assert got, f"the cancel was not answered within {timeout:g} s"
        (sent,) = [t for t, kind, *rest in self.log.snapshot()
                   if kind == "sent" and getattr(rest[0], "command_id", None) == command_id]
        return got[0][0] - sent, got[0][1]

    def handled(self, command_id: int) -> bool:
        """Whether the reader has handed the command on: a cancel's wait is in the lane by then."""
        return any(kind == "handled" and rest[0] == command_id
                   for _t, kind, *rest in self.log.snapshot())

    def sent_at(self, command_id: int) -> float:
        (t,) = [t for t, kind, *rest in self.log.snapshot()
                if kind == "sent" and getattr(rest[0], "command_id", None) == command_id]
        return t

    def moving(self) -> bool:
        raise NotImplementedError

    def close(self) -> None:
        raise NotImplementedError


class _SimRig(_Rig):
    def __init__(self, tmp_path, monkeypatch, never: bool, negotiated: str) -> None:
        self.log = log = _Log()
        monkeypatch.setattr(sim_adapter, "queue", types.SimpleNamespace(
            Queue=_logging_queue(log), Empty=queue.Empty))
        self.a = a = SimAdapter(pb, NO_EDGE, _Plant(log, never), ROBOT, "z1",
                                "sim", artifact_dir=str(tmp_path), latch=True)
        a._sim.spawn(ROBOT, 0.0, 0.0)
        a._negotiated_contract = negotiated
        self.lane = a._my_lane()
        stub = _Stub(log)
        threading.Thread(target=a.run_safety_stream, args=(stub,), daemon=True).start()
        self._safety = stub.opened.get(timeout=2)
        self._reader = _Reader(a._handle_command, log)
        self._reader.send(_assignment("t1", 1))
        assert _wait(lambda: a._current_action == "t1", 2), "the assignment was not taken"
        with a._sim_lock:
            for _ in range(3):
                a._sim.tick(0.05)       # up to cruise speed toward t1's target
        assert self.moving()

    def moving(self) -> bool:
        with self.a._sim_lock:
            return not self.a._sim.is_stopped(ROBOT)

    def close(self) -> None:
        self.a._shutdown.set()
        self._reader.close()
        self._safety.close()


class _FleetRig(_Rig):
    def __init__(self, monkeypatch, never: bool, negotiated: str) -> None:
        self.log = log = _Log()
        monkeypatch.setattr(sim_adapter, "queue", types.SimpleNamespace(
            Queue=_logging_queue(log), Empty=queue.Empty))
        ids = [ROBOT, "amr-2"]
        stub = _Stub(log)
        self.fleet = fleet = SimFleet(pb, NO_EDGE, _Plant(log, never), ids,
                                      "z-1", "simulation", None, latch=True)
        with fleet.sim_lock:
            for rid in ids:
                fleet._sim.spawn(rid, 0.0, 0.0)
        for rid in ids:
            fleet.robot(rid)._schedule = lambda _delay, _fn: None
        fleet._handle_control_plane(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
            accepted=True, negotiated_contract_version="1.4.0")))   # starts the tick thread
        fleet.robot(ROBOT)._negotiated_contract = negotiated
        self.lane = fleet._lane(ROBOT)
        threading.Thread(target=fleet.run_safety_stream, args=(stub,), daemon=True).start()
        self._safety = stub.opened.get(timeout=2)
        self._reader = _Reader(
            lambda cmd: fleet._handle_control_plane(pb.ControlPlaneMessage(command=cmd)), log)
        self._reader.send(_assignment("t1", 1))
        assert _wait(lambda: fleet.robot(ROBOT)._current_action == "t1", 2), (
            "the assignment was not taken")
        assert _wait(self.moving, 2), "the robot never got moving"

    def moving(self) -> bool:
        with self.fleet.sim_lock:
            return not self.fleet._sim.is_stopped(ROBOT)

    def close(self) -> None:
        self._reader.close()
        self._safety.close()
        self.fleet.shutdown()


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


def _braking_robot(robot_mod, log: _Log, never: bool):
    """The template's SimulatedRobot, braking 0.3 s after a stop, or forever with `never`."""

    class Braking(robot_mod.SimulatedRobot):
        def __init__(self) -> None:
            super().__init__()
            self.braking_since = None

        def command_stop(self, robot_id: str) -> None:
            if self._moving.get(robot_id) and self.braking_since is None:
                self.braking_since = time.monotonic()
            self._target[robot_id] = None   # the move is abandoned; the robot is still braking

        def tick(self, dt: float) -> None:
            if (self.braking_since is not None and not never
                    and time.monotonic() - self.braking_since >= 0.3):
                for r in self._moving:
                    self._moving[r] = False
                self.braking_since = None
                log.add("rest", ROBOT)
            super().tick(dt)

    return Braking()


class _TemplateRig(_Rig):
    def __init__(self, rendered, monkeypatch, never: bool, negotiated: str) -> None:
        adapter_mod, robot_mod, package = rendered
        self.log = log = _Log()
        monkeypatch.setattr(adapter_mod, "queue", types.SimpleNamespace(
            Queue=_logging_queue(log), Empty=queue.Empty))
        self.robot = robot = _braking_robot(robot_mod, log, never)
        robot.spawn(ROBOT)
        self.a = a = getattr(adapter_mod, f"{package}Adapter")(
            pb, NO_EDGE, robot, ROBOT)
        a._registered = True        # an accepted RegisterAck on this session
        a._negotiated_contract = negotiated
        self.lane = a._lane
        stub = _Stub(log)
        threading.Thread(target=a.run_safety_stream, args=(stub,), daemon=True).start()
        self._safety = stub.opened.get(timeout=2)
        self._reader = _Reader(a._handle_command, log)
        self._reader.send(_assignment("t1", 1))
        assert _wait(lambda: a._current_action == "t1", 2), "the assignment was not taken"
        assert self.moving()

    def moving(self) -> bool:
        return bool(self.robot._moving.get(ROBOT))

    def close(self) -> None:
        self.a._shutdown.set()
        self._reader.close()
        self._safety.close()


ADAPTERS = ["sim", "fleet", "template"]


@pytest.fixture
def rig(request, tmp_path, monkeypatch):
    made = []

    def make(which: str, *, never: bool = True, negotiated: str = "1.4.0") -> _Rig:
        if which == "sim":
            r = _SimRig(tmp_path, monkeypatch, never, negotiated)
        elif which == "fleet":
            r = _FleetRig(monkeypatch, never, negotiated)
        else:
            r = _TemplateRig(request.getfixturevalue("rendered"), monkeypatch, never,
                             negotiated)
        made.append(r)
        return r
    yield make
    for r in made:
        r.close()


def _unconfirmed_in_time(took: float, res, *, acknowledged: bool = True) -> None:
    disposition = (pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED if acknowledged
                   else pb.CANCEL_DISPOSITION_UNSPECIFIED)
    assert (res.acknowledged, res.disposition) == (acknowledged, disposition), res
    assert took <= CANCEL_S + LATE_S, (
        f"the cancel was answered {took:.2f} s after it was sent; the control plane stops "
        f"waiting at 2 s, and the adapter's budget is {CANCEL_S:g} s from the cancel's arrival")
    assert took >= CANCEL_S - EARLY_S, (
        f"the cancel was answered as not confirmed after {took:.2f} s, before its budget ran out")


@pytest.mark.parametrize("which", ADAPTERS)
def test_a_cancel_behind_a_stop_that_never_rests_is_answered_within_its_budget(rig, which):
    """The stop ahead of the cancel in the robot's lane waits its whole 2 s budget for a rest
    that never comes. The cancel's answer must not wait for that and then its own wait."""
    r = rig(which)
    r.stop("e1")
    time.sleep(0.1)
    r.cancel("t1")
    took, res = r.answer()
    _unconfirmed_in_time(took, res)


@pytest.mark.parametrize("which", ADAPTERS)
def test_a_cancel_whose_rest_never_comes_is_answered_within_its_budget(rig, which):
    r = rig(which)
    r.cancel("t1")
    took, res = r.answer()
    _unconfirmed_in_time(took, res)


@pytest.mark.parametrize("which", ADAPTERS)
def test_below_1_4_0_a_cancel_whose_rest_never_comes_is_not_acknowledged_within_its_budget(
        rig, which):
    r = rig(which, negotiated="1.3.0")
    r.stop("e1")
    time.sleep(0.1)
    r.cancel("t1")
    took, res = r.answer()
    _unconfirmed_in_time(took, res, acknowledged=False)


@pytest.mark.parametrize("which", ADAPTERS)
def test_a_cancel_answered_at_its_deadline_is_answered_once(rig, which):
    """The deadline answers the cancel while the lane is still on the stop; the lane then
    reaches the cancel and must not answer it again."""
    r = rig(which)
    r.stop("e1")
    time.sleep(0.1)
    r.cancel("t1")
    r.answer()
    assert r.lane.idle(2 * STOP_S + 1.0), "the robot's lane never finished"
    time.sleep(0.2)
    assert len(r.answers()) == 1, f"the cancel was answered {len(r.answers())} times"


@pytest.mark.parametrize("which", ADAPTERS)
def test_a_cancel_whose_robot_rests_within_its_budget_is_a_safe_stop(rig, which):
    r = rig(which, never=False)
    r.cancel("t1")
    took, res = r.answer()
    assert res.acknowledged and res.disposition == pb.CANCEL_DISPOSITION_STOPPED_SAFELY, res
    rest = [t for t, kind, *_ in r.log.snapshot() if kind == "rest"]
    assert rest and rest[0] <= r.sent_at(CANCEL_ID) + took, "answered before the robot rested"
    assert took <= CANCEL_S + LATE_S
    time.sleep(0.3)
    assert len(r.answers()) == 1, f"the cancel was answered {len(r.answers())} times"


@pytest.mark.parametrize("which", ADAPTERS)
def test_a_cancel_holds_its_robots_lane_no_longer_than_its_budget(rig, which):
    """Whatever comes after the cancel in the robot's lane, a stop's confirmation or a clear's
    answer, waits for it: no longer than the cancel's budget."""
    r = rig(which)
    r.cancel("t1")
    assert _wait(lambda: r.handled(CANCEL_ID), 1.0), "the reader never handed the cancel on"
    left = r.sent_at(CANCEL_ID) + LANE_IDLE_S - time.monotonic()
    assert r.lane.idle(left), (
        f"the robot's lane was still on the cancel {LANE_IDLE_S:g} s after it was sent")


def test_a_cancel_naming_another_action_whose_robot_is_off_it_is_answered_within_its_budget(
        rig):
    """The simulation adapter answers a cancel naming an action other than the one running
    STOPPED_SAFELY once a reading shows the robot on the running one. Here the plant has dropped
    the running action's move, so no reading ever does."""
    r = rig("sim", never=False)
    with r.a._sim_lock:
        r.a._sim._robots[ROBOT].target = None      # the plant has dropped t1's move
    r.cancel("t0")
    took, res = r.answer()
    _unconfirmed_in_time(took, res)
