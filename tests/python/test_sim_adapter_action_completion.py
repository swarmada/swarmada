# Copyright 2026 The Swarmada Authors. All rights reserved.

"""The simulation adapter completes an action when the robot arrives, and only then.

The control plane moves a FleetAction to Succeeded only on an ActionStatusUpdate with
ACTION_STATE_SUCCEEDED. An adapter that never sends one leaves every task InProgress until its
lease lapses, so no task can finish. These tests hold the adapter to reporting arrival, with the
action's own fencing token, and to reporting nothing when the robot is stopped short of the
target: every mid-action path (estop, cancel, pause, zone hold) must still act before arrival.
"""

from __future__ import annotations

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from swarmada_sdk.safety import confirm_estop  # noqa: E402

from adapters.simulation.sim_adapter import SimAdapter  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402


def _adapter() -> SimAdapter:
    a = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z1", "sim")
    a._sim.spawn("amr-1", 0.0, 0.0)
    return a


def _assign(a: SimAdapter, action_id: str = "t1", token: int = 7, cid: int = 1) -> None:
    a._handle_command(pb.Command(
        command_id=cid, robot_id="amr-1",
        assign_action=pb.AssignAction(action_id=action_id, action_type="Navigate",
                                      fencing_token=token, lease_generation=1,
                                      lease_duration_ms=600_000)))


def _run(a: SimAdapter, seconds: float, dt: float = 0.2) -> None:
    """What the telemetry loop does each cycle: advance the sim, then check for arrival."""
    for _ in range(int(seconds / dt)):
        with a._sim_lock:
            a._sim.tick(dt)
        a._complete_on_arrival()


def _statuses(a: SimAdapter) -> list:
    out = []
    while not a._outbox.empty():
        m = a._outbox.get_nowait()
        if m is not None and m.WhichOneof("payload") == "action_status":
            out.append(m.action_status)
    return out


def _succeeded(a: SimAdapter) -> list:
    return [s for s in _statuses(a) if s.state == pb.ACTION_STATE_SUCCEEDED]


def test_arrival_reports_succeeded_with_the_actions_token() -> None:
    a = _adapter()
    _assign(a, token=7)
    _run(a, 15.0)  # 10 m at 1 m/s
    done = _succeeded(a)
    assert [s.action_id for s in done] == ["t1"], "arrival must report SUCCEEDED exactly once"
    assert done[0].fencing_token == 7, "SUCCEEDED must echo the token the action was accepted under"
    assert a._current_action == "", "a completed action is no longer the robot's current action"


def test_no_success_before_arrival() -> None:
    a = _adapter()
    _assign(a)
    _run(a, 5.0)  # half way
    assert not _succeeded(a)
    assert a._current_action == "t1"


def test_estop_before_arrival_never_completes() -> None:
    a = _adapter()
    _assign(a)
    _run(a, 4.0)
    with a._sim_lock:
        confirm_estop(a._sim, "amr-1")
    _run(a, 15.0)
    assert not _succeeded(a), "a robot estopped short of its target has not arrived"


def test_cancel_before_arrival_never_completes() -> None:
    a = _adapter()
    _assign(a)
    _run(a, 4.0)
    a._handle_command(pb.Command(command_id=2, robot_id="amr-1",
                                 cancel_action=pb.CancelAction(action_id="t1")))
    _run(a, 15.0)
    states = [s.state for s in _statuses(a)]
    assert pb.ACTION_STATE_CANCELLED in states
    assert pb.ACTION_STATE_SUCCEEDED not in states


def test_pause_before_arrival_never_completes() -> None:
    a = _adapter()
    _assign(a)
    _run(a, 4.0)
    a._handle_command(pb.Command(command_id=2, robot_id="amr-1",
                                 pause=pb.PauseCapabilities(require_stop_before_ack=False)))
    _run(a, 15.0)
    assert not _succeeded(a)
    assert a._current_action == "t1", "a paused action is still the robot's action"


def test_zone_hold_then_admit_completes_at_the_assigned_target() -> None:
    a = _adapter()
    a._handle_command(pb.Command(command_id=2, robot_id="amr-1",
                                 zone_admission=pb.ZoneAdmission(zone_name="z", admit=False)))
    _assign(a)
    _run(a, 15.0)
    assert not _succeeded(a), "held at the boundary: the robot has not moved, so not arrived"
    a._handle_command(pb.Command(command_id=3, robot_id="amr-1",
                                 zone_admission=pb.ZoneAdmission(zone_name="z", admit=True)))
    _run(a, 15.0)
    assert [s.action_id for s in _succeeded(a)] == ["t1"]
