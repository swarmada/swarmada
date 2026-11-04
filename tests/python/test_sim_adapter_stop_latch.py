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

"""The simulation adapter keeps the stop latch with --latch, and claims 1.4.0 only then.

RFC-0001 #fleet-adapter-protocol-the-stop-latch binds a connection at contract 1.4.0. An
adapter that reports 1.4.0 and keeps no latch is not a 1.4.0 adapter, so the simulation
adapter reports 1.4.0 with --latch and 1.3.0 without it. Its deployments run it without,
because the reference control plane ends no local set at v0.4: a latching adapter there that
took an edge stop would take no work until it restarts.
"""

import json

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

ROBOT = "amr-1"


def _adapter(tmp_path, *, latch: bool = True) -> SimAdapter:
    a = SimAdapter(pb, None, make_simulator("kinematic"), ROBOT, "z1", "sim",
                   artifact_dir=str(tmp_path), latch=latch)
    a._sim.spawn(ROBOT, 0.0, 0.0)
    return a


def _drain(q) -> list:
    out = []
    while not q.empty():
        item = q.get_nowait()
        if item is not None:
            out.append(item)
    return out


def _hello(a):
    return next(a._control_requests()).hello


def _assign(a, action_id: str, token: int, *, term: int = 1, command_id: int | None = None):
    a._handle_command(pb.Command(
        command_id=command_id or token, robot_id=ROBOT, leader_term=term,
        assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", action_type="Navigate",
            fencing_token=token, payload_json=json.dumps({"x": 5.0, "y": 0.0}).encode())))
    (res,) = [m.command_result.assign_action for m in _drain(a._outbox)
              if m.WhichOneof("payload") == "command_result"]
    return res


def _fence(a, estop_id: str, *, term: int = 1):
    a._handle_command(pb.Command(command_id=900, robot_id=ROBOT, leader_term=term,
                                 estop=pb.Estop(estop_id=estop_id, issued_by="test")))


def _clear(a, estop_id: str, *, term: int = 1) -> list:
    a._on_estop_clear(pb.EstopClear(estop_id=estop_id, leader_term=term), a._safety_outbox)
    return [m.estop_clear_ack.estop_id for m in _drain(a._safety_outbox)
            if m.WhichOneof("payload") == "estop_clear_ack"]


def test_without_the_latch_it_reports_1_3_0_and_takes_work_after_a_stop(tmp_path):
    a = _adapter(tmp_path, latch=False)
    assert _hello(a).contract_version == "1.3.0"
    _fence(a, "e1")
    assert [m for m in _drain(a._outbox) if m.WhichOneof("payload") == "command_result"
            and m.command_result.unsupported], "a 1.3.0 adapter may ignore the fence"
    assert _assign(a, "t1", 1).accepted


def test_with_the_latch_it_reports_1_4_0(tmp_path):
    assert _hello(_adapter(tmp_path)).contract_version == "1.4.0"


def test_a_fence_latches_and_work_is_refused_until_the_clear_lifts_it(tmp_path):
    a = _adapter(tmp_path)
    _fence(a, "e1")
    acks = [m.estop_ack for m in _drain(a._safety_outbox) if m.WhichOneof("payload") == "estop_ack"]
    assert [x.estop_id for x in acks] == ["e1"], "the fence is answered on SafetyStream"
    receipts = [m.command_result for m in _drain(a._outbox)
                if m.WhichOneof("payload") == "command_result"]
    assert receipts and receipts[0].HasField("estop"), "the permitted receipt"

    held = _assign(a, "t1", 1)
    assert not held.accepted
    assert held.rejection == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE
    reports = _drain(a._safety_outbox)
    assert any(m.WhichOneof("payload") == "estop_ack" and m.estop_ack.estop_id == "e1"
               for m in reports), "each refusal reports the latched IDs"
    assert any(m.WhichOneof("payload") == "local_stop" for m in reports)

    assert _clear(a, "e1") == ["e1"]
    assert _assign(a, "t2", 2).accepted
    assert _clear(a, "e1") == ["e1"], "a clear for an ID already lifted is acknowledged again"


def test_a_clear_that_arrives_before_its_fence_takes_effect_with_the_fence(tmp_path):
    a = _adapter(tmp_path)
    a._latch_stop("e1")  # the Estop on SafetyStream
    assert _clear(a, "e1") == [], "no fence yet: the clear is kept, nothing is lifted"
    assert not _assign(a, "t1", 1).accepted
    _fence(a, "e1")
    lifted = [m.estop_clear_ack.estop_id for m in _drain(a._safety_outbox)
              if m.WhichOneof("payload") == "estop_clear_ack"]
    assert lifted == ["e1"]
    _drain(a._outbox)
    assert _assign(a, "t2", 2).accepted


