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

"""Several simulated robots on one ControlStream and one SafetyStream (SimFleet).

One stream multiplexes every robot an adapter manages, each message naming its robot
(RFC-0001 #fleet-adapter-protocol-the-controlstream-service). These tests hold the fleet to
what that requires of an adapter, robot by robot:

* a command, a stop and a clear reach the robot they name, and no other; one naming a robot
  the adapter does not serve reaches none, and a stop for it is never confirmed;
* a stop for one robot stops that robot and leaves the others moving, and a fleet-wide stop
  is commanded for every robot before any one robot's rest is awaited;
* fencing tokens, leases, fences and the stop latch are per robot;
* a RegisterAck, which names no robot, is paired with the register it answers, in order;
* one tick moves the one world, so N robots do not move N times faster;
* an edge-issued stop stops every robot served through that edge node, with one ack.

The single-robot adapter is pinned separately, byte for byte
(test_sim_adapter_golden_transcript.py).
"""

from __future__ import annotations

import queue
import threading
import time

import pytest

pytest.importorskip("grpc")

try:
    from proto.fleet_adapter.v1 import fleet_adapter_pb2 as pb
except ImportError:  # proto/ already on PYTHONPATH (conformance layout)
    from fleet_adapter.v1 import fleet_adapter_pb2 as pb  # type: ignore

from adapters.simulation import sim_adapter  # noqa: E402
from adapters.simulation.sim_adapter import SimAdapter, SimFleet  # noqa: E402
from adapters.simulation.simulator import make_simulator  # noqa: E402

GHOST = "robot-not-served"


# ── helpers ───────────────────────────────────────────────────────────────────

def _ids(n: int) -> list[str]:
    return [f"amr-{i}" for i in range(1, n + 1)]


@pytest.fixture
def make_fleet():
    made: list[SimFleet] = []

    def make(n: int = 3, latch: bool = False, contract: str = "1.4.0") -> SimFleet:
        fleet = SimFleet(pb, None, make_simulator("kinematic"), _ids(n), "z-1", "simulation",
                         None, latch=latch)
        with fleet.sim_lock:
            for rid in _ids(n):
                fleet._sim.spawn(rid, 0.0, 0.0)
        for rid in _ids(n):
            fleet.robot(rid)._schedule = lambda _delay, _fn: None
        made.append(fleet)
        fleet._handle_control_plane(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
            accepted=True, negotiated_contract_version=contract)))   # starts the tick thread
        return fleet

    yield make
    for fleet in made:
        fleet.shutdown()


def _admit_all(fleet: SimFleet, **ack) -> None:
    for _ in fleet._ids:
        fleet._handle_control_plane(pb.ControlPlaneMessage(
            register_ack=pb.RegisterAck(accepted=True, **ack)))


def _command(fleet: SimFleet, cid: int, robot: str, term: int = 1, **arm) -> None:
    fleet._handle_control_plane(pb.ControlPlaneMessage(command=pb.Command(
        command_id=cid, robot_id=robot, leader_term=term, **arm)))


def _assign(action_id: str, token: int, x: float = 50.0, lease_ms: int = 60000):
    return pb.AssignAction(action_id=action_id, action_type="Navigate",
                           payload_json=f'{{"x": {x}, "y": 0.0}}'.encode(),
                           fencing_token=token, lease_duration_ms=lease_ms, lease_generation=1)


def _safety(fleet: SimFleet, robot: str, **arm) -> None:
    fleet._on_safety(pb.ControlPlaneSafetyMessage(robot_id=robot, **arm),
                     fleet.link.safety_outbox)


def _take(q: queue.Queue, pred, timeout: float = 3.0):
    """The first message on `q` matching `pred`; the ones before it are discarded."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            msg = q.get(timeout=0.05)
        except queue.Empty:
            continue
        if msg is not None and pred(msg):
            return msg
    raise AssertionError("no matching message within the timeout")


def _result(fleet: SimFleet, cid: int):
    return _take(fleet.link.outbox, lambda m: m.WhichOneof("payload") == "command_result"
                 and m.command_result.command_id == cid).command_result


def _estop_ack(fleet: SimFleet, estop_id: str, timeout: float = 3.0):
    """The stop's terminal EstopAck, STOPPED or FAILED: the STOPPING before it is passed over."""
    return _take(fleet.link.safety_outbox,
                 lambda m: m.WhichOneof("payload") == "estop_ack"
                 and m.estop_ack.estop_id == estop_id
                 and m.estop_ack.state != pb.ESTOP_STATE_STOPPING, timeout)


def _edge_stop(fleet: SimFleet, address: str, estop_id: str = "edge-stop"):
    """An edge stop through `address`, and its terminal EstopAck on the node's stream."""
    outbox: queue.Queue = queue.Queue()
    fleet._edge_estop(address, pb.Estop(estop_id=estop_id), outbox)
    return _take(outbox, lambda m: m.WhichOneof("msg") == "estop_ack"
                 and m.estop_ack.state != pb.ESTOP_STATE_STOPPING, 4.0)


def _x(fleet: SimFleet, rid: str) -> float:
    with fleet.sim_lock:
        return fleet._sim.pose(rid).x


def _moving(fleet: SimFleet, rid: str) -> bool:
    with fleet.sim_lock:
        return not fleet._sim.is_stopped(rid)


def _drain(q: queue.Queue) -> list:
    out = []
    while True:
        try:
            msg = q.get_nowait()
        except queue.Empty:
            return out
        if msg is not None:
            out.append(msg)


# ── construction and the command line ─────────────────────────────────────────

def test_a_fleet_needs_two_distinct_robots() -> None:
    sim = make_simulator("kinematic")
    with pytest.raises(ValueError):
        SimFleet(pb, None, sim, ["amr-1"], "z", "sim")
    with pytest.raises(ValueError):
        SimFleet(pb, None, sim, ["amr-1", "amr-1"], "z", "sim")
    with pytest.raises(ValueError):
        SimFleet(pb, None, sim, ["amr-1", ""], "z", "sim")


