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

"""Multi-robot mode and C20: robots served on one connection are kept apart.

Why this file exists. The harness drove one robot, so nothing graded an adapter that serves
several robots on one ControlStream: a command for one robot that moves another, a token mark
or a lease shared between robots, a clear that lifts every robot's latch. Each fake below
breaks one of those, and each test fails on a harness without C20.

The lens tests pin how multi-robot mode reads one robot's traffic out of the shared streams:
each robot after the first gets its own action and stop IDs, and nothing a robot sends is lost
while another robot's rows run.
"""

from __future__ import annotations

import queue
import threading
import time

import pytest

from adapters.conformance.report import CheckStatus, Report, SkipCause

A, B = "amr-1", "amr-2"
GHOST = "robot-not-served"


@pytest.fixture
def quick(harness_mod, monkeypatch):
    for name, value in (("_GET_TIMEOUT", 1.0), ("_CLEAR_WAIT", 0.6), ("_C20_WATCH", 0.4),
                        ("_C20_LEASE_WAIT", 1.8), ("_C20_LEASE_MS", 900),
                        ("_UNSERVED_ACK_WAIT", 0.6), ("_UNSERVED_ACK_TAIL", 0.3)):
        monkeypatch.setattr(harness_mod, name, value)


def _multi_driver(harness_mod, robots=(A, B)):
    pb = harness_mod.pb
    session = harness_mod._Session()
    report = Report(adapter="test-double")
    drv = harness_mod._Driver(session, report, port=0, robots=list(robots))
    navigate = pb.SupportedAction(action_type="Navigate", params=[
        pb.ActionParam(name="x", unit="m", kind="number", required=True),
        pb.ActionParam(name="y", unit="m", kind="number", required=True)])
    drv._catalog, drv._served = [navigate], navigate
    drv._contract_ok, drv._reported_contract = True, "1.4.0"
    return drv, session, report


def _rows(report):
    return {r.check_id: r for r in report.results}


