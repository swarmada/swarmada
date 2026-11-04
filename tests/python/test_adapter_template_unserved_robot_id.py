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

"""An adapter generated from `adapters/template/` acts only for the robot it serves.

One ControlStream carries the commands for every Robot bound to an adapter, and each `Command`
names its robot in `robot_id` (RFC-0001 #fleet-adapter-protocol-the-controlstream-service). A
generated adapter serves one robot. A command addressed to any other id is for a robot it does
not serve: carrying it out drives or stops the wrong robot. So every arm that acts on the robot
is answered with the negative the contract defines for it, echoing the id it was addressed to,
with a reason that names that id and the served one, and nothing reaches the robot:

* ``assign_action``: declined ``ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE``; the robot does not
  move and the fencing high-water mark is not raised.
* ``cancel_action``: not acknowledged; the action that is running keeps running.
* ``renew_lease``, ``pause``, ``resume``: answered negative; motion and lease are untouched.
* a SafetyStream ``estop``: answered ``ESTOP_STATE_FAILED`` for the id it named, and this
  robot is not stopped by it.

An action the adapter accepted keeps the robot id it was assigned under for its whole life:
the assignment's result, every renewal and cancel result, and every TelemetryPayload that
reports it as the current action carry that id. An ActionStatusUpdate names its action, not a
robot (the control plane attributes it through the action's assigned robot), so for status
updates the test requires the action id.

The template is rendered the way `scripts/check-adapter-template.py` renders it, and the
rendered adapter is driven with the real protocol stubs.
"""

from __future__ import annotations

import importlib
import importlib.util
import json
import queue
import sys
import time
from pathlib import Path

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

ROOT = Path(__file__).resolve().parents[2]
ROBOT = "robot-a"
OTHER = "robot-b"


@pytest.fixture(scope="module")
def rendered(tmp_path_factory):
    """(adapter module, robot module) of a freshly rendered template."""
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


def _adapter(rendered):
    adapter_mod, robot_mod, package = rendered
    robot = robot_mod.SimulatedRobot()
    robot.spawn(ROBOT)
    a = getattr(adapter_mod, f"{package}Adapter")(pb, None, robot, ROBOT)
    a._registered = True  # an accepted RegisterAck on this session
    return a, robot


def _drain(a) -> list:
    out = []
    while True:
        try:
            out.append(a._outbox.get_nowait())
        except queue.Empty:
            return out


def _command(robot_id: str, command_id: int, **arm):
    return pb.Command(command_id=command_id, robot_id=robot_id, **arm)


def _assign(robot_id: str, action_id: str, token: int, x: float = 2.0):
    return _command(robot_id, token, assign_action=pb.AssignAction(
        action_id=action_id, action_type="Navigate", fencing_token=token,
        assignment_id=f"as-{action_id}", lease_duration_ms=30_000, lease_generation=1,
        payload_json=json.dumps({"x": x, "y": 0.0}).encode()))


def _result(a, cmd):
    _drain(a)
    a._handle_command(cmd)
    results = [m.command_result for m in _drain(a) if m.WhichOneof("payload") == "command_result"]
    assert len(results) == 1, results
    return results[0]


def _tick(robot, seconds: float = 0.5) -> None:
    for _ in range(int(seconds / 0.05)):
        robot.tick(0.05)


def _names_both(message: str) -> bool:
    return OTHER in message and ROBOT in message


# ── commands addressed to a robot this adapter does not serve ──────────────────


def test_an_assignment_for_an_unserved_robot_is_declined_and_moves_nothing(rendered) -> None:
    a, robot = _adapter(rendered)
    before = robot.pose(ROBOT)
    res = _result(a, _assign(OTHER, "for-b", token=7))
    _tick(robot)
    assert res.robot_id == OTHER, "the refusal must echo the id it was addressed to"
    assert not res.assign_action.accepted
    assert res.assign_action.rejection == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE, (
        pb.AssignActionRejection.Name(res.assign_action.rejection))
    assert _names_both(res.assign_action.message), res.assign_action.message
    assert robot.pose(ROBOT) == before, f"an assignment for {OTHER!r} moved {ROBOT!r}"
    assert a._current_action == ""
    assert a._fence.highest(ROBOT) in (None, 0), "the refusal raised the fencing high-water mark"


def test_a_cancel_for_an_unserved_robot_is_refused_and_the_running_action_keeps_running(
        rendered) -> None:
    a, robot = _adapter(rendered)
    assert _result(a, _assign(ROBOT, "for-a", token=3, x=5.0)).assign_action.accepted
    res = _result(a, _command(OTHER, 4, cancel_action=pb.CancelAction(action_id="for-a")))
    _tick(robot, 0.2)
    assert res.robot_id == OTHER
    assert not res.cancel_action.acknowledged
    assert _names_both(res.cancel_action.message), res.cancel_action.message
    assert a._current_action == "for-a", f"a cancel for {OTHER!r} ended {ROBOT!r}'s action"
    assert not robot.is_stopped(ROBOT), f"a cancel for {OTHER!r} stopped {ROBOT!r}"


