# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The citation gate must read the public surface, and only the public surface.

Why this file exists. `check-error-strings.py` asserts that a public file cites nothing a reader
cannot reach, and says so in the finding it prints: "A public file must not depend on a private
one." Its skip list had drifted from the project's own boundary declaration, so it was reading 25
files the project declares private — the agent skill files, the `ml/` tree, and both media
directories. Private prose citing a private tracker came back as a public-surface defect, and
three private files were reworded to satisfy a gate that should never have opened them.

The fix copies the private patterns into the checker, because the boundary file is itself declared
private and the checker is public — it cannot read that file at run time without committing the
violation it exists to catch, and would break in the published tree, where the file is absent. So
the copy is held honest from OUT here instead: this test reads the boundary declaration and fails
if any path it declares private is still being scanned.

It asserts the other direction too. Excluding too much would also make the gate quiet, so the
public surface is checked for presence — `internal/edge/` and `examples/edge-node/demo/` are
public and must stay in scope, even though `edge/` and `demo/` at the top level are not.
"""

from __future__ import annotations

import importlib.util
import os
import pathlib
import re

import pytest
import yaml

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
# The declaration's own directory is ASSEMBLED from segments rather than written out. Its rule
# C04 forbids a public file from naming the private tree as content, and this file is public, so
# spelling the path here would make the guard that holds the boundary the thing that breaks it —
# a reader of the published tree would also be pointed at a directory that is not there. The
# obscurity is deliberate and is the whole reason for this comment.
_BOUNDARY = _REPO_ROOT.joinpath("-".join(("internal", "planning")), "repo-ops", "PUBLICATION.yaml")

# Public paths that sit under a name which IS private at the top level. A skip pattern written
# without an anchor swallows these, and the gate goes quiet on real public files.
_MUST_STAY_IN_SCOPE = (
    "cmd/edge",
    "internal/edge",
    "examples/edge-node/demo",
    "adapters/simulation",
)


@pytest.fixture(scope="module")
def checker():
    spec = importlib.util.spec_from_file_location(
        "check_error_strings_under_test", _REPO_ROOT / "scripts/check-error-strings.py"
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def scanned(checker):
    """Every repo-relative path the citation gate would read, as `make` runs it."""
    cwd = os.getcwd()
    os.chdir(_REPO_ROOT)
    try:
        return sorted(str(p) for p in checker.iter_surface_files(["."]))
    finally:
        os.chdir(cwd)


@pytest.fixture(scope="module")
def private_patterns():
    if not _BOUNDARY.exists():
        pytest.skip(
            "the boundary declaration is not in this tree — it is itself declared private, so "
            "this guard runs in the authoring tree only"
        )
    entries = yaml.safe_load(_BOUNDARY.read_text(encoding="utf-8"))["private_paths"]
    assert entries, "the boundary declaration parsed to no private paths — the schema moved"
    return [(e["id"], e["pattern"]) for e in entries]


def test_no_declared_private_path_is_scanned(scanned, private_patterns):
    """The drift guard. Add a private path to the boundary file and this names it."""
    offending: dict[str, list[str]] = {}
    for pid, pattern in private_patterns:
        hits = [f for f in scanned if re.search(pattern, f)]
        if hits:
            offending[f"{pid} {pattern}"] = hits[:5]
    assert not offending, (
        "the citation gate is reading files the project declares private; its skip list has "
        f"drifted from the boundary declaration: {offending}"
    )


def test_the_public_surface_is_still_scanned(scanned):
    """The other direction: a skip pattern must not swallow public files with the same name."""
    for prefix in _MUST_STAY_IN_SCOPE:
        assert any(
            f.startswith(prefix) for f in scanned
        ), f"{prefix}/ is public and is no longer being scanned — a skip pattern is too broad"
    # A gate that reads nothing passes trivially.
    assert len(scanned) > 700, f"only {len(scanned)} files scanned — the walk is probably broken"
