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

"""C16 — task status reporting, terminal states included.

The catalog: "Send an ``ActionStatusUpdate`` at every task phase transition,
terminal states included. ... a task that ends without one is resolvable only by
lease expiry." The harness matched the FIRST update for the action, which is the
acceptance ``RUNNING``, so the half the sentence spends a clause on was never
asserted.
"""

from __future__ import annotations

from adapters.conformance.report import CheckStatus


def _status(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded; got "
                         f"{[r.check_id for r in report.results]}")


def test_c161_fails_when_only_the_acceptance_update_is_sent(pb, driver):
    """RUNNING alone leaves the terminal half unverified."""
    drv, _session, report = driver
    running = pb.ActionStatusUpdate(action_id="t-status",
                                    state=pb.ACTION_STATE_RUNNING,
                                    fencing_token=14)
    drv._record_task_status(transition=running, terminal=None)

    row = _status(report, "C16.1")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "terminal" in row.detail


def test_c161_passes_with_both_a_transition_and_a_terminal_update(pb, driver):
    """A task the harness drove to an end reported it: the full obligation."""
    drv, _session, report = driver
    drv._record_task_status(
        transition=pb.ActionStatusUpdate(action_id="t-status",
                                         state=pb.ACTION_STATE_RUNNING,
                                         fencing_token=14),
        terminal=pb.ActionStatusUpdate(action_id="t1",
                                       state=pb.ACTION_STATE_CANCELLED,
                                       fencing_token=5))
    assert _status(report, "C16.1").status is CheckStatus.PASS


def test_c161_skips_when_no_task_was_accepted(pb, driver):
    """No accepted assignment leaves the obligation unestablished, not broken."""
    drv, _session, report = driver
    drv._record_task_status(transition=None, terminal=None, accepted=False)

    row = _status(report, "C16.1")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith("adapter-did-not-reach-state")
