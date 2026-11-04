# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The ungraded-obligations gate can go red, in each of the ways it exists to catch.

`scripts/gen-ungraded-obligations.py --check` runs in `make test` against this tree. That run
proves the committed chapter is current and every keyword occurrence in scope is classified; it
cannot prove the check would notice if either were not. These tests build a small tree, generate
its chapter, and then break it one way at a time: a new MUST with no ledger entry, the chapter
edited by hand, a softened summary, a renamed test, a Not-met note removed, and a published tree
whose listed evidence is gone. They also pin that a Not-met rule never renders as a row.

The tree carries a stand-in for rfcs/assemble.py with the two names the generator uses, so the
tests run on a checkout that does not carry the RFC sources.
"""

from __future__ import annotations

import shutil
import subprocess
import sys
from pathlib import Path

import pytest

_REPO_ROOT = Path(__file__).resolve().parents[2]
_GEN = "scripts/gen-ungraded-obligations.py"
_SPEC = "rfcs/rfc-0001-core-spec"
_OUT = _SPEC + "/ungraded-obligations.md"

_ASSEMBLE = '''import re
_FM = re.compile(r"\\A---\\n.*?\\n---\\n", re.DOTALL)
_HEADING_RE = re.compile(r"^(#{2,8})\\s+(.+?)\\s*$")


def renumber_body(body, base_depth, chapter_number, chapter_id):
    out = []
    for line in body.splitlines(keepends=True):
        m = _HEADING_RE.match(line.rstrip("\\n"))
        if m and len(m.group(1)) > base_depth:
            slug = chapter_id + "-" + re.sub(r"[^a-z0-9]+", "-", m.group(2).lower()).strip("-")
            out.append('<a id="%s"></a>\\n' % slug)
        out.append(line)
    return "".join(out)
'''

_MANIFEST = """rfc:
  title: "Widgets"
sections:
  - id: detailed
    title: "Detailed"
    file: detailed.md
    children:
      - id: crds
        title: "Resources"
        file: crds/_index.md
        children:
          - { id: widget, title: "Widget", file: crds/widget.md }
      - { id: fleet-adapter-protocol, title: "Protocol", file: fleet-adapter-protocol.md }
      - { id: sensor-streams, title: "Streams", file: sensor-streams.md }
      - { id: control-plane, title: "Control Plane", file: control-plane.md }
      - { id: tde, title: "TDE", file: tde.md }
      - { id: security, title: "Security", file: security.md }
      - { id: safety, title: "Safety", file: safety.md }
      - { id: conformance-profiles, title: "Profiles", file: conformance-profiles.md }
"""

_CONTROL_PLANE = """---
chapter: control-plane
---

#### Dispatch

The control plane MUST NOT dispatch a widget twice.

#### Holds

The control plane MUST hold a stopped widget. **Not met at v0.4:** nothing holds it.
"""

_LEDGER = """updated: 2026-10-17
entries:
  - id: CPO-001
    chapter: control-plane.md
    quote: "The control plane MUST NOT dispatch a widget twice."
    keyword: MUST NOT
    disposition: ungraded
    statement: "The control plane MUST NOT dispatch a widget twice."
    evidence:
      - {file: internal/widget/widget_test.go, func: TestWidget_NeverTwice}
  - id: CPO-002
    chapter: control-plane.md
    quote: "The control plane MUST hold a stopped widget."
    keyword: MUST
    disposition: not-met
    note: "**Not met at v0.4:** nothing holds it."
"""

_GO_TEST = """package widget

import "testing"

