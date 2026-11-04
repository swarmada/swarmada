# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""`assemble.py --all` writes only the RFCs that publish, and its staleness check agrees.

Why this file exists. Each RFC manifest declares `published: true|false`. The assembler carried
the flag into the document's front matter, but `--all` still wrote every manifest's document, and
`--check-stale` reported an unpublished RFC whose document was absent as MISSING. So an RFC held
back from the release kept an assembled copy in `rfcs/dist/`, and deleting that copy turned the
staleness gate red until the next `--all` wrote it back.

These tests build a small `rfcs/` tree with one published and one unpublished RFC and check both
halves: `--all` does not write, rewrite or ask for the unpublished document, and it still writes,
checks and fails on the published one. Naming an unpublished manifest explicitly still assembles
it, so a per-RFC target keeps working, and a manifest with no declaration is refused, not skipped.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

import pytest

_REPO_ROOT = Path(__file__).resolve().parents[2]
_ASSEMBLER = _REPO_ROOT / "rfcs" / "assemble.py"

# The assembler is not published (the public tree carries the assembled documents only), so
# these tests run in the authoring tree and skip where it is absent.
pytestmark = pytest.mark.skipif(not _ASSEMBLER.exists(), reason="the assembler is not in this tree")

_PUBLISHED_OUT = "RFC-0901-widgets.md"
_HELD_OUT = "RFC-0902-gadgets.md"

_MANIFEST = """rfc:
  title: "{title}"
  author: "Swarmada maintainers <maintainers@swarmada.io>"
  status: Review
{published}  created: "2026-01-01"
  updated: "2026-01-02"
  swarmada-version: "v0.4"
  output: ../dist/{out}

sections:
  - {{ id: {slug}-abstract, title: "Abstract", file: front/abstract.md }}
"""
_CHAPTER = """---
status: review
---

{title} says one thing.
"""


def _rfc(rfcs: Path, num: str, slug: str, out: str, published: str | None) -> Path:
    d = rfcs / f"rfc-{num}-{slug}"
    (d / "front").mkdir(parents=True)
    flag = "" if published is None else f"  published: {published}\n"
    title = f"RFC-{num}: {slug.title()}"
    meta = d / f"rfc-{num}.meta.yaml"
    meta.write_text(
        _MANIFEST.format(title=title, published=flag, out=out, slug=slug), encoding="utf-8"
    )
    (d / "front" / "abstract.md").write_text(_CHAPTER.format(title=title), encoding="utf-8")
    return meta


@pytest.fixture
def rfcs(tmp_path: Path) -> Path:
    root = tmp_path / "rfcs"
    root.mkdir()
    shutil.copy2(_ASSEMBLER, root / "assemble.py")
    _rfc(root, "0901", "widgets", _PUBLISHED_OUT, "true")
    _rfc(root, "0902", "gadgets", _HELD_OUT, "false")
    return root


@pytest.fixture(params=["pyyaml", "builtin"])
def parser_env(request) -> dict:
    """Both manifest parsers: PyYAML yields a bool for the flag, the built-in one a string."""
    env = dict(os.environ)
    env.pop("SWARMADA_ASSEMBLE_NOPYYAML", None)
    if request.param == "builtin":
        env["SWARMADA_ASSEMBLE_NOPYYAML"] = "1"
    return env


def _run(rfcs: Path, env: dict, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(rfcs / "assemble.py"), *args],
        capture_output=True,
        text=True,
        check=False,
        env=env,
        cwd=str(rfcs),
    )


def _says(text: str, needle: str) -> bool:
    return needle in text


def test_all_writes_the_published_rfc_and_not_the_held_one(rfcs: Path, parser_env: dict):
    r = _run(rfcs, parser_env, "--all")
    assert r.returncode == 0, r.stdout + r.stderr
    assert (rfcs / "dist" / _PUBLISHED_OUT).is_file(), "the published RFC must still be assembled"
    assert not (rfcs / "dist" / _HELD_OUT).exists(), "--all wrote an RFC declared published: false"


def test_all_leaves_an_existing_held_document_unwritten(rfcs: Path, parser_env: dict):
    held = rfcs / "dist" / _HELD_OUT
    held.parent.mkdir()
    held.write_text("an earlier assembly\n", encoding="utf-8")
    old = time.time() - 3600
    os.utime(held, (old, old))
    r = _run(rfcs, parser_env, "--all")
    assert r.returncode == 0, r.stdout + r.stderr
    assert held.read_text(encoding="utf-8") == "an earlier assembly\n"
    assert held.stat().st_mtime == pytest.approx(old, abs=1)


def test_check_stale_does_not_report_a_held_rfc_missing(rfcs: Path, parser_env: dict):
    built = _run(rfcs, parser_env, "--all")
    assert built.returncode == 0, built.stdout + built.stderr
    r = _run(rfcs, parser_env, "--all", "--check-stale")
    out = r.stdout + r.stderr
    assert r.returncode == 0, out
    assert not _says(out, "MISSING"), out
    # The skip is said, not silent: a reader of the gate's output sees why the RFC is absent.
    held = [line for line in r.stdout.splitlines() if _HELD_OUT in line]
    assert len(held) == 1 and _says(held[0], "published: false"), out


def test_check_stale_still_reports_a_published_rfc_missing(rfcs: Path, parser_env: dict):
    r = _run(rfcs, parser_env, "--all", "--check-stale")
    out = r.stdout + r.stderr
    assert r.returncode == 1, out
    assert _says(out, "MISSING"), out
    assert _says(out, _PUBLISHED_OUT), out


def test_check_stale_still_reports_a_published_rfc_stale(rfcs: Path, parser_env: dict):
    built = _run(rfcs, parser_env, "--all")
    assert built.returncode == 0, built.stdout + built.stderr
    chapter = rfcs / "rfc-0901-widgets" / "front" / "abstract.md"
    later = time.time() + 60
    os.utime(chapter, (later, later))
    r = _run(rfcs, parser_env, "--all", "--check-stale")
    assert r.returncode == 1, r.stdout + r.stderr
    assert _says(r.stdout, "STALE"), r.stdout


def test_naming_a_held_manifest_still_assembles_it(rfcs: Path, parser_env: dict):
    meta = rfcs / "rfc-0902-gadgets" / "rfc-0902.meta.yaml"
    r = _run(rfcs, parser_env, str(meta))
    assert r.returncode == 0, r.stdout + r.stderr
    held = rfcs / "dist" / _HELD_OUT
    assert held.is_file(), "a per-RFC target must still assemble an unpublished draft"
    assert _says(held.read_text(encoding="utf-8"), "published: false\n")


@pytest.mark.parametrize("args", [("--all",), ("--all", "--check-stale")])
def test_an_undeclared_manifest_is_refused_not_skipped(rfcs: Path, parser_env: dict, args):
    _rfc(rfcs, "0903", "gizmos", "RFC-0903-gizmos.md", None)
    r = _run(rfcs, parser_env, *args)
    out = r.stdout + r.stderr
    assert r.returncode != 0, out
    assert _says(out, "rfc.published must be true or false"), out
