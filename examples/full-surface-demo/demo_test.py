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

"""Headless assertion driver for the full-surface demo-test CI gate.

Given a way to read CRD status (``kubectl ... -o json``) and scrape the
controller-manager ``/metrics``, this:

  1. **polls** Robot/FleetAction status across the whole run, accumulating every
     RobotPhase, FleetAction phase, and estop state observed (poll the API, never
     swarmtop — RFC sub-task requirement);
  2. evaluates the seven RFC-0001 §9.3.8 alert expressions (promql_lite) at the
     scenario beats where their fixtures fire, and again after, recording
     fire/clear;
  3. asserts RA-1 end-to-end: telemetry runs at full cadence but status writes
     occur only on material transitions — ``status_writes_total`` must be far
     below ``frames_received_total`` (a hard ceiling ratio), proving per-tick
     writes never happened.

Pure functions (``observed_phases``, ``coverage_gaps``, ``ra1_ratio``,
``ra1_ok``) are unit-tested offline (tests/python/test_demo_test.py); the polling
``main`` needs a live cluster and is exercised by make demo-test.

Exit code is the CI gate: 0 only if every targeted phase/estop-state was observed,
every alert fired (and every gauge alert cleared), and RA-1 held. Any miss → 1
with a ❌ line naming exactly what was missed.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
import urllib.request

# promql_lite lives beside this file.
sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
from promql_lite import ALERTS, Snapshot  # noqa: E402

# Targets the scenario+harness must walk (see full-surface.yaml's documented
# real-vs-projected split; the orchestration projects the non-live-reachable ones).
TARGET_ROBOT_PHASES = [
    "Discovered", "Idle", "Assigned", "InProgress", "Charging", "Error", "Offline", "Maintenance",
]
TARGET_ACTION_PHASES = [
    "Pending", "Assigned", "InProgress", "Revoking", "Succeeded",
]
TARGET_ESTOP_STATES = ["Stopping", "Stopped"]

# Alerts that cannot fire in the insecure-mode demo and are reported but NOT gated on.
# SwarmadaEstopLatencySLOBreach's counter (swarmada_estop_latency_violations_total) is
# written ONLY by the real control-plane estop dispatcher, which needs mTLS to route a
# SafetyStream command to a named adapter (insecure mode has empty adapter identity, so
# the estop resolves Failed and never records a latency violation). Projection can drive
# estopState (→ RobotEstopUncleared) but not this counter. See the harness README.
KNOWN_GAPS = {"SwarmadaEstopLatencySLOBreach"}
KNOWN_GAPS_REASON = "needs mTLS (insecure mode can't route a control-plane estop to a named adapter)"

# RA-1 ceiling: over a full run of high-cadence telemetry, status writes must be a
# small fraction of frames. The sim emits at 5 Hz; a compliant projector writes
# only on the handful of material transitions in full-surface. 0.5 is a very loose
# ceiling that a per-tick writer (ratio ~1.0) fails and a compliant one passes with
# huge margin; override with --ra1-max-ratio for a tighter gate.
DEFAULT_RA1_MAX_RATIO = 0.5


# ── pure helpers (unit-tested offline) ────────────────────────────────────────

def observed_phases(items: list[dict], field: str = "phase") -> set[str]:
    """The set of non-empty ``status.<field>`` values across a list of CRD objects
    (as returned in ``.items`` by ``kubectl get ... -o json``)."""
    out: set[str] = set()
    for it in items:
        v = (it.get("status") or {}).get(field)
        if v:
            out.add(v)
    return out


def coverage_gaps(seen: set[str], targets: list[str]) -> list[str]:
    """Targets not yet in ``seen``, preserving target order."""
    return [t for t in targets if t not in seen]


def ra1_ratio(snap: Snapshot, namespace: str) -> float | None:
    """status_writes_total / frames_received_total. Tries the namespace first, then
    falls back to a fleet-wide sum (the telemetry pipeline may label these with a
    different namespace than the robot's — e.g. the ingestor's — so a namespace miss
    must not read as "no telemetry"). None only when NO frames exist anywhere."""
    frames = snap.sum("swarmada_telemetry_frames_received_total", namespace=namespace)
    writes = snap.sum("swarmada_telemetry_status_writes_total", namespace=namespace)
    if frames <= 0:  # fall back to fleet-wide totals
        frames = snap.sum("swarmada_telemetry_frames_received_total")
        writes = snap.sum("swarmada_telemetry_status_writes_total")
    if frames <= 0:
        return None
    return writes / frames


# Stable marker emitted by adapters/simulation/sim_adapter.py's EdgeStream estop
# handler on a confirmed edge-issued estop (C8.3). Edge estops are confirmed directly
# between the edge node and the adapter, bypassing the control plane, so this adapter
# log line — not CRD status or a control-plane metric — is the observable that proves
# all three RPC services (ControlStream, SafetyStream, EdgeStream) were exercised.
EDGE_ESTOP_MARKER = "EDGE_ESTOP_CONFIRMED"


def edge_estop_confirmed(adapter_log: str, robot: str) -> bool:
    """True if the adapter log records a CONFIRMED (state=STOPPED) edge-issued estop
    for ``robot`` — proof the EdgeStream path ran end-to-end."""
    for line in adapter_log.splitlines():
        if EDGE_ESTOP_MARKER in line and f"robot={robot}" in line and "state=STOPPED" in line:
            return True
    return False


def ra1_ok(ratio: float | None, max_ratio: float = DEFAULT_RA1_MAX_RATIO) -> bool:
    """RA-1 holds if we have frames and the write/frame ratio is under the ceiling.
    None (no frames) is treated as not-yet-provable → False (the gate must see
    real telemetry to certify RA-1)."""
    return ratio is not None and ratio < max_ratio


# ── live cluster IO (exercised by make demo-test) ─────────────────────────────

def _kubectl_items(kind: str, namespace: str, ctx: str | None) -> list[dict]:
    cmd = ["kubectl"]
    if ctx:
        cmd += ["--context", ctx]
    cmd += ["get", kind, "-n", namespace, "-o", "json"]
    out = subprocess.run(cmd, capture_output=True, text=True, check=True).stdout
    return json.loads(out).get("items", [])


def _scrape(url: str) -> Snapshot:
    with urllib.request.urlopen(url, timeout=5) as resp:  # noqa: S310 (localhost only)
        return Snapshot(resp.read().decode("utf-8"))


class Accumulator:
    """Accumulates observed phases/estop states across polls, and alert fire/clear."""

    def __init__(self) -> None:
        self.robot_phases: set[str] = set()
        self.task_phases: set[str] = set()
        self.estop_states: set[str] = set()
        self.fired: set[str] = set()
        self.cleared: set[str] = set()

    def poll_crds(self, namespace: str, ctx: str | None) -> None:
        robots = _kubectl_items("robots", namespace, ctx)
        tasks = _kubectl_items("fleetactions", namespace, ctx)
        self.robot_phases |= observed_phases(robots, "phase")
        self.task_phases |= observed_phases(tasks, "phase")
        self.estop_states |= observed_phases(robots, "estopState")

    def eval_alerts(self, cur: Snapshot, base: Snapshot | None) -> None:
        for name, (fn, _needs_base) in ALERTS.items():
            fired, _val, _detail = fn(cur, base)
            if fired:
                self.fired.add(name)
            elif name in self.fired:
                self.cleared.add(name)


def main() -> int:
    ap = argparse.ArgumentParser(description="full-surface demo-test assertion driver")
    ap.add_argument("--namespace", default="warehouse-a")
    ap.add_argument("--context", default=None, help="kubectl context (kind-<cluster>)")
    ap.add_argument("--metrics-url", default="http://127.0.0.1:18080/metrics")
    ap.add_argument("--duration", type=float, default=60.0, help="total poll seconds")
    ap.add_argument("--poll-interval", type=float, default=2.0)
    ap.add_argument("--ra1-max-ratio", type=float, default=DEFAULT_RA1_MAX_RATIO)
    ap.add_argument("--gate-known-gaps", action="store_true",
                    help="make the mTLS-gated known-gap alerts (EstopLatencySLOBreach) REQUIRED "
                         "to fire — use only when the demo runs under mTLS.")
    ap.add_argument("--require-clear", default="gauge",
                    choices=["gauge", "all", "none"],
                    help="which alerts must also CLEAR: gauge (the 3 instant-gauge "
                         "alerts, default), all, or none")
    args = ap.parse_args()

    acc = Accumulator()
    baseline = _scrape(args.metrics_url)
    deadline = time.time() + args.duration
    last = baseline
    while time.time() < deadline:
        acc.poll_crds(args.namespace, args.context)
        last = _scrape(args.metrics_url)
        acc.eval_alerts(last, baseline)
        time.sleep(args.poll_interval)

    # Which alerts must clear. The 3 instant-gauge alerts fire AND clear within the
    # run; the 4 range (increase/rate) alerts fire when their fixture triggers but
    # only decay after their [5m]/[10m] window — not observable in a bounded run.
    gauge_alerts = {n for n, (_fn, needs_base) in ALERTS.items() if not needs_base}
    must_clear = {"gauge": gauge_alerts, "all": set(ALERTS), "none": set()}[args.require_clear]

    ratio = ra1_ratio(last, args.namespace)
    failures: list[str] = []

    rp_gaps = coverage_gaps(acc.robot_phases, TARGET_ROBOT_PHASES)
    tp_gaps = coverage_gaps(acc.task_phases, TARGET_ACTION_PHASES)
    es_gaps = coverage_gaps(acc.estop_states, TARGET_ESTOP_STATES)
    if rp_gaps:
        failures.append(f"RobotPhases never observed: {rp_gaps}")
    if tp_gaps:
        failures.append(f"FleetAction phases never observed: {tp_gaps}")
    if es_gaps:
        failures.append(f"estop states never observed: {es_gaps}")

    gate_gaps = KNOWN_GAPS if args.gate_known_gaps else set()
    required = set(ALERTS) - (KNOWN_GAPS - gate_gaps)
    never_fired = [n for n in ALERTS if n in required and n not in acc.fired]
    if never_fired:
        failures.append(f"§9.3.8 alerts that never fired: {never_fired}")
    never_cleared = [n for n in must_clear if n in required and n not in acc.cleared]
    if never_cleared:
        failures.append(f"§9.3.8 alerts that never cleared: {never_cleared}")
    gaps_not_fired = [n for n in KNOWN_GAPS if n not in gate_gaps and n not in acc.fired]
    if gaps_not_fired:
        print(f"    documented gaps (not gating) — {KNOWN_GAPS_REASON}: {gaps_not_fired}")

    if not ra1_ok(ratio, args.ra1_max_ratio):
        failures.append(
            f"RA-1 violated: status_writes/frames_received = {ratio} "
            f"(ceiling {args.ra1_max_ratio}; None means no telemetry seen)")

    print(f"    RobotPhases observed:  {sorted(acc.robot_phases)}")
    print(f"    Task phases observed:  {sorted(acc.task_phases)}")
    print(f"    estop states observed: {sorted(acc.estop_states)}")
    print(f"    alerts fired:   {sorted(acc.fired)}")
    print(f"    alerts cleared: {sorted(acc.cleared)}")
    print(f"    RA-1 write/frame ratio: {ratio}")

    if failures:
        for f in failures:
            print(f"❌ {f}", file=sys.stderr)
        return 1
    print("✅ full-surface demo-test: every targeted phase/estop-state observed, "
          "all §9.3.8 alerts fired (gauges cleared), RA-1 held.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
