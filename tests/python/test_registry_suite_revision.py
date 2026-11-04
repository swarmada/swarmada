# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""`adapters/REGISTRY.md` prints the suite revision it names as authoritative.

Why this file exists. REGISTRY.md states the current conformance suite revision,
the check-family range and the check count in its own words, and one sentence
later names `revision:` in `conformance/profiles.yaml` as the authoritative
value. Nothing compared the two, and they drifted three times: the registry
printed `2026.09.13`, C0–C17 and 57 checks while `profiles.yaml` read
`2026.09.15` and the harness recorded C0–C18 and 62. A stale revision in the
registry defeats the one distinction it records two versions to make -- a
*failing* row against an *unverified* one.

These tests pin the printed values to their sources, with no adapter, no
cluster and no network, so the drift fails at `make test` rather than in a
reader's hands.

THE ADAPTER TEMPLATE PRINTS THE SAME RANGE, and drifted the same way: it said
C0–C16 while the harness recorded C0–C18. It is the first file a new adapter
author copies, and its generated README and CI workflow are copied into every
vendor repository, so every `Cn–Cm` range in those three files is pinned to the
harness here too, from the same derivation as the registry's.

EACH ROW IS READ AGAINST THE REVISION CLASSES. `profiles.yaml` declares, for
every suite revision, a class: `harness` (what an implementation must do to pass
changed, so a result earned before it must be re-earned) or `profile-layer`
(only grouping or naming changed, so a result carries forward). Each registry
row names the revision its result was earned at, once, as "earned at suite
`<rev>`". A row is STALE when a `harness`-class revision later than that one is
at or below the current revision, and CURRENT otherwise; a stale row must say
**stale** and a current row must not. Before the classes existed the registry
could only state that no row had been re-run, and a reader could not tell a row
that needed re-earning from one that did not.

WHAT THEY DO NOT CHECK
    Whether a row's result is TRUE. A row names the revision its result was
    earned at; the guard checks that the row's stale marker follows from the
    declared classes, not that the run it describes happened.

    The count is read from harness.py statically -- the distinct quoted
    `"Cn.m"` ids, the same reading `adapters/conformance/tests/test_harness_docs.py`
    uses for the family range. It matched a `make conformance-sim` run (62 rows)
    when this file was written. An id the harness builds at runtime would be
    missed, and an id quoted without being recorded would be counted; either
    fails here rather than passing silently.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

REGISTRY = ROOT / "adapters" / "REGISTRY.md"
PROFILES = ROOT / "conformance" / "profiles.yaml"
HARNESS = ROOT / "adapters" / "conformance" / "harness.py"

# The template's printed ranges: its own README, and the README and CI workflow it generates.
_TEMPLATE = ROOT / "adapters" / "template"
_GENERATED = _TEMPLATE / "{{cookiecutter.adapter_slug}}"
TEMPLATE_RANGE_FILES = (
    _TEMPLATE / "README.md",
    _GENERATED / "README.md",
    _GENERATED / ".github" / "workflows" / "conformance.yml",
)

# "this page is written against **`2026.09.15`, C0–C18, 62 automated checks**"
_CURRENT = re.compile(
    r"written\s+against\s+\*\*`(?P<rev>[^`]+)`,\s+C(?P<lo>\d+)[–-]C(?P<hi>\d+),\s+"
    r"(?P<count>\d+)\s+automated checks\*\*"
)
# "62 is the number a `make conformance` run reports."
_RUN_COUNT = re.compile(r"(?P<count>\d+) is the number a `make conformance` run reports")
_REVISION = re.compile(r'(?m)^revision:\s*"(?P<rev>[^"]+)"')
# "earned at suite `2026.09.03`" -- the one place a Conformance cell names its revision.
_EARNED = re.compile(r"(?i)earned at suite `(?P<rev>\d{4}\.\d{2}\.\d{2}(?:\.\d+)?)`")
_STALE = "**stale**"
REVISION_CLASSES = ("harness", "profile-layer")
_RECORDED = re.compile(r'"(C\d+\.\d+)"')
# "(C0–C18)" / "(C0-C18)" -- a family range. `C7.1` and `C17.1`/`C17.2` are not ranges.
_RANGE = re.compile(r"\bC(?P<lo>\d+)\s*[–-]\s*C(?P<hi>\d+)\b")


