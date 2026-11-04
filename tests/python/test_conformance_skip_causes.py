# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""A skip cause that is not enumerated must fail LOUDLY (ADR-0048).

Why this file exists. `PROFILES.md` has always required a skip cause "from the
enumerated `SkipCause` set -- never free text, because a prose justification is
not falsifiable". Nothing enforced it: `_level()` wrote the literal string
"unenumerated" into the report and carried on. Two checks sat in that state --
C5.6 and C3.7 -- because the FleetAdapter harness and the profiler each carry
their own SkipCause vocabulary and the two had drifted, and a quiet fallthrough
turned the drift into a valid-LOOKING report.

The guard is the fix. These tests are what stop the guard being removed by
someone who finds it inconvenient, and what stops the two vocabularies drifting
again without anyone noticing.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

from conformance.model import (
    NOT_APPLICABLE_CAUSES,
    CheckResult,
    CheckStatus,
    SkipCause,
    UnenumeratedSkipCause,
    _level,
)

ROOT = Path(__file__).resolve().parents[2]


def _skip(check_id: str, detail: str) -> CheckResult:
    return CheckResult(
        check_id=check_id, title="t", status=CheckStatus.SKIP, level="MUST", detail=detail
    )


def test_unenumerated_cause_raises_rather_than_reporting_unenumerated():
    """The defect: a bad cause used to become a report string."""
    with pytest.raises(UnenumeratedSkipCause) as e:
        _level([_skip("C9.9", "a prose justification nobody enumerated")])
    # The error must name the check and offer the vocabulary, or the next person
    # hits the same wall with no way over it.
    assert "C9.9" in str(e.value)
    assert "legal-alternative-path" in str(e.value)


def test_the_exact_misspelling_that_hid_would_now_raise():
    """`adapter-did-not-reach-state` vs `target-did-not-reach-state` (C5.6),
    and `needs-adapter-restart` vs `needs-target-restart` (C3.7). Both are now
    reconciled by the alias table, so both must RESOLVE -- and a near-miss on
    either must still raise."""
    assert SkipCause.of("adapter-did-not-reach-state: x") is SkipCause.TARGET_DID_NOT_REACH_STATE
    assert SkipCause.of("needs-adapter-restart: x") is SkipCause.NEEDS_TARGET_RESTART
    with pytest.raises(UnenumeratedSkipCause):
        _level([_skip("C5.6", "adapter-did-not-reach-STATE: wrong case is not an alias")])


def test_every_harness_cause_is_understood_by_the_profiler():
    """The drift guard. Both surfaces publish their own SkipCause vocabulary; a
    value the harness can WRITE that the profiler cannot READ is the
    defect, and it is only visible by comparing the two enums."""
    harness = (ROOT / "adapters/conformance/report.py").read_text()
    # Stop at the NEXT class, or CheckStatus's own pass/fail/skip members bleed
    # in and the test fails on values that were never skip causes.
    block = re.split(r"^class ", harness.split("class SkipCause")[1], flags=re.M)[0]
    written = set(re.findall(r'^\s+[A-Z_]+ = "([a-z0-9-]+)"', block, re.M))
    unreadable = {v for v in written if SkipCause.of(v + ": x") is None}
    assert not unreadable, (
        f"the harness can write {sorted(unreadable)}, which conformance/model.py "
        f"cannot resolve. Add the member, or an alias in _CAUSE_ALIASES."
    )


def test_legal_alternative_path_is_notApplicable_not_a_skip():  # noqa: N802 -- the verdict's spelling
    """ADR-0048 rule B. A check unreachable because the implementation took
    another legal route was never an obligation of it, so it must not hold the
    profile at notVerified -- the CE6.2/CE6.3 rule, applied to C5.6."""
    assert SkipCause.LEGAL_ALTERNATIVE_PATH in NOT_APPLICABLE_CAUSES
    lv = _level(
        [
            CheckResult("C5.1", "t", CheckStatus.PASS, "MUST", ""),
            _skip("C5.6", "legal-alternative-path: acked terminal directly"),
        ]
    )
    assert lv.result == "pass", "a legal alternative path must not hold a profile at notVerified"
    assert lv.statistics["notApplicable"] == 1
    assert lv.statistics["skip"] == 0
    assert lv.skipped == [], "a notApplicable check is not a skip and must not be listed as one"


def test_optional_declined_is_deliberately_still_a_skip():
    """ADR-0048 holds OPTIONAL_DECLINED back: C10.1 records it for a path made
    unreachable by an earlier refusal, which is not a decline. Promoting it
    would turn that mislabelling into a pass. If someone promotes it later, this
    test should fail and send them to the ADR."""
    assert SkipCause.OPTIONAL_DECLINED not in NOT_APPLICABLE_CAUSES
    lv = _level(
        [
            CheckResult("C7.1", "t", CheckStatus.PASS, "MAY", ""),
            _skip("C10.1", "optional-declined: not reachable"),
        ]
    )
    assert lv.result == "notVerified"


def test_extended_never_reports_fail_and_the_failure_is_still_disclosed():
    """ADR-0048 rule A. Extended is optional, so a failed Extended check makes
    its FEATURE unsupported and does not make the LEVEL fail -- but the count
    must survive in statistics, or the verdict change would be a cover-up."""
    rows = [
        CheckResult("C13.2", "t", CheckStatus.FAIL, "SHOULD", "accepted work after refusal"),
        _skip("C1.5", "needs-extended-run: no curve"),
    ]
    ext = _level(rows, level="extended")
    assert ext.result == "notVerified", "Extended must never report `fail`"
    assert ext.statistics["fail"] == 1, "the failure must still be counted"

    core = _level(rows, level="core")
    assert core.result == "fail", "Core is mandatory; `fail` there is correct and must remain"