func TestWidget_NeverTwice(t *testing.T) {}
"""


def _run(root: Path, *args: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(root / _GEN), "--root", str(root), *args],
        capture_output=True,
        text=True,
        check=False,
    )


def _write(root: Path, rel: str, text: str) -> None:
    p = root / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text, encoding="utf-8")


@pytest.fixture
def tree(tmp_path: Path) -> Path:
    root = tmp_path / "repo"
    (root / "scripts").mkdir(parents=True)
    shutil.copy(_REPO_ROOT / _GEN, root / _GEN)
    _write(root, "rfcs/assemble.py", _ASSEMBLE)
    _write(root, _SPEC + "/rfc-0001.meta.yaml", _MANIFEST)
    for rel in ("detailed.md", "crds/_index.md", "crds/widget.md", "fleet-adapter-protocol.md",
                "sensor-streams.md", "tde.md", "security.md", "safety.md",
                "conformance-profiles.md"):
        _write(root, _SPEC + "/" + rel, "---\nchapter: x\n---\n\nNo keyword here.\n")
    _write(root, _SPEC + "/control-plane.md", _CONTROL_PLANE)
    _write(root, _SPEC + "/obligations.yaml", _LEDGER)
    _write(root, "internal/widget/widget_test.go", _GO_TEST)
    _write(root, "conformance/CONTROL-PLANE.md", "- **CP9.9 (MUST)** A widget check.\n")
    r = _run(root)
    assert r.returncode == 0, r.stdout + r.stderr
    return root


def _edit(root: Path, rel: str, old: str, new: str) -> None:
    p = root / rel
    text = p.read_text(encoding="utf-8")
    assert text.count(old) == 1
    p.write_text(text.replace(old, new), encoding="utf-8")


def test_a_generated_tree_checks_clean(tree: Path) -> None:
    r = _run(tree, "--check")
    assert r.returncode == 0, r.stdout + r.stderr
    out = (tree / _OUT).read_text(encoding="utf-8")
    assert "| CPO-001 | {{ref:control-plane-dispatch}} |" in out
    assert "`TestWidget_NeverTwice` (`internal/widget/widget_test.go`)" in out


def test_a_not_met_rule_never_renders(tree: Path) -> None:
    out = (tree / _OUT).read_text(encoding="utf-8")
    assert "CPO-002" not in out
    assert "stopped widget" not in out


def test_a_new_must_with_no_entry_is_unclassified(tree: Path) -> None:
    _edit(tree, _SPEC + "/tde.md", "No keyword here.", "The TDE MUST grant a slot.")
    r = _run(tree, "--check")
    assert r.returncode == 1
    assert "UNCLASSIFIED tde.md" in r.stdout


def test_a_hand_edited_chapter_is_stale(tree: Path) -> None:
    _edit(tree, _OUT, "dispatch a widget twice.", "dispatch a widget twice, or thrice.")
    r = _run(tree, "--check")
    assert r.returncode == 1
    assert "STALE" in r.stdout


def test_a_softened_statement_is_refused(tree: Path) -> None:
    _edit(tree, _SPEC + "/obligations.yaml", 'statement: "The control plane MUST NOT dispatch',
          'statement: "The control plane SHOULD NOT dispatch')
    r = _run(tree, "--check")
    assert r.returncode == 2
    assert "CPO-001: the statement does not hold MUST NOT" in r.stderr


def test_a_renamed_test_is_refused(tree: Path) -> None:
    _edit(tree, "internal/widget/widget_test.go", "TestWidget_NeverTwice", "TestWidget_Renamed")
    r = _run(tree, "--check")
    assert r.returncode == 2
    assert "evidence TestWidget_NeverTwice not found" in r.stderr


def test_a_removed_not_met_note_is_refused(tree: Path) -> None:
    _edit(tree, _SPEC + "/control-plane.md", " **Not met at v0.4:** nothing holds it.", "")
    r = _run(tree, "--check")
    assert r.returncode == 2
    assert "CPO-002: note not found" in r.stderr


def test_a_check_no_catalog_defines_is_refused(tree: Path) -> None:
    _edit(tree, _SPEC + "/obligations.yaml", "    disposition: not-met\n    note:",
          "    disposition: graded\n    check: CP4.4\n    note:")
    r = _run(tree, "--check")
    assert r.returncode == 2
    assert "check 'CP4.4' is defined by no catalog" in r.stderr


def _published(tree: Path) -> Path:
    out = (tree / _OUT).read_text(encoding="utf-8").split("---\n", 2)[2]
    dist = ('<a id="control-plane"></a>\n### Control Plane\n\nText.\n\n'
            '<a id="ungraded-control-plane-obligations"></a>\n'
            "### Control-Plane Obligations Not Graded at v0.4\n" + out
            + '\n<a id="terminology"></a>\n## Terminology\n')
    shutil.rmtree(tree / _SPEC)
    (tree / "rfcs/assemble.py").unlink()
    _write(tree, "rfcs/dist/RFC-0001-core-spec.md", dist)
    return tree


def test_a_published_tree_checks_the_listed_evidence(tree: Path) -> None:
    r = _run(_published(tree), "--check")
    assert r.returncode == 0, r.stdout + r.stderr
    assert "checked the evidence of the 1 rows" in r.stdout


def test_a_published_tree_with_missing_evidence_fails(tree: Path) -> None:
    _published(tree)
    _edit(tree, "internal/widget/widget_test.go", "TestWidget_NeverTwice", "TestWidget_Renamed")
    r = _run(tree, "--check")
    assert r.returncode == 1
    assert "EVIDENCE CPO-001" in r.stdout


def test_a_tree_with_nothing_to_check_could_not_run(tree: Path) -> None:
    shutil.rmtree(tree / "rfcs")
    r = _run(tree, "--check")
    assert r.returncode == 2
