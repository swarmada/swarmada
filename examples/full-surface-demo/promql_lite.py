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

"""A tiny, dependency-free evaluator for the RFC-0001 §9.3.8 alert expressions.

The demo-test CI gate scrapes the controller-manager's ``/metrics`` endpoint
directly — there is no Prometheus or Alertmanager in the kind quickstart — so it
must evaluate each §9.3.8 alert ``expr`` itself. This module parses the Prometheus
text-exposition format into a :class:`Snapshot` and evaluates the seven §9.3.8
expressions VERBATIM from deploy/swarmada/templates/prometheusrule.yaml.

Two evaluation shapes, both grounded in a Prometheus range window:

  * **instant gauge** expressions (``x{...} > 0`` / ``== 0``) evaluate against a
    single scrape — these fire AND clear within a bounded run as the scenario
    drives the underlying CRD state (e.g. fleetactions_by_phase{phase="Revoking"}
    goes >0 during the lease-expiry beat and back to 0 after).

  * **range** expressions (``increase(c[5m])``, ``histogram_quantile(0.99,
    rate(b[10m]))``) are approximated from the DELTA between a baseline scrape and
    a later scrape — ``increase(c[w]) > 0`` ⇔ the counter advanced between the two
    scrapes; the histogram quantile is computed from per-bucket deltas. These fire
    when their fault fixture triggers; "clearing" a counter/rate expression instead
    requires its window ``w`` to elapse with no new events (documented in the gate
    output — a bounded fast run observes the fire, not the multi-minute decay).

Proto-free and control-plane-free: it only reads text, so it is unit-testable
offline (tests/python/test_promql_lite.py) without a cluster.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class Sample:
    name: str
    labels: tuple[tuple[str, str], ...]  # sorted (k, v) pairs — hashable
    value: float

    def label(self, key: str) -> str | None:
        for k, v in self.labels:
            if k == key:
                return v
        return None


def _parse_labels(blob: str) -> tuple[tuple[str, str], ...]:
    """Parse ``a="1",b="2"`` (the inside of the ``{...}``) into sorted pairs."""
    out: list[tuple[str, str]] = []
    i, n = 0, len(blob)
    while i < n:
        eq = blob.index("=", i)
        key = blob[i:eq].strip()
        assert blob[eq + 1] == '"', f"unquoted label value in {blob!r}"
        j = eq + 2
        val_chars: list[str] = []
        while blob[j] != '"':
            if blob[j] == "\\":  # unescape \\ and \"
                j += 1
            val_chars.append(blob[j])
            j += 1
        out.append((key, "".join(val_chars)))
        i = j + 1
        while i < n and blob[i] in ", ":
            i += 1
    return tuple(sorted(out))


def parse_metrics(text: str) -> list[Sample]:
    """Parse Prometheus text-exposition format. Ignores ``#`` HELP/TYPE lines."""
    samples: list[Sample] = []
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "{" in line:
            name = line[: line.index("{")]
            rest = line[line.index("{") + 1 :]
            close = rest.rindex("}")
            labels = _parse_labels(rest[:close])
            value_str = rest[close + 1 :].strip().split()[0]
        else:
            parts = line.split()
            name, value_str = parts[0], parts[1]
            labels = ()
        try:
            value = float(value_str)
        except ValueError:
            continue  # +Inf/NaN histogram edge — skip; not needed by §9.3.8 exprs
        samples.append(Sample(name, labels, value))
    return samples


class Snapshot:
    """One ``/metrics`` scrape, queryable by name + exact label match."""

    def __init__(self, text: str) -> None:
        self.samples = parse_metrics(text)

    def series(self, name: str, **match: str) -> list[Sample]:
        out = []
        for s in self.samples:
            if s.name != name:
                continue
            if all(s.label(k) == v for k, v in match.items()):
                out.append(s)
        return out

    def sum(self, name: str, **match: str) -> float:
        return sum(s.value for s in self.series(name, **match))

    def max(self, name: str, **match: str) -> float:
        vals = [s.value for s in self.series(name, **match)]
        return max(vals) if vals else 0.0


def histogram_quantile(q: float, bucket_counts: list[tuple[float, float]]) -> float:
    """Prometheus ``histogram_quantile`` over cumulative (le, count) buckets.

    ``bucket_counts`` is a list of (le, cumulative_count) including the +Inf
    bucket; le may be ``float('inf')``. Returns the linearly-interpolated quantile,
    matching Prometheus semantics closely enough for a ">60s" threshold check.
    """
    buckets = sorted(bucket_counts, key=lambda b: b[0])
    if not buckets:
        return 0.0
    total = buckets[-1][1]  # +Inf cumulative count == total observations
    if total <= 0:
        return 0.0
    rank = q * total
    prev_le, prev_count = 0.0, 0.0
    for le, count in buckets:
        if count >= rank:
            if le == float("inf"):
                return prev_le  # quantile in the overflow bucket → lower bound
            if count == prev_count:
                return le
            # linear interpolation within [prev_le, le]
            frac = (rank - prev_count) / (count - prev_count)
            return prev_le + (le - prev_le) * frac
        prev_le, prev_count = le, count
    return buckets[-1][0]


