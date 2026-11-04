# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The profile layer's skip-consistency row says how many causes it could contradict.

Why this file exists. `conformance/registry.py` records CE0.1, CP0.1 and RB0.2, "Skip
reasons are consistent with the run". Its tests can contradict three cause types --
`surface-not-implemented`, `binding-not-declared` and `contract-not-declared` -- and
only when the run recorded the context each needs. The passing row used to print
"N recorded skip cause(s) checked against what the run observed" with N every cause
recorded, so the ControlPlane report said 10 were checked when none of the 10 could
have been contradicted. A report that says more than it measured lets a skip pass as
a measurement.

The adapter harness's C0.1 already prints "k of N ... are of a kind this run can
contradict"; these tests hold the profile layer to the same wording.
"""

from __future__ import annotations

from conformance.model import CheckStatus, ResultSet, SkipCause
from conformance.registry import Registry, self_checks


def _row(rs: ResultSet, check_id: str):
    return next(r for r in rs.results if r.check_id == check_id)


def _run(context: dict, skips: list[tuple[str, SkipCause]]) -> str:
    rs = ResultSet("ComputeEndpoint")
    rs.context.update(context)
    for cid, cause in skips:
        rs.skip(cid, "t", cause)
    self_checks(Registry(), "ComputeEndpoint", rs, "CE")
    row = _row(rs, "CE0.1")
    assert row.status is CheckStatus.PASS, row.detail
    return row.detail


def test_a_cause_no_test_can_contradict_is_not_counted_as_checked():
    detail = _run({}, [("CE9.1", SkipCause.NEEDS_LIVE_CLUSTER),
                       ("CE9.2", SkipCause.NEEDS_LIVE_CLUSTER)])
    assert detail.startswith(
        "0 of 2 recorded skip cause(s) are of a kind this run can contradict"), detail
    assert "checked against what the run observed" not in detail, detail
    assert "The other 2 are recorded, not checked" in detail, detail


def test_a_checkable_cause_is_counted_and_the_rest_are_named():
    detail = _run({"binding": "Http", "contract": "App"},
                  [("CE4.1", SkipCause.BINDING_NOT_DECLARED),
                   ("CE7.1", SkipCause.CONTRACT_NOT_DECLARED),
                   ("CE9.1", SkipCause.NEEDS_LIVE_CLUSTER)])
    assert detail.startswith("2 of 3 recorded skip cause(s)"), detail
    assert "The other 1 is recorded, not checked" in detail, detail
    assert SkipCause.NEEDS_LIVE_CLUSTER.value in detail, detail


def test_a_contradicted_cause_still_fails_the_row():
    """The count is about wording; the check itself must still fire."""
    rs = ResultSet("ComputeEndpoint")
    rs.context.update({"binding": "Http"})
    rs.skip("CE3.1", "t", SkipCause.BINDING_NOT_DECLARED)
    self_checks(Registry(), "ComputeEndpoint", rs, "CE")
    assert _row(rs, "CE0.1").status is CheckStatus.FAIL
