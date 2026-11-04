# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Two label vocabularies, and the citation gate must tell them apart.

Why this file exists. `check-error-strings.py` resolves every `RA-n` / bare rule label on the
public surface against what some public document actually defines — a real gate that has caught
real defects. But `-` is not a word character, so the bare-label pattern also matched INSIDE a
namespaced chapter rule id such as `FZ-R7`, which is a FleetZone resolution rule and not a
runtime assertion at all. Nineteen correct references in one chapter were reported as undefined;
`FZ-R1`..`FZ-R5` passed only by colliding with bare runtime assertions that happen to exist; and
`make test` — the command AGENTS.md requires after any code change — could not pass at all.

These tests hold both halves, because each fails silently on its own. Widening the pattern until
nothing is ever reported would also have turned the gate green.
"""

from __future__ import annotations

import importlib.util
import pathlib

import pytest

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
# The one place the gate reads rule ids from: generated from the RFC-0001 sources, and present
# in every checkout, published or not (scripts/gen-rule-index.py).
_INDEX = _REPO_ROOT / "docs/rule-index.md"

# A test about undefined labels must not WRITE one into a file the gate scans, or the test is
# itself a finding — which is exactly what happened on this file's first run. Composing them at
# import time keeps the literal out of the source text the gate reads.
_UNDEFINED_BARE = "R" + "47"
_UNDEFINED_RULE = "FZ-" + "R" + "42"


@pytest.fixture(scope="module")
def checker():
    spec = importlib.util.spec_from_file_location(
        "check_error_strings_under_test", _REPO_ROOT / "scripts/check-error-strings.py"
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _labels_reported(checker, tmp_path, body: str) -> set[str]:
    """Every label the citation gate reports as undefined in a planted document."""
    (tmp_path / "planted.md").write_text(body, encoding="utf-8")
    deny, _warn, scanned = checker.check_citations([str(tmp_path)])
    assert scanned == 1, f"the planted document was not scanned (scanned={scanned})"
    return {line.split("'")[1] for line in deny if "' is a " in line}


def test_chapter_rule_ids_are_defined_by_the_chapter_that_introduces_them(checker):
    """They come from the rules index, in two shapes, and both must count as definitions."""
    defined = checker.defined_labels(_REPO_ROOT)
    # `| **FZ-R1** |` — bold label, no dash.
    for rule in ("FZ-R1", "FZ-R7", "FZ-R9"):
        assert rule in defined, f"{rule} is defined in {_INDEX.name} and must resolve"
    # `| **FA-R1 — declare** |` — bold label, em-dash, then a name.
    for rule in ("FA-R1", "FA-R5", "FA-R6"):
        assert rule in defined, f"{rule} is defined in {_INDEX.name} and must resolve"


def test_a_chapter_rule_id_is_not_reported_as_an_undefined_runtime_assertion(checker, tmp_path):
    """The reported defect itself: a defined chapter rule id must draw no finding."""
    reported = _labels_reported(checker, tmp_path, "Resolution follows FZ-R7 and FA-R6.\n")
    assert reported == set(), f"defined chapter rule ids reported as undefined: {reported}"


def test_an_undefined_label_is_still_reported_in_either_vocabulary(checker, tmp_path):
    """The guard against the cheap way to make this gate green: report nothing.

    Both arms must still fire — the bare one, which is the half that has caught real defects,
    and the namespaced one, so widening the pattern did not simply stop checking it.
    """
    planted = f"An undefined bare {_UNDEFINED_BARE}, and a namespaced {_UNDEFINED_RULE}.\n"
    reported = _labels_reported(checker, tmp_path, planted)
    assert reported == {_UNDEFINED_BARE, _UNDEFINED_RULE}, (
        "an undefined label must be reported whichever vocabulary it is written in; "
        f"got {reported}"
    )


def test_the_finding_names_the_vocabulary_it_came_from(checker, tmp_path):
    """Calling a chapter rule id a "runtime-assertion label" is how the two got confused."""
    (tmp_path / "planted.md").write_text(
        f"Undefined: {_UNDEFINED_BARE} and {_UNDEFINED_RULE}.\n", encoding="utf-8"
    )
    deny, _warn, _scanned = checker.check_citations([str(tmp_path)])
    said = {line.split("'")[1]: line for line in deny if "' is a " in line}
    assert "runtime-assertion label" in said[_UNDEFINED_BARE]
    assert "chapter rule id" in said[_UNDEFINED_RULE]