def test_the_command_line_refuses_a_repeated_robot_id(monkeypatch, capsys) -> None:
    monkeypatch.setattr("sys.argv", ["sim_adapter.py", "--robot-id", "a", "--robot-id", "b",
                                     "--robot-id", "a"])
    with pytest.raises(SystemExit) as exc:
        sim_adapter.main()
    assert exc.value.code == 2
    assert "--robot-id given more than once for a" in capsys.readouterr().err


def test_every_robot_shares_the_fleets_streams_lock_and_shutdown(make_fleet) -> None:
    fleet = make_fleet()
    robots = [fleet.robot(r) for r in fleet._ids]
    assert all(r._outbox is fleet.link.outbox for r in robots)
    assert all(r._safety_outbox is fleet.link.safety_outbox for r in robots)
    assert all(r._sim_lock is fleet.sim_lock for r in robots)
    assert all(r._shutdown is fleet.shutdown_event for r in robots)
    fresh = queue.Queue()
    fleet.link.outbox = fresh
    assert all(r._outbox is fresh for r in robots)  # one swap reaches every robot


# ── registration ─────────────────────────────────────────────────────────────

def test_hello_registers_every_robot_in_order_each_with_its_snapshot(make_fleet) -> None:
    fleet = make_fleet()
    kinds = [(m.WhichOneof("payload"), m.register.robot_id or m.capabilities.robot_id)
             for m in _drain(fleet.link.outbox) if m.WhichOneof("payload") != "telemetry"]
    want = []
    for rid in fleet._ids:
        want += [("register", rid), ("capabilities", rid)]
    assert kinds == want


def test_register_acks_are_paired_in_order(make_fleet) -> None:
    fleet = make_fleet(3)
    a, b, c = fleet._ids
    scheduled = []
    fleet.robot(b)._schedule = lambda delay, fn: scheduled.append(fn)
    _drain(fleet.link.outbox)
    fleet._handle_control_plane(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
        accepted=True, telemetry_interval_seconds=7)))
    fleet._handle_control_plane(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
        accepted=False, rejection=pb.REGISTRATION_REJECTION_NOT_ADMITTED)))
    fleet._handle_control_plane(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
        accepted=True, authoritative_action_state=pb.RobotActionState(
            phase=pb.ROBOT_ACTION_PHASE_IDLE, fencing_token=40))))
    assert fleet.robot(a)._admitted and fleet.robot(a)._telemetry_interval == 7.0
    assert not fleet.robot(b)._admitted
    assert fleet.robot(c)._admitted and fleet.robot(c)._fence.highest(c) == 40
    assert fleet.robot(a)._fence.highest(a) == 0, "another robot's token was adopted"
    discovers = [m.discover.robot_id for m in _drain(fleet.link.outbox)
                 if m.WhichOneof("payload") == "discover"]
    assert discovers == [b]
    # The provisioning loop's next register for b is paired with the next ack.
    assert len(scheduled) == 1
    scheduled[0]()
    assert list(fleet._unanswered) == [fleet.robot(b)]
    fleet._handle_control_plane(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
        accepted=True)))
    assert fleet.robot(b)._admitted


def test_an_unpaired_register_ack_is_applied_to_no_robot(make_fleet, capsys) -> None:
    fleet = make_fleet(2)
    _admit_all(fleet)
    before = [fleet.robot(r)._fence.highest(r) for r in fleet._ids]
    fleet._handle_control_plane(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
        accepted=True, authoritative_action_state=pb.RobotActionState(
            phase=pb.ROBOT_ACTION_PHASE_IDLE, fencing_token=99))))
    assert [fleet.robot(r)._fence.highest(r) for r in fleet._ids] == before
    assert "REGISTER_ACK_UNPAIRED" in capsys.readouterr().err


def test_a_register_for_a_replaced_stream_is_not_sent(make_fleet) -> None:
    fleet = make_fleet(2)
    _admit_all(fleet)
    old = fleet.link.outbox
    fleet.link.outbox = queue.Queue()
    fleet.robot(fleet._ids[0])._admitted = False
    fleet.robot(fleet._ids[0])._provisioning_iteration(old)
    assert not fleet._unanswered
    assert not [m for m in _drain(fleet.link.outbox) if m.WhichOneof("payload") == "register"]


# ── routing ───────────────────────────────────────────────────────────────────

def test_an_assignment_moves_only_the_robot_it_names(make_fleet) -> None:
    fleet = make_fleet(3)
    a, b, c = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, b, assign_action=_assign("act-b", 5))
    assert _result(fleet, 1).robot_id == b
    time.sleep(0.6)
    assert _x(fleet, b) > 0.2
    assert _x(fleet, a) == 0.0 and _x(fleet, c) == 0.0
    assert fleet.robot(b)._current_action == "act-b"
    assert fleet.robot(a)._current_action == "" and fleet.robot(c)._current_action == ""


def test_a_cancel_for_one_robot_leaves_another_robots_action_running(make_fleet) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, b, assign_action=_assign("act-b", 5))
    _command(fleet, 2, a, cancel_action=pb.CancelAction(action_id="act-b", fencing_token=5))
    _result(fleet, 2)
    time.sleep(0.5)
    assert fleet.robot(b)._current_action == "act-b"
    assert _moving(fleet, b)


def test_telemetry_is_per_robot(make_fleet) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    time.sleep(0.8)
    frames = {}
    for m in _drain(fleet.link.outbox):
        if m.WhichOneof("payload") == "telemetry":
            frames[m.telemetry.robot_id] = m.telemetry
    assert set(frames) == {a, b}
    assert frames[a].current_action == "act-a" and frames[a].position.x > 0.0
    assert frames[b].current_action == "" and frames[b].position.x == 0.0


def test_one_tick_moves_the_world_once(make_fleet) -> None:
    fleet = make_fleet(4)
    _admit_all(fleet)
    for n, rid in enumerate(fleet._ids, start=1):
        _command(fleet, n, rid, assign_action=_assign(f"act-{n}", 5))
    began = time.monotonic()
    time.sleep(1.0)
    elapsed = time.monotonic() - began
    # KinematicSim.SPEED is 1 m/s. Four robots each ticking the world would cover ~4 m.
    for rid in fleet._ids:
        assert _x(fleet, rid) <= elapsed * 1.0 + 0.25


