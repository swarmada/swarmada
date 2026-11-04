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

"""C3.9 — a cancel naming a preempted action leaves the preemptor running to completion.

Every cancel the harness sent named the action that was running, so an adapter that stops
whatever is running on ANY cancel passed every row. The control plane sends exactly the
other kind: when an action is preempted in place, the robot switches to the preemptor
first, and the preempted action's reconcile then cancels the preempted action by name. An
adapter that ignores `action_id` aborts the preemptor, and the preemptor never completes.
"""

from __future__ import annotations

import queue

import pytest

from adapters.conformance.report import CheckStatus, SkipCause


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got {[r.check_id for r in report.results]}")


class _Robot:
    """One robot, one running action, answering by a rule for a cancel it did not expect.

    'reads-name'     a cancel naming an action it is not running is acknowledged, and
                     nothing stops (what the simulation adapter does)
    'stale'          such a cancel is refused stale=true (one of the two answers
                     RFC-0001's fencing rule accepts)
    'stops-running'  every cancel stops whatever is running, whatever it names
    'token-only'     a cancel below the mark is refused stale; one at the mark stops
                     whatever is running, whatever it names
    'no-end'         reads the name, but never reports the preemptor ending
    'refuses-second' refuses any assignment while one is running (ROBOT_BUSY)
    """

    def __init__(self, pb, session, rule):
        self.pb, self.session, self.rule = pb, session, rule
        self.running = ""
        self.mark = 0
        self.cancels = 0
        self.cancel_results = []

    def _status(self, action_id, state, message=""):
        self.session.control_in.put(self.pb.AdapterMessage(action_status=self.pb.ActionStatusUpdate(
            action_id=action_id, state=state, message=message, fencing_token=self.mark)))

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
            arm = cmd.WhichOneof("command")
            if arm == "assign_action":
                a = cmd.assign_action
                if self.rule == "refuses-second" and self.running:
                    res.assign_action.CopyFrom(pb.AssignActionResult(
                        accepted=False, rejection=pb.ASSIGN_ACTION_REJECTION_ROBOT_BUSY))
                else:
                    self.running, self.mark = a.action_id, a.fencing_token
                    res.assign_action.CopyFrom(pb.AssignActionResult(
                        accepted=True, accepted_fencing_token=a.fencing_token))
                    self._status(a.action_id, pb.ACTION_STATE_RUNNING, "accepted")
            elif arm == "renew_lease":
                r = cmd.renew_lease
                res.renew_lease.CopyFrom(pb.RenewActionLeaseResult(
                    renewed=True, running=self.running == r.action_id))
                # The preemptor arrives on the renewal after the cancels.
                if self.cancels and self.running == r.action_id and self.rule != "no-end":
                    self.session.control_in.put(pb.AdapterMessage(command_result=res))
                    self._status(self.running, pb.ACTION_STATE_SUCCEEDED, "arrived")
                    self.running = ""
                    continue
            elif arm == "cancel_action":
                c = cmd.cancel_action
                self.cancels += 1
                other = c.action_id != self.running
                if other and self.rule in ("reads-name", "no-end", "refuses-second"):
                    ans = pb.CancelActionResult(
                        acknowledged=True, disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY)
                elif other and self.rule == "stale":
                    ans = pb.CancelActionResult(acknowledged=False, stale=True)
                elif self.rule == "token-only" and c.fencing_token < self.mark:
                    ans = pb.CancelActionResult(acknowledged=False, stale=True)
                else:
                    if self.running:
                        self._status(self.running, pb.ACTION_STATE_CANCELLED, "cancelled")
                    self.running = ""
                    ans = pb.CancelActionResult(
                        acknowledged=True, disposition=pb.CANCEL_DISPOSITION_STOPPED_SAFELY)
                self.cancel_results.append((c.action_id, c.fencing_token, c.cause, ans))
                res.cancel_action.CopyFrom(ans)
            self.session.control_in.put(pb.AdapterMessage(command_result=res))


