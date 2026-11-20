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

"""A process whose stream is closed as superseded stops every robot it drives and doesn't redial.

RFC-0001 (Connection lifecycle): one adapter identity is served on one ControlStream and one
SafetyStream, by one live process at a time, and a process whose stream is closed as superseded
by a stream it did not open itself MUST stop every robot it drives and hold no work. The control
plane closes a replaced stream with gRPC ABORTED, and ABORTED for nothing else
(internal/controlstream/server.go, supersededStatus and supersededSafetyStatus), so any ABORTED
on either stream counts as superseded, whatever its details say. A stream dropped any other way,
UNAVAILABLE for one, is redialled as before, even if its details say "superseded".

Superseded, the adapter closes its other stream, so a fence on it can't arrive with no
SafetyStream left to answer it; stops every robot and drops the work each holds; doesn't redial
either stream; and stays up, idle, until it is terminated: a process that exited would be
restarted by its supervisor and displace the stream that replaced it.

Each adapter is run against a control plane faked in this file: it accepts the handshake,
admits every robot, assigns each a Navigate far away, then closes one stream with the status
the test names.
"""

from __future__ import annotations

import importlib
import importlib.util
import json
import queue
import sys
import threading
import time
from pathlib import Path

import pytest

grpc = pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import SimAdapter, SimFleet  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

ROOT = Path(__file__).resolve().parents[2]
SUPERSEDED = {
    "control": ('superseded: a newer ControlStream for adapter "sim" in namespace "default" was '
                'accepted; one adapter identity is served on one stream, so this one is closed'),
    "safety": ('superseded: a newer SafetyStream for adapter "sim" in namespace "default" was '
               'accepted; one adapter identity is served on one stream, so this one is closed'),
}
REST_S = 3.0      # the 2 s stopping budget, and a margin
REDIAL_S = 1.5    # longer than either adapter's first redial (at most 0.5 s)


class _Status(grpc.RpcError):
    """A stream ending with a gRPC status, as grpcio raises it from the response iterator."""

    def __init__(self, code, details: str = "") -> None:
        super().__init__(details)
        self._code, self._details = code, details

    def code(self):
        return self._code

    def details(self):
        return self._details


class _Call:
    """One stream as grpcio hands it back: an iterator of responses that can be cancelled.
    It ends with CANCELLED once the adapter cancels it or ends its own side of it."""

    def __init__(self, plane: _Plane, kind: str, n: int) -> None:
        self.plane, self.kind, self.n = plane, kind, n
        self.dialled_at = time.monotonic()
        self.ended = threading.Event()      # the adapter cancelled it or ended its side
        self.status_sent = False            # the plane ended it with a status
        self.responses = iter(())

    def cancel(self) -> None:
        self.ended.set()

    def __iter__(self):
        return self

    def __next__(self):
        return next(self.responses)


