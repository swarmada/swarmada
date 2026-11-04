# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The RFC-0001 overview's conformance sentence follows from `adapters/CONFORMANCE.md`.

Why this file exists. `rfcs/RFC-0001-overview.md` states four numbers about the
executable catalog in one sentence: the family range (`C0`-`Cn`), how many checks
the catalog enumerates, how many the harness runs, and how many count toward
conformance. The catalog is the only source for all four, and the sentence drifted
each time a check was added: it said 69 checks while the catalog enumerated 70,
`C0`-`C18` while the catalog ran to `C19`, and 59 counting toward conformance when
its own stated rule gave 65.

The specification's own count check compares the first number, but `make test` does
not run it, and it does not compare the other three. These tests derive every number from the
catalog, by the rule the sentence itself states, so a new check fails `make test`
until the overview is updated.

THE RULES, AS THE SENTENCE STATES THEM
    checks        every `- **Cn.m (...)**` entry in the catalog;
    harness runs  checks minus those marked as an obligation on the edge node
                  (`EDGE NODE — not an adapter obligation`), which no adapter result
                  can pass or fail, and minus those marked `NOT YET RUN`, which are
                  defined and not yet run by the harness (`C3.10`, `C3.11`);
    count toward  harness runs minus the `(context)` rows, which record harness
                  context rather than grade the adapter, and minus the rows the catalog
                  says are graded on the ControlPlane surface (`C6.3`, as `CP8.1`), which
                  the harness records but no adapter verdict counts.

The last rule is checked against the registry too: the rows that count are exactly the
`C` rows the FleetAdapter surface's profiles in `conformance/profiles.yaml` list.
"""

from __future__ import annotations

import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
OVERVIEW = ROOT / "rfcs" / "RFC-0001-overview.md"
CATALOG = ROOT / "adapters" / "CONFORMANCE.md"
REGISTRY = ROOT / "conformance" / "profiles.yaml"

_ENTRY = re.compile(r"^\s*-\s*\*\*(C(\d+)\.\d+)\s*\(([^)]*)\)", re.M)
_ON_CONTROL_PLANE = re.compile(r"graded on the ControlPlane surface as `(CP\d+\.\d+)`")


def _catalog() -> dict[str, str]:
    """Map each check id to its strength tag, e.g. {'C0.2': 'context'}."""
    return {m.group(1): m.group(3) for m in _ENTRY.finditer(CATALOG.read_text())}


def _graded_on_control_plane() -> dict[str, str]:
    """Map each check whose catalog entry says it is graded on the ControlPlane surface to the
    ControlPlane row that grades it, e.g. {'C6.3': 'CP8.1'}. An entry runs to the next one."""
    text = CATALOG.read_text()
    starts = list(_ENTRY.finditer(text))
    out = {}
    for m, nxt in zip(starts, starts[1:] + [None]):
        body = " ".join(text[m.start():nxt.start() if nxt else len(text)].split())
        cp = _ON_CONTROL_PLANE.search(body)
        if cp:
            out[m.group(1)] = cp.group(1)
    return out


def _sentence() -> str:
    text = " ".join(OVERVIEW.read_text().split())
    m = re.search(r"executable conformance catalog .*? count toward conformance \([^)]*\)", text)
    assert m, "the overview no longer carries the conformance-catalog sentence this test reads"
    return m.group(0)


def _counted() -> set[str]:
    cat = _catalog()
    return {cid for cid, tag in cat.items()
            if "EDGE NODE" not in tag and "NOT YET RUN" not in tag and tag != "context"
            and cid not in _graded_on_control_plane()}


def test_the_catalog_parses():
    """A parser that finds nothing would make every comparison below vacuous."""
    cat = _catalog()
    assert len(cat) > 60, f"only {len(cat)} catalog entries parsed from {CATALOG}"
    assert any(tag == "context" for tag in cat.values())
    assert any("EDGE NODE" in tag for tag in cat.values())
    assert _graded_on_control_plane(), (
        "no catalog entry says it is graded on the ControlPlane surface"
    )


def test_the_family_range_is_the_catalogs():
    families = sorted({int(cid[1:].split(".")[0]) for cid in _catalog()})
    want = f"`C{families[0]}`–`C{families[-1]}`"
    assert want in _sentence(), f"the overview should name the catalog range {want}"


def test_the_three_counts_follow_from_the_catalog():
    cat = _catalog()
    checks = len(cat)
    runs = checks - sum(1 for tag in cat.values() if "EDGE NODE" in tag or "NOT YET RUN" in tag)
    count = len(_counted())
    s = _sentence()
    m = re.search(r"(\d+) checks, of which the harness runs (\d+) .*? and (\d+) count toward", s)
    assert m, f"the sentence no longer states the three counts in the expected form: {s}"
    assert (int(m.group(1)), int(m.group(2)), int(m.group(3))) == (checks, runs, count), (
        f"the overview says {m.group(1)} checks / {m.group(2)} run / {m.group(3)} count; "
        f"{CATALOG.name} gives {checks} / {runs} / {count}")


def test_the_sentence_names_each_row_graded_on_the_control_plane():
    s = _sentence()
    for cid, cp in _graded_on_control_plane().items():
        assert f"`{cid}` is graded on the control plane as `{cp}`" in s, (
            f"the overview counts {cid} out of conformance without saying it is graded as {cp}")


def test_the_counted_rows_are_the_fleet_adapter_profiles():
    """The rows the sentence counts are the rows an adapter's verdict reads."""
    reg = yaml.safe_load(REGISTRY.read_text())
    surface = next(s for s in reg["surfaces"] if s["name"] == "FleetAdapter")
    listed = {cid for p in surface["profiles"] for level in ("core", "extended")
              for cid in p.get(level) or [] if re.fullmatch(r"C\d+\.\d+", cid)}
    assert listed == _counted(), (
        f"counted but in no FleetAdapter profile: {sorted(_counted() - listed)}; "
        f"in a profile but not counted: {sorted(listed - _counted())}")
