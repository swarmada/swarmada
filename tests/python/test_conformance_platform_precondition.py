# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The safety profile's platform precondition, graded from a plant declaration (ADR-0071).

A STOPPED on a platform whose only motion reading echoes the command is the inference C5.3
forbids, however the wire looks: nothing on it can confirm rest. The wire cannot tell; the
plant declaration's guided motion test can. A profiled run given a plant.yaml whose odometry
is all command-derived grades C5.3 `not-supported-by-platform`, and the cause is kept out of
the never-an-obligation set so the verdict cannot reach `pass`.
"""

from __future__ import annotations

import conformance.model as m
from conformance.adapter import apply_platform_precondition


def _rs(c53_status: str = "pass") -> m.ResultSet:
    rs = m.ResultSet("FleetAdapter")
    rs.results.append(m.CheckResult("C5.1", "t", m.CheckStatus.PASS, "MUST", "acked"))
    rs.results.append(m.CheckResult("C5.3", "STOPPED is adapter-confirmed, not inferred",
                                    m.CheckStatus(c53_status), "MUST NOT",
                                    "attested via stop_initiated_at"))
    return rs


def _plant(**odometry) -> dict:
    return {"plant": {"kind": {"kind": "operator", "value": "hardware"}},
            "motion_channel": {"odometry": {t: {"kind": "measured", "value": v}
                                            for t, v in odometry.items()}}}


def _report(rs: m.ResultSet) -> m.ConformanceReport:
    lv = m._level(rs.results)
    lv.check_ids = ["C5.1", "C5.3"]
    return m.ConformanceReport(
        surface="FleetAdapter", implementation={"project": "x", "version": "0"},
        date="2026-01-01T00:00:00Z", protocol_version="fleet_adapter.v1",
        contract_version="1.2.0", suite_revision="r", mode="simulated",
        capability_tier="FullControl", standalone=True,
        profiles=[m.ProfileResult("safety", required=True, core=lv)], results=rs.results)


def test_command_derived_odometry_grades_c53_not_supported_and_blocks_pass() -> None:
    rs = _rs()
    assert apply_platform_precondition(rs, _plant(**{"/odom": "command-derived"})) == "unmet"
    row = next(r for r in rs.results if r.check_id == "C5.3")
    assert row.status is m.CheckStatus.SKIP
    assert m.SkipCause.of(row.detail) is m.SkipCause.NO_INDEPENDENT_MOTION_CHANNEL
    assert "What the run observed: attested via stop_initiated_at" in row.detail
    rep = _report(rs)
    assert rep.sheet["C5.3"].grade is m.Grade.NOT_SUPPORTED_BY_PLATFORM
    assert rep.sheet["C5.3"].unlock and "encoders" in rep.sheet["C5.3"].unlock
    assert rep.verdict == "not-established", "the precondition must keep the verdict off pass"


def test_a_measured_channel_meets_the_precondition() -> None:
    rs = _rs()
    plant = _plant(**{"/odom": "command-derived", "/wheel_odom": "measured"})
    assert apply_platform_precondition(rs, plant) == "met"
    row = next(r for r in rs.results if r.check_id == "C5.3")
    assert row.status is m.CheckStatus.PASS
    assert "precondition met" in row.detail
    assert _report(rs).verdict == "pass"


def test_an_undeclared_motion_channel_is_stated_not_assumed() -> None:
    rs = _rs()
    assert apply_platform_precondition(rs, {"plant": {}}) == "unknown"
    row = next(r for r in rs.results if r.check_id == "C5.3")
    assert row.status is m.CheckStatus.PASS
    assert "not established" in row.detail


def test_no_plant_declaration_changes_nothing() -> None:
    rs = _rs()
    assert apply_platform_precondition(rs, None) is None
    assert next(r for r in rs.results if r.check_id == "C5.3").detail == \
        "attested via stop_initiated_at"


def test_the_cause_stays_out_of_the_never_an_obligation_set() -> None:
    assert m.SkipCause.NO_INDEPENDENT_MOTION_CHANNEL not in m.NOT_APPLICABLE_CAUSES
    assert m.CAUSE_GRADE[m.SkipCause.NO_INDEPENDENT_MOTION_CHANNEL] is \
        m.Grade.NOT_SUPPORTED_BY_PLATFORM