class _Fleet:
    """A scripted adapter serving ``robots`` on one session, broken as its flags say.

    ``shared_mark``: one fencing-token mark for every robot. ``shared_lease``: a renewal of
    any robot's lease renews every robot's. ``clear_lifts_all``: a clear lifts every robot's
    latch. ``stop_all``: a stop for one robot stops every robot. ``route_to``: run every
    command on this robot, whatever it names. ``unserved``: how a stop for a robot not served
    is answered (an EstopState, or None for no answer); ``accept_unserved``: an assignment
    for one is accepted.
    """

    def __init__(self, pb, session, robots=(A, B), *, shared_mark=False, shared_lease=False,
                 clear_lifts_all=False, stop_all=False, route_to=None, unserved=None,
                 accept_unserved=False) -> None:
        self.pb, self.s, self.robots = pb, session, list(robots)
        self.shared_mark, self.shared_lease = shared_mark, shared_lease
        self.clear_lifts_all, self.stop_all, self.route_to = clear_lifts_all, stop_all, route_to
        self.unserved = pb.ESTOP_STATE_FAILED if unserved is None else unserved
        self.accept_unserved = accept_unserved
        self.mark = {r: 0 for r in self.robots}
        self.latched = {r: set() for r in self.robots}
        self.action: dict[str, str | None] = {r: None for r in self.robots}
        self.lease: dict[str, float | None] = {r: None for r in self.robots}
        self.fenced: set[str] = set()
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._threads = [threading.Thread(target=f, daemon=True)
                         for f in (self._control, self._safety, self._tick)]

    def __enter__(self):
        for t in self._threads:
            t.start()
        return self

    def __exit__(self, *exc):
        self._stop.set()
        for t in self._threads:
            t.join(timeout=2)

    def _status(self, action_id, state) -> None:
        self.s.control_in.put(self.pb.AdapterMessage(action_status=self.pb.ActionStatusUpdate(
            action_id=action_id, state=state)))

    def _end(self, robot, state) -> None:
        if self.action[robot]:
            self._status(self.action[robot], state)
        self.action[robot], self.lease[robot] = None, None

    def _control(self) -> None:
        pb = self.pb
        while not self._stop.is_set():
            try:
                msg = self.s.control_out.get(timeout=0.02)
            except queue.Empty:
                continue
            if not hasattr(msg, "WhichOneof") or msg.WhichOneof("payload") != "command":
                continue
            cmd = msg.command
            arm = cmd.WhichOneof("command")
            result = pb.CommandResult(command_id=cmd.command_id, robot_id=cmd.robot_id)
            named = cmd.robot_id
            with self._lock:
                if named not in self.robots:
                    if arm == "assign_action":
                        result.assign_action.CopyFrom(pb.AssignActionResult(
                            accepted=self.accept_unserved,
                            rejection=(0 if self.accept_unserved
                                       else pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE)))
                        if self.accept_unserved:
                            self._status(cmd.assign_action.action_id, pb.ACTION_STATE_RUNNING)
                    elif arm == "estop":
                        continue
                    self.s.control_in.put(pb.AdapterMessage(command_result=result))
                    continue
                robot = self.route_to or named
                if arm == "estop":
                    self.fenced.add(cmd.estop.estop_id)
                    self.latched[robot].add(cmd.estop.estop_id)
                    continue
                if arm == "assign_action":
                    a = cmd.assign_action
                    mark = max(self.mark.values()) if self.shared_mark else self.mark[robot]
                    if self.latched[robot]:
                        res = pb.AssignActionResult(
                            accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE)
                    elif a.fencing_token <= mark:
                        res = pb.AssignActionResult(
                            accepted=False,
                            rejection=pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN)
                    else:
                        self.mark[robot] = a.fencing_token
                        self.action[robot] = a.action_id
                        self.lease[robot] = (time.monotonic() + a.lease_duration_ms / 1000.0
                                             if a.lease_duration_ms else None)
                        res = pb.AssignActionResult(accepted=True,
                                                    accepted_fencing_token=a.fencing_token)
                        self._status(a.action_id, pb.ACTION_STATE_RUNNING)
                    result.assign_action.CopyFrom(res)
                elif arm == "cancel_action":
                    if self.action[robot] == cmd.cancel_action.action_id:
                        self._end(robot, pb.ACTION_STATE_CANCELLED)
                    result.cancel_action.CopyFrom(pb.CancelActionResult(
                        acknowledged=True, disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY))
                elif arm == "renew_lease":
                    r = cmd.renew_lease
                    mine = self.action[robot] == r.action_id
                    if mine:
                        renewed = (self.robots if self.shared_lease else [robot])
                        for other in renewed:
                            if self.lease[other] is not None:
                                self.lease[other] = time.monotonic() + r.lease_duration_ms / 1000
                    result.renew_lease.CopyFrom(pb.RenewActionLeaseResult(
                        renewed=mine, running=mine))
            self.s.control_in.put(pb.AdapterMessage(command_result=result))

    def _safety(self) -> None:
        pb = self.pb
        while not self._stop.is_set():
            try:
                msg = self.s.safety_out.get(timeout=0.02)
            except queue.Empty:
                continue
            if not hasattr(msg, "WhichOneof"):
                continue
            arm = msg.WhichOneof("payload")
            with self._lock:
                if arm == "estop":
                    stop_id = msg.estop.estop_id
                    if msg.robot_id not in self.robots:
                        if self.unserved is not False:
                            self.s.safety_in.put(pb.AdapterSafetyMessage(
                                robot_id=msg.robot_id, estop_ack=pb.EstopAck(
                                    estop_id=stop_id, state=self.unserved)))
                        continue
                    hit = self.robots if self.stop_all else [self.route_to or msg.robot_id]
                    for robot in hit:
                        self.latched[robot].add(stop_id)
                        self._end(robot, pb.ACTION_STATE_STOPPED)
                    self.s.safety_in.put(pb.AdapterSafetyMessage(
                        robot_id=msg.robot_id, estop_ack=pb.EstopAck(
                            estop_id=stop_id, state=pb.ESTOP_STATE_STOPPED,
                            stop_initiated_at=1)))
                elif arm == "estop_clear":
                    stop_id = msg.estop_clear.estop_id
                    if stop_id not in self.fenced:
                        continue
                    for robot in (self.robots if self.clear_lifts_all else [msg.robot_id]):
                        self.latched[robot].clear() if self.clear_lifts_all else \
                            self.latched[robot].discard(stop_id)
                    self.s.safety_in.put(pb.AdapterSafetyMessage(
                        robot_id=msg.robot_id,
                        estop_clear_ack=pb.EstopClearAck(estop_id=stop_id)))

    def _tick(self) -> None:
        pb = self.pb
        while not self._stop.is_set():
            time.sleep(0.05)
            with self._lock:
                for robot in self.robots:
                    if self.lease[robot] is not None and time.monotonic() > self.lease[robot]:
                        self._end(robot, pb.ACTION_STATE_STOPPED)
                    self.s.control_in.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(
                        robot_id=robot, current_action=self.action[robot] or "")))


