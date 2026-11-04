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

"""Rows a run could not reach are recorded with the cause, never left out.

The profile layer grades a Core row the run recorded nothing for as a suite gap
(``not-tested-suite-gap``). When a check returns early because the ADAPTER did not
reach the state it needs -- it never redialled, or registered no robot -- every row
that branch owns must still be written, with ``adapter-did-not-reach-state``, or the
report blames the suite for the adapter's behaviour. C0.1 is pinned here too: its
PASS line counts only the causes it can contradict.
"""

from __future__ import annotations

from adapters.conformance.report import CheckStatus, SkipCause


def _rows(report):
    return {r.check_id: r for r in report.results}


def _unreached(row):
    return (row.status is CheckStatus.SKIP
            and row.detail.startswith(SkipCause.ADAPTER_DID_NOT_REACH_STATE.value))


def test_no_edge_advertised_records_c81_c82_and_c83(pb, driver):
    drv, _session, report = driver
    drv._check_edge(advertised_edge=False)
    rows = _rows(report)
    for cid in ("C8.1", "C8.2", "C8.3"):
        assert cid in rows, f"{cid} not recorded; got {sorted(rows)}"
        assert _unreached(rows[cid]), rows[cid].detail


def test_no_redial_records_every_reconnect_row(pb, driver, harness_mod, monkeypatch):
    monkeypatch.setattr(harness_mod, "_RECONNECT_WAIT", 0.05)
    drv, _session, report = driver
    drv._check_accepting_reconnect()
    rows = _rows(report)
    for cid in ("C2.4", "C6.1", "C2.2", "C3.8", "C2.3"):
        assert cid in rows, f"{cid} not recorded; got {sorted(rows)}"
        assert _unreached(rows[cid]), rows[cid].detail


def test_no_redial_on_the_refused_session_records_c31_and_c37(pb, driver, harness_mod,
                                                             monkeypatch):
    monkeypatch.setattr(harness_mod, "_RECONNECT_WAIT", 0.05)
    drv, _session, report = driver
    drv._check_negotiation_refusal()
    rows = _rows(report)
    assert _unreached(rows["C3.1"]), rows["C3.1"].detail
    # C3.7 is a genuine suite gap on every path, and says so.
    assert rows["C3.7"].status is CheckStatus.SKIP
    assert rows["C3.7"].detail.startswith(SkipCause.NEEDS_ADAPTER_RESTART.value)


def test_c01_counts_only_the_causes_it_can_contradict(pb, driver):
    drv, _session, report = driver
    drv.telemetry_seen = True
    # One cause C0.1 has a test for, and one it has none for.
    report.add("C12.1", "scan answers", CheckStatus.SKIP,
               detail=f"{SkipCause.ADAPTER_DECLARES_NO_ACTION.value}: none")
    drv._catalog = []
    report.add("C18.2", "no sensor bytes", CheckStatus.SKIP,
               detail=SkipCause.ADAPTER_DECLARES_NO_SENSOR_STREAM.value)
    drv._record_unverified()
    row = _rows(report)["C0.1"]
    assert row.status is CheckStatus.PASS, row.detail
    # _record_unverified adds C1.3, C1.4 and C1.5 itself, none of them checkable.
    assert row.detail.startswith("1 of 5 recorded skip cause(s)"), row.detail
    assert SkipCause.ADAPTER_DECLARES_NO_SENSOR_STREAM.value in row.detail
