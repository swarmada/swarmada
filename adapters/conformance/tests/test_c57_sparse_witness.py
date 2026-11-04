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

"""C5.7 against a sparse witness: the tolerance is the sample gap at the rest, when wider.

A witness shows rest only at its first sample within the noise. The robot came to rest
somewhere between that sample and the one before it, which still showed motion. When the
witness samples once a second, the first rest sample can be almost a second after the true
rest, so a STOPPED sent at the true rest reads as early by nearly that second. Judged
against a declared resolution of 0.2 s, a timely STOPPED then fails.

The check compares with the larger of the declared resolution and the gap between the last
sample that shows motion and the first that shows rest. A STOPPED sent before the last
sample that shows motion is still early by any reading, and still fails.
"""

from __future__ import annotations

import json

from adapters.conformance.report import CheckStatus

T0 = 1_000_000.0  # the estop's send time, on the witness's epoch clock
DECLARED_RESOLUTION = 0.2
# One sample a second: moving before the estop and 0.5 s after it, at rest from 1.5 s.
# The robot's true rest lies somewhere in (T0 + 0.5, T0 + 1.5].
SPARSE = [(T0 - 0.5, 0.5), (T0 + 0.5, 0.3), (T0 + 1.5, 0.0), (T0 + 2.5, 0.0),
          (T0 + 3.5, 0.0)]
LAST_MOVING, FIRST_REST = T0 + 0.5, T0 + 1.5


def _row(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded")


def _sparse_witness(tmp_path):
    path = tmp_path / "witness.jsonl"
    lines = [json.dumps({"witness": "sparse test plant model", "rest_noise_mps": 0.0,
                         "hold_s": 0.5, "resolution_s": DECLARED_RESOLUTION})]
    lines += [json.dumps({"t": t, "robot": "r1", "speed": v}) for t, v in SPARSE]
    path.write_text("\n".join(lines) + "\n")
    return str(path)


def _stopped(pb, at: float):
    return pb.EstopAck(estop_id="e1", state=pb.ESTOP_STATE_STOPPED,
                       confirmed_at_ms=int(round(at * 1000)))


def test_c57_green_for_a_timely_stopped_against_a_sparse_witness(driver, pb, tmp_path):
    """STOPPED at the true rest, 0.7 s before the first sample that can show it."""
    drv, _s, report = driver
    drv._witness_path, drv._registered_robot = _sparse_witness(tmp_path), "r1"
    true_rest = T0 + 0.8
    drv._grade_early_stopped(_stopped(pb, true_rest), true_rest + 0.01, T0)
    row = _row(report, "C5.7")
    assert row.status is CheckStatus.PASS, row.detail
    assert "sample gap" in row.detail, "the row says what tolerance it applied"


def test_c57_still_red_for_a_stopped_before_the_last_sample_showing_motion(driver, pb,
                                                                           tmp_path):
    """Sent while the witness still shows the robot moving: early by any reading."""
    drv, _s, report = driver
    drv._witness_path, drv._registered_robot = _sparse_witness(tmp_path), "r1"
    drv._grade_early_stopped(_stopped(pb, LAST_MOVING - 0.3), LAST_MOVING - 0.29, T0)
    row = _row(report, "C5.7")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "EARLY" in row.detail and row.is_blocking_failure


def test_c57_a_dense_witness_keeps_its_declared_resolution(driver, pb, tmp_path):
    """Where samples are denser than the declared resolution, the declared value governs."""
    drv, _s, report = driver
    path = tmp_path / "witness.jsonl"
    lines = [json.dumps({"witness": "dense test plant model", "rest_noise_mps": 0.0,
                         "hold_s": 0.1, "resolution_s": DECLARED_RESOLUTION})]
    speeds = [1.0] * 5 + [0.5, 0.0, 0.0, 0.0, 0.0, 0.0]  # rest at the 7th sample
    lines += [json.dumps({"t": T0 - 0.2 + i * 0.05, "robot": "r1", "speed": v})
              for i, v in enumerate(speeds)]
    path.write_text("\n".join(lines) + "\n")
    drv._witness_path, drv._registered_robot = str(path), "r1"
    rest = T0 - 0.2 + 6 * 0.05
    drv._grade_early_stopped(_stopped(pb, rest - 0.25), rest - 0.24, T0)
    row = _row(report, "C5.7")
    assert row.status is CheckStatus.FAIL, row.detail