def test_a_heartbeat_is_answered_once_whatever_it_names(make_fleet) -> None:
    fleet = make_fleet(3)
    _drain(fleet.link.outbox)
    fleet._handle_control_plane(pb.ControlPlaneMessage(heartbeat=pb.HeartbeatRequest()))
    beats = [m for m in _drain(fleet.link.outbox) if m.WhichOneof("payload") == "heartbeat"]
    assert len(beats) == 1 and beats[0].heartbeat.robot_id == ""


# ── a robot the fleet does not serve ─────────────────────────────────────────

@pytest.mark.parametrize("arm, msg", [
    ("assign_action", _assign("act-g", 9)),
    ("renew_lease", pb.RenewActionLease(action_id="act-a", lease_generation=1,
                                        lease_duration_ms=60000)),
    ("cancel_action", pb.CancelAction(action_id="act-a", fencing_token=5)),
    ("pause", pb.PauseCapabilities()),
    ("resume", pb.ResumeCapabilities()),
    ("zone_admission", pb.ZoneAdmission(zone_name="z-1", admit=False)),
    ("validate_action", pb.ValidateAction(action_type="Navigate")),
    ("activate_stream", pb.ActivateStream(robot_id=GHOST, stream_id="front-rgb")),
    ("deactivate_stream", pb.DeactivateStream(robot_id=GHOST, stream_id="front-rgb")),
    ("push_firmware", pb.PushFirmware(target_version="2")),
    ("model_update", pb.ModelUpdate()),
])
def test_a_command_for_an_unserved_robot_reaches_no_robot(make_fleet, arm, msg) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    _result(fleet, 1)
    _command(fleet, 2, GHOST, **{arm: msg})
    result = _result(fleet, 2)
    assert result.robot_id == GHOST
    assert not result.unsupported
    answer = getattr(result, arm if arm != "push_firmware" else "push_firmware")
    assert "is not served by this adapter, which serves ['amr-1', 'amr-2']" in getattr(
        answer, "message", "is not served by this adapter, which serves ['amr-1', 'amr-2']")
    time.sleep(0.4)
    assert fleet.robot(a)._current_action == "act-a" and _moving(fleet, a)
    assert fleet.robot(b)._current_action == "" and _x(fleet, b) == 0.0
    assert fleet.robot(a)._zone_hold == "" and fleet.robot(b)._zone_hold == ""
    assert fleet.robot(a)._fence.highest(a) == 5 and fleet.robot(b)._fence.highest(b) == 0


def test_a_fence_for_an_unserved_robot_is_failed_and_stops_nothing(make_fleet) -> None:
    fleet = make_fleet(2, latch=True)
    a, _b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    _result(fleet, 1)
    _command(fleet, 2, GHOST, estop=pb.Estop(estop_id="fence-g"))
    result = _result(fleet, 2)
    assert result.estop.state == pb.ESTOP_STATE_FAILED
    ack = _estop_ack(fleet, "fence-g")
    assert ack.robot_id == GHOST and ack.estop_ack.state == pb.ESTOP_STATE_FAILED
    time.sleep(0.3)
    assert _moving(fleet, a) and not fleet.robot(a)._latched


def test_another_arm_for_an_unserved_robot_is_declined(make_fleet) -> None:
    fleet = make_fleet(2)
    _command(fleet, 1, GHOST, scan=pb.ScanCapabilities())
    assert _result(fleet, 1).unsupported


def test_an_estop_for_an_unserved_robot_is_failed_and_stops_nothing(make_fleet) -> None:
    fleet = make_fleet(2)
    a, _b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    _safety(fleet, GHOST, estop=pb.Estop(estop_id="stop-g"))
    ack = _estop_ack(fleet, "stop-g")
    assert ack.robot_id == GHOST and ack.estop_ack.state == pb.ESTOP_STATE_FAILED
    time.sleep(0.3)
    assert _moving(fleet, a) and fleet.robot(a)._current_action == "act-a"


# ── stops ─────────────────────────────────────────────────────────────────────

def test_an_estop_stops_its_robot_and_only_its_robot(make_fleet) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    _command(fleet, 2, b, assign_action=_assign("act-b", 5))
    time.sleep(0.5)
    _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))
    ack = _estop_ack(fleet, "stop-a")
    assert ack.robot_id == a and ack.estop_ack.state == pb.ESTOP_STATE_STOPPED
    assert ack.estop_ack.HasField("stop_initiated_at")
    assert not _moving(fleet, a) and fleet.robot(a)._current_action == ""
    time.sleep(0.3)
    assert _moving(fleet, b) and fleet.robot(b)._current_action == "act-b"


def test_a_fleet_wide_stop_is_commanded_for_every_robot_at_once(make_fleet) -> None:
    fleet = make_fleet(10)
    _admit_all(fleet)
    for n, rid in enumerate(fleet._ids, start=1):
        _command(fleet, n, rid, assign_action=_assign(f"act-{n}", 5))
    time.sleep(1.2)   # every robot at cruise speed
    sent = {}
    began = time.monotonic()
    for rid in fleet._ids:
        sent[rid] = time.monotonic()
        _safety(fleet, rid, estop=pb.Estop(estop_id=f"stop-{rid}"))
    # The reader waits on no robot: ten stops are handed over in well under one stop's time.
    assert time.monotonic() - began < 0.5   # serialised, ten would take ~2.5 s
    for rid in fleet._ids:
        with fleet.sim_lock:
            assert fleet._sim._robots[rid].stopping, f"{rid}'s stop was not commanded at once"
    latest = {}
    deadline = time.monotonic() + 3.0
    while len(latest) < len(fleet._ids) and time.monotonic() < deadline:
        try:
            msg = fleet.link.safety_outbox.get(timeout=0.05)
        except queue.Empty:
            continue
        if msg is None or msg.WhichOneof("payload") != "estop_ack":
            continue
        rid = msg.robot_id   # the acks come back in whatever order the robots come to rest
        assert msg.estop_ack.estop_id == f"stop-{rid}"
        if msg.estop_ack.state == pb.ESTOP_STATE_STOPPING:
            continue         # each stop's first answer, sent as it is commanded
        assert msg.estop_ack.state == pb.ESTOP_STATE_STOPPED
        latest[rid] = time.monotonic() - sent[rid]
    assert set(latest) == set(fleet._ids), f"no ack for {set(fleet._ids) - set(latest)}"
    # Serialised, the tenth would wait for nine confirmations (~0.25 s each at 1 m/s).
    assert max(latest.values()) < 1.5, latest
    # And no stop is applied retroactively: from 1 m/s at KinematicSim.DECEL (4 m/s^2) rest
    # takes 0.25 s of world time, which is wall-clock time, so no ack can come sooner.
    assert min(latest.values()) >= 0.2, latest


