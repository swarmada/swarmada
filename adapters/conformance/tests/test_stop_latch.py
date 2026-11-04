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

"""The harness drives the stop latch as RFC-0001 states it, and grades what it was asked to.

Why this file exists. A 1.4.0 adapter that keeps the stop latch
(RFC-0001 #fleet-adapter-protocol-the-stop-latch) refuses every assignment after a stop
until the stop is cleared, reports its latched stops and its local latch on SafetyStream,
and may answer a fence with a receipt on ControlStream. The harness sent no clear, read the
first SafetyStream message as each stop's answer, ran C8.3's edge stop (which no harness
message can lift) before the checks that give the robot work, and failed any fence receipt.
So it failed C5.2, C5.4, C17.1, C17.2, C17.4, C3.8, C3.1 and C14.1 for an adapter that met
the latch, and skipped C3.9.

Each test below fails on the harness before the fix. The ones named ``..._still_fails_...``
pass on both: they show a fixed check still fails an adapter that answers wrongly.
"""

from __future__ import annotations

import queue

import pytest

from adapters.conformance.report import CheckStatus


def _rows(report):
    return {r.check_id: r for r in report.results}


def _drain(q: queue.Queue) -> list:
    out = []
    while True:
        try:
            item = q.get_nowait()
        except queue.Empty:
            return out
        if hasattr(item, "WhichOneof"):
            out.append(item)


def _ack(pb, estop_id, state=None, **kw):
    return pb.AdapterSafetyMessage(robot_id="robot-conformance-1", estop_ack=pb.EstopAck(
        estop_id=estop_id, state=state or pb.ESTOP_STATE_STOPPED, **kw))


def _clear_ack(pb, estop_id):
    return pb.AdapterSafetyMessage(robot_id="robot-conformance-1",
                                   estop_clear_ack=pb.EstopClearAck(estop_id=estop_id))


def _local_report(pb, latched=False, set_id=""):
    return pb.AdapterSafetyMessage(robot_id="robot-conformance-1", local_stop=pb.LocalStopReport(
        latched=latched, set_id=set_id, source="robot"))


def _at(drv, version: str) -> None:
    drv._contract_ok = True
    drv._reported_contract = version


@pytest.fixture
def quick(harness_mod, monkeypatch):
    """Short waits: these tests play the adapter by hand, and silence is the answer."""
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.4)
    monkeypatch.setattr(harness_mod, "_CLEAR_WAIT", 0.3, raising=False)
    monkeypatch.setattr(harness_mod, "_ESTOP_TERMINAL_BUDGET_S", 0.3)


# ── C5 reads its answer by arm and stop ID ────────────────────────────────────────

def test_c5_reads_the_ack_for_its_stop_past_a_local_latch_report(pb, driver, quick):
    """A 1.4.0 adapter sends a LocalStopReport on each SafetyStream that opens, and with each
    refusal an EstopAck for every latched stop. Either can wait ahead of C5's answer."""
    drv, session, report = driver
    session.safety_in.put(_local_report(pb))
    session.safety_in.put(_ack(pb, "e-earlier"))
    session.safety_in.put(_ack(pb, "e1", stop_initiated_at=1))
    drv._check_estop()
    rows = _rows(report)
    for cid in ("C5.1", "C5.2", "C5.3", "C5.5"):
        assert rows[cid].status is CheckStatus.PASS, (cid, rows[cid].detail)
    assert "local_stop" in rows["C5.5"].detail, rows["C5.5"].detail


def test_c5_still_fails_an_ack_that_names_another_stop(pb, driver, quick):
    """Matching by stop ID must not turn a wrong answer into no answer: C5.1, C5.2 and C5.5
    fail, and C5.5 no longer times an ack that does not answer the stop."""
    drv, session, report = driver
    session.safety_in.put(_ack(pb, "e-other", stop_initiated_at=1))
    drv._check_estop()
    rows = _rows(report)
    for cid in ("C5.1", "C5.2", "C5.5"):
        assert rows[cid].status is CheckStatus.FAIL, (cid, rows[cid].detail)
        assert "e-other" in rows[cid].detail


# ── every stop the harness issues is cleared, at 1.4.0 only ──────────────────────

def test_at_1_4_0_each_stop_is_fenced_cleared_and_its_lift_awaited(pb, driver, quick):
    drv, session, report = driver
    _at(drv, "1.4.0")
    session.safety_in.put(_ack(pb, "e1", stop_initiated_at=1))
    session.safety_in.put(_clear_ack(pb, "e1"))
    session.safety_in.put(_ack(pb, "e-control-probe", stop_initiated_at=1))
    session.safety_in.put(_clear_ack(pb, "e-control-probe"))
    drv._check_estop()

    safety = _drain(session.safety_out)
    clears = [m.estop_clear for m in safety if m.WhichOneof("payload") == "estop_clear"]
    assert [c.estop_id for c in clears] == ["e1", "e-control-probe"], safety
    assert all(c.leader_term >= 1 for c in clears), "an EstopClear must carry the leader term"
    fences = [m.command for m in _drain(session.control_out)
              if m.WhichOneof("payload") == "command"
              and m.command.WhichOneof("command") == "estop"]
    # The clear lifts a stop only once its fence has arrived: e1 gets one, as row S1 sends it.
    assert [f.estop.estop_id for f in fences] == ["e1", "e-control-probe"], fences
    assert all(f.leader_term >= 1 for f in fences)
    assert drv._unlifted == []
    assert _rows(report)["C5.4"].status is CheckStatus.PASS


