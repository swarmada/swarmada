# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The capability sheet: one grade per row, evidence, unlock, and a verdict that
cannot be earned by a skip (ADR-0070, extending ADR-0048).

Why this file exists. A `ConformanceReport` used to carry one boolean over Core,
and that boolean read `true` while Core rows were skipped: the reference adapter's
report said `conformant: true` with its `handshake` Core `notVerified` and `C3.7`
skipped with a note saying it attested nothing about durability. A reader could not
tell what had been established, at what fidelity, or what had never been tried.

These tests hold the four properties the change rests on:

* the SkipCause -> grade mapping is TOTAL and SINGLE-VALUED, in the code and in the
  ADR that publishes it, so a new cause cannot arrive without a decision;
* a simulated run can never produce, serialise or print a hardware claim;
* a skipped Core row prevents `pass`, while a check that was never an obligation
  (ADR-0048 rule B) still does not;
* a row that is not measured and carries no `unlock` is an error, not a report.

Attributes are looked up inside each test rather than imported at module level, so
that before the change each test fails on its own rather than the file failing to
import.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

import conformance.model as m

ROOT = Path(__file__).resolve().parents[2]
#: The harness report the FleetAdapter surface is profiled from: the simulation adapter's
#: recorded harness run, committed with the repository (test/samples pins it).
HARNESS_REPORT = ROOT / "test" / "samples" / "testdata" / "recorded-sim-adapter-report.json"
#: Every report `python -m conformance all` writes. Named here rather than globbed from a
#: directory, so a report that is not written fails a test instead of dropping out of the
#: parametrization. conformance-reports/ is not read: it is the local, gitignored output of
#: `make conformance-profiles` and a fresh clone has none.
REPORTS = [
    "compute-endpoint-plate-reader-grpc",
    "compute-endpoint-plate-reader-http",
    "compute-endpoint-plate-reader-mcp",
    "compute-endpoint-plate-watcher-grpc",
    "control-plane",
    "fleet-adapter",
]
ADR = ROOT / "docs" / "adr" / "0070-a-conformance-report-grades-each-row-and-names-its-unlock.md"


def _row(check_id: str, status: str, detail: str = "", level: str = "MUST") -> m.CheckResult:
    return m.CheckResult(check_id, "t", m.CheckStatus(status), level, detail)


def _report(results: list[m.CheckResult], core: list[str], *, required: bool = True,
            fidelity: str = "simulated", unlocks: dict | None = None) -> m.ConformanceReport:
    """A one-profile report whose Core level holds ``core``."""
    lv = m._level([r for r in results if r.check_id in core])
    lv.check_ids = list(core)
    prof = m.ProfileResult("p", required=required, applicable=True, core=lv)
    return m.ConformanceReport(
        surface="FleetAdapter", implementation={"project": "x", "version": "0"},
        date="2026-01-01T00:00:00Z", protocol_version="fleet_adapter.v1",
        contract_version="1.2.0", suite_revision="r", mode="simulated",
        capability_tier="FullControl", standalone=True, profiles=[prof], results=results,
        fidelity=fidelity,
        unlocks=m.Unlocks(unlocks if unlocks is not None else
                          {"byCause": {}, "byCheck": {"FleetAdapter": {"C1.3": "u"}}}))


@pytest.fixture(scope="module")
def profiled(tmp_path_factory: pytest.TempPathFactory) -> Path:
    """Profile every surface now, as `make conformance-profiles` does, into a temp directory.

    The reports are generated from committed inputs (the recorded harness report, the
    in-tree Compute Endpoint fixtures, the shipped CRD and RBAC YAML), so what is checked is
    what this tree produces, not whatever a previous run left on disk."""
    out = tmp_path_factory.mktemp("profiled")
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join(
        ["proto", "."] + ([env["PYTHONPATH"]] if env.get("PYTHONPATH") else []))
    run = subprocess.run(
        [sys.executable, "-m", "conformance", "all", "--adapter-report", str(HARNESS_REPORT),
         "--out", str(out)],
        cwd=ROOT, env=env, capture_output=True, text=True, timeout=600, check=False)
    # 0 pass, 1 a surface failed, 3 a surface not established: each wrote its reports. 2 is a
    # refusal, and anything else is a crash; neither is a profile.
    assert run.returncode in (0, 1, 3), (
        f"`python -m conformance all` exited {run.returncode}:\n{run.stdout[-2000:]}\n"
        f"{run.stderr[-2000:]}")
    return out