class _Plane:
    """A control plane for one adapter. Its first ControlStream accepts the handshake, admits
    every robot and assigns each a Navigate; its first SafetyStream stays open. Each first stream
    then waits until the test closes it with a status, or the adapter ends it. A stream the
    adapter opens later (a redial) is counted, accepted, and open until the adapter ends it."""

    def __init__(self, robots: list[str]) -> None:
        self.robots = robots
        self.calls = {"control": 0, "safety": 0}
        self.dials: list[_Call] = []
        self.ended = {"control": threading.Event(), "safety": threading.Event()}  # first streams
        self.close: dict[str, queue.Queue] = {"control": queue.Queue(), "safety": queue.Queue()}
        self.raised: dict[str, float] = {}   # kind -> when its first stream was ended with a status
        self.assigned = threading.Event()
        self.done = threading.Event()   # the test is over: every stream still open returns

    def open_streams(self) -> list[_Call]:
        return [c for c in self.dials if not c.ended.is_set() and not c.status_sent]

    def _dial(self, kind: str, requests, on_message=None) -> _Call:
        self.calls[kind] += 1
        call = _Call(self, kind, self.calls[kind])
        self.dials.append(call)
        if call.n == 1:
            call.ended = self.ended[kind]

        def run():
            for m in requests:
                if on_message is not None:
                    on_message(m)
            call.ended.set()   # the adapter ended its side of the stream
        threading.Thread(target=run, daemon=True).start()
        return call

    def _hold(self, call: _Call):
        while not self.done.is_set():
            if call.n == 1:
                try:
                    code, details = self.close[call.kind].get(timeout=0.02)
                except queue.Empty:
                    pass
                else:
                    call.status_sent = True
                    self.raised[call.kind] = time.monotonic()
                    raise _Status(code, details)
            else:
                time.sleep(0.02)
            if call.ended.is_set():
                raise _Status(grpc.StatusCode.CANCELLED, "the adapter closed the stream")
        return
        yield  # a generator

    def ControlStream(self, requests):  # noqa: N802 -- the gRPC method name
        registers: queue.Queue = queue.Queue()
        call = self._dial("control", requests, lambda m: registers.put(m)
                          if m.WhichOneof("payload") == "register" else None)

        def responses():
            yield pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
                accepted=True, negotiated_contract_version="1.4.0"))
            if call.n == 1:
                for _ in self.robots:
                    registers.get(timeout=5)
                    yield pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
                        accepted=True, telemetry_interval_seconds=1))
                for i, rid in enumerate(self.robots, start=1):
                    yield pb.ControlPlaneMessage(command=pb.Command(
                        command_id=i, robot_id=rid, leader_term=1,
                        assign_action=pb.AssignAction(
                            action_id=f"act-{rid}", assignment_id=f"asg-{rid}",
                            action_type="Navigate", fencing_token=i, lease_duration_ms=60_000,
                            lease_generation=1,
                            payload_json=json.dumps({"x": 50.0, "y": 0.0}).encode())))
                self.assigned.set()
            yield from self._hold(call)
        call.responses = responses()
        return call

    def SafetyStream(self, requests):  # noqa: N802 -- the gRPC method name
        call = self._dial("safety", requests)
        call.responses = self._hold(call)
        return call


# ── the three adapters, each with the same questions ─────────────────────────────────

class _Run:
    """One adapter, its two stream loops on threads of their own, and what the tests read."""

    def __init__(self, robots, moving, holds_work, stop, deliver, outbox) -> None:
        self.outbox = outbox            # the ControlStream's current outbox
        self.robots = robots
        self.moving = moving            # robot id -> whether it is moving
        self.holds_work = holds_work    # robot id -> the action it holds, or ""
        self.stop = stop
        self.deliver = deliver          # a Command -> its CommandResult, as the reader handles it
        self.control_thread: threading.Thread | None = None

    def start(self, adapter, plane: _Plane) -> None:
        threading.Thread(target=adapter.run_safety_stream, args=(plane,), daemon=True).start()
        self.control_thread = threading.Thread(
            target=adapter.run_control_stream, args=(plane,), daemon=True)
        self.control_thread.start()


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
    yield (importlib.import_module(f"{package}.adapter"),
           importlib.import_module(f"{package}.robot"), package)
    sys.path.remove(str(project))


def _result_on(outbox: queue.Queue, command_id: int):
    deadline = time.monotonic() + 2.0
    while time.monotonic() < deadline:
        try:
            m = outbox.get(timeout=0.05)
        except queue.Empty:
            continue
        if (m is not None and m.WhichOneof("payload") == "command_result"
                and m.command_result.command_id == command_id):
            return m.command_result
    return None


def _template(rendered) -> tuple:
    adapter_mod, robot_mod, package = rendered
    robot, rid = robot_mod.SimulatedRobot(), "robot-a"
    a = getattr(adapter_mod, f"{package}Adapter")(pb, None, robot, rid)

    def stop():
        a._shutdown.set()
        a._outbox.put(None)
        a._safety_outbox.put(None)
    def deliver(cmd):
        a._handle_command(cmd)
        return _result_on(a._outbox, cmd.command_id)
    return a, _Run([rid], lambda r: robot._moving.get(r, False),
                   lambda r: a._current_action, stop, deliver, lambda: a._outbox)


def _sim_alone(_rendered) -> tuple:
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z-1", "sim")

    def moving(r):
        with a._sim_lock:
            return not a._sim.is_stopped(r)
    def deliver(cmd):
        a._handle_command(cmd)
        return _result_on(a._outbox, cmd.command_id)
    return a, _Run(["amr-1"], moving, lambda r: a._current_action, a.shutdown, deliver,
                   lambda: a._outbox)


