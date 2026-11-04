# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""`conformance/` imports on Python 3.10 — the interpreter the harness it profiles runs on.

Why this file exists. The repository declares `requires-python = ">=3.12"`, and that floor is
right for the repository: numpy needs it. It is not right for `conformance/`. The robot's
vendor container is Python 3.10.12; it is where the adapter harness has to run, because DDS
discovery does not cross a virtual-machine boundary to a host on the LAN; and the stage that
grades a run against a `plant.yaml` is the one stage with a reason to run where the plant is.

`conformance/model.py` used `enum.StrEnum`, which arrived in 3.11. Two things followed, and
neither was visible on a developer's machine:

    python3 -m conformance fleet-adapter …   AttributeError: module 'enum' has no attribute
                                             'StrEnum'
    pytest adapters/conformance/tests/       1 failed — one harness test imports this model,
                                             so the harness's OWN suite was red on the host it
                                             ships to, and "the suite passes where it will run"
                                             could not be said.

This guard is static on purpose. It needs no second interpreter, so it runs in CI and on any
developer machine, and it fails at `make test-py` rather than on a robot at the end of a
hardware day. It reads the source for names that do not exist on the floor; what it cannot
see is a name reached for dynamically, and the answer to that is to run the suite under 3.10,
not to weaken this file.