def test_below_1_4_0_no_clear_is_sent(pb, driver, quick):
    """RFC-0001 The Stop Latch, Version: the control plane sends a 1.3.x connection no
    EstopClear. The harness must not either."""
    drv, session, _report = driver
    _at(drv, "1.3.0")
    session.safety_in.put(_ack(pb, "e1", stop_initiated_at=1))
    drv._check_estop()
    safety = _drain(session.safety_out)
    assert not [m for m in safety if m.WhichOneof("payload") == "estop_clear"], safety
    assert drv._unlifted == []


def test_a_clear_with_no_ack_is_named_and_c17_1_still_fails_the_refusal(pb, driver, quick):
    """An adapter that latches and never lifts: the harness cleared the stop as a control
    plane does, so C17.1's ROBOT_UNAVAILABLE is the adapter's, and the row still fails."""
    drv, session, report = driver
    _at(drv, "1.4.0")
    session.safety_in.put(_ack(pb, "e1", stop_initiated_at=1))
    drv._check_estop()
    assert drv._unlifted == ["e1", "e-control-probe"]

    _drain(session.control_out)
    cid = drv._next_command_id
    session.control_in.put(pb.AdapterMessage(command_result=pb.CommandResult(
        command_id=cid, assign_action=pb.AssignActionResult(
            accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE))))
    status, detail = drv._probe_operating_constraints(["camera"], 17, "t-operating-core")
    assert status is CheckStatus.FAIL
    assert "e1" in detail and "EstopClearAck" in detail, detail


# ── C5.4: a fence receipt is allowed; an ack carried only on ControlStream is not ───

def _receipt(pb, command_id, estop_id):
    return pb.AdapterMessage(command_result=pb.CommandResult(
        command_id=command_id, estop=pb.EstopAck(estop_id=estop_id,
                                                 state=pb.ESTOP_STATE_STOPPED)))


def test_c5_4_passes_a_receipt_beside_the_safety_stream_ack(pb, driver, quick):
    drv, session, report = driver
    probe_cid = drv._next_command_id
    session.control_in.put(_receipt(pb, probe_cid, "e-control-probe"))
    session.safety_in.put(_ack(pb, "e-control-probe"))
    drv._check_fence_ack()
    row = _rows(report)["C5.4"]
    assert row.status is CheckStatus.PASS, row.detail
    assert "receipt" in row.detail


def test_c5_4_still_fails_a_receipt_with_no_safety_stream_ack(pb, driver, quick):
    drv, session, report = driver
    session.control_in.put(_receipt(pb, drv._next_command_id, "e-control-probe"))
    drv._check_fence_ack()
    row = _rows(report)["C5.4"]
    assert row.status is CheckStatus.FAIL, row.detail


def test_c5_4_keeps_traffic_it_does_not_read(pb, driver, quick):
    """The old probe drained ControlStream and dropped what it found, an ActionStatusUpdate
    included. Traffic that is not the probe's answer is put back for the next reader."""
    drv, session, _report = driver
    status = pb.AdapterMessage(action_status=pb.ActionStatusUpdate(action_id="t-status"))
    session.control_in.put(status)
    drv._check_fence_ack()
    assert drv._await_action_status("t-status", timeout=0.2) is not None


# ── C8.3 last, read by stop ID ─────────────────────────────────────────────────────

def test_c8_sends_no_edge_stop_until_c8_3_runs_last(pb, driver, quick):
    drv, session, report = driver
    session.edge_established.set()
    session.edge_in.put(pb.AdapterEdgeMessage(position=pb.PositionFrame(robot_id="r")))
    drv._check_edge(advertised_edge=True)
    rows = _rows(report)
    assert rows["C8.1"].status is CheckStatus.PASS and rows["C8.2"].status is CheckStatus.PASS
    assert "C8.3" not in rows, "C8.3 is graded last, after every check that gives work"
    assert _drain(session.edge_out) == [], "no edge stop before the checks that need the robot"

    session.edge_in.put(pb.AdapterEdgeMessage(estop_ack=pb.EstopAck(
        estop_id="other", state=pb.ESTOP_STATE_FAILED)))
    session.edge_in.put(pb.AdapterEdgeMessage(estop_ack=pb.EstopAck(
        estop_id="edge-e1", state=pb.ESTOP_STATE_STOPPED)))
    drv._check_edge_stop()
    assert _rows(report)["C8.3"].status is CheckStatus.PASS


