# Copyright 2026 The Swarmada Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Every conformance report this repository pins names the current suite revision.

The sample FleetAdapters pin a recorded harness report by digest, and
`conformance-reports/` holds the profiled reports. Twice a pinned report outlived the
harness that wrote it: the first-run sample kept attesting a check the suite no longer
claimed to perform, because nothing compared the revision a report was produced by with
the revision the harness is now. A pinned report from an older suite states results that
the current suite may grade differently, so it is re-recorded when the suite moves.

A report that names no revision is treated as older than every revision: it cannot say
which suite produced it.

PENDING RE-RECORDING. A revision can land before the reports it outdates are re-recorded:
re-recording the ROS 2 adapter's report needs a run of that adapter. The current revision's
entry in `conformance/profiles.yaml` then lists those reports under `pendingReRecording:`,
and their cases here are expected failures, strictly: once the harness names that revision,
a listed report still on an older suite is reported XFAIL, and a listed report that passes
fails the run, so the entry is removed when the report is re-recorded. Until the harness
names the revision the mark does not apply and each case is graded as before. The list is
read from the registry, not written here, so the public record of what is outstanding and
the test cannot disagree.

WHAT THIS DOES NOT CHECK
    Whether the report's verdicts are what a run at the current revision would give.
    Re-record with `hack/sim-conformance-report.sh` and `make conformance-profiles`, and
    compare per-check verdicts against the previous recording.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

import pytest
import yaml

from adapters.conformance.report import SUITE_REVISION

ROOT = Path(__file__).resolve().parents[2]
PROFILES = ROOT / "conformance" / "profiles.yaml"

_PATTERNS = (
    "config/**/conformance-report.json",
    "examples/**/conformance-report.json",
    "test/samples/testdata/recorded-*report*.json",
    "conformance-reports/*.json",
    "conformance-reports/*.yaml",
)
_REV = re.compile(r"^\d{4}\.\d{2}\.\d{2}(?:\.\d+)?$")
# Generated locally by `make conformance-profiles`; version control ignores it.
GENERATED_REPORTS = "conformance-reports"


def _key(rev: str) -> tuple[int, ...]:
    return tuple(int(p) for p in rev.split("."))


def _pinned_reports() -> list[Path]:
    found = sorted({p for pat in _PATTERNS for p in ROOT.glob(pat) if p.is_file()})
    # Fail, never skip: finding nothing means the patterns no longer match the tree.
    assert found, "no pinned conformance report found; update the patterns with the tree"
    return found


def _pending_re_recording() -> tuple[str, set[str], str]:
    """The current suite revision, the reports its entry lists as pending re-recording, and
    the reason it gives."""
    doc = yaml.safe_load(PROFILES.read_text(encoding="utf-8"))
    current = str(doc["revision"])
    entry = next((e for e in doc.get("revisions") or [] if str(e.get("revision")) == current),
                 None)
    assert entry is not None, f"profiles.yaml has no `revisions:` entry for {current}"
    pending = entry.get("pendingReRecording") or {}
    reports = {str(r) for r in pending.get("reports") or []}
    reason = str(pending.get("reason") or "")
    assert not reports or reason, (
        f"profiles.yaml lists reports pending re-recording at {current} and states no reason")
    return current, reports, reason


def _cases() -> list:
    """One case per pinned report. A report the current revision lists as pending
    re-recording is a strict expected failure once the harness names that revision."""
    current, pending, reason = _pending_re_recording()
    harness_moved = _key(SUITE_REVISION) >= _key(current)
    cases = []
    for path in _pinned_reports():
        where = path.relative_to(ROOT).as_posix()
        marks = (pytest.mark.xfail(harness_moved, reason=reason, strict=True)
                 if where in pending else ())
        cases.append(pytest.param(path, marks=marks, id=where))
    return cases


def _revision(path: Path) -> str | None:
    text = path.read_text(encoding="utf-8")
    doc = json.loads(text) if path.suffix == ".json" else yaml.safe_load(text)
    rev = doc.get("suite_revision") or doc.get("suiteRevision")
    return None if rev is None else str(rev)


def test_the_patterns_find_the_reports_the_samples_pin() -> None:
    # The reports a FleetAdapter pins by digest, and the recording they are compared with.
    # conformance-reports/ is not listed: it is generated locally and ignored by version
    # control, so a published checkout has none, and whatever of it is present is checked
    # by the test below.
    names = {p.relative_to(ROOT).as_posix() for p in _pinned_reports()}
    for must in ("config/samples/first-run/conformance-report.json",
                 "config/overlays/demo-webhooks/sim-adapter/conformance-report.json",
                 "test/samples/testdata/recorded-sim-adapter-report.json"):
        assert must in names, f"{must} is pinned but the patterns do not find it"


@pytest.mark.parametrize("path", _cases())
def test_a_pinned_report_is_on_the_harness_suite_revision(path: Path) -> None:
    rev = _revision(path)
    where = path.relative_to(ROOT).as_posix()
    assert rev is not None, (
        f"{where} names no suite revision, so it cannot say which suite produced it; "
        f"re-record it at {SUITE_REVISION}")
    assert _REV.match(rev), f"{where} names suite revision {rev!r}, which is not a revision"
    assert _key(rev) >= _key(SUITE_REVISION), (
        f"{where} was produced by suite {rev}; the harness is {SUITE_REVISION}. Re-record it "
        f"and compare its per-check verdicts with the previous recording")


def test_every_report_pending_re_recording_is_a_pinned_report() -> None:
    # An entry naming a report the patterns do not find marks nothing, and would stay listed
    # as outstanding after the report was moved or deleted.
    current, pending, _ = _pending_re_recording()
    names = {p.relative_to(ROOT).as_posix() for p in _pinned_reports()}
    stray = set(pending - names)
    # conformance-reports/ is generated by `make conformance-profiles` and not published, so a
    # clone without it holds none of the entries listed under it. Only when the directory is
    # absent are those entries not checked, and the skip below says so. Where it exists every
    # entry is checked, and an entry for a file missing from it is still a stray.
    unchecked = (set() if (ROOT / GENERATED_REPORTS).is_dir()
                 else {s for s in stray if s.startswith(GENERATED_REPORTS + "/")})
    stray -= unchecked
    assert not stray, (
        f"profiles.yaml lists {sorted(stray)} as pending re-recording at {current}, and no pinned "
        f"report has that path")
    if unchecked:
        pytest.skip(
            f"{GENERATED_REPORTS}/ is generated by `make conformance-profiles` and not published, "
            f"and is absent here: {len(unchecked)} pending entries under it were not checked "
            f"({', '.join(sorted(unchecked))})")
