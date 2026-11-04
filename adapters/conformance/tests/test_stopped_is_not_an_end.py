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

"""C3.6 accepts every disposition the contract defines, and no row reads STOPPED as an end.

Contract 1.4.0 added ``CANCEL_DISPOSITION_STOP_UNCONFIRMED`` ("the stop was commanded and the
robot's rest could not be witnessed"), and C3.6 still accepted only the four values before it,
so an adapter answering a cancel with it failed a MUST the contract does not make.

``ACTION_STATE_STOPPED`` is the commanded stop and is not terminal (RFC-0001 safety chapter,
*terminal*; the proto's comment). The terminal half of C16.1 and the preemptor's end in C3.9
took it as the end, so an adapter that stopped and never reported the end passed C16.1, and a
preemptor that reported a stop on its way to SUCCEEDED failed C3.9.
"""

from __future__ import annotations

import queue

import pytest

from adapters.conformance.report import CheckStatus


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got {[r.check_id for r in report.results]}")


class _Canceller:
    """Answers the C3.6 cancel of t1 with `disposition`, then reports `after` for t1."""

    def __init__(self, pb, session, disposition, after):
        self.pb, self.session = pb, session
        self.disposition, self.after = disposition, after

    def pump(self):
        pb = self.pb
        while True:
            try:
                msg = self.session.control_out.get_nowait()
            except queue.Empty:
                return
            if msg.WhichOneof("payload") != "command":
                continue
            cmd = msg.command
            res = pb.CommandResult(command_id=cmd.command_id, robot_id=cmd.robot_id)
            if cmd.WhichOneof("command") == "cancel_action":
                res.cancel_action.CopyFrom(pb.CancelActionResult(
                    acknowledged=True, disposition=self.disposition))
            self.session.control_in.put(pb.AdapterMessage(command_result=res))
            for state in self.after:
                self.session.control_in.put(pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
                    action_id="t1", state=state, fencing_token=10)))


@pytest.fixture
def run_cancel(driver, pb, harness_mod, monkeypatch):
    monkeypatch.setattr(harness_mod, "_TERMINAL_STATUS_WAIT", 0.3)
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.05)

    def _run(disposition, *after):
        drv, session, report = driver
        robot = _Canceller(pb, session, disposition, after)
        real = drv._await_result

        def _await(command_id):
            robot.pump()
            return real(command_id)

        drv._await_result = _await
        drv._check_cancel_disposition()
        return _row(report, "C3.6"), drv

    return _run


def test_c36_green_for_stop_unconfirmed(run_cancel, pb):
    row, _ = run_cancel(pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED, pb.ACTION_STATE_CANCELLED)
    assert row.status is CheckStatus.PASS, row.detail
    assert "CANCEL_DISPOSITION_STOP_UNCONFIRMED" in row.detail, row.detail


@pytest.mark.parametrize("name", ["CANCEL_DISPOSITION_UNSPECIFIED",
                                  "CANCEL_DISPOSITION_STOPPED_SAFELY",
                                  "CANCEL_DISPOSITION_COMPLETED",
                                  "CANCEL_DISPOSITION_RECOVERED",
                                  "CANCEL_DISPOSITION_STOP_UNCONFIRMED"])
def test_c36_accepts_every_disposition_the_contract_defines(run_cancel, pb, name):
    row, _ = run_cancel(getattr(pb, name), pb.ACTION_STATE_CANCELLED)
    assert row.status is CheckStatus.PASS, row.detail


def test_c36_valid_set_is_the_contract_enum(run_cancel, pb):
    """Every value of the enum, and nothing outside it: a value the stubs do not define fails."""
    assert set(pb.CancelDisposition.values()) == {
        pb.CANCEL_DISPOSITION_UNSPECIFIED, pb.CANCEL_DISPOSITION_STOPPED_SAFELY,
        pb.CANCEL_DISPOSITION_COMPLETED, pb.CANCEL_DISPOSITION_RECOVERED,
        pb.CANCEL_DISPOSITION_STOP_UNCONFIRMED}, "the contract's enum moved; re-read C3.6"
    row, _ = run_cancel(max(pb.CancelDisposition.values()) + 1, pb.ACTION_STATE_CANCELLED)
    assert row.status is CheckStatus.FAIL, row.detail


def test_c161_terminal_half_skips_the_commanded_stop(run_cancel, pb):
    """STOPPED then CANCELLED: the end C16.1 records is CANCELLED."""
    _, drv = run_cancel(pb.CANCEL_DISPOSITION_STOPPED_SAFELY,
                        pb.ACTION_STATE_STOPPED, pb.ACTION_STATE_CANCELLED)
    assert drv._terminal_status is not None
    assert drv._terminal_status.state == pb.ACTION_STATE_CANCELLED


def test_c161_terminal_half_is_unmet_by_a_stop_alone(run_cancel, pb):
    """A cancelled task that reports the stop and never its end has sent no terminal update."""
    _, drv = run_cancel(pb.CANCEL_DISPOSITION_STOPPED_SAFELY, pb.ACTION_STATE_STOPPED)
    assert drv._terminal_status is None
    drv._record_task_status(
        transition=pb.ActionStatusUpdate(action_id="t-status", state=pb.ACTION_STATE_RUNNING,
                                         fencing_token=14),
        terminal=drv._terminal_status)
    row = _row(drv.r, "C16.1")
    assert row.status is CheckStatus.FAIL, row.detail


def test_no_terminal_set_holds_stopped(harness_mod, pb):
    assert pb.ACTION_STATE_STOPPED not in harness_mod._TERMINAL_ACTION_STATES()
    rfc = getattr(harness_mod, "_RFC_TERMINAL_ACTION_STATES", None)
    assert rfc is not None, "C4.2's terminal set, RFC-0001's *terminal*, is not defined"
    assert set(rfc()) == {pb.ACTION_STATE_SUCCEEDED, pb.ACTION_STATE_FAILED,
                          pb.ACTION_STATE_CANCELLED, pb.ACTION_STATE_UNKNOWN}


def test_c39_preemptor_end_skips_the_commanded_stop(driver, pb, harness_mod, monkeypatch):
    """A preemptor that reports a stop on its way to SUCCEEDED completed: C3.9 reads the end."""
    from adapters.conformance.tests.test_c39 import _Robot

    class _StopsFirst(_Robot):
        def _status(self, action_id, state, message=""):
            if state == pb.ACTION_STATE_SUCCEEDED:
                super()._status(action_id, pb.ACTION_STATE_STOPPED, "at rest")
            super()._status(action_id, state, message)

    monkeypatch.setattr(harness_mod, "_PREEMPTOR_TERMINAL_WAIT", 0.3)
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.05)
    drv, session, report = driver
    robot = _StopsFirst(pb, session, "reads-name")
    real = drv._await_result

    def _await(command_id):
        robot.pump()
        return real(command_id)

    drv._await_result = _await
    drv._check_cancel_names_its_action()
    row = _row(report, "C3.9")
    assert row.status is CheckStatus.PASS, row.detail
    assert "SUCCEEDED" in row.detail, row.detail
