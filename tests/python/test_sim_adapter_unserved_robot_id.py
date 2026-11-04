# Copyright 2026 The Swarmada Authors. All rights reserved.

"""The simulation adapter acts only on commands addressed to the robot it serves.

One ControlStream carries every command for every Robot bound to an adapter, and each
`Command` names its robot in `robot_id` ("robot_id selects the managed robot", RFC-0001
#fleet-adapter-protocol-the-controlstream-service). This adapter serves exactly one robot. A
command addressed to any other id is a command for a robot it does not serve, so carrying it
out moves the wrong robot: the reference failure is an assignment for a second Robot object,
bound to the same adapter, that drove this adapter's robot out of a stopped zone.

So every arm that acts on the robot is refused when the id is not its own, with the refusal
the contract already defines for that arm, and nothing reaches the robot:

* ``assign_action`` — declined ``ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE``; the robot does
  not move, no action starts, and the fencing high-water mark is not raised.
* ``cancel_action`` — not acknowledged; the action that is running keeps running.
* ``pause`` / ``resume`` / ``renew_lease`` / ``zone_admission`` — answered negative; the
  robot's motion, lease and hold are untouched.
* an ``estop`` on the SafetyStream — answered ``ESTOP_STATE_FAILED`` for the id it named
  (this adapter cannot confirm that robot stopped, and a STOPPED for it would be a false
  confirmation), and this adapter's robot is not stopped by it.

Every refusal echoes the ``robot_id`` it was addressed to, so the control plane correlates it
with the command it sent.
"""

from __future__ import annotations

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
# Shaped like the reference failure's second Robot object: one suffix away from the served id.
OTHER = "amr-1-c"


def _adapter() -> SimAdapter:
    a = SimAdapter(pb, None, make_simulator("kinematic"), ROBOT, "z1", "sim")
    a._sim.spawn(ROBOT, 0.0, 0.0)
    return a


def _results(a: SimAdapter) -> list:
    out = []
    while not a._outbox.empty():
        m = a._outbox.get_nowait()
        if m.WhichOneof("payload") == "command_result":
            out.append(m.command_result)
    return out


def _statuses(a: SimAdapter) -> list:
    out = []
    while not a._outbox.empty():
        m = a._outbox.get_nowait()
        if m.WhichOneof("payload") == "action_status":
            out.append(m.action_status)
    return out


def _tick(a: SimAdapter, dt: float = 0.2, n: int = 5) -> None:
    for _ in range(n):
        a._sim.tick(dt)


def _moved(a: SimAdapter) -> float:
    p = a._sim.pose(ROBOT)
    return (p.x ** 2 + p.y ** 2) ** 0.5


def _send(a: SimAdapter, robot_id: str, cid: int, **arm):
    a._handle_command(pb.Command(command_id=cid, robot_id=robot_id, **arm))
    res = _results(a)
    assert len(res) == 1, f"expected one CommandResult, got {len(res)}"
    return res[0]


def _nav(action_id: str, token: int) -> pb.AssignAction:
    return pb.AssignAction(action_id=action_id, assignment_id=f"a-{action_id}",
                           action_type="Navigate", fencing_token=token, lease_generation=1,
                           lease_duration_ms=60_000,
                           payload_json=json.dumps({"x": 4.0, "y": 3.0}).encode())


def _running(a: SimAdapter) -> None:
    """The served robot under an accepted assignment, and moving."""
    res = _send(a, ROBOT, 1, assign_action=_nav("t-mine", 1))
    assert res.assign_action.accepted
    _tick(a)
    assert not a._sim.is_stopped(ROBOT), "the served assignment should have the robot moving"


# ── assign_action: the reference failure ─────────────────────────────────────


def test_assign_for_an_unserved_robot_is_declined_and_the_stopped_robot_stays_put() -> None:
    a = _adapter()
    assert a._sim.is_stopped(ROBOT)
    res = _send(a, OTHER, 7, assign_action=_nav("t-other", 1))
    _tick(a)
    assert not res.assign_action.accepted, (
        f"an assignment addressed to {OTHER!r} was ACCEPTED by the adapter serving {ROBOT!r}")
    assert res.assign_action.rejection == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE
    assert OTHER in res.assign_action.message and ROBOT in res.assign_action.message
    assert res.robot_id == OTHER, "the refusal echoes the id the command was addressed to"
    assert a._sim.is_stopped(ROBOT) and _moved(a) < 1e-6, (
        f"the robot moved {_moved(a):.3f} m on a command for another robot")
    assert a._current_action == "", "no action may start on a command for another robot"


def test_a_declined_unserved_assign_does_not_raise_the_fence() -> None:
    """The refusal comes before the fence: the same token, to the served robot, is accepted."""
    a = _adapter()
    _send(a, OTHER, 1, assign_action=_nav("t-other", 5))
    res = _send(a, ROBOT, 2, assign_action=_nav("t-mine", 5))
    assert res.assign_action.accepted, res.assign_action


def test_the_served_robot_still_takes_its_own_assignment() -> None:
    a = _adapter()
    res = _send(a, ROBOT, 1, assign_action=_nav("t-mine", 1))
    _tick(a)
    assert res.assign_action.accepted and not a._sim.is_stopped(ROBOT)