def test_an_estop_and_its_clear_are_answered_in_order(make_fleet) -> None:
    fleet = make_fleet(2)
    a, _b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    time.sleep(0.5)
    _drain(fleet.link.safety_outbox)
    _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))
    _safety(fleet, a, estop_clear=pb.EstopClear(estop_id="stop-a", leader_term=1))
    time.sleep(1.0)
    answers = [(m.WhichOneof("payload"), m.estop_ack.state)
               for m in _drain(fleet.link.safety_outbox)]
    assert answers == [("estop_ack", pb.ESTOP_STATE_STOPPING),
                       ("estop_ack", pb.ESTOP_STATE_STOPPED),
                       ("estop_clear_ack", pb.ESTOP_STATE_UNSPECIFIED)]


def test_a_failed_confirmation_is_answered_failed(make_fleet, monkeypatch) -> None:
    fleet = make_fleet(2)
    a, _b = fleet._ids
    _admit_all(fleet)

    def broken(*_args):
        raise RuntimeError("lane fault")

    monkeypatch.setattr(fleet.robot(a), "_fleet_estop_finish", broken)
    _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))
    ack = _estop_ack(fleet, "stop-a")
    assert ack.estop_ack.state == pb.ESTOP_STATE_FAILED
    assert ack.estop_ack.HasField("stop_initiated_at")


def test_the_lease_of_one_robot_stops_only_that_robot(make_fleet) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5, lease_ms=300))
    _command(fleet, 2, b, assign_action=_assign("act-b", 5))
    end = _take(fleet.link.outbox, lambda m: m.WhichOneof("payload") == "action_status"
                and m.action_status.action_id == "act-a"
                and m.action_status.state == pb.ACTION_STATE_FAILED)
    assert end.action_status.end_cause == pb.ACTION_END_CAUSE_LEASE_EXPIRED
    assert end.action_status.message == "ended by lease expiry"
    assert not _moving(fleet, a)
    assert _moving(fleet, b) and fleet.robot(b)._current_action == "act-b"


def test_renewing_one_robots_lease_does_not_renew_another(make_fleet) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5, lease_ms=400))
    _command(fleet, 2, b, assign_action=_assign("act-b", 5, lease_ms=60000))
    _command(fleet, 3, b, renew_lease=pb.RenewActionLease(action_id="act-a",
                                                          lease_generation=1,
                                                          lease_duration_ms=60000))
    assert not _result(fleet, 3).renew_lease.running  # b is not running act-a
    _take(fleet.link.outbox, lambda m: m.WhichOneof("payload") == "action_status"
          and m.action_status.action_id == "act-a"
          and m.action_status.state == pb.ACTION_STATE_FAILED)
    assert _moving(fleet, b)


def test_fencing_tokens_are_per_robot(make_fleet) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 100))
    assert _result(fleet, 1).assign_action.accepted
    _command(fleet, 2, b, assign_action=_assign("act-b", 5))
    assert _result(fleet, 2).assign_action.accepted, "a's token made b's assignment stale"
    _command(fleet, 3, a, assign_action=_assign("act-a2", 50))
    assert (_result(fleet, 3).assign_action.rejection
            == pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN)


def test_a_fence_latches_only_its_robot_and_its_clear_lifts_only_it(make_fleet) -> None:
    fleet = make_fleet(2, latch=True)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, estop=pb.Estop(estop_id="stop-a"))
    fence = _result(fleet, 1)
    assert fence.estop.state == pb.ESTOP_STATE_STOPPED
    _command(fleet, 2, a, assign_action=_assign("act-a", 5))
    assert (_result(fleet, 2).assign_action.rejection
            == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE)
    _command(fleet, 3, b, assign_action=_assign("act-b", 5))
    assert _result(fleet, 3).assign_action.accepted
    _safety(fleet, a, estop_clear=pb.EstopClear(estop_id="stop-a", leader_term=1))
    _take(fleet.link.safety_outbox, lambda m: m.WhichOneof("payload") == "estop_clear_ack")
    _command(fleet, 4, a, assign_action=_assign("act-a", 6))
    assert _result(fleet, 4).assign_action.accepted
    assert fleet.robot(b)._current_action == "act-b"


def test_a_clear_that_arrives_while_its_fence_is_confirmed_follows_the_fence(make_fleet):
    fleet = make_fleet(2, latch=True)
    a, _b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    time.sleep(1.0)   # at cruise speed, so the fence's rest takes a few ticks
    _drain(fleet.link.safety_outbox)
    _command(fleet, 2, a, estop=pb.Estop(estop_id="stop-a"))
    _safety(fleet, a, estop_clear=pb.EstopClear(estop_id="stop-a", leader_term=1))
    time.sleep(1.0)
    order = [(m.WhichOneof("payload"), m.estop_ack.message)
             for m in _drain(fleet.link.safety_outbox)
             if m.WhichOneof("payload") in ("estop_ack", "estop_clear_ack")]
    assert order[0] == ("estop_ack", "fence")
    assert order[-1][0] == "estop_clear_ack"
    assert not fleet.robot(a)._latched


