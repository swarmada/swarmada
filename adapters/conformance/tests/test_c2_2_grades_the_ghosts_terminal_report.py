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

"""C2.2 grades the ghost's terminal report, never ``ACTION_STATE_STOPPED``.

On the accepting reconnect the harness's ``RegisterAck`` declares the action the adapter was
running ``CANCELLED``, and the adapter MUST reconcile to it: halt, and report the action's end.
The harness waited for ``CANCELLED``, ``STOPPED`` or ``UNKNOWN`` and passed on the first, so an
adapter that sent only ``STOPPED`` -- the commanded stop, which RFC-0001 does not call terminal
-- passed, and one that ended the action ``FAILED`` or ``SUCCEEDED`` failed. The row now grades
as C4.2 does: any state RFC-0001 calls terminal (``SUCCEEDED``, ``FAILED``, ``CANCELLED``,
``UNKNOWN``) passes, and a ``STOPPED`` is named when it comes first and never counted. A
``STOPPED`` held from before the reconnect is not the reconciliation.
"""

from __future__ import annotations

import pytest

from adapters.conformance.report import CheckStatus

_GHOST = "t-status"  # the action the RegisterAck declares CANCELLED


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got {[r.check_id for r in report.results]}")


@pytest.fixture
def run_c22(driver, pb, harness_mod, monkeypatch):
    """Fault the stream, redial and register, then report ``after`` for the ghost.

    ``before`` puts reports for the ghost on the harness's held messages first, as reports
    that arrived before the stream was faulted would be.
    """
    # The window is a liveness bound for a real robot; a test double answers at once.
    monkeypatch.setattr(harness_mod, "_TERMINAL_STATUS_WAIT", 0.3)
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.05)

    def _status(state):
        return pb.AdapterMessage(action_status=pb.ActionStatusUpdate(
            action_id=_GHOST, state=state, fencing_token=14))

    def _run(*after, before=()):
        drv, session, report = driver
        drv._contract_ok = True
        drv._reported_contract = "1.4.0"
        real_drop = session.drop_control

        def _drop():
            real_drop()
            session.control_opens += 1

        session.drop_control = _drop
        drv._pushback.extend(_status(state) for state in before)
        session.control_in.put(pb.AdapterMessage(hello=pb.AdapterHello(
            protocol_version="fleet_adapter.v1", contract_version="1.4.0")))
        session.control_in.put(pb.AdapterMessage(register=pb.RegisterRobot(
            robot_id="robot-conformance-1")))
        for state in after:
            session.control_in.put(_status(state))
        monkeypatch.setattr(drv, "_await_telemetry", lambda timeout=0.0: None)
        monkeypatch.setattr(drv, "_measure_cadence", lambda: None)
        drv._check_accepting_reconnect()
        return _row(report, "C2.2")

    return _run


def test_c22_red_when_the_adapter_sends_only_the_commanded_stop(run_c22, pb):
    """STOPPED alone halts the ghost and ends nothing: the row fails it."""
    row = run_c22(pb.ACTION_STATE_STOPPED)
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.is_blocking_failure
    assert "ACTION_STATE_STOPPED" in row.detail and "not terminal" in row.detail, row.detail


def test_c22_fails_when_nothing_is_reported(run_c22):
    """Failed before this change too; the detail now says what the row waits for."""
    row = run_c22()
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.detail.startswith("no terminal report"), row.detail


def test_c22_green_for_stopped_then_cancelled(run_c22, pb):
    """The commanded stop, then the end the RegisterAck declared."""
    row = run_c22(pb.ACTION_STATE_STOPPED, pb.ACTION_STATE_CANCELLED)
    assert row.status is CheckStatus.PASS, row.detail
    assert row.detail.startswith("reported ACTION_STATE_CANCELLED"), row.detail
    assert "ACTION_STATE_STOPPED" in row.detail, "the detail names the uncounted commanded stop"


_TERMINAL = ["ACTION_STATE_SUCCEEDED", "ACTION_STATE_FAILED", "ACTION_STATE_CANCELLED",
             "ACTION_STATE_UNKNOWN"]


@pytest.mark.parametrize("terminal", _TERMINAL)
def test_c22_accepts_every_state_rfc_0001_calls_terminal(run_c22, pb, terminal):
    """D-3, as C4.2: any state RFC-0001 calls terminal passes after the commanded stop."""
    row = run_c22(pb.ACTION_STATE_STOPPED, getattr(pb, terminal))
    assert row.status is CheckStatus.PASS, row.detail
    assert row.detail.startswith(f"reported {terminal}"), row.detail


@pytest.mark.parametrize("terminal", _TERMINAL)
def test_c22_accepts_every_terminal_state_with_no_stop_before_it(run_c22, pb, terminal):
    row = run_c22(getattr(pb, terminal))
    assert row.status is CheckStatus.PASS, row.detail
    assert row.detail.startswith(f"reported {terminal}"), row.detail


def test_c22_names_no_stopped_when_none_came_before_the_end(run_c22, pb):
    row = run_c22(pb.ACTION_STATE_CANCELLED)
    assert row.status is CheckStatus.PASS, row.detail
    assert "ACTION_STATE_STOPPED" not in row.detail, row.detail


@pytest.mark.parametrize("not_terminal", ["ACTION_STATE_RUNNING", "ACTION_STATE_PAUSED"])
def test_c22_red_for_the_other_states_that_are_not_terminal(run_c22, pb, not_terminal):
    row = run_c22(getattr(pb, not_terminal))
    assert row.status is CheckStatus.FAIL, row.detail


def test_a_stopped_held_from_before_the_reconnect_is_not_the_reconciliation(run_c22, pb):
    """A STOPPED for the ghost that arrived before the stream was faulted is not an answer to
    the RegisterAck: with nothing after it the row fails, and it is not named as a stop."""
    row = run_c22(before=(pb.ACTION_STATE_STOPPED,))
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.detail.startswith("no terminal report"), row.detail
    assert "ACTION_STATE_STOPPED" not in row.detail, row.detail


def test_a_stopped_held_from_before_the_reconnect_is_not_named_beside_the_end(run_c22, pb):
    row = run_c22(pb.ACTION_STATE_CANCELLED, before=(pb.ACTION_STATE_STOPPED,))
    assert row.status is CheckStatus.PASS, row.detail
    assert "ACTION_STATE_STOPPED" not in row.detail, row.detail