def test_the_profiler_writes_every_report(profiled: Path) -> None:
    written = sorted(p.stem for p in profiled.glob("*.yaml"))
    assert written == sorted(REPORTS), (
        f"the profiler wrote {written}; the checks below cover {sorted(REPORTS)}. A surface "
        f"added to or dropped from `conformance all` belongs in REPORTS")


# ── 1. The mapping is total, single-valued, and the ADR publishes the same one ──

def test_every_skip_cause_maps_to_exactly_one_grade() -> None:
    assert set(m.CAUSE_GRADE) == set(m.SkipCause), (
        f"unmapped: {sorted(c.value for c in set(m.SkipCause) - set(m.CAUSE_GRADE))}")
    assert all(isinstance(g, m.Grade) for g in m.CAUSE_GRADE.values())
    # A skip is never a measurement, and never a failure: those two come from the
    # check's own pass/fail, not from a cause.
    assert not {m.Grade.MEASURED_HARDWARE, m.Grade.MEASURED_SIMULATION,
                m.Grade.FAILED} & set(m.CAUSE_GRADE.values())


def test_the_adr_table_is_the_code_table() -> None:
    """Each cause appears in the ADR's table exactly once, with the grade the code uses.

    A cause that fits two grades is a finding to record, not a second row."""
    rows = re.findall(r"^\|\s*`([a-z0-9-]+)`\s*\|\s*`([a-z-]+)`\s*\|", ADR.read_text(), re.M)
    causes = [c for c, _ in rows if m.SkipCause.of(c) is not None and c in
              {s.value for s in m.SkipCause}]
    dupes = sorted({c for c in causes if causes.count(c) > 1})
    assert not dupes, f"causes mapped more than once in the ADR: {dupes}"
    published = {m.SkipCause(c): m.Grade(g) for c, g in rows if c in {s.value for s in m.SkipCause}}
    assert published == m.CAUSE_GRADE


def test_every_harness_spelling_resolves_to_a_grade() -> None:
    """The FleetAdapter harness writes its own vocabulary; every spelling it can
    write must land on a grade, through the alias table where it differs."""
    harness = (ROOT / "adapters/conformance/report.py").read_text()
    block = re.split(r"^class ", harness.split("class SkipCause")[1], flags=re.M)[0]
    written = set(re.findall(r'^\s+[A-Z_]+ = "([a-z0-9-]+)"', block, re.M))
    for spelling in written:
        cause = m.SkipCause.of(spelling + ": x")
        assert cause is not None and cause in m.CAUSE_GRADE, spelling


# ── 2. A simulated run is never a hardware claim ─────────────────────────────

def test_a_simulated_run_cannot_emit_measured_hardware() -> None:
    rep = _report([_row("C1.1", "pass", "both streams opened")], ["C1.1"])
    assert rep.sheet["C1.1"].grade is m.Grade.MEASURED_SIMULATION
    for rendered in (rep.to_yaml(), rep.to_json(), rep.to_text()):
        assert "measured-hardware" not in rendered
        assert "fidelity: hardware" not in rendered and '"fidelity": "hardware"' not in rendered
    assert rep.to_dict()["subject"]["fidelity"] == "simulated"