# -- the lens ---------------------------------------------------------------------


def test_each_robot_after_the_first_gets_its_own_ids(harness_mod):
    pb = harness_mod.pb
    demux = harness_mod._Demux(harness_mod._Session(), [A, B, "amr-3"])
    out = demux.to_robot(B, pb.ControlPlaneMessage(command=pb.Command(
        command_id=7, robot_id=B, assign_action=pb.AssignAction(action_id="t1"))))
    assert out.command.assign_action.action_id == "t1-r2"
    stop = demux.to_robot("amr-3", pb.ControlPlaneSafetyMessage(
        robot_id="amr-3", estop_clear=pb.EstopClear(estop_id="e1")))
    assert stop.estop_clear.estop_id == "e1-r3"
    first = demux.to_robot(A, pb.ControlPlaneMessage(command=pb.Command(
        command_id=8, robot_id=A, assign_action=pb.AssignAction(action_id="t1"))))
    assert first.command.assign_action.action_id == "t1", "the first robot's IDs are as sent"
    # An ActionStatusUpdate names no robot: it is the robot's whose ID it carries, and that
    # robot reads it with its own suffix removed.
    status = pb.AdapterMessage(action_status=pb.ActionStatusUpdate(action_id="t1-r2"))
    assert demux.owner(status) == B
    assert demux.from_robot(B, status).action_status.action_id == "t1"
    assert demux.owner(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
        action_id="t1"))) == A
    # A result is the robot's whose command it answers, whatever it names.
    assert demux.owner(pb.AdapterMessage(command_result=pb.CommandResult(command_id=7))) == B
    # A message for a robot the run does not serve belongs to no lens.
    assert demux.owner(pb.AdapterMessage(telemetry=pb.TelemetryPayload(robot_id=GHOST))) is None


def test_a_robots_traffic_is_held_while_another_robots_rows_read(harness_mod):
    pb = harness_mod.pb
    session = harness_mod._Session()
    demux = harness_mod._Demux(session, [A, B])
    lens_a = harness_mod._RobotLens(session, demux, A)
    lens_b = harness_mod._RobotLens(session, demux, B)
    session.control_in.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(robot_id=B)))
    session.control_in.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
        action_id="t-status-r2", state=pb.ACTION_STATE_CANCELLED)))
    session.control_in.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(robot_id=GHOST)))
    session.control_in.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(robot_id=A)))
    assert lens_a.control_in.get(timeout=0.5).telemetry.robot_id == A
    with pytest.raises(queue.Empty):
        lens_a.control_in.get(timeout=0.1)
    assert lens_b.control_in.get(timeout=0.1).telemetry.robot_id == B
    assert lens_b.control_in.get(timeout=0.1).action_status.action_id == "t-status"
    with pytest.raises(queue.Empty):
        lens_b.control_in.get(timeout=0.1)


def test_a_row_is_the_worst_of_the_robots(harness_mod):
    def report(*rows):
        r = Report(adapter="x")
        for cid, status, detail in rows:
            r.add(cid, "t", status, detail=detail)
        return r

    skip = SkipCause.ADAPTER_DID_NOT_REACH_STATE.value + ": no answer"
    merged = {r.check_id: r for r in harness_mod._merge_robot_rows([A, B], {
        A: report(("C1", CheckStatus.PASS, "ok"), ("C2", CheckStatus.PASS, ""),
                  ("C3", CheckStatus.FAIL, "bad"), ("C4", CheckStatus.PASS, "only a")),
        B: report(("C1", CheckStatus.PASS, "ok"), ("C2", CheckStatus.SKIP, skip),
                  ("C3", CheckStatus.SKIP, skip)),
    })}
    assert merged["C1"].status is CheckStatus.PASS
    assert merged["C2"].status is CheckStatus.SKIP
    assert merged["C2"].detail.startswith(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value), (
        "a merged skip keeps its enumerated cause first, so C0.1 can read it")
    assert merged["C3"].status is CheckStatus.FAIL and "amr-2: skip" in merged["C3"].detail
    assert merged["C4"].status is CheckStatus.SKIP, "a row one robot never recorded is not "
    assert "not recorded for amr-2" in merged["C4"].detail, "established for that robot"


