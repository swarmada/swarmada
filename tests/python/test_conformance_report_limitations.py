# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Every report states the suite's limitations, in its text and in its JSON.

Why this file exists. The conformance fixture replaces a goal after a fixed dwell. On a plant
whose in-place reversal takes longer than that dwell, the replacement lands before the
reversal completes, so a motion row attests travel in one direction and not a turn. The
effect is real, measured, and failed no row on hardware — which is exactly why it needed
saying: a limitation that costs nothing today is the kind that goes unrecorded and is
re-derived from scratch, at a robot, by whoever next reads a green motion row.

The grading vocabulary already refuses to let a report be silent about what it could not
establish: a skip carries an enumerated cause, and every unmeasured row carries an unlock.
This is the same rule applied to the SUITE rather than to a row. A limitation is a property
of the fixture, not of the run, so it does not belong in a row's evidence, and it is stated
whether the run passed or failed.

WHAT THIS DOES NOT CHECK: that the limitation is true. It is a claim about the fixture's
dwell against a plant's turn time, and the plant declaration is where that is measured.
"""

from __future__ import annotations

import json

import pytest
import yaml

import conformance.model as m


def _report(status: m.CheckStatus = m.CheckStatus.PASS) -> m.ConformanceReport:
    rs = [m.CheckResult("C5.1", "EstopCommand is acknowledged", status, "MUST", "acked")]
    lv = m._level(rs)
    lv.check_ids = ["C5.1"]
    return m.ConformanceReport(
        surface="FleetAdapter", implementation={"project": "x", "version": "0"},
        date="2026-01-01T00:00:00Z", protocol_version="fleet_adapter.v1",
        contract_version="1.2.0", suite_revision="r", mode="simulated",
        capability_tier="FullControl", standalone=True,
        profiles=[m.ProfileResult("safety", required=True, core=lv)], results=rs)


def test_the_suite_states_the_one_directional_travel_limitation() -> None:
    assert len(m.SUITE_LIMITATIONS) == 1
    only = m.SUITE_LIMITATIONS[0]
    assert "reversal" in only and "dwell" in only
    assert "one-directional travel" in only


@pytest.mark.parametrize("status", [m.CheckStatus.PASS, m.CheckStatus.FAIL],
                         ids=["a run that passed", "a run that failed"])
def test_the_json_carries_every_limitation_whatever_the_run_did(
        status: m.CheckStatus) -> None:
    doc = json.loads(_report(status).to_json())
    assert doc["limitations"] == list(m.SUITE_LIMITATIONS)
    assert doc["verdict"] in ("pass", "fail")


def test_the_yaml_carries_them_too() -> None:
    doc = yaml.safe_load(_report().to_yaml())
    assert doc["limitations"] == list(m.SUITE_LIMITATIONS)


def test_the_human_rendering_prints_each_limitation_above_the_rows() -> None:
    lines = _report().to_text().splitlines()
    printed = [ln for ln in lines if ln.startswith("limitation: ")]
    assert printed == [f"limitation: {text}" for text in m.SUITE_LIMITATIONS]
    # Above the sheet, not appended under the verdict: a reader who stops at the verdict has
    # still read it.
    assert lines.index(printed[0]) < next(i for i, ln in enumerate(lines)
                                          if ln.startswith("grades: "))


def test_a_limitation_is_not_smuggled_into_a_rows_evidence() -> None:
    """It is a property of the suite. A row's evidence is what the run observed."""
    doc = json.loads(_report().to_json())
    for check in doc["checks"]:
        assert "limitation" not in json.dumps(check)