def test_a_hardware_fidelity_needs_an_operator_declared_hardware_plant() -> None:
    with pytest.raises(m.FidelityRefused):
        m.resolve_fidelity("physical", None)
    sim = {"plant": {"kind": {"kind": "read", "value": "simulated", "source": "graph: x"}}}
    with pytest.raises(m.FidelityRefused):
        m.resolve_fidelity("physical", sim)
    read_hw = {"plant": {"kind": {"kind": "read", "value": "hardware", "source": "graph: x"}}}
    with pytest.raises(m.FidelityRefused):
        m.resolve_fidelity("physical", read_hw)
    hw = {"plant": {"kind": {"kind": "operator", "value": "hardware", "source": "operator",
                             "prompt": "Is this a physical robot?"}}}
    with pytest.raises(m.FidelityRefused):
        m.resolve_fidelity("simulated", hw)
    assert m.resolve_fidelity("physical", hw) is m.Fidelity.HARDWARE
    assert m.resolve_fidelity("simulated", None) is m.Fidelity.SIMULATED
    assert m.resolve_fidelity("static", None) is m.Fidelity.SIMULATED


def test_a_report_cannot_be_built_as_hardware_without_the_plant() -> None:
    with pytest.raises(m.FidelityRefused):
        _report([_row("C1.1", "pass", "x")], ["C1.1"], fidelity="hardware")


# ── 3. The verdict: skipped Core prevents pass; a non-obligation does not ─────

def test_a_skipped_core_row_prevents_pass() -> None:
    rep = _report([_row("C1.1", "pass", "x"),
                   _row("C1.3", "skip", "deployment-property: insecure port")],
                  ["C1.1", "C1.3"])
    assert rep.verdict == "not-established"
    assert rep.conformant is False
    assert [b["checkId"] for b in rep.verdict_blockers] == ["C1.3"]
    assert rep.to_dict()["conformant"] is False
    assert rep.to_dict()["verdict"] == "not-established"


def test_a_failed_core_row_is_fail_not_not_established() -> None:
    rep = _report([_row("C1.1", "fail", "no hello"),
                   _row("C1.3", "skip", "deployment-property: insecure port")],
                  ["C1.1", "C1.3"])
    assert rep.verdict == "fail"
    assert rep.conformant is False


def test_a_legal_alternative_path_still_does_not_hold_core() -> None:
    """ADR-0048 rule B is extended, not undone."""
    rep = _report([_row("C5.1", "pass", "acked"),
                   _row("C5.6", "skip", "legal-alternative-path: terminal ack directly")],
                  ["C5.1", "C5.6"],
                  unlocks={"byCause": {}, "byCheck": {"FleetAdapter": {"C5.6": "u"}}})
    assert rep.verdict == "pass"
    assert rep.conformant is True
    assert rep.sheet["C5.6"].grade is m.Grade.NOT_SUPPORTED_BY_PLATFORM


def test_a_skipped_row_outside_a_required_core_does_not_block() -> None:
    rep = _report([_row("C1.1", "pass", "x"),
                   _row("C1.3", "skip", "deployment-property: insecure port")],
                  ["C1.1", "C1.3"], required=False)
    assert rep.verdict == "pass"


def test_a_core_row_the_run_never_recorded_prevents_pass() -> None:
    rep = _report([_row("C1.1", "pass", "x")], ["C1.1", "C1.9"])
    assert rep.verdict == "not-established"
    assert [b["checkId"] for b in rep.verdict_blockers] == ["C1.9"]


# ── 4. unlock is due on every non-measured row ───────────────────────────────

def test_a_non_measured_row_without_unlock_fails() -> None:
    with pytest.raises(m.MissingUnlock) as e:
        _report([_row("C1.4", "skip", "not-observable-on-wire: no health signal")],
                ["C1.4"], unlocks={"byCause": {}, "byCheck": {}})
    assert "C1.4" in str(e.value)


def test_measured_and_failed_rows_carry_no_unlock() -> None:
    rep = _report([_row("C1.1", "pass", "x"), _row("C1.2", "fail", "y")], ["C1.1", "C1.2"])
    assert rep.sheet["C1.1"].unlock is None
    assert rep.sheet["C1.2"].unlock is None