# -- one robot --------------------------------------------------------------------


def test_one_robot_records_the_rows_that_need_two_as_not_applicable(driver):
    drv, _session, report = driver
    drv._check_robots_kept_apart()
    rows = _rows(report)
    for cid in ("C20.1", "C20.2", "C20.5", "C20.6", "C20.7"):
        assert rows[cid].status is CheckStatus.SKIP
        assert rows[cid].detail.startswith(SkipCause.ADAPTER_SERVES_ONE_ROBOT.value)


def test_the_profile_layer_knows_the_cause_and_never_counts_it():
    from conformance import model
    cause = model.SkipCause(SkipCause.ADAPTER_SERVES_ONE_ROBOT.value)
    assert cause in model.NOT_APPLICABLE_CAUSES
    assert cause in model.CAUSE_GRADE


# -- C20.3 and C20.4 -----------------------------------------------------------------


def _unserved(harness_mod, drv, session, **fleet):
    pb = harness_mod.pb
    with _Fleet(pb, session, [drv.robot], **fleet):
        drv._probe_unserved_stop_before_registration()
        drv._check_unserved_command()
        drv._check_unserved_stop()


def test_c20_3_and_c20_4_pass_an_adapter_that_refuses_and_fails(harness_mod, driver, quick):
    drv, session, report = driver
    drv._contract_ok, drv._reported_contract = True, "1.4.0"
    _unserved(harness_mod, drv, session)
    rows = _rows(report)
    assert rows["C20.3"].status is CheckStatus.PASS, rows["C20.3"].detail
    assert rows["C20.4"].status is CheckStatus.PASS, rows["C20.4"].detail
    assert rows["C20.3"].level == rows["C20.4"].level == "SHOULD"


def test_c20_4_fails_a_stopped_for_a_robot_not_served(harness_mod, driver, quick):
    drv, session, report = driver
    _unserved(harness_mod, drv, session, unserved=harness_mod.pb.ESTOP_STATE_STOPPED)
    row = _rows(report)["C20.4"]
    assert row.status is CheckStatus.FAIL and "STOPPED" in row.detail
    assert not row.is_blocking_failure, "advisory: RFC-0001 states no such rule in words"


def test_c20_4_fails_a_stop_left_unanswered(harness_mod, driver, quick):
    drv, session, report = driver
    _unserved(harness_mod, drv, session, unserved=False)
    row = _rows(report)["C20.4"]
    assert row.status is CheckStatus.FAIL and "no EstopAck" in row.detail


def test_c20_3_fails_an_assignment_accepted_for_a_robot_not_served(harness_mod, driver, quick):
    drv, session, report = driver
    _unserved(harness_mod, drv, session, accept_unserved=True)
    row = _rows(report)["C20.3"]
    assert row.status is CheckStatus.FAIL and "accepted" in row.detail


# -- C20.1, C20.2, C20.5 to C20.7 --------------------------------------------------


def _kept_apart(harness_mod, **fleet):
    drv, session, report = _multi_driver(harness_mod)
    with _Fleet(harness_mod.pb, session, **fleet):
        drv._check_robots_kept_apart()
    return _rows(report)


def test_an_adapter_that_keeps_its_robots_apart_passes(harness_mod, quick):
    rows = _kept_apart(harness_mod)
    for cid in ("C20.1", "C20.2", "C20.5", "C20.6", "C20.7"):
        assert rows[cid].status is CheckStatus.PASS, (cid, rows[cid].detail)


def test_c20_2_fails_a_command_run_on_another_robot(harness_mod, quick):
    rows = _kept_apart(harness_mod, route_to=A)
    assert rows["C20.2"].status is CheckStatus.FAIL, rows["C20.2"].detail


def test_c20_5_fails_one_mark_for_every_robot(harness_mod, quick):
    rows = _kept_apart(harness_mod, shared_mark=True)
    assert rows["C20.5"].status is CheckStatus.FAIL, rows["C20.5"].detail


