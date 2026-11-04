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

"""C5.7 — a STOPPED sent before the witnessed moment of rest fails.

C5.3 concedes the wire cannot tell a confirmed stop from an inferred one. A witness can:
something other than the adapter's confirmation path that records the robot's actual
speed. These tests hand the check a witness file and an EstopAck and grade the timing.
"""

from __future__ import annotations

import json

from adapters.conformance.report import CheckStatus, SkipCause

T0 = 1_000_000.0  # the estop's send time, on the witness's epoch clock


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded")


def _witness(tmp_path, speeds, *, dt=0.05, hold=0.1, resolution=0.05, noise=0.0,
             robot="r1"):
    """A witness whose samples start 0.2 s before the estop, one every `dt`."""
    path = tmp_path / "witness.jsonl"
    lines = [json.dumps({"witness": "test plant model", "rest_noise_mps": noise,
                         "hold_s": hold, "resolution_s": resolution})]
    for i, v in enumerate(speeds):
        lines.append(json.dumps({"t": T0 - 0.2 + i * dt, "robot": robot, "speed": v}))
    path.write_text("\n".join(lines) + "\n")
    return str(path)


def _stopped(pb, at: float | None):
    ack = pb.EstopAck(estop_id="e1", state=pb.ESTOP_STATE_STOPPED)
    if at is not None:
        ack.confirmed_at_ms = int(at * 1000)
    return ack


# Moving at 1 m/s until the estop, decelerating over 0.25 s, then at rest.
DECEL = [1.0] * 5 + [0.8, 0.6, 0.4, 0.2, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]
REST_AT = T0 - 0.2 + 9 * 0.05  # the first 0.0 sample


def test_c57_red_when_stopped_precedes_the_witnessed_rest(driver, pb, tmp_path):
    """The one-phase STOPPED sent at the moment of the stop command, before rest."""
    drv, _s, report = driver
    drv._witness_path, drv._registered_robot = _witness(tmp_path, DECEL), "r1"
    drv._grade_early_stopped(_stopped(pb, T0 + 0.01), T0 + 0.02, T0)
    row = _row(report, "C5.7")
    assert row.status is CheckStatus.FAIL, row.detail
    assert row.level == "MUST NOT" and row.is_blocking_failure
    assert "EARLY" in row.detail


def test_c57_green_when_stopped_follows_the_witnessed_rest(driver, pb, tmp_path):
    drv, _s, report = driver
    drv._witness_path, drv._registered_robot = _witness(tmp_path, DECEL), "r1"
    drv._grade_early_stopped(_stopped(pb, REST_AT + 0.01), REST_AT + 0.02, T0)
    row = _row(report, "C5.7")
    assert row.status is CheckStatus.PASS, row.detail


def test_c57_uses_receipt_time_and_says_so_without_confirmed_at(driver, pb, tmp_path):
    drv, _s, report = driver
    drv._witness_path, drv._registered_robot = _witness(tmp_path, DECEL), "r1"
    drv._grade_early_stopped(_stopped(pb, None), REST_AT + 0.02, T0)
    row = _row(report, "C5.7")
    assert row.status is CheckStatus.PASS
    assert "receipt time" in row.detail and "lenient" in row.detail


def test_c57_not_testable_without_a_witness(driver, pb):
    drv, _s, report = driver
    drv._grade_early_stopped(_stopped(pb, T0 + 0.01), T0 + 0.02, T0)
    row = _row(report, "C5.7")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.NO_MOTION_WITNESS.value)


def test_c57_cannot_grade_a_robot_that_was_already_at_rest(driver, pb, tmp_path):
    """A stop issued to a robot at rest proves nothing about an early STOPPED."""
    drv, _s, report = driver
    drv._witness_path, drv._registered_robot = _witness(tmp_path, [0.0] * 12), "r1"
    drv._grade_early_stopped(_stopped(pb, T0 + 0.01), T0 + 0.02, T0)
    row = _row(report, "C5.7")
    assert row.status is CheckStatus.SKIP
    assert row.detail.startswith(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value)


def test_c57_red_when_the_witness_never_shows_rest(driver, pb, tmp_path):
    drv, _s, report = driver
    drv._witness_path, drv._registered_robot = _witness(tmp_path, [1.0] * 12), "r1"
    drv._grade_early_stopped(_stopped(pb, T0 + 0.01), T0 + 0.02, T0)
    assert _row(report, "C5.7").status is CheckStatus.FAIL


def test_c57_does_not_grade_a_failed_stop(driver, pb, tmp_path):
    drv, _s, report = driver
    drv._witness_path, drv._registered_robot = _witness(tmp_path, DECEL), "r1"
    drv._grade_early_stopped(pb.EstopAck(estop_id="e1", state=pb.ESTOP_STATE_FAILED),
                             T0 + 0.02, T0)
    assert _row(report, "C5.7").status is CheckStatus.SKIP
