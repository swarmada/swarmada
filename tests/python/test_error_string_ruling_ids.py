# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The citation check refuses a maintainer ruling id in a public file.

Why this file exists. A Go comment carried a maintainer ruling's id as its reason, and
`check-error-strings.py` reported no violations: it knew tracker items and session names, not
ruling ids. The rulings are not published, so the id told a reader nothing. The check now refuses
the `R-P<n>-<n>` and `R-<4 digits>-<n>` forms, and this file plants one of each in each kind of file
it reads, beside the labels it must leave alone. The planted ids are invented (pass 99, and dates
that do not exist), and they are assembled at run time, so this file does not carry one for the
check to find. The checker exempts its own file from the scan, so the last test reads that file
for a ruling id directly.
"""

from __future__ import annotations

import importlib.util
import pathlib

import pytest

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
_C = "//"
_R = "R-"  # split, so the check does not read this file's own controls

PLANTED = {
    "pass ruling in a Go comment": (
        "m.go",
        f"package m\n\n{_C} The widget is painted blue ({_R}P99-1).\n",
    ),
    "dated ruling in a Go comment": (
        "m.go",
        f"package m\n\n{_C} Ruled {_R}1399-2: the widget is painted blue.\n",
    ),
    "two-digit sequence in Markdown": (
        "notes.md",
        f"The widget colour follows {_R}0000-17 point 3.\n",
    ),
    "ruling in a YAML comment": (
        "values.yaml",
        f"# blue per {_R}1340-5\ncolour: blue\n",
    ),
}

LOOKALIKES = {
    "runtime-assertion and rule labels": (
        "m.go",
        f"package m\n\n{_C} RA-1 holds; see RFC-0001 and ADR-0046.\n",
    ),
    "an id inside a longer word": (
        "m.go",
        f"package m\n\n{_C} The part number XR-1002-1 is a vendor's.\n",
    ),
    "a short number": (
        "m.go",
        f"package m\n\n{_C} R-12-3 is not a ruling form.\n",
    ),
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


@pytest.mark.parametrize("shape", sorted(PLANTED))
def test_a_ruling_id_is_refused(checker, tmp_path, shape):
    name, body = PLANTED[shape]
    deny = _deny(checker, tmp_path, name, body)
    assert any("maintainer ruling" in d for d in deny), f"{shape}: the gate did not refuse\n{body}"


@pytest.mark.parametrize("shape", sorted(LOOKALIKES))
def test_what_it_resembles_is_not(checker, tmp_path, shape):
    name, body = LOOKALIKES[shape]
    deny = _deny(checker, tmp_path, name, body)
    assert not any("maintainer ruling" in d for d in deny), f"{shape}: refused: {deny}"


def test_the_exempt_checker_carries_no_ruling_id(checker):
    """The checker skips its own file, so its comments are read here with its own rule."""
    rule = next(rx for rx, why in checker.CITATIONS if "maintainer ruling" in why)
    source = (_REPO_ROOT / "scripts/check-error-strings.py").read_text(encoding="utf-8")
    found = [m.group(0) for m in rule.finditer(source)]
    assert not found, f"scripts/check-error-strings.py names ruling ids: {found}"
