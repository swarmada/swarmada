# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""`make conformance-profiles` cannot profile a report an earlier run left behind.

Why this file exists. The target runs in two halves: the adapter harness writes
`_adapter.json`, and the profiler grades it into the stored `ConformanceReport` that the
adapter registry's cell then cites. The first half ran as

    … --json conformance-reports/_adapter.json >/dev/null 2>&1 || true

so a harness run that refused to start, died, or wrote nothing left exactly the trace a clean
run left: none. The profiler then graded whatever `_adapter.json` was on disk, which could be
the previous run's — and nothing in the output distinguished "this report was earned now"
from "this report is what was already there".

The fix is not a bigger check, it is an ordering: remove the old report BEFORE the run, so
nothing on disk afterwards can be stale, and stop the target when the run leaves nothing.
`|| true` is gone and the reason for it is kept — the harness exits non-zero for a
non-conformant adapter and the simulation adapter is not conformant, so the status is printed
and the recipe continues. That is a property of the recipe's TEXT, which is why this guard
reads the text: running the target needs the generated stubs and several seconds, and a gate
nobody can afford to run is not a gate.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
MAKEFILE = ROOT / "Makefile"
REPORT = "_adapter.json"
#: The harness report the profiler grades here: the recorded simulation-adapter run, committed.
HARNESS_REPORT = ROOT / "test" / "samples" / "testdata" / "recorded-sim-adapter-report.json"
OVERLAY = "gate-integrity-register.txt"


def _recipe(target: str) -> list[str]:
    """The recipe lines of one target: tab-indented lines after its rule, comments dropped."""
    text = MAKEFILE.read_text(encoding="utf-8")
    m = re.search(rf"(?m)^{re.escape(target)}:[^\n]*\n((?:\t[^\n]*\n|\n)*)", text)
    assert m, (
        f"the Makefile has no `{target}:` rule. If it was renamed, update this guard with it; "
        f"do not delete the guard")
    lines = [ln for ln in m.group(1).splitlines() if ln.startswith("\t")]
    assert lines, f"the `{target}` recipe is empty; this guard cannot run"
    return [ln.lstrip("\t") for ln in lines if not ln.lstrip("\t").startswith("#")]


@pytest.fixture(scope="module")
def recipe() -> list[str]:
    return _recipe("conformance-profiles")


def test_the_harness_half_is_not_silenced(recipe: list[str]) -> None:
    for line in recipe:
        assert ">/dev/null" not in line and "2>&1" not in line, (
            f"the conformance-profiles recipe sends output to /dev/null again:\n  {line}\n"
            f"The stored report and the registry cell citing it are earned from that run; a "
            f"run nobody can see is a run nobody can check")


def test_the_harness_half_does_not_discard_its_exit_status(recipe: list[str]) -> None:
    run = [ln for ln in recipe if "-m adapters.conformance" in "".join(recipe)
           and "adapters.conformance" in ln]
    assert run, "the recipe no longer runs the adapter harness; this guard cannot run"
    joined = "".join(recipe)
    assert "|| true" not in joined, (
        "`|| true` is back on the conformance-profiles recipe, so a harness run that failed "
        "for any reason reads as one that succeeded. Record the status and continue instead")
    assert "status=$$?" in joined and "exited $$status" in joined, (
        "the harness run's exit status is neither propagated nor printed. The harness exits "
        "non-zero for a non-conformant adapter, so the recipe may continue -- but the status "
        "has to reach the reader")


def test_the_previous_runs_report_is_removed_before_the_run(recipe: list[str]) -> None:
    removal = next((i for i, ln in enumerate(recipe)
                    if ln.startswith("rm -f") and REPORT in ln), None)
    run = next((i for i, ln in enumerate(recipe) if "adapters.conformance" in ln), None)
    assert removal is not None, (
        f"the conformance-profiles recipe does not remove {REPORT} before running the "
        f"harness, so a run that writes nothing leaves the previous run's report to be "
        f"profiled as if it were fresh")
    assert run is not None and removal < run, (
        f"{REPORT} is removed after the harness runs, which deletes the report the profiler "
        f"is about to read")


