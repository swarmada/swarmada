# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The platform precondition reaches every row it implies, and no row it does not.

Why this file exists. ADR-0071's precondition says a platform whose only motion reading
echoes the command cannot satisfy C5.2 and C5.3 together, and that the honest terminal
answer to every estop on such a platform is FAILED. The precondition was implemented for
C5.3 alone, so an adapter that gave exactly that honest answer had its C5.2, C8.3, C14.1 and
C3.6 rows left as the wire recorded them -- four failed Core rows, attributed to the adapter,
on a robot whose drivetrain has no encoder. A reader was told the adapter had four defects,
and the cheapest way to turn those rows green was to make the adapter claim STOPPED again,
which is the inference the precondition exists to forbid.

The sibling file (test_conformance_platform_precondition.py) pins C5.3 and the grade table.
This one pins the WIDTH of the rule:

    * the four rows that join C5.3 on a command-derived plant, with the cause on each;
    * that the verdict stays short of `pass` -- the precondition explains a red, it never
      excuses one;
    * that the SAME adapter answers on a plant with a measured channel still read `failed`,
      because a robot that can see its own motion has no excuse;
    * that C16.1 is NOT in the set. A cancel that emits no terminal ActionStatusUpdate
      leaves the control plane without an outcome whatever the drivetrain can witness, so it
      is graded from the wire and stays that way. It is the row a widened precondition would
      most easily swallow, and the one it must not.
