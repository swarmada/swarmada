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

"""C5.8: a stop holds the robot's work until its EstopClear lifts it.

Why this file exists. RFC-0001 #fleet-adapter-protocol-the-stop-latch binds a 1.4.0
connection: after a stop, every AssignAction is refused ``ROBOT_UNAVAILABLE`` until the
stop's EstopClear takes effect, and the adapter answers that clear with an EstopClearAck.
No row graded it, so an adapter that reported 1.4.0 and kept no latch passed the suite.

Each test plays the adapter from a thread that answers what the harness sends, by a
policy: one that keeps the latch, and one per way of breaking it. Every test fails on the
harness before the row existed.
"""

from __future__ import annotations

import queue
import threading

import pytest

from adapters.conformance.report import CheckStatus

ROBOT = "robot-conformance-1"


@pytest.fixture
def quick(harness_mod, monkeypatch):
    """Short waits: the scripted adapter answers at once, and silence is an answer."""
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.6)
    monkeypatch.setattr(harness_mod, "_CLEAR_WAIT", 0.6, raising=False)


class _Adapter:
    """Answers the harness by a policy, until stopped.

    ``latches``: a stop latches its ID and the robot is refused work while any is latched.
    ``acks_clear``: an EstopClear for a fenced ID is answered with an EstopClearAck.
    ``lifts``: that clear lifts the ID. ``refusal``: the rejection used under the latch.
    """

    def __init__(self, pb, session, *, latches=True, acks_clear=True, lifts=True,
                 refusal=None) -> None:
        self.pb, self.s = pb, session
        self.latches, self.acks_clear, self.lifts = latches, acks_clear, lifts
        self.refusal = refusal or pb.ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE
        self.latched: set[str] = set()
        self.fenced: set[str] = set()
        self.assigned: list[tuple[str, bool]] = []
        self.clears: list = []
        self._stop = threading.Event()
        self._threads = [threading.Thread(target=self._control, daemon=True),
                         threading.Thread(target=self._safety, daemon=True)]

    def __enter__(self):
        for t in self._threads:
            t.start()
        return self

    def __exit__(self, *exc):
        self._stop.set()
        for t in self._threads:
            t.join(timeout=2)

    def _control(self) -> None:
        pb = self.pb
        while not self._stop.is_set():
            try:
                msg = self.s.control_out.get(timeout=0.05)
            except queue.Empty:
                continue
            if not hasattr(msg, "WhichOneof") or msg.WhichOneof("payload") != "command":
                continue
            cmd = msg.command
            arm = cmd.WhichOneof("command")
            result = pb.CommandResult(command_id=cmd.command_id, robot_id=ROBOT)
            if arm == "estop":
                self.fenced.add(cmd.estop.estop_id)
                if self.latches:
                    self.latched.add(cmd.estop.estop_id)
                continue  # a fence needs no answer on ControlStream
            if arm == "assign_action":
                held = self.latches and bool(self.latched)
                self.assigned.append((cmd.assign_action.action_id, not held))
                result.assign_action.CopyFrom(
                    pb.AssignActionResult(accepted=False, rejection=self.refusal) if held
                    else pb.AssignActionResult(
                        accepted=True, accepted_fencing_token=cmd.assign_action.fencing_token))
            elif arm == "cancel_action":
                result.cancel_action.CopyFrom(pb.CancelActionResult(
                    acknowledged=True, disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY))
            self.s.control_in.put(pb.AdapterMessage(command_result=result))

    def _safety(self) -> None:
        pb = self.pb
        while not self._stop.is_set():
            try:
                msg = self.s.safety_out.get(timeout=0.05)
            except queue.Empty:
                continue
            if not hasattr(msg, "WhichOneof"):
                continue
            arm = msg.WhichOneof("payload")
            if arm == "estop":
                if self.latches:
                    self.latched.add(msg.estop.estop_id)
                self.s.safety_in.put(pb.AdapterSafetyMessage(robot_id=ROBOT, estop_ack=pb.EstopAck(
                    estop_id=msg.estop.estop_id, state=pb.ESTOP_STATE_STOPPED,
                    stop_initiated_at=1)))
            elif arm == "estop_clear":
                self.clears.append(msg.estop_clear)
                estop_id = msg.estop_clear.estop_id
                if estop_id not in self.fenced:
                    continue
                if self.lifts:
                    self.latched.discard(estop_id)
                if self.acks_clear:
                    self.s.safety_in.put(pb.AdapterSafetyMessage(
                        robot_id=ROBOT, estop_clear_ack=pb.EstopClearAck(estop_id=estop_id)))