def test_an_absent_report_stops_the_target(recipe: list[str]) -> None:
    joined = "".join(recipe)
    guard = next((ln for ln in recipe if f"test -s $(CONFORMANCE_REPORT_DIR)/{REPORT}" in ln),
                 None)
    assert guard is not None, (
        f"nothing in the recipe checks that the harness wrote {REPORT}; an absent report is "
        f"the case the old `|| true` hid")
    assert "exit 1" in joined, "the check does not stop the target"
    profile = next((i for i, ln in enumerate(recipe) if "-m conformance all" in ln), None)
    check = next(i for i, ln in enumerate(recipe) if ln is guard)
    assert profile is not None and check < profile, (
        "the report is profiled before it is checked for")


def _logical(recipe: list[str]) -> list[str]:
    """The recipe's shell commands, with backslash-continued lines joined."""
    out, cur = [], ""
    for ln in recipe:
        cur += ln.rstrip("\\") + " " if ln.endswith("\\") else ln
        if not ln.endswith("\\"):
            out.append(cur)
            cur = ""
    return out + ([cur] if cur else [])


def _gate(root: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "check-gate-integrity.py"), "--root", str(root)],
        capture_output=True, text=True, timeout=300, check=False)


#: Profiling as a tree without the registry overlays would: the overlay glob is
#: pointed at nothing, which is what conformance/registry.py finds in such a tree.
_NO_OVERLAYS = (
    "import sys, conformance.registry as r; r.OVERLAY_GLOB = 'rfcs/*/no-overlay-in-this-shape'; "
    "from conformance.__main__ import main; sys.argv[0] = 'conformance'; sys.exit(main())")

#: The two shapes a clone can have. `as-shipped` is this tree; `without-overlays` is the same
#: tree with every rfcs/*/conformance-registry.yaml cut, which a tree may not carry.
SHAPES = ("as-shipped", "without-overlays")


def _install(reports: Path, shape: str) -> subprocess.CompletedProcess[str]:
    """Install the reports' entries by running the Makefile's own target on this tree."""
    args = ["make", "--no-print-directory", "-C", str(ROOT), "conformance-reports-register",
            f"CONFORMANCE_REPORT_DIR={reports}"]
    if shape == "without-overlays":
        args.append("CONFORMANCE_OVERLAYS=")
    return subprocess.run(args, capture_output=True, text=True, timeout=120, check=False)


def _profiled_clone(root: Path, shape: str) -> Path:
    """A scratch clone holding the reports the profiler writes for `shape`, and no entries."""
    reports = root / "conformance-reports"
    (root / "Makefile").write_text("check:\n\ttrue\n")
    (root / OVERLAY).write_text("")
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join(
        ["proto", "."] + ([env["PYTHONPATH"]] if env.get("PYTHONPATH") else []))
    head = ["-m", "conformance"] if shape == "as-shipped" else ["-c", _NO_OVERLAYS]
    run = subprocess.run(
        [sys.executable, *head, "all", "--adapter-report", str(HARNESS_REPORT),
         "--out", str(reports)],
        cwd=ROOT, env=env, capture_output=True, text=True, timeout=600, check=False)
    assert run.returncode in (0, 1, 3), (
        f"`python -m conformance all` ({shape}) exited {run.returncode}:\n{run.stderr[-2000:]}")
    return reports