def test_c20_6_fails_one_renewal_for_every_lease(harness_mod, quick):
    rows = _kept_apart(harness_mod, shared_lease=True)
    assert rows["C20.6"].status is CheckStatus.FAIL, rows["C20.6"].detail


def test_c20_7_fails_a_clear_that_lifts_every_robot(harness_mod, quick):
    rows = _kept_apart(harness_mod, clear_lifts_all=True)
    assert rows["C20.7"].status is CheckStatus.FAIL, rows["C20.7"].detail


def test_c20_1_warns_on_a_stop_that_stops_every_robot(harness_mod, quick):
    rows = _kept_apart(harness_mod, stop_all=True)
    row = rows["C20.1"]
    assert row.status is CheckStatus.FAIL and not row.is_blocking_failure, row.detail


def test_c20_7_does_not_bind_a_1_3_connection(harness_mod, quick):
    drv, session, report = _multi_driver(harness_mod)
    drv._reported_contract = "1.3.0"
    drv._check_robots_kept_apart()
    for cid in ("C20.1", "C20.2", "C20.5", "C20.6", "C20.7"):
        assert _rows(report)[cid].detail.startswith(
            SkipCause.CONTRACT_PREDATES_OBLIGATION.value), cid


# -- registration, the edge, the refused session ----------------------------------------


def test_each_robot_is_played_through_first_contact(harness_mod, monkeypatch):
    pb = harness_mod.pb
    monkeypatch.setattr(harness_mod, "_ADMISSION_WAIT", 3.0)
    drv, session, _report = _multi_driver(harness_mod)
    acks = []

    def adapter():
        # The acks answer the registers in the order they were sent: the adapter pairs them.
        pending = [A, B]
        for robot in pending:
            session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(robot_id=robot)))
        while len(acks) < 4:
            msg = session.control_out.get(timeout=2)
            if msg.WhichOneof("payload") != "register_ack":
                continue
            robot = pending.pop(0)
            acks.append((robot, msg.register_ack.accepted))
            if not msg.register_ack.accepted:
                session.control_in.put(pb.AdapterMessage(
                    discover=pb.DiscoverRobot(robot_id=robot)))
                session.control_in.put(pb.AdapterMessage(
                    register=pb.RegisterRobot(robot_id=robot)))
                pending.append(robot)

    t = threading.Thread(target=adapter, daemon=True)
    t.start()
    assert drv._handle_registrations() is True
    t.join(timeout=3)
    for robot in (A, B):
        row = _rows(drv._ctxs[robot]["r"])["C2.1"]
        assert row.status is CheckStatus.PASS, (robot, row.detail)
    assert [ok for _r, ok in acks] == [False, False, True, True]


def test_c8_2_needs_a_frame_for_every_robot(harness_mod, monkeypatch):
    pb = harness_mod.pb
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.3)
    drv, session, report = _multi_driver(harness_mod)
    session.edge_established.set()
    session.edge_in.put(pb.AdapterEdgeMessage(position=pb.PositionFrame(robot_id=A)))
    drv._check_edge(True)
    row = _rows(report)["C8.2"]
    assert row.status is CheckStatus.FAIL and B in row.detail


def test_the_refused_session_refuses_every_robots_register(harness_mod, monkeypatch):
    pb = harness_mod.pb
    drv, session, _report = _multi_driver(harness_mod)
    for robot in (A, B):
        session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(robot_id=robot)))
    assert drv._refuse_registration_version_mismatch() is True
    acks = []
    while not session.control_out.empty():
        acks.append(session.control_out.get_nowait().register_ack)
    assert [a.rejection for a in acks] == [pb.REGISTRATION_REJECTION_VERSION_MISMATCH] * 2


def test_every_accepted_register_ack_lists_the_edge_endpoint(harness_mod, driver, pb):
    drv, session, _report = driver
    drv._accept_register(pb.AdapterMessage(register=pb.RegisterRobot(robot_id="r")))
    ack = session.control_out.get_nowait().register_ack
    assert [e.address for e in ack.edge_endpoints] == ["localhost:0"]
    assert drv._edge_endpoints()[0].address == "localhost:0", (
        "the reconnect's RegisterAck lists the same endpoint (harness.py, the accepting "
        "reconnect): an adapter serves a robot through exactly the nodes its latest ack lists")


# -- from the distributed-systems review ---------------------------------------------