def _bucket_deltas(
    baseline: Snapshot, current: Snapshot, name: str, **match: str
) -> list[tuple[float, float]]:
    """Per-``le`` (cumulative_count) DELTA between two scrapes — the discrete stand-in
    for ``rate(bucket[w])`` over the interval between them."""
    def by_le(snap: Snapshot) -> dict[float, float]:
        out: dict[float, float] = {}
        for s in snap.series(name, **match):
            le_raw = s.label("le")
            if le_raw is None:
                continue
            le = float("inf") if le_raw in ("+Inf", "Inf") else float(le_raw)
            out[le] = out.get(le, 0.0) + s.value
        return out

    base, cur = by_le(baseline), by_le(current)
    return [(le, cur[le] - base.get(le, 0.0)) for le in sorted(cur)]


# ── The seven §9.3.8 alert expressions (verbatim exprs from prometheusrule.yaml) ──
#
# Each returns (fired: bool, observed: float, detail: str). ``increase``/``rate``
# expressions take a baseline; instant gauges ignore it.

def estop_latency_slo_breach(cur: Snapshot, base: Snapshot | None = None) -> tuple[bool, float, str]:
    """increase(swarmada_estop_latency_violations_total[5m]) > 0"""
    delta = cur.sum("swarmada_estop_latency_violations_total") - (
        base.sum("swarmada_estop_latency_violations_total") if base else 0.0)
    return delta > 0, delta, "estop ACK latency violations (>500ms)"


def telemetry_dropped_frames(cur: Snapshot, base: Snapshot | None = None) -> tuple[bool, float, str]:
    """increase(swarmada_telemetry_dropped_frames_total[5m]) > 0"""
    delta = cur.sum("swarmada_telemetry_dropped_frames_total") - (
        base.sum("swarmada_telemetry_dropped_frames_total") if base else 0.0)
    return delta > 0, delta, "telemetry frames dropped (sink not Drop)"


def telemetry_tsdb_write_errors(cur: Snapshot, base: Snapshot | None = None) -> tuple[bool, float, str]:
    """increase(swarmada_telemetry_tsdb_write_errors_total[5m]) > 0"""
    delta = cur.sum("swarmada_telemetry_tsdb_write_errors_total") - (
        base.sum("swarmada_telemetry_tsdb_write_errors_total") if base else 0.0)
    return delta > 0, delta, "TSDB sink write errors"


def scheduler_assignment_latency_high(cur: Snapshot, base: Snapshot | None = None) -> tuple[bool, float, str]:
    """histogram_quantile(0.99, rate(...assignment_latency_seconds_bucket{priority="Normal"}[10m])) > 60"""
    buckets = _bucket_deltas(
        base or Snapshot(""), cur,
        "swarmada_scheduler_assignment_latency_seconds_bucket", priority="Normal")
    p99 = histogram_quantile(0.99, buckets)
    return p99 > 60, p99, "P99 Normal-priority assignment latency (s)"


def fleetaction_stuck_revoking(cur: Snapshot, base: Snapshot | None = None) -> tuple[bool, float, str]:
    """swarmada_fleetactions_by_phase{phase="Revoking"} > 0"""
    v = cur.max("swarmada_fleetactions_by_phase", phase="Revoking")
    return v > 0, v, "FleetActions in Revoking"


def fleet_adapter_disconnected(cur: Snapshot, base: Snapshot | None = None) -> tuple[bool, float, str]:
    """swarmada_fleet_adapter_connected == 0"""
    series = cur.series("swarmada_fleet_adapter_connected")
    disconnected = [s for s in series if s.value == 0]
    return bool(disconnected), float(len(disconnected)), "adapters with connected==0"


def robot_estop_uncleared(cur: Snapshot, base: Snapshot | None = None) -> tuple[bool, float, str]:
    """swarmada_robots_in_estop{estop_state="Stopped"} > 0"""
    v = cur.max("swarmada_robots_in_estop", estop_state="Stopped")
    return v > 0, v, "robots in estop_state=Stopped"


# alert name → (evaluator, needs_baseline). needs_baseline=True marks the
# range (increase/rate) expressions whose "clear" needs the window to elapse.
ALERTS: dict[str, tuple] = {
    "SwarmadaEstopLatencySLOBreach": (estop_latency_slo_breach, True),
    "SwarmadaTelemetryDroppedFrames": (telemetry_dropped_frames, True),
    "SwarmadaTelemetryTSDBWriteErrors": (telemetry_tsdb_write_errors, True),
    "SwarmadaSchedulerAssignmentLatencyHigh": (scheduler_assignment_latency_high, True),
    "SwarmadaFleetActionStuckRevoking": (fleetaction_stuck_revoking, False),
    "SwarmadaFleetAdapterDisconnected": (fleet_adapter_disconnected, False),
    "SwarmadaRobotEstopUncleared": (robot_estop_uncleared, False),
}
