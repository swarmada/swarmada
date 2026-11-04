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

"""An adapter generated from `adapters/template/` keeps the stop latch of contract 1.4.0.

The template reports 1.4.0, and RFC-0001 #fleet-adapter-protocol-the-stop-latch binds a
connection at 1.4.0: a stop latches its ID, on SafetyStream or as its fence on ControlStream;
every AssignAction is then refused ROBOT_UNAVAILABLE until the stop's EstopClear lifts it, which
is answered with an EstopClearAck; work the stop interrupted ends FAILED and is never resumed;
a lower leader term is rejected. `tests/python/test_adapter_template_conformance.py` runs the
harness against the generated adapter, C5.8 included; this file drives the rules one at a time.

The template is rendered the way `scripts/check-adapter-template.py` renders it, and the
rendered adapter is driven with the real protocol stubs.
"""

from __future__ import annotations

import importlib
import importlib.util
import json
import queue
import sys
from pathlib import Path

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

ROOT = Path(__file__).resolve().parents[2]
ROBOT = "robot-a"


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


def _adapter(rendered):
    adapter_mod, robot_mod, package = rendered
    robot = robot_mod.SimulatedRobot()
    robot.spawn(ROBOT)
    a = getattr(adapter_mod, f"{package}Adapter")(pb, None, robot, ROBOT)
    a._registered = True  # an accepted RegisterAck on this session
    return a


def _drain(q: queue.Queue) -> list:
    out = []
    while True:
        try:
            item = q.get_nowait()
        except queue.Empty:
            return out
        if item is not None:
            out.append(item)


def _results(a) -> list:
    return [m.command_result for m in _drain(a._outbox)
            if m.WhichOneof("payload") == "command_result"]


def _assign(a, action_id: str, token: int, *, term: int = 1):
    a._handle_command(pb.Command(
        command_id=token, robot_id=ROBOT, leader_term=term, assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", action_type="Navigate",
            fencing_token=token, lease_duration_ms=30_000, lease_generation=1,
            payload_json=json.dumps({"x": 2.0, "y": 0.0}).encode())))
    (res,) = [r.assign_action for r in _results(a) if r.HasField("assign_action")]
    return res


def _fence(a, estop_id: str, *, term: int = 1) -> None:
    a._handle_command(pb.Command(command_id=900, robot_id=ROBOT, leader_term=term,
                                 estop=pb.Estop(estop_id=estop_id, issued_by="test")))


def _clear(a, estop_id: str, *, term: int = 1) -> list:
    a._on_estop_clear(pb.EstopClear(estop_id=estop_id, leader_term=term), a._safety_outbox)
    return [m.estop_clear_ack.estop_id for m in _drain(a._safety_outbox)
            if m.WhichOneof("payload") == "estop_clear_ack"]


def test_the_template_reports_1_4_0(rendered):
    assert rendered[0].CONTRACT_VERSION == "1.4.0"


def test_work_is_refused_under_a_stop_until_its_clear_lifts_it(rendered):
    a = _adapter(rendered)
    _fence(a, "e1")
    acks = [m.estop_ack.estop_id for m in _drain(a._safety_outbox)
            if m.WhichOneof("payload") == "estop_ack"]
    assert acks == ["e1"], "the fence is answered with an EstopAck on SafetyStream"
    held = _assign(a, "t1", 1)
    assert not held.accepted
    assert held.rejection == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE
    assert _clear(a, "e1") == ["e1"]
    assert _assign(a, "t2", 2).accepted


def test_a_clear_before_its_fence_waits_for_the_fence(rendered):
    a = _adapter(rendered)
    assert _clear(a, "e1") == []
    _fence(a, "e1")
    lifted = [m.estop_clear_ack.estop_id for m in _drain(a._safety_outbox)
              if m.WhichOneof("payload") == "estop_clear_ack"]
    assert lifted == ["e1"]
    _drain(a._outbox)
    assert _assign(a, "t1", 1).accepted


def test_a_lower_leader_term_is_rejected_and_lifts_nothing(rendered):
    a = _adapter(rendered)
    _fence(a, "e1", term=3)
    _drain(a._outbox)
    assert _clear(a, "e1", term=2) == []
    a._handle_command(pb.Command(command_id=5, robot_id=ROBOT, leader_term=2,
                                 assign_action=pb.AssignAction(action_id="t1", fencing_token=1)))
    (res,) = _results(a)
    assert res.stale_leader_term


def test_interrupted_work_ends_failed_and_is_never_resumed(rendered):
    a = _adapter(rendered)
    assert _assign(a, "t1", 4).accepted
    _drain(a._outbox)
    _fence(a, "e1")
    ended = [m.action_status for m in _drain(a._outbox)
             if m.WhichOneof("payload") == "action_status"
             and m.action_status.state == pb.ACTION_STATE_FAILED]
    assert ended and ended[0].end_cause == pb.ACTION_END_CAUSE_ESTOP
    assert _assign(a, "t1", 4).accepted, "a re-delivery of interrupted work is accepted"
    assert a._current_action == "", "and the robot does not take it up again"


def _session(a, negotiated: str) -> None:
    """A ControlStream whose HelloAck negotiated `negotiated` (the telemetry loop not started)."""
    a._telemetry_started = True
    a._handle_control_plane(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
        accepted=True, negotiated_contract_version=negotiated)), None)
    _drain(a._outbox)


def _answer(a, action_id: str, token: int, *, term: int):
    a._handle_command(pb.Command(
        command_id=token, robot_id=ROBOT, leader_term=term, assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", action_type="Navigate",
            fencing_token=token, lease_duration_ms=30_000, lease_generation=1,
            payload_json=json.dumps({"x": 2.0, "y": 0.0}).encode())))
    (res,) = _results(a)
    return res


def test_a_connection_below_1_4_0_is_not_rejected_for_its_leader_term(rendered):
    """The leader-term rule binds a connection that negotiated 1.4.0 (RFC-0001
    #fleet-adapter-protocol-the-stop-latch, *Version*). A control plane below 1.4.0 sends no
    term (0), and "a connection below 1.4.0 is not rejected" (Note 2). The template kept no
    negotiated version, so after a 1.4.0 session at term 2, a later session that negotiated
    1.3.0 (a control plane rolled back) had every Command rejected stale_leader_term."""
    a = _adapter(rendered)
    _session(a, "1.4.0")
    assert _answer(a, "t1", 1, term=2).assign_action.accepted
    _session(a, "1.3.0")
    res = _answer(a, "t2", 2, term=0)
    assert not res.stale_leader_term, "a 1.3.0 connection's Command carries no term"
    assert res.HasField("assign_action"), "it is answered in its own arm"


@pytest.mark.parametrize("negotiated", ["1.4.0", "", "not-a-version", "2.0.0"])
def test_the_leader_term_is_checked_unless_the_connection_negotiated_below_1_4_0(
        rendered, negotiated):
    """The guard beside the row above: at 1.4.0, and while the version cannot be read as one
    below 1.4.0, a lower term is still rejected. A stale assignment is not safe (Note 2)."""
    a = _adapter(rendered)
    _session(a, "1.4.0")
    assert _answer(a, "t1", 1, term=2).assign_action.accepted
    _session(a, negotiated)
    assert _answer(a, "t2", 2, term=1).stale_leader_term