def test_a_stale_leader_term_is_rejected_per_robot(make_fleet) -> None:
    fleet = make_fleet(2, latch=True)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, term=5, assign_action=_assign("act-a", 5))
    _result(fleet, 1)
    _command(fleet, 2, a, term=4, assign_action=_assign("act-a2", 6))
    assert _result(fleet, 2).stale_leader_term
    # b has seen no term yet: a term a saw is not b's.
    _command(fleet, 3, b, term=4, assign_action=_assign("act-b", 5))
    assert _result(fleet, 3).assign_action.accepted


# ── between a stop and its confirmation ───────────────────────────────────────

def _cruise(fleet: SimFleet, rid: str, cid: int = 1, token: int = 5) -> None:
    _command(fleet, cid, rid, assign_action=_assign(f"act-{rid}", token))
    time.sleep(1.2)   # at cruise speed, so rest takes a few ticks after a stop


def _world(fleet: SimFleet, rid: str):
    with fleet.sim_lock:
        return fleet._sim._robots[rid]


def test_work_assigned_under_a_stop_moves_only_once_the_stop_is_confirmed(make_fleet) -> None:
    fleet = make_fleet(2)
    a, _b = fleet._ids
    _admit_all(fleet)
    _cruise(fleet, a)
    _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))
    _command(fleet, 2, a, assign_action=_assign("act-back", 6, x=-50.0))
    assert _result(fleet, 2).assign_action.accepted
    state = _world(fleet, a)
    assert state.stopping and state.target is None, "set moving under a stop being confirmed"
    assert fleet.robot(a)._motion_held
    ack = _estop_ack(fleet, "stop-a")
    assert ack.estop_ack.state == pb.ESTOP_STATE_STOPPED
    time.sleep(0.6)
    assert _world(fleet, a).target == (-50.0, 0.0) and _moving(fleet, a)  # then it moves
    assert not fleet.robot(a)._motion_held and fleet.robot(a)._stops_in_flight == 0


def test_a_zone_admit_under_an_edge_stop_moves_nothing_until_it_is_confirmed(make_fleet):
    fleet = make_fleet(2)
    a, _b = fleet._ids
    _admit_all(fleet)
    _cruise(fleet, a)
    fleet._edge_robots["edge-1:9600"] = [fleet.robot(a)]
    box = {}
    worker = threading.Thread(target=lambda: box.setdefault(
        "ack", _edge_stop(fleet, "edge-1:9600")))
    worker.start()
    deadline = time.monotonic() + 1.0
    while fleet.robot(a)._stops_in_flight == 0 and time.monotonic() < deadline:
        time.sleep(0.005)
    _command(fleet, 2, a, zone_admission=pb.ZoneAdmission(zone_name="z-1", admit=True))
    assert _world(fleet, a).stopping, "a zone admit set the robot moving under an edge stop"
    worker.join(3.0)
    assert box["ack"].estop_ack.state == pb.ESTOP_STATE_STOPPED


def test_a_stop_and_an_assignment_on_two_threads_are_one_or_the_other(make_fleet,
                                                                     monkeypatch) -> None:
    fleet = make_fleet(2, latch=True)
    a, _b = fleet._ids
    _admit_all(fleet)
    robot = fleet.robot(a)
    real = robot._servable

    def slow(*args):
        time.sleep(0.3)   # the stop arrives while the assignment is being judged
        return real(*args)

    monkeypatch.setattr(robot, "_servable", slow)
    worker = threading.Thread(target=lambda: _command(
        fleet, 1, a, assign_action=_assign("act-a", 5)))
    worker.start()
    time.sleep(0.1)
    _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))  # waits for the assignment
    worker.join(2.0)
    ack = _estop_ack(fleet, "stop-a")
    assert ack.estop_ack.state == pb.ESTOP_STATE_STOPPED
    assert robot._latched == {"stop-a"} and robot._current_action == ""
    time.sleep(0.4)
    assert not _moving(fleet, a), "latched work was started after the latch"


@pytest.mark.parametrize("path", ["safety", "fence"])
def test_no_latched_stop_is_confirmed_before_the_plant_could_rest(make_fleet, path) -> None:
    fleet = make_fleet(2, latch=True)
    a, _b = fleet._ids
    _admit_all(fleet)
    _cruise(fleet, a)
    sent = time.monotonic()
    if path == "safety":
        _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))
        state = _estop_ack(fleet, "stop-a").estop_ack.state
    else:
        _command(fleet, 2, a, estop=pb.Estop(estop_id="stop-a"))
        state = _result(fleet, 2).estop.state
    took = time.monotonic() - sent
    assert state == pb.ESTOP_STATE_STOPPED
    # From 1 m/s at 4 m/s^2, rest takes 0.25 s of world time, which is wall-clock time.
    assert took >= 0.2, f"confirmed after {took:.3f}s"


def test_the_latch_report_says_stopping_until_the_stop_is_confirmed(make_fleet) -> None:
    fleet = make_fleet(2, latch=True)
    a, _b = fleet._ids
    _admit_all(fleet)
    _cruise(fleet, a)
    _drain(fleet.link.safety_outbox)
    _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))
    _command(fleet, 2, a, assign_action=_assign("act-x", 6))   # refused: reports the latch
    assert (_result(fleet, 2).assign_action.rejection
            == pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE)
    report = _take(fleet.link.safety_outbox, lambda m: m.WhichOneof("payload") == "estop_ack"
                   and m.estop_ack.message == "latched (report)")
    assert report.estop_ack.state == pb.ESTOP_STATE_STOPPING
    assert report.estop_ack.confirmed_at_ms == 0   # nothing confirmed, so no time of it
    assert _estop_ack(fleet, "stop-a").estop_ack.state == pb.ESTOP_STATE_STOPPED