# ── cancel / pause / resume / renew_lease / zone_admission ───────────────────


def test_cancel_for_an_unserved_robot_is_not_acknowledged_and_the_action_runs_on() -> None:
    a = _adapter()
    _running(a)
    res = _send(a, OTHER, 2, cancel_action=pb.CancelAction(action_id="t-mine", fencing_token=1))
    _tick(a)
    assert not res.cancel_action.acknowledged, (
        f"a cancel addressed to {OTHER!r} was acknowledged by the adapter serving {ROBOT!r}")
    assert not res.cancel_action.stale, "the token was current; stale would name the wrong cause"
    assert res.robot_id == OTHER
    assert a._current_action == "t-mine" and not a._sim.is_stopped(ROBOT), (
        "a cancel for another robot stopped this one")
    assert not [s for s in _statuses(a) if s.action_id == "t-mine"], (
        "a cancel for another robot reported this robot's action terminal")


def test_cancel_with_no_action_named_for_an_unserved_robot_stops_nothing() -> None:
    a = _adapter()
    _running(a)
    res = _send(a, OTHER, 2, cancel_action=pb.CancelAction(fencing_token=1))
    _tick(a)
    assert not res.cancel_action.acknowledged
    assert a._current_action == "t-mine" and not a._sim.is_stopped(ROBOT)


def test_pause_for_an_unserved_robot_is_refused_and_the_robot_keeps_moving() -> None:
    a = _adapter()
    _running(a)
    res = _send(a, OTHER, 2, pause=pb.PauseCapabilities(require_stop_before_ack=True))
    _tick(a)
    assert not res.pause.paused
    assert OTHER in res.pause.message
    assert not a._sim.is_stopped(ROBOT), "a pause for another robot stopped this one"


def test_resume_for_an_unserved_robot_is_refused() -> None:
    a = _adapter()
    res = _send(a, OTHER, 2, resume=pb.ResumeCapabilities())
    assert not res.resume.resumed
    assert OTHER in res.resume.message


def test_renew_lease_for_an_unserved_robot_is_refused_and_renews_nothing() -> None:
    a = _adapter()
    _running(a)
    res = _send(a, OTHER, 2, renew_lease=pb.RenewActionLease(
        action_id="t-mine", lease_generation=1, lease_duration_ms=60_000))
    assert not res.renew_lease.renewed and not res.renew_lease.running


def test_zone_admission_for_an_unserved_robot_does_not_release_a_hold() -> None:
    a = _adapter()
    _send(a, ROBOT, 1, zone_admission=pb.ZoneAdmission(zone_name="zone-full", admit=False))
    _send(a, ROBOT, 2, assign_action=_nav("t-mine", 1))
    _tick(a)
    assert a._sim.is_stopped(ROBOT), "held at the boundary"
    res = _send(a, OTHER, 3, zone_admission=pb.ZoneAdmission(zone_name="zone-full", admit=True))
    _tick(a)
    assert not res.zone_admission.acknowledged
    assert a._zone_hold == "zone-full" and a._sim.is_stopped(ROBOT), (
        "an admit for another robot released this robot's hold")


# ── estop on the SafetyStream ────────────────────────────────────────────────


class _OneEstopStub:
    """A SafetyStream that delivers one estop and then closes cleanly."""

    def __init__(self, robot_id: str) -> None:
        self._cp = pb.ControlPlaneSafetyMessage(
            robot_id=robot_id, estop=pb.Estop(estop_id="e-1", reason="test"))

    def SafetyStream(self, _requests):  # noqa: N802 -- the gRPC method name
        return iter([self._cp])


def _acks(a: SimAdapter) -> list:
    out = []
    while not a._safety_outbox.empty():
        m = a._safety_outbox.get_nowait()
        if m is not None and m.WhichOneof("payload") == "estop_ack":
            out.append(m)
    return out


def test_estop_for_an_unserved_robot_is_never_confirmed_stopped_and_stops_nothing() -> None:
    a = _adapter()
    _running(a)
    a.run_safety_stream(_OneEstopStub(OTHER))
    _tick(a)
    acks = _acks(a)
    assert len(acks) == 1
    assert acks[0].robot_id == OTHER
    assert acks[0].estop_ack.state == pb.ESTOP_STATE_FAILED, (
        f"an estop for {OTHER!r} was answered "
        f"{pb.EstopState.Name(acks[0].estop_ack.state)}: this adapter cannot confirm a robot it "
        f"does not serve is at rest")
    assert not acks[0].estop_ack.HasField("stop_initiated_at"), "no stop was commanded"
    assert OTHER in acks[0].estop_ack.message
    assert not a._sim.is_stopped(ROBOT), "an estop for another robot stopped this one"


def test_estop_for_the_served_robot_is_still_confirmed() -> None:
    a = _adapter()
    _running(a)
    a.run_safety_stream(_OneEstopStub(ROBOT))
    acks = _acks(a)
    assert len(acks) == 1 and acks[0].robot_id == ROBOT
    assert acks[0].estop_ack.state in (pb.ESTOP_STATE_STOPPED, pb.ESTOP_STATE_FAILED)
    assert acks[0].estop_ack.HasField("stop_initiated_at"), "the stop was commanded"
