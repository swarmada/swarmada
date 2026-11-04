"""The reference simulation adapter serves what its catalog declares, and nothing else.

It advertised `_SUPPORTED_ACTIONS` and judged `validate_action` against it, while its
`assign_action` never read `action_type`: a `Clean`, an `External`, an empty type or any
string was accepted and the robot drove to a fixed point. RFC-0001: an adapter MUST NOT treat
an action type it does not recognise as a nearest-neighbour verb; it rejects the assignment.
Both paths now judge an action against the one table the catalog is built from.
"""

from __future__ import annotations

import json

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation.sim_adapter import _SUPPORTED_ACTIONS, SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402


def _adapter() -> SimAdapter:
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim")
    a._sim.spawn("amr-1", 0.0, 0.0)
    return a


def _result(a: SimAdapter):
    while not a._outbox.empty():
        m = a._outbox.get_nowait()
        if m.WhichOneof("payload") == "command_result":
            return m.command_result
    raise AssertionError("no CommandResult")


def _assign(a, action_type, payload=None, token=1):
    a._handle_command(pb.Command(command_id=token, robot_id="amr-1", assign_action=pb.AssignAction(
        action_id=f"t{token}", assignment_id=f"a{token}", action_type=action_type,
        fencing_token=token,
        payload_json=json.dumps(payload).encode() if payload is not None else b"")))
    return _result(a).assign_action


def _validate(a, action_type, payload=None):
    a._handle_command(pb.Command(command_id=99, robot_id="amr-1", validate_action=pb.ValidateAction(
        action_type=action_type,
        payload_json=json.dumps(payload).encode() if payload is not None else b"")))
    return _result(a).validate_action


@pytest.mark.parametrize("action_type", ["Clean", "External", "", "navigate", "Teleport"])
def test_an_undeclared_type_is_refused_invalid_action_and_moves_nothing(action_type):
    a = _adapter()
    res = _assign(a, action_type, {"x": 3.0, "y": 0.0})
    assert not res.accepted
    assert res.rejection == pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION
    a._sim.tick(0.2)
    assert a._sim.is_stopped("amr-1"), "a refused assignment must not move the robot"


@pytest.mark.parametrize("entry", _SUPPORTED_ACTIONS, ids=[e[0] for e in _SUPPORTED_ACTIONS])
def test_validate_and_assign_agree_for_every_declared_type(entry):
    a = _adapter()
    assert _validate(a, entry[0], {"x": 1.0, "y": 1.0}).servable
    assert _assign(a, entry[0], {"x": 1.0, "y": 1.0}).accepted


def test_validate_and_assign_agree_for_an_undeclared_type():
    a = _adapter()
    assert not _validate(a, "Clean").servable
    assert not _assign(a, "Clean").accepted


def test_the_target_is_read_from_the_payload():
    a = _adapter()
    assert _assign(a, "Navigate", {"x": 0.0, "y": -4.0}).accepted
    for _ in range(3):
        a._sim.tick(0.2)
    pose = a._sim.pose("amr-1")
    assert pose.y < 0 and abs(pose.x) < 1e-6, "the robot drives toward the payload's target"


def test_a_half_target_is_refused():
    a = _adapter()
    res = _assign(a, "Navigate", {"x": 2.0})
    assert not res.accepted and res.rejection == pb.ASSIGN_ACTION_REJECTION_INVALID_ACTION


def test_the_catalog_declares_the_target_parameters_it_reads():
    a = _adapter()
    nav = next(s for s in a._supported_actions() if s.action_type == "Navigate")
    assert {(p.name, p.required) for p in nav.params} == {("x", False), ("y", False)}
