# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The rules index gate can go red, in each of the ways it exists to catch.

`scripts/gen-rule-index.py --check` runs in `make test` against this tree. That run proves the
committed index is current; it cannot prove the check would notice if it were not. These tests
build a small tree, generate its index, and then break it one way at a time: a public file
citing an identifier the index lacks, an index edited by hand, a staged rule changed at its
source, and a checkout with nothing to generate from.

Identifiers are composed at import time, so this file cites none the citation gate would read.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
from pathlib import Path

import pytest

_REPO_ROOT = Path(__file__).resolve().parents[2]
_GEN = "scripts/gen-rule-index.py"
_CHECKER = "scripts/check-error-strings.py"

_NS = "FZ-" + "Q"  # a namespace family no real document uses
_R1, _R2, _UNDEFINED = _NS + "1", _NS + "2", _NS + "99"
_STAGED = "FA-" + "Q" + "1"

_DIST = f"""<a id="widgets"></a>
## 1 Widgets

| ID | Rule |
| :---- | :---- |
| **{_R1}** | A widget MUST be valid alone. |
| **{_R2}** | A widget MUST NOT widen its parent (see {{{{ref:widgets}}}}). |
"""
_AMENDMENT = f"""---
title: "Widget Obligations"
status: proposed
---

| ID | Obligation |
| :---- | :---- |
| **{_STAGED} — declare** | An adapter MUST declare its widgets. |
"""


def _run(root: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(root / _GEN), "--root", str(root), *args],
        capture_output=True,
        text=True,
        check=False,
    )


@pytest.fixture
def tree(tmp_path: Path) -> Path:
    for rel in (_GEN, _CHECKER):
        (tmp_path / rel).parent.mkdir(parents=True, exist_ok=True)
        shutil.copy(_REPO_ROOT / rel, tmp_path / rel)
    (tmp_path / "rfcs/dist").mkdir(parents=True)
    (tmp_path / "rfcs/dist/RFC-0001-core-spec.md").write_text(_DIST, encoding="utf-8")
    (tmp_path / "rfcs/amendments").mkdir(parents=True)
    (tmp_path / "rfcs/amendments/RFC-0001-widgets.md").write_text(_AMENDMENT, encoding="utf-8")
    (tmp_path / "docs").mkdir()
    guide = f"Widgets follow {_R1} and {_STAGED}.\n"
    (tmp_path / "docs/guide.md").write_text(guide, encoding="utf-8")
    out = _run(tmp_path)
    assert out.returncode == 0, out.stdout + out.stderr
    return tmp_path


def test_a_generated_index_passes_its_own_check(tree: Path) -> None:
    out = _run(tree, "--check")
    assert out.returncode == 0, out.stdout + out.stderr
    index = (tree / "docs/rule-index.md").read_text(encoding="utf-8")
    for rid in (_R1, _R2, _STAGED):
        assert f"**{rid}" in index, f"{rid} is defined at its source and missing from the index"
    assert "{{ref:" not in index, "a {{ref:}} token reached the index unrendered"


def test_a_public_citation_the_index_lacks_is_red(tree: Path) -> None:
    (tree / "docs/guide.md").write_text(f"Widgets follow {_UNDEFINED}.\n", encoding="utf-8")
    out = _run(tree, "--check")
    assert out.returncode == 1, out.stdout + out.stderr
    assert repr(_UNDEFINED) in out.stdout


def test_a_hand_edited_index_is_red(tree: Path) -> None:
    index = tree / "docs/rule-index.md"
    index.write_text(index.read_text(encoding="utf-8").replace("MUST be valid", "SHOULD be valid"))
    out = _run(tree, "--check")
    assert out.returncode == 1, out.stdout + out.stderr
    assert "is stale" in out.stdout


def test_a_changed_source_is_red_until_regenerated(tree: Path) -> None:
    src = tree / "rfcs/amendments/RFC-0001-widgets.md"
    src.write_text(src.read_text(encoding="utf-8").replace("MUST declare", "MUST publish"))
    assert _run(tree, "--check").returncode == 1
    assert _run(tree).returncode == 0
    assert _run(tree, "--check").returncode == 0


def test_without_amendments_staged_rows_are_carried_and_the_rest_checked(
    tree: Path,
) -> None:
    """The published shape: the assembly is present, the staged amendments are not."""
    shutil.rmtree(tree / "rfcs/amendments")
    out = _run(tree, "--check")
    assert out.returncode == 0, out.stdout + out.stderr
    assert "carried over" in out.stdout, "the carried rows must be said, not assumed"
    dist = tree / "rfcs/dist/RFC-0001-core-spec.md"
    dist.write_text(dist.read_text(encoding="utf-8").replace("MUST NOT widen", "MUST NOT narrow"))
    assert _run(tree, "--check").returncode == 1, "an assembled row must still be re-derived"


def test_nothing_to_generate_from_is_not_a_pass(tree: Path) -> None:
    (tree / "rfcs/dist/RFC-0001-core-spec.md").unlink()
    assert _run(tree, "--check").returncode == 2


def _decide(tree: Path, adr_status: str | None) -> None:
    """Mark the staged amendment as decided by an ADR, with that ADR's status line."""
    src = tree / "rfcs/amendments/RFC-0001-widgets.md"
    text = src.read_text(encoding="utf-8")
    decided = text.replace("status: proposed\n", "status: proposed\ndecided-by: ADR-9901\n")
    src.write_text(decided, encoding="utf-8")
    if adr_status is not None:
        (tree / "docs/adr").mkdir(parents=True, exist_ok=True)
        (tree / "docs/adr/9901-widgets.md").write_text(
            f"# ADR-9901: Widgets\n\n- **Status:** {adr_status}\n", encoding="utf-8"
        )


def test_a_decided_amendment_shows_its_decisions_status(tree: Path) -> None:
    """An amendment whose ADR is accepted is not "proposed" to a reader of the index."""
    _decide(tree, "accepted")
    out = _run(tree)
    assert out.returncode == 0, out.stdout + out.stderr
    text = (tree / "docs/rule-index.md").read_text(encoding="utf-8")
    assert "Status: accepted (ADR-9901)." in text
    assert "Status: proposed." not in text


def test_a_decision_that_cannot_be_found_is_not_a_pass(tree: Path) -> None:
    _decide(tree, None)
    assert _run(tree).returncode == 2