RAISING THE FLOOR IS ALLOWED, and it is a decision, not an edit: change `FLOOR` here, say in
`conformance/README.md` what the robot is then expected to run, and expect the next hardware
session to read both.
"""

from __future__ import annotations

import ast
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
PACKAGES = (ROOT / "conformance", ROOT / "adapters" / "conformance")

#: The interpreter the graded half of the pipeline has to import on.
FLOOR = (3, 10)

#: Names the standard library gained AFTER the floor, spelled as a caller writes them.
#: Each entry is the attribute access this guard refuses and the version that introduced it.
#: The list is what has actually been reached for here or is a plausible next reach; it is
#: not a complete inventory of the standard library, and adding to it is cheap.
TOO_NEW = {
    ("enum", "StrEnum"): "3.11",
    ("enum", "ReprEnum"): "3.11",
    ("datetime", "UTC"): "3.11",
    ("typing", "Self"): "3.11",
    ("typing", "Never"): "3.11",
    ("typing", "assert_never"): "3.11",
    ("typing", "LiteralString"): "3.11",
    ("typing", "TypeAliasType"): "3.12",
    ("typing", "override"): "3.12",
    ("asyncio", "TaskGroup"): "3.11",
    ("asyncio", "timeout"): "3.11",
    ("contextlib", "chdir"): "3.11",
    ("itertools", "batched"): "3.12",
    ("pathlib", "UnsupportedOperation"): "3.13",
}

#: Modules that do not exist on the floor at all.
TOO_NEW_MODULES = {"tomllib": "3.11", "asyncio.taskgroups": "3.11"}

#: Builtins and syntax the floor does not have. `ExceptionGroup` and `except*` are 3.11.
TOO_NEW_BUILTINS = {"ExceptionGroup": "3.11", "BaseExceptionGroup": "3.11"}


def _sources() -> list[Path]:
    files = sorted(p for pkg in PACKAGES for p in pkg.rglob("*.py")
                   if "__pycache__" not in p.parts)
    assert files, "no Python sources found under conformance/; this guard cannot run"
    return files


def _rel(path: Path) -> str:
    return str(path.relative_to(ROOT))


def too_new(source: str, filename: str = "<source>") -> list[str]:
    """Every name in `source` that the floor does not have, as `line N: name (Python X.Y)`.

    Import ALIASES are resolved first. `conformance/__main__.py` writes `import datetime as
    _dt` and then `_dt.UTC`, and a reader that matched only the canonical spelling would have
    walked straight past the second defect this file was written for.
    """
    tree = ast.parse(source, filename=filename)
    alias_of: dict[str, str] = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                alias_of[alias.asname or alias.name.split(".")[0]] = alias.name.split(".")[0]
    bad: list[str] = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Attribute) and isinstance(node.value, ast.Name):
            module = alias_of.get(node.value.id, node.value.id)
            if (module, node.attr) in TOO_NEW:
                since = TOO_NEW[(module, node.attr)]
                bad.append(f"line {node.lineno}: {node.value.id}.{node.attr} "
                           f"({module}.{node.attr}, Python {since})")
        if isinstance(node, ast.Name) and node.id in TOO_NEW_BUILTINS:
            bad.append(f"line {node.lineno}: {node.id} (Python {TOO_NEW_BUILTINS[node.id]})")
        elif isinstance(node, ast.Import):
            for alias in node.names:
                if alias.name.split(".")[0] in TOO_NEW_MODULES:
                    bad.append(f"line {node.lineno}: import {alias.name} "
                               f"(Python {TOO_NEW_MODULES[alias.name.split('.')[0]]})")
        elif isinstance(node, ast.ImportFrom) and node.module:
            root = node.module.split(".")[0]
            if root in TOO_NEW_MODULES:
                bad.append(f"line {node.lineno}: from {node.module} import … "
                           f"(Python {TOO_NEW_MODULES[root]})")
            for alias in node.names:
                if (node.module, alias.name) in TOO_NEW:
                    bad.append(f"line {node.lineno}: from {node.module} import {alias.name} "
                               f"(Python {TOO_NEW[(node.module, alias.name)]})")
    return bad


@pytest.mark.parametrize("path", _sources(), ids=_rel)
def test_no_module_reaches_for_a_name_the_floor_does_not_have(path: Path) -> None:
    bad = too_new(path.read_text(encoding="utf-8"), str(path))
    assert not bad, (
        f"{_rel(path)} uses names the Python {FLOOR[0]}.{FLOOR[1]} the adapter harness runs "
        f"on does not have:\n  " + "\n  ".join(bad) + "\n"
        "Spell it so it runs on the floor, or raise FLOOR in this file and say in "
        "conformance/README.md what the robot is then expected to run")


@pytest.mark.parametrize("path", _sources(), ids=_rel)
def test_every_module_compiles_against_the_floors_syntax(path: Path) -> None:
    """Syntax, not names: a match statement is fine on 3.10, PEP 695 generics are not."""
    source = path.read_text(encoding="utf-8")
    try:
        compile(source, str(path), "exec", dont_inherit=True,
                flags=ast.PyCF_ONLY_AST, optimize=0, _feature_version=FLOOR[1])
    except SyntaxError as exc:  # pragma: no cover -- the assertion below is the report
        pytest.fail(f"{_rel(path)} is not valid Python {FLOOR[0]}.{FLOOR[1]} syntax: "
                    f"line {exc.lineno}: {exc.msg}")


def test_the_report_models_string_enums_print_their_value() -> None:
    """The shim that replaced `enum.StrEnum` has to keep `enum.StrEnum`'s semantics.

    `(str, Enum)` without the two dunders prints `CheckStatus.PASS` where `enum.StrEnum`
    prints `pass`, and this module's report text and its f-strings were written against the
    latter. A silently different `str()` is exactly the kind of drift the 3.10 change could
    have introduced, so it is pinned rather than assumed.
    """
    import conformance.model as m

    for member, value in ((m.CheckStatus.PASS, "pass"),
                          (m.Grade.NOT_SUPPORTED_BY_PLATFORM, "not-supported-by-platform"),
                          (m.Verdict.NOT_ESTABLISHED, "not-established"),
                          (m.Fidelity.HARDWARE, "hardware"),
                          (m.SkipCause.NO_INDEPENDENT_MOTION_CHANNEL,
                           "no-independent-motion-channel")):
        assert str(member) == value
        assert f"{member}" == value
        assert member == value, "the member still compares equal to its own string"
        assert isinstance(member, str)


def test_the_guard_sees_each_shape_of_too_new_name() -> None:
    """The guard's own falsifiability, kept in the suite rather than taken on trust.

    Both defects this file was written for are in here -- the enum base class and the
    `datetime.UTC` alias -- beside the import shapes that would otherwise slip past an
    attribute-only reader. The last two lines are the control: valid 3.10 code, nothing
    reported, so a guard that flagged everything would fail here.
    """
    reported = too_new(
        "import enum\n"
        "import datetime as _dt\n"
        "import tomllib\n"
        "from typing import Self\n"
        "class S(enum.StrEnum):\n"
        "    A = 'a'\n"
        "now = _dt.datetime.now(_dt.UTC)\n"
        "raise ExceptionGroup('g', [])\n"
    )
    assert len(reported) == 5, reported
    assert any("enum.StrEnum" in r for r in reported)
    assert any("_dt.UTC" in r for r in reported), "an aliased import is resolved"
    assert any("import tomllib" in r for r in reported)
    assert any("import Self" in r for r in reported)
    assert any("ExceptionGroup" in r for r in reported)
    assert too_new("import enum\n\n\nclass S(str, enum.Enum):\n    A = 'a'\n") == []


@pytest.mark.skipif(sys.version_info < (3, 11),
                    reason="below 3.11 the import itself is the proof")
def test_the_shim_is_used_rather_than_the_standard_library_class() -> None:
    """One spelling on every interpreter, not a version test that picks a different class.

    A version test would mean the developer's interpreter and the robot's run different code,
    which is the shape of defect this whole file exists to catch.
    """
    import conformance.model as m

    assert m.StrEnum.__module__ == m.__name__, (
        "conformance.model.StrEnum is the standard library's on this interpreter, so the "
        "robot's 3.10 runs a class this suite never exercised")
