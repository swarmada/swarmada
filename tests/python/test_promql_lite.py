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

"""Offline unit tests for the §9.3.8 alert-expression evaluator (no cluster)."""

import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "examples" / "full-surface-demo"))

from promql_lite import (  # noqa: E402
    ALERTS,
    Snapshot,
    histogram_quantile,
    parse_metrics,
)


def test_parse_labels_and_values():
    snap = Snapshot(
        '# HELP x help\n'
        '# TYPE x gauge\n'
        'swarmada_fleetactions_by_phase{namespace="warehouse-a",phase="Revoking"} 1\n'
        'swarmada_fleetactions_by_phase{namespace="warehouse-a",phase="InProgress"} 2\n'
        'swarmada_fleet_adapter_connected{namespace="warehouse-a",adapter="sim"} 0\n'
    )
    assert snap.max("swarmada_fleetactions_by_phase", phase="Revoking") == 1
    assert snap.sum("swarmada_fleetactions_by_phase") == 3
    assert snap.series("swarmada_fleet_adapter_connected")[0].value == 0


def test_parse_ignores_comments_and_inf():
    samples = parse_metrics(
        "# a comment\n"
        'b_bucket{le="+Inf"} 5\n'
        "plain_metric 7\n"
    )
    names = {s.name for s in samples}
    assert "plain_metric" in names and "b_bucket" in names


def test_histogram_quantile_over_60():
    # One slow obs in the le=120 bucket (the real histogram has 120 & 300 bounds):
    # p99 interpolates within [60,120] and lands >60, firing the alert.
    buckets = [(1.0, 0), (60.0, 0), (120.0, 1), (300.0, 1), (float("inf"), 1)]
    assert histogram_quantile(0.99, buckets) > 60


def test_histogram_quantile_all_fast():
    buckets = [(1.0, 100), (60.0, 100), (120.0, 100), (float("inf"), 100)]
    assert histogram_quantile(0.99, buckets) <= 1.0


# ── fire / clear per §9.3.8 alert ─────────────────────────────────────────────

def _gauge(name, **labels):
    lbl = ",".join(f'{k}="{v}"' for k, v in labels.items())
    return f"{name}{{{lbl}}}"


def test_gauge_alerts_fire_and_clear():
    ns = {"namespace": "warehouse-a"}
    fire = Snapshot(
        _gauge("swarmada_fleetactions_by_phase", phase="Revoking", **ns) + " 1\n"
        + _gauge("swarmada_fleet_adapter_connected", adapter="sim", **ns) + " 0\n"
        + _gauge("swarmada_robots_in_estop", estop_state="Stopped", **ns) + " 1\n"
    )
    clear = Snapshot(
        _gauge("swarmada_fleetactions_by_phase", phase="Revoking", **ns) + " 0\n"
        + _gauge("swarmada_fleet_adapter_connected", adapter="sim", **ns) + " 1\n"
        + _gauge("swarmada_robots_in_estop", estop_state="Stopped", **ns) + " 0\n"
    )
    for alert in ("SwarmadaFleetActionStuckRevoking", "SwarmadaFleetAdapterDisconnected",
                  "SwarmadaRobotEstopUncleared"):
        fn, _ = ALERTS[alert]
        assert fn(fire)[0] is True, f"{alert} should fire"
        assert fn(clear)[0] is False, f"{alert} should clear"


def test_counter_increase_alerts_fire_on_delta():
    base = Snapshot(
        'swarmada_estop_latency_violations_total{namespace="warehouse-a"} 0\n'
        'swarmada_telemetry_dropped_frames_total{namespace="warehouse-a"} 0\n'
        'swarmada_telemetry_tsdb_write_errors_total{namespace="warehouse-a"} 0\n'
    )
    fired = Snapshot(
        'swarmada_estop_latency_violations_total{namespace="warehouse-a"} 1\n'
        'swarmada_telemetry_dropped_frames_total{namespace="warehouse-a"} 3\n'
        'swarmada_telemetry_tsdb_write_errors_total{namespace="warehouse-a"} 2\n'
    )
    for alert in ("SwarmadaEstopLatencySLOBreach", "SwarmadaTelemetryDroppedFrames",
                  "SwarmadaTelemetryTSDBWriteErrors"):
        fn, needs_base = ALERTS[alert]
        assert needs_base is True
        assert fn(fired, base)[0] is True, f"{alert} should fire on counter advance"
        assert fn(base, base)[0] is False, f"{alert} should be quiet with no advance"


def test_scheduler_latency_fires_from_bucket_delta():
    b = "swarmada_scheduler_assignment_latency_seconds_bucket"
    base = Snapshot(
        f'{b}{{namespace="warehouse-a",priority="Normal",le="1"}} 0\n'
        f'{b}{{namespace="warehouse-a",priority="Normal",le="60"}} 0\n'
        f'{b}{{namespace="warehouse-a",priority="Normal",le="+Inf"}} 0\n'
    )
    # One obs at ~90s: cumulative 0 through le=60, then 1 at le=120/300/+Inf.
    slow = Snapshot(
        f'{b}{{namespace="warehouse-a",priority="Normal",le="1"}} 0\n'
        f'{b}{{namespace="warehouse-a",priority="Normal",le="60"}} 0\n'
        f'{b}{{namespace="warehouse-a",priority="Normal",le="120"}} 1\n'
        f'{b}{{namespace="warehouse-a",priority="Normal",le="300"}} 1\n'
        f'{b}{{namespace="warehouse-a",priority="Normal",le="+Inf"}} 1\n'
    )
    fn, _ = ALERTS["SwarmadaSchedulerAssignmentLatencyHigh"]
    assert fn(slow, base)[0] is True
    fn2, _ = ALERTS["SwarmadaSchedulerAssignmentLatencyHigh"]
    assert fn2(base, base)[0] is False


def test_all_seven_alerts_present():
    assert len(ALERTS) == 7
