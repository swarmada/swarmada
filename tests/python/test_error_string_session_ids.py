# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The citation check refuses an internal session or review label in a public file.

Why this file exists. Public comments and documents carried the labels of internal work: a
remediation session, a review round, a gate ruling, an audit finding. None of them is published,
so each gave a reader a name with nothing to look it up in. `check-error-strings.py` knew tracker
items and ruling ids, not these. It now refuses all four forms, and this file plants one of each
in each kind of file it reads, beside the words it must leave alone. The planted labels are
invented, and they are assembled at run time, so this file does not carry one for the check to
find. The checker exempts its own file from the scan, so the last test reads that file for these
labels directly.
"""

from __future__ import annotations

import importlib.util
import pathlib

import pytest

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
_C = "//"
# split, so the check does not read this file's own controls
_REM, _RR, _GATE, _AUDIT = "RE" + "M", "R" + "R-", "Ga" + "te", "Au" + "dit"

PLANTED = {
    "a remediation session in a Go comment": (
        "m.go", f"package m\n\n{_C} The widget is painted blue ({_REM}99).\n"),
    "a remediation session with a part letter in Markdown": (
        "notes.md", f"The widget colour was fixed in {_REM}12b.\n"),
    "a review round in a Python comment": (
        "m.py", f"# blue, as {_RR}99 ruled\ncolour = 'blue'\n"),
    "a gate ruling in YAML": (
        "values.yaml", f"# blue ({_GATE} 9 Ruling 2)\ncolour: blue\n"),
    "an audit finding in Markdown": (
        "notes.md", f"{_AUDIT} 9 found that the widget was red.\n"),
    "an audit finding named as such in a shell comment": (
        "run.sh", f"# {_AUDIT} 9 finding D3: the widget is blue\necho blue\n"),
}

LOOKALIKES = {
    "a word that starts with the letters": (
        "m.go", f"package m\n\n{_C} REMOTE and REMOVE are words; {_REM} alone is not a label.\n"),
    "a letter pair inside a longer token": (
        "m.go", f"package m\n\n{_C} The part number ARR-12 is a vendor's.\n"),
    "a gate without a ruling": (
        "notes.md", f"The widget passes {_GATE} 9 of the release checklist.\n"),
    "an audit without a finding": (
        "notes.md", f"An {_AUDIT} 9 log entry records the change.\n"),
    "the runtime-assertion labels": (
        "m.go", f"package m\n\n{_C} RA-1 holds; see RFC-0001 and ADR-0046.\n"),
}


def _load(path: pathlib.Path):
    spec = importlib.util.spec_from_file_location("check_error_strings_under_test", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def checker():
    return _load(_REPO_ROOT / "scripts/check-error-strings.py")


def _deny(checker, tmp_path, name: str, body: str) -> list[str]:
    (tmp_path / name).write_text(body, encoding="utf-8")
    deny, _warn, scanned = checker.check_citations([str(tmp_path)])
    assert scanned == 1, f"the planted file was not scanned (scanned={scanned})"
    return deny


def _session_rules(checker):
    return [rx for rx, why in checker.CITATIONS if "internal session" in why]


@pytest.mark.parametrize("shape", sorted(PLANTED))
def test_a_session_label_is_refused(checker, tmp_path, shape):
    name, body = PLANTED[shape]
    deny = _deny(checker, tmp_path, name, body)
    assert any("internal session" in d for d in deny), f"{shape}: the gate did not refuse\n{body}"


@pytest.mark.parametrize("shape", sorted(LOOKALIKES))
def test_what_it_resembles_is_not(checker, tmp_path, shape):
    name, body = LOOKALIKES[shape]
    deny = _deny(checker, tmp_path, name, body)
    assert not any("internal session" in d for d in deny), f"{shape}: refused: {deny}"


def test_the_exempt_checker_carries_no_session_label(checker):
    """The checker skips its own file, so its comments are read here with its own rules."""
    rules = _session_rules(checker)
    assert len(rules) == 4, "expected the four session-label rules"
    source = (_REPO_ROOT / "scripts/check-error-strings.py").read_text(encoding="utf-8")
    found = [m.group(0) for rx in rules for m in rx.finditer(source)]
    assert not found, f"scripts/check-error-strings.py names session labels: {found}"
