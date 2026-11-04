# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The parenthesis scars a citation cut leaves in comments and prose, and the code they resemble.

`check-error-strings.py` already refused a parenthetical emptied down to a lead-in word, a `§` or
whitespace. A cut that took a citation out of the MIDDLE of a parenthetical, or took the closing
paren with it, left shapes none of those arms matched, and they stood in comments, a .proto
comment and an ADR sentence while the gate stayed green.

Each shape below must be refused, and each code look-alike must not be: the check folds every
line break before matching, so the arms are confined to comment and prose lines. A test that only
planted scars would pass for a pattern that matched everything.
"""

from __future__ import annotations

import importlib.util
import pathlib

import pytest

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]

# Each planted text is (file name, content). The comment markers are joined at run time so this
# file's own source carries no comment line the gate could read as a scar.
_C = "/" + "/"

SCARS = {
    "paren-space-word-comma-close": (
        "a.go",
        f"package a\n\n{_C} The design ( rule 3, ) puts the responsibility on the executor.\n",
    ),
    "comma-space-close": (
        "b.go",
        f"package b\n\n{_C} It carries its identity (C1.3, ). This is checked.\n",
    ),
    "paren-space-word": (
        "c.proto",
        f'syntax = "proto3";\n\n{_C} Reuses the envelope\'s vocabulary ( so both\n'
        f"{_C} bindings report outcomes the same way.\nmessage M {{}}\n",
    ),
    "call-paren-space-word": (
        "d.go",
        f"package d\n\n{_C} is why this rule earns a test: aggregate( treats a nil\n"
        f"{_C} quorum as zero.\n",
    ),
    "call-paren-across-a-line-break": (
        "e.go",
        f"package e\n\n{_C} Entries are addressed by index. append(\n"
        f"{_C} reallocates the slice when it outgrows its capacity.\n",
    ),
    "comma-close-in-prose": (
        "f.md",
        "The tier is optional (`PROFILES.md`, *Support levels*,), so a fail reads it as "
        "mandatory.\n",
    ),
    "punctuation-opening-a-continuation-line": (
        "g.go",
        f"package g\n\n{_C} a failure is not evicted because more recent successes exist\n"
        f"{_C} . Owner-referenced children, so this is an ordinary Delete.\n",
    ),
}

# The same characters in code, in a code span, and on one line of a usage comment.
LOOKALIKES = {
    "go-multiline-call": (
        "h.go",
        "package h\n\nfunc f() {\n\tg(\n\t\tctx,\n\t\tname,\n\t)\n\tconds = append(\n"
        "\t\tconds, c)\n}\n",
    ),
    "go-deref-line-is-not-a-comment": (
        "i.go",
        "package i\n\nfunc f() {\n\t*conds = append(\n\t\tconds, c)\n}\n",
    ),
    "python-one-element-tuple": ("j.py", "PAIR = (value,)\nCALL = f(\n    first,\n)\n"),
    "code-span-in-a-comment": ("Makefile", "# `ifeq ($(GO),)` was false, so the guard ran.\n"),
    "fenced-code-in-prose": ("k.md", "Run it:\n\n```\nf(\n    arg,\n)\n```\n"),
    "command-in-a-usage-comment": ("l.ini", "# Run locally:   vale .\n#\n# NOTE: none.\n"),
    "ordinary-parenthetical": (
        "m.go",
        f"package m\n\n{_C} Nil falls back to Client (a unit test's fake client), and\n"
        f"{_C} f() returns (a, b).\n",
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


@pytest.mark.parametrize("shape", sorted(SCARS))
def test_a_cut_parenthetical_is_refused(checker, tmp_path, shape):
    name, body = SCARS[shape]
    deny = _deny(checker, tmp_path, name, body)
    assert deny, f"{shape}: the gate did not refuse\n{body}"


@pytest.mark.parametrize("shape", sorted(LOOKALIKES))
def test_the_code_it_resembles_is_not(checker, tmp_path, shape):
    name, body = LOOKALIKES[shape]
    deny = _deny(checker, tmp_path, name, body)
    assert deny == [], f"{shape}: an ordinary construct was refused: {deny}"