"""

from __future__ import annotations

import pytest

import conformance.model as m
from conformance.adapter import PLATFORM_PRECONDITION_CHECKS, apply_platform_precondition

#: What an adapter taking ADR-0071's honest answer records on a platform that cannot confirm
#: rest: the estop is acknowledged FAILED rather than STOPPED, and the cancel claims no
#: disposition. C16.1 and C5.1 are recorded beside them as controls -- one a row the
#: precondition must not touch, one a row outside its families entirely.
HONEST_ANSWERS = [
    ("C3.6", "cancel_action reports a valid CancelDisposition", "MUST",
     "acknowledged=False disposition=0"),
    ("C5.1", "EstopCommand is acknowledged", "MUST", "acked in 41ms"),
    ("C5.2", "Confirmed EstopAck (STOPPED) on SafetyStream", "MUST", "estop_id='e1' state=3"),
    ("C5.3", "STOPPED is adapter-confirmed, not inferred", "MUST NOT",
     "attested via stop_initiated_at"),
    ("C8.3", "Confirmed EstopAck for an edge-issued estop", "MUST", "state=1 (not STOPPED)"),
    ("C14.1", "Estop honored against an adapter that failed the version gate", "MUST",
     "estop_id='e-refused' state=3"),
    ("C16.1", "An unacknowledged cancel emits a terminal ActionStatusUpdate", "MUST",
     "no terminal ActionStatusUpdate after the cancel"),
]

#: The rows the precondition re-grades, and the ones it leaves alone. Written out rather than
#: derived from PLATFORM_PRECONDITION_CHECKS: a test that reads the set it is checking passes
#: whatever the set says.
REGRADED = ("C3.6", "C5.2", "C5.3", "C8.3", "C14.1")
UNTOUCHED = ("C5.1", "C16.1")


def _rs() -> m.ResultSet:
    rs = m.ResultSet("FleetAdapter")
    for cid, title, level, detail in HONEST_ANSWERS:
        status = m.CheckStatus.PASS if cid in ("C5.1", "C5.3") else m.CheckStatus.FAIL
        rs.results.append(m.CheckResult(cid, title, status, level, detail))
    return rs


def _plant(**odometry: str) -> dict:
    return {"plant": {"kind": {"kind": "operator", "value": "hardware"}},
            "motion_channel": {"odometry": {t: {"kind": "measured", "value": v}
                                            for t, v in odometry.items()}}}


COMMAND_DERIVED = {"/odom": "command-derived", "/odom_raw": "command-derived"}
MEASURED = {"/odom": "command-derived", "/wheel_odom": "measured"}


def _report(rs: m.ResultSet) -> m.ConformanceReport:
    """One required profile whose Core is every row above, so the verdict reads them all."""
    lv = m._level(rs.results)
    lv.check_ids = [cid for cid, *_ in HONEST_ANSWERS]
    return m.ConformanceReport(
        surface="FleetAdapter", implementation={"project": "x", "version": "0"},
        date="2026-01-01T00:00:00Z", protocol_version="fleet_adapter.v1",
        contract_version="1.2.0", suite_revision="r", mode="simulated",
        capability_tier="FullControl", standalone=True,
        profiles=[m.ProfileResult("safety", required=True, core=lv)], results=rs.results)


def _by_id(rs: m.ResultSet) -> dict[str, m.CheckResult]:
    return {r.check_id: r for r in rs.results}


def test_the_set_is_the_five_rows_and_names_c16_1_in_neither_direction() -> None:
    assert set(PLATFORM_PRECONDITION_CHECKS) == set(REGRADED)
    assert "C16.1" not in PLATFORM_PRECONDITION_CHECKS


@pytest.mark.parametrize("check_id", REGRADED)
def test_a_command_derived_plant_regrades_every_row_the_precondition_reaches(
        check_id: str) -> None:
    rs = _rs()
    assert apply_platform_precondition(rs, _plant(**COMMAND_DERIVED)) == "unmet"
    row = _by_id(rs)[check_id]
    assert row.status is m.CheckStatus.SKIP
    assert m.SkipCause.of(row.detail) is m.SkipCause.NO_INDEPENDENT_MOTION_CHANNEL
    # The observation the run recorded survives the re-grade. A row that replaced what was
    # seen with the reason it is excused would be unfalsifiable.
    observed = dict((cid, detail) for cid, _t, _l, detail in HONEST_ANSWERS)[check_id]
    assert f"What the run observed: {observed}" in row.detail


@pytest.mark.parametrize("check_id", REGRADED)
def test_each_regraded_row_is_not_supported_by_platform_and_carries_its_unlock(
        check_id: str) -> None:
    rs = _rs()
    apply_platform_precondition(rs, _plant(**COMMAND_DERIVED))
    row = _report(rs).sheet[check_id]
    assert row.grade is m.Grade.NOT_SUPPORTED_BY_PLATFORM
    assert row.unlock and "independent of the command path" in row.unlock
    assert row.obligation, "the cause must stay outside the never-an-obligation set"


def test_the_verdict_stays_short_of_pass_on_a_command_derived_plant() -> None:
    """Five rows disclosed as the platform's, and the verdict is still not a pass.

    Two cases, because they are the two a reader has to be able to tell apart. With the
    adapter's own C16.1 defect present the verdict is `fail` -- the precondition explains
    four rows and excuses none, and a real defect beside them still fails the run. With
    C16.1 fixed the verdict is `not-established`: nothing failed, and the safety obligation
    is unestablished on this platform rather than satisfied. Neither is `pass`, which is the
    property the cause is kept out of the never-an-obligation set to guarantee.
    """
    rs = _rs()
    apply_platform_precondition(rs, _plant(**COMMAND_DERIVED))
    report = _report(rs)
    assert report.verdict == "fail", "a genuine adapter defect still fails the run"
    blocked = {b["checkId"]: b["grade"] for b in report.verdict_blockers}
    assert blocked["C16.1"] == m.Grade.FAILED.value
    assert all(blocked[cid] == m.Grade.NOT_SUPPORTED_BY_PLATFORM.value for cid in REGRADED)

    rs = _rs()
    fixed = _by_id(rs)["C16.1"]
    rs.results[rs.results.index(fixed)] = m.CheckResult(
        fixed.check_id, fixed.title, m.CheckStatus.PASS, fixed.level,
        "terminal ActionStatusUpdate state=ACTION_STATE_FAILED after the cancel")
    apply_platform_precondition(rs, _plant(**COMMAND_DERIVED))
    report = _report(rs)
    assert report.verdict == "not-established"
    assert not report.conformant
    assert {b["checkId"] for b in report.verdict_blockers} == set(REGRADED)


@pytest.mark.parametrize("check_id", UNTOUCHED)
def test_rows_outside_the_set_are_graded_from_the_wire(check_id: str) -> None:
    rs = _rs()
    before = _by_id(rs)[check_id]
    status, detail = before.status, before.detail
    apply_platform_precondition(rs, _plant(**COMMAND_DERIVED))
    after = _by_id(rs)[check_id]
    assert after.status is status
    assert after.detail == detail, "an untouched row keeps the wire's own words"


def test_a_measured_channel_excuses_nothing() -> None:
    """The same answers, on a robot that can see its own motion, are still the adapter's."""
    rs = _rs()
    assert apply_platform_precondition(rs, _plant(**MEASURED)) == "met"
    rows = _by_id(rs)
    for cid in REGRADED:
        expected = m.CheckStatus.PASS if cid == "C5.3" else m.CheckStatus.FAIL
        assert rows[cid].status is expected, f"{cid} was re-graded on a measured plant"
        assert "platform precondition met" in rows[cid].detail
    report = _report(rs)
    assert report.verdict == "fail"
    assert all(report.sheet[cid].grade is m.Grade.FAILED
               for cid in REGRADED if cid != "C5.3")


def test_an_undeclared_motion_channel_re_grades_nothing_and_says_so() -> None:
    rs = _rs()
    assert apply_platform_precondition(rs, {"plant": {}}) == "unknown"
    rows = _by_id(rs)
    for cid in REGRADED:
        assert rows[cid].status is not m.CheckStatus.SKIP
        assert "not established" in rows[cid].detail


def test_no_plant_declaration_changes_nothing() -> None:
    rs = _rs()
    assert apply_platform_precondition(rs, None) is None
    assert _by_id(rs)["C5.2"].detail == "estop_id='e1' state=3"


def test_a_run_that_recorded_none_of_these_rows_reports_no_finding() -> None:
    """None, not "unmet": the precondition graded nothing, and saying otherwise would put a
    platform finding on a report that has no row to hang it from."""
    rs = m.ResultSet("FleetAdapter")
    rs.results.append(m.CheckResult("C1.1", "t", m.CheckStatus.PASS, "MUST", "ok"))
    assert apply_platform_precondition(rs, _plant(**COMMAND_DERIVED)) is None