def test_robots_registered_one_after_another_are_each_admitted(harness_mod, monkeypatch):
    """An adapter may register its next robot only once the one before is admitted. The robot
    not heard from yet is not given up on while another is mid-way through first contact."""
    pb = harness_mod.pb
    monkeypatch.setattr(harness_mod, "_ADMISSION_WAIT", 8.0)
    drv, session, _report = _multi_driver(harness_mod)
    acks = []

    def adapter():
        for robot in (A, B):
            session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(robot_id=robot)))
            while True:
                msg = session.control_out.get(timeout=5)
                if msg.WhichOneof("payload") != "register_ack":
                    continue
                acks.append((robot, msg.register_ack.accepted))
                if msg.register_ack.accepted:
                    break
                session.control_in.put(pb.AdapterMessage(
                    discover=pb.DiscoverRobot(robot_id=robot)))
                time.sleep(2.0)  # the provisioning loop's next iteration, after _REGISTER_WAIT
                session.control_in.put(pb.AdapterMessage(
                    register=pb.RegisterRobot(robot_id=robot)))

    t = threading.Thread(target=adapter, daemon=True)
    t.start()
    assert drv._handle_registrations() is True
    t.join(timeout=10)
    assert acks == [(A, False), (A, True), (B, False), (B, True)], acks
    for robot in (A, B):
        assert _rows(drv._ctxs[robot]["r"])["C2.1"].status is CheckStatus.PASS, robot


def test_the_reconnect_gives_no_lens_what_the_old_stream_carried(harness_mod, monkeypatch):
    """What the queue held from before the drop is not the first telemetry after the reconnect
    (C6.1), nor the reconciliation of the action the RegisterAck declares (C2.2)."""
    pb = harness_mod.pb
    drv, session, _report = _multi_driver(harness_mod)
    graded = []
    monkeypatch.setattr(drv, "_grade_reconnect",
                        lambda accepted, discovered: graded.append(
                            (drv.robot, [drv.s.control_in.get_nowait() for _ in range(1)])))
    old = pb.AdapterMessage(telemetry=pb.TelemetryPayload(robot_id=B, current_action="old"))
    session.control_in.put(old)
    session.control_in.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
        action_id="t-status-r2", state=pb.ACTION_STATE_SUCCEEDED)))

    def redial():
        time.sleep(0.2)
        session.control_opens += 1
        session.control_in.put(pb.AdapterMessage(hello=pb.AdapterHello(
            protocol_version="fleet_adapter.v1", contract_version="1.4.0")))
        session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(robot_id=A)))
        session.control_in.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(
            robot_id=B, current_action="new")))
        session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(robot_id=B)))
        session.control_in.put(pb.AdapterMessage(telemetry=pb.TelemetryPayload(
            robot_id=A, current_action="new")))

    threading.Thread(target=redial, daemon=True).start()
    drv._check_accepting_reconnect()
    firsts = {robot: msgs[0] for robot, msgs in graded}
    assert firsts[A].telemetry.current_action == "new"
    assert firsts[B].telemetry.current_action == "new", (
        "robot B's lens read a message the old stream carried")


def test_each_unserved_probe_stop_is_cleared(harness_mod, driver):
    """An adapter may latch every robot it drives for a stop it cannot attribute (C20.4, with
    no registration accepted). The harness clears the probe's stop, as a control plane clears
    any stop it issued, so no later row is graded against that latch."""
    drv, session, _report = driver
    drv._contract_ok, drv._reported_contract = True, "1.4.0"
    drv._probe_unserved_stop_before_registration()
    sent = []
    while not session.safety_out.empty():
        sent.append(session.safety_out.get_nowait())
    clears = [m for m in sent if m.WhichOneof("payload") == "estop_clear"]
    assert [(m.robot_id, m.estop_clear.estop_id) for m in clears] == [
        (GHOST, "e-unserved-before-registration")]


def test_a_register_from_a_robot_not_in_the_run_does_not_end_the_refusals(harness_mod):
    pb = harness_mod.pb
    drv, session, _report = _multi_driver(harness_mod)
    for robot in ("robot-elsewhere", A, B):
        session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(robot_id=robot)))
    drv._refuse_registration_version_mismatch()
    acks = []
    while not session.control_out.empty():
        acks.append(session.control_out.get_nowait())
    assert len(acks) == 3, "every register on the refused session is answered, in order"