def test_a_robot_that_never_rests_is_answered_failed_and_reported_failed(make_fleet,
                                                                       monkeypatch) -> None:
    fleet = make_fleet(2, latch=True)
    a, b = fleet._ids
    _admit_all(fleet)
    real = fleet._plant.is_stopped
    monkeypatch.setattr(fleet._plant, "is_stopped", lambda rid: False if rid == a else real(rid))
    _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))
    _safety(fleet, b, estop=pb.Estop(estop_id="stop-b"))
    assert _estop_ack(fleet, "stop-b").estop_ack.state == pb.ESTOP_STATE_STOPPED
    ack = _estop_ack(fleet, "stop-a", timeout=4.0)   # the SDK's 2 s stopping budget
    assert ack.estop_ack.state == pb.ESTOP_STATE_FAILED
    _drain(fleet.link.safety_outbox)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    _result(fleet, 1)
    report = _take(fleet.link.safety_outbox, lambda m: m.WhichOneof("payload") == "estop_ack"
                   and m.estop_ack.estop_id == "stop-a")
    assert report.estop_ack.state == pb.ESTOP_STATE_FAILED


def test_a_stop_is_accepted_whatever_its_leader_term(make_fleet) -> None:
    fleet = make_fleet(2, latch=True)
    a, _b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, term=5, assign_action=_assign("act-a", 5))
    _result(fleet, 1)
    _command(fleet, 2, a, term=1, estop=pb.Estop(estop_id="stop-a"))
    assert _result(fleet, 2).estop.state == pb.ESTOP_STATE_STOPPED
    assert fleet.robot(a)._latched == {"stop-a"}


# ── edge nodes ────────────────────────────────────────────────────────────────

def test_a_register_ack_attaches_its_robot_to_its_edge_nodes(make_fleet, monkeypatch) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    monkeypatch.setattr(fleet, "_edge_loop", lambda _address: None)
    fleet._handle_control_plane(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
        accepted=True, edge_endpoints=[pb.EdgeEndpoint(zone="z-1", address="edge-1:9600")])))
    fleet._handle_control_plane(pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
        accepted=True)))
    assert fleet._edge_robots == {"edge-1:9600": [fleet.robot(a)]}
    assert fleet.robot(b) not in fleet._edge_robots["edge-1:9600"]

def test_an_edge_stop_stops_every_robot_served_through_the_node(make_fleet, capsys) -> None:
    fleet = make_fleet(3)
    a, b, c = fleet._ids
    _admit_all(fleet)
    for n, rid in enumerate((a, b, c), start=1):
        _command(fleet, n, rid, assign_action=_assign(f"act-{n}", 5))
    time.sleep(0.6)
    fleet._edge_robots["edge-1:9600"] = [fleet.robot(a), fleet.robot(b)]
    ack = _edge_stop(fleet, "edge-1:9600")
    assert ack.estop_ack.estop_id == "edge-stop"
    assert ack.estop_ack.state == pb.ESTOP_STATE_STOPPED
    assert not _moving(fleet, a) and not _moving(fleet, b)
    assert _moving(fleet, c)
    err = capsys.readouterr().err
    assert f"EDGE_ESTOP_CONFIRMED robot={a} id=edge-stop state=STOPPED" in err
    assert f"EDGE_ESTOP_CONFIRMED robot={b} id=edge-stop state=STOPPED" in err


def test_an_edge_stop_is_failed_if_any_robot_is_not_confirmed(make_fleet, monkeypatch) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    fleet._edge_robots["edge-1:9600"] = [fleet.robot(a), fleet.robot(b)]
    real = fleet._await_rest
    monkeypatch.setattr(fleet, "_await_rest", lambda rid: (
        pb.ESTOP_STATE_FAILED if rid == b else real(rid)))
    ack = _edge_stop(fleet, "edge-1:9600")
    assert ack.estop_ack.state == pb.ESTOP_STATE_FAILED


def test_an_edge_stop_through_a_node_no_robot_uses_is_failed(make_fleet) -> None:
    fleet = make_fleet(2)
    ack = _edge_stop(fleet, "edge-9:9600")
    assert ack.estop_ack.state == pb.ESTOP_STATE_FAILED


def test_a_robot_is_attached_to_an_edge_node_once(make_fleet, monkeypatch) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    dialled = []
    monkeypatch.setattr(sim_adapter.threading, "Thread",
                        lambda target, args=(), daemon=None, name=None: _NoThread(
                            dialled, args))
    fleet._edge_attach(fleet.robot(a), "edge-1:9600")
    fleet._edge_attach(fleet.robot(b), "edge-1:9600")
    fleet._edge_attach(fleet.robot(a), "edge-1:9600")   # a re-register on a new stream
    assert dialled == ["edge-1:9600"]
    assert fleet._edge_robots["edge-1:9600"] == [fleet.robot(a), fleet.robot(b)]
    # The loop sends the frames of the robots attached when it dials (here a); a robot
    # attached to a node already dialled gets its frame at once.
    frames = _drain(fleet._edge_outboxes["edge-1:9600"])
    assert [f.position.robot_id for f in frames] == [b]


class _NoThread:
    def __init__(self, dialled: list, args: tuple) -> None:
        dialled.append(args[0])

    def start(self) -> None:
        pass


def test_a_robot_leaves_an_edge_node_its_register_ack_no_longer_lists(make_fleet,
                                                                    monkeypatch) -> None:
    fleet = make_fleet(2)
    a, _b = fleet._ids
    monkeypatch.setattr(fleet, "_edge_loop", lambda _address: None)
    fleet._edge_sync(fleet.robot(a), ["edge-1:9600", "edge-2:9600"])
    fleet._edge_sync(fleet.robot(a), ["edge-2:9600"])
    assert fleet._edge_robots["edge-1:9600"] == []
    assert fleet._edge_robots["edge-2:9600"] == [fleet.robot(a)]


class _Dropped(sim_adapter.grpc.RpcError):
    def code(self):
        return sim_adapter.grpc.StatusCode.UNAVAILABLE