def _read(path: Path) -> str:
    # Fail, never skip: a guard that cannot find its inputs has not passed.
    assert path.is_file(), f"{path.relative_to(ROOT)} is missing; this guard cannot run"
    return path.read_text(encoding="utf-8")


def _registry_current() -> re.Match:
    m = _CURRENT.search(" ".join(_read(REGISTRY).replace(">", " ").split()))
    assert m, (
        "adapters/REGISTRY.md no longer states the current suite revision as "
        "'this page is written against **`<rev>`, C<lo>–C<hi>, <n> automated checks**'. "
        "If the sentence was reworded, update this guard with it; do not delete the guard"
    )
    return m


def _recorded_ids() -> set[str]:
    ids = set(_RECORDED.findall(_read(HARNESS)))
    assert ids, "adapters/conformance/harness.py records no check ids; this guard cannot run"
    return ids


def test_registry_revision_matches_profiles_yaml() -> None:
    m = _REVISION.search(_read(PROFILES))
    assert m, "conformance/profiles.yaml declares no `revision:`; this guard cannot run"
    printed = _registry_current()["rev"]
    assert printed == m["rev"], (
        f"adapters/REGISTRY.md prints suite revision {printed}, and the file it names as "
        f"authoritative, conformance/profiles.yaml, reads {m['rev']}. Update the registry's "
        f"suite-revision block, and state whether the rows were re-run at {m['rev']}"
    )


def test_registry_family_range_matches_harness() -> None:
    families = {int(i.split(".")[0][1:]) for i in _recorded_ids()}
    cur = _registry_current()
    printed = (int(cur["lo"]), int(cur["hi"]))
    assert printed == (min(families), max(families)), (
        f"adapters/REGISTRY.md prints C{printed[0]}–C{printed[1]}; the harness records "
        f"C{min(families)}–C{max(families)}"
    )


def test_registry_check_count_matches_harness() -> None:
    recorded = len(_recorded_ids())
    printed = int(_registry_current()["count"])
    run = _RUN_COUNT.search(" ".join(_read(REGISTRY).replace(">", " ").split()))
    assert run, "adapters/REGISTRY.md no longer states the count a `make conformance` run reports"
    assert printed == int(run["count"]) == recorded, (
        f"adapters/REGISTRY.md prints {printed} automated checks and says a run reports "
        f"{run['count']}; the harness records {recorded} distinct check ids"
    )


@pytest.mark.parametrize(
    "path", TEMPLATE_RANGE_FILES, ids=[str(p.relative_to(_TEMPLATE)) for p in TEMPLATE_RANGE_FILES]
)
def test_template_family_range_matches_harness(path: Path) -> None:
    families = {int(i.split(".")[0][1:]) for i in _recorded_ids()}
    want = (min(families), max(families))
    ranges = [(int(m["lo"]), int(m["hi"])) for m in _RANGE.finditer(_read(path))]
    assert ranges, (
        f"{path.relative_to(ROOT)} no longer prints a C<lo>–C<hi> family range. If it was "
        f"reworded, update TEMPLATE_RANGE_FILES with it; do not delete the guard"
    )
    stale = [r for r in ranges if r != want]
    assert not stale, (
        f"{path.relative_to(ROOT)} prints "
        f"{', '.join(f'C{lo}–C{hi}' for lo, hi in stale)}; the harness records "
        f"C{want[0]}–C{want[1]}"
    )


# ── revision classes ──────────────────────────────────────────────────────────