def test_run_ends_with_the_refused_session_then_c8_3(pb, driver, monkeypatch):
    drv, session, _report = driver
    calls = []
    for name in [n for n in dir(drv) if n.startswith("_check_")] + [
            "_record_unverified", "_maybe_handle_registration"]:
        monkeypatch.setattr(drv, name, lambda *a, _n=name, **k: calls.append(_n))
    session.control_established.set()
    session.safety_established.set()
    session.control_in.put(pb.AdapterMessage(hello=pb.AdapterHello(
        protocol_version="fleet_adapter.v1", contract_version="1.4.0")))
    drv.run()
    assert calls[-3:] == ["_check_negotiation_refusal", "_check_edge_stop",
                          "_record_unverified"], calls


# ── the refused session: C3.1 and C13.2 before C14.1's stop, which cannot be cleared ─

def test_c3_1_and_c13_2_are_probed_before_c14_1_stops_the_robot(pb, driver, quick,
                                                                monkeypatch):
    drv, session, report = driver
    sent_before_c14_1 = []
    real = drv._check_estop_after_refusal

    def c14_1():
        sent_before_c14_1.extend(m.command.assign_action.action_id
                                 for m in _drain(session.control_out)
                                 if m.WhichOneof("payload") == "command")
        real()

    monkeypatch.setattr(drv, "_check_estop_after_refusal", c14_1)
    session.control_reconnected.set()
    session.control_in.put(pb.AdapterMessage(hello=pb.AdapterHello()))
    session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(
        robot_id="robot-conformance-1")))
    for cid, rejection in ((1, pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN),
                           (2, pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE)):
        session.control_in.put(pb.AdapterMessage(command_result=pb.CommandResult(
            command_id=cid, assign_action=pb.AssignActionResult(
                accepted=False, rejection=rejection))))
    session.safety_in.put(_ack(pb, "e-refused"))
    drv._check_negotiation_refusal()
    assert sent_before_c14_1 == ["t-persist", "t-refused"], sent_before_c14_1
    rows = _rows(report)
    for cid in ("C3.1", "C13.2", "C14.1"):
        assert rows[cid].status is CheckStatus.PASS, (cid, rows[cid].detail)


def test_c14_1_reads_the_ack_for_its_stop_past_latch_reports(pb, driver, quick):
    drv, session, report = driver
    session.safety_in.put(_local_report(pb))
    session.safety_in.put(_ack(pb, "e1"))
    session.safety_in.put(_ack(pb, "e-refused"))
    drv._check_estop_after_refusal()
    row = _rows(report)["C14.1"]
    assert row.status is CheckStatus.PASS, row.detail


def test_c14_1_does_not_take_a_fence_ack_as_its_answer(pb, driver, quick):
    """C5.4's fence is answered with an EstopAck for its own stop ID on SafetyStream. Waiting
    there ahead of C14.1's answer, it is the fence's answer, not the later stop's."""
    drv, session, report = driver
    session.safety_in.put(_ack(pb, "e-control-probe"))
    session.safety_in.put(_ack(pb, "e-refused", state=pb.ESTOP_STATE_STOPPING))
    session.safety_in.put(_ack(pb, "e-control-probe"))
    session.safety_in.put(_ack(pb, "e-refused"))
    drv._check_estop_after_refusal()
    row = _rows(report)["C14.1"]
    assert row.status is CheckStatus.PASS, row.detail


def test_c14_1_still_fails_when_only_a_fence_ack_comes(pb, driver, quick):
    """The planted shape: the stop is answered with the fence's stop ID and never its own."""
    drv, session, report = driver
    session.safety_in.put(_ack(pb, "e-control-probe"))
    drv._check_estop_after_refusal()
    row = _rows(report)["C14.1"]
    assert row.status is CheckStatus.FAIL, row.detail
    assert "e-refused" in row.detail and "e-control-probe" in row.detail, row.detail


def test_c14_1_still_fails_when_no_ack_names_its_stop(pb, driver, quick):
    drv, session, report = driver
    session.safety_in.put(_ack(pb, "e1"))
    drv._check_estop_after_refusal()
    row = _rows(report)["C14.1"]
    assert row.status is CheckStatus.FAIL and "e-refused" in row.detail, row.detail


# ── the leader term ───────────────────────────────────────────────────────────────

def test_every_command_carries_the_leader_term(pb, driver):
    """RFC-0001 The Stop Latch, The leader term: the control plane MUST set the sending
    leader's term on every Command. 0 means no term."""
    drv, session, _report = driver
    drv._send_command(scan=pb.ScanCapabilities())
    (msg,) = _drain(session.control_out)
    assert msg.command.leader_term >= 1