def test_a_stale_leader_term_is_rejected_and_a_stale_clear_lifts_nothing(tmp_path):
    a = _adapter(tmp_path)
    _fence(a, "e1", term=5)
    _drain(a._outbox)
    assert _clear(a, "e1", term=4) == []
    a._handle_command(pb.Command(command_id=7, robot_id=ROBOT, leader_term=4,
                                 assign_action=pb.AssignAction(action_id="t1", fencing_token=1)))
    (res,) = [m.command_result for m in _drain(a._outbox)
              if m.WhichOneof("payload") == "command_result"]
    assert res.stale_leader_term
    _fence(a, "e2", term=1)  # a stop is accepted whatever its term
    assert "e2" in a._latched


def test_interrupted_work_ends_failed_and_a_re_delivery_is_accepted_with_no_motion(tmp_path):
    a = _adapter(tmp_path)
    assert _assign(a, "t1", 3).accepted
    _drain(a._outbox)
    _fence(a, "e1")
    ended = [m.action_status for m in _drain(a._outbox)
             if m.WhichOneof("payload") == "action_status"
             and m.action_status.state == pb.ACTION_STATE_FAILED]
    assert ended and ended[0].action_id == "t1"
    assert ended[0].end_cause == pb.ACTION_END_CAUSE_ESTOP
    again = _assign(a, "t1", 3)
    assert again.accepted, "a re-delivery of interrupted work is not refused"
    assert a._current_action == "", "and the robot does not take it up again"


def test_a_fence_for_a_cleared_id_does_not_latch_again(tmp_path):
    a = _adapter(tmp_path)
    _fence(a, "e1")
    assert _clear(a, "e1") == ["e1"]
    _fence(a, "e1")
    _drain(a._outbox)
    assert _assign(a, "t1", 1).accepted


def _session(a, negotiated: str) -> None:
    """A ControlStream whose HelloAck negotiated `negotiated` (the telemetry loop not started)."""
    a._telemetry_started = True
    a._handle_control_plane(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
        accepted=True, negotiated_contract_version=negotiated)), None)
    _drain(a._outbox)


def _answer(a, action_id: str, token: int, *, term: int):
    a._handle_command(pb.Command(
        command_id=token, robot_id=ROBOT, leader_term=term,
        assign_action=pb.AssignAction(
            action_id=action_id, assignment_id=f"a-{action_id}", action_type="Navigate",
            fencing_token=token, payload_json=json.dumps({"x": 5.0, "y": 0.0}).encode())))
    (res,) = [m.command_result for m in _drain(a._outbox)
              if m.WhichOneof("payload") == "command_result"]
    return res


def test_a_connection_below_1_4_0_is_not_rejected_for_its_leader_term(tmp_path):
    """The leader-term rule binds a connection that negotiated 1.4.0 (RFC-0001
    #fleet-adapter-protocol-the-stop-latch, *Version*). A control plane below 1.4.0 sends no
    term (0), and "a connection below 1.4.0 is not rejected" (Note 2). The highest term is kept
    across reconnects, so after a 1.4.0 session at term 2, a later session that negotiated
    1.3.0 (a control plane rolled back) had every Command rejected stale_leader_term."""
    a = _adapter(tmp_path)
    _session(a, "1.4.0")
    assert _answer(a, "t1", 1, term=2).assign_action.accepted
    _session(a, "1.3.0")
    res = _answer(a, "t2", 2, term=0)
    assert not res.stale_leader_term, "a 1.3.0 connection's Command carries no term"
    assert res.HasField("assign_action"), "it is answered in its own arm"


@pytest.mark.parametrize("negotiated", ["1.4.0", "", "not-a-version", "2.0.0"])
def test_the_leader_term_is_checked_unless_the_connection_negotiated_below_1_4_0(
        tmp_path, negotiated):
    """The guard beside the row above: at 1.4.0, and while the version cannot be read as one
    below 1.4.0, a lower term is still rejected. A stale assignment is not safe (Note 2)."""
    a = _adapter(tmp_path)
    _session(a, "1.4.0")
    assert _answer(a, "t1", 1, term=2).assign_action.accepted
    _session(a, negotiated)
    assert _answer(a, "t2", 2, term=1).stale_leader_term