def _sim_fleet(_rendered) -> tuple:
    ids = ["amr-1", "amr-2", "amr-3"]
    f = SimFleet(pb, None, make_simulator("kinematic"), ids, "z-1", "sim")

    def moving(r):
        with f.sim_lock:
            return not f._sim.is_stopped(r)
    def deliver(cmd):
        f._handle_control_plane(pb.ControlPlaneMessage(command=cmd))
        return _result_on(f.link.outbox, cmd.command_id)
    return f, _Run(ids, moving, lambda r: f.robot(r)._current_action, f.shutdown, deliver,
                   lambda: f.link.outbox)


ADAPTERS = {"template": _template, "sim-alone": _sim_alone, "sim-fleet": _sim_fleet}


@pytest.fixture
def running(rendered):
    started = []

    def make(name: str):
        adapter, run = ADAPTERS[name](rendered)
        plane = _Plane(run.robots)
        plane.fleet = adapter
        run.start(adapter, plane)
        started.append((plane, run))
        assert plane.assigned.wait(5), "the fake control plane never got as far as assigning"
        assert _wait(lambda: all(run.moving(r) for r in run.robots), 3.0), \
            "the assigned robots never started moving"
        return plane, run

    yield make
    for plane, run in started:
        plane.done.set()
        run.stop()


def _telemetry_queued(outbox: queue.Queue) -> int:
    """How many TelemetryPayloads wait on `outbox`. A stream's release sentinel (None) and the
    work's last status reports are not telemetry, and may land after a supersession."""
    with outbox.mutex:
        items = list(outbox.queue)
    return sum(1 for m in items if m is not None and m.WhichOneof("payload") == "telemetry")


def _wait(pred, timeout: float):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if pred():
            return True
        time.sleep(0.02)
    return pred()


OTHER = {"control": "safety", "safety": "control"}
NAME = {"control": "ControlStream", "safety": "SafetyStream"}


@pytest.mark.parametrize("adapter", list(ADAPTERS))
@pytest.mark.parametrize("first,details", [
    ("control", SUPERSEDED["control"]),
    ("safety", SUPERSEDED["safety"]),
    ("control", "the stream was closed"),
], ids=["control-superseded", "safety-superseded", "control-aborted-other-details"])
def test_a_stream_closed_as_superseded_stops_every_robot_and_is_not_redialled(
        running, capfd, adapter, first, details):
    plane, run = running(adapter)
    plane.close[first].put((grpc.StatusCode.ABORTED, details))
    other = OTHER[first]
    assert plane.ended[other].wait(2.0), (
        f"the {NAME[other]} was left open after the {NAME[first]} was closed ABORTED "
        f"({details!r}): the adapter did not take it as superseded")
    assert _wait(lambda: not any(run.moving(r) for r in run.robots), REST_S), (
        "superseded, the adapter left a robot moving: "
        f"{[r for r in run.robots if run.moving(r)]}")
    assert not [r for r in run.robots if run.holds_work(r)], (
        "superseded, the adapter still holds work: "
        f"{ {r: run.holds_work(r) for r in run.robots if run.holds_work(r)} }")
    queued = _telemetry_queued(run.outbox())
    time.sleep(REDIAL_S)
    assert plane.calls == {"control": 1, "safety": 1}, (
        f"superseded, the adapter dialled again: {plane.calls}")
    assert run.control_thread.is_alive(), (
        "superseded, the adapter's ControlStream loop returned: a process that exits is "
        "restarted, and displaces the stream that replaced it")
    assert _telemetry_queued(run.outbox()) == queued, (
        "superseded, the adapter still queues telemetry for a stream that is gone")
    err = capfd.readouterr().err
    assert "SUPERSEDED stream=" in err, "the reason was not written to stderr"
    unlogged = [r for r in run.robots if f"SUPERSEDED_REST robot={r}:" not in err]
    assert not unlogged, f"no robot's rest was logged for {unlogged}"


@pytest.mark.parametrize("adapter", list(ADAPTERS))
@pytest.mark.parametrize("first", ["control", "safety"])
def test_a_stream_dropped_unavailable_is_redialled_and_stops_nothing(running, adapter, first):
    """UNAVAILABLE is a drop, not a supersession, even with "superseded" in its details: the
    adapter redials, and its robots keep the work they hold."""
    plane, run = running(adapter)
    plane.close[first].put((grpc.StatusCode.UNAVAILABLE, SUPERSEDED[first]))
    assert _wait(lambda: plane.calls[first] == 2, 3.0), (
        f"the {NAME[first]} dropped UNAVAILABLE was not redialled: {plane.calls}")
    assert not plane.ended[OTHER[first]].is_set(), f"the {NAME[OTHER[first]]} was closed"
    assert all(run.moving(r) for r in run.robots), (
        f"a drop stopped a robot: {[r for r in run.robots if not run.moving(r)]}")
    assert all(run.holds_work(r) for r in run.robots), "a drop dropped the work"