def test_an_edge_stream_that_drops_is_redialled(make_fleet) -> None:
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    _command(fleet, 2, b, assign_action=_assign("act-b", 5))
    streams = []

    class EdgeStub:
        def __init__(self, _channel) -> None:
            pass

        def EdgeStream(self, _requests):  # noqa: N802 -- the gRPC method name
            streams.append(1)
            if len(streams) == 1:
                raise _Dropped()   # the node is not up yet

            def node():
                yield pb.EdgeControlMessage(estop=pb.Estop(estop_id="edge-stop"))
                while not fleet.shutdown_event.wait(0.05):
                    pass
            return node()

    fleet._pb_grpc = type("Grpc", (), {"EdgeServiceStub": EdgeStub})
    fleet._edge_sync(fleet.robot(a), ["edge-1:9600"])
    deadline = time.monotonic() + 3.0
    while len(streams) < 2 and time.monotonic() < deadline:
        time.sleep(0.02)
    assert len(streams) == 2, "the edge stream was not redialled"
    outbox = fleet._edge_outboxes["edge-1:9600"]
    frame = _take(outbox, lambda m: m.WhichOneof("msg") == "position")
    assert frame.position.robot_id == a   # the new stream gets the node's robots' poses
    ack = _take(outbox, lambda m: m.WhichOneof("msg") == "estop_ack"
                and m.estop_ack.state != pb.ESTOP_STATE_STOPPING)
    assert ack.estop_ack.state == pb.ESTOP_STATE_STOPPED
    assert not _moving(fleet, a) and _moving(fleet, b)


def test_a_fault_in_one_robots_command_stops_that_robot_and_spares_the_rest(make_fleet,
                                                                          monkeypatch):
    fleet = make_fleet(2)
    a, b = fleet._ids
    _admit_all(fleet)
    _command(fleet, 1, a, assign_action=_assign("act-a", 5))
    _command(fleet, 2, b, assign_action=_assign("act-b", 5))
    time.sleep(0.5)

    def boom(*_args):
        raise OSError("read-only file system")

    monkeypatch.setattr(fleet.robot(a), "_on_pause", boom)
    _command(fleet, 3, a, pause=pb.PauseCapabilities())   # raises inside a's handler
    # a's work is ended first (as a stop ends it), then the command is refused in band.
    end = _take(fleet.link.outbox, lambda m: m.WhichOneof("payload") == "action_status"
                and m.action_status.action_id == "act-a"
                and m.action_status.state == pb.ACTION_STATE_FAILED)
    assert end.action_status.end_cause == pb.ACTION_END_CAUSE_ESTOP
    result = _result(fleet, 3)
    assert not result.pause.paused and "failed while handling" in result.pause.message
    time.sleep(0.5)
    assert not _moving(fleet, a) and fleet.robot(a)._current_action == ""
    assert _moving(fleet, b) and fleet.robot(b)._current_action == "act-b"


def test_a_fence_receipt_goes_on_the_stream_the_fence_came_on(make_fleet) -> None:
    fleet = make_fleet(2, latch=True)
    a, _b = fleet._ids
    _admit_all(fleet)
    _cruise(fleet, a)
    old = fleet.link.outbox
    _command(fleet, 2, a, estop=pb.Estop(estop_id="stop-a"))
    fleet.link.outbox = queue.Queue()   # the ControlStream is replaced while rest is awaited
    receipt = _take(old, lambda m: m.WhichOneof("payload") == "command_result"
                    and m.command_result.command_id == 2)
    assert receipt.command_result.estop.state == pb.ESTOP_STATE_STOPPED
    assert not [m for m in _drain(fleet.link.outbox)
                if m.WhichOneof("payload") == "command_result"]


def test_a_fence_that_faults_still_stops_the_robot_and_is_answered_failed(make_fleet,
                                                                         monkeypatch):
    fleet = make_fleet(2, latch=True)
    a, _b = fleet._ids
    _admit_all(fleet)
    _cruise(fleet, a)

    def boom():
        raise RuntimeError("interrupt fault")

    monkeypatch.setattr(fleet.robot(a), "_interrupt_work", boom)
    _command(fleet, 2, a, estop=pb.Estop(estop_id="stop-a"))
    assert _result(fleet, 2).estop.state == pb.ESTOP_STATE_FAILED
    assert _estop_ack(fleet, "stop-a").estop_ack.state == pb.ESTOP_STATE_FAILED
    assert _world(fleet, a).stopping, "the fence's stop was not commanded"
    assert fleet.robot(a)._stops_in_flight == 0, "the faulted stop was left unbalanced"
    assert fleet.robot(a)._latched == {"stop-a"}   # still refuses work
    # The fence arrived, so its stop can still be cleared (RFC-0001, the stop latch, The
    # clear): an operator's clear must not wait for a restart.
    _safety(fleet, a, estop_clear=pb.EstopClear(estop_id="stop-a", leader_term=1))
    clear = _take(fleet.link.safety_outbox,
                  lambda m: m.WhichOneof("payload") == "estop_clear_ack")
    assert clear.estop_clear_ack.estop_id == "stop-a"
    assert not fleet.robot(a)._latched


def test_an_edge_stop_whose_confirmation_faults_answers_every_robot(make_fleet,
                                                                   monkeypatch) -> None:
    fleet = make_fleet(3)
    a, b, c = fleet._ids
    _admit_all(fleet)
    fleet._edge_robots["edge-1:9600"] = [fleet.robot(a), fleet.robot(b), fleet.robot(c)]
    real = fleet._await_rest

    def await_rest(rid):
        if rid == a:
            raise RuntimeError("confirmation fault")
        return real(rid)

    monkeypatch.setattr(fleet, "_await_rest", await_rest)
    ack = _edge_stop(fleet, "edge-1:9600")
    assert ack.estop_ack.state == pb.ESTOP_STATE_FAILED
    assert [fleet.robot(r)._stops_in_flight for r in (a, b, c)] == [0, 0, 0]


# ── the fleet's own stream loops, through a scripted stub ─────────────────────

class _FleetStub:
    def __init__(self, fleet: SimFleet, control: list, safety: list) -> None:
        self._fleet = fleet
        self._control = list(control)
        self._safety = list(safety)
        self.control_queues: list = []
        self.safety_queues: list = []

    def ControlStream(self, requests):  # noqa: N802 -- the gRPC method name
        self.control_queues.append(self._fleet.link.outbox)
        next(requests)   # the hello
        return self._control.pop(0)()

    def SafetyStream(self, _requests):  # noqa: N802 -- the gRPC method name
        self.safety_queues.append(self._fleet.link.safety_outbox)
        return self._safety.pop(0)()


