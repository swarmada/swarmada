"""Every check the harness runs through _when_served has its rows in _NEEDS_SERVED_ACTION.

_when_served looks a check up in _NEEDS_SERVED_ACTION to record its rows as skipped when the
adapter serves no action. A check run through it with no entry there raised KeyError for any
adapter whose catalog was not served (one reporting contract 1.0.0, for example), so the run
ended with no report. This test reads the harness source, so a check added later without its
rows fails here rather than in someone's conformance run.
"""
import ast
import pathlib

from adapters.conformance import harness

_SOURCE = pathlib.Path(harness.__file__).read_text()


def _checks_run_when_served() -> set[str]:
    names = set()
    for node in ast.walk(ast.parse(_SOURCE)):
        if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                and node.func.attr == "_when_served" and node.args
                and isinstance(node.args[0], ast.Attribute)):
            names.add(node.args[0].attr)
    return names


def test_every_check_run_when_served_has_its_rows():
    checks = _checks_run_when_served()
    assert "_check_latch_holds_work" in checks, checks  # the read itself works
    missing = sorted(checks - set(harness._NEEDS_SERVED_ACTION))
    assert not missing, f"run through _when_served with no rows in _NEEDS_SERVED_ACTION: {missing}"


def test_the_latch_row_is_c5_8_and_a_must():
    rows = harness._NEEDS_SERVED_ACTION["_check_latch_holds_work"]
    assert [(cid, level) for cid, _, level in rows] == [("C5.8", "MUST")]