@pytest.mark.parametrize("adapter", list(ADAPTERS))
def test_work_sent_after_a_supersession_is_refused_and_moves_nothing(running, adapter):
    """Superseded, the adapter holds no work, so an assignment that still reaches it, one the
    reader had already taken off the stream, is refused ROBOT_UNAVAILABLE and moves nothing."""
    plane, run = running(adapter)
    plane.close["safety"].put((grpc.StatusCode.ABORTED, SUPERSEDED["safety"]))
    assert _wait(lambda: not any(run.moving(r) for r in run.robots), REST_S), (
        "superseded, the adapter left a robot moving: "
        f"{[r for r in run.robots if run.moving(r)]}")
    rid = run.robots[0]
    result = run.deliver(pb.Command(
        command_id=99, robot_id=rid, leader_term=1, assign_action=pb.AssignAction(
            action_id="late", assignment_id="asg-late", action_type="Navigate",
            fencing_token=99, lease_duration_ms=60_000, lease_generation=1,
            payload_json=json.dumps({"x": 80.0, "y": 0.0}).encode())))
    assert result is not None, "the assignment was not answered"
    assert not result.assign_action.accepted, "superseded, the adapter accepted new work"
    assert result.assign_action.rejection == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE, \
        pb.AssignActionRejection.Name(result.assign_action.rejection)
    time.sleep(0.5)
    assert not run.moving(rid) and not run.holds_work(rid), "the refused work moved the robot"


@pytest.mark.parametrize("adapter", list(ADAPTERS))
@pytest.mark.parametrize("dropped", ["safety", "control"])
def test_a_stream_in_its_backoff_is_not_dialled_once_the_other_is_superseded(
        running, rendered, monkeypatch, adapter, dropped):
    """One stream drops and waits out its backoff; the other is superseded meanwhile. The
    dropped one is then not dialled again: a redial would displace the process that replaced
    this one, and leave both idle."""
    monkeypatch.setattr(rendered[0], "_backoff_delay", lambda _attempt: 0.5)  # as the sim waits
    plane, run = running(adapter)
    plane.close[dropped].put((grpc.StatusCode.UNAVAILABLE, "connection reset"))
    assert _wait(lambda: dropped in plane.raised, 1.0)
    superseded = OTHER[dropped]
    plane.close[superseded].put((grpc.StatusCode.ABORTED, SUPERSEDED[superseded]))
    assert _wait(lambda: superseded in plane.raised, 1.0)
    time.sleep(REDIAL_S)
    late = [(c.kind, round(c.dialled_at - plane.raised[superseded], 2)) for c in plane.dials
            if c.dialled_at > plane.raised[superseded] + 0.1]
    assert not late, (f"the {NAME[superseded]} was superseded during the {NAME[dropped]}'s "
                      f"backoff, and the adapter dialled again: {late}")
    assert not plane.open_streams(), (
        f"superseded, the adapter left a stream open: {[c.kind for c in plane.open_streams()]}")
    assert not any(run.moving(r) for r in run.robots), "superseded, a robot is still moving"


@pytest.mark.parametrize("fault", ["_interrupt_work", "_give_up"])
def test_one_robots_fault_leaves_no_other_robot_of_the_fleet_moving(running, fault):
    """A fault stopping one robot of the fleet, in dropping its work or in all of its
    supersession, does not leave the robots after it moving, or a stream open."""
    plane, run = running("sim-fleet")
    fleet_robot = plane.fleet.robot("amr-2")

    def fails(*_args, **_kwargs):
        raise RuntimeError(f"a fault in {fault}")
    setattr(fleet_robot, fault, fails)
    plane.close["control"].put((grpc.StatusCode.ABORTED, SUPERSEDED["control"]))
    assert plane.ended["safety"].wait(2.0), "the SafetyStream was left open"
    expected = run.robots if fault == "_interrupt_work" else ["amr-1", "amr-3"]
    assert _wait(lambda: not any(run.moving(r) for r in expected), REST_S), (
        f"a fault in amr-2's {fault} left robots moving: "
        f"{[r for r in expected if run.moving(r)]}")
