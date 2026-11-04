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

"""The harness negotiates the contract version per connection, as a control plane does.

RFC-0001 #fleet-adapter-protocol-package-and-versioning: "A compatible connection negotiates
1.4.0 when the adapter reported a 1.4 version, and the version the adapter reported otherwise, so
a 1.3.x adapter's connection negotiates 1.3.x and is held to it: it is sent every stop, and no
AssignAction." The harness told every compatible adapter 1.4.0, so no row ever ran on a 1.3.x
connection. It now agrees 1.3.x with a 1.3 adapter, sends it no AssignAction, records
each row that needs one ``contract-predates-obligation``, and still grades the rules that bind
every adapter there: a 1.3.x adapter that does not answer a stop fails.
"""

from __future__ import annotations

import queue

import pytest

from adapters.conformance.report import CheckStatus, SkipCause


def _at(drv, version: str) -> None:
    drv._contract_ok = True
    drv._reported_contract = version


def _predates(detail: str) -> bool:
    return detail.split(":", 1)[0] == SkipCause.CONTRACT_PREDATES_OBLIGATION.value


def _rows(report):
    return {r.check_id: r for r in report.results}


def _assigns(sent, session) -> list:
    return [m for m in sent(session.control_out)
            if m.WhichOneof("payload") == "command"
            and m.command.WhichOneof("command") == "assign_action"]


@pytest.fixture
def quick(harness_mod, monkeypatch):
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.4)
    monkeypatch.setattr(harness_mod, "_CLEAR_WAIT", 0.3, raising=False)
    monkeypatch.setattr(harness_mod, "_ESTOP_TERMINAL_BUDGET_S", 0.3)


@pytest.mark.parametrize("reported, agreed", [
    ("1.3.0", "1.3.0"), ("1.3.2", "1.3.2"), ("1.4.0", "1.4.0"), ("1.4.7", "1.4.0"),
    ("01.03.00", "1.3.0"), ("1.5.0", ""), ("1.2.9", ""), ("", ""), ("1.3", "")])
def test_the_version_agreed_for_a_connection(harness_mod, reported, agreed):
    assert harness_mod._negotiate(reported) == agreed


@pytest.mark.parametrize("reported, agreed",
                         [("1.3.0", "1.3.0"), ("1.4.2", "1.4.0"), ("2.0.0", "")])
def test_the_hello_ack_states_the_version_agreed_for_the_connection(pb, driver, reported, agreed):
    drv, _session, _report = driver
    ack = drv._hello_ack(reported)
    assert ack.accepted
    assert ack.negotiated_contract_version == agreed


def test_a_1_3_connection_is_sent_no_assign_action_and_its_dispatch_rows_do_not_bind_it(
        pb, driver, sent, quick):
    drv, session, report = driver
    _at(drv, "1.3.0")
    for check in (drv._check_catalog_enforced, drv._check_fencing, drv._check_cancel_disposition,
                  drv._check_leases, drv._check_action_status, drv._check_cancel_names_its_action):
        drv._when_served(check)
    assert _assigns(sent, session) == [], "an AssignAction went to a connection held to 1.3.0"
    rows = _rows(report)
    for cid in ("C19.1", "C3.2", "C3.3", "C3.4", "C3.5", "C3.6", "C4.1", "C4.2", "C16.1", "C6.4",
                "C3.9"):
        assert rows[cid].status is CheckStatus.SKIP, (cid, rows[cid])
        assert _predates(rows[cid].detail), rows[cid].detail
        assert "1.3.0" in rows[cid].detail


def test_c17_on_a_1_3_connection_grades_the_declaration_and_sends_no_assignment(
        pb, driver, sent, quick):
    drv, session, report = driver
    _at(drv, "1.3.0")
    drv._honoured_subjects = []
    drv._check_zone_restrictions()
    assert _assigns(sent, session) == []
    rows = _rows(report)
    assert not _predates(rows["C17.3"].detail), rows["C17.3"]
    for cid in ("C17.1", "C17.2", "C17.4"):
        assert _predates(rows[cid].detail), (cid, rows[cid])


def test_a_1_4_connection_is_still_given_work(pb, driver, sent, quick):
    """The control: at 1.4.0 the same check puts its assignments on the wire."""
    drv, session, _report = driver
    _at(drv, "1.4.0")
    try:
        drv._when_served(drv._check_fencing)
    except queue.Empty:
        pass  # nothing answers here; what matters is what went on the wire
    assert _assigns(sent, session), "a 1.4.0 connection was sent no AssignAction"


def test_a_1_3_adapter_that_does_not_answer_a_stop_still_fails(pb, driver, quick):
    """A row graded on a 1.3.x connection fails an adapter that breaks a
    rule *Version* binds on it. Every stop goes to every connection, and must be answered."""
    drv, session, report = driver
    _at(drv, "1.3.0")
    drv._check_estop()  # the adapter answers nothing on SafetyStream
    row = _rows(report)["C5.1"]
    assert row.status is CheckStatus.FAIL, row


def test_the_redial_is_agreed_its_own_version(pb, driver, sent, monkeypatch, quick):
    """The accepting reconnect's HelloAck is agreed per connection too; and on a connection held
    to 1.3.x neither C2.2 nor C3.8 is graded, and the cadence drive sends no assignment."""
    drv, session, report = driver
    _at(drv, "1.3.0")
    real_drop = session.drop_control

    def _drop():
        real_drop()
        session.control_opens += 1

    session.drop_control = _drop
    session.control_in.put(pb.AdapterMessage(hello=pb.AdapterHello(
        protocol_version="fleet_adapter.v1", contract_version="1.3.0")))
    session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(robot_id="robot-conformance-1")))
    monkeypatch.setattr(drv, "_await_telemetry", lambda timeout=0.0: None)
    monkeypatch.setattr(drv, "_await_action_status", lambda *a, **k: None)
    drv._check_accepting_reconnect()

    msgs = sent(session.control_out)
    acks = [m.hello_ack for m in msgs if m.WhichOneof("payload") == "hello_ack"]
    assert [a.negotiated_contract_version for a in acks] == ["1.3.0"], acks
    assert not [m for m in msgs if m.WhichOneof("payload") == "command"
                and m.command.WhichOneof("command") == "assign_action"]
    rows = _rows(report)
    for cid in ("C2.2", "C3.8"):
        assert _predates(rows[cid].detail), (cid, rows[cid])
    assert "C2.3" in rows


def test_the_refused_session_sends_no_durability_probe_to_a_1_3_adapter(pb, driver, sent, quick):
    """C3.1 probes a mark raised by assignments accepted before the reconnect; a connection held
    to 1.3.x was sent none, so the probe is not sent and the row does not bind it."""
    drv, session, report = driver
    _at(drv, "1.3.0")
    session.control_reconnected.set()
    session.control_in.put(pb.AdapterMessage(hello=pb.AdapterHello(
        protocol_version="fleet_adapter.v1", contract_version="1.3.0")))
    try:
        drv._check_negotiation_refusal()
    except queue.Empty:
        pass
    rows = _rows(report)
    assert _predates(rows["C3.1"].detail), rows["C3.1"]
    probes = [m for m in sent(session.control_out) if m.WhichOneof("payload") == "command"
              and m.command.WhichOneof("command") == "assign_action"
              and m.command.assign_action.action_id == "t-persist"]
    assert probes == [], probes