@pytest.mark.parametrize("arm, field, check", [
    ("renew_lease", pb.RenewActionLease(action_id="for-a", lease_duration_ms=30_000,
                                        lease_generation=2),
     lambda r: not r.renew_lease.renewed and _names_both(r.renew_lease.message)),
    ("pause", pb.PauseCapabilities(require_stop_before_ack=True),
     lambda r: not r.pause.paused and _names_both(r.pause.message)),
    ("resume", pb.ResumeCapabilities(),
     lambda r: not r.resume.resumed and _names_both(r.resume.message)),
])
def test_lease_pause_and_resume_for_an_unserved_robot_are_refused(rendered, arm, field,
                                                                   check) -> None:
    a, robot = _adapter(rendered)
    assert _result(a, _assign(ROBOT, "for-a", token=3, x=5.0)).assign_action.accepted
    res = _result(a, _command(OTHER, 9, **{arm: field}))
    _tick(robot, 0.2)
    assert res.robot_id == OTHER
    assert not res.unsupported, f"{arm} for {OTHER!r} was declined as unsupported, not refused"
    assert check(res), res
    assert a._current_action == "for-a" and not robot.is_stopped(ROBOT), (
        f"{arm} for {OTHER!r} acted on {ROBOT!r}")


# ── estop on the SafetyStream ──────────────────────────────────────────────────


class _OneEstop:
    """A SafetyStream that delivers one estop and closes once the adapter answers it."""

    def __init__(self, robot_id: str) -> None:
        self._cp = pb.ControlPlaneSafetyMessage(
            robot_id=robot_id, estop=pb.Estop(estop_id="e-1", reason="test"))
        self.acks: list = []

    def SafetyStream(self, requests):  # noqa: N802 -- the gRPC method name
        def responses():
            yield self._cp
            # At contract 1.4.0 the adapter also reports its latch on each SafetyStream that
            # opens (RFC-0001 #fleet-adapter-protocol-the-stop-latch). The answer to this estop
            # is the EstopAck that names it.
            for msg in requests:
                if (msg.WhichOneof("payload") == "estop_ack"
                        and msg.estop_ack.estop_id == self._cp.estop.estop_id):
                    self.acks.append(msg)
                    return

        return responses()


def test_an_estop_for_an_unserved_robot_is_answered_failed_and_stops_nothing(rendered) -> None:
    a, robot = _adapter(rendered)
    assert _result(a, _assign(ROBOT, "for-a", token=3, x=5.0)).assign_action.accepted
    stub = _OneEstop(OTHER)
    a.run_safety_stream(stub)
    _tick(robot, 0.2)
    (ack,) = stub.acks
    assert ack.robot_id == OTHER
    assert ack.estop_ack.state == pb.ESTOP_STATE_FAILED, (
        f"an estop for {OTHER!r} was answered {pb.EstopState.Name(ack.estop_ack.state)}: this "
        f"adapter cannot confirm a robot it does not serve is at rest")
    assert _names_both(ack.estop_ack.message), ack.estop_ack.message
    assert not ack.estop_ack.HasField("stop_initiated_at"), "no stop was commanded"
    assert not robot.is_stopped(ROBOT), f"an estop for {OTHER!r} stopped {ROBOT!r}"


def test_an_estop_for_the_served_robot_is_still_confirmed(rendered) -> None:
    a, robot = _adapter(rendered)
    stub = _OneEstop(ROBOT)
    a.run_safety_stream(stub)
    (ack,) = stub.acks
    assert ack.robot_id == ROBOT
    assert ack.estop_ack.state == pb.ESTOP_STATE_STOPPED
    assert ack.estop_ack.HasField("stop_initiated_at")


# ── an accepted action keeps its robot id for its whole life ──────────────────


def test_an_accepted_action_carries_its_robot_id_for_its_whole_life(rendered) -> None:
    a, robot = _adapter(rendered)
    res = _result(a, _assign(ROBOT, "act-1", token=3, x=0.3))
    assert res.robot_id == ROBOT and res.assign_action.accepted

    renew = _result(a, _command(ROBOT, 4, renew_lease=pb.RenewActionLease(
        action_id="act-1", lease_duration_ms=30_000, lease_generation=2)))
    assert renew.robot_id == ROBOT and renew.renew_lease.running

    # Telemetry while it runs, and the arrival that completes it.
    a._session_ready = True
    a._telemetry_interval = 0.0
    t = __import__("threading").Thread(target=a._telemetry_loop, args=(None,), daemon=True)
    t.start()
    deadline = time.monotonic() + 10.0
    while a._current_action and time.monotonic() < deadline:
        time.sleep(0.05)
    time.sleep(0.2)
    a._shutdown.set()
    t.join(timeout=2.0)
    msgs = _drain(a)
    tele = [m.telemetry for m in msgs if m.WhichOneof("payload") == "telemetry"]
    statuses = [m.action_status for m in msgs if m.WhichOneof("payload") == "action_status"]
    running = [tm for tm in tele if tm.current_action == "act-1"]
    assert running, "no TelemetryPayload reported the action while it ran"
    assert {tm.robot_id for tm in tele} == {ROBOT}, {tm.robot_id for tm in tele}
    assert [s.action_id for s in statuses] == ["act-1"] and statuses[0].state == (
        pb.ACTION_STATE_SUCCEEDED), statuses

    # A second action, cancelled by the control plane under the same robot id.
    a._shutdown.clear()
    assert _result(a, _assign(ROBOT, "act-2", token=5, x=5.0)).robot_id == ROBOT
    cancel = _result(a, _command(ROBOT, 6, cancel_action=pb.CancelAction(action_id="act-2")))
    assert cancel.robot_id == ROBOT and cancel.cancel_action.acknowledged