def _revision_classes() -> dict[str, str]:
    """revision -> class, from `revisions:` in profiles.yaml."""
    import yaml

    doc = yaml.safe_load(_read(PROFILES))
    entries = doc.get("revisions")
    assert entries, (
        "conformance/profiles.yaml declares no `revisions:` list, so no registry row can be "
        "classified as stale or current; this guard cannot run"
    )
    out: dict[str, str] = {}
    for e in entries:
        rev, cls = str(e.get("revision")), e.get("class")
        assert rev not in out, f"profiles.yaml declares revision {rev} twice"
        assert cls in REVISION_CLASSES, (
            f"profiles.yaml revision {rev} has class {cls!r}; want one of {REVISION_CLASSES}"
        )
        assert e.get("reason"), f"profiles.yaml revision {rev} states no reason for its class"
        out[rev] = cls
    return out


def is_stale(earned_at: str, current: str, classes: dict[str, str]) -> bool:
    """A result is stale when a `harness` revision after it is at or below the current one.

    Revisions are dates written `YYYY.MM.DD`, with an optional `.N` for a second revision on
    the same day, and are compared numerically field by field.
    """
    e, c = _rev_key(earned_at), _rev_key(current)
    return any(e < _rev_key(rev) <= c and cls == "harness" for rev, cls in classes.items())


def _rev_key(rev: str) -> tuple[int, ...]:
    return tuple(int(x) for x in rev.split("."))


def test_the_stale_rule_reads_the_class() -> None:
    """The rule itself, on a synthetic history: a profile-layer revision carries a
    result forward; a harness revision does not, and neither does one that follows it."""
    classes = {"2026.01.01": "harness", "2026.02.01": "profile-layer", "2026.03.01": "harness"}
    assert not is_stale("2026.01.01", "2026.02.01", classes), "profile-layer carries forward"
    assert is_stale("2026.01.01", "2026.03.01", classes), "a later harness revision re-earns"
    assert is_stale("2026.02.01", "2026.03.01", classes)
    same_day = {"2026.03.01": "harness", "2026.03.01.2": "harness"}
    assert is_stale("2026.03.01", "2026.03.01.2", same_day), "a same-day revision orders after"
    assert not is_stale("2026.03.01", "2026.03.01", classes), "earned at current is current"


def test_every_revision_is_classified_and_the_current_one_is_declared() -> None:
    classes = _revision_classes()
    current = _REVISION.search(_read(PROFILES))["rev"]
    assert current in classes, (
        f"the current revision {current} has no entry in `revisions:`; a revision without a "
        f"class leaves every registry row unclassifiable"
    )
    latest = max(classes, key=_rev_key)
    assert latest == current, (
        f"`revisions:` declares {latest}, later than the current revision {current}"
    )


def _registry_rows() -> list[tuple[str, str]]:
    """(adapter, Conformance cell) for every row of the registry table."""
    rows = []
    for line in _read(REGISTRY).splitlines():
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) == 6 and cells[0].startswith("`") and not cells[0].startswith(":"):
            rows.append((cells[0].strip("`"), cells[5]))
    assert rows, "adapters/REGISTRY.md has no registry rows; this guard cannot run"
    return rows


def test_each_row_names_the_revision_it_was_earned_at_once() -> None:
    classes = _revision_classes()
    for adapter, cell in _registry_rows():
        found = _EARNED.findall(cell)
        assert len(found) == 1, (
            f"the {adapter} row names {len(found)} 'earned at suite `<rev>`'; it must name "
            f"exactly one, the revision its result was earned at"
        )
        assert found[0] in classes, (
            f"the {adapter} row was earned at {found[0]}, which profiles.yaml does not classify"
        )


def test_each_row_is_marked_stale_exactly_when_the_classes_say_so() -> None:
    classes = _revision_classes()
    current = _REVISION.search(_read(PROFILES))["rev"]
    for adapter, cell in _registry_rows():
        found = _EARNED.findall(cell)
        assert found, f"the {adapter} row names no 'earned at suite `<rev>`'"
        stale = is_stale(found[0], current, classes)
        marked = _STALE in cell
        assert marked == stale, (
            f"the {adapter} row was earned at {found[0]}; against current {current} and the "
            f"declared classes it is {'STALE' if stale else 'CURRENT'}, and the cell "
            f"{'does not say' if stale else 'says'} {_STALE}"
        )