def _bare_fleet(n: int, latch: bool = False) -> SimFleet:
    fleet = SimFleet(pb, None, make_simulator("kinematic"), _ids(n), "z-1", "simulation",
                     None, latch=latch)
    for rid in _ids(n):
        fleet.robot(rid)._schedule = lambda _delay, _fn: None
    return fleet


def test_a_reconnect_registers_every_robot_again_and_pairs_afresh() -> None:
    fleet = _bare_fleet(3)
    a, b, c = fleet._ids

    def first():
        yield pb.ControlPlaneMessage(hello_ack=pb.HelloAck(accepted=True))
        for _ in range(2):   # the stream drops with c's register unanswered
            yield pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
                accepted=True, telemetry_interval_seconds=3))
        raise _Dropped()

    def second():
        yield pb.ControlPlaneMessage(hello_ack=pb.HelloAck(accepted=True))
        for secs in (4, 5, 6):
            yield pb.ControlPlaneMessage(register_ack=pb.RegisterAck(
                accepted=True, telemetry_interval_seconds=secs))

    stub = _FleetStub(fleet, [first, second], [])
    try:
        fleet.run_control_stream(stub)   # returns on the second session's clean close
    finally:
        fleet.shutdown()
    registers = [[m.register.robot_id for m in _drain(q) if m is not None
                  and m.WhichOneof("payload") == "register"] for q in stub.control_queues]
    assert registers == [[a, b, c], [a, b, c]]
    # Paired afresh: had c's unanswered register carried over, the second session's first
    # ack would have gone to c.
    assert [fleet.robot(r)._telemetry_interval for r in (a, b, c)] == [4.0, 5.0, 6.0]


def test_a_stop_confirmed_after_its_safety_stream_dropped_and_the_report_on_the_new_one():
    fleet = _bare_fleet(2, latch=True)
    a, _b = fleet._ids
    with fleet.sim_lock:
        for rid in fleet._ids:
            fleet._sim.spawn(rid, 0.0, 0.0)
    fleet._handle_control_plane(pb.ControlPlaneMessage(hello_ack=pb.HelloAck(
        accepted=True, negotiated_contract_version="1.4.0")))
    _admit_all(fleet)
    _cruise(fleet, a)

    def first():
        yield pb.ControlPlaneSafetyMessage(robot_id=a, estop=pb.Estop(estop_id="stop-a"))
        raise _Dropped()   # mid-confirmation

    def second():
        while not fleet.shutdown_event.wait(0.05):
            pass
        return
        yield  # pragma: no cover -- a generator that sends nothing

    stub = _FleetStub(fleet, [], [first, second])
    worker = threading.Thread(target=fleet.run_safety_stream, args=(stub,), daemon=True)
    worker.start()
    try:
        deadline = time.monotonic() + 3.0
        while len(stub.safety_queues) < 2 and time.monotonic() < deadline:
            time.sleep(0.02)
        assert len(stub.safety_queues) == 2
        report = _take(stub.safety_queues[1],
                       lambda m: m.WhichOneof("payload") == "estop_ack"
                       and m.estop_ack.estop_id == "stop-a")
        assert report.estop_ack.message == "latched (report)"
        # The stop's own ack went to the stream it came on, never to the new one.
        ack = _take(stub.safety_queues[0],
                    lambda m: m.WhichOneof("payload") == "estop_ack"
                    and m.estop_ack.message == ""
                    and m.estop_ack.state != pb.ESTOP_STATE_STOPPING)
        assert ack.estop_ack.state == pb.ESTOP_STATE_STOPPED
        assert not _moving(fleet, a)
    finally:
        fleet.shutdown()
        worker.join(2.0)


# ── the SDK's estop-binding contract (swarmada_sdk.safety, state_seq) ──────────

def test_a_fleet_stop_is_confirmed_without_a_state_seq_warning(make_fleet, monkeypatch,
                                                             caplog) -> None:
    """The fleet confirms rest through _LockedPlant, which has no state_seq: it reads the
    simulated world directly, under the simulator lock, and the fleet's tick thread keeps that
    world current. It says so (tick_keeps_state_current), as KinematicSim does; without it the
    SDK warns that a confirmation relies on tick() keeping the state current."""
    import logging

    from swarmada_sdk import safety
    monkeypatch.setattr(safety, "_warned_without_state_seq", set())
    fleet = make_fleet(2)
    a, _b = fleet._ids
    _admit_all(fleet)
    _cruise(fleet, a)
    with caplog.at_level(logging.WARNING, logger="swarmada_sdk.safety"):
        _safety(fleet, a, estop=pb.Estop(estop_id="stop-a"))
        assert _estop_ack(fleet, "stop-a").estop_ack.state == pb.ESTOP_STATE_STOPPED
    assert not [r for r in caplog.records if "has no state_seq" in r.getMessage()]


def test_the_warning_the_test_above_watches_for_is_real(monkeypatch, caplog) -> None:
    """A control: a binding with neither state_seq nor tick_keeps_state_current IS warned about,
    so the absence of the warning above means something."""
    import logging

    from swarmada_sdk import safety

    class Bare:
        def tick(self, _dt) -> None:
            pass

        def is_stopped(self, _robot) -> bool:
            return True

    monkeypatch.setattr(safety, "_warned_without_state_seq", set())
    with caplog.at_level(logging.WARNING, logger="swarmada_sdk.safety"):
        safety.await_estop_confirmation(Bare(), "r1", sleep=lambda _: None)
    assert [r for r in caplog.records if "has no state_seq" in r.getMessage()]


# ── the robot served alone is untouched by any of this ───────────────────────

def test_a_robot_served_alone_has_no_fleet() -> None:
    robot = SimAdapter(pb, None, make_simulator("kinematic"), "amr-1", "z", "sim")
    assert robot._fleet is None
    assert robot._outbox is robot._link.outbox
