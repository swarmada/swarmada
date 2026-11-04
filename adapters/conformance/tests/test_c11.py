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

"""C11.1 — pause/resume is required, so declining it is a failed MUST.

The catalog: "Declining is NOT conformant: these are in RFC-0001's Required-message
table and absent from its optional-command list". The harness recorded a declined pause
as an ``optional-declined`` skip at MAY, so an adapter that declined it passed the
harness's ``conformant`` -- the boolean the control plane admits on -- while a
maintenance window draining its zone silently did nothing.
"""

from __future__ import annotations

import pytest

from adapters.conformance.report import CheckStatus


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded")


class _Pauser:
    """Answers pause and resume per arm: 'answer', 'decline', or 'silent'."""

    def __init__(self, pb, session, *, pause, resume):
        self.pb, self.session = pb, session
        self.mode = {"pause": pause, "resume": resume}

    def pump(self):
        import queue as _q

        while True:
            try:
                msg = self.session.control_out.get_nowait()
            except _q.Empty:
                return
            if msg.WhichOneof("payload") != "command":
                continue
            cmd = msg.command
            arm = cmd.WhichOneof("command")
            mode = self.mode.get(arm, "silent")
            if mode == "silent":
                continue
            res = self.pb.CommandResult(command_id=cmd.command_id, robot_id=cmd.robot_id)
            if mode == "decline":
                res.unsupported = True
            elif arm == "pause":
                res.pause.paused = True
            else:
                res.resume.resumed = True
            self.session.control_in.put(self.pb.AdapterMessage(command_result=res))


def _drive(drv, pauser, monkeypatch=None, harness_mod=None):
    real = drv._await_result

    def _await(command_id):
        pauser.pump()
        return real(command_id)

    drv._await_result = _await
    drv._check_pause_resume()


def test_c111_a_declined_pause_is_a_failed_must(driver, pb):
    drv, session, report = driver
    _drive(drv, _Pauser(pb, session, pause="decline", resume="decline"))
    row = _row(report, "C11.1")
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.level == "MUST"
    assert row.is_blocking_failure, "a declined required command must block the harness verdict"
    assert not report.conformant


def test_c111_answered_pause_and_resume_passes_at_must(driver, pb):
    drv, session, report = driver
    _drive(drv, _Pauser(pb, session, pause="answer", resume="answer"))
    row = _row(report, "C11.1")
    assert row.status is CheckStatus.PASS, row.detail
    assert row.level == "MUST"


def test_c111_pause_answered_resume_declined_fails_at_must(driver, pb):
    drv, session, report = driver
    _drive(drv, _Pauser(pb, session, pause="answer", resume="decline"))
    row = _row(report, "C11.1")
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.level == "MUST"


def test_c111_no_answer_to_pause_is_a_failed_must(driver, pb, harness_mod, monkeypatch):
    """No CommandResult for a required command is a failure, as it is for scan (C12.1)."""
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.05)
    drv, session, report = driver
    _drive(drv, _Pauser(pb, session, pause="silent", resume="silent"))
    row = _row(report, "C11.1")
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.level == "MUST"


@pytest.mark.parametrize("check_id,want", [("C9.1", "SHOULD")])
def test_c91_is_recorded_at_its_catalog_level(driver, pb, check_id, want):
    """C9.1 is SHOULD in the catalog; the harness recorded MAY on every arm."""
    drv, session, report = driver

    def _answer():
        import queue as _q

        while True:
            try:
                msg = session.control_out.get_nowait()
            except _q.Empty:
                return
            cmd = msg.command
            res = pb.CommandResult(command_id=cmd.command_id, robot_id=cmd.robot_id)
            res.verify.actual_metrics["scanHz"] = 10.0
            session.control_in.put(pb.AdapterMessage(command_result=res))

    real = drv._await_result
    drv._await_result = lambda cid: (_answer(), real(cid))[1]
    drv._check_verify_metrics()
    assert _row(report, check_id).level == want