def test_the_shipped_unlock_table_covers_every_cause_without_a_default(profiled: Path) -> None:
    """Every cause either has a default unlock or every check that records it has
    its own. Read off the profiled reports, which are the rows that actually occur."""
    table = m.Unlocks.load()
    skips = 0
    for name in REPORTS:
        path = profiled / f"{name}.yaml"
        doc = yaml.safe_load(path.read_text())
        for r in doc["checks"]:
            if r["status"] != "skip":
                continue
            skips += 1
            cause = m.SkipCause.of(r["detail"])
            assert table.resolve(doc["surface"], r["checkId"], cause), (
                f"{path.name}: {r['checkId']} ({cause.value}) has no unlock")
    assert skips, "no profiled report records a skip; this check read no row it could fail on"


# ── 5. The profiled reports carry the sheet ──────────────────────────────────

@pytest.mark.parametrize("name", REPORTS)
def test_every_profiled_report_carries_the_sheet(profiled: Path, name: str) -> None:
    doc = yaml.safe_load((profiled / f"{name}.yaml").read_text())
    grades = {g.value for g in m.Grade}
    due = {"not-supported-by-platform", "not-testable-here", "not-tested-suite-gap"}
    assert doc["subject"]["fidelity"] in ("simulated", "hardware")
    assert doc["verdict"] in ("pass", "fail", "not-established")
    assert doc["conformant"] is (doc["verdict"] == "pass")
    for r in doc["checks"]:
        if r["level"] == "context":
            assert "grade" not in r
            continue
        assert r["grade"] in grades, r["checkId"]
        assert "evidence" in r, r["checkId"]
        if r["grade"] in due:
            assert r.get("unlock"), f"{r['checkId']} is {r['grade']} with no unlock"
        else:
            assert "unlock" not in r, r["checkId"]
        if doc["subject"]["fidelity"] == "simulated":
            assert r["grade"] != "measured-hardware", r["checkId"]


@pytest.mark.parametrize("name", REPORTS)
def test_profiled_blockers_are_exactly_the_unmeasured_required_core_rows(profiled: Path,
                                                                        name: str) -> None:
    from conformance.registry import Registry
    doc = yaml.safe_load((profiled / f"{name}.yaml").read_text())
    reg = Registry()
    by_id = {r["checkId"]: r for r in doc["checks"]}
    na = {c.value for c in m.NOT_APPLICABLE_CAUSES}
    expect = []
    for p in doc["profiles"]:
        if p.get("applicable") is False or not p["required"] or "core" not in p:
            continue
        spec = next(x for x in reg.surface(doc["surface"])["profiles"] if x["name"] == p["name"])
        for cid in spec.get("core") or []:
            r = by_id.get(cid)
            if r is None:
                expect.append(cid)
                continue
            cause = m.SkipCause.of(r["detail"]) if r["status"] == "skip" else None
            if cause is not None and cause.value in na:
                continue
            if not r["grade"].startswith("measured-"):
                expect.append(cid)
    got = [b["checkId"] for b in doc.get("verdictBlockedBy", [])]
    assert sorted(got) == sorted(expect)


def test_no_measured_fleet_adapter_row_rests_on_an_empty_observation(profiled: Path) -> None:
    """Evidence is the observation, not the verdict. A passing row whose harness
    recorded nothing is a verdict string wearing the evidence field."""
    doc = yaml.safe_load((profiled / "fleet-adapter.yaml").read_text())
    passing = [r for r in doc["checks"] if r["level"] != "context" and r["status"] == "pass"]
    assert passing, "the FleetAdapter report has no passing row; this check read nothing"
    empty = [r["checkId"] for r in passing if not (r.get("evidence") or {}).get("observed")]
    assert not empty, f"measured with no recorded observation: {empty}"