def _row(report):
    rows = {r.check_id: r for r in report.results}
    assert "C5.8" in rows, sorted(rows)
    return rows["C5.8"]


def _at(drv, version: str) -> None:
    drv._contract_ok = True
    drv._reported_contract = version


def test_c5_8_passes_a_robot_held_until_its_clear_lifts_it(pb, driver, quick):
    drv, session, report = driver
    _at(drv, "1.4.0")
    with _Adapter(pb, session) as adapter:
        drv._check_latch_holds_work()
    row = _row(report)
    assert row.status is CheckStatus.PASS, row.detail
    assert row.level == "MUST"
    assert [ok for _, ok in adapter.assigned] == [False, True], adapter.assigned
    assert adapter.clears and adapter.clears[0].leader_term >= 1
    assert adapter.latched == set(), "the probe's stop is left lifted"


def test_c5_8_fails_an_adapter_that_keeps_no_latch(pb, driver, quick):
    """The planted shape the ruling names: 1.4.0 reported, no latch kept."""
    drv, session, report = driver
    _at(drv, "1.4.0")
    with _Adapter(pb, session, latches=False, acks_clear=False):
        drv._check_latch_holds_work()
    row = _row(report)
    assert row.status is CheckStatus.FAIL, row.detail
    assert "accepted" in row.detail and "e-latch" in row.detail, row.detail


def test_c5_8_fails_an_adapter_that_never_answers_the_clear(pb, driver, quick):
    drv, session, report = driver
    _at(drv, "1.4.0")
    with _Adapter(pb, session, acks_clear=False, lifts=False):
        drv._check_latch_holds_work()
    row = _row(report)
    assert row.status is CheckStatus.FAIL, row.detail
    assert "EstopClearAck" in row.detail, row.detail


def test_c5_8_fails_an_adapter_that_acks_the_clear_and_keeps_the_latch(pb, driver, quick):
    drv, session, report = driver
    _at(drv, "1.4.0")
    with _Adapter(pb, session, lifts=False):
        drv._check_latch_holds_work()
    row = _row(report)
    assert row.status is CheckStatus.FAIL, row.detail
    assert "EstopClearAck" in row.detail and "refused" in row.detail, row.detail


def test_c5_8_fails_a_refusal_under_the_latch_that_is_not_robot_unavailable(pb, driver, quick):
    """RFC-0001: refused "with accepted = false and ASSIGN_ACTION_REJECTION_ROBOT_UNAVAILABLE,
    whatever its fencing token"."""
    drv, session, report = driver
    _at(drv, "1.4.0")
    with _Adapter(pb, session, refusal=pb.ASSIGN_ACTION_REJECTION_STALE_FENCING_TOKEN):
        drv._check_latch_holds_work()
    row = _row(report)
    assert row.status is CheckStatus.FAIL, row.detail
    assert "ROBOT_UNAVAILABLE" in row.detail, row.detail


def test_c5_8_below_1_4_0_sends_nothing_and_records_why(pb, driver, quick):
    """The latch binds a 1.4.0 connection, and a 1.3.x one is sent no EstopClear."""
    drv, session, report = driver
    _at(drv, "1.3.0")
    with _Adapter(pb, session) as adapter:
        drv._check_latch_holds_work()
    row = _row(report)
    assert row.status is CheckStatus.SKIP, row.detail
    assert row.detail.startswith("contract-predates-obligation"), row.detail
    assert adapter.assigned == [] and adapter.clears == [] and adapter.fenced == set()


def test_c5_8_runs_after_the_accepting_reconnect_and_before_the_refused_session(
        pb, driver, monkeypatch):
    """Tokens 24 and 25 are above the cadence drive's 23 and below the refused session's 99,
    and no stop of its own is left latched for the refused session's checks."""
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
    assert "_check_latch_holds_work" in calls, calls
    i = calls.index("_check_latch_holds_work")
    assert calls[i - 1] == "_check_accepting_reconnect", calls
    assert calls[i + 1] == "_check_negotiation_refusal", calls