@pytest.mark.parametrize("shape", SHAPES)
def test_a_clone_that_runs_the_target_passes_gate_integrity(
    recipe: list[str], tmp_path: Path, shape: str
) -> None:
    """A clone that follows the docs gets a green `make gate-integrity` after this target.

    conformance-reports/ is not published. The gate-integrity entries for the reports lived
    only beside the reports, in a file no command wrote, so a clone that ran this target had
    the reports and not their entries, and the gate reported every counted site NEW. The
    recipe now installs the entries beside the reports it writes, and does not stop on the
    profiler's verdict before it does.

    Which entries apply depends on the tree: a level that only a registry overlay declares
    exists only where the overlay is, and a tree may carry no overlay. An entry for such a level
    kept in scripts/conformance-reports-register.txt was green on a tree with the overlay and
    red on one without it. So this profiles the committed harness report into a scratch
    tree in BOTH shapes, shows the gate red on the reports alone, installs the entries with the
    Makefile's own target, and shows it green.
    """
    commands = _logical(recipe)
    profile = next((i for i, c in enumerate(commands) if "-m conformance all" in c), None)
    assert profile is not None, "the recipe no longer runs the profiler; this guard cannot run"
    assert "status=$$?" in commands[profile] and "exit $$status" in commands[profile], (
        "the profiler's exit status stops the recipe on a verdict (3 is `not-established`, "
        "which the simulation adapter always is), so nothing after it runs")
    install = next((i for i, c in enumerate(commands) if "conformance-reports-register" in c),
                   None)
    assert install is not None and install > profile, (
        f"the recipe does not install the reports' gate-integrity entries into "
        f"$(CONFORMANCE_REPORT_DIR)/{OVERLAY} after it writes them, so on a clone "
        f"`make gate-integrity` reports every counted site in them as NEW")

    reports = _profiled_clone(tmp_path, shape)
    alone = _gate(tmp_path)
    assert alone.returncode == 1 and "NEW" in alone.stdout, (
        "the reports alone no longer carry a counted site, so this guard proves nothing; "
        f"re-read it against the gate's output:\n{alone.stdout[-2000:]}")
    made = _install(reports, shape)
    assert made.returncode == 0 and (reports / OVERLAY).is_file(), (
        f"`make conformance-reports-register` failed:\n{made.stdout[-1000:]}{made.stderr[-1000:]}")
    after = _gate(tmp_path)
    assert after.returncode == 0, (
        f"gate-integrity is red on a clone ({shape}) after `make conformance-profiles`. An entry "
        f"reported REPAIRED only {'without' if shape == 'without-overlays' else 'with'} the "
        f"overlays belongs beside the overlay whose level it counts "
        f"(rfcs/<spec>/conformance-reports-register.txt), not in "
        f"scripts/conformance-reports-register.txt:\n{after.stdout[-3000:]}")


def test_a_stale_installed_entry_is_named_by_the_gate(tmp_path: Path) -> None:
    """An entry that names a site the reports do not have is a red gate that names the entry.

    This is the drift the target exists to prevent. It plants one stale entry in what the
    target installed, shows the gate red naming it, then reinstalls and shows it green.
    """
    reports = _profiled_clone(tmp_path, "as-shipped")
    assert _install(reports, "as-shipped").returncode == 0
    stale = "conformance-reports/fleet-adapter.yaml::verdict"
    with (reports / OVERLAY).open("a", encoding="utf-8") as f:
        f.write(f"inert :: {stale} :: 1 :: planted by this test: a level the report lacks\n")
    red = _gate(tmp_path)
    assert red.returncode == 1 and "REPAIRED" in red.stdout and stale in red.stdout, (
        f"a stale entry in the installed register did not turn the gate red with its name:\n"
        f"{red.stdout[-2000:]}")
    assert _install(reports, "as-shipped").returncode == 0
    green = _gate(tmp_path)
    assert green.returncode == 0, (
        f"reinstalling did not clear the planted entry:\n{green.stdout[-2000:]}")


def test_a_source_without_a_final_newline_does_not_merge_entries(tmp_path: Path) -> None:
    """The target joins its sources line by line: one file's last entry never runs into the next.

    A source whose last line has no newline, joined byte for byte, glues that entry to the next
    file's first line, and the gate then reads one malformed entry where there were two.
    """
    first, second = tmp_path / "a.txt", tmp_path / "b.txt"
    first.write_text("# first\ninert :: x.yaml::verdict :: 1 :: a")
    second.write_text("inert :: y.yaml::verdict :: 1 :: b\n")
    reports = tmp_path / "conformance-reports"
    made = subprocess.run(
        ["make", "--no-print-directory", "-C", str(ROOT), "conformance-reports-register",
         f"CONFORMANCE_REPORT_DIR={reports}", f"CONFORMANCE_REGISTER_SOURCES={first} {second}"],
        capture_output=True, text=True, timeout=120, check=False)
    assert made.returncode == 0, made.stderr[-1000:]
    lines = (reports / OVERLAY).read_text().splitlines()
    assert lines == ["# first", "inert :: x.yaml::verdict :: 1 :: a",
                     "inert :: y.yaml::verdict :: 1 :: b"], (
        f"the installed register did not keep the two sources' entries apart: {lines}")


def test_every_overlay_register_sits_beside_its_overlay() -> None:
    """An overlay's entries are installed only when its conformance-registry.yaml is beside them.

    A conformance-reports-register.txt anywhere else is never installed, so its entries count
    nothing and nothing says so.
    """
    for frag in sorted(ROOT.glob("rfcs/*/conformance-reports-register.txt")):
        assert (frag.parent / "conformance-registry.yaml").is_file(), (
            f"{frag.relative_to(ROOT)} has no conformance-registry.yaml beside it, so "
            f"`make conformance-profiles` never installs it")
