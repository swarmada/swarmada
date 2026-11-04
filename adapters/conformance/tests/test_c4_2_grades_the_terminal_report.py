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

"""C4.2 grades the lapsed lease's terminal report, never ``ACTION_STATE_STOPPED``.

The catalog: "An ``ACTION_STATE_STOPPED`` report says the stop was commanded; it is not
terminal ... The adapter then sends the action's terminal report (RFC-0001 safety chapter,
*terminal*) ... That terminal report is what this check grades." RFC-0001 names
``SUCCEEDED``, ``FAILED``, ``CANCELLED`` and ``UNKNOWN`` terminal and ``STOPPED`` not, and it
names no terminal state for a lease self-stop, so the row accepts any of the four and
requires none in particular. The harness counted ``STOPPED`` as the end, so an adapter that
sent only the commanded stop passed.
"""

from __future__ import annotations

import queue

import pytest

from adapters.conformance.report import CheckStatus

_LEASE_ACTION = "t-lease-expiry"


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got {[r.check_id for r in report.results]}")


class _Robot:
    """Accepts the lease-bearing assignment and, once it lapses, reports `after` in order.

    Every other assignment is refused, so C4.1, C4.3 and C4.4 record skips and this file
    grades C4.2 alone.
    """

    def __init__(self, pb, session, after):
        self.pb, self.session, self.after = pb, session, after

    def _status(self, state):
        self.session.control_in.put(self.pb.AdapterMessage(action_status=self.pb.ActionStatusUpdate(
            action_id=_LEASE_ACTION, state=state, fencing_token=11)))

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
            if cmd.WhichOneof("command") == "assign_action":
                ok = cmd.assign_action.action_id == _LEASE_ACTION
                token = cmd.assign_action.fencing_token if ok else 0
                res.assign_action.CopyFrom(pb.AssignActionResult(
                    accepted=ok, accepted_fencing_token=token))
                self.session.control_in.put(pb.AdapterMessage(command_result=res))
                if ok:
                    self._status(pb.ACTION_STATE_RUNNING)
                    for state in self.after:
                        self._status(state)
                continue
            self.session.control_in.put(pb.AdapterMessage(command_result=res))


@pytest.fixture
def run_c42(driver, pb, harness_mod, monkeypatch):
    # The window is a liveness bound for a real robot; a test double answers at once.
    # raising=False: the constant is new with this change, and the red run predates it.
    monkeypatch.setattr(harness_mod, "_LEASE_END_WAIT", 0.3, raising=False)
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.05)

    def _run(*after):
        drv, session, report = driver
        robot = _Robot(pb, session, after)
        real = drv._await_result

        def _await(command_id):
            robot.pump()
            return real(command_id)

        drv._await_result = _await
        drv._check_leases()
        return _row(report, "C4.2")

    return _run


def test_c42_red_when_the_adapter_sends_only_the_commanded_stop(run_c42, pb):
    """STOPPED alone is the stop commanded, and no end: what the catalog's row fails."""
    row = run_c42(pb.ACTION_STATE_STOPPED)
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.is_blocking_failure
    assert "ACTION_STATE_STOPPED" in row.detail and "not terminal" in row.detail, row.detail


def test_c42_red_when_nothing_is_reported(run_c42):
    row = run_c42()
    assert row.status is CheckStatus.FAIL, row.detail
    assert "no terminal action_status" in row.detail, row.detail


def test_c42_green_for_stopped_then_failed(run_c42, pb):
    """The reference adapters' report: the commanded stop, then the end."""
    row = run_c42(pb.ACTION_STATE_STOPPED, pb.ACTION_STATE_FAILED)
    assert row.status is CheckStatus.PASS, row.detail
    assert row.detail.startswith("reported ACTION_STATE_FAILED"), row.detail
    assert "ACTION_STATE_STOPPED" in row.detail, "the detail names the uncounted commanded stop"


@pytest.mark.parametrize("terminal", ["ACTION_STATE_SUCCEEDED", "ACTION_STATE_FAILED",
                                      "ACTION_STATE_CANCELLED", "ACTION_STATE_UNKNOWN"])
def test_c42_accepts_every_state_rfc_0001_calls_terminal(run_c42, pb, terminal):
    """RFC-0001 names no terminal state for a lease self-stop, so the row names none either."""
    row = run_c42(pb.ACTION_STATE_STOPPED, getattr(pb, terminal))
    assert row.status is CheckStatus.PASS, row.detail
    assert row.detail.startswith(f"reported {terminal}"), row.detail


def test_c42_green_for_a_terminal_report_with_no_stopped_before_it(run_c42, pb):
    """The row grades the terminal report; a STOPPED before it is the adapter's to send."""
    row = run_c42(pb.ACTION_STATE_FAILED)
    assert row.status is CheckStatus.PASS, row.detail
    assert "after ACTION_STATE_STOPPED" not in row.detail, row.detail


@pytest.mark.parametrize("not_terminal", ["ACTION_STATE_RUNNING", "ACTION_STATE_PAUSED"])
def test_c42_red_for_the_other_states_that_are_not_terminal(run_c42, pb, not_terminal):
    row = run_c42(getattr(pb, not_terminal))
    assert row.status is CheckStatus.FAIL, row.detail


def test_c42_window_covers_the_graded_adapters_stopping_budget(harness_mod):
    """The terminal report now follows rest, so the window holds the lease, the lapse's
    detection and a stopping budget. The camera adapter looks at its lease every 0.2 s and
    confirms rest within 2.6 s; MP1 looks every 0.05 s and confirms rest in at most 40 looks
    0.05 s apart (about 2 s); the 500 ms lease comes first."""
    camera = 0.5 + 0.2 + 2.6
    mp1 = 0.5 + 0.05 + 40 * 0.05
    assert harness_mod._LEASE_END_WAIT >= max(camera, mp1)