@pytest.fixture
def run_c39(driver, pb, harness_mod, monkeypatch):
    # The waits are liveness bounds for a real robot; a test double answers at once.
    monkeypatch.setattr(harness_mod, "_PREEMPTOR_TERMINAL_WAIT", 0.3)
    monkeypatch.setattr(harness_mod, "_GET_TIMEOUT", 0.05)

    def _run(rule):
        drv, session, report = driver
        robot = _Robot(pb, session, rule)
        real = drv._await_result

        def _await(command_id):
            robot.pump()
            return real(command_id)

        drv._await_result = _await
        drv._check_cancel_names_its_action()
        return _row(report, "C3.9"), robot

    return _run


def test_c39_green_for_an_adapter_that_reads_the_name(run_c39, pb):
    row, robot = run_c39("reads-name")
    assert row.status is CheckStatus.PASS, row.detail
    assert "SUCCEEDED" in row.detail and "running=True" in row.detail
    # Both cancels name the PREEMPTED action: one at its own token, one at the mark the
    # preemptor raised, and both say why.
    assert [(a, t, c) for a, t, c, _ in robot.cancel_results] == [
        ("t-preempted", 15, pb.CANCEL_CAUSE_SUPERSEDED),
        ("t-preempted", 16, pb.CANCEL_CAUSE_SUPERSEDED)]


def test_c39_green_for_an_adapter_that_refuses_the_cancel_stale(run_c39):
    """A stale=true refusal is one of the two answers RFC-0001's fencing rule accepts; the
    preemptor is untouched, so the row passes, and the answer is in the detail rather than
    graded."""
    row, _ = run_c39("stale")
    assert row.status is CheckStatus.PASS, row.detail
    assert "stale=True" in row.detail


def test_c39_red_for_an_adapter_that_stops_whatever_is_running(run_c39):
    row, _ = run_c39("stops-running")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "ACTION_STATE_CANCELLED" in row.detail
    assert row.is_blocking_failure


def test_c39_red_for_an_adapter_that_checks_only_the_token(run_c39):
    """The cancel at the mark is the one the control plane sends when the two actions'
    tokens are equal. An adapter that refuses the first cancel as stale and never reads
    `action_id` must still fail, or the row passes the defect it exists for."""
    row, robot = run_c39("token-only")
    assert robot.cancel_results[0][3].stale, "the first cancel was expected to be refused stale"
    assert row.status is CheckStatus.FAIL, row.detail


def test_c39_red_when_the_preemptor_is_never_seen_to_complete(run_c39):
    row, _ = run_c39("no-end")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "no terminal ActionStatusUpdate" in row.detail


def test_c39_skips_when_the_preemptor_is_refused(run_c39):
    row, robot = run_c39("refuses-second")
    assert row.status is CheckStatus.SKIP, row.detail
    assert row.detail.startswith(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value)
    assert robot.cancel_results == [], "no cancel is sent without a preemption to cancel around"


def test_c39_skips_without_a_declared_action(driver):
    drv, _session, report = driver
    drv._catalog, drv._served = [], None
    drv._when_served(drv._check_cancel_names_its_action)
    row = _row(report, "C3.9")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.ADAPTER_DECLARES_NO_ACTION.value)


def test_c39_tokens_sit_between_c16_and_c17(harness_mod):
    """The fence is per robot, so C3.9's two assignments must rise above C16's 14 and stay
    below C17's first probe, and the C3.8 adoption chain must still sit above all of it."""
    assert 14 < harness_mod._PREEMPTED_TOKEN < harness_mod._PREEMPTOR_TOKEN
    assert harness_mod._PREEMPTOR_TOKEN < harness_mod._ZONE_CORE_TOKEN
    assert harness_mod._ZONE_HONOURED_TOKEN < harness_mod._ADOPTION_PROBE_TOKEN
