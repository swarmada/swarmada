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

"""Offline unit tests for the demo-test driver's pure helpers (no cluster)."""

import pathlib
import sys

_DEMO = pathlib.Path(__file__).resolve().parents[2] / "examples" / "full-surface-demo"
sys.path.insert(0, str(_DEMO))

from demo_test import (  # noqa: E402
    TARGET_ROBOT_PHASES,
    coverage_gaps,
    edge_estop_confirmed,
    observed_phases,
    ra1_ok,
    ra1_ratio,
)
from promql_lite import Snapshot  # noqa: E402


def test_observed_phases_collects_nonempty():
    items = [
        {"status": {"phase": "Idle"}},
        {"status": {"phase": "Offline"}},
        {"status": {"phase": ""}},   # empty ignored
        {"status": {}},              # missing ignored
        {},                          # no status ignored
    ]
    assert observed_phases(items) == {"Idle", "Offline"}


def test_observed_phases_alternate_field():
    items = [{"status": {"estopState": "Stopped"}}, {"status": {"estopState": "Normal"}}]
    assert observed_phases(items, "estopState") == {"Stopped", "Normal"}


def test_coverage_gaps_preserves_target_order():
    seen = {"Idle", "Offline", "Discovered"}
    gaps = coverage_gaps(seen, TARGET_ROBOT_PHASES)
    assert gaps == ["Assigned", "InProgress", "Charging", "Error", "Maintenance"]


def test_coverage_gaps_empty_when_all_seen():
    assert coverage_gaps(set(TARGET_ROBOT_PHASES), TARGET_ROBOT_PHASES) == []


def _ra1_snap(frames, writes, ns="warehouse-a"):
    return Snapshot(
        f'swarmada_telemetry_frames_received_total{{namespace="{ns}",adapter="sim"}} {frames}\n'
        f'swarmada_telemetry_status_writes_total{{namespace="{ns}",transition_type="phase_change"}} {writes}\n'
    )


def test_ra1_ratio_and_ok_compliant():
    # 200 frames, 6 status writes → ratio 0.03, well under ceiling.
    snap = _ra1_snap(200, 6)
    r = ra1_ratio(snap, "warehouse-a")
    assert abs(r - 0.03) < 1e-9
    assert ra1_ok(r) is True


def test_ra1_ok_fails_per_tick_writer():
    # A per-tick writer: ~1 write per frame → ratio ~1.0, fails RA-1.
    snap = _ra1_snap(200, 200)
    assert ra1_ok(ra1_ratio(snap, "warehouse-a")) is False


def test_ra1_ratio_none_without_frames():
    snap = _ra1_snap(0, 0)
    assert ra1_ratio(snap, "warehouse-a") is None
    assert ra1_ok(None) is False   # no telemetry seen → not provable → fail


def test_edge_estop_confirmed_matches_stopped_line():
    log = (
        "some noise\n"
        "EDGE_ESTOP_CONFIRMED robot=sim-robot-001 id=edge-1 state=STOPPED\n"
        "more noise\n"
    )
    assert edge_estop_confirmed(log, "sim-robot-001") is True
    assert edge_estop_confirmed(log, "sim-robot-002") is False  # wrong robot


def test_edge_estop_confirmed_rejects_failed_and_absent():
    assert edge_estop_confirmed(
        "EDGE_ESTOP_CONFIRMED robot=sim-robot-001 id=e state=FAILED\n", "sim-robot-001") is False
    assert edge_estop_confirmed("no marker here\n", "sim-robot-001") is False
